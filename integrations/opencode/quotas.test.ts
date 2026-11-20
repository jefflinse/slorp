import assert from 'node:assert/strict'
import { test, type TestContext } from 'node:test'
import { QuotaCollector, quotaSource, type Source } from './quotas.ts'

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
  completions: { percent_remaining: 80, unlimited: false, entitlement: 2000 },
} }
const settle = async () => { for (let i = 0; i < 15; i++) await new Promise(resolve => setImmediate(resolve)) }

function world(t: TestContext) {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'], now: NOW })
  const calls: { url: string; init: RequestInit }[] = []
  const closed = new Set<string>()
  let response = async (_url: string, _init: RequestInit): Promise<Response> => Response.json(codex)
  const fetch = (url: string, init: RequestInit) => { calls.push({ url, init }); return response(url, init) }
  const collector = new QuotaCollector(root => !closed.has(root))
  t.after(() => collector.dispose())
  const source = (provider: Source['provider'] = 'openai-codex', identity: unknown[] = ['credential1']): Source =>
    ({ provider, identity, status: 'unavailable', fetch })
  return { calls, closed, collector, source, fetch,
    response: (fn: typeof response) => { response = fn },
    observe: (s = source(), root = 'root', origin = root) => collector.observe(root, origin, s),
    report: (root = 'root') => collector.snapshot(root)![0]!,
    tick: async (ms = 60000) => { t.mock.timers.tick(ms); await settle() },
  }
}

function context(id = 'openai') {
  const state = {
    connection: { type: 'credential', id: 'credOne', method: id === 'opencode-go' ? 'key' : 'oauth' } as any,
    credential: id === 'opencode-go' ? { type: 'key', key: 'go-key' } as any :
      { type: 'oauth', methodID: 'oauth', access: 'access-token', refresh: 'github-token', expires: NOW + 100000, metadata: { accountID: 'account' } } as any,
  }
  const ctx = { app: { version: '2.3.2' }, integration: { connection: {
    active: async (_: string) => state.connection,
    resolve: async (_: any) => state.credential,
  } } }
  const baseURL = id === 'openai' ? 'https://chatgpt.com/backend-api/codex' :
    id === 'github-copilot' ? 'https://api.githubcopilot.com' : 'https://opencode.ai/zen/go/v1'
  const provider: any = { id, integrationID: id, package: '@opencode/ai/providers/openai', settings: { baseURL } }
  const model: any = { id: 'model', providerID: id, package: '@opencode/ai/providers/openai', settings: { baseURL } }
  return { ctx, state, provider, model }
}

test('V2 Codex resolves current OAuth access and capital-ID account metadata with materialized packages', async t => {
  const w = world(t), c = context()
  const source = await quotaSource(c.ctx as any, c.provider, c.model, w.fetch)
  assert.ok(source?.fetch)
  w.observe(source)
  await settle()
  assert.equal(w.calls[0]!.url, 'https://chatgpt.com/backend-api/wham/usage')
  assert.deepEqual(w.calls[0]!.init.headers, { authorization: 'Bearer access-token', 'ChatGPT-Account-Id': 'account' })
  assert.equal(w.calls[0]!.init.redirect, 'error')
  assert.deepEqual(w.report().windows, [
    { kind: 'primary_window', label: '5-hour', percentUsed: 25, resetsAt: RESET, windowSeconds: 18000 },
    { kind: 'secondary_window', label: '7-day', percentUsed: 70, resetsAt: new Date(NOW + 7200000).toISOString(), windowSeconds: 604800 },
  ])
  c.state.credential.access = 'refreshed-token'
  await w.tick()
  assert.equal(new Headers(w.calls[1]!.init.headers).get('authorization'), 'Bearer refreshed-token')
  assert.doesNotMatch(JSON.stringify(w.collector.snapshot('root')), /token|account|credential/)
})

