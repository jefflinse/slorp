import assert from 'node:assert/strict'
import { test, type TestContext } from 'node:test'
import { Slorp } from './slorp.ts'
import { BroadcastChannel } from 'node:worker_threads'
import { readFile } from 'node:fs/promises'

// The world around the plugin: the Slorp service behind fetch, and OpenCode
// behind the client, each answered from memory and recorded.

type Post = { path: string; body: any }
type Message = { info: { role: string; [key: string]: any }; parts: { type: string; text?: string; synthetic?: boolean; [key: string]: any }[] }

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
    messageGates: {} as Record<string, Promise<void>>,
    messageErrors: {} as Record<string, unknown>,
    children: {} as Record<string, any[]>,
    childrenErrors: {} as Record<string, unknown>,
    childrenGates: {} as Record<string, Promise<void>>,
    childLookups: [] as string[],
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
    return out instanceof Response ? out : out === undefined ? new Response(null, { status: 204 }) : Response.json(out)
  }) as typeof fetch

  const client = {
    session: {
      status: async () => { const data = structuredClone(w.statuses); await w.statusGate; return { data } },
      get: async ({ path }: any) => (w.lookups.push(path.id), { data: w.infos[path.id] }),
      children: async ({ path }: any) => {
        w.childLookups.push(path.id)
        const result = { data: structuredClone(w.children[path.id] ?? []), error: w.childrenErrors[path.id] }
        await w.childrenGates[path.id]
        return result
      },
      messages: async ({ path }: any) => {
        const result = { data: structuredClone(w.messages[path.id] ?? []), error: w.messageErrors[path.id] ?? w.messageError }
        await w.messageGate
        await w.messageGates[path.id]
        return result
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

test("subagent turns are delivered under the root, never as root cards or root turns", async t => {
  const { w, say, emit, tick } = await world(t)
  await emit('session.created', { info: { id: 'ses_child', parentID: SID, title: 'Child session - 2026-11-17T18:25:54.371Z' } })
  await say('ses_child', text('explore the repo'))
  await emit('session.idle', { sessionID: 'ses_child' })

  w.infos.ses_resumed = { id: 'ses_resumed', parentID: SID }
  await say('ses_resumed', text('explore some more'))
  await tick()
  assert.deepEqual(w.lookups, [SID, 'ses_resumed'], 'a session the plugin never saw created is looked up, once')
  assert.ok(w.to('/api/bridge/poll').every(p => p.sessionId === SID))
  assert.deepEqual(w.to('/api/bridge/event'), [])
  assert.deepEqual(w.to('/api/bridge/subagent').map(p => [p.sessionId, p.subagent.id]), [[SID, 'ses_child'], [SID, 'ses_resumed']])
  assert.equal(w.to('/api/bridge/poll')[0].snapshot.lastPrompt, '')
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
  await o.emit('message.part.updated', { part: { id: 'prt_compact', type: 'compaction', sessionID: SID, auto: true } })
  await o.emit('session.compacted', { sessionID: SID })
  await o.tick()
  const first = o.w.to('/api/bridge/poll').at(-1).snapshot
  await o.tick()
  const second = o.w.to('/api/bridge/poll').at(-1).snapshot
  assert.equal(first.contextTokens, 0)
  assert.equal(first.compacted.auto, true)
  assert.equal(first.compacted.id, 'prt_compact')
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
  assert.equal(poll.pluginVersion, '0.3.0')
  assert.equal(poll.pluginRevision, 'managed-build')
})

test('reopening after an overflow replay hydrates the original prompt and timestamp', async t => {
  const o = await world(t)
  const doc = JSON.parse(await readFile(new URL('../../internal/opencode/testdata/overflow.json', import.meta.url), 'utf8'))
  o.w.infos[SID] = { ...doc.info, id: SID, directory: '/work/proj' }
  o.w.messages[SID] = doc.messages.filter((m: Message) => !['msg_repeat', 'msg_repeatanswer'].includes(m.info.id)).map((m: Message) => ({ ...m, info: { ...m.info, sessionID: SID } }))
  await o.open(SID)
  const s = o.w.to('/api/bridge/poll').at(-1).snapshot
  assert.equal(s.lastPrompt, 'Apply the fix')
  assert.equal(s.promptAt, 1700000003000)
  assert.equal(s.lastReply, 'Fixed after compaction.')
})

const CHILD = 'ses_child'
const GRANDCHILD = 'ses_grandchild'
const returned = (id: string, state: string, report: string) => `<task id="${id}" state="${state}">\n<${state === 'error' ? 'task_error' : 'task_result'}>\n${report}\n</${state === 'error' ? 'task_error' : 'task_result'}>\n</task>`

function taskPart(parent = SID, child = CHILD, changes: Record<string, any> = {}) {
  return {
    id: `task_${child}`, sessionID: parent, messageID: `work_${parent}`, type: 'tool', tool: 'task',
    state: {
      status: 'running', time: { start: 100 },
      input: { description: 'Find the callers', prompt: 'Find every caller', subagent_type: 'explore' },
      metadata: { parentSessionId: parent, sessionId: child, model: { providerID: 'openai', modelID: 'test' } },
      ...changes,
    },
  }
}

function message(session: string, id: string, role: string, at: number, parts: Message['parts'], extra: object = {}): Message {
  return {
    info: { sessionID: session, id, role, time: { created: at }, ...extra },
    parts: parts.map((p, i) => ({ ...p, id: p.id ?? `${id}_${i}`, sessionID: session, messageID: id })),
  }
}

async function childWorld(t: TestContext) {
  const o = await world(t)
  o.w.infos[SID] = { id: SID, title: 'Root', time: { created: 10, updated: 100 } }
  o.w.children[SID] = [{ id: CHILD, parentID: SID, title: 'Child', time: { created: 100, updated: 140 } }]
  o.w.messages[SID] = [
    message(SID, 'root_user', 'user', 50, [text('Root prompt')]),
    message(SID, `work_${SID}`, 'assistant', 100, [taskPart(), text('Root answer')], { tokens: usage(), cost: 0.1 }),
  ]
  o.w.messages[CHILD] = [
    message(CHILD, 'child_user', 'user', 110, [text('Find every caller')]),
    message(CHILD, 'child_answer', 'assistant', 140, [
      text('There are two callers'), { type: 'reasoning', text: 'private reasoning' },
      { type: 'tool', tool: 'read', state: { status: 'completed', input: { secret: 'tool input' }, output: 'tool output' } },
    ], { tokens: usage(30), cost: 0.2, providerID: 'openai', modelID: 'test', variant: 'high' }),
  ]
  return {
    ...o,
    records: () => o.w.to('/api/bridge/subagent'),
    latest: (id = CHILD) => {
      const records = o.w.to('/api/bridge/subagent').filter(r => r.subagent.id === id)
      return { ...records.at(-1), report: records.findLast(r => r.report)?.report }
    },
    part: (part: object) => o.emit('message.part.updated', { part }),
    msg: async (m: Message) => {
      await o.emit('message.updated', { info: m.info })
      for (const part of m.parts) await o.emit('message.part.updated', { part })
    },
  }
}

test('resumed child trees hydrate recursively, with inclusive root usage and exclusive child usage', async t => {
  const o = await childWorld(t)
  o.w.children[CHILD] = [{ id: GRANDCHILD, parentID: CHILD, time: { created: 120, updated: 160 } }]
  o.w.messages[CHILD]!.push(message(CHILD, `work_${CHILD}`, 'assistant', 150, [taskPart(CHILD, GRANDCHILD, { time: { start: 150 } })]))
  o.w.messages[GRANDCHILD] = [message(GRANDCHILD, 'grand_answer', 'assistant', 160, [text('Nested result')], { tokens: usage(40), cost: 0.3 })]
  await o.open(SID)
  assert.deepEqual(o.w.childLookups, [SID, CHILD, GRANDCHILD])
  assert.deepEqual(o.records().map(r => [r.sessionId, r.subagent.id, r.subagent.parentId, r.subagent.depth]), [[SID, CHILD, '', 1], [SID, GRANDCHILD, CHILD, 2]])
  assert.equal(o.latest().subagent.tools, 2)
  assert.equal(o.latest().subagent.used.out, 40)
  assert.equal(o.latest(GRANDCHILD).subagent.used.out, 50)
  assert.equal(o.latest().subagent.costUsd, 0.2)
  assert.equal(o.latest().subagent.model, 'openai/test')
  assert.equal(o.latest().subagent.effort, 'high')
  assert.deepEqual(o.latest().report, { prompt: 'Find every caller', report: 'There are two callers', handed: false })
  const poll = o.w.to('/api/bridge/poll').at(-1)
  assert.deepEqual(poll.snapshot.used, { in: 300, out: 120, cacheRead: 120, cacheWrite: 90 })
  assert.ok(Math.abs(poll.snapshot.costUsd - 0.6) < 1e-10)
  assert.equal(poll.snapshot.lastPrompt, 'Root prompt')
  assert.equal(poll.snapshot.lastReply, 'Root answer')
  assert.equal(o.w.posts.findIndex(p => p.path === '/api/bridge/poll') < o.w.posts.findIndex(p => p.path === '/api/bridge/subagent'), true)
  await o.msg(message(CHILD, 'child_answer', 'assistant', 140, o.w.messages[CHILD]![1]!.parts, { tokens: usage(60), cost: 0.4 }))
  await o.tick()
  assert.equal(o.latest().subagent.tools, 2)
  assert.equal(o.latest().subagent.used.out, 70)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.used.out, 150)
})

test('failed discovery and child hydration omit totals, retry, and never replace retained totals with empty data', async t => {
  const o = await childWorld(t)
  o.w.childrenErrors[SID] = new Error('children unavailable')
  o.w.messageErrors[CHILD] = new Error('messages unavailable')
  await o.open(SID)
  assert.equal(o.records().length, 0)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.used, undefined)
  delete o.w.childrenErrors[SID]
  delete o.w.messageErrors[CHILD]
  await o.tick()
  assert.equal(o.latest().subagent.used.in, 100)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.used.in, 200)
  const count = o.records().length
  o.w.childrenErrors[CHILD] = new Error('nested discovery failed')
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.used, undefined)
  assert.equal(o.records().length, count)
  delete o.w.childrenErrors[CHILD]
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.used.in, 200)
})

