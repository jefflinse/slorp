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

async function world(t, hash, extra = {}) {
  const data = {
    sessions: [{ ...session('quiet', 'solo', 'Tidy the importer', 'idle', 8), lastReply: '**Done.** The importer is tidy.\n\n- Want the tests next?' }, session('asks', 'pair', 'Wire the outbox', 'waiting', 2), session('busy', 'pair', 'Benchmark the loader', 'working', 5),
      ...Array.from({ length: 8 }, (_, i) => session(`w${i}`, 'fleet', `Worker ${i + 1}`, 'idle', 10 + i))],
    efforts: [...['solo', 'pair', 'fleet', 'bare'].map(id => ({ id, name: id, about: '', dirs: [`/work/${id}`] })),
      { id: 'past', name: 'past', about: '', dirs: ['/work/past'], activeAt: ago(2880) }],
    items: [{ id: 'item', sessionId: 'asks', kind: 'question', title: 'Which retry policy?', ask: 'Pick one.', urgency: 'awaiting', at: ago(2), replies: [] }],
    summarizer: { model: 'fixture' }, capabilities: {}, ...extra,
  }
  const posts = []
  const server = createServer((req, res) => {
    if (req.url === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write(': connected\n\n')
    } else if (req.method === 'POST' || req.method === 'DELETE') {
      let body = ''
      req.on('data', chunk => { body += chunk }).on('end', () => { posts.push([req.method === 'POST' ? req.url : `${req.method} ${req.url}`, JSON.parse(body || '{}')]); res.setHeader('content-type', 'application/json'); res.end('{}') })
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

test('the top bar starts a session under no effort, presuming no directory', async t => {
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

test('a key opens the New Session dialog from anywhere, and only Escape or Cancel closes it', async t => {
  const page = await world(t, 'home')
  const dialog = page.getByRole('dialog', { name: 'New Session' })
  await page.locator('.home').waitFor()
  assert.equal(await page.locator('[data-form="start"]').count(), 0)
  await page.keyboard.press('n')
  await dialog.waitFor()
  assert.equal(await dialog.locator('[name=dir]').evaluate(el => el === document.activeElement), true)
  await page.mouse.click(8, 500)
  assert.equal(await dialog.count(), 1)
  await page.keyboard.press('Escape')
  assert.equal(await dialog.count(), 0)
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByRole('button', { name: 'New sessions' }).click()
  await page.getByLabel('Default directory').focus()
  await page.keyboard.press('Control+n')
  await dialog.waitFor()
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  assert.equal(await dialog.count(), 0)
  assert.equal(await page.getByRole('button', { name: 'New Session', exact: true }).count(), 1)
})

test('the New Session dialog opens with the defaults chosen in Settings, and starting a session leaves them be', async t => {
  const page = await world(t, 'settings')
  await page.getByRole('button', { name: 'New sessions' }).click()
  await page.getByLabel('Default directory').fill('/work/elsewhere')
  await page.getByLabel('Default directory').press('Tab')
  await page.getByLabel('Default effort', { exact: true }).selectOption('pair')
  await page.getByLabel('Default model').selectOption('opus')
  await page.reload()
  const dialog = page.getByRole('dialog', { name: 'New Session' }), field = name => dialog.locator(`[name="${name}"]`)
  const shown = async () => [await field('dir').inputValue(), await field('effortPick').inputValue(), await field('model').inputValue()]
  await page.getByRole('button', { name: 'New Session', exact: true }).click()
  assert.deepEqual(await shown(), ['/work/elsewhere', 'pair', 'opus'])
  assert.equal(await field('prompt').evaluate(el => el === document.activeElement), true)
  await field('prompt').fill('Add a dead-letter queue')
  await field('effortPick').selectOption('solo')
  assert.equal(await field('dir').inputValue(), '/work/solo')
  assert.equal(await field('prompt').inputValue(), 'Add a dead-letter queue')
  await dialog.getByRole('button', { name: 'Start session' }).click()
  await page.waitForFunction(() => !document.querySelector('.modal.start'))
  const [, body] = page.posts.find(([url]) => url === '/api/sessions')
  assert.deepEqual([body.prompt, body.dir, body.effort, body.model], ['Add a dead-letter queue', '/work/solo', 'solo', 'opus'])
  await page.keyboard.press('n')
  assert.deepEqual(await shown(), ['/work/elsewhere', 'pair', 'opus'])
  await page.keyboard.press('Escape')
  await page.goto(page.url().replace(/#.*/, '#efforts'))
  await page.locator('.ecard[data-id="fleet"] .eh').click()
  await page.locator('.ebig').getByRole('button', { name: 'New Session', exact: true }).click()
  assert.deepEqual(await shown(), ['/work/fleet', 'fleet', 'opus'])
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

test('the legacy Attention view is there only while Settings keeps it, and the logo leads Home', async t => {
  const page = await world(t, 'settings')
  const keep = page.getByRole('group', { name: 'Show legacy Attention view' }), tabs = () => page.locator('#nav button').allInnerTexts()
  await keep.waitFor()
  assert.equal(await keep.locator('button.on').innerText(), 'Off')
  assert.deepEqual((await tabs()).slice(0, 2), ['Home1', 'Efforts'])
  await keep.getByRole('button', { name: 'On' }).click()
  assert.deepEqual((await tabs()).slice(0, 3), ['Home1', 'Attention', 'Efforts'])
  await page.locator('#nav button', { hasText: 'Attention' }).click()
  await page.locator('.brief').waitFor()
  assert.equal(new URL(page.url()).hash, '#attention')
  assert.equal(await page.locator('.row.sel .t').innerText(), 'Which retry policy?')
  assert.match(await page.locator('.pane.detail h1').innerText(), /^Wire the outbox/)
  await page.locator('a.brand').click()
  await page.locator('.home').waitFor()
  assert.equal(new URL(page.url()).hash, '#home')
  await page.goto(page.url().replace('#home', '#settings'))
  await keep.getByRole('button', { name: 'Off' }).click()
  assert.deepEqual((await tabs()).slice(0, 2), ['Home1', 'Efforts'])
  await page.evaluate(() => { location.hash = 'attention' })
  await page.locator('.home').waitFor()
  assert.equal(await page.locator('.brief').count(), 0)
})

test('a session page puts the box to answer in after its last exchange', async t => {
  const page = await world(t, 'sessions')
  const inner = page.locator('.pane.detail .inner')
  await inner.locator('.need textarea').waitFor()
  const parts = await inner.evaluate(el => [...el.children].map(child => child.classList[0]))
  assert.ok(parts.indexOf('tabbody') >= 0 && parts.indexOf('tabbody') < parts.indexOf('need'), parts.join(' '))
  const head = await inner.locator('.crumb').allInnerTexts()
  assert.equal(head.length, 3)
  assert.match(head[0], /pair ›\s*Wire the outbox$/)
  assert.equal(head[1], 'Claude Code')
  assert.match(head[2], /^Opus 5\.5\s*50k \/ 200k \(25%\)\s*awaiting you\s*active 2m ago$/)
})

test('an insight Claude Code offers in passing is set apart from the rest of what it wrote', async t => {
  const page = await world(t, 'sessions')
  await page.locator('.pane.detail .inner').waitFor()
  const rule = '─'.repeat(40), said = ['Done.', '`★ Insight ' + rule + '`', '- Rebasing keeps **history** linear.', '`' + rule + '`', 'Next?'].join('\n')
  const [html, gist] = await page.evaluate(text => [md(text), gist(text)], said)
  assert.equal(html, '<div class="md"><p>Done.</p><aside class="tip"><b>★ Insight</b><div class="md"><ul><li>Rebasing keeps <b>history</b> linear.</li></ul></div></aside><p>Next?</p></div>')
  assert.equal(gist, 'Done. Rebasing keeps history linear. Next?')
  assert.equal(await page.evaluate(text => md(text), '`★ Insight ' + rule + '`\nnever closed'), '<div class="md"><p><code>★ Insight ' + rule + '</code><br>never closed</p></div>')
})

test('the top bar counts tokens before cost and says when the local model is at work', async t => {
  const page = await world(t, 'sessions')
  const model = page.locator('#tools .model')
  await model.waitFor()
  assert.equal(await model.locator('.dot.ready').count(), 1)
  await page.evaluate(() => {
    Object.assign(live[0], { used: { in: 1000, out: 500, cacheWrite: 0, cacheRead: 0 }, costUsd: 1.5 })
    summarizer.doing = 'describe quiet'
    render()
  })
  assert.equal(await page.locator('#used').innerText(), '2k tokens · ≈$1.50')
  assert.equal(await model.innerText(), 'live · fixture · ✦describing')
  assert.equal(await model.locator('.dot.running').count(), 1)
  assert.equal(await model.getAttribute('title'), 'The local model is at work: describe quiet')
})

test('a large effort card shows the three most pressing things that need you until asked for them all', async t => {
  const items = ['w0', 'w1', 'w2', 'w3'].map((id, i) => ({ id: `ask-${id}`, sessionId: id, kind: 'question', title: `Question ${i + 1}`, ask: 'Pick one.', urgency: 'awaiting', at: ago(i + 1), replies: [] }))
  const page = await world(t, 'efforts', { items })
  await page.locator('.ecard[data-id="fleet"] .eh').click()
  const needs = page.locator('.ebig .sec').filter({ hasText: 'Needs you (4)' })
  await needs.waitFor()
  assert.deepEqual(await needs.locator('.row .t').allInnerTexts(), ['Question 1', 'Question 2', 'Question 3'])
  await needs.getByRole('button', { name: 'Show all four' }).click()
  assert.equal(await needs.locator('.row').count(), 4)
  await needs.getByRole('button', { name: 'Show fewer' }).click()
  assert.equal(await needs.locator('.row').count(), 3)
})

test('a session or a thing that needs you is dragged from one card on the board to another to file it there', async t => {
  const page = await world(t, 'efforts')
  const card = id => page.locator(`.board .ecard[data-id="${id}"]`), filed = () => page.posts.filter(([url]) => url.endsWith('/effort'))
  await card('solo').locator('.en').waitFor()
  await card('solo').locator('.en', { hasText: 'Tidy the importer' }).dragTo(card('pair'))
  await page.waitForFunction(() => document.querySelector('#toast.show'))
  assert.deepEqual(filed(), [['/api/sessions/quiet/effort', { effort: 'pair' }]])
  await card('pair').locator('.en', { hasText: 'Which retry policy?' }).dragTo(card('fleet'))
  await page.waitForFunction(() => document.querySelectorAll('.ecard.drop').length === 0)
  assert.deepEqual(filed()[1], ['/api/sessions/asks/effort', { effort: 'fleet' }])
  await card('pair').locator('.en', { hasText: 'Benchmark the loader' }).dragTo(card('pair').locator('.eh'))
  assert.equal(filed().length, 2)
})

const github = { integrations: [{ id: 'github', name: 'GitHub', enabled: true, available: true, lookups: [], watches: [] }] }

test('a pull request is dragged onto an effort to file it there on its own', async t => {
  const pull = { source: 'github', repo: 'acme/widgets', number: 7, title: 'Prune old snapshots', url: 'https://github.com/acme/widgets/pull/7', author: 'marta', mine: true, at: ago(30), needs: 'Waiting on review', effort: 'solo' }
  const page = await world(t, 'efforts', { ...github, pullRequests: [pull] })
  await page.locator('.ecard[data-id="solo"] .eh').click()
  await page.locator('.ebig').getByRole('tab', { name: /^PRs/ }).click()
  await page.locator('.ebig .tabbody .row', { hasText: 'Prune old snapshots' }).dragTo(page.locator('.estack .ecard[data-id="pair"]'))
  await page.waitForFunction(() => document.querySelector('#toast.show'))
  assert.deepEqual(page.posts.filter(([url]) => /effort$/.test(url)), [['/api/pulls/effort', { pull: 'acme/widgets#7', effort: 'pair' }]])
  assert.equal(await page.locator('#toast').innerText(), 'acme/widgets#7 filed under pair')
})

test("a pull request's page files it on its own, back with its repository, or files the whole repository", async t => {
  const pull = { source: 'github', repo: 'acme/widgets', number: 7, title: 'Prune old snapshots', url: 'https://github.com/acme/widgets/pull/7', author: 'marta', mine: true, at: ago(30), needs: 'Waiting on review', effort: 'solo', effortVia: 'you', points: [] }
  const page = await world(t, 'pulls', { ...github, pullRequests: [pull] })
  const menu = page.locator('#acts')
  await page.locator('.crumb', { hasText: 'filed by you' }).waitFor()
  const pick = async (part, to) => {
    await menu.locator('summary.icon').click()
    if (part) await menu.locator('summary', { hasText: part }).click()
    await menu.getByRole('button', { name: to, exact: true }).click()
  }
  await pick('Move to', 'pair')
  await pick('', 'File with acme/widgets again')
  await pick('File all of acme/widgets under', 'fleet')
  await page.waitForFunction(() => document.querySelector('#toast.show'))
  assert.deepEqual(page.posts.filter(([url]) => /effort$/.test(url)), [
    ['/api/pulls/effort', { pull: 'acme/widgets#7', effort: 'pair' }],
    ['DELETE /api/pulls/effort', { pull: 'acme/widgets#7' }],
    ['/api/repos/effort', { repo: 'acme/widgets', effort: 'fleet' }],
  ])
})
