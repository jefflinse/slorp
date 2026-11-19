import assert from 'node:assert/strict'
import { test, type TestContext } from 'node:test'
import { Slorp } from './slorp.ts'
import { BroadcastChannel } from 'node:worker_threads'
import { readFile } from 'node:fs/promises'

// The world around the plugin: the Slorp service behind fetch, and OpenCode
// behind the client, each answered from memory and recorded.

type Post = { path: string; body: any }
type Message = { info: { role: string; [key: string]: any }; parts: { type: string; text?: string; synthetic?: boolean }[] }

const SID = 'ses_top'

async function world(t: TestContext, options?: Record<string, unknown>) {
  const w = {
    url: '',
    /** What the Slorp service was sent, in order. */
    posts: [] as Post[],
    /** What the Slorp service answers, by path. Unlisted paths answer with no body. */
    answers: {} as Record<string, (body: any) => unknown>,
    isDown: false,
    infos: {} as Record<string, object>,
    lookups: [] as string[],
    messages: {} as Record<string, Message[]>,
    prompted: [] as any[],
    promptError: undefined as unknown,
    promptThrows: false,
    messageError: undefined as unknown,
    messageGate: undefined as Promise<void> | undefined,
    statuses: {} as Record<string, object>,
    statusGate: undefined as Promise<void> | undefined,
    /** Permission answers sent to the route that carries a message. */
    answered: [] as any[],
    answerError: undefined as unknown,
    answerStatus: undefined as number | undefined,
    /** Permission answers sent to the older route. */
    answeredOld: [] as any[],
    to: (path: string) => w.posts.filter(p => p.path === path).map(p => p.body),
    polls: () => w.to('/api/bridge/poll').map(({ snapshot, pluginVersion, pluginRevision, pluginTui, ...rest }) => rest),
  }

  const realFetch = globalThis.fetch
  t.after(() => (globalThis.fetch = realFetch))
  globalThis.fetch = (async (url: string, init: RequestInit) => {
    if (w.isDown) throw new Error('connection refused')
    const at = new URL(url)
    w.url = at.origin
    const body = JSON.parse(init.body as string)
    w.posts.push({ path: at.pathname, body })
    const gone = new Promise<never>((_, reject) => init.signal?.addEventListener('abort', () => reject(new Error('aborted'))))
    const out = await Promise.race([Promise.resolve().then(() => w.answers[at.pathname]?.(body)), gone])
    return out === undefined ? new Response(null, { status: 204 }) : Response.json(out)
  }) as typeof fetch

  const client = {
    session: {
      status: async () => { await w.statusGate; return { data: w.statuses } },
      get: async ({ path }: any) => (w.lookups.push(path.id), { data: w.infos[path.id] }),
      messages: async ({ path }: any) => {
        await w.messageGate
        return { data: w.messages[path.id] ?? [], error: w.messageError }
      },
      promptAsync: async ({ signal, ...o }: any) => {
        w.prompted.push(o)
        if (w.promptThrows) throw new Error('OpenCode unreachable')
        return { error: w.promptError }
      },
    },
    vcs: { get: async () => ({ data: { branch: 'main' } }) },
    _client: { post: async ({ signal, ...o }: any) => (w.answered.push(o), { error: w.answerError, response: w.answerStatus ? new Response(null, { status: w.answerStatus }) : undefined }) },
    postSessionIdPermissionsPermissionId: async ({ signal, ...o }: any) => (w.answeredOld.push(o), {}),
  }

  t.mock.timers.reset()
  t.mock.timers.enable({ apis: ['setInterval', 'Date'] })
  const hooks: any = await Slorp({ client, directory: '/work/proj' } as any, options)
  t.after(() => hooks.dispose())

  /** Lets everything the plugin has started run as far as it can. */
  const settle = async () => {
    for (let i = 0; i < 20; i++) await new Promise(resolve => setImmediate(resolve))
  }
  const emit = async (type: string, properties: object) => {
    await hooks.event({ event: { type, properties } })
    await settle()
  }
  return {
    w,
    settle,
    emit,
    dispatch: (type: string, properties: object) => hooks.event({ event: { type, properties } }),
    open: async (sessionId?: string, directory = '/work/proj') => {
      const channel = new BroadcastChannel('slorp.opencode.presence')
      t.after(() => channel.close())
      channel.postMessage({ directory, sessionId })
      await settle()
    },
    tick: async (ms = 2000) => {
      t.mock.timers.tick(ms)
      await settle()
    },
    say: async (sessionID: string, ...parts: Message['parts']) => {
      await hooks['chat.message']({ sessionID }, { parts })
      await settle()
    },
    dispose: () => hooks.dispose(),
    params: async (sessionID: string, context: number) => hooks['chat.params']({ sessionID, model: { limit: { context } } }),
    sayWith: async (input: object, message: object, ...parts: Message['parts']) => {
      await hooks['chat.message']({ sessionID: SID, ...input }, { message, parts })
      await settle()
    },
  }
}

const text = (t: string, more: object = {}) => ({ type: 'text', text: t, ...more })
const assistant = (...parts: Message['parts']): Message => ({ info: { role: 'assistant' }, parts })
const user = (t: string): Message => ({ info: { role: 'user' }, parts: [text(t)] })

