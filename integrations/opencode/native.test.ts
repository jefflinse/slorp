import assert from 'node:assert/strict'
import { test } from 'node:test'
import { OpenCode } from '@opencode/client'
import { Native, maxRelayInFlight, relayRequest, transcript, type NativeRequest } from './native.ts'

test('explicit HTTP verifies the same PID before authenticated operations and disallows redirects', async t => {
  const calls: { url: string; init: RequestInit }[] = []
  const password = process.env.OPENCODE_PASSWORD
  process.env.OPENCODE_PASSWORD = 'private'
  t.after(() => { if (password === undefined) delete process.env.OPENCODE_PASSWORD; else process.env.OPENCODE_PASSWORD = password })
  t.mock.method(globalThis, 'fetch', async (url: string, init: RequestInit) => {
    calls.push({ url, init })
    return Response.json(url.endsWith('/api/info') ? { pid: process.pid } : { data: {} })
  })
  const native = new Native({} as any, 'http://127.0.0.1:4096')
  await native.active(new AbortController().signal)
  assert.deepEqual(calls.map(c => c.url), ['http://127.0.0.1:4096/api/info', 'http://127.0.0.1:4096/api/session/active'])
  assert.ok(calls.every(c => c.init.redirect === 'error'))
  assert.ok(calls.every(c => new Headers(c.init.headers).get('authorization') === `Basic ${Buffer.from('opencode:private').toString('base64')}`))
})

test('non-loopback, userinfo, non-HTTP, query and path endpoints are rejected before fetch', async t => {
  let calls = 0
  t.mock.method(globalThis, 'fetch', async () => { calls++; throw new Error('must not fetch') })
  for (const url of ['https://evil.invalid', 'http://user@localhost:4096', 'file:///tmp/server', 'http://localhost:4096/?token=secret', 'http://localhost:4096/path']) {
    await assert.rejects(new Native({} as any, url).active(new AbortController().signal), /loopback/)
  }
  assert.equal(calls, 0)
})

test('PID mismatch and rejected verification never issue a session or form operation', async t => {
  const calls: string[] = []
  t.mock.method(globalThis, 'fetch', async (url: string) => { calls.push(url); return Response.json({ pid: process.pid + 1 }) })
  await assert.rejects(new Native({} as any, 'http://localhost:4096').form('sesOne', 'formOne', { key: 'value' }, new AbortController().signal), /not this OpenCode/)
  assert.deepEqual(calls, ['http://localhost:4096/api/info'])
})

// managed gives the test a Slorp-managed run's environment, or with no run
// an ordinary shell's, whichever the tests themselves run in.
function managed(t: { after: (fn: () => void) => void }, run?: { file: string; run: string }) {
  const saved = { SLORP_MANAGED_FILE: process.env.SLORP_MANAGED_FILE, SLORP_MANAGED_RUN: process.env.SLORP_MANAGED_RUN }
  if (run) { process.env.SLORP_MANAGED_FILE = run.file; process.env.SLORP_MANAGED_RUN = run.run }
  else { delete process.env.SLORP_MANAGED_FILE; delete process.env.SLORP_MANAGED_RUN }
  t.after(() => {
    for (const [key, value] of Object.entries(saved)) {
      if (value === undefined) delete process.env[key]; else process.env[key] = value
    }
  })
}

test('a managed run never consults shared discovery', async t => {
  managed(t, { file: '/nonexistent/session.json', run: 'token' })
  const calls: string[] = []
  t.mock.method(globalThis, 'fetch', async (url: string) => { calls.push(url); return Response.json({ pid: process.pid }) })
  let discovered = 0
  const native = new Native({} as any, undefined, async () => {
    discovered++
    return { url: 'http://127.0.0.1:4096', auth: undefined }
  })
  await assert.rejects(native.active(new AbortController().signal), /terminal transport/)
  assert.equal(discovered, 0)
  assert.deepEqual(calls, [])
})

test('matching shared discovery uses registration auth rather than the process password', async t => {
  managed(t)
  const calls: { url: string; init: RequestInit }[] = []
  t.mock.method(globalThis, 'fetch', async (url: string, init: RequestInit) => {
    calls.push({ url, init })
    return Response.json(url.endsWith('/api/info') ? { pid: process.pid } : { data: {} })
  })
  let discovered = 0
  const native = new Native({} as any, undefined, async () => {
    discovered++
    return { url: 'http://127.0.0.1:4096', auth: { type: 'basic', username: 'opencode', password: 'registration-secret' } }
  })
  await native.active(new AbortController().signal)
  await native.active(new AbortController().signal)
  assert.equal(discovered, 1)
  assert.ok(calls.every(c => new Headers(c.init.headers).get('authorization') === `Basic ${Buffer.from('opencode:registration-secret').toString('base64')}`))
  assert.equal(calls.filter(c => c.url.endsWith('/api/info')).length, 1)
})

