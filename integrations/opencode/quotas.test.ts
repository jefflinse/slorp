import assert from 'node:assert/strict'
import { test, type TestContext } from 'node:test'
import { QuotaCollector } from './quotas.ts'

const NOW = Date.parse('2026-11-20T12:00:00Z')
const RESET = '2026-12-01T00:00:00.000Z'
const codex = { rate_limit: {
  primary_window: { used_percent: 25, limit_window_seconds: 18000, reset_at: Date.parse(RESET) / 1000 },
  secondary_window: { used_percent: 70, limit_window_seconds: 604800, reset_after_seconds: 7200 },
} }
const go = { usage: {
  rolling: { percent: 15, resetsAt: RESET, status: 'ok' },
  weekly: { percent: 50, resetsAt: RESET, status: 'ok' },
  monthly: { percent: 100, resetsAt: RESET, status: 'rate-limited' },
} }
const copilot = { quota_reset_date_utc: RESET, quota_snapshots: {
  premium_interactions: { percent_remaining: 60, unlimited: false, entitlement: '300', quota_remaining: 180 },
  chat: { percent_remaining: 0, unlimited: true, entitlement: 0 },
  completions: { percent_remaining: 80, unlimited: false, entitlement: 2000, quota_reset_at: Date.parse('2026-11-25T00:00:00Z') / 1000 },
} }

const settle = async () => { for (let i = 0; i < 20; i++) await new Promise(resolve => setImmediate(resolve)) }

function world(t: TestContext) {
  t.mock.timers.enable({ apis: ['setInterval', 'setTimeout', 'Date'], now: NOW })
  const calls: { url: string; init: RequestInit }[] = []
  const closed = new Set<string>()
  let reply: (url: string, init: RequestInit) => Promise<Response> = async () => Response.json(codex)
  const fetch = (url: string, init: RequestInit) => { calls.push({ url, init }); return reply(url, init) }
  const collector = new QuotaCollector(root => !closed.has(root), fetch)
  t.after(() => collector.dispose())
  const context = (id = 'openai', native = fetch) => ({
    provider: { id, key: undefined as unknown, options: {
      apiKey: (id === 'openai' ? 'opencode-oauth-dummy-key' : id === 'opencode-go' ? 'go-test-key' : '') as unknown,
      fetch: (id === 'opencode-go' ? undefined : native) as unknown,
      baseURL: undefined as unknown,
      headers: undefined as unknown,
    } },
    model: { providerID: id, api: { url: id === 'openai' ? 'https://api.openai.com/v1' : id === 'opencode-go' ? 'https://opencode.ai/zen/go/v1' : 'https://api.githubcopilot.com' }, options: {} as Record<string, unknown>, headers: undefined as unknown },
  })
  return {
    collector, calls, context, closed,
    reply: (fn: typeof reply) => { reply = fn },
    observe: (c = context(), root = 'root', origin = root) => collector.observe(root, origin, c.provider, c.model),
    snapshot: (root = 'root') => collector.snapshot(root),
    report: (root = 'root') => collector.snapshot(root)![0]!,
    tick: async (ms = 60_000) => { t.mock.timers.tick(ms); await settle() },
  }
}

test('Codex uses the native capability and normalizes actual durations and reset forms', async t => {
  const w = world(t)
  await w.tick()
  assert.equal(w.calls.length, 0)
  assert.equal(w.snapshot(), undefined)
  w.observe()
  await settle()
  assert.equal(w.calls[0]!.url, 'https://chatgpt.com/backend-api/wham/usage')
  assert.equal(w.calls[0]!.init.method, 'GET')
  assert.equal(w.calls[0]!.init.redirect, 'error')
  assert.equal(w.calls[0]!.init.headers, undefined)
  assert.deepEqual(w.report(), {
    provider: 'openai-codex', status: 'ok', checkedAt: NOW + 60_000, measuredAt: NOW + 60_000,
    windows: [
      { kind: 'primary_window', label: '5-hour', percentUsed: 25, resetsAt: RESET, windowSeconds: 18000 },
      { kind: 'secondary_window', label: '7-day', percentUsed: 70, resetsAt: new Date(NOW + 60_000 + 7200_000).toISOString(), windowSeconds: 604800 },
    ],
  })
})

test('Go uses only the fixed official endpoint and the runtime API key precedence', async t => {
  const w = world(t)
  w.reply(async () => Response.json(go))
  const c = w.context('opencode-go')
  c.provider.key = 'fallback-secret'
  w.observe(c)
  await settle()
  assert.equal(w.calls[0]!.url, 'https://opencode.ai/zen/go/v1/usage')
  assert.deepEqual(w.calls[0]!.init.headers, { Authorization: 'Bearer go-test-key' })
  assert.equal(w.calls[0]!.init.redirect, 'error')
  assert.deepEqual(w.report().windows.map(w => [w.kind, w.percentUsed, w.resetsAt]), [
    ['rolling', 15, RESET], ['weekly', 50, RESET], ['monthly', 100, RESET],
  ])
  assert.equal(w.report().status, 'ok')
  c.provider.options.apiKey = undefined
  w.observe(c)
  assert.deepEqual(w.report().windows, [])
  await w.tick()
  assert.deepEqual(w.calls[1]!.init.headers, { Authorization: 'Bearer fallback-secret' })
  assert.doesNotMatch(JSON.stringify(w.snapshot()), /secret|go-test-key|Authorization/)
})