test('children discovered without any parent task parts are hydrated and retried', async t => {
  const o = await childWorld(t)
  o.w.messages[SID]![1]!.parts = [text('Root answer')]
  o.w.childrenErrors[SID] = new Error('offline')
  await o.open(SID)
  assert.equal(o.records().length, 0)
  delete o.w.childrenErrors[SID]
  o.w.statuses[CHILD] = { type: 'retry', message: 'provider overloaded' }
  await o.tick()
  assert.equal(o.latest().subagent.status, 'waiting')
  assert.equal(o.latest().subagent.doing, 'Retrying: provider overloaded')
  assert.equal(o.latest().report.prompt, 'Find every caller')
})

test('only parent terminal task state completes a foreground child and the returned result wins', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  await o.emit('session.idle', { sessionID: CHILD })
  await o.tick()
  assert.equal(o.latest().subagent.done, false)
  await o.part(taskPart(SID, CHILD, { status: 'completed', time: { start: 100, end: 500 }, output: returned(CHILD, 'completed', 'Actual returned report') }))
  await o.tick()
  assert.equal(o.latest().subagent.status, 'completed')
  assert.deepEqual(o.latest().report, { prompt: 'Find every caller', report: 'Actual returned report', handed: true })
  await o.emit('session.status', { sessionID: CHILD, status: { type: 'busy' } })
  await o.emit('session.idle', { sessionID: CHILD })
  await o.msg(message(CHILD, 'late_answer', 'assistant', 145, [text('Delayed fallback')]))
  await o.part(taskPart())
  await o.tick()
  assert.equal(o.latest().subagent.status, 'completed')
  assert.equal(o.latest().report.report, 'Actual returned report')
})

