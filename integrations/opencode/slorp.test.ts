import assert from 'node:assert/strict'
import { test, type TestContext } from 'node:test'
import { Slorp } from './slorp.ts'
import { randomUUID } from 'node:crypto'
import { PRESENCE_PREFIX } from './presence.ts'
import { mkdir, mkdtemp, readFile, rename, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

// The world around the plugin: the Slorp service behind fetch, and OpenCode
// behind the client, each answered from memory and recorded.

type Post = { path: string; body: any; headers: Headers; signal?: AbortSignal | null }
type Message = { info: { role: string; [key: string]: any }; parts: { type: string; text?: string; synthetic?: boolean; [key: string]: any }[] }

const SID = 'ses_top'

async function world(t: TestContext, options?: Record<string, unknown>, managedEnv: { file?: string; run?: string } = {}) {
  const w = {
    url: '',
    /** What the Slorp service was sent, in order. */
    posts: [] as Post[],
    /** What the Slorp service answers, by path. Unlisted paths answer with no body. */
    answers: {} as Record<string, (body: any) => unknown>,
    isDown: false,
    infos: {} as Record<string, object>,
    infoGates: {} as Record<string, Promise<void>>,
    infoWait: undefined as ((id: string, signal: AbortSignal) => Promise<void>) | undefined,
    lookups: [] as string[],
    lookupRequests: [] as any[],
    infoErrors: {} as Record<string, unknown>,
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
    answerGate: undefined as Promise<void> | undefined,
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
    w.posts.push({ path: at.pathname, body, headers: new Headers(init.headers), signal: init.signal })
    const gone = new Promise<never>((_, reject) => init.signal?.addEventListener('abort', () => reject(new Error('aborted'))))
    const out = await Promise.race([Promise.resolve().then(() => w.answers[at.pathname]?.(body)), gone])
    return out instanceof Response ? out : out === undefined ? new Response(null, { status: 204 }) : Response.json(out)
  }) as typeof fetch

  const client = {
    session: {
      status: async () => { const data = structuredClone(w.statuses); await w.statusGate; return { data } },
      get: async ({ path, ...request }: any) => {
        w.lookups.push(path.id)
        w.lookupRequests.push({ path, ...request })
        const data = w.infos[path.id] && structuredClone({ directory: '/work/proj', projectID: 'project', ...w.infos[path.id] })
        await w.infoGates[path.id]
        await w.infoWait?.(path.id, request.signal)
        return { data, error: w.infoErrors[path.id] }
      },
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
    _client: { post: async ({ signal, ...o }: any) => {
      w.answered.push(o)
      const result = { error: w.answerError, response: w.answerStatus ? new Response(null, { status: w.answerStatus }) : undefined }
      await w.answerGate
      return result
    } },
    postSessionIdPermissionsPermissionId: async ({ signal, ...o }: any) => (w.answeredOld.push(o), {}),
  }

  t.mock.timers.reset()
  t.mock.timers.enable({ apis: ['setInterval', 'setTimeout', 'Date'] })
  const controlTicks: Promise<unknown>[] = []
  if (managedEnv.file !== undefined || managedEnv.run !== undefined) {
    const interval = globalThis.setInterval
    t.mock.method(globalThis, 'setInterval', (callback: () => Promise<unknown>, ms: number) => interval(() => {
      controlTicks.push(callback())
    }, ms))
  }
  const oldFile = process.env.SLORP_MANAGED_FILE, oldRun = process.env.SLORP_MANAGED_RUN
  const setEnv = (file?: string, run?: string) => {
    if (file === undefined) delete process.env.SLORP_MANAGED_FILE
    else process.env.SLORP_MANAGED_FILE = file
    if (run === undefined) delete process.env.SLORP_MANAGED_RUN
    else process.env.SLORP_MANAGED_RUN = run
  }
  let hooks: any
  try {
    setEnv(managedEnv.file, managedEnv.run)
    hooks = await Slorp({ client, directory: '/work/proj', project: { id: 'project' } } as any, options)
  } finally {
    setEnv(oldFile, oldRun)
  }
  t.after(() => hooks.dispose())

  /** Lets everything the plugin has started run as far as it can. */
  const settle = async () => {
    for (let i = 0; i < 20; i++) await new Promise(resolve => setImmediate(resolve))
  }
  const emit = async (type: string, properties: object) => {
    await hooks.event({ event: { type, properties } })
    await settle()
  }
  const terminalId = randomUUID()
  let sequence = 0
  const announce = (payload: object) => emit('tui.command.execute', { command: PRESENCE_PREFIX + JSON.stringify({
    version: 1, terminalId, sequence: ++sequence, directory: '/work/proj', projectID: 'project', ...payload,
  }) })
  return {
    w,
    settle,
    emit,
    announce,
    dispatch: (type: string, properties: object) => hooks.event({ event: { type, properties } }),
    open: async (sessionId?: string, directory = '/work/proj') => {
      await announce({ directory, sessionId })
    },
    tick: async (ms = 2000) => {
      t.mock.timers.tick(ms)
      await Promise.all(controlTicks.splice(0))
      await settle()
    },
    say: async (sessionID: string, ...parts: Message['parts']) => {
      await hooks['chat.message']({ sessionID }, { parts })
      await settle()
    },
    dispose: () => hooks.dispose(),
    restart: async () => {
      await hooks.dispose()
      hooks = await Slorp({ client, directory: '/work/proj', project: { id: 'project' } } as any, options)
    },
    params: async (sessionID: string, context: number) => hooks['chat.params']({ sessionID, model: { limit: { context } } }),
    runtimeParams: async (sessionID: string, provider: object, model: object) => hooks['chat.params']({ sessionID, provider, model }),
    sayWith: async (input: object, message: object, ...parts: Message['parts']) => {
      await hooks['chat.message']({ sessionID: SID, ...input }, { message, parts })
      await settle()
    },
  }
}

const text = (t: string, more: object = {}) => ({ type: 'text', text: t, ...more })
const assistant = (...parts: Message['parts']): Message => ({ info: { role: 'assistant' }, parts })
const user = (t: string): Message => ({ info: { role: 'user' }, parts: [text(t)] })

function quotaRuntime(fetch: (url: string, init: RequestInit) => Promise<Response>) {
  return {
    provider: { id: 'openai', options: { apiKey: 'opencode-oauth-dummy-key', fetch }, key: 'private-runtime-reference', models: {} },
    model: { providerID: 'openai', api: { url: 'https://api.openai.com/v1' }, limit: { context: 128000 } },
  }
}

const quotaBody = () => ({
  account_id: 'private-account', email: 'private-email',
  rate_limit: { primary_window: { used_percent: 42, limit_window_seconds: 18000, reset_at: 1800000000 } },
})

test('account quotas start only at chat.params and repeat sanitized cached reports without altering ordinary limits', async t => {
  const o = await world(t)
  let calls = 0
  const runtime = quotaRuntime(async (url, init) => {
    calls++
    assert.equal(url, 'https://chatgpt.com/backend-api/wham/usage')
    assert.equal(init.headers, undefined)
    assert.equal(init.redirect, 'error')
    return Response.json(quotaBody())
  })
  const native = runtime.provider.options.fetch
  await o.say(SID, text('hello'))
  await o.tick()
  assert.equal(calls, 0)
  assert.ok(!('providerLimits' in o.w.to('/api/bridge/poll').at(-1)))
  assert.ok(!('providerLimitsEpoch' in o.w.to('/api/bridge/poll').at(-1)))
  assert.ok(!('providerLimitsRevision' in o.w.to('/api/bridge/poll').at(-1)))
  await o.runtimeParams(SID, runtime.provider, runtime.model)
  await o.settle()
  await o.tick()
  const first = o.w.to('/api/bridge/poll').at(-1)
  assert.equal(first.snapshot.contextWindow, 128000)
  assert.equal(first.providerLimits[0].windows[0].percentUsed, 42)
  assert.equal(first.providerLimits[0].provider, 'openai-codex')
  assert.equal(first.pluginVersion, '0.6.0')
  assert.match(first.providerLimitsEpoch, /^[a-f0-9-]{36}$/)
  assert.ok(Number.isSafeInteger(first.providerLimitsRevision) && first.providerLimitsRevision > 0)
  assert.equal(runtime.provider.options.fetch, native)
  await o.tick()
  assert.equal(calls, 1)
  assert.deepEqual(o.w.to('/api/bridge/poll').at(-1).providerLimits, first.providerLimits)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).providerLimitsEpoch, first.providerLimitsEpoch)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).providerLimitsRevision, first.providerLimitsRevision)
  assert.doesNotMatch(JSON.stringify(o.w.posts), /private-runtime|private-account|private-email|opencode-oauth-dummy-key|ChatGPT-Account-Id/)
  await o.tick(56_000)
  assert.equal(calls, 2, 'refresh sixty seconds after the first observation, not on a constructor-aligned interval')
})