test('Copilot uses its native capability, finite entitlements, and reset fallbacks', async t => {
  const w = world(t)
  w.reply(async () => Response.json(copilot))
  w.observe(w.context('github-copilot'))
  await settle()
  assert.equal(w.calls[0]!.url, 'https://api.github.com/copilot_internal/user')
  assert.equal(w.calls[0]!.init.headers, undefined)
  assert.deepEqual(w.report().windows, [
    { kind: 'premium_interactions', label: 'Premium requests', percentUsed: 40, resetsAt: RESET },
    { kind: 'completions', label: 'Completions', percentUsed: 20, resetsAt: '2026-11-25T00:00:00.000Z' },
  ])
})

test('unsupported runtime/auth/endpoint shapes never invoke a transport', async t => {
  const cases: { name: string; id?: string; change: (c: ReturnType<ReturnType<typeof world>['context']>) => void }[] = [
    { name: 'OpenAI API key', change: c => { c.provider.options.apiKey = 'sk-test' } },
    { name: 'OpenAI default API key', change: c => { c.provider.options.apiKey = undefined; c.provider.options.fetch = undefined; c.provider.key = 'sk-test' } },
    { name: 'wrong dummy', change: c => { c.provider.options.apiKey = 'opencode_dummy_key' } },
    { name: 'no native capability', change: c => { c.provider.options.fetch = undefined } },
    { name: 'no native auth shape', id: 'github-copilot', change: c => { c.provider.options.apiKey = undefined } },
    { name: 'Copilot Enterprise', id: 'github-copilot', change: c => { c.model.api.url = 'https://copilot-api.company.ghe.com' } },
    { name: 'legacy Enterprise id', id: 'github-copilot-enterprise', change: () => {} },
    { name: 'model proxy', change: c => { c.model.api.url = 'https://proxy.example/v1' } },
    { name: 'baseURL proxy', change: c => { c.provider.options.baseURL = 'https://proxy.example/v1' } },
    { name: 'model baseURL proxy', change: c => { c.model.options.baseURL = 'https://proxy.example/v1' } },
    { name: 'model credential override', change: c => { c.model.options.apiKey = 'other' } },
    { name: 'mismatched model', change: c => { c.model.providerID = 'other' } },
    { name: 'URL userinfo', change: c => { c.model.api.url = 'https://user@api.openai.com/v1' } },
    { name: 'URL query', change: c => { c.model.api.url += '?proxy=1' } },
    { name: 'Go proxy', id: 'opencode-go', change: c => { c.model.api.url = 'https://proxy.example/zen/go/v1' } },
    { name: 'Go OAuth sentinel', id: 'opencode-go', change: c => { c.provider.options.apiKey = 'opencode-oauth-dummy-key' } },
    { name: 'Go custom fetch', id: 'opencode-go', change: c => { c.provider.options.fetch = async () => { throw new Error('must not run') } } },
  ]
  for (const item of cases) await t.test(item.name, async t => {
    const w = world(t), c = w.context(item.id)
    item.change(c)
    w.observe(c)
    await settle()
    await w.tick(300_000)
    assert.equal(w.calls.length, 0)
    assert.equal(w.report().status, 'unsupported')
    assert.deepEqual(w.report().windows, [])
    assert.equal(w.report().measuredAt, undefined)
  })
})

test('missing Go key needs auth; unknown providers are omitted; legacy SDK context cannot grant capability', async t => {
  const w = world(t)
  w.observe(w.context('custom-openai'))
  assert.equal(w.snapshot(), undefined)
  const c = w.context('opencode-go')
  c.provider.options.apiKey = ''
  c.provider.key = 'must-not-fallback'
  w.observe(c)
  assert.equal(w.report().status, 'auth_required')
  w.collector.observe('legacy', 'legacy', { source: 'api', info: w.context().provider, options: {} }, w.context().model)
  assert.equal(w.report('legacy').status, 'unsupported')
  assert.equal(w.calls.length, 0)
})