test('background launch ACK is not a report; only a known child’s final synthetic notification is handed back', async t => {
  const o = await childWorld(t)
  o.w.messages[SID]![1]!.parts[0] = taskPart(SID, CHILD, {
    status: 'completed', time: { start: 100, end: 120 },
    metadata: { parentSessionId: SID, sessionId: CHILD, background: true, jobId: CHILD },
    output: returned(CHILD, 'running', 'Launch ACK, not the report'),
  })
  await o.open(SID)
  assert.equal(o.latest().subagent.background, true)
  assert.equal(o.latest().subagent.done, false)
  assert.equal(o.latest().report.handed, false)
  assert.equal(o.latest().report.report, 'There are two callers')
  await o.msg(message(SID, 'foreign', 'user', 500, [text(returned('not_a_child', 'completed', 'Wrong'), { synthetic: true })]))
  await o.tick()
  assert.equal(o.records().length, 1)
  await o.msg(message(SID, 'notification', 'user', 600, [text(returned(CHILD, 'completed', 'Final background result').replace('<task_result>', '<summary>Background finished</summary>\n<task_result>'), { synthetic: true })]))
  await o.tick()
  assert.equal(o.latest().subagent.status, 'completed')
  assert.equal(o.latest().report.report, 'Final background result')
  assert.equal(o.latest().report.handed, true)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.lastPrompt, 'Root prompt')
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.lastReply, 'Root answer')
})

test('background error notifications and legacy returned task output retain source text', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  await o.part(taskPart(SID, CHILD, { status: 'completed', time: { start: 100, end: 500 }, output: `task_id: ${CHILD} (for resuming to continue this task if needed)\n\n<task_result>\nLegacy result\n</task_result>` }))
  await o.tick()
  assert.equal(o.latest().report.report, 'Legacy result')
  assert.equal(o.latest().report.handed, true)
  await o.msg(message(SID, 'failure', 'user', 600, [text(returned(CHILD, 'error', 'Failure returned to parent'), { synthetic: true })]))
  await o.tick()
  assert.equal(o.latest().subagent.status, 'failed')
  assert.equal(o.latest().report.report, 'Failure returned to parent')
})

test('task errors preserve the child mapping when OpenCode drops terminal metadata', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  await o.part({ id: `task_${CHILD}`, sessionID: SID, messageID: `work_${SID}`, type: 'tool', tool: 'task', state: { status: 'error', error: 'tool internals must not be a report', time: { start: 100, end: 500 } } })
  await o.tick()
  assert.equal(o.latest().subagent.status, 'failed')
  assert.equal(o.latest().subagent.type, 'explore')
  assert.equal(o.latest().report.report, 'There are two callers')
  assert.equal(o.latest().report.handed, false)
})

test('permissions, retries, terminal errors and deletion retain child facts without resurrection', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'child-permission', answerable: false })
  await o.emit('permission.asked', { id: 'child-permission', sessionID: CHILD, permission: 'bash', metadata: { command: 'ls' } })
  await o.tick()
  assert.equal(o.latest().subagent.status, 'waiting')
  assert.equal(o.latest().subagent.doing, 'Waiting for bash')
  assert.equal(o.w.to('/api/bridge/permission').at(-1).agentId, CHILD)
  await o.emit('permission.replied', { requestID: 'child-permission' })
  await o.emit('session.status', { sessionID: CHILD, status: { type: 'retry', message: 'try later' } })
  await o.tick()
  assert.equal(o.latest().subagent.doing, 'Retrying: try later')
  await o.emit('session.error', { sessionID: CHILD, error: { name: 'APIError' } })
  await o.emit('message.updated', { info: { ...o.w.messages[CHILD]![1]!.info, time: { created: 140, completed: Date.now() }, error: { name: 'APIError' } } })
  await o.emit('session.idle', { sessionID: CHILD })
  await o.tick()
  assert.equal(o.latest().subagent.status, 'failed')
  await o.emit('session.error', { sessionID: CHILD, error: { name: 'MessageAbortedError' } })
  await o.emit('message.updated', { info: { ...o.w.messages[CHILD]![1]!.info, time: { created: 140, completed: Date.now() }, error: { name: 'MessageAbortedError' } } })
  await o.tick()
  assert.equal(o.latest().subagent.status, 'killed')
  const before = o.latest()
  await o.emit('session.deleted', { info: { id: CHILD } })
  await o.emit('session.status', { sessionID: CHILD, status: { type: 'busy' } })
  await o.msg(message(CHILD, 'after_delete', 'user', 50000, [text('must not reopen')]))
  await o.tick()
  assert.deepEqual(o.latest().report, before.report)
  assert.equal(o.latest().subagent.status, 'killed')
  assert.equal(o.latest().subagent.tools, 1)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.used.in, 200)
  assert.ok(o.w.to('/api/bridge/poll').every(p => p.sessionId === SID))
})

