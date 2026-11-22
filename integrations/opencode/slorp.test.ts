import assert from 'node:assert/strict'
import { after, before, test, type TestContext } from 'node:test'
import { randomUUID } from 'node:crypto'
import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import plugin, { Slorp } from './slorp.ts'
import { PRESENCE_PREFIX } from './presence.ts'

// The harness chooses the native endpoint: one the caller set is put back after.
const inheritedEndpoint = process.env.SLORP_OPENCODE_URL
before(() => { delete process.env.SLORP_OPENCODE_URL })
after(() => { if (inheritedEndpoint === undefined) delete process.env.SLORP_OPENCODE_URL; else process.env.SLORP_OPENCODE_URL = inheritedEndpoint })

const SID = 'sesTop'
const DIR = '/work/proj'
const usage = { input: 100, output: 20, reasoning: 5, cache: { read: 30, write: 10 } }
const info = (id = SID, extra = {}) => ({ id, projectID: 'project', location: { directory: DIR }, time: { created: 100, updated: 200 },
  agent: 'build', model: { providerID: 'openai', id: 'model', variant: 'high' }, tokens: usage, cost: 0, ...extra })
const user = (id = 'msgUser', text = 'hello', at = 300) => ({ id, type: 'user', text, time: { created: at } })
const assistant = (id = 'msgAnswer', text = 'answer', at = 400, extra = {}) => ({ id, type: 'assistant', time: { created: at, completed: at + 1 },
  agent: 'build', model: { providerID: 'openai', id: 'model', variant: 'high' }, tokens: usage, cost: 0.25, content: [{ type: 'text', text }], ...extra })
const settle = async () => { for (let i = 0; i < 25; i++) await new Promise(resolve => setImmediate(resolve)) }