test('descendant runtime quotas are associated with the top root and removed as an entire snapshot', async t => {
  const o = await world(t)
  const child = 'ses_quota_child', grandchild = 'ses_quota_grandchild'
  o.w.infos[child] = { id: child, parentID: SID }
  o.w.infos[grandchild] = { id: grandchild, parentID: child }
  await o.say(grandchild, text('child work'))
  await o.params(SID, 32000)
  const runtime = quotaRuntime(async () => Response.json(quotaBody()))
  await o.runtimeParams(grandchild, runtime.provider, runtime.model)
  await o.settle()
  await o.tick()
  let poll = o.w.to('/api/bridge/poll').at(-1)
  assert.equal(poll.sessionId, SID)
  assert.equal(poll.snapshot.contextWindow, 32000, 'child context capacity is not the root capacity')
  assert.equal(poll.providerLimits[0].status, 'ok')
  const revision = poll.providerLimitsRevision, epoch = poll.providerLimitsEpoch
  assert.ok(o.w.to('/api/bridge/poll').every(p => p.sessionId === SID))
  await o.emit('session.deleted', { info: { id: child } })
  await o.tick()
  poll = o.w.to('/api/bridge/poll').at(-1)
  assert.deepEqual(poll.providerLimits, [])
  assert.ok(poll.providerLimitsRevision > revision)
  assert.equal(poll.providerLimitsEpoch, epoch)
  assert.equal(poll.snapshot.contextWindow, 32000)
})

test('quota hooks do not await session lookups or providers, and superseded lookups cannot activate old contexts', async t => {
  const o = await world(t)
  const held = gate()
  o.w.infoGates[SID] = held.promise
  let calls = 0
  const runtime = quotaRuntime(async () => { calls++; return Response.json(quotaBody()) })
  await o.runtimeParams(SID, runtime.provider, runtime.model)
  assert.equal(calls, 0)
  await o.runtimeParams(SID, { id: 'unknown' }, { providerID: 'unknown', limit: { context: 1000 } })
  held.release()
  await o.settle()
  assert.equal(calls, 0)
  await o.say(SID, text('hello'))
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).snapshot.contextWindow, 1000)
  assert.ok(!('providerLimits' in o.w.to('/api/bridge/poll').at(-1)))
  const waiting = gate()
  runtime.provider.options.fetch = async () => { calls++; await waiting.promise; return Response.json(quotaBody()) }
  await o.runtimeParams(SID, runtime.provider, runtime.model)
  await o.settle()
  assert.equal(calls, 1)
  await o.runtimeParams(SID, { id: 'unknown' }, { providerID: 'unknown', limit: { context: 2000 } })
  await o.settle()
  waiting.release()
  await o.tick()
  assert.deepEqual(o.w.to('/api/bridge/poll').at(-1).providerLimits, [])
})

test('quota hooks absorb failed session resolution and root deletion or disposal abort active provider reads', async t => {
  for (const action of ['delete', 'dispose', 'lookup-error'] as const) await t.test(action, async t => {
    const o = await world(t)
    let signal: AbortSignal | null | undefined
    const held = gate()
    const runtime = quotaRuntime(async (_url, init) => { signal = init.signal; await held.promise; return Response.json(quotaBody()) })
    if (action === 'lookup-error') o.w.infoErrors[SID] = new Error('private lookup error')
    else await o.say(SID, text('work'))
    await o.runtimeParams(SID, runtime.provider, runtime.model)
    await o.settle()
    if (action === 'lookup-error') assert.equal(signal, undefined)
    else {
      assert.equal(signal?.aborted, false)
      if (action === 'delete') await o.emit('session.deleted', { info: { id: SID } })
      else await o.dispose()
      assert.equal(signal?.aborted, true)
    }
    held.release()
    await o.tick(120_000)
    assert.equal(o.w.to('/api/bridge/poll').length, 0)
  })
})

test('managed control removal aborts quota reads and reopening explicitly clears the snapshot', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  await o.control()
  await o.tick()
  const held = gate()
  let signal: AbortSignal | null | undefined
  const runtime = quotaRuntime(async (_url, init) => { signal = init.signal; await held.promise; return Response.json(quotaBody()) })
  await o.runtimeParams(SID, runtime.provider, runtime.model)
  await o.settle()
  await rm(o.file)
  await o.tick()
  assert.equal(signal?.aborted, true)
  held.release()
  await o.settle()
  await o.control()
  await o.tick()
  assert.deepEqual(o.w.to('/api/bridge/poll').at(-1).providerLimits, [])
})

test('a first model request before managed activation is retained without calling providers before control is verified', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  let calls = 0
  const runtime = quotaRuntime(async () => { calls++; return Response.json(quotaBody()) })
  await o.runtimeParams(SID, runtime.provider, runtime.model)
  await o.settle()
  assert.equal(calls, 0)
  await o.control()
  await o.tick()
  assert.equal(calls, 1)
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).providerLimits[0].status, 'ok')
})

test('quota observations use global hook-entry order, not descendant lookup completion order', async t => {
  const o = await world(t), held = gate(), child = 'ses_slow_quota_child'
  await o.say(SID, text('root'))
  o.w.infos[child] = { id: child, parentID: SID }
  o.w.infoGates[child] = held.promise
  let olderCalls = 0, newerCalls = 0
  const older = quotaRuntime(async () => { olderCalls++; return Response.json(quotaBody()) })
  const newer = quotaRuntime(async () => {
    newerCalls++
    const body = quotaBody()
    body.rate_limit.primary_window.used_percent = 90
    return Response.json(body)
  })
  await o.runtimeParams(child, older.provider, older.model)
  await o.runtimeParams(SID, newer.provider, newer.model)
  await o.settle()
  held.release()
  await o.settle()
  await o.tick()
  const poll = o.w.to('/api/bridge/poll').at(-1)
  assert.equal(poll.providerLimits[0].windows[0].percentUsed, 90)
  assert.equal(newerCalls, 1)
  assert.equal(olderCalls, 0)
  await o.runtimeParams(SID, { id: 'unknown' }, { providerID: 'unknown', limit: { context: 1000 } })
  await o.settle()
  await o.tick()
  assert.deepEqual(o.w.to('/api/bridge/poll').at(-1).providerLimits[0].windows, [])
  assert.ok(o.w.to('/api/bridge/poll').at(-1).providerLimitsRevision > poll.providerLimitsRevision)
  await o.tick(56_000)
  assert.equal(olderCalls, 1, 'the older child context remains available as a fallback')
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).providerLimits[0].windows[0].percentUsed, 42)
})

async function timedPresenceWorld(t: TestContext) {
  const o = await world(t)
  const delays: Record<string, number> = {}
  const reads: { id: string; signal: AbortSignal; completed: boolean; aborted: boolean }[] = []
  t.mock.method(AbortSignal, 'timeout', (ms: number) => {
    const controller = new AbortController()
    setTimeout(() => controller.abort(new DOMException('Request timed out', 'TimeoutError')), ms)
    return controller.signal
  })
  o.w.infoWait = async (id, signal) => {
    if (!delays[id]) return
    const read = { id, signal, completed: false, aborted: false }
    reads.push(read)
    await new Promise<void>((resolve, reject) => {
      const abort = () => { clearTimeout(timer); read.aborted = true; reject(signal.reason) }
      const timer = setTimeout(() => {
        signal.removeEventListener('abort', abort)
        read.completed = true
        resolve()
      }, delays[id])
      if (signal.aborted) abort()
      else signal.addEventListener('abort', abort, { once: true })
    })
  }
  return { ...o, delays, reads }
}

async function managedWorld(t: TestContext) {
  const dir = await mkdtemp(join(tmpdir(), 'slorp-managed-'))
  t.after(() => rm(dir, { recursive: true, force: true }))
  const file = join(dir, 'control.json')
  const run = 'private-fencing-token'
  const o = await world(t, undefined, { file, run })
  return {
    ...o, file, run,
    control: async (contents = JSON.stringify({ sessionId: SID })) => {
      await writeFile(file + '.next', contents)
      await rename(file + '.next', file)
    },
  }
}