/** A world in which the top-level session has finished one turn. */
async function afterOneTurn(t: TestContext) {
  const o = await world(t)
  await o.emit('session.created', { info: { id: SID, title: 'New session - 2026-11-17T18:25:54.371Z', directory: '/work/proj' } })
  await o.say(SID, text('fix the build'))
  await o.emit('session.updated', {
    info: { id: SID, title: 'Fix the build', directory: '/work/proj', agent: 'plan', model: { id: 'qwen', providerID: 'lm-studio' } },
  })
  o.w.messages[SID] = [user('fix the build'), assistant(text('It is fixed.'))]
  await o.emit('session.idle', { sessionID: SID })
  return o
}

/** A world in which the top-level session is waiting on a permission prompt Slorp may answer. */
async function asked(t: TestContext, waits: unknown[]) {
  const o = await afterOneTurn(t)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'slorp-1', answerable: true })
  o.w.answers['/api/bridge/permission/wait'] = () => waits.shift() ?? new Promise(() => {})
  await o.emit('permission.asked', { id: 'per_1', sessionID: SID, permission: 'bash', patterns: ['rm -rf build'], metadata: { command: 'rm -rf build' } })
  return o
}

test('a turn is reported as it starts and as it ends', async t => {
  const { w, say, emit } = await world(t)
  await emit('session.created', { info: { id: SID, title: 'Fix the build', directory: '/work/proj' } })
  await say(SID, text('  fix the build  '), text('injected context', { synthetic: true }), { type: 'file' }, text('and the tests'))
  assert.deepEqual(w.to('/api/bridge/event'), [{ agent: 'opencode', sessionId: SID, type: 'turn.start', text: 'fix the build\n\nand the tests' }])

  w.messages[SID] = [
    user('an earlier prompt'),
    assistant(text('An earlier answer.')),
    user('fix the build'),
    assistant({ type: 'tool' }, text('Looking.')),
    assistant({ type: 'reasoning', text: 'thinking' }, text('It is fixed.'), text('Tests pass.')),
    assistant({ type: 'tool' }),
  ]
  await emit('session.idle', { sessionID: SID })
  assert.equal(w.to('/api/bridge/event')[1].snapshot.lastReply, 'It is fixed.\n\nTests pass.')
})

test('a turn that wrote nothing clears the answer before it', async t => {
  const { w, say, emit } = await afterOneTurn(t)
  await say(SID, text('now deploy'))
  w.messages[SID] = [user('fix the build'), assistant(text('It is fixed.')), user('now deploy'), assistant({ type: 'tool' })]
  await emit('session.idle', { sessionID: SID })
  assert.equal(w.to('/api/bridge/event').at(-1).snapshot.lastReply, '')
})

test('Slorp hears of a turn ending only after it has heard of it starting', async t => {
  const { w, say, emit, settle } = await world(t)
  let heard = () => {}
  w.answers['/api/bridge/event'] = body => (body.type === 'turn.start' ? new Promise<void>(resolve => (heard = resolve)) : undefined)
  w.messages[SID] = [user('fix the build'), assistant(text('It is fixed.'))]
  await say(SID, text('fix the build'))
  await emit('session.idle', { sessionID: SID })
  assert.deepEqual(w.to('/api/bridge/event').map(e => e.type), ['turn.start'])
  heard()
  await settle()
  assert.deepEqual(w.to('/api/bridge/event').map(e => e.type), ['turn.start', 'snapshot'])
})

test('a session whose parents lead back to itself is still resolved', async t => {
  const { w, say, emit } = await world(t)
  await emit('session.created', { info: { id: 'ses_a', parentID: 'ses_b' } })
  await emit('session.created', { info: { id: 'ses_b', parentID: 'ses_a' } })
  await say('ses_a', text('hello'))
  assert.ok(w.posts.length <= 1)
})

test('a turn that wrote nothing is not credited with an earlier turn\'s answer', async t => {
  const { w, say, emit } = await world(t)
  w.messages[SID] = [user('an earlier prompt'), assistant(text('An earlier answer.')), user('now deploy'), assistant({ type: 'tool' })]
  await say(SID, text('now deploy'))
  await emit('session.idle', { sessionID: SID })
  assert.equal(w.to('/api/bridge/event').at(-1).snapshot.lastReply, '')
})

test("a subagent's turns are not reported", async t => {
  const { w, say, emit, tick } = await world(t)
  await emit('session.created', { info: { id: 'ses_child', parentID: SID, title: 'Child session - 2026-11-17T18:25:54.371Z' } })
  await say('ses_child', text('explore the repo'))
  await emit('session.idle', { sessionID: 'ses_child' })

  w.infos.ses_resumed = { id: 'ses_resumed', parentID: SID }
  await say('ses_resumed', text('explore some more'))
  await tick()
  assert.deepEqual(w.lookups, [SID, 'ses_resumed'], 'a session the plugin never saw created is looked up, once')
  assert.deepEqual(w.posts, [])
})

