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
  /* The session's record, oldest first, and what the page asked of it: the service answers with what was said before the latest. */
  const record = [], asks = []
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
    } else if (req.url === '/api/sessions/ses_history/exchanges' && req.method === 'POST') {
      let body = ''
      for await (const chunk of req) body += chunk
      const asked = JSON.parse(body)
      asks.push(asked)
      if (failure) { res.writeHead(502).end(failure); return }
      const before = record.slice(0, -1), at = asked.from ? before.findIndex(x => x.at >= asked.from) : -1
      const first = Math.max(0, (at < 0 ? before.length : at) - (asked.more || 0))
      res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ exchanges: before.slice(first).reverse(), head: record.at(-1)?.at, more: first > 0 }))
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
  return { page, session, data, button, reading, record, asks, calls: () => calls, release: () => release(), fail: why => { failure = why } }
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

test('earlier exchanges are read one at a time, as cards that open onto all that was said', async t => {
  const o = await world(t), page = o.page
  const ago = mins => new Date(Date.now() - mins * 60000).toISOString()
  const said = (mins, prompt, reply) => ({ at: ago(mins), prompt, reply })
  o.record.push(said(180, 'First question', 'First answer'), said(120, 'Second question', 'The **second** answer'), said(60, 'Latest question', 'Latest answer'))
  Object.assign(o.session, { lastPrompt: 'Latest question', lastReply: 'Latest answer', timeline: [{ at: ago(60), prompt: 'Latest question' }] })
  await page.evaluate(() => refresh(true))
  const cards = page.locator('.tabbody .xc'), more = page.getByRole('button', { name: 'Load More', exact: true })
  await more.waitFor()
  assert.equal(await cards.count(), 0)
  assert.deepEqual(o.asks, [])

  await more.click()
  await cards.first().waitFor()
  assert.deepEqual(o.asks, [{ more: 1 }])
  assert.match(await cards.first().innerText(), /2h ago\s+Second question/)
  assert.equal(await cards.first().locator('.xb').count(), 0)

  /* The model's line on an exchange reaches its card by way of the timeline. */
  o.session.timeline.unshift({ at: o.record[1].at, prompt: 'Second question', line: 'Chose the retry policy', summary: 'It settled on backing off.' })
  await page.evaluate(() => refresh(true))
  await cards.first().getByText('Chose the retry policy').waitFor()
  assert.match(await cards.first().innerText(), /It settled on backing off\./)

  await cards.first().locator('.xh').click()
  assert.match(await cards.first().locator('.xb').innerText(), /YOU SAID\s+Second question\s+The second answer/i)
  assert.equal(await cards.first().locator('.xb b').innerText(), 'second')

  await more.click()
  await cards.nth(1).waitFor()
  assert.deepEqual(o.asks[1], { from: o.record[1].at, more: 1 })
  assert.match(await cards.nth(1).innerText(), /3h ago\s+First question/)
  assert.equal(await cards.first().locator('.xb').count(), 1, 'an opened exchange stays open as more are read')
  assert.equal(await more.count(), 0)
  await page.locator('.xmore').getByText('This is as far back as Slorp reads.').waitFor()

  /* Once the user speaks again, what was the latest exchange joins the cards, without being asked for. */
  o.record.push(said(0, 'Newest question', ''))
  Object.assign(o.session, { lastPrompt: 'Newest question' })
  o.session.timeline.push({ at: o.record[3].at, prompt: 'Newest question' })
  await page.evaluate(() => refresh(true))
  await cards.nth(2).waitFor()
  assert.deepEqual(o.asks[2], { from: o.record[0].at, more: 0 })
  assert.deepEqual((await cards.allInnerTexts()).map(text => text.split('\n').filter(Boolean)[1]), ['Latest question', '✦Chose the retry policy', 'First question'])
  await page.evaluate(() => refresh(true))
  assert.equal(o.asks.length, 3)
})

test('a record that cannot be read says why and leaves the exchange to be asked for again', async t => {
  const o = await world(t), page = o.page, more = page.getByRole('button', { name: 'Load More', exact: true })
  o.record.push({ at: new Date(Date.now() - 3600000).toISOString(), prompt: 'Only question', reply: 'Only answer' })
  o.fail('OpenCode export failed: session not found')
  await more.click()
  assert.match(await page.locator('.xmore [role="alert"]').innerText(), /could not be read: OpenCode export failed: session not found/)
  o.fail('')
  await more.click()
  await page.locator('.xmore').getByText('Nothing was said before this.').waitFor()
  o.data.capabilities.opencode.history = false
  await page.evaluate(() => refresh(true))
  assert.equal(await page.locator('.earlier').count(), 0)
})