test('schema validation never substitutes fabricated zero or exhaustion', async t => {
  const cases: [string, string, unknown, string, number][] = [
    ['Codex absent rate_limit', 'openai', {}, 'unavailable', 0],
    ['Codex no windows', 'openai', { rate_limit: {} }, 'unsupported', 0],
    ['Codex invalid reset', 'openai', { rate_limit: { primary_window: { ...codex.rate_limit.primary_window, reset_at: 0 } } }, 'unavailable', 0],
    ['Codex bad percentage', 'openai', { rate_limit: { primary_window: { ...codex.rate_limit.primary_window, used_percent: '0' } } }, 'unavailable', 0],
    ['Codex invalid duration', 'openai', { rate_limit: { primary_window: { ...codex.rate_limit.primary_window, limit_window_seconds: 0 } } }, 'unavailable', 0],
    ['Codex independently valid secondary', 'openai', { rate_limit: { primary_window: {}, secondary_window: codex.rate_limit.secondary_window } }, 'ok', 1],
    ['Go migrated schema', 'opencode-go', { usage: { credits: 100 } }, 'unavailable', 0],
    ['Go unknown status', 'opencode-go', { usage: { ...go.usage, rolling: { ...go.usage.rolling, status: 'migrated' } } }, 'unavailable', 0],
    ['Go coerced status', 'opencode-go', { usage: { ...go.usage, rolling: { ...go.usage.rolling, status: ['ok'] } } }, 'unavailable', 0],
    ['Go missing reset', 'opencode-go', { usage: { ...go.usage, monthly: { percent: 0, status: 'ok' } } }, 'unavailable', 0],
    ['Go impossible date', 'opencode-go', { usage: { ...go.usage, monthly: { ...go.usage.monthly, resetsAt: '2026-02-30T00:00:00Z' } } }, 'unavailable', 0],
    ['Copilot absent quotas', 'github-copilot', {}, 'unavailable', 0],
    ['Copilot no quotas', 'github-copilot', { quota_snapshots: {} }, 'unsupported', 0],
    ['Copilot zero denominator', 'github-copilot', { quota_snapshots: { chat: { percent_remaining: 0, unlimited: false, entitlement: 0 } } }, 'unsupported', 0],
    ['Copilot pooled unknown denominator', 'github-copilot', { quota_snapshots: { chat: { percent_remaining: 0, unlimited: false } } }, 'unsupported', 0],
    ['Copilot unlimited', 'github-copilot', { quota_snapshots: { chat: { percent_remaining: 0, unlimited: true, entitlement: 100 } } }, 'unsupported', 0],
    ['Copilot invalid denominator', 'github-copilot', { quota_snapshots: { chat: { percent_remaining: 0, unlimited: false, entitlement: 'unknown' } } }, 'unavailable', 0],
    ['Copilot zero string denominator', 'github-copilot', { quota_snapshots: { chat: { percent_remaining: 0, unlimited: false, entitlement: '0' } } }, 'unsupported', 0],
    ['Copilot invalid authoritative reset', 'github-copilot', { quota_reset_date: RESET, quota_snapshots: { chat: { percent_remaining: 0, unlimited: false, entitlement: '100', quota_reset_at: 0 } } }, 'unavailable', 0],
    ['Copilot missing reset', 'github-copilot', { quota_snapshots: { chat: { percent_remaining: 0, unlimited: false, entitlement: 100 } } }, 'unavailable', 0],
    ['Copilot date fallback', 'github-copilot', { quota_reset_date: '2026-12-01', quota_snapshots: { chat: { percent_remaining: 0, unlimited: false, entitlement: 100 } } }, 'ok', 1],
    ['Copilot invalid remaining quota', 'github-copilot', { quota_reset_date: RESET, quota_snapshots: { chat: { percent_remaining: 0, unlimited: false, entitlement: 100, quota_remaining: 101 } } }, 'unavailable', 0],
  ]
  for (const [name, id, body, status, count] of cases) await t.test(name, async t => {
    const w = world(t)
    w.reply(async () => Response.json(body))
    w.observe(w.context(id))
    await settle()
    assert.equal(w.report().status, status)
    assert.equal(w.report().windows.length, count)
    assert.equal(w.report().measuredAt, status === 'ok' ? NOW : undefined)
  })
})

test('cached Go snapshots and identical observations coalesce without manufacturing read timestamps', async t => {
  const w = world(t), c = w.context('opencode-go')
  w.reply(async () => Response.json(go))
  w.observe(c)
  await settle()
  const first = w.snapshot()
  for (let i = 0; i < 10; i++) { w.observe(c); assert.deepEqual(w.snapshot(), first) }
  await w.tick(59_999)
  assert.equal(w.calls.length, 1)
  assert.deepEqual(w.snapshot(), first)
  await w.tick(1)
  assert.equal(w.calls.length, 2)
  assert.equal(w.report().measuredAt, NOW + 60_000)
  const snapshot = w.snapshot()!
  snapshot[0]!.windows[0]!.percentUsed = 99
  assert.equal(w.report().windows[0]!.percentUsed, 15)
})

test('transient Go failures retain same-context measurements; auth clears them', async t => {
  const w = world(t)
  w.reply(async () => Response.json(go))
  w.observe(w.context('opencode-go'))
  await settle()
  const windows = w.report().windows
  w.reply(async () => new Response('secret error', { status: 503 }))
  await w.tick()
  assert.equal(w.report().status, 'unavailable')
  assert.equal(w.report().checkedAt, NOW + 60_000)
  assert.equal(w.report().measuredAt, NOW)
  assert.deepEqual(w.report().windows, windows)
  await w.tick()
  assert.equal(w.calls.length, 3)
  await w.tick()
  assert.equal(w.calls.length, 3, 'exponential backoff')
  w.reply(async () => new Response('secret auth failure', { status: 401 }))
  await w.tick()
  assert.equal(w.report().status, 'auth_required')
  assert.deepEqual(w.report().windows, [])
  assert.equal(w.report().measuredAt, undefined)
  assert.doesNotMatch(JSON.stringify(w.snapshot()), /secret/)
})

