import assert from 'node:assert/strict'
import { test, type TestContext } from 'node:test'
import { Slorp } from './slorp.ts'

// The world around the plugin: the Slorp service behind fetch, and OpenCode
// behind the client, each answered from memory and recorded.

type Post = { path: string; body: any }
type Message = { info: { role: string }; parts: { type: string; text?: string; synthetic?: boolean }[] }

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
    /** Permission answers sent to the route that carries a message. */
    answered: [] as any[],
    answerError: undefined as unknown,
    /** Permission answers sent to the older route. */
    answeredOld: [] as any[],
    to: (path: string) => w.posts.filter(p => p.path === path).map(p => p.body),
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
    const out = await Promise.race([w.answers[at.pathname]?.(body), gone])
    return out === undefined ? new Response(null, { status: 204 }) : Response.json(out)
  }) as typeof fetch

  const client = {
    session: {
      get: async ({ path }: any) => (w.lookups.push(path.id), { data: w.infos[path.id] }),
      messages: async ({ path }: any) => ({ data: w.messages[path.id] ?? [] }),
      promptAsync: async (o: any) => (w.prompted.push(o), { error: w.promptError }),
    },
    vcs: { get: async () => ({ data: { branch: 'main' } }) },
    _client: { post: async (o: any) => (w.answered.push(o), { error: w.answerError }) },
    postSessionIdPermissionsPermissionId: async (o: any) => (w.answeredOld.push(o), {}),
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
    tick: async (ms = 2000) => {
      t.mock.timers.tick(ms)
      await settle()
    },
    say: async (sessionID: string, ...parts: Message['parts']) => {
      await hooks['chat.message']({ sessionID }, { parts })
      await settle()
    },
    dispose: () => hooks.dispose(),
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
  assert.deepEqual(w.to('/api/bridge/event')[1], { agent: 'opencode', sessionId: SID, type: 'turn.complete', text: 'It is fixed.\n\nTests pass.' })
})

test('a turn that wrote nothing keeps the answer before it', async t => {
  const { w, say, emit } = await afterOneTurn(t)
  await say(SID, text('now deploy'))
  w.messages[SID] = [user('fix the build'), assistant(text('It is fixed.')), user('now deploy'), assistant({ type: 'tool' })]
  await emit('session.idle', { sessionID: SID })
  assert.equal(w.to('/api/bridge/event').at(-1).text, 'It is fixed.')
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
  assert.deepEqual(w.to('/api/bridge/event').map(e => e.type), ['turn.start', 'turn.complete'])
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
  assert.equal(w.to('/api/bridge/event').at(-1).text, '')
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
  assert.deepEqual(w.to('/api/bridge/poll'), [
    { agent: 'opencode', sessionId: SID, cwd: '/work/proj', title: 'Fix the build', branch: 'main' },
    { agent: 'opencode', sessionId: 'ses_new', cwd: '/work/other', title: '', branch: 'main' },
  ])

  await emit('vcs.branch.updated', { branch: 'release' })
  await emit('session.deleted', { info: { id: 'ses_new' } })
  await tick()
  assert.deepEqual(w.to('/api/bridge/poll').slice(2), [{ agent: 'opencode', sessionId: SID, cwd: '/work/proj', title: 'Fix the build', branch: 'release' }])
})

test('a session known only by its id is polled for with the instance directory', async t => {
  const { w, say, tick } = await world(t)
  await say('ses_unknown', text('hello'))
  await tick()
  assert.deepEqual(w.to('/api/bridge/poll'), [{ agent: 'opencode', sessionId: 'ses_unknown', cwd: '/work/proj', title: '', branch: 'main' }])
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
  assert.deepEqual(w.to('/api/bridge/event'), [
    { agent: 'opencode', sessionId: SID, type: 'turn.start', text: 'fix the build' },
    { agent: 'opencode', sessionId: SID, type: 'turn.complete', text: 'It is fixed.' },
  ])

  await say(SID, text('now deploy'))
  w.posts.length = 0
  await tick()
  assert.deepEqual(w.to('/api/bridge/event'), [{ agent: 'opencode', sessionId: SID, type: 'turn.start', text: 'now deploy' }])

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
  assert.deepEqual(o.w.to('/api/bridge/poll'), [{ agent: 'opencode', sessionId: SID, cwd: '/work/resumed', title: 'Resumed', branch: 'main' }])
})

test('what a prompt asks is described by its command, its diff, or its patterns and details', async t => {
  const o = await afterOneTurn(t)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'slorp-1', answerable: false })
  const shown = async (request: object) => {
    await o.emit('permission.asked', { id: 'per_' + o.w.posts.length, sessionID: SID, ...request })
    return o.w.to('/api/bridge/permission').at(-1)
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
  }
  const [fits, tooLong] = o.w.to('/api/bridge/permission')
  assert.deepEqual([fits.canAnswer, fits.input.length, fits.summary.length], [true, 8000, 400])
  assert.deepEqual([tooLong.canAnswer, tooLong.input.length], [false, 8000])
})

test('a question is shown in Slorp but can only be answered in OpenCode', async t => {
  const o = await afterOneTurn(t)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'slorp-1', answerable: false })
  await o.emit('question.asked', { id: 'que_1', sessionID: SID, questions: [{ question: 'Which database?' }, { question: 'Which region?' }] })
  assert.deepEqual(o.w.to('/api/bridge/permission'), [
    { agent: 'opencode', sessionId: SID, tool: 'question', summary: 'Which database?\nWhich region?', input: 'Which database?\nWhich region?', canAnswer: false, question: true },
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