test('without a local model the history action explains why it is disabled', async t => {
  const o = await world(t)
  o.data.summarizer = { off: true }
  await o.page.evaluate(() => refresh(true))
  assert.equal(await o.button().first().isDisabled(), true)
  assert.match(await o.button().first().getAttribute('title'), /No local model/)
  assert.equal(o.calls(), 0)
})

test('a session at work shows what was just said to it, each thing it has said since, and that more is coming; an idle one folds all but its answer', async t => {
  const o = await world(t), page = o.page
  const body = page.locator('.tabbody'), outs = body.locator('.out'), working = body.locator('.atwork')
  const show = async more => { Object.assign(o.session, more); await page.evaluate(() => refresh(true)) }
  const tools = name => page.locator('.tabs .tools').getByRole('button', { name, exact: true })
  const fold = body.getByRole('button', { name: /^Earlier in This Response/ })

  await show({ lastPrompt: 'Fix the cache', lastReply: 'The cache is fixed.', said: ['Reading the cache code.', 'The cache is fixed.'] })
  await fold.waitFor()
  assert.match(await fold.innerText(), /1$/)
  assert.equal(await fold.getAttribute('aria-expanded'), 'false')
  assert.deepEqual(await outs.allInnerTexts(), ['The cache is fixed.'])
  await fold.click()
  await outs.nth(1).waitFor()
  assert.deepEqual(await outs.allInnerTexts(), ['Reading the cache code.', 'The cache is fixed.'])
  assert.equal(await fold.getAttribute('aria-expanded'), 'true')
  await fold.click()
  await page.waitForFunction(() => document.querySelectorAll('.tabbody .out').length === 1)
  assert.equal(await working.count(), 0)
  assert.equal(await tools('Copy').count(), 1)

  /* What it said to the prompt before is not shown under the new one. */
  await show({ status: 'working', lastPrompt: 'Now add a test', said: undefined })
  await working.waitFor()
  assert.match(await body.locator('.said').innerText(), /Now add a test/)
  assert.equal(await outs.count(), 0)
  assert.equal(await fold.count(), 0)
  assert.equal(await body.getByText('No response found').count(), 0)
  assert.match(await working.innerText(), /^Working$/)
  assert.equal(await tools('Copy').count(), 0)
  assert.equal(await tools('Send To…').count(), 0)

  await show({ lastReply: 'Writing the test.', said: ['Looking at the existing tests.', 'Writing the **test**.'], saidEarlier: 2,
    subagents: [{ id: 'a1', type: 'Explore', description: 'Find the tests', status: 'running' }] })
  await outs.nth(1).waitFor()
  assert.deepEqual(await outs.allInnerTexts(), ['Looking at the existing tests.', 'Writing the test.'])
  assert.equal(await outs.nth(1).locator('b').innerText(), 'test')
  await body.getByText('This response began with two earlier messages that are not kept here.').waitFor()
  assert.match(await working.innerText(), /Working\s+one subagent running/)
  assert.ok(await page.evaluate(() => document.querySelector('.atwork').compareDocumentPosition([...document.querySelectorAll('.tabbody .out')].at(-1)) & Node.DOCUMENT_POSITION_PRECEDING))

  await show({ status: 'waiting' })
  await body.getByText('It is waiting for your answer below.').waitFor()
  assert.equal(await working.locator('.wave').count(), 0)

  await show({ status: 'idle', lastReply: 'The test is in.', said: ['Looking at the existing tests.', 'Writing the test.', 'The test is in.'], subagents: [] })
  await fold.waitFor()
  assert.match(await fold.innerText(), /2$/)
  assert.deepEqual(await outs.allInnerTexts(), ['The test is in.'])
  assert.equal(await body.getByText('not kept here').count(), 0)
  await fold.click()
  await body.getByText('This response began with two earlier messages that are not kept here.').waitFor()
  assert.equal(await outs.count(), 3)
  assert.equal(await working.count(), 0)
  assert.equal(await tools('Copy').count(), 1)

  /* A session whose agent tells only of the last thing it said shows that, once it has stopped. */
  await show({ said: undefined })
  await page.waitForFunction(() => document.querySelectorAll('.tabbody .out').length === 1)
  assert.deepEqual(await outs.allInnerTexts(), ['The test is in.'])
})