test('reused tasks clear completion immediately, retain the original prompt and accumulate usage and tools', async t => {
  const o = await childWorld(t)
  const completed = taskPart(SID, CHILD, { status: 'completed', time: { start: 100, end: 500 }, output: returned(CHILD, 'completed', 'Old result') })
  o.w.messages[SID]![1]!.parts[0] = completed
  await o.open(SID)
  assert.equal(o.latest().subagent.done, true)
  await o.msg(message(CHILD, 'resume_user', 'user', 2100, [text('Continue with a new request')]))
  await o.tick()
  assert.equal(o.latest().subagent.done, false)
  assert.deepEqual(o.latest().report, { prompt: 'Find every caller', report: '', handed: false })
  const resumed = { ...taskPart(SID, CHILD, { time: { start: 2000 }, input: { description: 'Follow up', prompt: 'Continue with a new request', subagent_type: 'explore', task_id: CHILD } }), id: 'task_resumed' }
  await o.part(resumed)
  await o.part(completed)
  await o.msg(message(CHILD, 'resume_answer', 'assistant', 2200, [text('New answer'), { type: 'tool', tool: 'bash', state: { status: 'completed' } }], { tokens: usage(), cost: 0.3 }))
  await o.tick()
  assert.equal(o.latest().subagent.done, false)
  assert.equal(o.latest().report.report, 'New answer')
  assert.equal(o.latest().subagent.tools, 2)
  assert.equal(o.latest().subagent.used.in, 200)
  assert.equal(o.latest().subagent.costUsd, 0.5)
  assert.equal(o.latest().subagent.startedAt, new Date(100).toISOString())
  await o.part({ ...resumed, state: { ...resumed.state, status: 'completed', time: { start: 2000, end: 2500 }, output: returned(CHILD, 'completed', 'Latest returned report') } })
  await o.tick()
  assert.deepEqual(o.latest().report, { prompt: 'Find every caller', report: 'Latest returned report', handed: true })
})

test('unchanged records are fingerprinted, live polls heartbeat, and epoch changes replay the same revision', async t => {
  const o = await childWorld(t)
  let epoch = 'first'
  o.w.answers['/api/bridge/poll'] = () => ({ replies: [], epoch })
  await o.open(SID)
  const first = o.latest()
  await o.tick(12000)
  await o.tick()
  assert.equal(o.records().length, 1)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).agents[0].status, 'running')
  assert.equal(o.w.to('/api/bridge/poll').at(-1).agents[0].report, undefined)
  epoch = 'restarted'
  await o.tick()
  assert.equal(o.records().length, 2)
  assert.deepEqual(o.latest(), first)
  await o.tick()
  assert.equal(o.records().length, 2)
})

test('failed upserts retry the identical revision only after a successful root poll; 204 is success', async t => {
  const o = await childWorld(t)
  const errors = [404, 409, 500]
  o.w.answers['/api/bridge/subagent'] = () => {
    const status = errors.shift()
    return status ? new Response(null, { status }) : undefined
  }
  await o.open(SID)
  await o.tick()
  o.w.answers['/api/bridge/poll'] = () => new Response(null, { status: 500 })
  await o.tick()
  assert.equal(o.records().length, 2)
  delete o.w.answers['/api/bridge/poll']
  await o.tick()
  await o.tick()
  assert.equal(o.records().length, 4)
  assert.ok(o.latest().revision > 0)
  assert.ok(o.records().every(r => r.revision === o.records()[0].revision))
  await o.tick()
  assert.equal(o.records().length, 4)
  const oldRevision = o.latest().revision
  t.mock.method(Date, 'now', () => 1)
  await o.part(taskPart(SID, CHILD, { status: 'completed', time: { start: 100, end: 500 }, output: returned(CHILD, 'completed', 'Changed') }))
  await o.tick()
  assert.ok(o.latest().revision > oldRevision)
})

test('reports and metadata are rune bounded and worst-case JSON escaping fits the body limit', async t => {
  const o = await childWorld(t)
  const prompt = '😀'.repeat(60001)
  const report = '\u0000'.repeat(60001)
  o.w.messages[CHILD]![1]!.info.providerID = '😀'.repeat(201)
  o.w.messages[CHILD]![1]!.info.variant = '😀'.repeat(81)
  o.w.messages[SID]![1]!.parts[0] = taskPart(SID, CHILD, {
    status: 'completed', time: { start: 100, end: 500 },
    input: { description: '😀'.repeat(1001), prompt, subagent_type: '😀'.repeat(81) },
    output: returned(CHILD, 'completed', report),
  })
  await o.open(SID)
  assert.equal(Array.from(o.latest().report.prompt).length, 60000)
  assert.equal(Array.from(o.latest().report.report).length, 60000)
  assert.equal(Array.from(o.latest().subagent.description).length, 1000)
  assert.equal(Array.from(o.latest().subagent.type).length, 80)
  assert.equal(Array.from(o.latest().subagent.model).length, 200)
  assert.equal(Array.from(o.latest().subagent.effort).length, 80)
  assert.ok(Buffer.byteLength(JSON.stringify(o.latest())) < 1024 * 1024)
  assert.equal(o.latest().report.prompt.endsWith('😀'), true)
  await o.part(taskPart(SID, CHILD, {
    status: 'completed', time: { start: 100, end: 500 },
    input: { prompt: '\u0000'.repeat(60001) }, output: returned(CHILD, 'completed', report),
  }))
  await o.tick()
  assert.equal(o.latest().report.prompt.length, 60000)
  assert.ok(Buffer.byteLength(JSON.stringify(o.latest())) < 1024 * 1024)
})

test('retained child selection is capped and IDs are byte bounded, without dropping usage beyond the display limit', async t => {
  const o = await world(t)
  o.w.children[SID] = Array.from({ length: 201 }, (_, i) => ({ id: i === 0 ? '😀'.repeat(64) : `child_${i}`, parentID: SID }))
  for (const child of o.w.children[SID]) o.w.messages[child.id] = [message(child.id, 'answer', 'assistant', 1, [], { tokens: usage() })]
  o.w.children[SID].push({ id: '😀'.repeat(65), parentID: SID }, { id: '../bad', parentID: SID })
  await o.say(SID, text('root'))
  await o.tick()
  const records = o.w.to('/api/bridge/subagent')
  assert.equal(records.length, 200)
  assert.ok(records.every(r => Buffer.byteLength(r.subagent.id) <= 256))
  assert.equal(Buffer.byteLength(records[0].subagent.id), 256)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.used.in, 20100)
  await o.tick()
  assert.equal(o.w.to('/api/bridge/subagent').length, 200)
})

