import assert from 'node:assert/strict'
import { after, before, test, type TestContext } from 'node:test'
import plugin from './tui.ts'
import { parsePresence } from './presence.ts'
import { Native, maxRelayInFlight } from './native.ts'

// Each test chooses its own native endpoint: one the caller set is put back after.
const inheritedEndpoint = process.env.SLORP_OPENCODE_URL
before(() => { delete process.env.SLORP_OPENCODE_URL })
after(() => { if (inheritedEndpoint === undefined) delete process.env.SLORP_OPENCODE_URL; else process.env.SLORP_OPENCODE_URL = inheritedEndpoint })

const settle = async () => { for (let i = 0; i < 10; i++) await new Promise(resolve => setImmediate(resolve)) }
const metadata = (id = 'sesFirst') => ({ id, location: { directory: '/server/project' }, projectID: 'project' })

async function terminal(t: TestContext, configure?: (api: any) => void) {
  t.mock.timers.enable({ apis: ['setInterval', 'setTimeout'] })
  const sent: { input: any; options: any }[] = []
  const gets: { input: any; options: any }[] = []
  const results: any[] = []
  const listeners: Record<string, (event: any) => void> = {}
  const api = {
    route: { type: 'session', sessionID: 'sesFirst' },
    directory: '/client/project',
    ui: { router: { current: (): any => api.route } },
    data: { location: { default: () => ({ directory: api.directory }) }, session: { get: (_: string): any => metadata() } },
    publish: async (input: any, options: any): Promise<any> => { sent.push({ input, options }); return null },
    client: {
      rpc: (definition: any) => {
        assert.equal(definition.id, 'slorp')
        return { presence: api.publish, result: async (result: any) => { results.push(result); return null },
          events: { on: (type: string, callback: any) => { listeners[type] = callback; return () => { delete listeners[type] } } } }
      },
      session: { get: async (input: any, options: any): Promise<any> => { gets.push({ input, options }); return metadata() } },
    },
  }
  configure?.(api)
  const dispose = await plugin.setup(api as any)
  t.after(() => dispose?.())
  await settle()
  return { api, sent, gets, results, listeners, dispose: () => dispose?.(), tick: async (ms = 2000) => { t.mock.timers.tick(ms); await settle() },
    payload: () => parsePresence(sent.at(-1)?.input.command)! }
}

test('local and attach presence uses authenticated client RPC and server-owned location', async t => {
  for (const directory of ['/local/project', '/remote/project']) await t.test(directory, async t => {
    const o = await terminal(t, api => { api.data.session.get = () => ({ ...metadata(), location: { directory } }) })
    assert.equal(o.sent.length, 1)
    assert.deepEqual(o.sent[0]!.options.location, { directory })
    assert.equal(o.payload().directory, directory)
    assert.equal(o.payload().sessionId, 'sesFirst')
    assert.equal(o.payload().projectID, 'project')
    o.dispose()
    await o.tick()
    assert.equal(o.sent.length, 1)
  })
})

test('session switches and home reuse one activation ID and increasing sequences', async t => {
  const o = await terminal(t), first = o.payload()
  o.api.route.sessionID = 'sesSecond'
  o.api.data.session.get = () => metadata('sesSecond')
  await o.tick()
  assert.equal(o.payload().sessionId, 'sesSecond')
  assert.equal(o.payload().sequence, 2)
  assert.equal(o.payload().terminalId, first.terminalId)
  o.api.route.type = 'home'
  await o.tick()
  assert.equal(o.payload().sessionId, undefined)
  assert.equal(o.payload().directory, '/client/project')
})

test('cache misses use V2 session.get and its returned remote location', async t => {
  const o = await terminal(t, api => { api.data.session.get = () => undefined })
  assert.deepEqual(o.gets[0]!.input, { sessionID: 'sesFirst' })
  assert.equal(o.payload().directory, '/server/project')
  assert.equal(o.gets[0]!.options.signal, o.sent[0]!.options.signal)
})

test('missing or mismatched metadata never publishes', async t => {
  for (const data of [undefined, { ...metadata(), id: 'sesOther' }, { ...metadata(), location: { directory: '' } }, { ...metadata(), projectID: '' }]) {
    await t.test(JSON.stringify(data) ?? 'missing', async t => {
      const o = await terminal(t, api => { api.data.session.get = () => undefined; api.client.session.get = async () => data })
      assert.equal(o.sent.length, 0)
    })
  }
})

test('location mismatch in partial cached metadata is rejected', async t => {
  const o = await terminal(t, api => {
    api.data.session.get = () => ({ ...metadata(), projectID: undefined, location: { directory: '/other' } })
  })
  assert.equal(o.sent.length, 0)
})

test('home waits for meaningful location and ignores plugin routes', async t => {
  const o = await terminal(t, api => { api.route.type = 'home'; api.directory = '' })
  assert.equal(o.sent.length, 0)
  o.api.directory = '/server/project'
  await o.tick()
  assert.equal(o.sent.length, 1)
  o.api.route.type = 'plugin'
  await o.tick()
  assert.equal(o.sent.length, 1)
})

test('a switch during a lookup discards the stale sample', async t => {
  let release = (_: any) => {}
  const o = await terminal(t, api => {
    api.data.session.get = () => undefined
    api.client.session.get = () => new Promise(resolve => { release = resolve })
  })
  o.api.route.type = 'home'
  release(metadata())
  await settle()
  assert.equal(o.sent.length, 0)
  await o.tick()
  assert.equal(o.payload().sessionId, undefined)
  assert.equal(o.payload().sequence, 2)
})