test('each session with something happening in it is polled for', async t => {
  const { w, say, emit, tick } = await afterOneTurn(t)
  await emit('session.created', { info: { id: 'ses_untouched', title: 'Never prompted', directory: '/work/proj' } })
  await emit('session.created', { info: { id: 'ses_new', title: 'New session - 2026-11-17T18:25:54.371Z', directory: '/work/other' } })
  await say('ses_new', text('hello'))
  await tick()
  assert.deepEqual(w.polls(), [
    { agent: 'opencode', sessionId: SID, cwd: '/work/proj', title: 'Fix the build', branch: 'main' },
    { agent: 'opencode', sessionId: 'ses_new', cwd: '/work/other', title: '', branch: 'main' },
  ])

  await emit('vcs.branch.updated', { branch: 'release' })
  await emit('session.deleted', { info: { id: 'ses_new' } })
  await tick()
  assert.deepEqual(w.polls().slice(2), [{ agent: 'opencode', sessionId: SID, cwd: '/work/proj', title: 'Fix the build', branch: 'release' }])
})

test('a session known only by its id is polled for with the instance directory', async t => {
  const { w, say, tick } = await world(t)
  await say('ses_unknown', text('hello'))
  await tick()
  assert.deepEqual(w.polls(), [{ agent: 'opencode', sessionId: 'ses_unknown', cwd: '/work/proj', title: '', branch: 'main' }])
})

test('a reply written in Slorp is submitted under the agent and model the session last used', async t => {
  const { w, tick } = await afterOneTurn(t)
  w.answers['/api/bridge/poll'] = () => ({ replies: [{ id: 'r1', text: 'go ahead' }] })
  await tick()
  assert.deepEqual(w.prompted, [
    { path: { id: SID }, body: { agent: 'plan', model: { providerID: 'lm-studio', modelID: 'qwen' }, parts: [{ type: 'text', text: 'go ahead' }] } },
  ])
  assert.deepEqual(w.to('/api/bridge/ack'), [{ id: 'r1', state: 'delivered' }])

  w.promptError = { name: 'NotFoundError' }
  w.answers['/api/bridge/poll'] = () => ({ replies: [{ id: 'r2', text: 'and again' }] })
  await tick()
  assert.deepEqual(w.to('/api/bridge/ack')[1], { id: 'r2', state: 'dropped' })
})

test('a Slorp that had not heard of a session is told where it stands', async t => {
  const { w, say, tick } = await afterOneTurn(t)
  w.posts.length = 0
  w.answers['/api/bridge/poll'] = () => ({ replies: [], fresh: true })
  await tick()
  assert.deepEqual(w.to('/api/bridge/event'), [])
  assert.equal(w.to('/api/bridge/poll')[0].snapshot.lastPrompt, 'fix the build')
  assert.equal(w.to('/api/bridge/poll')[0].snapshot.lastReply, 'It is fixed.')
  assert.equal(w.to('/api/bridge/poll')[0].snapshot.status, 'idle')

  await say(SID, text('now deploy'))
  w.posts.length = 0
  await tick()
  assert.deepEqual(w.to('/api/bridge/event'), [])
  assert.equal(w.to('/api/bridge/poll')[0].snapshot.status, 'working')
  assert.equal(w.to('/api/bridge/poll')[0].snapshot.lastPrompt, 'now deploy')

  w.posts.length = 0
  w.answers['/api/bridge/poll'] = () => ({ replies: [], fresh: false })
  await tick()
  assert.deepEqual(w.to('/api/bridge/event'), [])
})

test('a poll still under way is not joined by another', async t => {
  const { w, tick, settle } = await afterOneTurn(t)
  let answer = (_: unknown) => {}
  w.answers['/api/bridge/poll'] = () => new Promise(resolve => (answer = resolve))
  await tick()
  await tick()
  assert.equal(w.to('/api/bridge/poll').length, 1)
  answer({ replies: [] })
  await settle()
  await tick()
  assert.equal(w.to('/api/bridge/poll').length, 2)
})

test('polling survives Slorp being down, and stops with the plugin', async t => {
  const { w, tick, dispose } = await afterOneTurn(t)
  w.isDown = true
  await tick()
  w.isDown = false
  w.posts.length = 0
  await tick()
  assert.equal(w.to('/api/bridge/poll').length, 1)

  await dispose()
  await tick()
  assert.equal(w.to('/api/bridge/poll').length, 1)
})

test('a permission prompt allowed in Slorp is allowed for that one call', async t => {
  const { w } = await asked(t, [{ state: 'waiting' }, { state: 'allow' }])
  assert.deepEqual(w.to('/api/bridge/permission'), [
    { agent: 'opencode', sessionId: SID, tool: 'bash', summary: 'rm -rf build', input: 'rm -rf build', canAnswer: true, question: false },
  ])
  assert.deepEqual(w.to('/api/bridge/permission/wait'), [{ id: 'slorp-1' }, { id: 'slorp-1' }])
  assert.deepEqual(w.answered, [
    { url: '/permission/{requestID}/reply', path: { requestID: 'per_1' }, body: { reply: 'once' }, headers: { 'content-type': 'application/json' } },
  ])
  assert.deepEqual(w.answeredOld, [])
})

test('a permission prompt denied in Slorp is rejected with what the user said', async t => {
  const { w } = await asked(t, [{ state: 'deny', message: 'not that directory' }])
  assert.deepEqual(w.answered[0].body, { reply: 'reject', message: 'The user denied this in Slorp and said: not that directory' })
})

