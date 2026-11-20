import assert from 'node:assert/strict'
import { test, type TestContext } from 'node:test'
import { createOpencodeClient } from '@opencode-ai/sdk/v2'
import plugin from './tui.ts'
import { parsePresence } from './presence.ts'

const settle = async () => { for (let i = 0; i < 10; i++) await new Promise(resolve => setImmediate(resolve)) }
const metadata = (id = 'ses_first') => ({ id, directory: '/server/project', projectID: 'project', workspaceID: 'wrk_remote' })

async function terminal(t: TestContext, configure?: (api: any) => void) {
  t.mock.timers.enable({ apis: ['setInterval', 'setTimeout'] })
  const sent: { input: any; signal: AbortSignal }[] = []
  const gets: { input: any; signal: AbortSignal }[] = []
  let dispose = () => {}
  const api = {
    route: { current: { name: 'session', params: { sessionID: 'ses_first' } } },
    state: { ready: true, path: { directory: '/attach/client' }, session: { get: (_: string): any => metadata() } },
    lifecycle: { onDispose: (fn: () => void) => { dispose = fn } },
    client: {
      tui: { publish: async (input: any, { signal }: any): Promise<any> => { sent.push({ input, signal }); return { data: true } } },
      session: { get: async (input: any, { signal }: any): Promise<any> => { gets.push({ input, signal }); return { data: metadata() } } },
    },
  }
  configure?.(api)
  await plugin.tui(api as any, undefined, {} as any)
  t.after(() => dispose())
  await settle()
  return { api, sent, gets, dispose: () => dispose(), tick: async (ms = 2000) => { t.mock.timers.tick(ms); await settle() },
    payload: () => parsePresence(sent.at(-1)?.input.body.properties.command)! }
}

test('local and attach terminals publish inert markers using the supplied live SDK client and transport', async t => {
  for (const directory of ['/local/project', '/remote/project']) await t.test(directory, async t => {
    const requests: Request[] = []
    const o = await terminal(t, api => {
      api.state.session.get = () => ({ ...metadata(), directory })
      api.client = createOpencodeClient({ baseUrl: 'https://attached.example:4096', headers: { Authorization: 'Basic attach-credentials' },
        fetch: async (input, init) => { requests.push(input instanceof Request ? input : new Request(input, init)); return Response.json(true) } })
    })
    assert.equal(requests.length, 1)
    const request = requests[0]!
    assert.equal(request.method, 'POST')
    assert.equal(request.url, `https://attached.example:4096/tui/publish?directory=${encodeURIComponent(directory)}&workspace=wrk_remote`)
    assert.equal(request.headers.get('authorization'), 'Basic attach-credentials')
    const body = await request.json() as any
    assert.equal(body.type, 'tui.command.execute')
    assert.deepEqual(parsePresence(body.properties.command), {
      version: 1, terminalId: parsePresence(body.properties.command)!.terminalId, sequence: 1,
      directory, projectID: 'project', workspaceID: 'wrk_remote', sessionId: 'ses_first',
    })
    o.dispose()
    await o.tick()
    assert.equal(requests.length, 1)
  })
})

test('startup, switches, home and disposal use one activation ID and increasing sequences', async t => {
  const o = await terminal(t)
  const first = o.payload()
  o.api.route.current.params.sessionID = 'ses_second'
  o.api.state.session.get = () => metadata('ses_second')
  await o.tick()
  assert.equal(o.payload().sessionId, 'ses_second')
  assert.equal(o.payload().sequence, 2)
  assert.equal(o.payload().terminalId, first.terminalId)
  o.api.route.current.name = 'home'
  o.api.state.path.directory = '/server/project'
  await o.tick()
  assert.equal(o.payload().sessionId, undefined)
  assert.equal(o.payload().directory, '/server/project')
  o.dispose()
  await o.tick()
  assert.equal(o.sent.length, 3)
})

test('home waits for ready and meaningful server directory metadata', async t => {
  const o = await terminal(t, api => { api.route.current.name = 'home'; api.state.ready = false; api.state.path.directory = '' })
  assert.equal(o.sent.length, 0)
  o.api.state.ready = true
  await o.tick()
  assert.equal(o.sent.length, 0)
  o.api.state.path.directory = '/server/project'
  await o.tick()
  assert.equal(o.sent.length, 1)
  assert.equal(o.gets.length, 0)
})

test('cache misses use scoped session.get on the same client and prefer returned server metadata', async t => {
  const o = await terminal(t, api => { api.state.session.get = () => undefined })
  assert.deepEqual(o.gets[0]!.input, { sessionID: 'ses_first', directory: '/attach/client' })
  assert.equal(o.payload().directory, '/server/project')
  assert.equal(o.sent[0]!.input.workspace, 'wrk_remote')
  assert.equal(o.gets[0]!.signal, o.sent[0]!.signal)
})

test('the live SDK resolves cache misses with attach authentication before publishing to the returned scope', async t => {
  const requests: Request[] = []
  await terminal(t, api => {
    api.state.session.get = () => undefined
    api.client = createOpencodeClient({ baseUrl: 'https://attached.example:4096', headers: { Authorization: 'Basic attach-credentials' },
      fetch: async (input, init) => {
        const request = input instanceof Request ? input : new Request(input, init)
        requests.push(request)
        return Response.json(request.method === 'GET' ? metadata() : true)
      } })
  })
  assert.deepEqual(requests.map(r => [r.method, r.url]), [
    ['GET', 'https://attached.example:4096/session/ses_first?directory=%2Fattach%2Fclient'],
    ['POST', 'https://attached.example:4096/tui/publish?directory=%2Fserver%2Fproject&workspace=wrk_remote'],
  ])
  assert.ok(requests.every(r => r.headers.get('authorization') === 'Basic attach-credentials'))
})