test('one in-flight RPC, timeout cancellation, retry and late rejection fencing', async t => {
  let reject = (_: Error) => {}
  const calls: { input: any; signal: AbortSignal }[] = []
  const o = await terminal(t, api => {
    api.publish = (input: any, options: any) => {
      calls.push({ input, signal: options.signal })
      return calls.length === 1 ? new Promise((_, fail) => { reject = fail }) : Promise.resolve(null)
    }
  })
  await o.tick(4000)
  assert.equal(calls.length, 1)
  await o.tick(1000)
  assert.equal(calls[0]!.signal.aborted, true)
  await o.tick()
  assert.equal(calls.length, 2)
  assert.equal(parsePresence(calls[1]!.input.command)!.sequence, 2)
  reject(new Error('late transport failure'))
  await settle()
})

test('disposal aborts lookup and prevents late RPC or retry', async t => {
  let release = (_: any) => {}, signal: AbortSignal | undefined
  const o = await terminal(t, api => {
    api.data.session.get = () => undefined
    api.client.session.get = (_: any, options: any) => { signal = options.signal; return new Promise(resolve => { release = resolve }) }
  })
  o.dispose()
  assert.equal(signal?.aborted, true)
  release(metadata())
  await o.tick(12000)
  assert.equal(o.sent.length, 0)
})

test('TUI relay routes server requests through the attach client and cancels in-flight reads', async t => {
  let signal: AbortSignal | undefined
  let release = (_: any) => {}
  const o = await terminal(t, api => {
    api.client.message = { list: (_: any, options: any) => {
      signal = options.signal
      return new Promise(resolve => { release = resolve })
    } }
  })
  const data = { id: 'request1', terminalId: o.payload().terminalId, method: 'GET', path: '/api/session/sesFirst/message?limit=100&order=asc' }
  o.listeners.request!({ location: { directory: '/server/project' }, data: { ...data, terminalId: 'other' } })
  assert.equal(signal, undefined)
  o.listeners.request!({ location: { directory: '/server/project' }, data })
  assert.equal((signal as AbortSignal | undefined)?.aborted, false)
  o.listeners.cancel!({ data: { id: data.id, terminalId: data.terminalId } })
  assert.equal((signal as AbortSignal | undefined)?.aborted, true)
  release({ data: [], cursor: {} })
  await settle()
  assert.equal(o.results.length, 0)
  o.dispose()
  assert.equal(Object.keys(o.listeners).length, 0)
})

test('Native disposal cancels pending relayed form POST and DELETE and fences late completion', async t => {
  const calls: { signal: AbortSignal; release: () => void }[] = []
  const o = await terminal(t, api => {
    const pending = (_: unknown, options: { signal: AbortSignal }) =>
      new Promise<void>(resolve => { calls.push({ signal: options.signal, release: resolve }) })
    api.client.session.form = { reply: pending, cancel: pending }
  })
  const native = new Native({} as any, undefined, async () => undefined)
  native.advertise(o.payload().terminalId)
  const cancellations: any[] = []
  native.relay(async (type, data) => {
    if (type === 'cancel') cancellations.push(data)
    o.listeners[type]!({ data, location: { directory: '/server/project' } })
  })
  const signal = new AbortController().signal
  const answers = Promise.allSettled([
    native.form('sesFirst', 'form1', { value: 'yes' }, signal),
    native.form('sesFirst', 'form2', undefined, signal),
  ])
  await settle()
  assert.equal(calls.length, 2)
  native.dispose()
  assert.equal(cancellations.length, 2)
  assert.equal(new Set(cancellations.map(c => c.id)).size, 2)
  assert.ok(calls.every(c => c.signal.aborted))
  assert.ok((await answers).every(r => r.status === 'rejected' && /Disposed/.test(r.reason.message)))
  for (const call of calls) call.release()
  await settle()
  assert.equal(o.results.length, 0)
})

test('TUI relay bounds bursts and retains slots for abort-ignoring operations until settlement', async t => {
  const calls: { signal: AbortSignal; release: (value: any) => void }[] = []
  const o = await terminal(t, api => {
    api.client.message = { list: (_: unknown, options: { signal: AbortSignal }) =>
      new Promise(resolve => { calls.push({ signal: options.signal, release: resolve }) }) }
  })
  const request = (id: string) => o.listeners.request!({ location: { directory: '/server/project' },
    data: { id, terminalId: o.payload().terminalId, method: 'GET', path: `/api/session/ses${id}/message` } })
  for (let i = 0; i < 512; i++) request(String(i))
  await settle()
  assert.equal(calls.length, maxRelayInFlight)
  assert.equal(o.results.length, 512 - maxRelayInFlight)
  assert.ok(o.results.every(r => /busy.*retry/.test(r.error)))
  await o.tick(5000)
  assert.ok(calls.every(c => c.signal.aborted))
  request('stillFull')
  await settle()
  assert.equal(calls.length, maxRelayInFlight)
  calls[0]!.release({ data: [], cursor: {} })
  await settle()
  request('recovered')
  assert.equal(calls.length, maxRelayInFlight + 1)
  for (const call of calls.slice(1)) call.release({ data: [], cursor: {} })
  await settle()
  assert.equal(o.results.filter(r => !r.error).length, 1)
  assert.equal(o.results.at(-1).id, 'recovered')
})