test('live usage and metadata-free task errors win over an in-flight historical child transcript', async t => {
  const o = await childWorld(t)
  o.w.children[CHILD] = [{ id: GRANDCHILD, parentID: CHILD }]
  o.w.messages[CHILD]!.push(message(CHILD, `work_${CHILD}`, 'assistant', 150, [taskPart(CHILD, GRANDCHILD, { time: { start: 150 } })]))
  let release = () => {}
  o.w.messageGates[CHILD] = new Promise(resolve => { release = resolve })
  await o.open(SID)
  await o.emit('message.updated', { info: { ...o.w.messages[CHILD]![1]!.info, tokens: usage(90), cost: 0.9 } })
  await o.part({ id: `task_${GRANDCHILD}`, sessionID: CHILD, messageID: `work_${CHILD}`, type: 'tool', tool: 'task', state: { status: 'error', time: { start: 150, end: 500 } } })
  release()
  await o.settle()
  assert.equal(o.latest().subagent.used.out, 100)
  assert.equal(o.latest().subagent.costUsd, 0.9)
  assert.equal(o.latest(GRANDCHILD).subagent.status, 'failed')
  assert.equal(o.latest(GRANDCHILD).subagent.type, 'explore')
  assert.equal(o.latest(GRANDCHILD).report.prompt, 'Find every caller')
})

test('a fresh parent task clears an earlier completion before the child user message arrives', async t => {
  const o = await childWorld(t)
  o.w.messages[SID]![1]!.parts[0] = taskPart(SID, CHILD, { status: 'completed', time: { start: 100, end: 500 }, output: returned(CHILD, 'completed', 'Old result') })
  await o.open(SID)
  await o.part({ ...taskPart(SID, CHILD, { time: { start: 2000 }, input: { prompt: 'Follow up', task_id: CHILD } }), id: 'new_task' })
  await o.tick()
  assert.equal(o.latest().subagent.done, false)
  assert.equal(o.latest().subagent.status, 'running')
  assert.deepEqual(o.latest().report, { prompt: 'Find every caller', report: '', handed: false })
})

test('a resumed transcript keeps its original prompt and latest task report regardless of input order', async t => {
  const o = await childWorld(t)
  o.w.messages[SID]![1]!.parts[0] = taskPart(SID, CHILD, { status: 'completed', time: { start: 100, end: 500 }, output: returned(CHILD, 'completed', 'First result') })
  o.w.messages[SID]!.unshift(message(SID, 'resume_task', 'assistant', 2000, [{ ...taskPart(SID, CHILD, {
    status: 'completed', time: { start: 2000, end: 2500 }, input: { prompt: 'Follow up', task_id: CHILD, subagent_type: 'reviewer' }, output: returned(CHILD, 'completed', 'Latest result'),
  }), id: 'second_task' }]))
  o.w.messages[CHILD]!.push(message(CHILD, 'second_user', 'user', 2100, [text('Follow up')]))
  o.w.messages[CHILD]!.push(message(CHILD, 'second_answer', 'assistant', 2200, [text('Latest fallback')], { tokens: usage(50), cost: 0.4 }))
  await o.open(SID)
  assert.equal(o.latest().subagent.type, 'reviewer')
  assert.equal(o.latest().subagent.done, true)
  assert.equal(o.latest().subagent.used.out, 100)
  assert.deepEqual(o.latest().report, { prompt: 'Find every caller', report: 'Latest result', handed: true })
})

test('tool and text part IDs handle updates and deltas without double counting or leaking internals', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  const tool = { id: 'live_tool', sessionID: CHILD, messageID: 'live_answer', type: 'tool', tool: 'bash', state: { status: 'running', title: 'Run checks', output: 'secret output' } }
  await o.msg(message(CHILD, 'live_answer', 'assistant', 300, [text('Checking'), tool, { type: 'reasoning', text: 'secret reasoning' }], { tokens: usage() }))
  await o.part(tool)
  await o.emit('message.part.delta', { sessionID: CHILD, messageID: 'live_answer', partID: 'live_answer_0', field: 'text', delta: ' the tests' })
  await o.tick()
  assert.equal(o.latest().subagent.tools, 2)
  assert.equal(o.latest().subagent.doing, 'Run checks')
  assert.equal(o.latest().report.report, 'Checking the tests')
  await o.part({ ...tool, state: { status: 'completed' } })
  await o.tick()
  assert.equal(o.latest().subagent.tools, 2)
  assert.equal(o.latest().subagent.doing, '')
})

test('deleting an active child retains its fallback report and usage, but never accepts fresh work', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  const before = o.latest()
  await o.emit('session.deleted', { info: { id: CHILD } })
  await o.tick()
  assert.equal(o.latest().subagent.status, 'killed')
  assert.deepEqual(o.latest().report, before.report)
  assert.deepEqual(o.latest().subagent.used, before.subagent.used)
  await o.part({ ...taskPart(SID, CHILD, { time: { start: 10000 }, input: { prompt: 'must not clear retained report' } }), id: 'late_task' })
  await o.say(CHILD, text('must not reopen'))
  await o.tick()
  assert.deepEqual(o.latest().report, before.report)
  assert.equal(o.latest().subagent.status, 'killed')
})

test('returned background reports survive removal of the synthetic parent message', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  await o.msg(message(SID, 'notification', 'user', 600, [text(returned(CHILD, 'completed', 'Final report'), { synthetic: true })]))
  await o.tick()
  await o.emit('message.removed', { sessionID: SID, messageID: 'notification' })
  await o.tick()
  assert.equal(o.latest().subagent.done, true)
  assert.equal(o.latest().report.report, 'Final report')
  assert.equal(o.latest().report.handed, true)
})