test('429 is throttling, not exhaustion, and honors seconds/date Retry-After', async t => {
  for (const value of ['180', new Date(NOW + 240_000).toUTCString()]) await t.test(value, async t => {
    const w = world(t)
    w.reply(async () => Response.json(go))
    w.observe(w.context('opencode-go'))
    await settle()
    w.reply(async () => new Response('private', { status: 429, headers: { 'Retry-After': value } }))
    await w.tick()
    assert.equal(w.report().status, 'throttled')
    assert.equal(w.report().retryAt, NOW + 240_000)
    assert.equal(w.report().measuredAt, NOW)
    assert.equal(w.report().windows[0]!.percentUsed, 15)
    await w.tick(120_000)
    assert.equal(w.calls.length, 2)
    await w.tick()
    assert.equal(w.calls.length, 3)
  })
})

test('HTTP, redirect, malformed JSON, oversized and streaming bodies fail closed', async t => {
  const cases: [string, () => Promise<Response>][] = [
    ['HTTP error', async () => new Response('private', { status: 500 })],
    ['redirect', async () => new Response(null, { status: 302, headers: { location: 'https://evil.example' } })],
    ['followed redirect', async () => { const r = Response.json(codex); Object.defineProperty(r, 'redirected', { value: true }); return r }],
    ['wrong response URL', async () => { const r = Response.json(codex); Object.defineProperty(r, 'url', { value: 'https://evil.example' }); return r }],
    ['fetch throws', async () => { throw new Error('private token') }],
    ['bad JSON', async () => new Response('{private')],
    ['too big', async () => new Response(' '.repeat(65537))],
    ['chunked too big', async () => new Response(new ReadableStream({ start(c) { c.enqueue(new Uint8Array(32768)); c.enqueue(new Uint8Array(32769)); c.close() } }))],
  ]
  for (const [name, reply] of cases) await t.test(name, async t => {
    const w = world(t)
    w.reply(reply)
    w.observe()
    await settle()
    assert.equal(w.report().status, 'unavailable')
    assert.deepEqual(w.report().windows, [])
    assert.equal(w.calls.length, 1)
    assert.doesNotMatch(JSON.stringify(w.snapshot()), /private|evil/)
  })
})

test('deadline fences an uncancellable fetch and prevents overlapping retries', async t => {
  const w = world(t)
  let resolve!: (r: Response) => void
  w.reply(() => new Promise(r => { resolve = r }))
  const c = w.context('opencode-go')
  w.observe(c)
  w.observe(c)
  assert.equal(w.calls.length, 1)
  await w.tick(5000)
  assert.equal(w.calls[0]!.init.signal!.aborted, true)
  assert.equal(w.report().checkedAt, NOW + 5000)
  assert.equal(w.report().status, 'unavailable')
  await w.tick(180_000)
  assert.equal(w.calls.length, 1)
  resolve(Response.json(codex))
  await settle()
  assert.equal(w.report().status, 'unavailable', 'late success must not overwrite timeout')
  w.reply(async () => Response.json(go))
  await w.tick()
  assert.equal(w.calls.length, 2)
  assert.equal(w.report().status, 'ok')
})

test('deadline covers response body and cancels the stream', async t => {
  const w = world(t)
  let cancelled = false
  w.reply(async () => new Response(new ReadableStream({ cancel() { cancelled = true } })))
  w.observe()
  await settle()
  await w.tick(5000)
  assert.equal(cancelled, true)
  assert.equal(w.report().status, 'unavailable')
  assert.equal(w.report().measuredAt, undefined)
})

test('context replacement aborts and fences old results, including same-provider key/function changes', async t => {
  const w = world(t)
  let resolve!: (r: Response) => void
  w.reply(() => new Promise(r => { resolve = r }))
  const c = w.context()
  w.observe(c)
  c.provider.options.fetch = async () => Response.json({ rate_limit: { primary_window: { ...codex.rate_limit.primary_window, used_percent: 90 } } })
  w.observe(c)
  assert.equal(w.calls[0]!.init.signal!.aborted, true)
  await settle()
  assert.deepEqual(w.report().windows, [])
  resolve(Response.json(codex))
  await settle()
  assert.deepEqual(w.report().windows, [])
  await w.tick()
  assert.equal(w.report().windows[0]!.percentUsed, 90)
  c.provider.key = 'new-key-identity'
  w.observe(c)
  assert.deepEqual(w.report().windows, [])
  await settle()
  c.provider.options.apiKey = 'api-key'
  w.observe(c)
  assert.equal(w.report().status, 'unsupported')
  assert.deepEqual(w.report().windows, [])
})

