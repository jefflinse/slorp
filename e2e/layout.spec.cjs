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
    sessions: [{ ...session('quiet', 'solo', 'Tidy the importer', 'idle', 8), lastReply: '**Done.** The importer is tidy.\n\n- Want the tests next?' }, session('asks', 'pair', 'Wire the outbox', 'waiting', 2), session('busy', 'pair', 'Benchmark the loader', 'working', 5),
      ...Array.from({ length: 8 }, (_, i) => session(`w${i}`, 'fleet', `Worker ${i + 1}`, 'idle', 10 + i))],
    efforts: [...['solo', 'pair', 'fleet', 'bare'].map(id => ({ id, name: id, about: '', dirs: [`/work/${id}`] })),
      { id: 'past', name: 'past', about: '', dirs: ['/work/past'], activeAt: ago(2880) }],
    items: [{ id: 'item', sessionId: 'asks', kind: 'question', title: 'Which retry policy?', ask: 'Pick one.', urgency: 'awaiting', at: ago(2), replies: [] }],
    summarizer: { model: 'fixture' }, capabilities: {},
  }
  const posts = []
  const server = createServer((req, res) => {
    if (req.url === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write(': connected\n\n')
    } else if (req.method === 'POST') {
      let body = ''
      req.on('data', chunk => { body += chunk }).on('end', () => { posts.push([req.url, JSON.parse(body || '{}')]); res.setHeader('content-type', 'application/json'); res.end('{}') })
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
  page.posts = posts
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

test('an effort with nothing open, even one with past work, is hidden until asked for, on the board and in the column', async t => {
  const page = await world(t, 'efforts')
  const cards = () => page.locator('.ecard[data-id]').evaluateAll(els => els.map(el => el.dataset.id))
  const toggle = page.getByLabel('Hide empty Efforts')
  await page.locator('.ecard[data-id="solo"]').waitFor()
  assert.deepEqual(await cards(), ['solo', 'pair', 'fleet'])
  assert.equal(await toggle.isChecked(), true)
  assert.match(await page.locator('.phead .opt').innerText(), /2 hidden/)
  await toggle.uncheck()
  assert.deepEqual(await cards(), ['solo', 'pair', 'fleet', 'bare', 'past'])
  await page.reload()
  await page.locator('.ecard[data-id="bare"]').waitFor()
  await page.locator('.ecard[data-id="solo"] .eh').click()
  await page.locator('.estack .ecard[data-id="bare"]').waitFor()
  await page.locator('.estack').getByLabel('Hide empty Efforts').check()
  assert.deepEqual(await cards(), ['solo', 'pair', 'fleet', 'unsorted'])
  await page.locator('.estack').getByLabel('Hide empty Efforts').uncheck()
  await page.locator('.estack .ecard[data-id="bare"]').click()
  await page.locator('.estack').getByLabel('Hide empty Efforts').check()
  assert.deepEqual(await cards(), ['solo', 'pair', 'fleet', 'unsorted', 'bare'])
})

test('the pane beside a session leads with its vitals and keeps Details folded until asked', async t => {
  const page = await world(t, 'sessions')
  const side = page.locator('.pane.side')
  await side.locator('.tl').waitFor()
  assert.deepEqual(await side.locator('.facts.vitals dt').allInnerTexts(), ['Context', 'Model', 'Last active'])
  assert.ok((await side.innerText()).indexOf('Last active') < (await side.innerText()).toUpperCase().indexOf('RECAP'))
  const details = side.getByRole('button', { name: 'Details' })
  assert.equal(await details.getAttribute('aria-expanded'), 'false')
  assert.equal(await side.locator('dt', { hasText: 'Directory' }).count(), 0)
  await details.click()
  assert.equal(await details.getAttribute('aria-expanded'), 'true')
  const facts = await side.locator('.facts:not(.vitals) dt').allInnerTexts()
  assert.ok(facts.includes('Directory'))
  for (const moved of ['Context', 'Model', 'Last active']) assert.ok(!facts.includes(moved))
})

test('Sessions starts a session under no effort, presuming no directory', async t => {
  const page = await world(t, 'sessions')
  await page.getByRole('button', { name: 'New Session', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'New Session' })
  assert.equal(await dialog.locator('form').getAttribute('data-id'), 'unsorted')
  assert.equal(await dialog.locator('input[name=dir]').inputValue(), '')
  assert.ok((await dialog.locator('#start-dirs option').evaluateAll(all => all.map(o => o.value))).includes('/work/solo'))
})

test('a theme is chosen in Settings and kept', async t => {
  const page = await world(t, 'settings')
  const look = () => page.evaluate(() => document.documentElement.dataset.look)
  await page.getByRole('button', { name: 'Hot Dog Stand' }).click()
  assert.equal(await look(), 'hotdog')
  assert.equal(await page.getByRole('group', { name: 'Appearance' }).count(), 0)
  await page.reload()
  await page.getByRole('button', { name: 'The Matrix' }).click()
  assert.equal(await look(), 'matrix')
  await page.getByRole('button', { name: 'Slorp', exact: true }).click()
  assert.equal(await look(), 'slorp')
  assert.equal(await page.getByRole('group', { name: 'Appearance' }).count(), 1)
})

test('Home opens the most pressing thing that needs you with its answer, and leads to its session', async t => {
  const page = await world(t, 'attention')
  const open = page.locator('.qi.open')
  await open.waitFor()
  assert.equal(new URL(page.url()).hash, '#home')
  assert.equal(await page.locator('#nav button.on').innerText(), 'Home1')
  assert.equal(await open.locator('.qh .t').innerText(), 'Which retry policy?')
  assert.equal(await open.locator('.ask').innerText(), 'Pick one.')
  assert.equal(await open.getByPlaceholder('Reply to this session…').count(), 1)
  await open.getByRole('button', { name: 'Open session ›' }).click()
  assert.equal(new URL(page.url()).hash, '#sessions')
  assert.match(await page.locator('.pane.detail h1').innerText(), /^Wire the outbox/)
  assert.deepEqual(await page.locator('.pane.list .ghead .name').allInnerTexts(), ['Needs you', 'Active', 'Closed'].map(name => `▼\n${name}`))
  assert.equal(await page.locator('.pane.list .ghead + .row .t').first().innerText(), 'Wire the outbox')
})

test('Home says what moved today, by effort and the latest first', async t => {
  const page = await world(t, 'home')
  const groups = page.locator('.home aside .tg')
  await groups.first().waitFor()
  assert.deepEqual(await groups.locator('.eff').allInnerTexts(), ['S\nsolo', 'P\npair', 'F\nfleet'])
  assert.deepEqual(await groups.nth(1).locator('.th button').allInnerTexts(), ['Wire the outbox', 'Benchmark the loader'])
  const pair = groups.nth(1).locator('.tr'), solo = groups.nth(0).locator('.tr')
  assert.match(await pair.nth(1).locator('.th .w').innerText(), /^working · /)
  assert.equal(await pair.nth(1).locator('.lead').count(), 0)
  assert.equal(await pair.nth(1).locator('.last').innerText(), 'Carry on')
  assert.match(await solo.locator('.th .w').innerText(), /^idle · /)
  assert.equal(await solo.locator('.lead').innerText(), 'Done. The importer is tidy. Want the tests next?')
  assert.equal(await solo.locator('.tm').innerText(), '25%')
  await groups.nth(1).locator('.th button').nth(1).click()
  assert.match(await page.locator('.pane.detail h1').innerText(), /^Benchmark the loader/)
})

test('Home starts a session from its prompt box, in the directory of the effort chosen', async t => {
  const page = await world(t, 'home')
  const box = page.locator('form.compose'), start = box.getByRole('button', { name: 'Start' })
  await box.waitFor()
  assert.equal(await box.getByRole('button', { name: 'Claude Code' }).getAttribute('aria-pressed'), 'true')
  assert.equal(await start.isDisabled(), true)
  await box.getByLabel('Prompt for a new session').fill('Add a dead-letter queue')
  await box.getByLabel('Effort to file it under').selectOption('pair')
  assert.equal(await box.getByLabel('Directory').inputValue(), '/work/pair')
  await start.click()
  await page.waitForFunction(() => document.querySelector('form.compose textarea')?.value === '')
  const [url, body] = page.posts.find(([url]) => url === '/api/sessions')
  assert.equal(url, '/api/sessions')
  assert.deepEqual([body.prompt, body.dir, body.effort], ['Add a dead-letter queue', '/work/pair', 'pair'])
})

test('Home lists a stopped session as your turn, with what it last said, a box to answer in and a way to set it aside', async t => {
  const page = await world(t, 'home')
  const turn = page.locator('.qi').filter({ hasText: 'Tidy the importer' })
  await turn.waitFor()
  assert.deepEqual(await page.locator('.home .sec > .label > span:first-child').allInnerTexts(), ['NEEDS YOU (1)', 'YOUR TURN (1)', 'TODAY'])
  assert.equal(await page.locator('.qi').count(), 2)
  assert.equal(await turn.locator('.st').innerText(), 'Done. The importer is tidy. Want the tests next?')
  await page.keyboard.press('j')
  assert.equal(await turn.locator('.qsaid b').innerText(), 'Done.')
  await turn.getByPlaceholder('Say something to this session…').fill('Yes, the tests')
  await turn.getByRole('button', { name: 'Send' }).click()
  await page.waitForFunction(() => document.querySelector('.qi.open textarea')?.value === '')
  assert.deepEqual(page.posts.find(([url]) => url === '/api/sessions/quiet/reply'), ['/api/sessions/quiet/reply', { text: 'Yes, the tests' }])
  await turn.getByRole('button', { name: 'Not now' }).click()
  await turn.waitFor({ state: 'detached' })
  assert.equal(await page.locator('.qi.open .qh .t').innerText(), 'Which retry policy?')
  await page.getByRole('button', { name: 'Undo' }).click()
  await turn.locator('.qsaid').waitFor()
})