async function cleanupWorld(t: TestContext, kind: 'ack' | 'settlement') {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  await o.control()
  await o.tick()
  const path = kind === 'ack' ? '/api/bridge/ack' : '/api/bridge/permission/done'
  const replies: { id: string; text: string }[] = []
  o.w.answers['/api/bridge/poll'] = () => ({ replies: replies.splice(0) })
  return {
    ...o, path,
    cleanups: () => o.w.to(path),
    queue: async (id: string, dropped = false) => {
      if (kind === 'ack') {
        o.w.promptError = dropped ? new Error('delivery failed') : undefined
        replies.push({ id, text: 'continue' })
        await o.tick()
      } else {
        o.w.answers['/api/bridge/permission'] = () => ({ id, answerable: false })
        await o.emit('permission.asked', { id: `native_${id}`, sessionID: SID, permission: 'bash', patterns: ['ls'] })
        await o.emit('permission.replied', { requestID: `native_${id}` })
      }
    },
  }
}

test('evicted managed cleanup tombstones do not block later queued entries after a lost success response', async t => {
  for (const kind of ['ack', 'settlement'] as const) await t.test(kind, async t => {
    const o = await cleanupWorld(t, kind)
    const pending = new Set(['old', 'later'])
    const completed = new Set<string>()
    const complete = (id: string) => {
      pending.delete(id)
      completed.add(id)
      if (completed.size > 256) completed.delete(completed.values().next().value!)
    }
    let release = (_: Response) => {}
    const evicted = new Promise<Response>(resolve => { release = resolve })
    o.w.answers[o.path] = ({ id }) => {
      if (pending.has(id)) {
        complete(id)
        if (id === 'old') throw new Error('successful cleanup response lost')
        return new Response(null, { status: 204 })
      }
      if (completed.has(id)) return new Response(null, { status: 204 })
      return evicted
    }
    await o.queue('old')
    assert.ok(completed.has('old'))
    assert.ok(!pending.has('old'))
    assert.deepEqual(o.cleanups().map(p => p.id), ['old'])
    for (let i = 0; i < 257; i++) complete(`other_${i}`)
    assert.equal(completed.size, 256)
    assert.ok(!completed.has('old'))
    await o.tick()
    await o.queue('later')
    assert.deepEqual(o.cleanups().map(p => p.id), ['old', 'old'], 'the in-flight retry holds the later cleanup behind it')
    release(new Response(null, { status: 409 }))
    await o.settle()
    assert.deepEqual(o.cleanups().map(p => p.id), ['old', 'old', 'later'])
    assert.ok(completed.has('later'))
    assert.ok(!pending.has('later'))
    await o.tick()
    assert.equal(o.cleanups().length, 3, 'both terminal cleanups have been removed from the queue')
    if (kind === 'ack') assert.equal(o.w.prompted.length, 2, 'retrying cleanup does not redeliver replies')
    assert.ok(o.w.posts.filter(p => p.path === o.path).every(p => p.headers.get('X-Slorp-Run') === o.run))
  })
})

test('managed cleanup still retries network and non-409 HTTP failures before later entries', async t => {
  for (const kind of ['ack', 'settlement'] as const) for (const status of [500, 410, undefined]) {
    await t.test(`${kind}: ${status ?? 'network'}`, async t => {
      const o = await cleanupWorld(t, kind)
      let down = true
      o.w.answers[o.path] = () => {
        if (!down) return new Response(null, { status: 204 })
        if (status) return new Response(null, { status })
        throw new Error('network failure mentioning Slorp: HTTP 409')
      }
      await o.queue('old')
      await o.queue('later')
      assert.ok(o.cleanups().length > 0)
      assert.ok(o.cleanups().every(p => p.id === 'old'))
      down = false
      const before = o.cleanups().length
      await o.tick()
      assert.deepEqual(o.cleanups().slice(before).map(p => p.id), ['old', 'later'])
      await o.tick()
      assert.equal(o.cleanups().length, before + 2)
    })
  }
})

test('a terminal cleanup response does not erase a concurrently changed acknowledgement', async t => {
  for (const status of [204, 409]) await t.test(String(status), async t => {
    const o = await cleanupWorld(t, 'ack')
    o.w.answers[o.path] = () => { throw new Error('response lost') }
    await o.queue('shared')
    let release = (_: Response) => {}
    o.w.answers[o.path] = () => new Promise<Response>(resolve => { release = resolve })
    await o.tick()
    await o.queue('shared', true)
    assert.deepEqual(o.cleanups().map(p => p.state), ['delivered', 'delivered'])
    release(new Response(null, { status }))
    await o.settle()
    o.w.answers[o.path] = () => new Response(null, { status: 204 })
    await o.tick()
    assert.deepEqual(o.cleanups().map(p => p.state), ['delivered', 'delivered', 'dropped'])
    await o.tick()
    assert.equal(o.cleanups().length, 3)
  })
})

test('managed 409 responses on poll and permission routes are not mistaken for successful cleanup', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  o.w.answers['/api/bridge/poll'] = () => new Response(null, { status: 409 })
  o.w.answers['/api/bridge/permission'] = () => new Response(null, { status: 409 })
  await o.control()
  await o.tick()
  await o.emit('permission.asked', { id: 'native', sessionID: SID, permission: 'bash', patterns: ['ls'] })
  await o.tick()
  assert.ok(o.w.to('/api/bridge/poll').length >= 2)
  assert.ok(o.w.to('/api/bridge/permission').length >= 2)
  assert.deepEqual(o.w.to('/api/bridge/ack'), [])
  assert.deepEqual(o.w.to('/api/bridge/permission/done'), [])
  assert.equal(o.w.prompted.length, 0)
  assert.equal(o.w.answered.length, 0)
  let claimed = false
  o.w.answers['/api/bridge/poll'] = () => ({ replies: claimed ? [] : (claimed = true, [{ id: 'reply', text: 'continue' }]) })
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'slorp', answerable: true })
  o.w.answers['/api/bridge/permission/wait'] = () => ({ state: 'allow' })
  await o.tick()
  assert.equal(o.w.prompted.length, 1)
  assert.equal(o.w.answered.length, 1)
})

test('managed activation reports a promptless idle root with its original timestamps', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID, title: 'New session - 2026-11-19', time: { created: 100, updated: 150 } }
  await o.tick()
  assert.equal(o.w.posts.length, 0)
  await o.control('{}')
  await o.tick()
  assert.deepEqual(o.w.lookups, [])
  await o.control()
  await o.tick()
  const poll = o.w.to('/api/bridge/poll')[0]
  assert.equal(poll.sessionId, SID)
  assert.equal(poll.title, '')
  assert.equal(poll.snapshot.status, 'idle')
  assert.equal(poll.snapshot.startedAt, 100)
  assert.equal(poll.snapshot.activeAt, 150)
  assert.equal(poll.snapshot.promptAt, undefined)
  assert.equal(poll.snapshot.lastPrompt, '')
  assert.equal(poll.snapshot.lastReply, '')
  assert.deepEqual(o.w.to('/api/bridge/event'), [])
  assert.deepEqual(o.w.prompted, [])
  await o.tick()
  await o.open(SID)
  assert.deepEqual(o.w.lookups, [SID, SID])
  assert.equal(o.w.to('/api/bridge/poll').length, 2)
  assert.ok(o.w.posts.every(p => p.headers.get('X-Slorp-Run') === o.run), 'environment is captured at plugin creation, not reread on a tick')
})

test('managed activation hydrates saved history and usage without a fake turn', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID, title: 'Saved work', time: { created: 100, updated: 400 } }
  o.w.messages[SID] = [
    message(SID, 'question', 'user', 200, [text('original question')]),
    message(SID, 'answer', 'assistant', 300, [text('original answer')], { tokens: usage(20), cost: 0.25, time: { created: 300, completed: 400 } }),
  ]
  await o.control()
  await o.tick()
  const s = o.w.to('/api/bridge/poll')[0].snapshot
  assert.equal(s.status, 'idle')
  assert.equal(s.startedAt, 100)
  assert.equal(s.activeAt, 400)
  assert.equal(s.promptAt, 200)
  assert.equal(s.lastPrompt, 'original question')
  assert.equal(s.lastReply, 'original answer')
  assert.deepEqual(s.used, { in: 100, out: 30, cacheRead: 40, cacheWrite: 30 })
  assert.equal(s.costUsd, 0.25)
  assert.deepEqual(o.w.to('/api/bridge/event'), [])
  assert.deepEqual(o.w.prompted, [])
})