async function world(t: TestContext, managed?: { file: string; run: string }, configure?: (w: any) => void) {
  t.mock.timers.enable({ apis: ['setInterval', 'setTimeout', 'Date'], now: 10000 })
  const w = {
    infos: { [SID]: info() } as Record<string, any>,
    messages: { [SID]: [user(), assistant()] } as Record<string, any[]>,
    active: {} as Record<string, { type: 'running' }>,
    pending: {} as Record<string, any[]>,
    forms: {} as Record<string, any[]>,
    posts: [] as { path: string; body: any; headers: Headers; signal?: AbortSignal | null }[],
    requests: [] as { path: string; method: string; body: any; signal?: AbortSignal | null }[],
    prompts: [] as any[],
    permissions: [] as any[],
    replies: {} as Record<string, (body: any) => any>,
    reads: {} as Record<string, () => Promise<void>>,
    failPrompt: false,
    failPermission: false,
    listCalls: [] as any[],
    listPages: undefined as ((input: any) => any) | undefined,
    hooks: {} as Record<string, (input: any) => Promise<void>>,
    rpc: undefined as any,
    streamSignal: undefined as AbortSignal | undefined,
  }
  configure?.(w)
  const env = { file: process.env.SLORP_MANAGED_FILE, run: process.env.SLORP_MANAGED_RUN }
  if (managed) { process.env.SLORP_MANAGED_FILE = managed.file; process.env.SLORP_MANAGED_RUN = managed.run }
  else { delete process.env.SLORP_MANAGED_FILE; delete process.env.SLORP_MANAGED_RUN }
  const oldFetch = globalThis.fetch
  t.after(() => {
    globalThis.fetch = oldFetch
    for (const [key, value] of [['SLORP_MANAGED_FILE', env.file], ['SLORP_MANAGED_RUN', env.run]]) {
      if (value === undefined) delete process.env[key!]; else process.env[key!] = value
    }
  })
  globalThis.fetch = (async (url: string, init: RequestInit) => {
    const at = new URL(url), body = init.body ? JSON.parse(String(init.body)) : undefined
    if (at.host === '127.0.0.1:4096') {
      w.requests.push({ path: at.pathname + at.search, method: init.method ?? 'GET', body, signal: init.signal })
      if (at.pathname === '/api/info') return Response.json({ pid: process.pid })
      const match = /^\/api\/session\/([^/]+)\/(message|form)(?:\/([^/]+))?(\/reply)?$/.exec(at.pathname)
      const savedMessages = match?.[2] === 'message' ? structuredClone(w.messages[match[1]!] ?? []) : []
      const savedForms = match?.[2] === 'form' ? structuredClone(w.forms[match[1]!] ?? []) : []
      await w.reads[at.pathname]?.()
      if (at.pathname === '/api/session/active') return Response.json({ data: w.active })
      if (match) {
        const [, id, resource, key] = match
        if (resource === 'form') {
          if (init.method !== 'GET') return new Response(null, { status: 204 })
          return Response.json({ data: savedForms })
        }
        const messages = savedMessages
        if (key) return Response.json({ data: messages.find(m => m.id === key) })
        const limit = Number(at.searchParams.get('limit')), order = at.searchParams.get('order')
        const data = order === 'desc' ? [...messages].reverse().slice(0, limit) : messages
        return Response.json({ data, cursor: { next: null } })
      }
      throw new Error(`Unexpected native route: ${url}`)
    }
    w.posts.push({ path: at.pathname, body, headers: new Headers(init.headers), signal: init.signal })
    const abort = new Promise<never>((_, reject) => {
      if (init.signal?.aborted) reject(new Error('aborted'))
      init.signal?.addEventListener('abort', () => reject(new Error('aborted')), { once: true })
    })
    const result = await Promise.race([Promise.resolve().then(() => w.replies[at.pathname]?.(body)), abort])
    return result instanceof Response ? result : result === undefined ? new Response(null, { status: 204 }) : Response.json(result)
  }) as typeof fetch
  const queue: any[] = []
  let wake = () => {}
  const ctx = {
    app: { version: '2.3.2' }, options: { url: 'http://bridge', revision: 'revision', opencodeURL: managed ? undefined : 'http://127.0.0.1:4096' },
    location: { directory: DIR, project: { id: 'project' } },
    session: {
      get: async ({ sessionID }: any) => { const result = structuredClone(w.infos[sessionID]); await w.reads[`info:${sessionID}`]?.(); if (!result) throw new Error('not found'); return result },
      list: async (input: any) => {
        w.listCalls.push(input)
        return w.listPages?.(input) ?? { data: Object.values(w.infos).filter(s => s.parentID === input.parentID), cursor: { next: null } }
      },
      prompt: async (input: any) => { w.prompts.push(input); if (w.failPrompt) throw new Error('unreachable'); return {} },
      hook: async (name: string, callback: any) => { w.hooks[name] = callback; return { dispose: async () => {} } },
    },
    permission: {
      list: async ({ sessionID }: any) => { const result = structuredClone(w.pending[sessionID] ?? []); await w.reads[`permission:${sessionID}`]?.(); return result },
      reply: async (input: any) => { w.permissions.push(input); if (w.failPermission) throw new Error('unreachable') },
    },
    model: { list: async () => ({ data: [{ id: 'model', providerID: 'openai', limit: { context: 128000 }, settings: {}, package: undefined }] }) },
    provider: { get: async () => ({ data: { id: 'openai', package: '@opencode/ai/providers/openai', settings: {} } }) },
    integration: { connection: { active: async () => undefined } },
    vcs: { get: async () => ({ data: { branch: { current: 'main' } } }) },
    rpc: { register: async (_: any, handlers: any) => { w.rpc = handlers; return { dispose: async () => {}, events: { emit: async () => {} } } } },
    event: { subscribe: async function* ({ signal }: any) {
      w.streamSignal = signal
      signal.addEventListener('abort', () => wake(), { once: true })
      while (!signal.aborted) {
        if (!queue.length) await new Promise<void>(resolve => { wake = resolve })
        while (queue.length && !signal.aborted) yield queue.shift()
      }
    } },
  }
  const dispose = await Slorp(ctx as any)
  t.after(dispose)
  await settle()
  let seq = 0
  const terminalId = randomUUID()
  const emit = async (type: string, data: object, extra = {}) => {
    queue.push({ id: `evt${++seq}`, created: Date.now(), type, data, location: { directory: DIR }, ...extra })
    wake()
    await settle()
  }
  return { w, ctx, dispose, emit, settle,
    to: (path: string) => w.posts.filter(p => p.path === path).map(p => p.body),
    snapshot: () => w.posts.filter(p => p.path === '/api/bridge/poll').at(-1)?.body.snapshot,
    tick: async (ms = 2000) => { t.mock.timers.tick(ms); await settle() },
    open: async (sessionId = SID, extra = {}) => {
      await w.rpc.presence({ command: PRESENCE_PREFIX + JSON.stringify({ version: 1, terminalId, sequence: ++seq, directory: DIR, projectID: 'project', sessionId, ...extra }) })
      await settle()
    },
    say: async (text = 'new prompt', id = 'msgNew') => {
      w.messages[SID]!.push(user(id, text, Date.now()))
      await emit('session.inbox.delivered', { sessionID: SID, inboxID: id })
    },
  }
}