test('V2 Copilot uses GitHub refresh token and supports account-specific and Anthropic protocol endpoints', async t => {
  const w = world(t), c = context('github-copilot')
  c.state.credential.metadata = { apiEndpoint: 'https://api.individual.githubcopilot.com' }
  c.provider.settings.baseURL = c.state.credential.metadata.apiEndpoint
  c.model.settings.baseURL = c.provider.settings.baseURL + '/v1'
  c.model.package = '@opencode/ai/providers/anthropic'
  w.response(async () => Response.json(copilot))
  const source = await quotaSource(c.ctx as any, c.provider, c.model, w.fetch)
  assert.ok(source?.fetch)
  w.observe(source)
  await settle()
  assert.equal(w.calls[0]!.url, 'https://api.github.com/copilot_internal/user')
  assert.equal(new Headers(w.calls[0]!.init.headers).get('authorization'), 'Bearer github-token')
  assert.deepEqual(w.report().windows.map(w => w.percentUsed), [40, 20])
})

test('V2 Go resolves the integration key and preserves all quota windows', async t => {
  const w = world(t), c = context('opencode-go')
  w.response(async () => Response.json(go))
  w.observe((await quotaSource(c.ctx as any, c.provider, c.model, w.fetch))!)
  await settle()
  assert.equal(w.calls[0]!.url, 'https://opencode.ai/zen/go/v1/usage')
  assert.equal(new Headers(w.calls[0]!.init.headers).get('authorization'), 'Bearer go-key')
  assert.deepEqual(w.report().windows.map(w => w.percentUsed), [15, 50, 100])
})

test('native protocol packages grant only the matching official account transport', async t => {
  for (const [id, pkg] of [
    ['openai', '@opencode/ai/providers/openai/responses'],
    ['github-copilot', '@opencode/ai/providers/openai/chat'],
    ['github-copilot', '@opencode/ai/providers/openai/responses'],
    ['github-copilot', '@opencode/ai/providers/openai-compatible/responses'],
    ['github-copilot', '@opencode/ai/providers/openai-compatible-responses'],
    ['opencode-go', '@opencode/ai/providers/openai/chat'],
  ]) await t.test(`${id} ${pkg}`, async () => {
    const c = context(id)
    c.provider.package = pkg
    c.model.package = pkg
    c.model.variants = [{ id: 'high', settings: { reasoningEffort: 'high' } }]
    const source = await quotaSource(c.ctx as any, c.provider, c.model)
    assert.equal(source?.status, 'unavailable')
    assert.equal(typeof source?.fetch, 'function')
  })
})

test('known selected variants ignore unrelated overrides while unknown selection remains conservative', async t => {
  for (const variant of [undefined, 'default', 'clean']) await t.test(variant ?? 'base', async t => {
    const w = world(t), c = context()
    c.model.variants = [
      { id: 'clean', settings: { reasoningEffort: 'high' } },
      { id: 'proxy', settings: { baseURL: 'https://proxy.invalid', apiKey: 'unused-secret' }, headers: { authorization: 'unused-secret' } },
    ]
    const selection = { providerID: c.model.providerID, id: c.model.id, variant }
    const source = (await quotaSource(c.ctx as any, c.provider, c.model, w.fetch, selection))!
    assert.equal(typeof source.fetch, 'function')
    w.observe(source)
    await settle()
    assert.equal(w.report().status, 'ok')
    assert.equal(w.calls[0]!.url, 'https://chatgpt.com/backend-api/wham/usage')
    assert.equal(new Headers(w.calls[0]!.init.headers).get('authorization'), 'Bearer access-token')
    c.model.variants[1].settings.apiKey = 'different-unused-secret'
    assert.deepEqual((await quotaSource(c.ctx as any, c.provider, c.model, w.fetch, selection))!.identity, source.identity)
    const unknown = await quotaSource(c.ctx as any, c.provider, c.model, w.fetch)
    assert.equal(unknown?.status, 'unsupported')
    assert.equal(unknown?.fetch, undefined)
  })
})