test('depth is bounded at 64 and deeper discovery never produces child root cards or partial totals', async t => {
  const o = await world(t)
  let parent = SID
  for (let depth = 1; depth <= 65; depth++) {
    const id = `depth_${depth}`
    o.w.children[parent] = [{ id, parentID: parent }]
    parent = id
  }
  await o.say(SID, text('root'))
  await o.tick()
  const records = o.w.to('/api/bridge/subagent')
  assert.equal(records.length, 64)
  assert.equal(records.at(-1).subagent.depth, 64)
  assert.equal(records.at(-1).subagent.parentId, 'depth_63')
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.used, undefined)
  assert.ok(o.w.to('/api/bridge/poll').every(p => p.sessionId === SID))
})

test('metadata-only changes omit unchanged reports, while restart replay includes them', async t => {
  const o = await childWorld(t)
  let epoch = 'one'
  o.w.answers['/api/bridge/poll'] = () => ({ epoch, replies: [] })
  await o.open(SID)
  await o.emit('session.status', { sessionID: CHILD, status: { type: 'retry', message: 'rate limit' } })
  await o.tick()
  assert.equal(o.records().at(-1).report, undefined)
  const latest = o.latest()
  epoch = 'two'
  await o.tick()
  assert.deepEqual(o.records().at(-1).report, latest.report)
  assert.equal(o.records().at(-1).revision, latest.revision)
})

test('parent idle does not settle a background child’s still-pending permission', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'permission', answerable: false })
  await o.emit('permission.asked', { id: 'permission', sessionID: CHILD, permission: 'bash', metadata: { command: 'ls' } })
  await o.emit('session.idle', { sessionID: SID })
  await o.tick()
  assert.equal(o.latest().subagent.status, 'waiting')
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [])
  await o.emit('session.idle', { sessionID: CHILD })
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [{ id: 'permission' }])
})

function gate() {
  let release = () => {}
  const promise = new Promise<void>(resolve => { release = resolve })
  return { promise, release }
}

test('recoverable context overflow is diagnostic, not a terminal child failure', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  await o.tick()
  await o.emit('session.error', { sessionID: CHILD, error: { name: 'ContextOverflowError' } })
  await o.emit('session.status', { sessionID: CHILD, status: { type: 'busy' } })
  await o.tick()
  assert.equal(o.latest().subagent.done, false)
  assert.equal(o.latest().subagent.status, 'running')
  await o.emit('message.updated', { info: { ...o.w.messages[CHILD]![1]!.info, time: { created: 140, completed: 5000 }, error: { name: 'ContextOverflowError' } } })
  await o.emit('session.idle', { sessionID: CHILD })
  await o.tick()
  assert.equal(o.latest().subagent.status, 'failed', 'persisted error confirms failure when automatic compaction is disabled')
})

test('removed child messages and parts cannot return through an older hydration response', async t => {
  const o = await childWorld(t)
  const held = gate()
  o.w.messageGates[CHILD] = held.promise
  await o.open(SID)
  await o.emit('message.removed', { sessionID: CHILD, messageID: 'child_answer' })
  await o.emit('message.part.removed', { sessionID: CHILD, messageID: 'child_user', partID: 'child_user_0' })
  o.w.messages[CHILD] = []
  held.release()
  await o.settle()
  assert.equal(o.latest().subagent.used.in, 0)
  assert.equal(o.latest().subagent.tools, 0)
  assert.equal(o.latest().report.report, '')
})

test('root deletion during child discovery prevents a later poll from reviving it', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  const held = gate()
  o.w.childrenGates[CHILD] = held.promise
  await o.tick()
  await o.emit('session.deleted', { info: { id: SID } })
  const count = o.w.posts.length
  held.release()
  await o.settle()
  await o.tick()
  assert.equal(o.w.posts.length, count)
})

test('a new traversal invalidates totals before concurrent root snapshots can observe a partial tree', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  o.w.children[CHILD] = [{ id: GRANDCHILD, parentID: CHILD }]
  const held = gate()
  o.w.childrenGates[CHILD] = held.promise
  await o.tick()
  await o.emit('session.status', { sessionID: SID, status: { type: 'busy' } })
  assert.equal(o.w.to('/api/bridge/event').at(-1).snapshot.used, undefined)
  held.release()
  await o.settle()
})

test('truncated foreground task output is the exact handed report, including the truncation notice', async t => {
  const o = await childWorld(t)
  const output = `<task id="${CHILD}" state="completed">\n<task_result>\n${'line\n'.repeat(2000)}\n...200 lines truncated...\nThe full output has been saved to: /tmp/tool-output`
  o.w.messages[SID]![1]!.parts[0] = taskPart(SID, CHILD, {
    status: 'completed', time: { start: 100, end: 500 },
    metadata: { parentSessionId: SID, sessionId: CHILD, truncated: true }, output,
  })
  await o.open(SID)
  assert.equal(o.latest().subagent.done, true)
  assert.equal(o.latest().report.handed, true)
  assert.equal(o.latest().report.report, output)
  const byteLimited = `<task id="${CHILD}" state="completed">\n<task_result>\n${'x'.repeat(50 * 1024)}\n...bytes truncated...\nThe full output has been saved to: /tmp/tool-output`
  await o.part(taskPart(SID, CHILD, { status: 'completed', time: { start: 100, end: 600 }, metadata: { truncated: true }, output: byteLimited }))
  await o.tick()
  assert.equal(o.latest().report.report, byteLimited)
  assert.equal(o.latest().report.handed, true)
})

test('removed child text and tool parts are independently tombstoned during hydration', async t => {
  const o = await childWorld(t)
  o.w.messages[SID]![1]!.parts = []
  const held = gate()
  o.w.messageGates[CHILD] = held.promise
  await o.open(SID)
  for (const [messageID, partID] of [['child_user', 'child_user_0'], ['child_answer', 'child_answer_0'], ['child_answer', 'child_answer_2']]) {
    await o.emit('message.part.removed', { sessionID: CHILD, messageID, partID })
  }
  held.release()
  await o.settle()
  assert.deepEqual(o.latest().report, { prompt: '', report: '', handed: false })
  assert.equal(o.latest().subagent.tools, 0)
  assert.equal(o.latest().subagent.used.in, 100, 'removing parts does not remove message usage')
})

