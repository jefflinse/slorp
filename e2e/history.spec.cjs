const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createServer } = require('node:http')
const { join } = require('node:path')
const { test } = require('node:test')
const { chromium } = require('playwright')

async function world(t) {
  const at = new Date().toISOString()
  const session = { id: 'ses_history', agent: 'opencode', kind: 'interactive', label: 'Cache repair', cwd: '/work', status: 'idle', startedAt: at, activeAt: at, lastPrompt: '', lastReply: '', used: {}, canReply: true, timeline: [] }
  const data = { sessions: [session], items: [], summarizer: { model: 'fixture' }, capabilities: { opencode: { history: true }, claude: { history: true } } }
  let calls = 0, failure = '', release = () => {}
  let onRead = () => {}
  const reading = new Promise(resolve => { onRead = resolve })
  const server = createServer(async (req, res) => {
    if (req.url === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write(': connected\n\n')
    } else if (req.url === '/api/sessions') {
      res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(data))
    } else if (req.url === '/api/sessions/ses_history/backfill' && req.method === 'POST') {
      calls++
      await new Promise(resolve => { release = resolve; onRead() })
      if (failure) { res.writeHead(502).end(failure); return }
      session.timeline = [{ at, prompt: 'Earlier question' }]
      session.backfilling = 1
      res.writeHead(202, { 'content-type': 'application/json' }).end(JSON.stringify({ exchanges: 1, queued: 1 }))
    } else {
      res.setHeader('content-type', 'text/html'); res.end(readFileSync(join(__dirname, '..', 'index.html')))
    }
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(() => { server.closeAllConnections(); server.close() })
  const browser = await chromium.launch({ headless: true })
  t.after(() => browser.close())
  const page = await browser.newPage()
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  await page.goto(`http://127.0.0.1:${server.address().port}/#sessions`)
  const button = () => page.getByRole('button', { name: 'Fill In Earlier History', exact: true })
  await button().first().waitFor()
  return { page, session, data, button, reading, calls: () => calls, release: () => release(), fail: why => { failure = why } }
}

test('history controls follow declared capability for either harness', async t => {
  const o = await world(t)
  assert.ok(await o.button().count())
  o.data.capabilities.opencode.history = false
  await o.page.evaluate(() => refresh(true))
  assert.equal(await o.button().count(), 0)
  o.session.agent = 'claude'
  await o.page.evaluate(() => refresh(true))
  assert.ok(await o.button().count())
  assert.equal(o.calls(), 0)
})

test('history reading has a loading state and transitions to model backfill', async t => {
  const o = await world(t)
  await o.button().first().click()
  await o.page.locator('span.none:visible').filter({ hasText: 'Reading earlier history…' }).first().waitFor()
  assert.equal(await o.button().count(), 0)
  await o.reading
  assert.equal(o.calls(), 1)
  o.release()
  await o.page.locator('span.none:visible').filter({ hasText: /Filling in .*earlier exchange/ }).first().waitFor()
  assert.ok((await o.page.locator('.tl:visible').first().innerText()).includes('Earlier question'))
})

test('export failures explain the problem and restore the action', async t => {
  const o = await world(t)
  o.fail('OpenCode export failed: session not found')
  await o.button().first().click()
  await o.reading
  o.release()
  await o.page.waitForFunction(() => document.querySelector('#toast').textContent.includes('session not found'))
  await o.button().first().waitFor()
  assert.equal(o.session.timeline.length, 0)
})

test('without a local model the history action explains why it is disabled', async t => {
  const o = await world(t)
  o.data.summarizer = { off: true }
  await o.page.evaluate(() => refresh(true))
  assert.equal(await o.button().first().isDisabled(), true)
  assert.match(await o.button().first().getAttribute('title'), /No local model/)
  assert.equal(o.calls(), 0)
})