test('V2 definition, idle presence discovery and retained snapshot contract', async t => {
  assert.equal(plugin.id, 'slorp')
  assert.equal(plugin.setup, Slorp)
  const o = await world(t)
  assert.equal(o.to('/api/bridge/poll').length, 0)
  await o.open()
  await o.tick()
  const poll = o.to('/api/bridge/poll').at(-1)
  assert.equal(poll.pluginVersion, '0.7.0')
  assert.equal(poll.pluginRevision, 'revision')
  assert.equal(poll.pluginTui, true)
  assert.equal(poll.branch, 'main')
  assert.equal(poll.cwd, DIR)
  assert.equal(poll.snapshot.lastPrompt, 'hello')
  assert.equal(poll.snapshot.lastReply, 'answer')
  assert.equal(poll.snapshot.model, 'openai/model')
  assert.equal(poll.snapshot.reasoning, 'high')
  assert.deepEqual(poll.snapshot.used, { in: 100, out: 25, cacheRead: 30, cacheWrite: 10 })
  assert.equal(poll.snapshot.costUsd, 0.25)
})

test('startup discovers active roots but excludes unrelated locations', async t => {
  const o = await world(t, undefined, w => {
    w.active = { [SID]: { type: 'running' }, sesOther: { type: 'running' } }
    w.infos.sesOther = info('sesOther', { location: { directory: '/other' } })
  })
  await o.tick()
  assert.deepEqual([...new Set(o.to('/api/bridge/poll').map(p => p.sessionId))], [SID])
  assert.equal(o.snapshot().status, 'working')
})

test('presence project, directory, session identity and lease checks fence discovery', async t => {
  const o = await world(t)
  await o.open(SID, { projectID: 'other' })
  await o.open(SID, { directory: '/other' })
  await o.open(SID, { workspaceID: 'unsupported' })
  assert.equal(o.to('/api/bridge/poll').length, 0)
  let release = () => {}
  o.w.reads[`info:${SID}`] = () => new Promise<void>(resolve => { release = resolve })
  await o.open()
  await o.open(SID, { directory: '/other' })
  release()
  await settle()
  assert.equal(o.to('/api/bridge/poll').length, 0)
})

test('presence expiry changes indicator without retiring a live session', async t => {
  const o = await world(t)
  await o.open()
  await o.tick(12000)
  const poll = o.to('/api/bridge/poll').at(-1)
  assert.equal(poll.pluginTui, false)
  assert.equal(poll.sessionId, SID)
})

test('only durable delivered user input starts turns; replies use current V2 session selection', async t => {
  const o = await world(t)
  assert.deepEqual(Object.keys(o.w.hooks), ['context'])
  await o.emit('session.inbox.enqueued', { sessionID: SID, inboxID: 'pending', item: { type: 'user', payload: { text: 'pending' } } })
  assert.equal(o.to('/api/bridge/event').filter(e => e.type === 'turn.start').length, 0)
  await o.say('durable prompt')
  assert.equal(o.to('/api/bridge/event').filter(e => e.type === 'turn.start').at(-1).text, 'durable prompt')
  o.w.replies['/api/bridge/poll'] = () => ({ replies: [{ id: 'reply1', text: 'from Slorp' }] })
  await o.tick()
  assert.deepEqual(o.w.prompts, [{ sessionID: SID, text: 'from Slorp' }])
  assert.deepEqual(o.to('/api/bridge/ack'), [{ id: 'reply1', state: 'delivered' }])
})