test('another registered service is never used; requests fall back to the terminal relay', async t => {
  managed(t)
  const calls: string[] = []
  t.mock.method(globalThis, 'fetch', async (url: string) => { calls.push(url); return Response.json({ pid: process.pid + 1 }) })
  const native = new Native({} as any, undefined, async () => ({ url: 'http://127.0.0.1:4096', auth: undefined }))
  native.advertise('terminal')
  native.relay(async (_type, data) => {
    native.result({ id: data.id, terminalId: 'terminal', value: { data: { sesOwn: { type: 'running' } } } })
  })
  assert.deepEqual(await native.active(new AbortController().signal), { sesOwn: { type: 'running' } })
  assert.deepEqual(calls, ['http://127.0.0.1:4096/api/info'])
})

test('history pagination consumes all pages, normalizes V2 content and rejects cursor loops', async t => {
  const cursors: (string | null)[] = []
  let repeat = false
  t.mock.method(globalThis, 'fetch', async (url: string) => {
    const at = new URL(url)
    if (at.pathname === '/api/info') return Response.json({ pid: process.pid })
    const cursor = at.searchParams.get('cursor')
    cursors.push(cursor)
    return Response.json({ data: [{ id: cursor ? 'msg2' : 'msg1', type: 'user', text: cursor ? 'second' : 'first', time: { created: cursor ? 2 : 1 } }],
      cursor: { next: cursor && !repeat ? null : 'next' } })
  })
  const native = new Native({} as any, 'http://localhost:4096')
  const messages = await native.messages('sesOne', new AbortController().signal)
  assert.deepEqual(cursors, [null, 'next'])
  assert.deepEqual(messages.map(m => m.parts[0]!.text), ['first', 'second'])
  repeat = true
  await assert.rejects(native.messages('sesOne', new AbortController().signal), /repeated message cursor/)
})

test('relay targets one leased terminal, rejects wrong terminal replies and cancels pending requests', async () => {
  const native = new Native({} as any, undefined, async () => undefined)
  const emitted: { type: string; data: any }[] = []
  native.relay(async (type, data) => { emitted.push({ type, data }) })
  await assert.rejects(native.active(new AbortController().signal), /waiting for terminal/)
  native.advertise('terminal1')
  const controller = new AbortController()
  const result = native.active(controller.signal)
  const request = emitted[0]!.data
  assert.equal(request.terminalId, 'terminal1')
  native.result({ id: request.id, terminalId: 'wrong', value: { data: { wrong: true } } })
  native.result({ id: request.id, terminalId: 'terminal1', value: { data: { sesOne: { type: 'running' } } } })
  assert.deepEqual(await result, { sesOne: { type: 'running' } })
  const pending = native.forms('sesOne', controller.signal)
  controller.abort()
  await assert.rejects(pending, /Aborted/)
  assert.equal(emitted.at(-1)!.type, 'cancel')
  native.dispose()
})

test('relay dispatch uses real 2.3.2 generated client HTTP and attach authentication', async () => {
  const requests: { url: string; init: RequestInit }[] = []
  const client = OpenCode.make({ baseUrl: 'https://attached.example', headers: { Authorization: 'Basic attached' },
    fetch: async (url: any, init: any) => {
      requests.push({ url: String(url), init })
      if (String(url).includes('/message?')) return Response.json({ data: [], cursor: {} })
      if (init?.method === 'POST' || init?.method === 'DELETE') return new Response(null, { status: 204 })
      return Response.json({ data: [] })
    } })
  const signal = new AbortController().signal
  const call = (path: string, method = 'GET', body?: object) => relayRequest(client, { id: 'request', terminalId: 'terminal', path, method, body }, signal)
  assert.deepEqual(await call('/api/session/sesOne/message?limit=100&order=asc'), { data: [], cursor: {} })
  await call('/api/session/sesOne/form/formOne/reply', 'POST', { answer: { env: 'prod' } })
  await call('/api/session/sesOne/form/formOne', 'DELETE')
  assert.ok(requests.every(r => new Headers(r.init.headers).get('authorization') === 'Basic attached'))
  assert.equal(requests[1]!.url, 'https://attached.example/api/session/sesOne/form/formOne/reply')
  assert.deepEqual(JSON.parse(String(requests[1]!.init.body)), { answer: { env: 'prod' } })
  await assert.rejects(call('/api/session/sesOne', 'DELETE'), /Unsupported/)
})