test('selected unsafe or missing variants and mismatched model references cannot grant transport', async t => {
  for (const [name, selected, change] of [
    ['proxy variant', 'custom', { settings: { baseURL: 'https://proxy.invalid' } }],
    ['key variant', 'custom', { settings: { apiKey: 'selected-secret' } }],
    ['header variant', 'custom', { headers: { authorization: 'selected-secret' } }],
    ['account variant', 'custom', { headers: { 'ChatGPT-Account-Id': 'other-account' } }],
    ['missing variant', 'missing', { settings: { reasoningEffort: 'high' } }],
  ] as const) await t.test(name, async () => {
    const c = context()
    c.model.variants = [{ id: 'custom', ...change }]
    const source = await quotaSource(c.ctx as any, c.provider, c.model, undefined,
      { providerID: c.model.providerID, id: c.model.id, variant: selected })
    assert.equal(source?.status, 'unsupported')
    assert.equal(source?.fetch, undefined)
  })
  for (const selection of [{ providerID: 'github-copilot', id: 'model' }, { providerID: 'openai', id: 'other-model' }]) {
    const c = context()
    const source = await quotaSource(c.ctx as any, c.provider, c.model, undefined, selection)
    assert.equal(source?.status, 'unsupported')
    assert.equal(source?.fetch, undefined)
  }
})

test('unsupported auth, Enterprise, package and endpoint overrides never grant a transport', async t => {
  const cases: [string, string, (c: ReturnType<typeof context>) => void][] = [
    ['OpenAI key', 'openai', c => { c.state.credential = { type: 'key', key: 'key' } }],
    ['Enterprise metadata', 'github-copilot', c => { c.state.credential.metadata.enterpriseUrl = 'company.ghe.com' }],
    ['custom package', 'openai', c => { c.model.package = 'custom' }],
    ['provider proxy', 'openai', c => { c.provider.settings.baseURL = 'https://proxy.invalid' }],
    ['model proxy', 'openai', c => { c.model.settings.baseURL = 'https://proxy.invalid' }],
    ['model key', 'openai', c => { c.model.settings.apiKey = 'override' }],
    ['auth header', 'openai', c => { c.provider.headers = { Authorization: 'other' } }],
    ['wrong model', 'openai', c => { c.model.providerID = 'other' }],
    ['Go OAuth', 'opencode-go', c => { c.state.credential = { type: 'oauth', access: 'token' } }],
    ['credential proxy', 'github-copilot', c => {
      c.state.credential.metadata.apiEndpoint = 'https://proxy.invalid'
      c.provider.settings.baseURL = c.model.settings.baseURL = 'https://proxy.invalid'
    }],
    ['lookalike Copilot endpoint', 'github-copilot', c => {
      c.state.credential.metadata.apiEndpoint = 'https://api.githubcopilot.com.evil.invalid'
      c.provider.settings.baseURL = c.model.settings.baseURL = c.state.credential.metadata.apiEndpoint
    }],
    ['variant proxy', 'openai', c => { c.model.variants = [{ id: 'custom', settings: { baseURL: 'https://proxy.invalid' } }] }],
    ['variant key', 'openai', c => { c.model.variants = [{ id: 'custom', settings: { apiKey: 'variant-secret' } }] }],
    ['variant auth header', 'openai', c => { c.model.variants = [{ id: 'custom', headers: { authorization: 'variant-secret' } }] }],
    ['different account header', 'openai', c => { c.model.headers = { 'ChatGPT-Account-Id': 'other-account' } }],
  ]
  for (const [name, id, change] of cases) await t.test(name, async () => {
    const c = context(id); change(c)
    const source = await quotaSource(c.ctx as any, c.provider, c.model, async () => { throw new Error('must not fetch') })
    assert.equal(source?.status, 'unsupported')
    assert.equal(source?.fetch, undefined)
  })
})