test('roots are independent and root snapshots select, not merge, same-provider contexts', async t => {
  const w = world(t), c = w.context()
  w.reply(async url => Response.json(url.includes('copilot') ? copilot : codex))
  w.observe(c, 'one')
  w.observe(c, 'two')
  await settle()
  assert.equal(w.calls.length, 2)
  w.observe(w.context('github-copilot'), 'one', 'child')
  await settle()
  assert.equal(w.snapshot('one')!.length, 2)
  assert.equal(w.snapshot('two')!.length, 1)
  const alternate = w.context('openai', async () => new Response(null, { status: 401 }))
  w.observe(alternate, 'one', 'other-child')
  await settle()
  assert.equal(w.snapshot('one')!.find(r => r.provider === 'openai-codex')!.status, 'unavailable')
  assert.equal(w.report('two').status, 'ok')
  await w.tick()
  assert.equal(w.snapshot('one')!.find(r => r.provider === 'openai-codex')!.status, 'auth_required')
  w.collector.remove('other-child')
  await settle()
  assert.equal(w.snapshot('one')!.find(r => r.provider === 'openai-codex')!.status, 'unavailable')
  await w.tick()
  assert.equal(w.snapshot('one')!.find(r => r.provider === 'openai-codex')!.status, 'ok')
  w.observe(w.context('unknown'), 'one', 'one')
  assert.equal(w.snapshot('one')!.length, 1)
  w.collector.remove('child')
  assert.deepEqual(w.snapshot('one'), [])
})

test('all three reports are bounded and retain independent statuses', async t => {
  const w = world(t)
  w.reply(async url => url.includes('wham') ? Response.json(codex) : url.includes('copilot') ? new Response(null, { status: 429 }) : Response.json(go))
  w.observe(w.context(), 'root', 'one')
  w.observe(w.context('github-copilot'), 'root', 'two')
  w.observe(w.context('opencode-go'), 'root', 'three')
  w.observe(w.context('unknown'), 'root', 'four')
  await settle()
  assert.deepEqual(w.snapshot()!.map(r => [r.provider, r.status]), [['openai-codex', 'ok'], ['github-copilot', 'throttled'], ['opencode-go', 'ok']])
  assert.ok(w.snapshot()!.every(r => r.windows.length <= 8))
})

test('removing a branch does not briefly reactivate another context in that same branch', async t => {
  const w = world(t)
  let first = 0, second = 0
  w.observe(w.context('openai', async () => { first++; return Response.json(codex) }), 'root', 'grandchild')
  await settle()
  w.observe(w.context('openai', async () => { second++; return Response.json(codex) }), 'root', 'child')
  await w.tick()
  w.collector.remove('child', 'grandchild')
  await settle()
  assert.deepEqual(w.snapshot(), [])
  assert.equal(first, 1)
  assert.equal(second, 1)
})

test('clear, closed-root polling, and disposal abort work and discard credentials/results', async t => {
  for (const action of ['clear', 'closed', 'dispose'] as const) await t.test(action, async t => {
    const w = world(t)
    let resolve!: (r: Response) => void
    w.reply(() => new Promise(r => { resolve = r }))
    w.observe()
    if (action === 'clear') w.collector.clear('root')
    if (action === 'closed') { w.closed.add('root'); await w.tick() }
    if (action === 'dispose') w.collector.dispose()
    assert.equal(w.calls[0]!.init.signal!.aborted, true)
    resolve(Response.json(codex))
    await settle()
    assert.deepEqual(w.snapshot(), action === 'dispose' ? undefined : [])
    await w.tick(120_000)
    assert.equal(w.calls.length, 1)
  })
})

test('OAuth observations fence same-function account switches and failures never retain OAuth measurements', async t => {
  for (const id of ['openai', 'github-copilot']) await t.test(id, async t => {
    const w = world(t)
    const body = id === 'openai' ? codex : copilot
    w.reply(async () => Response.json(body))
    const c = w.context(id)
    w.observe(c)
    await settle()
    assert.equal(w.report().status, 'ok')
    w.reply(async () => new Response(null, { status: 503 }))
    w.observe(c)
    assert.equal(w.report().status, 'unavailable')
    assert.deepEqual(w.report().windows, [])
    assert.equal(w.report().measuredAt, undefined)
    await w.tick()
    assert.deepEqual(w.report().windows, [])
    w.reply(async () => Response.json(body))
    await w.tick()
    assert.equal(w.report().status, 'ok')
    w.reply(async () => new Response(null, { status: 429 }))
    await w.tick()
    assert.equal(w.report().status, 'throttled')
    assert.deepEqual(w.report().windows, [], 'even without another hook a native closure may have switched accounts')
    assert.equal(w.report().measuredAt, undefined)
  })
})

