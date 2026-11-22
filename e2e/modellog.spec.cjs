const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createServer } = require('node:http')
const { join } = require('node:path')
const { test } = require('node:test')
const { chromium } = require('playwright')

const ago = mins => new Date(Date.now() - mins * 60000).toISOString()
const session = (id, label) => ({ id, agent: 'claude', kind: 'interactive', label, cwd: '/w/widgets', status: 'idle', effort: 'widgets',
  startedAt: ago(90), activeAt: ago(5), lastPrompt: '', lastReply: '', used: {}, canReply: true, timeline: [{ at: ago(5), prompt: 'Carry on' }] })
const act = (n, kind, more = {}) => ({ id: `a${n}`, kind, at: ago(n), millis: 2100, model: 'lab/fixture-model', ...more })

/* What the local model has done, the latest first: the service's state carries the first five. */
const log = () => [
  act(1, 'describe', { outcome: 'Wants a decision on the retry policy', sessions: [{ id: 'open', name: 'Retry the upload' }], efforts: [{ id: 'widgets', name: 'Widgets' }],
    pulls: [{ key: 'acme/widgets#7', title: 'Retry uploads', url: 'https://github.com/acme/widgets/pull/7' }], workspaces: [{ id: 'w1', name: 'Widgets checkout' }], lookups: ['GitHub PR #7'] }),
  act(2, 'file', { outcome: 'Filed it under Widgets', sessions: [{ id: 'open', name: 'Retry the upload' }], efforts: [{ id: 'widgets', name: 'Widgets' }] }),
  act(3, 'describe', { error: 'model server returned 500 Internal Server Error', tries: 3, sessions: [{ id: 'gone', name: 'A session since closed' }] }),
  act(4, 'wrapup', { day: '2026-11-20', outcome: 'The upload retry landed.', efforts: [{ id: 'widgets', name: 'Widgets' }, { id: 'deleted', name: 'Since deleted' }],
    sessions: ['One', 'Two', 'Three', 'Four'].map(n => ({ id: `s${n}`, name: `Session ${n}` })) }),
  act(5, 'note', { outcome: 'Waiting on your retry decision', efforts: [{ id: 'widgets', name: 'Widgets' }] }),
  act(6, 'brief', { outcome: 'Yours: answer the question', pulls: [{ key: 'acme/widgets#7', title: 'Retry uploads', url: 'https://github.com/acme/widgets/pull/7' }] }),
  act(7, 'role', { outcome: 'Guessed no role', sessions: [{ id: 'open', name: 'Retry the upload' }] }),
]