test('a denial with nothing said is rejected with no message', async t => {
  const { w } = await asked(t, [{ state: 'deny' }])
  assert.deepEqual(w.answered[0].body, { reply: 'reject' })
})

test('an answer Slorp gave is not then reported back to it as settled elsewhere', async t => {
  const { w, emit } = await asked(t, [{ state: 'allow' }])
  await emit('permission.replied', { sessionID: SID, requestID: 'per_1', reply: 'once' })
  assert.deepEqual(w.to('/api/bridge/permission/done'), [])
})

test('an OpenCode without the newer route is answered through the older one', async t => {
  const o = await afterOneTurn(t)
  o.w.answerError = { name: 'NotFoundError' }
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'slorp-1', answerable: true })
  const waits: unknown[] = [{ state: 'allow' }, { state: 'deny', message: 'no' }]
  o.w.answers['/api/bridge/permission/wait'] = () => waits.shift()
  await o.emit('permission.asked', { id: 'per_1', sessionID: SID, permission: 'bash', metadata: { command: 'ls' } })
  await o.emit('permission.asked', { id: 'per_2', sessionID: SID, permission: 'bash', metadata: { command: 'ls' } })
  assert.deepEqual(o.w.answeredOld, [
    { path: { id: SID, permissionID: 'per_1' }, body: { response: 'once' } },
    { path: { id: SID, permissionID: 'per_2' }, body: { response: 'reject' } },
  ])
})

test('a prompt answered in OpenCode stops being offered in Slorp', async t => {
  const { w, emit } = await asked(t, [])
  await emit('permission.replied', { sessionID: SID, requestID: 'per_1', reply: 'once' })
  assert.deepEqual(w.to('/api/bridge/permission/done'), [{ id: 'slorp-1' }])
  assert.deepEqual([w.answered, w.answeredOld], [[], []])

  await emit('permission.replied', { sessionID: SID, requestID: 'per_1', reply: 'once' })
  assert.equal(w.to('/api/bridge/permission/done').length, 1)
})

test('a prompt answered in OpenCode before Slorp had recorded it is settled once Slorp has', async t => {
  const o = await afterOneTurn(t)
  let record = (_: unknown) => {}
  o.w.answers['/api/bridge/permission'] = () => new Promise(resolve => (record = resolve))
  await o.emit('permission.asked', { id: 'per_1', sessionID: SID, permission: 'bash', metadata: { command: 'ls' } })
  await o.emit('permission.replied', { sessionID: SID, requestID: 'per_1', reply: 'reject' })
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [])
  record({ id: 'slorp-1', answerable: true })
  await o.settle()
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [{ id: 'slorp-1' }])
  assert.deepEqual(o.w.to('/api/bridge/permission/wait'), [])
})

test('a prompt nobody answers within the hour is left to OpenCode', async t => {
  const o = await afterOneTurn(t)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'slorp-1', answerable: true })
  o.w.answers['/api/bridge/permission/wait'] = () => new Promise(resolve => setImmediate(() => resolve({ state: 'waiting' })))
  await o.emit('permission.asked', { id: 'per_1', sessionID: SID, permission: 'bash', metadata: { command: 'ls' } })
  await o.tick(60 * 60_000 - 4000)
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [])
  await o.tick(4000)
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [{ id: 'slorp-1' }])
  const waited = o.w.to('/api/bridge/permission/wait').length
  await o.settle()
  assert.equal(o.w.to('/api/bridge/permission/wait').length, waited)
  assert.deepEqual([o.w.answered, o.w.answeredOld], [[], []])
})

test('a turn that ends with a prompt still open settles it', async t => {
  const { w, emit } = await asked(t, [])
  await emit('session.idle', { sessionID: SID })
  assert.deepEqual(w.to('/api/bridge/permission/done'), [{ id: 'slorp-1' }])
})

test('a prompt Slorp will not answer is only shown there', async t => {
  const o = await afterOneTurn(t)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'slorp-1', answerable: false })
  await o.emit('permission.asked', { id: 'per_1', sessionID: SID, permission: 'bash', metadata: { command: 'ls' } })
  assert.deepEqual(o.w.to('/api/bridge/permission/wait'), [])
  await o.emit('permission.replied', { sessionID: SID, requestID: 'per_1', reply: 'once' })
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [{ id: 'slorp-1' }])
})

test("a subagent's prompt is shown under the session it works for, and answered where it was asked", async t => {
  const o = await afterOneTurn(t)
  o.w.posts.length = 0
  o.w.answerError = { name: 'NotFoundError' }
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'slorp-1', answerable: true })
  o.w.answers['/api/bridge/permission/wait'] = () => ({ state: 'allow' })
  await o.emit('session.created', { info: { id: 'ses_child', parentID: SID } })
  await o.emit('session.created', { info: { id: 'ses_grandchild', parentID: 'ses_child' } })
  await o.emit('permission.asked', { id: 'per_1', sessionID: 'ses_grandchild', permission: 'bash', metadata: { command: 'ls' } })
  assert.equal(o.w.to('/api/bridge/permission')[0].sessionId, SID)
  assert.equal(o.w.answered[0].path.requestID, 'per_1')
  assert.deepEqual(o.w.answeredOld[0].path, { id: 'ses_grandchild', permissionID: 'per_1' })
})