test('managed launch retains a chat hook before the first control tick, including resumed history', async t => {
  for (const resumed of [false, true]) await t.test(resumed ? 'resumed' : 'new', async t => {
    const o = await managedWorld(t)
    o.w.infos[SID] = { id: SID, time: { created: 100, updated: 300 } }
    o.w.messages[SID] = resumed ? [
      message(SID, 'old_user', 'user', 200, [text('old prompt')]),
      message(SID, 'old_answer', 'assistant', 300, [text('old answer')], { tokens: usage(), cost: 0.25, providerID: 'old', modelID: 'old-model' }),
    ] : []
    await o.control()
    const prompt = message(SID, 'launch_user', 'user', 500, [text('launch prompt')], { agent: 'build', model: { providerID: 'test', modelID: 'new-model', variant: 'high' } })
    await o.sayWith({ agent: 'build', model: prompt.info.model, variant: 'high' }, prompt.info, ...prompt.parts)
    await o.params(SID, 32000)
    assert.equal(o.w.posts.length, 0)
    o.w.messages[SID]!.push(prompt)
    await o.emit('message.updated', { info: prompt.info })
    for (const part of prompt.parts) await o.emit('message.part.updated', { part })
    o.w.statuses[SID] = { type: 'busy' }
    await o.tick()
    const s = o.w.to('/api/bridge/poll')[0].snapshot
    assert.equal(s.lastPrompt, 'launch prompt')
    assert.equal(s.promptAt, 500)
    assert.equal(s.startedAt, 100)
    assert.equal(s.activeAt, 500)
    assert.equal(s.lastReply, '')
    assert.equal(s.model, 'test/new-model')
    assert.equal(s.reasoning, 'high')
    assert.equal(s.contextWindow, 32000)
    assert.equal(s.status, 'working')
    assert.equal(s.costUsd, resumed ? 0.25 : 0)
    assert.deepEqual(o.w.to('/api/bridge/event'), [])
  })
})

test('managed activation refreshes history missed while closed without inventing replay timestamps', async t => {
  const o = await managedWorld(t)
  const doc = JSON.parse(await readFile(new URL('../../internal/opencode/testdata/overflow.json', import.meta.url), 'utf8'))
  o.w.infos[SID] = { ...doc.info, id: SID }
  await o.params(SID, 64000)
  await o.control()
  await o.tick()
  await o.control('{')
  await o.tick()
  o.w.messages[SID] = doc.messages.filter((m: Message) => !['msg_repeat', 'msg_repeatanswer'].includes(m.info.id)).map((m: Message) => ({ ...m, info: { ...m.info, sessionID: SID } }))
  await o.control()
  await o.tick()
  const s = o.w.to('/api/bridge/poll').at(-1).snapshot
  assert.equal(s.lastPrompt, 'Apply the fix')
  assert.equal(s.promptAt, 1700000003000)
  assert.equal(s.lastReply, 'Fixed after compaction.')
  assert.deepEqual(o.w.to('/api/bridge/event'), [])
})

test('managed discovery cannot overwrite newer hook selections before delivering its first queued reply', async t => {
  const o = await managedWorld(t)
  const old = { id: SID, agent: 'old-agent', model: { providerID: 'old-provider', id: 'old-model', variant: 'low' }, time: { created: 100, updated: 500 } }
  o.w.infos[SID] = old
  await o.emit('session.created', { info: old })
  let release = () => {}
  o.w.infoGates[SID] = new Promise(resolve => { release = resolve })
  await o.control()
  await o.tick()
  assert.deepEqual(o.w.lookups, [SID])
  const prompt = message(SID, 'new_prompt', 'user', 500, [text('new prompt')], { agent: 'new-agent', model: { providerID: 'new-provider', modelID: 'new-model', variant: 'high' } })
  await o.sayWith({ agent: 'new-agent', model: prompt.info.model, variant: 'high' }, prompt.info, ...prompt.parts)
  await o.params(SID, 64000)
  o.w.messages[SID] = [prompt]
  o.w.answers['/api/bridge/poll'] = () => ({ replies: [{ id: 'first_reply', text: 'continue' }] })
  assert.equal(o.w.posts.length, 0)
  release()
  await o.settle()
  assert.deepEqual(o.w.prompted, [{ path: { id: SID }, body: {
    agent: 'new-agent', model: { providerID: 'new-provider', modelID: 'new-model' }, variant: 'high', parts: [text('continue')],
  } }])
  const s = o.w.to('/api/bridge/poll')[0].snapshot
  assert.equal(s.lastPrompt, 'new prompt')
  assert.equal(s.promptAt, 500)
  assert.equal(s.model, 'new-provider/new-model')
  assert.equal(s.reasoning, 'high')
  assert.equal(s.contextWindow, 64000)
})

test('managed selection accepts later session updates and rejects older snapshots without freezing selection', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  const prompt = message(SID, 'prompt', 'user', 500, [text('prompt')])
  await o.sayWith({ agent: 'hook-agent', model: { providerID: 'hook', modelID: 'hook-model' }, variant: 'high' }, prompt.info, ...prompt.parts)
  o.w.messages[SID] = [prompt]
  await o.control()
  await o.tick()
  const stale = { id: SID, agent: 'stale-agent', model: { providerID: 'stale', id: 'stale-model', variant: 'low' }, time: { created: 100, updated: 400 } }
  await o.emit('session.updated', { info: stale })
  o.w.answers['/api/bridge/poll'] = () => ({ replies: [{ id: 'reply', text: 'continue' }] })
  await o.tick()
  assert.deepEqual(o.w.prompted[0].body, {
    agent: 'hook-agent', model: { providerID: 'hook', modelID: 'hook-model' }, variant: 'high', parts: [text('continue')],
  })
  await o.emit('session.updated', { info: { id: SID, agent: 'later-agent', model: { providerID: 'later', id: 'later-model' }, time: { created: 100, updated: 800 } } })
  await o.emit('session.updated', { info: { ...stale, time: { created: 100, updated: 600 } } })
  await o.tick()
  assert.deepEqual(o.w.prompted[1].body, {
    agent: 'later-agent', model: { providerID: 'later', modelID: 'later-model' }, parts: [text('continue')],
  })
  const s = o.w.to('/api/bridge/poll').at(-1).snapshot
  assert.equal(s.lastPrompt, 'prompt')
  assert.equal(s.promptAt, 500)
  assert.equal(s.model, 'later/later-model')
  assert.equal(s.reasoning, '')
  await Promise.all(['next', 'latest'].map(name => o.dispatch('session.updated', { info: {
    id: SID, agent: `${name}-agent`, model: { providerID: name, id: `${name}-model`, variant: 'max' }, time: { created: 100, updated: 900 },
  } })))
  await o.control('{}')
  await o.tick()
  await o.control()
  await o.tick()
  assert.deepEqual(o.w.prompted[2].body, {
    agent: 'latest-agent', model: { providerID: 'latest', modelID: 'latest-model' }, variant: 'max', parts: [text('continue')],
  }, 'later queued selections survive another activation/history refresh')
})

test('managed activation retains a hook that native history has not saved yet', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID, time: { created: 100, updated: 100 } }
  const prompt = message(SID, 'pending_user', 'user', 200, [text('not saved yet')])
  await o.sayWith({}, prompt.info, ...prompt.parts)
  await o.control()
  await o.tick()
  const s = o.w.to('/api/bridge/poll')[0].snapshot
  assert.equal(s.lastPrompt, 'not saved yet')
  assert.equal(s.promptAt, 200)
  assert.equal(s.activeAt, 200)
  assert.deepEqual(o.w.to('/api/bridge/event'), [])
})

test('managed history refresh replaces stale cached assistant text and usage', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  o.w.messages[SID] = [
    message(SID, 'user', 'user', 100, [text('question')]),
    message(SID, 'answer', 'assistant', 200, [text('partial')], { tokens: usage(10), cost: 0.1 }),
  ]
  await o.control()
  await o.tick()
  await rm(o.file)
  await o.tick()
  o.w.messages[SID]![1] = message(SID, 'answer', 'assistant', 200, [text('complete')], { tokens: usage(50), cost: 0.5, time: { created: 200, completed: 300 } })
  await o.control()
  await o.tick()
  const s = o.w.to('/api/bridge/poll').at(-1).snapshot
  assert.equal(s.lastReply, 'complete')
  assert.equal(s.activeAt, 300)
  assert.equal(s.used.out, 60)
  assert.equal(s.costUsd, 0.5)
})