test('connection changes during source selection never grant the old account transport', async () => {
  const c = context()
  c.ctx.integration.connection.resolve = async () => {
    c.state.connection = { ...c.state.connection, id: 'credTwo' }
    return c.state.credential
  }
  const source = await quotaSource(c.ctx as any, c.provider, c.model)
  assert.equal(source?.status, 'unavailable')
  assert.equal(source?.fetch, undefined)
})

test('connection labels and status are not account identity', async t => {
  const w = world(t), c = context()
  const source = (await quotaSource(c.ctx as any, c.provider, c.model, w.fetch))!
  c.state.connection = { ...c.state.connection, label: 'Renamed account', status: { type: 'warning' } }
  w.observe(source)
  await settle()
  assert.equal(w.calls.length, 1)
  assert.equal(w.report().status, 'ok')
})

test('same-ID credential invalidation during resolution aborts before the provider request', async t => {
  const w = world(t), c = context()
  const source = (await quotaSource(c.ctx as any, c.provider, c.model, w.fetch))!
  let resolve = (_: any) => {}
  c.ctx.integration.connection.resolve = () => new Promise(done => { resolve = done })
  w.observe(source)
  await settle()
  w.collector.clear('root')
  c.state.credential.access = 'replacement-secret'
  resolve(c.state.credential)
  await settle()
  assert.equal(w.calls.length, 0)
  assert.deepEqual(w.collector.snapshot('root'), [])
})

test('a switch while quota HTTP is in flight cancels the old account response', async t => {
  const w = world(t), c = context()
  let release = (_: Response) => {}, cancelled = false
  w.response(() => new Promise(resolve => { release = resolve }))
  w.observe((await quotaSource(c.ctx as any, c.provider, c.model, w.fetch))!)
  await settle()
  c.state.connection = { ...c.state.connection, id: 'credTwo' }
  release(new Response(new ReadableStream({ cancel: () => { cancelled = true } })))
  await settle()
  assert.equal(cancelled, true)
  assert.equal(w.report().status, 'unavailable')
  assert.deepEqual(w.report().windows, [])
})

test('resolved capabilities cannot send credentials to another endpoint or follow redirects', async () => {
  const c = context()
  const calls: RequestInit[] = []
  const source = (await quotaSource(c.ctx as any, c.provider, c.model, async (_, init) => {
    calls.push(init)
    return Response.json(codex)
  }))!
  await assert.rejects(source.fetch!('https://proxy.invalid', { method: 'GET' }), /Unsupported quota request/)
  await assert.rejects(source.fetch!('https://chatgpt.com/backend-api/wham/usage', { method: 'POST' }), /Unsupported quota request/)
  assert.equal(calls.length, 0)
  await source.fetch!('https://chatgpt.com/backend-api/wham/usage', { method: 'GET', redirect: 'follow', headers: { cookie: 'caller-secret' } })
  assert.equal(calls[0]!.redirect, 'error')
  assert.equal(new Headers(calls[0]!.headers).get('cookie'), null)
})