test('a prompt in a session not yet prompted in this run makes the session known to Slorp', async t => {
  const o = await world(t)
  o.w.infos[SID] = { id: SID, title: 'Resumed', directory: '/work/resumed' }
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'slorp-1', answerable: false })
  await o.emit('permission.asked', { id: 'per_1', sessionID: SID, permission: 'bash', metadata: { command: 'ls' } })
  await o.tick()
  assert.deepEqual(o.w.polls(), [{ agent: 'opencode', sessionId: SID, cwd: '/work/resumed', title: 'Resumed', branch: 'main' }])
})

test('what a prompt asks is described by its command, its diff, or its patterns and details', async t => {
  const o = await afterOneTurn(t)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'slorp-1', answerable: false })
  const shown = async (request: object) => {
    const id = 'per_' + o.w.posts.length
    await o.emit('permission.asked', { id, sessionID: SID, ...request })
    const shown = o.w.to('/api/bridge/permission').at(-1)
    await o.emit('permission.replied', { requestID: id })
    return shown
  }
  assert.equal((await shown({ permission: 'edit', patterns: ['a.txt'], metadata: { filepath: '/w/a.txt', diff: '+hello' } })).input, '+hello')
  assert.equal(
    (await shown({ permission: 'external_directory', patterns: ['/tmp/*', '/var/*'], metadata: { parentDir: '/tmp' } })).input,
    '/tmp/*\n/var/*\n{\n  "parentDir": "/tmp"\n}',
  )
  assert.equal((await shown({ permission: 'webfetch', patterns: ['https://example.com'], metadata: {} })).input, 'https://example.com')
  assert.equal((await shown({ permission: 'doom_loop' })).input, '')
})

test('a prompt too long to judge in Slorp is shown cut short and left to OpenCode', async t => {
  const o = await afterOneTurn(t)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'slorp-1', answerable: false })
  for (const length of [8000, 8001]) {
    await o.emit('permission.asked', { id: 'per_' + length, sessionID: SID, permission: 'bash', metadata: { command: 'x'.repeat(length) } })
    await o.emit('permission.replied', { requestID: 'per_' + length })
  }
  const [fits, tooLong] = o.w.to('/api/bridge/permission')
  assert.deepEqual([fits.canAnswer, fits.input.length, fits.summary.length], [true, 8000, 400])
  assert.deepEqual([tooLong.canAnswer, tooLong.input.length], [false, 8000])
})

test('a question carries its options to Slorp and settles when answered in OpenCode', async t => {
  const o = await afterOneTurn(t)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'slorp-1', answerable: false })
  await o.emit('question.asked', { id: 'que_1', sessionID: SID, questions: [{ question: 'Which database?' }, { question: 'Which region?' }] })
  const prompt = o.w.to('/api/bridge/permission')[0]
  assert.equal(prompt.summary, 'Which database?\nWhich region?')
  assert.equal(prompt.canAnswer, true)
  assert.equal(prompt.ordered, true)
  assert.deepEqual(JSON.parse(prompt.input).questions, [
    { question: 'Which database?', options: [], multiSelect: false, custom: true },
    { question: 'Which region?', options: [], multiSelect: false, custom: true },
  ])
  await o.emit('question.replied', { sessionID: SID, requestID: 'que_1', answers: [['Postgres']] })
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [{ id: 'slorp-1' }])

  await o.emit('question.asked', { id: 'que_2', sessionID: SID, questions: [{ question: 'Sure?' }] })
  await o.emit('question.rejected', { sessionID: SID, requestID: 'que_2' })
  assert.equal(o.w.to('/api/bridge/permission/done').length, 2)
})

test('the service address comes from SLORP_URL, then the url option, then the default', async t => {
  const reached = async (env: string | undefined, options?: Record<string, unknown>) => {
    if (env === undefined) delete process.env.SLORP_URL
    else process.env.SLORP_URL = env
    const { w, say } = await world(t, options)
    await say(SID, text('hello'))
    return w.url + w.posts[0]!.path
  }
  const before = process.env.SLORP_URL
  t.after(() => (before === undefined ? delete process.env.SLORP_URL : (process.env.SLORP_URL = before)))

  assert.equal(await reached(undefined), 'http://127.0.0.1:4747/api/bridge/event')
  assert.equal(await reached(undefined, { url: 'http://127.0.0.1:5000//' }), 'http://127.0.0.1:5000/api/bridge/event')
  assert.equal(await reached('', { url: 'http://127.0.0.1:5000' }), 'http://127.0.0.1:5000/api/bridge/event')
  assert.equal(await reached('http://127.0.0.1:6000/', { url: 'http://127.0.0.1:5000' }), 'http://127.0.0.1:6000/api/bridge/event')
})

const usage = (output = 20) => ({ input: 100, output, reasoning: 10, cache: { read: 40, write: 30 } })