test('reply failures are dropped and acknowledgement retries never resubmit input', async t => {
  const o = await world(t)
  await o.open()
  let claim = true, ack = 0
  o.w.failPrompt = true
  o.w.replies['/api/bridge/poll'] = () => claim ? (claim = false, { replies: [{ id: 'reply1', text: 'reply' }] }) : {}
  o.w.replies['/api/bridge/ack'] = () => ++ack === 1 ? new Response(null, { status: 503 }) : {}
  await o.tick()
  await o.tick()
  assert.equal(o.w.prompts.length, 1)
  assert.ok(o.to('/api/bridge/ack').every(a => a.state === 'dropped'))
  assert.ok(ack >= 2)
})

test('context hook reads V2 model catalog and quota capabilities', async t => {
  const o = await world(t)
  await o.open()
  await o.w.hooks.context!({ sessionID: SID, model: { providerID: 'openai', id: 'model' } })
  await settle()
  await o.tick()
  assert.equal(o.snapshot().contextWindow, 128000)
  assert.equal(o.to('/api/bridge/poll').at(-1).providerLimits[0].status, 'auth_required')
})

test('durable selection events outrank stale transcript metadata and can clear a variant', async t => {
  const o = await world(t)
  await o.open()
  assert.equal(o.snapshot().contextWindow, 128000)
  await o.emit('session.agent.selected', { sessionID: SID, agent: 'review' })
  await o.emit('session.model.selected', { sessionID: SID, model: { providerID: 'openai', id: 'model' } })
  await o.emit('session.step.ended', { sessionID: SID })
  await o.tick()
  assert.equal(o.snapshot().reasoning, '')
  assert.equal(o.snapshot().model, 'openai/model')
})

test('retry, completed answer, failure and interruption update bridge status', async t => {
  const o = await world(t)
  await o.say()
  await o.emit('session.retry.scheduled', { sessionID: SID, error: { message: 'rate limited' } })
  await o.tick()
  assert.equal(o.snapshot().waitingFor, 'Retrying: rate limited')
  o.w.messages[SID]!.push(assistant('msgDone', 'finished', Date.now() + 1))
  await o.emit('session.execution.succeeded', { sessionID: SID })
  await o.tick()
  assert.equal(o.snapshot().status, 'idle')
  assert.equal(o.snapshot().lastReply, 'finished')
  await o.emit('session.execution.failed', { sessionID: SID, error: { message: 'provider down' } })
  await o.tick()
  assert.equal(o.snapshot().status, 'failed')
  assert.equal(o.snapshot().waitingFor, 'provider down')
  await o.emit('session.execution.interrupted', { sessionID: SID, reason: 'user' })
  await o.tick()
  assert.equal(o.snapshot().status, 'idle')
})

test('permissions use V2 scoped reply once and preserve denial feedback', async t => {
  const o = await world(t)
  o.w.replies['/api/bridge/permission'] = () => ({ id: 'approval', answerable: true })
  o.w.replies['/api/bridge/permission/wait'] = () => ({ state: 'deny', message: 'wrong environment' })
  await o.emit('permission.asked', { id: 'perm1', sessionID: SID, action: 'shell', resources: ['deploy'], metadata: { command: 'deploy production' } })
  assert.equal(o.to('/api/bridge/permission')[0].input, 'deploy production')
  assert.deepEqual(o.w.permissions, [{ sessionID: SID, requestID: 'perm1', decision: 'reject', message: 'The user denied this in Slorp and said: wrong environment' }])
})