test('quota reports and identities never serialize credentials, headers, account metadata or raw responses', async t => {
  for (const [id, body] of [['openai', codex], ['github-copilot', copilot], ['opencode-go', go]] as const) {
    await t.test(id, async t => {
      const w = world(t), c = context(id)
      c.state.credential = id === 'opencode-go' ? {
        type: 'key', key: 'secret-key-sentinel', metadata: { private: 'secret-metadata-sentinel' },
      } : {
        type: 'oauth', methodID: 'oauth', access: 'secret-access-sentinel', refresh: 'secret-refresh-sentinel',
        expires: NOW + 100000, metadata: { accountID: 'secret-account-sentinel', private: 'secret-metadata-sentinel' },
      }
      c.provider.headers = { 'x-private-context': 'secret-header-sentinel' }
      w.response(async () => Response.json({
        ...body, access: 'secret-body-sentinel', credential: c.state.credential,
      }, { headers: { 'set-cookie': 'secret-cookie-sentinel' } }))
      const source = (await quotaSource(c.ctx as any, c.provider, c.model, w.fetch))!
      assert.doesNotMatch(JSON.stringify(source), /secret-[a-z]+-sentinel/)
      w.observe(source)
      await settle()
      assert.equal(w.report().status, 'ok')
      assert.deepEqual(Object.keys(w.report()).sort(), ['checkedAt', 'measuredAt', 'provider', 'status', 'windows'])
      const wire = () => JSON.stringify({ reports: w.collector.snapshot('root'), ...w.collector.metadata('root') })
      assert.doesNotMatch(wire(), /secret-[a-z]+-sentinel|authorization|cookie|credential|metadata/)
      w.response(async () => { throw new Error('secret-error-sentinel') })
      await w.tick()
      assert.equal(w.report().status, 'unavailable')
      assert.doesNotMatch(wire(), /secret-[a-z]+-sentinel|authorization|cookie|credential|metadata/)
    })
  }
})

test('missing active credentials report auth_required and unknown providers are omitted', async () => {
  const c = context()
  c.state.connection = undefined
  assert.equal((await quotaSource(c.ctx as any, c.provider, c.model))?.status, 'auth_required')
  c.provider.id = 'unknown'
  assert.equal(await quotaSource(c.ctx as any, c.provider, c.model), undefined)
})

test('credential change during resolve prevents credential use and account change during fetch fences reports', async t => {
  const w = world(t), c = context()
  const source = (await quotaSource(c.ctx as any, c.provider, c.model, w.fetch))!
  c.ctx.integration.connection.resolve = async () => {
    c.state.connection = { ...c.state.connection, id: 'different' }
    return c.state.credential
  }
  w.observe(source)
  await settle()
  assert.equal(w.calls.length, 0)
  assert.equal(w.report().status, 'unavailable')
  let release = (_: Response) => {}
  w.response(() => new Promise(resolve => { release = resolve }))
  w.collector.clear('root')
  w.observe(w.source())
  await w.tick()
  const active = w.calls.at(-1)!
  w.collector.clear('root')
  assert.equal(active.init.signal?.aborted, true)
  release(Response.json(codex))
  await settle()
  assert.deepEqual(w.collector.snapshot('root'), [])
})

test('schema validation never fabricates zero usage or exhausted windows', async t => {
  const cases: [Source['provider'], unknown, string, number][] = [
    ['openai-codex', {}, 'unavailable', 0],
    ['openai-codex', { rate_limit: {} }, 'unsupported', 0],
    ['openai-codex', { rate_limit: { primary_window: { ...codex.rate_limit.primary_window, used_percent: '0' } } }, 'unavailable', 0],
    ['openai-codex', { rate_limit: { primary_window: {}, secondary_window: codex.rate_limit.secondary_window } }, 'ok', 1],
    ['opencode-go', { usage: { ...go.usage, monthly: { percent: 0, status: 'ok' } } }, 'unavailable', 0],
    ['opencode-go', { usage: { ...go.usage, monthly: { ...go.usage.monthly, resetsAt: '2026-02-30T00:00:00Z' } } }, 'unavailable', 0],
    ['github-copilot', { quota_snapshots: { chat: { percent_remaining: 0, unlimited: false, entitlement: 0 } } }, 'unsupported', 0],
    ['github-copilot', { quota_snapshots: { chat: { percent_remaining: 0, unlimited: false } } }, 'unsupported', 0],
    ['github-copilot', { quota_snapshots: { chat: { percent_remaining: 0, unlimited: true, entitlement: 100 } } }, 'unsupported', 0],
    ['github-copilot', { quota_reset_date: RESET, quota_snapshots: { chat: { percent_remaining: 0, unlimited: false, entitlement: 100, quota_remaining: 101 } } }, 'unavailable', 0],
  ]
  for (const [provider, body, status, count] of cases) await t.test(`${provider} ${JSON.stringify(body)}`, async t => {
    const w = world(t)
    w.response(async () => Response.json(body))
    w.observe(w.source(provider))
    await settle()
    assert.equal(w.report().status, status)
    assert.equal(w.report().windows.length, count)
  })
})