test('same native function cannot publish an outstanding old observation after a new observation', async t => {
  const w = world(t), c = w.context()
  let resolve!: (r: Response) => void
  w.reply(() => new Promise(r => { resolve = r }))
  w.observe(c)
  w.observe(c)
  assert.equal(w.calls.length, 1)
  assert.equal(w.calls[0]!.init.signal!.aborted, true)
  const revision = w.collector.metadata('root')!.providerLimitsRevision
  resolve(Response.json(codex))
  await settle()
  assert.deepEqual(w.report().windows, [])
  assert.equal(w.collector.metadata('root')!.providerLimitsRevision, revision)
  w.reply(async () => Response.json(codex))
  for (let i = 0; i < 59; i++) { await w.tick(1000); w.observe(c) }
  assert.equal(w.calls.length, 1)
  await w.tick(1000)
  assert.equal(w.calls.length, 2, 'observations do not postpone the transport cooldown')
  assert.equal(w.report().status, 'ok')
})

test('hook entry synchronously fences associated OAuth work while root resolution is pending', async t => {
  const w = world(t), c = w.context()
  let resolve!: (r: Response) => void
  w.reply(() => new Promise(r => { resolve = r }))
  w.observe(c)
  const sequence = w.collector.nextSequence('root')
  assert.equal(w.calls[0]!.init.signal!.aborted, true)
  const metadata = w.collector.metadata('root')
  resolve(Response.json(codex))
  await settle()
  await w.tick(60_000)
  w.collector.activate('root')
  assert.equal(w.calls.length, 1, 'pending lookup must not re-use the old context')
  assert.deepEqual(w.report().windows, [])
  assert.deepEqual(w.collector.metadata('root'), metadata)
  w.reply(async () => Response.json(codex))
  w.collector.observe('root', 'root', c.provider, c.model, false, sequence)
  await settle()
  assert.equal(w.calls.length, 2)
  assert.equal(w.report().status, 'ok')
})

test('a known nonselected child hook fences the selected root OAuth read before child resolution', async t => {
  const w = world(t), c = w.context()
  w.observe(c, 'root', 'child')
  await settle()
  let resolve!: (r: Response) => void
  w.reply(() => new Promise(r => { resolve = r }))
  w.observe(c, 'root', 'root')
  await w.tick()
  assert.equal(w.calls.length, 2)
  const revision = w.collector.metadata('root')!.providerLimitsRevision

  const sequence = w.collector.nextSequence('child')
  assert.equal(sequence, 3)
  assert.equal(w.calls[1]!.init.signal!.aborted, true)
  assert.equal(w.report().status, 'unavailable')
  assert.deepEqual(w.report().windows, [])
  assert.equal(w.report().measuredAt, undefined)
  const pending = w.collector.metadata('root')!
  assert.equal(pending.providerLimitsRevision, revision + 1)

  resolve(Response.json(codex))
  await settle()
  await w.tick()
  w.collector.activate('root')
  assert.equal(w.calls.length, 2, 'expired cooldown must not re-use the root context while child resolution is pending')
  assert.deepEqual(w.report().windows, [])
  assert.deepEqual(w.collector.metadata('root'), pending, 'the old root result cannot publish')

  w.reply(async () => Response.json({ rate_limit: { primary_window: { ...codex.rate_limit.primary_window, used_percent: 77 } } }))
  w.collector.observe('root', 'child', c.provider, c.model, false, sequence)
  await settle()
  assert.equal(w.calls.length, 3)
  assert.equal(w.report().status, 'ok')
  assert.equal(w.report().windows[0]!.percentUsed, 77)
})

test('a known nonselected child hook does not fence a same-key Go read or clear its measurement', async t => {
  const w = world(t), c = w.context('opencode-go')
  w.reply(async () => Response.json(go))
  w.observe(c, 'root', 'child')
  await settle()
  w.observe(c, 'root', 'root')
  let resolve!: (r: Response) => void
  w.reply(() => new Promise(r => { resolve = r }))
  await w.tick()
  assert.equal(w.calls.length, 2)
  const report = w.report(), metadata = w.collector.metadata('root')

  const sequence = w.collector.nextSequence('child')
  assert.equal(sequence, 3)
  assert.equal(w.calls[1]!.init.signal!.aborted, false)
  assert.deepEqual(w.report(), report)
  assert.deepEqual(w.collector.metadata('root'), metadata)
  resolve(Response.json(go))
  await settle()
  assert.equal(w.report().status, 'ok')
  assert.equal(w.report().measuredAt, NOW + 60_000)
  const measured = w.report()
  w.collector.observe('root', 'child', c.provider, c.model, false, sequence)
  assert.deepEqual(w.report(), measured)
  assert.equal(w.calls.length, 2)
})

test('hook-entry sequence wins across origins despite reversed lookup completion and retains fallback', async t => {
  const w = world(t)
  const old = w.context('openai', async () => Response.json(codex))
  const current = w.context('openai', async () => Response.json({ rate_limit: { primary_window: { ...codex.rate_limit.primary_window, used_percent: 90 } } }))
  const a = w.collector.nextSequence(), b = w.collector.nextSequence()
  w.collector.observe('root', 'new', current.provider, current.model, false, b)
  await settle()
  const revision = w.collector.metadata('root')!.providerLimitsRevision
  w.collector.observe('root', 'old', old.provider, old.model, false, a)
  assert.equal(w.report().windows[0]!.percentUsed, 90)
  assert.equal(w.collector.metadata('root')!.providerLimitsRevision, revision)
  w.collector.remove('new')
  assert.deepEqual(w.report().windows, [])
  await w.tick()
  assert.equal(w.report().windows[0]!.percentUsed, 25)
})