test('root hydration cannot resurrect removed prompts, replies, or usage', async t => {
  const o = await world(t)
  o.w.infos[SID] = { id: SID }
  o.w.messages[SID] = [
    message(SID, 'u', 'user', 100, [text('removed prompt')]),
    message(SID, 'a', 'assistant', 200, [text('removed reply')], { tokens: usage(), cost: 1 }),
    message(SID, 'b', 'assistant', 300, [text('removed part')]),
  ]
  const held = gate()
  o.w.messageGates[SID] = held.promise
  await o.open(SID)
  await o.emit('message.removed', { sessionID: SID, messageID: 'u' })
  await o.emit('message.removed', { sessionID: SID, messageID: 'a' })
  await o.emit('message.part.removed', { sessionID: SID, messageID: 'b', partID: 'b_0' })
  o.w.messages[SID] = []
  held.release()
  await o.settle()
  const snapshot = o.w.to('/api/bridge/poll').at(-1).snapshot
  assert.equal(snapshot.lastPrompt, '')
  assert.equal(snapshot.lastReply, '')
  assert.equal(snapshot.used.in, 0)
  assert.equal(snapshot.costUsd, 0)
})

test('an in-flight last-answer read honors message and part removal tombstones', async t => {
  const o = await afterOneTurn(t)
  o.w.messages[SID] = [
    message(SID, 'u', 'user', 100, [text('prompt')]),
    message(SID, 'a', 'assistant', 200, [text('removed message')]),
    message(SID, 'b', 'assistant', 300, [text('removed text')]),
  ]
  const held = gate()
  o.w.messageGates[SID] = held.promise
  await o.emit('session.idle', { sessionID: SID })
  await o.emit('message.removed', { sessionID: SID, messageID: 'a' })
  await o.emit('message.part.removed', { sessionID: SID, messageID: 'b', partID: 'b_0' })
  held.release()
  await o.settle()
  assert.equal(o.w.to('/api/bridge/event').at(-1).snapshot.lastReply, '')
})

test('live recreation survives but does not admit pre-removal history for the same message ID', async t => {
  const o = await childWorld(t)
  const held = gate()
  o.w.messageGates[CHILD] = held.promise
  await o.open(SID)
  await o.emit('message.removed', { sessionID: CHILD, messageID: 'child_answer' })
  await o.msg(message(CHILD, 'child_answer', 'assistant', 200, [text('Recreated answer')], { tokens: usage(90), cost: 0.9 }))
  held.release()
  await o.settle()
  assert.equal(o.latest().report.report, 'Recreated answer')
  assert.equal(o.latest().subagent.used.out, 100)
  assert.equal(o.latest().subagent.tools, 0, 'old parts from the removed incarnation stay removed')
  await o.emit('message.part.removed', { sessionID: CHILD, partID: 'child_answer_0', messageID: 'child_answer' })
  await o.part({ id: 'child_answer_0', sessionID: CHILD, messageID: 'child_answer', type: 'text', text: 'Recreated part' })
  await o.tick()
  assert.equal(o.latest().report.report, 'Recreated part')
})

test('live updates queued before removal cannot clear newer tombstones', async t => {
  const o = await world(t)
  o.w.infos[SID] = { id: SID }
  const m = message(SID, 'a', 'assistant', 100, [text('must stay removed')], { tokens: usage() })
  o.w.messages[SID] = [m]
  const held = gate()
  o.w.messageGates[SID] = held.promise
  const updating = o.dispatch('message.updated', { info: m.info })
  await o.settle()
  const part = o.dispatch('message.part.updated', { part: m.parts[0] })
  await o.emit('message.removed', { sessionID: SID, messageID: 'a' })
  await o.emit('message.part.removed', { sessionID: SID, messageID: 'a', partID: 'a_0' })
  held.release()
  await Promise.all([updating, part])
  await o.say(SID, text('new turn'))
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.used.in, 0)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.lastReply, '')
})

test('deletion cancels queued polls and events as well as delayed idle answer reads', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  const children = gate(), queue = gate()
  o.w.childrenGates[CHILD] = children.promise
  await o.tick()
  o.w.answers['/api/bridge/event'] = body => body.sessionId === 'blocker' ? queue.promise : undefined
  await o.say('blocker', text('hold the send queue'))
  children.release()
  await o.settle()
  await o.emit('session.status', { sessionID: SID, status: { type: 'busy' } })
  await o.say(SID, text('queued turn'))
  await o.emit('session.idle', { sessionID: SID })
  await o.emit('session.deleted', { info: { id: SID } })
  const count = o.w.posts.filter(p => p.body.sessionId === SID).length
  queue.release()
  await o.settle()
  assert.equal(o.w.posts.filter(p => p.body.sessionId === SID).length, count)
})

test('root deletion cancels an upsert already queued behind another root’s event', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  await o.part(taskPart(SID, CHILD, { status: 'completed', time: { start: 100, end: 500 }, output: returned(CHILD, 'completed', 'new report') }))
  const polled = gate(), queue = gate()
  o.w.answers['/api/bridge/poll'] = () => polled.promise
  await o.tick()
  o.w.answers['/api/bridge/event'] = body => body.sessionId === 'blocker' ? queue.promise : undefined
  await o.say('blocker', text('hold the queue after poll'))
  polled.release()
  await o.settle()
  await o.emit('session.deleted', { info: { id: SID } })
  const count = o.records().length
  queue.release()
  await o.settle()
  assert.equal(o.records().length, count)
})