test('terminal permission settlement cancels pending Slorp wait and retries done', async t => {
  const o = await world(t)
  o.w.replies['/api/bridge/permission'] = () => ({ id: 'approval', answerable: true })
  o.w.replies['/api/bridge/permission/wait'] = () => new Promise(() => {})
  let done = 0
  o.w.replies['/api/bridge/permission/done'] = () => ++done === 1 ? new Response(null, { status: 503 }) : {}
  await o.emit('permission.asked', { id: 'perm1', sessionID: SID, action: 'edit', resources: ['file'] })
  const wait = o.w.posts.find(p => p.path.endsWith('/wait'))!
  await o.emit('permission.replied', { requestID: 'perm1', sessionID: SID, decision: 'once' })
  assert.equal(wait.signal?.aborted, true)
  await o.tick()
  assert.ok(done >= 2)
  assert.equal(o.w.permissions.length, 0)
})

test('forms map ordered selections to keyed values and cancellation', async t => {
  const o = await world(t)
  o.w.replies['/api/bridge/permission'] = () => ({ id: randomUUID(), answerable: true })
  o.w.replies['/api/bridge/permission/wait'] = () => ({ state: 'allow', selections: [['Production'], ['One', 'Two'], ['Yes']] })
  const form = { id: 'form1', sessionID: SID, title: 'Deploy', fields: [
    { key: 'env', type: 'string', options: [{ label: 'Production', value: 'prod' }], custom: false },
    { key: 'zones', type: 'multiselect', options: [{ label: 'One', value: '1' }, { label: 'Two', value: '2' }] },
    { key: 'ready', type: 'boolean' },
  ] }
  await o.emit('form.created', { form })
  assert.deepEqual(o.w.requests.find(r => r.method === 'POST' && r.path.endsWith('/reply'))?.body, { answer: { env: 'prod', zones: ['1', '2'], ready: true } })
  o.w.replies['/api/bridge/permission/wait'] = () => ({ state: 'deny' })
  await o.emit('form.created', { form: { ...form, id: 'form2' } })
  assert.ok(o.w.requests.some(r => r.method === 'DELETE' && r.path.endsWith('/form/form2')))
})

test('conditional or external forms and oversized permissions remain terminal-only', async t => {
  const o = await world(t)
  o.w.replies['/api/bridge/permission'] = () => ({ id: randomUUID(), answerable: false })
  await o.emit('form.created', { form: { id: 'form1', sessionID: SID, title: 'Auth', fields: [{ type: 'external', key: 'auth', url: 'https://example.com' }] } })
  assert.equal(o.to('/api/bridge/permission').at(-1).canAnswer, false)
  await o.emit('form.cancelled', { id: 'form1', sessionID: SID })
  await o.emit('permission.asked', { id: 'perm1', sessionID: SID, action: 'shell', resources: ['x'.repeat(9000)] })
  assert.equal(o.to('/api/bridge/permission').at(-1).canAnswer, false)
})

test('history hydrates nested subagents, totals, failed outcomes and epoch redelivery', async t => {
  const o = await world(t, undefined, w => {
    w.infos.sesChild = info('sesChild', { parentID: SID, agent: 'explore', title: 'Inspect' })
    w.infos.sesGrandchild = info('sesGrandchild', { parentID: 'sesChild' })
    w.messages.sesChild = [user('msgChildUser', 'inspect'), assistant('msgChild', 'child report')]
    w.messages.sesGrandchild = [user('msgGrandUser', 'deep inspect'), assistant('msgGrand', 'grandchild report')]
  })
  o.w.replies['/api/bridge/poll'] = () => ({ epoch: 'first' })
  await o.open('sesChild')
  await o.tick()
  const records = o.to('/api/bridge/subagent')
  assert.ok(records.some(r => r.subagent.id === 'sesChild' && r.subagent.depth === 1 && r.report.report === 'child report'))
  assert.ok(records.some(r => r.subagent.id === 'sesGrandchild' && r.subagent.parentId === 'sesChild' && r.subagent.depth === 2))
  assert.equal(o.snapshot().costUsd, 0.75)
  const count = records.length
  await o.tick()
  assert.equal(o.to('/api/bridge/subagent').length, count)
  o.w.replies['/api/bridge/poll'] = () => ({ epoch: 'second' })
  await o.tick()
  assert.equal(o.to('/api/bridge/subagent').length, count + 2)
  await o.emit('session.execution.failed', { sessionID: 'sesChild', error: { message: 'failed' } })
  await o.tick()
  assert.equal(o.to('/api/bridge/subagent').filter(r => r.subagent.id === 'sesChild').at(-1).subagent.status, 'failed')
})