test('partial cached metadata supplies explicit workspace routing and is checked against resolution', async t => {
  const o = await terminal(t, api => { api.state.session.get = () => ({ ...metadata(), projectID: undefined }) })
  assert.deepEqual(o.gets[0]!.input, { sessionID: 'ses_first', directory: '/server/project', workspace: 'wrk_remote' })
  assert.equal(o.sent.length, 1)
})

test('missing, mismatched, malformed and failed session metadata never publishes', async t => {
  for (const data of [undefined, { ...metadata(), id: 'ses_other' }, { ...metadata(), directory: '' }, { ...metadata(), projectID: '' }]) {
    await t.test(JSON.stringify(data) ?? 'missing', async t => {
      const o = await terminal(t, api => { api.state.session.get = () => undefined; api.client.session.get = async () => ({ data }) })
      assert.equal(o.sent.length, 0)
    })
  }
})

test('a switch while metadata is pending discards the old sample and retries the latest on the next tick', async t => {
  let release = (_: any) => {}
  const o = await terminal(t, api => {
    api.state.session.get = () => undefined
    api.client.session.get = () => new Promise(resolve => { release = resolve })
  })
  o.api.route.current.name = 'home'
  release({ data: metadata() })
  await settle()
  assert.equal(o.sent.length, 0)
  await o.tick()
  assert.equal(o.payload().sessionId, undefined)
  assert.equal(o.payload().sequence, 2)
})

test('only one publish is in flight, with latest state sampled after it completes', async t => {
  let release = () => {}
  const calls: any[] = []
  const o = await terminal(t, api => { api.client.tui.publish = async (input: any) => {
    calls.push(input); await new Promise<void>(resolve => { release = resolve }); return { data: true }
  } })
  o.api.route.current.name = 'home'
  await o.tick(4000)
  assert.equal(calls.length, 1)
  release()
  await settle()
  await o.tick()
  assert.equal(calls.length, 2)
  assert.equal(parsePresence(calls[1].body.properties.command)!.sessionId, undefined)
  release()
})

test('timeouts abort requests, retry with a new sequence and fence late lookup continuations', async t => {
  let release = (_: any) => {}
  let signal: AbortSignal | undefined
  const o = await terminal(t, api => {
    api.state.session.get = () => undefined
    api.client.session.get = (_: any, options: any) => { signal = options.signal; return new Promise(resolve => { release = resolve }) }
  })
  await o.tick(5000)
  assert.equal(signal?.aborted, true)
  o.api.state.session.get = () => metadata()
  await o.tick()
  assert.equal(o.payload().sequence, 2)
  release({ data: metadata() })
  await settle()
  assert.equal(o.sent.length, 1)
})

test('publish rejection and SDK error responses retry only on the next tick', async t => {
  const sequences: number[] = []
  const o = await terminal(t, api => { api.client.tui.publish = async (input: any) => {
    sequences.push(parsePresence(input.body.properties.command)!.sequence)
    if (sequences.length === 1) throw new Error('offline')
    return sequences.length === 2 ? { error: { message: 'unavailable' } } : { data: true }
  } })
  assert.deepEqual(sequences, [1])
  await o.tick()
  await o.tick()
  assert.deepEqual(sequences, [1, 2, 3])
})

test('disposal aborts an in-flight lookup and prevents late publication', async t => {
  let release = (_: any) => {}
  let signal: AbortSignal | undefined
  const o = await terminal(t, api => {
    api.state.session.get = () => undefined
    api.client.session.get = (_: any, options: any) => { signal = options.signal; return new Promise(resolve => { release = resolve }) }
  })
  o.dispose()
  assert.equal(signal?.aborted, true)
  release({ data: metadata() })
  await settle()
  await o.tick(12000)
  assert.equal(o.sent.length, 0)
})

test('publish timeouts and disposal abort the send without late rejection or stale retries', async t => {
  for (const reason of ['timeout', 'dispose']) await t.test(reason, async t => {
    const calls: { signal: AbortSignal; sequence: number }[] = []
    let reject = (_: Error) => {}
    const o = await terminal(t, api => { api.client.tui.publish = (input: any, { signal }: any) => {
      calls.push({ signal, sequence: parsePresence(input.body.properties.command)!.sequence })
      return calls.length === 1 ? new Promise((_, fail) => { reject = fail }) : Promise.resolve({ data: true })
    } })
    if (reason === 'timeout') await o.tick(5000)
    else o.dispose()
    assert.equal(calls[0]!.signal.aborted, true)
    await settle()
    await o.tick()
    assert.deepEqual(calls.map(c => c.sequence), reason === 'timeout' ? [1, 2] : [1])
    reject(new Error('late transport rejection'))
    await settle()
  })
})

test('SDK lookup errors retry on ticks and fresh terminal activations use independent IDs', async t => {
  const ids: string[] = []
  for (let i = 0; i < 2; i++) await t.test(`activation ${i}`, async t => {
    let attempts = 0
    const o = await terminal(t, api => {
      api.state.session.get = () => undefined
      api.client.session.get = async () => ++attempts === 1 ? { error: 'offline' } : { data: metadata() }
    })
    assert.equal(attempts, 1)
    assert.equal(o.sent.length, 0)
    await o.tick()
    assert.equal(o.payload().sequence, 2)
    ids.push(o.payload().terminalId)
  })
  assert.notEqual(ids[0], ids[1])
})