test('managed activation normalizes a locally ingested overflow replay to its original prompt', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID, time: { created: 100, updated: 100 } }
  const original = message(SID, 'original', 'user', 200, [text('original prompt')])
  const compact = message(SID, 'compact', 'user', 300, [{ type: 'compaction', auto: true, overflow: true }])
  const summary = message(SID, 'summary', 'assistant', 400, [text('summary')], { parentID: 'compact', summary: true, time: { created: 400, completed: 450 } })
  const replay = message(SID, 'replay', 'user', 500, [text('original prompt')])
  o.w.messages[SID] = [original, compact, summary]
  await o.sayWith({}, replay.info, ...replay.parts)
  o.w.messages[SID]!.push(replay, message(SID, 'answer', 'assistant', 600, [text('final answer')]))
  await o.control()
  await o.tick()
  const s = o.w.to('/api/bridge/poll')[0].snapshot
  assert.equal(s.lastPrompt, 'original prompt')
  assert.equal(s.promptAt, 200)
  assert.equal(s.activeAt, 600)
  assert.equal(s.lastReply, 'final answer')
  assert.deepEqual(o.w.to('/api/bridge/event'), [])
})

test('managed native prompts survive unavailable control before presentation and after a suspended wait', async t => {
  for (const kind of ['permission', 'question']) for (const active of [false, true]) {
    await t.test(`${kind}: ${active ? 'suspended wait' : 'before activation'}`, async t => {
      const o = await managedWorld(t)
      o.w.infos[SID] = { id: SID }
      let registrations = 0
      const waits = new Map<string, (value: unknown) => void>()
      o.w.answers['/api/bridge/permission'] = () => ({ id: `ask_${++registrations}`, answerable: true })
      o.w.answers['/api/bridge/permission/wait'] = ({ id }) => new Promise(resolve => { waits.set(id, resolve) })
      if (active) { await o.control(); await o.tick() }
      await o.emit(`${kind}.asked`, kind === 'permission'
        ? { id: 'native_ask', sessionID: SID, permission: 'bash', patterns: ['ls'] }
        : { id: 'native_ask', sessionID: SID, questions: [{ question: 'Continue?', options: [{ label: 'Yes' }] }] })
      assert.equal(registrations, active ? 1 : 0)
      if (active) await rm(o.file)
      const before = o.w.posts.length
      await o.tick()
      await o.control('{')
      await o.tick()
      assert.equal(o.w.posts.length, before)
      assert.equal(o.w.answered.length, 0)
      assert.deepEqual(o.w.to('/api/bridge/permission/done'), [])
      await o.control()
      await o.tick()
      const expected = active ? 2 : 1
      assert.equal(registrations, expected, 'the same native prompt is offered again, once')
      if (active) waits.get('ask_1')!({ state: 'allow', selections: [['stale']] })
      await o.settle()
      assert.equal(o.w.answered.length, 0)
      await o.tick()
      assert.equal(registrations, expected)
      waits.get(`ask_${expected}`)!({ state: 'allow', selections: [['Yes']] })
      await o.settle()
      assert.deepEqual(o.w.answered.map(a => [a.path.requestID, a.body]), [['native_ask', kind === 'permission' ? { reply: 'once' } : { answers: [['Yes']] }]])
      await o.tick()
      assert.equal(registrations, expected)
      assert.ok(o.w.to('/api/bridge/permission/done').every(p => active && p.id === 'ask_1'), 'only an obsolete Slorp presentation may be retired')
    })
  }
})

test('old managed presentation continuations cannot clear or settle a replacement attempt', async t => {
  for (const kind of ['permission', 'question']) for (const status of [undefined, 500, 404]) await t.test(`${kind}: old answer ${status ?? 'succeeds'}`, async t => {
    const o = await managedWorld(t)
    o.w.infos[SID] = { id: SID }
    await o.control()
    await o.tick()
    let release = () => {}, decide = (_: unknown) => {}, registrations = 0
    o.w.answerGate = new Promise(resolve => { release = resolve })
    o.w.answerError = status ? new Error('old transport failed') : undefined
    o.w.answerStatus = status
    o.w.answers['/api/bridge/permission'] = () => ({ id: `ask_${++registrations}`, answerable: true })
    o.w.answers['/api/bridge/permission/wait'] = ({ id }) => id === 'ask_1' ? { state: 'allow', selections: [['Yes']] } : new Promise(resolve => { decide = resolve })
    await o.emit(`${kind}.asked`, kind === 'permission'
      ? { id: 'native_ask', sessionID: SID, permission: 'bash', patterns: ['ls'] }
      : { id: 'native_ask', sessionID: SID, questions: [{ question: 'Continue?', options: [{ label: 'Yes' }] }] })
    assert.equal(o.w.answered.length, 1)
    await rm(o.file)
    await o.tick()
    o.w.answerGate = undefined
    o.w.answerError = undefined
    o.w.answerStatus = undefined
    await o.control()
    await o.tick()
    assert.equal(registrations, 2)
    release()
    await o.settle()
    await o.tick()
    assert.equal(registrations, 2)
    assert.deepEqual(o.w.answeredOld, [])
    assert.ok(o.w.to('/api/bridge/permission/done').every(p => p.id !== 'ask_2'))
    decide({ state: 'allow', selections: [['Yes']] })
    await o.settle()
    assert.equal(o.w.answered.length, 2)
    await o.tick()
    assert.equal(registrations, 2)
  })
})

test('native settlement while managed presentation is suspended prevents restoration', async t => {
  for (const kind of ['permission', 'question']) await t.test(kind, async t => {
    const o = await managedWorld(t)
    o.w.infos[SID] = { id: SID }
    await o.control()
    await o.tick()
    o.w.answers['/api/bridge/permission'] = () => ({ id: 'old_presentation', answerable: true })
    o.w.answers['/api/bridge/permission/wait'] = () => new Promise(() => {})
    await o.emit(`${kind}.asked`, kind === 'permission'
      ? { id: 'native_ask', sessionID: SID, permission: 'bash', patterns: ['ls'] }
      : { id: 'native_ask', sessionID: SID, questions: [{ question: 'Continue?' }] })
    await rm(o.file)
    await o.tick()
    await o.emit(`${kind}.replied`, { requestID: 'native_ask' })
    await o.control()
    await o.tick()
    assert.equal(o.w.to('/api/bridge/permission').length, 1)
    assert.deepEqual(o.w.answered, [])
    assert.deepEqual(o.w.to('/api/bridge/permission/done'), [{ id: 'old_presentation' }])
  })
})

test('managed control retries absent, malformed, oversized and unavailable session data on later ticks', async t => {
  const o = await managedWorld(t)
  await o.tick()
  for (const contents of ['{', '{}', 'null', '[]', '{"sessionId":42}', '{"sessionId":"other"}', '{"sessionId":"ses_../bad"}', JSON.stringify({ sessionId: SID, padding: 'é'.repeat(600) })]) {
    await o.control(contents)
    await o.tick()
    assert.deepEqual(o.w.posts, [])
    assert.deepEqual(o.w.lookups, [])
  }
  await o.control()
  await o.tick()
  assert.deepEqual(o.w.lookups, [SID])
  await o.settle()
  assert.deepEqual(o.w.lookups, [SID], 'a failed discovery must not spin')
  o.w.infos[SID] = { id: SID, time: { created: 1, updated: 2 } }
  await o.tick()
  assert.deepEqual(o.w.lookups, [SID, SID])
  assert.equal(o.w.to('/api/bridge/poll').length, 1)
})

test('a managed control ID must resolve to that native root, never a child or a different root', async t => {
  const o = await managedWorld(t)
  await o.control()
  o.w.infos[SID] = { id: 'ses_other' }
  await o.tick()
  o.w.infos[SID] = { id: SID, parentID: 'ses_other' }
  await o.tick()
  assert.deepEqual(o.w.posts, [])
  assert.deepEqual(o.w.lookups, [SID, SID])
  o.w.infos[SID] = { id: SID }
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll')[0].sessionId, SID)
})

test('managed control rejects non-files and permits exactly one KiB, then recovers only its pinned root', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  await mkdir(o.file)
  await o.tick()
  assert.equal(o.w.posts.length, 0)
  await rm(o.file, { recursive: true })
  const control = JSON.stringify({ sessionId: SID }).padEnd(1024, ' ')
  await o.control(control)
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').length, 1)
  await o.control(control + ' ')
  await o.tick()
  await o.say(SID, text('must not advertise while control is invalid'))
  assert.equal(o.w.posts.length, 1)
  await o.control()
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').length, 2)
  assert.deepEqual(o.w.lookups, [SID, SID])
  await o.control(JSON.stringify({ sessionId: 'ses_other' }))
  await o.tick()
  assert.equal(o.w.posts.length, 2)
  assert.deepEqual(o.w.lookups, [SID, SID])
})