test('child discovery consumes V2 cursors and withheld totals recover after read failure', async t => {
  const o = await world(t)
  o.w.infos.sesChild = info('sesChild', { parentID: SID })
  o.w.messages.sesChild = [assistant()]
  let failed = true
  o.w.listPages = input => {
    if (input.parentID !== SID && input.cursor !== 'page2') return { data: [], cursor: {} }
    if (failed) throw new Error('temporarily unavailable')
    if (!input.cursor) return { data: [], cursor: { next: 'page2' } }
    return { data: [o.w.infos.sesChild], cursor: {} }
  }
  await o.open()
  assert.equal(o.snapshot().used, undefined)
  failed = false
  await o.tick()
  assert.ok(o.w.listCalls.some(c => c.cursor === 'page2'))
  assert.equal(o.snapshot().costUsd, 0.5)
})

test('session deletion fences late hydration and stops bridge traffic', async t => {
  const o = await world(t)
  await o.open()
  let release = () => {}
  o.w.reads[`/api/session/${SID}/message`] = () => new Promise<void>(resolve => { release = resolve })
  await o.emit('session.text.ended', { sessionID: SID })
  await o.emit('session.deleted', { sessionID: SID })
  const before = o.w.posts.length
  release()
  await o.tick()
  assert.equal(o.w.posts.length, before)
})

test('managed activation requires valid control and run token; removal cancels waits', async t => {
  const dir = await mkdtemp(join(tmpdir(), 'slorp-v2-'))
  t.after(() => rm(dir, { recursive: true, force: true }))
  const file = join(dir, 'control.json')
  const o = await world(t, { file, run: 'private-run' })
  await o.say()
  await o.tick()
  assert.equal(o.w.posts.length, 0)
  await writeFile(file, JSON.stringify({ sessionId: SID, opencodeURL: 'http://127.0.0.1:4096' }))
  await o.tick()
  await o.tick()
  assert.ok(o.to('/api/bridge/poll').length > 0)
  assert.ok(o.w.posts.every(p => p.headers.get('X-Slorp-Run') === 'private-run'))
  o.w.replies['/api/bridge/permission'] = () => ({ id: 'approval', answerable: true })
  o.w.replies['/api/bridge/permission/wait'] = () => new Promise(() => {})
  await o.emit('permission.asked', { id: 'perm1', sessionID: SID, action: 'edit', resources: ['file'] })
  const wait = o.w.posts.find(p => p.path.endsWith('/wait'))!
  await rm(file)
  await o.tick()
  assert.equal(wait.signal?.aborted, true)
  const before = o.w.posts.length
  await o.tick()
  assert.equal(o.w.posts.length, before)
})

test('cleanup aborts stream, pending bridge waits and timers', async t => {
  const o = await world(t)
  await o.open()
  o.w.replies['/api/bridge/permission'] = () => ({ id: 'approval', answerable: true })
  o.w.replies['/api/bridge/permission/wait'] = () => new Promise(() => {})
  await o.emit('permission.asked', { id: 'perm1', sessionID: SID, action: 'edit', resources: ['file'] })
  await o.dispose()
  assert.equal(o.w.streamSignal?.aborted, true)
  assert.equal(o.w.posts.find(p => p.path.endsWith('/wait'))!.signal?.aborted, true)
  await settle()
  const before = o.w.posts.length
  await o.tick(60000)
  assert.equal(o.w.posts.length, before)
})