test('usage is hydrated, revised by message id, and never counted twice', async t => {
  const o = await world(t)
  const info = { id: 'msg_1', sessionID: SID, role: 'assistant', time: { created: 200 }, providerID: 'openai', modelID: 'test', variant: 'high', tokens: usage(), cost: 0.2 }
  o.w.messages[SID] = [
    { info: { role: 'user', time: { created: 100 } }, parts: [text('earlier prompt')] },
    { info, parts: [text('earlier answer')] },
  ]
  await o.sayWith({ agent: 'build', model: { providerID: 'openai', modelID: 'test' }, variant: 'high' }, { time: { created: 300 } }, text('next'))
  await o.params(SID, 1000)
  await o.emit('message.updated', { info: { ...info, tokens: usage(50), cost: 0.3 } })
  await o.emit('message.updated', { info: { ...info, tokens: usage(50), cost: 0.3 } })
  await o.tick()
  const s = o.w.to('/api/bridge/poll').at(-1).snapshot
  assert.deepEqual(s.used, { in: 100, out: 60, cacheRead: 40, cacheWrite: 30 })
  assert.equal(s.costUsd, 0.3)
  assert.equal(s.contextTokens, 230)
  assert.equal(s.contextWindow, 1000)
  assert.equal(s.model, 'openai/test')
  assert.equal(s.reasoning, 'high')
  assert.equal(s.promptAt, 300)

  await o.emit('message.removed', { sessionID: SID, messageID: 'msg_1' })
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.costUsd, 0)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.used.out, 0)
})

test('a reply preserves the selected agent, provider, model and variant', async t => {
  const o = await world(t)
  await o.sayWith({ agent: 'reviewer', model: { providerID: 'openai', modelID: 'test' }, variant: 'high' }, { time: { created: 100 } }, text('review'))
  o.w.answers['/api/bridge/poll'] = () => ({ replies: [{ id: 'r1', text: 'continue' }] })
  await o.tick()
  assert.deepEqual(o.w.prompted[0].body, { agent: 'reviewer', model: { providerID: 'openai', modelID: 'test' }, variant: 'high', parts: [text('continue')] })
  await o.params(SID, 1000)
  await o.sayWith({ model: { providerID: 'local', modelID: 'small' } }, { time: { created: 200 } }, text('again'))
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.contextWindow, 0)
  assert.equal(o.w.prompted.at(-1).body.variant, undefined)
})

test('snapshots recover missed turns with their original timestamps and clear retry state', async t => {
  const o = await world(t)
  o.w.isDown = true
  await o.sayWith({}, { time: { created: 12345 } }, text('work'))
  await o.emit('session.status', { sessionID: SID, status: { type: 'retry', message: 'rate limited' } })
  o.w.isDown = false
  await o.tick()
  let s = o.w.to('/api/bridge/poll').at(-1).snapshot
  assert.equal(s.promptAt, 12345)
  assert.equal(s.lastPrompt, 'work')
  assert.equal(s.waitingFor, 'Retrying: rate limited')
  await o.emit('session.status', { sessionID: SID, status: { type: 'busy' } })
  o.w.messages[SID] = [user('work'), assistant(text('done'))]
  await o.emit('session.idle', { sessionID: SID })
  await o.tick()
  s = o.w.to('/api/bridge/poll').at(-1).snapshot
  assert.equal(s.status, 'idle')
  assert.equal(s.waitingFor, '')
  assert.equal(s.lastReply, 'done')
})

test('failures survive the following idle event, while cancellation is idle', async t => {
  const o = await afterOneTurn(t)
  await o.emit('session.error', { sessionID: SID, error: { name: 'APIError', data: { message: 'provider unavailable' } } })
  await o.emit('session.idle', { sessionID: SID })
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.status, 'failed')
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.waitingFor, 'provider unavailable')
  await o.emit('session.error', { sessionID: SID, error: { name: 'MessageAbortedError' } })
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.status, 'idle')
})

test('compaction clears context and repeats one stable timeline marker', async t => {
  const o = await afterOneTurn(t)
  await o.emit('message.updated', { info: { id: 'msg_1', role: 'assistant', sessionID: SID, tokens: usage() } })
  await o.emit('message.part.updated', { part: { type: 'compaction', sessionID: SID, auto: true } })
  await o.emit('session.compacted', { sessionID: SID })
  await o.tick()
  const first = o.w.to('/api/bridge/poll').at(-1).snapshot
  await o.tick()
  const second = o.w.to('/api/bridge/poll').at(-1).snapshot
  assert.equal(first.contextTokens, 0)
  assert.equal(first.compacted.auto, true)
  assert.equal(first.compacted.before, 200)
  assert.deepEqual(first.compacted, second.compacted)
})

test('a failed answer lookup is retried on the next poll', async t => {
  const o = await afterOneTurn(t)
  await o.say(SID, text('next'))
  o.w.messageError = { message: 'temporarily unavailable' }
  o.w.messages[SID] = [user('next'), assistant(text('new answer'))]
  await o.emit('session.idle', { sessionID: SID })
  o.w.messageError = undefined
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.lastReply, 'new answer')
})

test('delivery failures are acknowledged and lost acknowledgements are retried without resubmitting', async t => {
  const o = await afterOneTurn(t)
  let claimed = false, ackDown = true
  o.w.answers['/api/bridge/poll'] = () => {
    if (claimed) return { replies: [] }
    claimed = true
    return { replies: [{ id: 'r1', text: 'continue' }] }
  }
  o.w.answers['/api/bridge/ack'] = () => { if (ackDown) throw new Error('lost connection') }
  o.w.promptThrows = true
  await o.tick()
  ackDown = false
  await o.tick()
  assert.equal(o.w.prompted.length, 1)
  assert.deepEqual(o.w.to('/api/bridge/ack'), [{ id: 'r1', state: 'dropped' }, { id: 'r1', state: 'dropped' }])
})