test('managed incidental chats, events, permissions and terminal presence cannot advertise another root', async t => {
  const o = await managedWorld(t)
  const other = 'ses_other', child = 'ses_other_child'
  o.w.infos[SID] = { id: SID }
  o.w.infos[other] = { id: other }
  o.w.infos[child] = { id: child, parentID: other }
  await o.open(other)
  assert.deepEqual(o.w.lookups, [])
  const incidental = async () => {
    await o.say(other, text('not managed'))
    await o.say(child, text('not a managed child'))
    await o.emit('session.status', { sessionID: other, status: { type: 'busy' } })
    await o.emit('session.status', { sessionID: child, status: { type: 'busy' } })
    await o.emit('message.updated', { info: { sessionID: child, id: 'msg_other', role: 'assistant' } })
    await o.emit('message.part.updated', { part: taskPart(other, child) })
    await o.emit('session.error', { sessionID: child, error: { name: 'Error' } })
    await o.emit('permission.asked', { id: 'per_other', sessionID: child, permission: 'bash', patterns: ['ls'] })
    await o.emit('question.asked', { id: 'q_other', sessionID: other, questions: [] })
    await o.open(other)
  }
  await incidental()
  assert.equal(o.w.posts.length, 0)
  await o.control()
  await o.tick()
  await incidental()
  await o.tick()
  assert.ok(o.w.posts.length > 0)
  assert.ok(o.w.posts.every(p => p.body.sessionId === SID))
  assert.deepEqual(o.w.to('/api/bridge/permission'), [])
  assert.deepEqual(o.w.to('/api/bridge/event'), [])
  await o.control(JSON.stringify({ sessionId: other }))
  const count = o.w.posts.length
  await o.tick()
  await incidental()
  await o.tick()
  assert.equal(o.w.posts.length, count, 'switching control cannot rebind the runtime or keep advertising its old root')
})

test('every managed bridge route carries the private fence and descendants stay under the chosen root', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  o.w.children[SID] = [{ id: CHILD, parentID: SID }]
  o.w.messages[CHILD] = [message(CHILD, 'child_answer', 'assistant', 100, [text('child report')])]
  await o.control()
  await o.tick()
  await o.say(SID, text('real turn'))
  o.w.answers['/api/bridge/poll'] = () => ({ replies: [{ id: 'reply_1', text: 'continue' }] })
  await o.tick()
  assert.equal(o.w.prompted[0].path.id, SID)
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'ask_1', answerable: true })
  o.w.answers['/api/bridge/permission/wait'] = () => new Promise(() => {})
  await o.emit('permission.asked', { id: 'per_child', sessionID: CHILD, permission: 'bash', patterns: ['ls'] })
  const permission = o.w.to('/api/bridge/permission')[0]
  assert.equal(permission.sessionId, SID)
  assert.equal(permission.agentId, CHILD)
  await o.emit('permission.replied', { requestID: 'per_child' })
  const paths = ['/api/bridge/poll', '/api/bridge/event', '/api/bridge/subagent', '/api/bridge/ack', '/api/bridge/permission', '/api/bridge/permission/wait', '/api/bridge/permission/done']
  assert.deepEqual([...new Set(o.w.posts.map(p => p.path))].sort(), paths.sort())
  for (const p of o.w.posts) {
    assert.equal(p.headers.get('X-Slorp-Run'), o.run, p.path)
    assert.ok(!JSON.stringify(p.body).includes(o.run))
    assert.ok(!JSON.stringify(p.body).includes(o.file))
    if (p.body.sessionId) assert.equal(p.body.sessionId, SID)
  }
  assert.equal(o.w.to('/api/bridge/subagent')[0].subagent.id, CHILD)
})

test('deleting managed control aborts waits and polls, suppresses late answers and closes subsequent activity', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  await o.control()
  await o.tick()
  let reply = (_: unknown) => {}, allow = (_: unknown) => {}
  o.w.answers['/api/bridge/poll'] = () => new Promise(resolve => { reply = resolve })
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'ask_1', answerable: true })
  o.w.answers['/api/bridge/permission/wait'] = () => new Promise(resolve => { allow = resolve })
  await o.emit('permission.asked', { id: 'per_stop', sessionID: SID, permission: 'bash', patterns: ['ls'] })
  await o.tick()
  const pending = o.w.posts.filter(p => p.path.endsWith('/wait') || p.path.endsWith('/poll')).slice(1)
  assert.equal(pending.length, 2)
  await rm(o.file)
  const count = o.w.posts.length
  await o.tick()
  assert.ok(pending.every(p => p.signal?.aborted))
  reply({ replies: [{ id: 'late', text: 'must not deliver' }] })
  allow({ state: 'allow' })
  await o.settle()
  await o.say(SID, text('after stop'))
  await o.open(SID)
  await o.emit('session.status', { sessionID: SID, status: { type: 'busy' } })
  await o.emit('permission.asked', { id: 'per_late', sessionID: SID, permission: 'bash', patterns: ['ls'] })
  await o.tick()
  assert.equal(o.w.posts.length, count)
  assert.deepEqual(o.w.prompted, [])
  assert.deepEqual(o.w.answered, [])
  assert.deepEqual(o.w.answeredOld, [])
})

test('managed discovery is not duplicated across ticks and disposal prevents delayed activation', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  let release = () => {}
  o.w.statusGate = new Promise(resolve => { release = resolve })
  await o.control()
  await o.tick()
  await o.tick()
  await o.open(SID)
  assert.deepEqual(o.w.lookups, [SID])
  await o.dispose()
  release()
  await o.settle()
  await o.tick()
  assert.deepEqual(o.w.posts, [])
})

test('removing control during discovery prevents delayed activation and retries only on a later valid tick', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  let release = () => {}
  o.w.statusGate = new Promise(resolve => { release = resolve })
  await o.control()
  await o.tick()
  await rm(o.file)
  await o.tick()
  release()
  await o.settle()
  assert.equal(o.w.posts.length, 0)
  await o.tick()
  assert.deepEqual(o.w.lookups, [SID])
  await o.control()
  await o.tick()
  assert.deepEqual(o.w.lookups, [SID, SID])
  assert.equal(o.w.to('/api/bridge/poll').length, 1)
})

test('managed descendant permission decisions go to the native request and disposal cancels active waits', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  o.w.children[SID] = [{ id: CHILD, parentID: SID }]
  await o.control()
  await o.tick()
  o.w.answers['/api/bridge/permission'] = () => ({ id: 'ask_child', answerable: true })
  o.w.answers['/api/bridge/permission/wait'] = () => ({ state: 'allow' })
  await o.emit('permission.asked', { id: 'per_child', sessionID: CHILD, permission: 'bash', patterns: ['ls'] })
  assert.deepEqual(o.w.answered.map(a => [a.path.requestID, a.body]), [['per_child', { reply: 'once' }]])
  assert.equal(o.w.to('/api/bridge/permission')[0].sessionId, SID)
  assert.equal(o.w.to('/api/bridge/permission')[0].agentId, CHILD)
  o.w.answers['/api/bridge/permission/wait'] = () => new Promise(() => {})
  await o.emit('permission.asked', { id: 'per_wait', sessionID: CHILD, permission: 'bash', patterns: ['pwd'] })
  const waiting = o.w.posts.at(-1)!
  assert.equal(waiting.path, '/api/bridge/permission/wait')
  const count = o.w.posts.length
  await o.dispose()
  await o.tick()
  assert.equal(waiting.signal?.aborted, true)
  assert.equal(o.w.posts.length, count)
  assert.equal(o.w.answered.length, 1)
})

test('disposal cancels managed activation while the control file is being read', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  await o.control()
  const reading = o.tick()
  await o.dispose()
  await reading
  assert.deepEqual(o.w.lookups, [])
  assert.deepEqual(o.w.posts, [])
})

test('incomplete managed environment pairs fail closed rather than becoming ordinary installations', async t => {
  for (const env of [{ file: '/not/read/without/token' }, { run: 'token-only' }, { file: '', run: 'token' }, { file: '/unused', run: '' }]) {
    await t.test(JSON.stringify(Object.keys(env)), async t => {
      const o = await world(t, undefined, env)
      o.w.infos[SID] = { id: SID }
      await o.open(SID)
      await o.say(SID, text('not ordinary'))
      await o.emit('session.status', { sessionID: SID, status: { type: 'busy' } })
      await o.emit('permission.asked', { id: 'per_1', sessionID: SID, permission: 'bash', patterns: ['ls'] })
      await o.tick()
      assert.deepEqual(o.w.posts, [])
    })
  }
})