test('late permission and form list snapshots cannot resurrect unknown settled requests', async t => {
  const o = await world(t)
  let permissions = () => {}, forms = () => {}
  o.w.pending[SID] = [{ id: 'perm1', sessionID: SID, action: 'edit', resources: ['file'] }]
  o.w.forms[SID] = [{ id: 'form1', sessionID: SID, title: 'Question', fields: [{ type: 'string', key: 'reply' }] }]
  o.w.reads[`permission:${SID}`] = () => new Promise<void>(resolve => { permissions = resolve })
  o.w.reads[`/api/session/${SID}/form`] = () => new Promise<void>(resolve => { forms = resolve })
  await o.open()
  await o.emit('permission.replied', { requestID: 'perm1', sessionID: SID, decision: 'once' })
  await o.emit('form.cancelled', { id: 'form1', sessionID: SID })
  permissions(); forms()
  await settle()
  assert.equal(o.to('/api/bridge/permission').length, 0)
})

test('failed waiting scans retain settlement fences until both lists finish and then retry', async t => {
  for (const failed of ['permission', 'form']) await t.test(failed, async t => {
    const o = await world(t)
    const permissionPath = `permission:${SID}`, formPath = `/api/session/${SID}/form`
    let fail = (_: Error) => {}, release = () => {}, reads = 0
    const failedPath = failed === 'permission' ? permissionPath : formPath
    const pendingPath = failed === 'permission' ? formPath : permissionPath
    o.w.pending[SID] = [{ id: 'perm1', sessionID: SID, action: 'edit', resources: ['file'] }]
    o.w.forms[SID] = [{ id: 'form1', sessionID: SID, title: 'Question', fields: [{ type: 'string', key: 'reply' }] }]
    o.w.reads[failedPath] = () => { reads++; return new Promise<void>((_, reject) => { fail = reject }) }
    o.w.reads[pendingPath] = () => new Promise<void>(resolve => { release = resolve })
    await o.open()
    await o.emit('permission.replied', { requestID: 'perm1', sessionID: SID, decision: 'once' })
    await o.emit('form.cancelled', { id: 'form1', sessionID: SID })
    fail(new Error('list unavailable'))
    await settle()
    await o.tick()
    assert.equal(reads, 1)
    release()
    await settle()
    assert.equal(o.to('/api/bridge/permission').length, 0)
    o.w.pending[SID] = []
    o.w.forms[SID] = []
    o.w.reads[failedPath] = async () => { reads++ }
    delete o.w.reads[pendingPath]
    await o.tick()
    assert.equal(reads, 2)
    assert.equal(o.to('/api/bridge/permission').length, 0)
  })
})

test('progress storms coalesce history reads into one running read and one dirty refresh', async t => {
  const o = await world(t)
  await o.open()
  let release = () => {}
  const path = `/api/session/${SID}/message`
  o.w.reads[path] = () => new Promise<void>(resolve => { release = resolve })
  const before = o.w.requests.filter(r => r.path.startsWith(path + '?')).length
  for (let i = 0; i < 50; i++) await o.emit('session.tool.progress', { sessionID: SID, id: 'tool', metadata: { status: String(i) } })
  assert.equal(o.w.requests.filter(r => r.path.startsWith(path + '?')).length, before + 1)
  delete o.w.reads[path]
  release()
  await settle()
  assert.equal(o.w.requests.filter(r => r.path.startsWith(path + '?')).length, before + 2)
})

test('revert fences a pending child history snapshot and removes absent usage and report parts', async t => {
  const o = await world(t, undefined, w => {
    w.infos.sesChild = info('sesChild', { parentID: SID })
    w.messages.sesChild = [user('msgChildUser', 'child'), assistant('msgStale', 'stale report')]
  })
  await o.open()
  let release = () => {}
  const path = '/api/session/sesChild/message'
  o.w.reads[path] = () => new Promise<void>(resolve => { release = resolve })
  await o.emit('session.text.ended', { sessionID: 'sesChild' })
  o.w.messages.sesChild = [user('msgChildUser', 'child')]
  await o.emit('session.revert.committed', { sessionID: 'sesChild' })
  delete o.w.reads[path]
  release()
  await settle()
  await o.tick()
  assert.equal(o.snapshot().costUsd, 0.25)
  assert.equal(o.to('/api/bridge/subagent').filter(r => r.subagent.id === 'sesChild').at(-1).report.report, '')
})