test('Go transient failures retain prior measurement, auth failure clears it', async t => {
  const w = world(t)
  w.response(async () => Response.json(go))
  w.observe(w.source('opencode-go'))
  await settle()
  const first = w.report()
  w.response(async () => new Response(null, { status: 503 }))
  await w.tick()
  assert.equal(w.report().status, 'unavailable')
  assert.equal(w.report().measuredAt, first.measuredAt)
  assert.deepEqual(w.report().windows, first.windows)
  w.response(async () => new Response(null, { status: 401 }))
  await w.tick()
  assert.equal(w.report().status, 'auth_required')
  assert.deepEqual(w.report().windows, [])
})

test('Retry-After and exponential backoff bound traffic and report retry times', async t => {
  const w = world(t)
  w.response(async () => new Response(null, { status: 429, headers: { 'retry-after': '120' } }))
  w.observe()
  await settle()
  assert.equal(w.report().status, 'throttled')
  assert.equal(w.report().retryAt, NOW + 120000)
  await w.tick(119999)
  assert.equal(w.calls.length, 1)
  await w.tick(1)
  assert.equal(w.calls.length, 2)
  assert.equal(w.report().retryAt, NOW + 240000)
})

test('account replacement resets throttling while repeated observations preserve its traffic budget', async t => {
  const w = world(t)
  w.response(async () => new Response(null, { status: 429, headers: { 'retry-after': '86400' } }))
  w.observe(w.source('openai-codex', ['account-a']))
  await settle()
  const retry = w.report().retryAt
  w.observe(w.source('openai-codex', ['account-a']))
  await settle()
  assert.equal(w.calls.length, 1)
  await w.tick(1000)
  assert.equal(w.calls.length, 1)
  w.response(async () => Response.json(codex))
  w.observe(w.source('openai-codex', ['account-b']))
  await settle()
  assert.equal(w.calls.length, 2)
  assert.equal(w.report().status, 'ok')
  assert.equal(w.report().retryAt, undefined)
  assert.equal(retry, NOW + 86400000)
})

test('same-source revalidation restores its measurement without another quota request', async t => {
  const w = world(t)
  w.observe()
  await settle()
  const report = w.report()
  const sequence = w.collector.nextSequence('root')
  assert.deepEqual(w.report().windows, [])
  w.collector.observe('root', 'root', w.source(), false, sequence)
  await settle()
  assert.deepEqual(w.report(), report)
  assert.equal(w.calls.length, 1)
  await w.tick()
  assert.equal(w.calls.length, 2)
})

test('same-ID updates and Go key replacement discard old measurements and backoff', async t => {
  const w = world(t), c = context('opencode-go')
  c.state.connection = { type: 'env', name: 'OPENCODE_GO_API_KEY' }
  w.response(async () => Response.json(go))
  w.observe((await quotaSource(c.ctx as any, c.provider, c.model, w.fetch))!)
  await settle()
  w.response(async () => new Response(null, { status: 429, headers: { 'retry-after': '86400' } }))
  await w.tick()
  assert.equal(w.report().status, 'throttled')
  assert.equal(w.report().windows.length, 3)
  c.state.credential = { type: 'key', key: 'new-account-secret' }
  const source = (await quotaSource(c.ctx as any, c.provider, c.model, w.fetch))!
  let release = (_: Response) => {}
  w.response(() => new Promise(resolve => { release = resolve }))
  w.observe(source)
  await settle()
  assert.equal(w.calls.length, 3)
  assert.deepEqual(w.report().windows, [])
  assert.equal(w.report().measuredAt, undefined)
  assert.equal(w.report().retryAt, undefined)
  w.collector.clear('root')
  assert.equal(w.calls[2]!.init.signal?.aborted, true)
  w.response(async () => new Response(null, { status: 503 }))
  w.observe(source)
  release(new Response(null, { status: 429, headers: { 'retry-after': '86400' } }))
  await settle()
  assert.equal(w.calls.length, 4)
  assert.equal(w.report().retryAt, Date.now() + 60000)
  assert.deepEqual(w.report().windows, [])
})

