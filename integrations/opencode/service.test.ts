import assert from 'node:assert/strict'
import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { createServer } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { discover, headers } from './service.js'

test('bundled discovery is read-only and validates the registered server', async t => {
  const root = await mkdtemp(join(tmpdir(), 'slorp-service-'))
  t.after(() => rm(root, { recursive: true, force: true }))
  const file = join(root, 'service.json')
  let requests = 0
  const server = createServer((request, response) => {
    requests++
    if (request.headers.authorization !== `Basic ${Buffer.from('opencode:isolated-test').toString('base64')}`) {
      response.writeHead(401, { 'content-type': 'application/json' }).end('{}')
      return
    }
    assert.equal(request.url, '/api/info')
    response.writeHead(200, { 'content-type': 'application/json' })
      .end(JSON.stringify({ version: '2.3.2', pid: process.pid }))
  })
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(() => { server.closeAllConnections(); server.close() })
  const address = server.address()
  assert.ok(address && typeof address === 'object')
  const registration = { url: `http://127.0.0.1:${address.port}`, password: 'isolated-test', pid: process.pid, version: '2.3.2' }
  assert.equal(await discover({ file, version: '2.3.2' }), undefined)
  assert.equal(requests, 0)
  await writeFile(file, JSON.stringify(registration))
  const endpoint = await discover({ file, version: '2.3.2' })
  assert.equal(endpoint?.url, registration.url)
  assert.deepEqual(headers(endpoint!), { authorization: `Basic ${Buffer.from('opencode:isolated-test').toString('base64')}` })
  assert.equal(await discover({ file, version: '2.4.0' }), undefined)
  for (const changed of [{ pid: process.pid + 1 }, { password: 'wrong' }, { version: '2.4.0' }]) {
    await writeFile(file, JSON.stringify({ ...registration, ...changed }))
    assert.equal(await discover({ file, version: '2.3.2' }), undefined)
  }
})
