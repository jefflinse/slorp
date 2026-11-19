const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createServer } = require('node:http')
const { join } = require('node:path')
const { test } = require('node:test')
const { chromium } = require('playwright')

const ago = mins => new Date(Date.now() - mins * 60000).toISOString()
const session = (id, effort, label, status, mins) => ({ id, agent: 'claude', kind: 'interactive', label, cwd: `/work/${effort || 'scratch'}`, status, effort,
  startedAt: ago(mins + 60), activeAt: ago(mins), lastPrompt: '', lastReply: '', used: {}, canReply: true, timeline: [{ at: ago(mins), prompt: 'Carry on' }],
  model: 'claude-opus-5-5', contextTokens: 50000, contextWindow: 200000 })

async function world(t, hash) {
  const data = {
    sessions: [session('quiet', 'solo', 'Tidy the importer', 'idle', 8), session('asks', 'pair', 'Wire the outbox', 'waiting', 2), session('busy', 'pair', 'Benchmark the loader', 'working', 5),
      ...Array.from({ length: 8 }, (_, i) => session(`w${i}`, 'fleet', `Worker ${i + 1}`, 'idle', 10 + i))],
    efforts: ['solo', 'pair', 'fleet'].map(id => ({ id, name: id, about: '', dirs: [`/work/${id}`] })),
    items: [{ id: 'item', sessionId: 'asks', kind: 'question', title: 'Which retry policy?', ask: 'Pick one.', urgency: 'awaiting', at: ago(2), replies: [] }],
    summarizer: { model: 'fixture' }, capabilities: {},
  }
  const server = createServer((req, res) => {
    if (req.url === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write(': connected\n\n')
    } else if (req.url === '/api/sessions') {
      res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(data))
    } else {
      res.setHeader('content-type', 'text/html'); res.end(readFileSync(join(__dirname, '..', 'index.html')))
    }
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(() => { server.closeAllConnections(); server.close() })
  const browser = await chromium.launch({ headless: true })
  t.after(() => browser.close())
  const page = await browser.newPage({ viewport: { width: 1500, height: 900 } })
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  await page.goto(`http://127.0.0.1:${server.address().port}/#${hash}`)
  return page
}

test('an effort card lists what needs you and then every other open session', async t => {
  const page = await world(t, 'efforts')
  const rows = id => page.locator(`.ecard[data-id="${id}"] .en .nt`).allInnerTexts()
  await page.locator('.ecard[data-id="solo"] .en').waitFor()
  assert.deepEqual(await rows('solo'), ['Tidy the importer'])
  assert.deepEqual(await rows('pair'), ['Which retry policy?', 'Benchmark the loader'])
  assert.deepEqual(await rows('fleet'), ['Worker 1', 'Worker 2', 'Worker 3', 'Worker 4', 'Worker 5', 'Worker 6'])
  assert.equal(await page.locator('.ecard[data-id="fleet"] .more').innerText(), 'and two more')
  await page.locator('.ecard[data-id="solo"] .en').click()
  assert.match(await page.locator('.pane.detail h1').innerText(), /^Tidy the importer/)
})

test('the pane beside a session leads with its vitals and keeps Details folded until asked', async t => {
  const page = await world(t, 'sessions')
  const side = page.locator('.pane.side')
  await side.locator('.tl').waitFor()
  assert.deepEqual(await side.locator('.facts.vitals dt').allInnerTexts(), ['Context', 'Model', 'Last active'])
  assert.ok((await side.innerText()).indexOf('Last active') < (await side.innerText()).toUpperCase().indexOf("WHAT'S GOING ON"))
  const details = side.getByRole('button', { name: 'Details' })
  assert.equal(await details.getAttribute('aria-expanded'), 'false')
  assert.equal(await side.locator('dt', { hasText: 'Directory' }).count(), 0)
  await details.click()
  assert.equal(await details.getAttribute('aria-expanded'), 'true')
  const facts = await side.locator('.facts:not(.vitals) dt').allInnerTexts()
  assert.ok(facts.includes('Directory'))
  for (const moved of ['Context', 'Model', 'Last active']) assert.ok(!facts.includes(moved))
})