test('Go auth-header overrides invalidate measurements without invoking any transport', async t => {
  for (const where of ['provider', 'model', 'model-options']) for (const key of ['Authorization', 'aUtHoRiZaTiOn', 'X-API-Key', 'Proxy-Authorization', 'Cookie']) {
    await t.test(`${where}: ${key}`, async t => {
      const w = world(t), c = w.context('opencode-go')
      w.reply(async () => Response.json(go))
      w.observe(c)
      await settle()
      const revision = w.collector.metadata('root')!.providerLimitsRevision
      const headers = { [key]: 'Bearer other-account-secret' }
      if (where === 'provider') c.provider.options.headers = headers
      if (where === 'model') c.model.headers = headers
      if (where === 'model-options') c.model.options.headers = headers
      w.observe(c)
      assert.equal(w.report().status, 'unsupported')
      assert.deepEqual(w.report().windows, [])
      assert.ok(w.collector.metadata('root')!.providerLimitsRevision > revision)
      await w.tick(120_000)
      assert.equal(w.calls.length, 1)
      assert.doesNotMatch(JSON.stringify(w.snapshot()), /secret|Bearer/)
    })
  }
})

test('Go benign header changes also invalidate the previous context, including in-place mutation', async t => {
  const w = world(t), c = w.context('opencode-go')
  const headers = { 'X-Workspace': 'first' }
  c.provider.options.headers = headers
  w.reply(async () => Response.json(go))
  w.observe(c)
  await settle()
  headers['X-Workspace'] = 'second'
  w.observe(c)
  assert.deepEqual(w.report().windows, [])
  w.reply(async () => new Response(null, { status: 500 }))
  await w.tick()
  assert.deepEqual(w.report().windows, [])
})

test('durable transport slots block function/key churn, removal and clear until the old transport settles', async t => {
  const w = world(t)
  let resolve!: (r: Response) => void
  w.reply(() => new Promise(r => { resolve = r }))
  w.observe()
  let latest = 0
  for (let i = 0; i < 100; i++) {
    w.observe(w.context('unknown'))
    w.collector.clear('root')
    w.observe(w.context('openai', async () => { latest++; return Response.json(codex) }))
  }
  await settle()
  await w.tick(120_000)
  assert.equal(w.calls.length, 1)
  assert.equal(latest, 0)
  resolve(Response.json(codex))
  await settle()
  assert.deepEqual(w.report().windows, [])
  await w.tick(1)
  assert.equal(latest, 1)
  assert.equal(w.report().status, 'ok')
})

test('provider-wide Retry-After survives context removal, root clear, reactivation and switchbacks', async t => {
  const w = world(t), c = w.context()
  w.reply(async () => new Response(null, { status: 429, headers: { 'Retry-After': '3600' } }))
  w.observe(c)
  await settle()
  assert.equal(w.report().retryAt, NOW + 3600_000)
  let otherCalls = 0
  const other = w.context('openai', async () => { otherCalls++; return Response.json(codex) })
  w.observe(w.context('unknown'))
  w.observe(other)
  w.collector.clear('root')
  w.observe(c)
  w.collector.activate('root')
  w.observe(other)
  await w.tick(3599_999)
  assert.equal(w.calls.length, 1)
  assert.equal(otherCalls, 0)
  await w.tick(1)
  assert.equal(otherCalls, 1)
})

test('Go key churn shares the durable transport lock and only the latest key is retried', async t => {
  const w = world(t), c = w.context('opencode-go')
  let resolve!: (r: Response) => void
  w.reply(() => new Promise(r => { resolve = r }))
  w.observe(c)
  for (let i = 0; i < 50; i++) {
    c.provider.options.apiKey = `test-key-${i}`
    w.observe(c)
  }
  await settle()
  await w.tick(120_000)
  assert.equal(w.calls.length, 1)
  resolve(Response.json(go))
  await settle()
  assert.deepEqual(w.report().windows, [])
  w.reply(async () => Response.json(go))
  await w.tick(1)
  assert.equal(w.calls.length, 2)
  assert.deepEqual(w.calls[1]!.init.headers, { Authorization: 'Bearer test-key-49' })
  assert.equal(w.report().status, 'ok')
})

test('late throttling from an aborted transport still delays the latest context', async t => {
  const w = world(t)
  let resolve!: (r: Response) => void
  w.reply(() => new Promise(r => { resolve = r }))
  w.observe()
  let next = 0
  w.observe(w.context('openai', async () => { next++; return Response.json(codex) }))
  await settle()
  resolve(new Response(null, { status: 429, headers: { 'Retry-After': '3600' } }))
  await settle()
  await w.tick(3599_999)
  assert.equal(next, 0)
  await w.tick(1)
  assert.equal(next, 1)
})