test('ordinary installations still advertise multiple roots without a run header', async t => {
  const o = await world(t)
  await o.say(SID, text('first'))
  await o.say('ses_other', text('second'))
  await o.tick()
  assert.deepEqual(o.w.polls().map(p => p.sessionId), [SID, 'ses_other'])
  assert.ok(o.w.posts.every(p => !p.headers.has('X-Slorp-Run')))
})

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
  assert.equal(o.w.lookups.length, 2, 'repeated presence validates ownership without relearning live state')
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

test('presence validates selected and ancestor scope before learning or activating anything', async t => {
  const cases = [
    { id: 'ses_wrong' }, { directory: '/foreign' }, { directory: undefined },
    { projectID: 'foreign' }, { projectID: undefined }, { workspaceID: 'wrk_foreign' },
  ]
  for (const parent of [false, true]) for (const bad of cases) await t.test(`${parent ? 'parent' : 'selected'} ${JSON.stringify(bad)}`, async t => {
    const o = await world(t)
    o.w.infos[SID] = { id: SID, ...(parent ? { parentID: 'ses_parent' } : bad) }
    o.w.infos.ses_parent = { id: 'ses_parent', ...bad }
    await o.open(SID)
    await o.tick()
    assert.deepEqual(o.w.posts, [])
    assert.deepEqual(o.w.childLookups, [])
  })
})

test('presence rejects missing ancestors, cycles, excessive depth and failed lookups without phantom roots', async t => {
  for (const kind of ['missing', 'cycle', 'depth', 'error']) await t.test(kind, async t => {
    const o = await world(t)
    o.w.infos[SID] = { id: SID, parentID: 'ses_parent' }
    if (kind === 'cycle') o.w.infos.ses_parent = { id: 'ses_parent', parentID: SID }
    if (kind === 'depth') {
      o.w.infos[SID] = { id: SID, parentID: 'ses_0' }
      for (let i = 0; i < 70; i++) o.w.infos[`ses_${i}`] = { id: `ses_${i}`, parentID: `ses_${i + 1}` }
    }
    if (kind === 'error') {
      o.w.infos[SID] = { id: SID }
      o.w.infoErrors[SID] = { message: 'unavailable' }
    }
    await o.open(SID)
    await o.tick()
    assert.deepEqual(o.w.posts, [])
    assert.ok(o.w.lookups.length <= 65)
    delete o.w.infoErrors[SID]
    o.w.infos[SID] = { id: SID }
    await o.open(SID)
    assert.deepEqual(o.w.polls().map(p => p.sessionId), [SID], 'failed resolution did not learn a phantom parent chain')
  })
})

test('presence carries explicit workspace routing and resolves descendants only to their owned root', async t => {
  const o = await world(t)
  o.w.infos.ses_child = { id: 'ses_child', parentID: SID, workspaceID: 'wrk_same' }
  o.w.infos[SID] = { id: SID, workspaceID: 'wrk_same' }
  await o.announce({ sessionId: 'ses_child', workspaceID: 'wrk_same' })
  assert.deepEqual(o.w.polls().map(p => p.sessionId), [SID])
  assert.ok(o.w.lookupRequests.every(r => r.query.directory === '/work/proj' && r.query.workspace === 'wrk_same'))
  assert.equal(o.w.to('/api/bridge/poll')[0].pluginTui, true)
})

test('duplicate and out-of-order sequences cannot change selection or refresh companion freshness', async t => {
  const o = await world(t)
  const terminalId = randomUUID()
  o.w.infos[SID] = { id: SID }
  o.w.infos.ses_other = { id: 'ses_other' }
  await o.announce({ terminalId, sequence: 3, sessionId: SID })
  await o.announce({ terminalId, sequence: 2, sessionId: 'ses_other' })
  await o.announce({ terminalId, sequence: 3, sessionId: 'ses_other' })
  assert.deepEqual(o.w.lookups, [SID])
  await o.tick(10000)
  await o.announce({ terminalId, sequence: 1, sessionId: 'ses_other' })
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).pluginTui, false)
  assert.ok(o.w.polls().every(p => p.sessionId === SID))
})

test('identical two-second heartbeats allow an abort-aware 2.1-second session lookup to activate', async t => {
  const o = await timedPresenceWorld(t)
  o.w.infos[SID] = { id: SID, time: { created: 100, updated: 150 } }
  o.delays[SID] = 2100
  await o.open(SID)
  await o.tick(2000)
  await o.open(SID)
  await o.tick(100)
  assert.equal(o.reads[0]!.aborted, false)
  assert.equal(o.reads[0]!.completed, true)
  assert.deepEqual(o.w.lookups, [SID])
  const poll = o.w.to('/api/bridge/poll').at(-1)
  assert.equal(poll.sessionId, SID)
  assert.equal(poll.pluginTui, true)
  assert.equal(poll.snapshot.activeAt, 150)
  assert.deepEqual(o.w.to('/api/bridge/event'), [])
})

test('identical heartbeats allow an ancestor walk longer than the heartbeat interval to complete', async t => {
  const o = await timedPresenceWorld(t)
  o.w.infos.ses_child = { id: 'ses_child', parentID: SID }
  o.w.infos[SID] = { id: SID }
  o.delays.ses_child = o.delays[SID] = 1400
  await o.open('ses_child')
  await o.tick(1400)
  assert.deepEqual(o.w.lookups, ['ses_child', SID])
  await o.tick(600)
  await o.open('ses_child')
  await o.tick(800)
  assert.ok(o.reads.every(r => r.completed && !r.aborted))
  assert.deepEqual(o.w.polls().map(p => p.sessionId), [SID])
  assert.equal(o.w.to('/api/bridge/poll').at(-1).pluginTui, true)
})

test('slow revalidation completes amid heartbeats and renews companion freshness beyond its original lease', async t => {
  const o = await timedPresenceWorld(t)
  o.w.infos[SID] = { id: SID }
  await o.open(SID)
  o.delays[SID] = 2100
  await o.tick(2000)
  for (let attempt = 0; attempt < 3; attempt++) {
    await o.open(SID)
    await o.tick(2000)
    await o.open(SID)
    await o.tick(100)
    assert.equal(o.reads[attempt]!.completed, true)
    assert.equal(o.reads[attempt]!.aborted, false)
    assert.equal(o.w.to('/api/bridge/poll').at(-1).pluginTui, true)
    if (attempt < 2) await o.tick(1900)
  }
  assert.equal(Date.now(), 12100)
  assert.deepEqual(o.w.lookups, [SID, SID, SID, SID])
})

test('two terminals renew independently while both have slow abort-aware validation in flight', async t => {
  const o = await timedPresenceWorld(t)
  const first = randomUUID(), second = randomUUID()
  o.w.infos[SID] = { id: SID }
  o.w.infos.ses_other = { id: 'ses_other' }
  o.delays[SID] = 2100
  o.delays.ses_other = 3100
  const heartbeat = async () => {
    await o.announce({ terminalId: first, sessionId: SID })
    await o.announce({ terminalId: second, sessionId: 'ses_other' })
  }
  await heartbeat()
  await o.tick(2000)
  await heartbeat()
  await o.tick(100)
  assert.ok(o.w.polls().some(p => p.sessionId === SID))
  await o.tick(1000)
  assert.ok(o.w.polls().some(p => p.sessionId === 'ses_other'))
  await o.tick(900)
  await heartbeat()
  await o.tick(2000)
  await heartbeat()
  await o.tick(100)
  await o.tick(1000)
  assert.equal(o.reads.length, 4)
  assert.ok(o.reads.every(r => r.completed && !r.aborted))
  for (const id of [SID, 'ses_other']) assert.equal(o.w.to('/api/bridge/poll').findLast(p => p.sessionId === id).pluginTui, true)
})

test('heartbeat renewal never extends the five-second request timeout and a later heartbeat retries', async t => {
  const o = await timedPresenceWorld(t)
  o.w.infos[SID] = { id: SID }
  o.delays[SID] = 60000
  await o.open(SID)
  await o.tick(2000)
  await o.open(SID)
  await o.tick(2000)
  await o.open(SID)
  await o.tick(999)
  assert.equal(o.reads[0]!.aborted, false)
  await o.tick(1)
  assert.equal(o.reads[0]!.aborted, true)
  assert.equal(o.reads[0]!.signal.reason.name, 'TimeoutError')
  assert.deepEqual(o.w.posts, [])
  o.delays[SID] = 2100
  await o.tick(1000)
  await o.open(SID)
  await o.tick(2000)
  await o.open(SID)
  await o.tick(100)
  assert.equal(o.reads.length, 2)
  assert.equal(o.reads[1]!.completed, true)
  assert.equal(o.reads[1]!.aborted, false)
  assert.deepEqual(o.w.polls().map(p => p.sessionId), [SID])
})