test('a permission missed while Slorp was down is offered after reconnection', async t => {
  const o = await afterOneTurn(t)
  o.w.isDown = true
  await o.emit('permission.asked', { id: 'per_1', sessionID: SID, permission: 'bash', metadata: { command: 'ls' } })
  o.w.isDown = false
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'new', answerable: true })
  o.w.answers['/api/bridge/permission/wait'] = () => ({ state: 'allow' })
  await o.tick()
  assert.equal(o.w.to('/api/bridge/permission').length, 1)
  assert.equal(o.w.answered.length, 1)
})

test('question answers preserve positions, commas, multiple choices, and custom text', async t => {
  const o = await afterOneTurn(t)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'q1', answerable: true })
  const selections = [['A, B', 'C'], ['my own answer']]
  o.w.answers['/api/bridge/permission/wait'] = () => ({ state: 'allow', selections })
  await o.emit('question.asked', { id: 'que_1', sessionID: SID, questions: [
    { question: 'Choose', options: [{ label: 'A, B' }, { label: 'C' }], multiple: true, custom: false },
    { question: 'Choose', options: [{ label: 'D' }] },
  ] })
  const qs = JSON.parse(o.w.to('/api/bridge/permission')[0].input).questions
  assert.equal(qs[0].multiSelect, true)
  assert.equal(qs[0].custom, false)
  assert.equal(qs[1].custom, true)
  assert.equal(o.w.answered[0].url, '/question/{requestID}/reply')
  assert.deepEqual(o.w.answered[0].body, { answers: selections })
})

test('dismissing a question rejects it through the question API', async t => {
  const o = await afterOneTurn(t)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'q1', answerable: true })
  o.w.answers['/api/bridge/permission/wait'] = () => ({ state: 'deny' })
  await o.emit('question.asked', { id: 'que_1', sessionID: SID, questions: [{ question: 'Why?', options: [] }] })
  assert.equal(o.w.answered[0].url, '/question/{requestID}/reject')
  assert.deepEqual(o.w.answeredOld, [])
})

test('a question answered in OpenCode wins over an in-flight Slorp answer', async t => {
  const o = await afterOneTurn(t)
  let decide = (_: unknown) => {}
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'q1', answerable: true })
  o.w.answers['/api/bridge/permission/wait'] = () => new Promise(resolve => { decide = resolve })
  await o.emit('question.asked', { id: 'que_1', sessionID: SID, questions: [{ question: 'Why?', options: [] }] })
  await o.emit('question.replied', { requestID: 'que_1' })
  decide({ state: 'allow', selections: [['too late']] })
  await o.settle()
  assert.deepEqual(o.w.answered, [])
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [{ id: 'q1' }])
})

test('permission API failures are not retried through the legacy API unless the route is missing', async t => {
  const o = await afterOneTurn(t)
  o.w.answerError = { message: 'server error' }
  o.w.answerStatus = 500
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'p1', answerable: true })
  o.w.answers['/api/bridge/permission/wait'] = () => ({ state: 'allow' })
  await o.emit('permission.asked', { id: 'per_1', sessionID: SID, permission: 'bash', metadata: { command: 'ls' } })
  assert.deepEqual(o.w.answeredOld, [])
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [{ id: 'p1' }])
})

test('terminal settlements are retried after a transient Slorp outage', async t => {
  const o = await asked(t, [])
  let down = true
  o.w.answers['/api/bridge/permission/done'] = () => { if (down) throw new Error('unreachable') }
  await o.emit('permission.replied', { requestID: 'per_1' })
  down = false
  await o.tick()
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [{ id: 'slorp-1' }, { id: 'slorp-1' }])
  await o.tick()
  assert.equal(o.w.to('/api/bridge/permission/done').length, 2)
})

test('idle and error events cannot be overtaken by a busy event hydrating history', async t => {
  const o = await world(t)
  let release = () => {}
  o.w.messageGate = new Promise(resolve => { release = resolve })
  const busy = o.dispatch('session.status', { sessionID: SID, status: { type: 'busy' } })
  await o.settle()
  const failure = o.dispatch('session.error', { sessionID: SID, error: { name: 'APIError', data: { message: 'failed' } } })
  const idle = o.dispatch('session.idle', { sessionID: SID })
  release()
  await Promise.all([busy, failure, idle])
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.status, 'failed')
})

test('a permission settled during initial history hydration is never offered', async t => {
  const o = await world(t)
  let release = () => {}
  o.w.messageGate = new Promise(resolve => { release = resolve })
  await o.emit('permission.asked', { id: 'per_1', sessionID: SID, permission: 'bash', metadata: { command: 'ls' } })
  await o.emit('permission.replied', { requestID: 'per_1' })
  release()
  await o.tick()
  assert.deepEqual(o.w.to('/api/bridge/permission'), [])
})