test('a poll response received after root deletion cannot deliver replies or children', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  const held = gate()
  o.w.answers['/api/bridge/poll'] = async () => {
    await held.promise
    return { replies: [{ id: 'late', text: 'must not resume deleted root' }] }
  }
  await o.tick()
  await o.emit('session.deleted', { info: { id: SID } })
  const count = o.w.posts.length
  held.release()
  await o.settle()
  assert.equal(o.w.prompted.length, 0)
  assert.equal(o.w.posts.length, count)
})

test('root deletion during initial hydration or idle refresh prevents subsequent outgoing events', async t => {
  for (const phase of ['initial', 'idle']) await t.test(phase, async t => {
    const o = phase === 'initial' ? await world(t) : await afterOneTurn(t)
    o.w.infos[SID] = { id: SID }
    const held = gate()
    o.w.messageGates[SID] = held.promise
    if (phase === 'initial') await o.open(SID)
    else await o.emit('session.idle', { sessionID: SID })
    await o.emit('session.deleted', { info: { id: SID } })
    const count = o.w.posts.length
    held.release()
    await o.settle()
    await o.tick()
    assert.equal(o.w.posts.length, count)
  })
})

test('graph changes during a traversal require a fresh complete generation before publishing totals', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  const held = gate()
  o.w.childrenGates[SID] = held.promise
  await o.tick()
  await o.emit('session.created', { info: { id: 'new_child', parentID: SID } })
  held.release()
  await o.settle()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.used, undefined)
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.used.in, 200)
  await o.emit('session.created', { info: { id: 'new_grandchild', parentID: CHILD } })
  await o.emit('session.status', { sessionID: SID, status: { type: 'busy' } })
  assert.equal(o.w.to('/api/bridge/event').at(-1).snapshot.used, undefined)
})

test('truncated background launch output never becomes a handed report or terminal state', async t => {
  const o = await childWorld(t)
  o.w.messages[SID]![1]!.parts[0] = taskPart(SID, CHILD, {
    status: 'completed', time: { start: 100, end: 500 },
    metadata: { parentSessionId: SID, sessionId: CHILD, background: true, truncated: true },
    output: `<task id="${CHILD}" state="running">\n<task_result>\nLaunch acknowledgement\n...truncated...`,
  })
  await o.open(SID)
  assert.equal(o.latest().subagent.done, false)
  assert.equal(o.latest().subagent.status, 'running')
  assert.equal(o.latest().report.handed, false)
  assert.equal(o.latest().report.report, 'There are two callers')
})

test('slow tree discovery uses bounded parallelism without stalling heartbeats, other roots, or permissions', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  await o.say('other_root', text('other work'))
  const held = gate()
  for (let i = 0; i < 8; i++) {
    const id = `sibling_${i}`
    o.w.children[SID]!.push({ id, parentID: SID })
    o.w.childrenGates[id] = held.promise
  }
  const before = o.w.childLookups.length
  await o.tick()
  const waiting = o.w.childLookups.slice(before).filter(id => id.startsWith('sibling_'))
  assert.ok(waiting.length > 1 && waiting.length <= 4)
  assert.ok(o.w.to('/api/bridge/poll').some(p => p.sessionId === 'other_root'))
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'permission', answerable: false })
  await o.emit('permission.asked', { id: 'permission', sessionID: CHILD, permission: 'bash', metadata: { command: 'ls' } })
  assert.equal(o.w.to('/api/bridge/permission').at(-1).agentId, CHILD)
  const polls = o.w.to('/api/bridge/poll').length
  await o.tick()
  assert.ok(o.w.to('/api/bridge/poll').length >= polls + 2)
  const heartbeat = o.w.to('/api/bridge/poll').findLast(p => p.sessionId === SID)
  assert.equal(heartbeat.snapshot.used, undefined)
  assert.equal(heartbeat.agents.find((a: any) => a.id === CHILD).status, 'waiting')
  held.release()
  await o.settle()
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').findLast(p => p.sessionId === SID).snapshot.used.in, 200)
})

test('another root discovers and delivers a new child while the first root’s discovery remains pending', async t => {
  const o = await childWorld(t)
  await o.open(SID)
  const other = 'other_root', child = 'other_child'
  await o.say(other, text('other work'))
  const held = gate()
  o.w.childrenGates[CHILD] = held.promise
  const firstLookups = o.w.childLookups.filter(id => id === CHILD).length
  await o.tick()
  assert.ok(o.w.to('/api/bridge/poll').some(p => p.sessionId === other))

  o.w.children[other] = [{ id: child, parentID: other }]
  o.w.messages[child] = [message(child, 'answer', 'assistant', 100, [text('Independent result')], { tokens: usage(), cost: 0.4 })]
  await o.emit('session.created', { info: { id: child, parentID: other } })
  await o.tick()
  const delivered = o.records().filter(r => r.sessionId === other)
  assert.equal(delivered.length, 1, 'B upserts its new child before A’s gate is released')
  assert.equal(delivered[0].subagent.id, child)
  assert.equal(delivered[0].report.report, 'Independent result')
  assert.equal(delivered[0].subagent.used.in, 100)
  assert.equal(o.w.to('/api/bridge/poll').findLast(p => p.sessionId === other).snapshot.used.in, 100)

  let claimed = false
  o.w.answers['/api/bridge/poll'] = body => {
    if (body.sessionId !== other || claimed) return { replies: [] }
    claimed = true
    return { replies: [{ id: 'independent-reply', text: 'continue B' }] }
  }
  await o.tick()
  assert.equal(o.w.prompted.length, 1)
  assert.equal(o.w.prompted[0].path.id, other)
  assert.deepEqual(o.w.to('/api/bridge/ack'), [{ id: 'independent-reply', state: 'delivered' }])
  await o.tick()
  assert.equal(o.w.prompted.length, 1)
  assert.equal(o.records().filter(r => r.sessionId === other).length, 1)
  assert.equal(o.w.childLookups.filter(id => id === CHILD).length, firstLookups + 1, 'A does not start duplicate traversals')
  held.release()
  await o.settle()
})