test('managed control invalidation aborts an already submitted form answer and prevents retries', async t => {
  const dir = await mkdtemp(join(tmpdir(), 'slorp-v2-form-'))
  t.after(() => rm(dir, { recursive: true, force: true }))
  const file = join(dir, 'control.json')
  await writeFile(file, JSON.stringify({ sessionId: SID, opencodeURL: 'http://127.0.0.1:4096' }))
  const o = await world(t, { file, run: 'private-run' })
  await o.tick()
  await o.tick()
  o.w.replies['/api/bridge/permission'] = () => ({ id: 'approval', answerable: true })
  o.w.replies['/api/bridge/permission/wait'] = () => ({ state: 'allow', selections: [['yes']] })
  let release = () => {}
  const path = `/api/session/${SID}/form/form1/reply`
  o.w.reads[path] = () => new Promise<void>(resolve => { release = resolve })
  await o.emit('form.created', { form: { id: 'form1', sessionID: SID, title: 'Question', fields: [{ type: 'string', key: 'reply' }] } })
  const request = o.w.requests.find(r => r.path === path)!
  assert.ok(request)
  await rm(file)
  await o.tick()
  assert.equal(request.signal?.aborted, true)
  release()
  await o.tick()
  assert.equal(o.w.requests.filter(r => r.path === path).length, 1)
})

test('foreign-location events do not admit roots and moving a local root stops its original bridge loop', async t => {
  const o = await world(t)
  o.w.infos.sesOther = info('sesOther', { location: { directory: '/other' } })
  await o.emit('session.created', { sessionID: 'sesOther', projectID: 'project', location: { directory: '/other' } }, { location: undefined })
  await o.emit('session.execution.started', { sessionID: 'sesOther' }, { location: { directory: '/other' } })
  await o.tick()
  assert.equal(o.w.posts.length, 0)
  await o.open()
  o.w.infos[SID]!.location.directory = '/other'
  await o.emit('session.moved', { sessionID: SID, location: { directory: '/other' } }, { location: { directory: '/other' } })
  const before = o.w.posts.length
  await o.tick()
  assert.equal(o.w.posts.length, before)
})

test('V2 background subagent completion is retained as a handed report', async t => {
  const o = await world(t, undefined, w => {
    w.infos.sesChild = info('sesChild', { parentID: SID, outcome: 'succeeded' })
    w.messages.sesChild = [user('msgChildUser', 'inspect'), assistant('msgChildAnswer', 'child final')]
    w.messages[SID]!.push({
      id: 'msgReport', type: 'synthetic', time: { created: 500 }, text: '<subagent sessionID="sesChild" state="completed" description="Inspect">\nHanded result\n</subagent>',
      metadata: { source: 'subagent', childID: 'sesChild', state: 'completed' },
    })
  })
  await o.open()
  await o.tick()
  const record = o.to('/api/bridge/subagent').find(r => r.subagent.id === 'sesChild')
  assert.equal(record.report.handed, true)
  assert.equal(record.report.report, 'Handed result')
  assert.equal(record.subagent.status, 'completed')
})

test('completed compaction retains billed usage without presenting the discarded context as current', async t => {
  const o = await world(t, undefined, w => {
    w.messages[SID]!.push({ id: 'msgCompact', type: 'compaction', status: 'completed', reason: 'auto', summary: 'Summary',
      time: { created: 600 }, tokens: { ...usage, input: 50 }, cost: 0.1 })
  })
  await o.open()
  await o.tick()
  assert.equal(o.snapshot().contextTokens, 0)
  assert.equal(o.snapshot().used.in, 150)
  assert.equal(o.snapshot().costUsd, 0.35)
  assert.equal(o.snapshot().compacted.auto, true)
  o.w.messages[SID]!.push(assistant('msgAfter', 'After compaction', 800))
  await o.emit('session.step.ended', { sessionID: SID })
  await o.tick()
  assert.equal(o.snapshot().contextTokens, 165)
})