async function world(t, all = log()) {
  const ready = { installed: true, enabled: true }
  const data = { sessions: [session('open', 'Retry the upload')], items: [], efforts: [{ id: 'widgets', name: 'Widgets', about: '', dirs: ['/w/widgets'] }],
    knowledge: { questions: [], context: [], routing: [], roles: [], workspaces: [{ id: 'w1', name: 'Widgets checkout', dir: '/w/widgets' }], profiles: [] },
    journal: [{ day: '2026-11-20', state: 'ready', at: ago(4), headline: 'The upload retry landed.' }],
    plugins: { claude: ready, opencode: ready }, summarizer: { model: 'lab/fixture-model' }, capabilities: {}, actions: all.slice(0, 5) }
  const reads = []
  const server = createServer((req, res) => {
    if (req.url === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write(': connected\n\n')
    } else if (req.url === '/api/sessions') {
      res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(data))
    } else if (req.url === '/api/actions') {
      reads.push(req.url)
      res.setHeader('content-type', 'application/json'); res.end(JSON.stringify({ actions: all }))
    } else {
      res.setHeader('content-type', 'text/html'); res.end(readFileSync(join(__dirname, '..', 'index.html')))
    }
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(() => { server.closeAllConnections(); server.close() })
  const browser = await chromium.launch({ headless: true })
  t.after(() => browser.close())
  /* The model's name leaves the top bar where there is no room for it. */
  const page = await browser.newPage({ viewport: { width: 1500, height: 900 } })
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  await page.goto(`http://127.0.0.1:${server.address().port}/`)
  await page.waitForFunction(() => typeof live !== 'undefined' && live !== null)
  const did = act => { all.unshift(act); data.actions = all.slice(0, 5) }
  return { page, data, all, reads, did, menu: page.locator('#worked'), rows: page.locator('#main .srow.act') }
}

test('the local model\'s name opens onto the last five things it did and what each was about', async t => {
  const { page, menu } = await world(t)
  assert.equal(await menu.locator('.pop').isVisible(), false)
  await menu.locator('summary').click()
  const rows = menu.locator('.did')
  assert.equal(await rows.count(), 5)
  const first = await rows.nth(0).innerText()
  assert.match(first, /described/i)
  assert.match(first, /Wants a decision on the retry policy/)
  assert.match(first, /Widgets[\s\S]*#7[\s\S]*Retry the upload[\s\S]*in Widgets checkout/)
  assert.match(await rows.nth(2).innerText(), /Failed three times in a row: model server returned 500/)
  assert.match(await rows.nth(3).innerText(), /Session One[\s\S]*Session Two[\s\S]*and two more/)
  assert.doesNotMatch(await menu.innerText(), /Yours: answer the question/)

  await page.evaluate(() => refresh(true))
  await page.waitForFunction(() => reads > 1)
  assert.equal(await menu.locator('.pop').isVisible(), true, 'it stays open while the page is drawn again')
  await page.keyboard.press('Escape')
  assert.equal(await menu.locator('.pop').isVisible(), false)
  await menu.locator('summary').click()
  await page.locator('#main').click({ position: { x: 5, y: 5 } })
  assert.equal(await menu.locator('.pop').isVisible(), false)

  await menu.locator('summary').click()
  await rows.nth(0).getByRole('button', { name: 'Retry the upload' }).click()
  assert.equal(await page.evaluate(() => location.hash + ' ' + state.actSel), '#sessions open')
  assert.equal(await menu.locator('.pop').isVisible(), false)
})

test('the whole log lists everything, narrows by kind, leads to what each was about, and follows the model', async t => {
  const { page, menu, rows, reads, did } = await world(t)
  await menu.locator('summary').click()
  await menu.getByRole('button', { name: 'See everything it has done' }).click()
  await page.waitForFunction(() => document.querySelectorAll('#main .srow.act').length === 7)
  assert.equal(await page.evaluate(() => location.hash), '#log')
  assert.equal(reads.length, 1)
  assert.match(await rows.nth(0).innerText(), /looked up GitHub PR #7[\s\S]*fixture-model · 2\.1s/)
  assert.match(await rows.nth(6).innerText(), /Guessed no role/)

  const kinds = page.locator('#main .kbnav')
  assert.match(await kinds.innerText(), /Everything\s*7[\s\S]*Descriptions\s*2[\s\S]*Failed\s*1/)
  await kinds.getByRole('button', { name: /Failed/ }).click()
  assert.equal(await rows.count(), 1)
  assert.match(await rows.nth(0).innerText(), /Failed three times in a row: model server returned 500[\s\S]*A session since closed/)
  assert.equal(await rows.nth(0).getByRole('button', { name: 'A session since closed' }).count(), 0, 'a session that is gone is named and not linked')
  await kinds.getByRole('button', { name: /Everything/ }).click()

  const wrapup = rows.nth(3)
  assert.equal(await wrapup.getByRole('button', { name: /Since deleted/ }).count(), 0)
  assert.match(await rows.nth(5).locator('a.pr').getAttribute('href'), /acme\/widgets\/pull\/7$/)
  await wrapup.getByRole('button', { name: 'Friday 20 November' }).click()
  assert.equal(await page.evaluate(() => location.hash + ' ' + state.journalSel), '#journal 2026-11-20')
  await page.goBack()
  await rows.nth(0).getByRole('button', { name: 'Widgets checkout' }).click()
  assert.equal(await page.evaluate(() => location.hash + ' ' + state.kbSel), '#knowledge workspaces')
  await page.goBack()
  await rows.nth(4).getByRole('button', { name: /Widgets/ }).click()
  assert.equal(await page.evaluate(() => location.hash + ' ' + state.effortSel), '#efforts widgets')
  await page.goBack()

  did({ id: 'new', kind: 'note', at: new Date().toISOString(), millis: 800, model: 'lab/fixture-model', outcome: 'Retry policy decided', efforts: [{ id: 'widgets', name: 'Widgets' }] })
  await page.evaluate(() => refresh(true))
  await page.waitForFunction(() => document.querySelectorAll('#main .srow.act').length === 8)
  assert.equal(reads.length, 2)
  assert.match(await rows.nth(0).innerText(), /Retry policy decided[\s\S]*800ms/)
})

test('a long log is shown a hundred at a time', async t => {
  const many = Array.from({ length: 130 }, (_, i) => act(i + 1, 'describe', { outcome: `Title ${i + 1}` }))
  const { page, rows } = await world(t, many)
  await page.evaluate(() => { location.hash = '#log' })
  await page.waitForFunction(() => document.querySelectorAll('#main .srow.act').length === 100)
  await page.getByRole('button', { name: 'Show 30 more' }).click()
  assert.equal(await rows.count(), 130)
  assert.equal(await page.getByRole('button', { name: /Show .* more/ }).count(), 0)
})

test('before the model has done anything the top bar and the log say so, and Settings leads to the log', async t => {
  const { page, menu } = await world(t, [])
  await menu.locator('summary').click()
  assert.match(await menu.locator('.pop').innerText(), /Nothing yet/)
  assert.equal(await menu.getByRole('button').count(), 0)
  await page.keyboard.press('Escape')
  await page.locator('[data-act="view"][data-view="settings"]').click()
  await page.getByRole('button', { name: /Local model/ }).click()
  await page.getByRole('button', { name: 'See the log' }).click()
  assert.equal(await page.evaluate(() => location.hash), '#log')
  assert.match(await page.locator('#main .detail').innerText(), /What the local model has done[\s\S]*Nothing yet/)
})