test('concurrent child permissions queue under one parent without replacing each other', async t => {
  const o = await afterOneTurn(t)
  await o.emit('session.created', { info: { id: 'child1', parentID: SID } })
  await o.emit('session.created', { info: { id: 'child2', parentID: SID } })
  let n = 0
  o.w.answers['/api/bridge/permission'] = () => ({ id: `p${++n}`, answerable: false })
  await o.emit('permission.asked', { id: 'per_1', sessionID: 'child1', permission: 'bash', metadata: { command: 'one' } })
  await o.emit('permission.asked', { id: 'per_2', sessionID: 'child2', permission: 'bash', metadata: { command: 'two' } })
  await o.tick()
  await o.tick()
  assert.deepEqual(o.w.to('/api/bridge/permission').map(p => p.input), ['one'])
  await o.emit('permission.replied', { requestID: 'per_1' })
  await o.tick()
  assert.deepEqual(o.w.to('/api/bridge/permission').map(p => p.input), ['one', 'two'])
})

test('a server epoch change restores even an unanswerable prompt when the session is not fresh', async t => {
  const o = await afterOneTurn(t)
  let epoch = 'old'
  o.w.answers['/api/bridge/permission'] = () => ({ id: `p-${epoch}`, epoch, answerable: false })
  o.w.answers['/api/bridge/poll'] = () => ({ fresh: false, epoch, replies: [] })
  await o.emit('permission.asked', { id: 'per_1', sessionID: SID, permission: 'bash', metadata: { command: 'ls' } })
  await o.tick()
  assert.equal(o.w.to('/api/bridge/permission').length, 1)
  epoch = 'new'
  await o.emit('session.status', { sessionID: SID, status: { type: 'busy' } })
  await o.tick()
  assert.equal(o.w.to('/api/bridge/permission').length, 2)
  await o.tick()
  assert.equal(o.w.to('/api/bridge/permission').length, 2)
})

test('an earlier queued idle event cannot settle the next turn’s permission', async t => {
  const o = await afterOneTurn(t)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'next', answerable: false })
  const idle = o.dispatch('session.idle', { sessionID: SID })
  const busy = o.dispatch('session.status', { sessionID: SID, status: { type: 'busy' } })
  const permission = o.dispatch('permission.asked', { id: 'per_next', sessionID: SID, permission: 'bash', metadata: { command: 'next command' } })
  await Promise.all([idle, busy, permission])
  await o.tick()
  assert.equal(o.w.to('/api/bridge/permission').length, 1)
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [])
  await o.emit('permission.replied', { requestID: 'per_next' })
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [{ id: 'next' }])
})

test('opening an idle saved conversation reports it without manufacturing a new turn', async t => {
  const o = await world(t)
  o.w.infos[SID] = { id: SID, title: 'Saved work', directory: '/work/proj', time: { created: 100, updated: 300 } }
  o.w.messages[SID] = [
    { info: { role: 'user', time: { created: 200 } }, parts: [text('original question')] },
    { info: { role: 'assistant', time: { created: 300 } }, parts: [text('original answer')] },
  ]
  await o.open(SID)
  const report = o.w.to('/api/bridge/poll')[0]
  assert.equal(report.title, 'Saved work')
  assert.equal(report.snapshot.status, 'idle')
  assert.equal(report.snapshot.promptAt, 200)
  assert.equal(report.snapshot.activeAt, 300)
  assert.equal(report.snapshot.lastReply, 'original answer')
  assert.deepEqual(o.w.to('/api/bridge/event'), [])
  await o.open(SID)
  assert.equal(o.w.lookups.length, 1)
})

test('presence is local to a directory and only discovers the selected conversation', async t => {
  const o = await world(t)
  o.w.infos[SID] = { id: SID, directory: '/work/proj' }
  o.w.infos.other = { id: 'other', directory: '/work/proj' }
  await o.open(SID, '/other/project')
  await o.open('missing')
  assert.equal(o.w.to('/api/bridge/poll').length, 0)
  o.w.statuses[SID] = { type: 'busy' }
  await o.open(SID)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.status, 'working')
  assert.ok(!o.w.lookups.includes('other'))
  await o.open('other')
  assert.ok(o.w.to('/api/bridge/poll').some(p => p.sessionId === 'other'))
})

test('idle events arriving during discovery win over an earlier busy status reading', async t => {
  const o = await world(t)
  o.w.infos[SID] = { id: SID, directory: '/work/proj' }
  o.w.statuses[SID] = { type: 'busy' }
  let release = () => {}
  o.w.statusGate = new Promise(resolve => { release = resolve })
  await o.open(SID)
  const idle = o.dispatch('session.idle', { sessionID: SID })
  release()
  await idle
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.status, 'idle')
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.status, 'idle')
})

test('the home screen heartbeat keeps terminal companion status without discovering a session', async t => {
  const o = await afterOneTurn(t)
  await o.open()
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).pluginTui, true)
  await o.tick(12000)
  await o.open()
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).pluginTui, true)
  assert.equal(o.w.polls().length, 3)
})

test('bridge version and managed revision identify the loaded installation', async t => {
  const o = await world(t, { revision: 'managed-build' })
  await o.say(SID, text('hello'))
  await o.tick()
  const manifest = JSON.parse(await readFile(new URL('./package.json', import.meta.url), 'utf8'))
  const poll = o.w.to('/api/bridge/poll')[0]
  assert.equal(poll.pluginVersion, manifest.version)
  assert.equal(poll.pluginRevision, 'managed-build')
})