test('Native bounds cross-session relay bursts and recovers capacity after cancellation', async () => {
  const native = new Native({} as any, undefined, async () => undefined)
  native.advertise('terminal')
  const emitted: { type: string; data: any }[] = []
  native.relay(async (type, data) => { emitted.push({ type, data }) })
  const controller = new AbortController()
  let busy = 0
  const operations = Array.from({ length: 512 }, (_, i) => native.forms(`ses${i}`, controller.signal).catch(error => {
    if (/busy.*retry/.test(error.message)) busy++
    else assert.match(error.message, /Aborted/)
  }))
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(emitted.filter(e => e.type === 'request').length, maxRelayInFlight)
  assert.equal(busy, 512 - maxRelayInFlight)
  controller.abort()
  await Promise.all(operations)
  assert.equal(emitted.filter(e => e.type === 'cancel').length, maxRelayInFlight)
  const recovered = native.forms('sesRecovered', new AbortController().signal)
  const request = emitted.at(-1)!.data
  native.result({ id: request.id, terminalId: request.terminalId, value: { data: [] } })
  assert.deepEqual(await recovered, [])
  native.dispose()
})

test('direct and SDK-relayed form cancellation preserve feedback in the query without a body', async t => {
  const feedback = 'do not deploy: a&b? x=1 + 2 / #é\nplease'
  const requests: { url: string; init: RequestInit }[] = []
  const fetcher = async (url: any, init: any) => {
    if (String(url).endsWith('/api/info')) return Response.json({ pid: process.pid })
    requests.push({ url: String(url), init })
    return new Response(null, { status: 204 })
  }
  t.mock.method(globalThis, 'fetch', fetcher)
  const signal = new AbortController().signal
  await new Native({} as any, 'http://localhost:4096').form('sesOne', 'formOne', undefined, signal, feedback)
  const client = OpenCode.make({ baseUrl: 'https://attached.example', fetch: fetcher })
  const native = new Native({} as any, undefined, async () => undefined)
  native.advertise('terminal')
  native.relay(async (type, data) => {
    if (type !== 'request') return
    const value = await relayRequest(client, data as NativeRequest, signal)
    native.result({ id: data.id, terminalId: data.terminalId, value })
  })
  await native.form('sesOne', 'formOne', undefined, signal, feedback)
  assert.equal(requests.length, 2)
  for (const request of requests) {
    const url = new URL(request.url)
    assert.equal(url.pathname, '/api/session/sesOne/form/formOne')
    assert.equal(url.searchParams.get('message'), feedback)
    assert.equal(request.init.method, 'DELETE')
    assert.equal(request.init.body, undefined)
  }
  native.dispose()
})

test('V2 asynchronous subagent records preserve launch input and handed-back reports', () => {
  const launch = transcript({ id: 'msgTool', type: 'assistant', time: { created: 1 }, agent: 'build', model: { providerID: 'p', id: 'm' },
    content: [{ type: 'tool', id: 'call1', name: 'subagent', time: { created: 1, completed: 2 }, state: {
      status: 'completed', input: { agent: 'explore', description: 'Inspect', prompt: 'Inspect files' },
      content: [{ type: 'text', text: 'Working in the background' }], metadata: { sessionID: 'sesChild', status: 'running' },
    } }] })
  const part = launch.parts[0]!
  assert.equal(part.tool, 'task')
  assert.equal((part.state as any).metadata.sessionId, 'sesChild')
  assert.equal((part.state as any).input.subagent_type, 'explore')
  assert.equal((part.state as any).metadata.background, true)
  const report = transcript({ id: 'msgReport', type: 'synthetic', time: { created: 3 },
    text: '<subagent sessionID="sesChild" state="completed" description="Inspect">\nDone\n</subagent>',
    metadata: { source: 'subagent', childID: 'sesChild', state: 'completed' } })
  assert.deepEqual(report.parts[0]!.subagent, { id: 'sesChild', state: 'completed', text: 'Done' })
})