test('late throttled completion cannot transfer scheduling state to the replacement account', async t => {
  const w = world(t)
  let release = (_: Response) => {}
  w.response(() => new Promise(resolve => { release = resolve }))
  w.observe(w.source('openai-codex', ['account-a']))
  await settle()
  w.observe(w.source('openai-codex', ['account-b']))
  await settle()
  assert.equal(w.calls.length, 1)
  w.response(async () => new Response(null, { status: 503 }))
  release(new Response(null, { status: 429, headers: { 'retry-after': '86400' } }))
  await settle()
  assert.equal(w.calls.length, 2)
  assert.equal(w.report().status, 'unavailable')
  assert.equal(w.report().retryAt, Date.now() + 60000)
  await w.tick(59999)
  assert.equal(w.calls.length, 2)
  await w.tick(1)
  assert.equal(w.calls.length, 3)
  assert.equal(w.report().retryAt, Date.now() + 120000)
})

test('transport timeout aborts but keeps concurrency lock until old request settles', async t => {
  const w = world(t)
  let release = (_: Response) => {}
  w.response(() => new Promise(resolve => { release = resolve }))
  w.observe()
  await settle()
  await w.tick(5000)
  assert.equal(w.calls[0]!.init.signal?.aborted, true)
  assert.equal(w.report().status, 'unavailable')
  await w.tick(300000)
  assert.equal(w.calls.length, 1)
  release(Response.json(codex))
  await settle()
  assert.equal(w.report().status, 'unavailable')
})

test('source generations fence late reads, removal clears slots and disposal cancels timers', async t => {
  const w = world(t)
  let release = (_: Response) => {}
  w.response(() => new Promise(resolve => { release = resolve }))
  w.observe()
  await settle()
  const sequence = w.collector.nextSequence('root')
  w.collector.observe('root', 'root', { provider: 'openai-codex', identity: ['new'], status: 'auth_required' }, false, sequence)
  assert.equal(w.calls[0]!.init.signal?.aborted, true)
  release(Response.json(codex))
  await settle()
  assert.equal(w.report().status, 'auth_required')
  w.collector.remove('root')
  assert.deepEqual(w.collector.snapshot('root'), [])
  w.collector.dispose()
  await w.tick(900000)
  assert.equal(w.calls.length, 1)
})

test('deferred managed observations activate only after root opens, snapshots are copied', async t => {
  const w = world(t)
  w.closed.add('root')
  w.collector.observe('root', 'root', w.source(), true)
  await settle()
  assert.equal(w.calls.length, 0)
  w.closed.delete('root')
  w.collector.activate('root')
  await settle()
  const copy = w.collector.snapshot('root')!
  copy[0]!.windows[0]!.percentUsed = 99
  assert.equal(w.report().windows[0]!.percentUsed, 25)
  assert.ok(w.collector.metadata('root')!.providerLimitsRevision > 0)
})

test('redirects, oversized bodies and malformed JSON are unavailable', async t => {
  for (const response of [
    () => new Response(null, { status: 302 }),
    () => new Response('x'.repeat(65537)),
    () => new Response('not json'),
  ]) await t.test('invalid response', async t => {
    const w = world(t)
    w.response(async () => response())
    w.observe()
    await settle()
    assert.equal(w.report().status, 'unavailable')
    assert.deepEqual(w.report().windows, [])
  })
})