test('disposal cannot reschedule a pending replacement after an uncancellable transport settles', async t => {
  const w = world(t)
  let resolve!: (r: Response) => void
  w.reply(() => new Promise(r => { resolve = r }))
  w.observe()
  let next = 0
  w.observe(w.context('openai', async () => { next++; return Response.json(codex) }))
  w.collector.dispose()
  resolve(Response.json(codex))
  await settle()
  await w.tick(3600_000)
  assert.equal(next, 0)
  assert.equal(w.snapshot(), undefined)
})

test('reset years, duration integers and emitted retry bounds obey the wire contract', async t => {
  for (const reset of [253402300800, 8640000000000, -1]) await t.test(`reset ${reset}`, async t => {
    const w = world(t)
    w.reply(async () => Response.json({ rate_limit: { primary_window: { ...codex.rate_limit.primary_window, reset_at: reset } } }))
    w.observe()
    await settle()
    assert.equal(w.report().status, 'unavailable')
    assert.deepEqual(w.report().windows, [])
  })
  for (const seconds of [Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER + 1, 1.5]) await t.test(`duration ${seconds}`, async t => {
    const w = world(t)
    w.reply(async () => Response.json({ rate_limit: { primary_window: { ...codex.rate_limit.primary_window, limit_window_seconds: seconds, reset_at: 253402300799 } } }))
    w.observe()
    await settle()
    if (Number.isSafeInteger(seconds)) {
      assert.equal(w.report().windows[0]!.resetsAt, '9999-12-31T23:59:59.000Z')
      assert.ok(w.report().windows[0]!.label.length <= 100)
    } else assert.equal(w.report().status, 'unavailable')
  })
  await t.test('Retry-After caps scheduling and wire at 24 hours', async t => {
    const w = world(t)
    w.reply(async () => new Response(null, { status: 429, headers: { 'Retry-After': '172800' } }))
    w.observe()
    await settle()
    assert.equal(w.report().retryAt, NOW + 86400_000)
    await w.tick(86400_000 - 1)
    assert.equal(w.calls.length, 1)
    await w.tick(1)
    assert.equal(w.calls.length, 2)
  })
  await t.test('backoff stops increasing at fifteen minutes', async t => {
    const w = world(t)
    w.reply(async () => new Response(null, { status: 500 }))
    w.observe()
    await settle()
    for (const expected of [60_000, 120_000, 240_000, 480_000, 900_000, 900_000]) {
      const report = w.report()
      assert.equal(report.retryAt! - report.checkedAt, expected)
      await w.tick(expected)
    }
  })
})

test('snapshot metadata versions report and membership changes but not cached polls', async t => {
  const w = world(t)
  assert.equal(w.collector.metadata('root'), undefined)
  w.observe()
  const initial = w.collector.metadata('root')!
  assert.match(initial.providerLimitsEpoch, /^[a-f0-9-]{36}$/)
  assert.equal(initial.providerLimitsRevision, 1)
  await settle()
  const measured = w.collector.metadata('root')!
  assert.equal(measured.providerLimitsRevision, 2)
  const old = w.report()
  w.reply(async () => Response.json(copilot))
  w.observe(w.context('github-copilot'), 'root', 'child')
  await settle()
  const both = w.collector.metadata('root')!
  assert.equal(both.providerLimitsRevision, 4)
  w.collector.remove('child')
  const removed = w.collector.metadata('root')!
  assert.equal(removed.providerLimitsRevision, 5)
  assert.deepEqual(w.report(), old, 'remaining cached reports need not get new checkedAt timestamps')
  for (let i = 0; i < 10; i++) { w.snapshot(); w.collector.activate('root'); assert.deepEqual(w.collector.metadata('root'), removed) }
  w.collector.clear('root')
  const empty = w.collector.metadata('root')!
  assert.equal(empty.providerLimitsRevision, 6)
  assert.deepEqual(w.snapshot(), [])
  assert.equal(empty.providerLimitsEpoch, initial.providerLimitsEpoch)
  w.collector.clear('root')
  assert.equal(w.collector.metadata('root')!.providerLimitsRevision, 7, 'explicit clears are ordered even within the same millisecond')
  w.observe(w.context('openai'), 'other')
  assert.equal(w.collector.metadata('other')!.providerLimitsRevision, 1)
  assert.equal(w.collector.metadata('other')!.providerLimitsEpoch, initial.providerLimitsEpoch)
  const fresh = new QuotaCollector(() => true, async () => Response.json(go))
  t.after(() => fresh.dispose())
  const c = w.context('opencode-go')
  fresh.observe('root', 'root', c.provider, c.model)
  assert.notEqual(fresh.metadata('root')!.providerLimitsEpoch, initial.providerLimitsEpoch)
  assert.equal(fresh.metadata('root')!.providerLimitsRevision, 1)
})