test('superseded, expired and disposed presence cannot activate after pending reads', async t => {
  for (const stage of ['lookup', 'history', 'status']) for (const supersede of ['home', 'switch', 'foreign', 'expiry', 'dispose']) {
    await t.test(`${stage}: ${supersede}`, async t => {
      const o = await world(t)
      o.w.infos[SID] = { id: SID }
      o.w.infos.ses_other = { id: 'ses_other' }
      let release = () => {}
      const gate = new Promise<void>(resolve => { release = resolve })
      if (stage === 'lookup') o.w.infoGates[SID] = gate
      if (stage === 'history') o.w.messageGate = gate
      if (stage === 'status') o.w.statusGate = gate
      await o.open(SID)
      if (supersede === 'home') await o.open()
      if (supersede === 'switch') await o.open('ses_other')
      if (supersede === 'foreign') await o.open(SID, '/foreign')
      if (supersede === 'expiry') await o.tick(10000)
      if (supersede === 'dispose') await o.dispose()
      release()
      await o.settle()
      await o.tick()
      assert.deepEqual(o.w.posts, [])
      if (supersede === 'switch') {
        await o.open('ses_other')
        assert.deepEqual(o.w.polls().map(p => p.sessionId), ['ses_other'])
      }
    })
  }
})

test('two terminals discover independently while one switches home during another pending lookup', async t => {
  const o = await world(t)
  const first = randomUUID(), second = randomUUID()
  o.w.infos[SID] = { id: SID }
  o.w.infos.ses_other = { id: 'ses_other' }
  let release = () => {}
  o.w.infoGates[SID] = new Promise(resolve => { release = resolve })
  await o.announce({ terminalId: first, sessionId: SID })
  await o.announce({ terminalId: second, sessionId: 'ses_other' })
  await o.announce({ terminalId: second })
  release()
  await o.settle()
  assert.deepEqual(new Set(o.w.polls().map(p => p.sessionId)), new Set([SID, 'ses_other']))
  await o.tick(8000)
  await o.announce({ terminalId: first, sessionId: SID })
  await o.tick(4000)
  const polls = o.w.to('/api/bridge/poll')
  assert.equal(polls.findLast(p => p.sessionId === SID).pluginTui, true)
  assert.equal(polls.findLast(p => p.sessionId === 'ses_other').pluginTui, false)
  assert.ok(polls.some(p => p.sessionId === 'ses_other'), 'expiry never retires observed roots')
})

test('repeated presence and lease expiry preserve native idle timestamps and live error, retry and busy state', async t => {
  for (const state of ['idle', 'error', 'retry', 'busy']) await t.test(state, async t => {
    const o = await world(t)
    o.w.infos[SID] = { id: SID, time: { created: 100, updated: 150 }, agent: 'build' }
    await o.open(SID)
    if (state === 'error') await o.emit('session.error', { sessionID: SID, error: { name: 'APIError', data: { message: 'failed' } } })
    if (state === 'retry' || state === 'busy') await o.emit('session.status', { sessionID: SID, status: { type: state, message: 'rate limited' } })
    await o.tick()
    const before = o.w.to('/api/bridge/poll').at(-1).snapshot
    o.w.infos[SID] = { id: SID, time: { created: 9000, updated: 9999 }, agent: 'stale' }
    o.w.statuses[SID] = { type: state === 'idle' ? 'busy' : 'idle' }
    await o.open(SID)
    await o.tick(12000)
    await o.tick()
    const after = o.w.to('/api/bridge/poll').at(-1)
    assert.deepEqual(after.snapshot, before)
    assert.equal(after.pluginTui, false)
    assert.deepEqual(o.w.prompted, [])
    assert.ok(!o.w.to('/api/bridge/event').some(e => e.type === 'turn.start'))
  })
})

test('managed presence cannot activate before control or refresh companion status for a foreign root or home', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  o.w.infos.ses_other = { id: 'ses_other' }
  o.w.infos.ses_child = { id: 'ses_child', parentID: SID }
  await o.open(SID)
  assert.deepEqual(o.w.lookups, [])
  await o.control()
  await o.tick()
  for (const sessionId of ['ses_other', undefined]) {
    await o.open(sessionId)
    await o.tick()
    assert.equal(o.w.to('/api/bridge/poll').at(-1).pluginTui, false)
  }
  await o.open('ses_child')
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).pluginTui, true)
  assert.ok(o.w.polls().every(p => p.sessionId === SID))
  await o.open('ses_other')
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).pluginTui, false)
  assert.ok(o.w.posts.every(p => p.body.sessionId === SID))
})

test('ordinary presence cannot overwrite a newer hook selection or hydrate over live messages', async t => {
  const o = await world(t)
  o.w.infos[SID] = { id: SID, agent: 'old', model: { id: 'old', providerID: 'provider' }, time: { created: 100, updated: 150 } }
  await o.emit('session.updated', { info: o.w.infos[SID] })
  let release = () => {}
  o.w.infoGates[SID] = new Promise(resolve => { release = resolve })
  await o.open(SID)
  await o.sayWith({ agent: 'new', model: { providerID: 'provider', modelID: 'new' }, variant: 'high' },
    { id: 'msg_live', role: 'user', time: { created: 200 } }, text('new prompt'))
  release()
  await o.settle()
  await o.tick()
  const snapshot = o.w.to('/api/bridge/poll').at(-1).snapshot
  assert.equal(snapshot.model, 'provider/new')
  assert.equal(snapshot.reasoning, 'high')
  assert.equal(snapshot.promptAt, 200)
  assert.equal(snapshot.activeAt, 200)
  assert.equal(snapshot.lastPrompt, 'new prompt')
  assert.equal(snapshot.status, 'working')
})

test('managed control removal fences pending companion validation', async t => {
  const o = await managedWorld(t)
  o.w.infos[SID] = { id: SID }
  await o.control()
  await o.tick()
  let release = () => {}
  o.w.infoGates[SID] = new Promise(resolve => { release = resolve })
  await o.open(SID)
  await rm(o.file)
  await o.tick()
  release()
  await o.settle()
  delete o.w.infoGates[SID]
  await o.control()
  await o.tick()
  assert.equal(o.w.to('/api/bridge/poll').at(-1).pluginTui, false)
})

test('periodic advertisements recover a restarted server bridge without turns or changed timestamps', async t => {
  const o = await world(t)
  o.w.infos[SID] = { id: SID, time: { created: 100, updated: 150 } }
  await o.open(SID)
  const before = o.w.to('/api/bridge/poll')[0].snapshot
  await o.restart()
  await o.tick()
  assert.equal(o.w.polls().length, 1)
  await o.open(SID)
  assert.equal(o.w.polls().length, 2)
  assert.deepEqual(o.w.to('/api/bridge/poll').at(-1).snapshot, before)
  assert.deepEqual(o.w.to('/api/bridge/event'), [])
})

test('unrelated commands and malformed presence markers never trigger discovery or companion freshness', async t => {
  const o = await afterOneTurn(t)
  const before = o.w.lookups.length
  for (const command of ['session.new', 'slorp.presence.v2:{}', PRESENCE_PREFIX + '{}', PRESENCE_PREFIX + 'x'.repeat(4096)]) {
    await o.emit('tui.command.execute', { command })
  }
  await o.emit('tui.session.select', { sessionID: SID })
  await o.tick()
  assert.equal(o.w.lookups.length, before)
  assert.equal(o.w.to('/api/bridge/poll').at(-1).pluginTui, false)
})

test('bridge version and managed revision identify the loaded installation', async t => {
  const o = await world(t, { revision: 'managed-build' })
  await o.say(SID, text('hello'))
  await o.tick()
  const manifest = JSON.parse(await readFile(new URL('./package.json', import.meta.url), 'utf8'))
  const poll = o.w.to('/api/bridge/poll')[0]
  assert.equal(poll.pluginVersion, manifest.version)
  assert.equal(poll.pluginVersion, '0.6.0')
  assert.equal(poll.pluginRevision, 'managed-build')
})

test('reopening after an overflow replay hydrates the original prompt and timestamp', async t => {
  const o = await world(t)
  const doc = JSON.parse(await readFile(new URL('../../internal/opencode/testdata/overflow.json', import.meta.url), 'utf8'))
  o.w.infos[SID] = { ...doc.info, id: SID, directory: '/work/proj', projectID: 'project' }
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
