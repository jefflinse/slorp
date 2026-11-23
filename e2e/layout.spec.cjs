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

async function world(t, hash, extra = {}, replies = {}) {
  const data = {
    sessions: [{ ...session('quiet', 'solo', 'Tidy the importer', 'idle', 8), lastReply: '**Done.** The importer is tidy.\n\n- Want the tests next?' }, session('asks', 'pair', 'Wire the outbox', 'waiting', 2), session('busy', 'pair', 'Benchmark the loader', 'working', 5),
      ...Array.from({ length: 8 }, (_, i) => session(`w${i}`, 'fleet', `Worker ${i + 1}`, 'idle', 10 + i))],
    efforts: [...['solo', 'pair', 'fleet', 'bare'].map(id => ({ id, name: id, about: '', dirs: [`/work/${id}`] })),
      { id: 'past', name: 'past', about: '', dirs: ['/work/past'], activeAt: ago(2880) }],
    items: [{ id: 'item', sessionId: 'asks', kind: 'question', title: 'Which retry policy?', ask: 'Pick one.', urgency: 'awaiting', at: ago(2), replies: [] }],
    summarizer: { model: 'fixture' }, capabilities: {}, ...extra,
  }
  const posts = [], reads = []
  /* Profiles are kept as the service keeps them, so that a change made in Settings is there to be read back. */
  const profiled = (method, id, sent) => {
    const list = (data.knowledge ||= { questions: [], context: [], routing: [], roles: [], workspaces: [] }).profiles ||= []
    const at = list.findIndex(p => p.id === id), kept = { ...sent, id: id || `p${list.length + 1}` }
    if (method === 'DELETE') { list.splice(at, 1); return {} }
    if (kept.default) list.forEach(p => { p.default = false })
    if (id) list[at] = { ...list[at], ...kept }; else list.push(kept)
    return id ? {} : { id: kept.id }
  }
  const server = createServer((req, res) => {
    if (req.url === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write(': connected\n\n')
    } else if (req.method === 'POST' || req.method === 'DELETE') {
      let body = ''
      req.on('data', chunk => { body += chunk }).on('end', () => {
        const sent = JSON.parse(body || '{}'), profile = req.url.match(/^\/api\/profiles(?:\/(.+))?$/)
        posts.push([req.method === 'POST' ? req.url : `${req.method} ${req.url}`, sent])
        res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(profile ? profiled(req.method, profile[1], sent) : replies[req.url] || {}))
      })
    } else if (req.url === '/api/sessions') {
      res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(data))
    } else if (replies[req.url.split('?')[0]]) {
      reads.push(decodeURIComponent(req.url))
      res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(replies[req.url.split('?')[0]]))
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
  page.reads = reads
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
  const toggle = page.getByLabel('Hide Empty Efforts')
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
  await page.locator('.estack').getByLabel('Hide Empty Efforts').check()
  assert.deepEqual(await cards(), ['solo', 'pair', 'fleet', 'unsorted'])
  await page.locator('.estack').getByLabel('Hide Empty Efforts').uncheck()
  await page.locator('.estack .ecard[data-id="bare"]').click()
  await page.locator('.estack').getByLabel('Hide Empty Efforts').check()
  assert.deepEqual(await cards(), ['solo', 'pair', 'fleet', 'unsorted', 'bare'])
})

test('the pane beside a session leads with its vitals and keeps Details folded until asked', async t => {
  const page = await world(t, 'sessions')
  const side = page.locator('.pane.side')
  await side.locator('.tl').waitFor()
  assert.deepEqual(await side.locator('.facts.vitals dt').allInnerTexts(), ['Context', 'Model', 'Last Active'])
  assert.ok((await side.innerText()).indexOf('Last Active') < (await side.innerText()).toUpperCase().indexOf('RECAP'))
  const details = side.getByRole('button', { name: 'Details' })
  assert.equal(await details.getAttribute('aria-expanded'), 'false')
  assert.equal(await side.locator('dt', { hasText: 'Directory' }).count(), 0)
  await details.click()
  assert.equal(await details.getAttribute('aria-expanded'), 'true')
  const facts = await side.locator('.facts:not(.vitals) dt').allInnerTexts()
  assert.ok(facts.includes('Directory'))
  for (const moved of ['Context', 'Model', 'Last Active']) assert.ok(!facts.includes(moved))
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
  await open.getByRole('button', { name: 'Open Session ›' }).click()
  assert.equal(new URL(page.url()).hash, '#sessions')
  assert.match(await page.locator('.pane.detail h1').innerText(), /^Wire the outbox/)
  assert.deepEqual(await page.locator('.pane.list .ghead .name').allInnerTexts(), ['Needs You', 'Active', 'Closed'].map(name => `▼\n${name}`))
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
  await page.getByRole('button', { name: 'Session Profiles' }).click()
  await page.locator('form[data-form="profiles"]').getByLabel('Name').focus()
  await page.keyboard.press('Control+n')
  await dialog.waitFor()
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  assert.equal(await dialog.count(), 0)
  assert.equal(await page.getByRole('button', { name: 'New Session', exact: true }).count(), 1)
})

/* What a profile in the fixture leaves unsaid. */
const profile = (id, name, sets = {}) => ({ id, name, default: false, engine: 'claude', dir: '', effort: '', role: '', terminal: false, model: '', effortLevel: '', thinking: true, temperature: null, ...sets })
const kept = profiles => ({ knowledge: { questions: [], context: [], routing: [], roles: [], workspaces: [], profiles } })

test('a profile is made and edited in Settings, with only one of them the default', async t => {
  const page = await world(t, 'settings')
  await page.getByRole('button', { name: /^Session Profiles/ }).click()
  const cards = page.locator('.card.profile'), form = page.locator('form[data-form="profiles"]')
  const changes = () => page.posts.filter(([url]) => url.startsWith('/api/profiles/')).length
  assert.match(await page.locator('.pane.detail .none').innerText(), /^No profiles yet/)
  await form.getByLabel('Name').fill('  Reviews ')
  await form.getByRole('button', { name: 'Add Profile' }).click()
  await page.getByLabel('Profile name').waitFor()
  assert.deepEqual(page.posts, [['/api/profiles', { engine: 'claude', thinking: true, name: 'Reviews' }]])
  assert.equal(await form.getByLabel('Name').inputValue(), '')
  assert.equal(await cards.locator('.why').innerText(), 'Claude Code')

  await page.getByLabel('Profile directory').fill('/work/elsewhere')
  await page.getByLabel('Profile directory').press('Tab')
  await page.getByLabel('Profile effort', { exact: true }).selectOption('pair')
  await page.getByLabel('Profile model').selectOption('opus')
  await page.getByLabel('Profile effort level').selectOption('high')
  await page.getByRole('group', { name: 'Default profile' }).getByRole('button', { name: 'On' }).click()
  await cards.locator('.tag', { hasText: 'Default' }).waitFor()
  assert.equal(changes(), 5)
  assert.deepEqual(page.posts.slice(1), [['/api/profiles/p1', { dir: '/work/elsewhere' }], ['/api/profiles/p1', { effort: 'pair' }], ['/api/profiles/p1', { model: 'opus' }], ['/api/profiles/p1', { effortLevel: 'high' }], ['/api/profiles/p1', { default: true }]])
  assert.equal(await cards.locator('.why').innerText(), 'Claude Code · Opus · high effort')
  assert.deepEqual(await cards.locator('.meta span:not(.mono)').allInnerTexts(), ['/work/elsewhere', 'filed under pair'])

  await page.getByRole('group', { name: 'What a session runs on' }).getByRole('button', { name: 'fixture' }).click()
  await page.getByLabel('Profile temperature').fill('0.4')
  await page.getByLabel('Profile temperature').press('Tab')
  await page.getByRole('group', { name: 'Thinking' }).getByRole('button', { name: 'Off' }).click()
  await page.waitForFunction(() => document.querySelector('.card.profile .why')?.textContent === 'fixture · no thinking · temperature 0.4')
  assert.equal(await page.getByLabel('Profile model').count(), 0)
  await page.getByLabel('Profile name').fill('   ')
  await page.getByLabel('Profile name').press('Tab')
  assert.equal(await page.getByLabel('Profile name').inputValue(), 'Reviews')
  assert.equal(changes(), 8)
  await page.getByLabel('Profile name').fill('Reviewing')
  await page.evaluate(() => refresh(true))
  assert.deepEqual([await page.getByLabel('Profile name').inputValue(), await page.getByLabel('Profile name').evaluate(el => el === document.activeElement), changes()], ['Reviewing', true, 8])
  await page.getByLabel('Profile name').press('Tab')
  await page.waitForFunction(() => document.querySelector('.card.profile .nm')?.textContent.startsWith('Reviewing'))
  assert.deepEqual([changes(), page.posts.at(-1)[1].name], [9, 'Reviewing'])
  await page.getByLabel('Profile name').fill('Reviews')
  await page.getByLabel('Profile name').press('Enter')
  await page.waitForFunction(() => document.querySelector('.card.profile .nm')?.textContent.trim() === 'Reviews Default')
  assert.equal(changes(), 10)

  await form.getByLabel('Name').fill('Quick')
  await form.getByRole('button', { name: 'Add Profile' }).click()
  await cards.nth(1).waitFor()
  assert.equal(await page.getByLabel('Profile name').inputValue(), 'Quick')
  await page.getByRole('group', { name: 'Default profile' }).getByRole('button', { name: 'On' }).click()
  await page.waitForFunction(() => [...document.querySelectorAll('.card.profile')].map(c => !!c.querySelector('.tag')).join() === 'false,true')
  await cards.nth(1).getByRole('button', { name: 'Done' }).click()
  assert.equal(await page.locator('.pedit').count(), 0)
  await cards.first().getByRole('button', { name: 'Remove' }).click()
  await page.waitForFunction(() => document.querySelectorAll('.card.profile').length === 1)
  assert.deepEqual(page.posts.at(-1), ['DELETE /api/profiles/p1', {}])
  assert.equal(await cards.locator('.nm').innerText(), 'Quick DEFAULT')
})

test('the New Session dialog opens with the default profile, and takes on another that is chosen, keeping what was written', async t => {
  const page = await world(t, 'home', kept([profile('p1', 'Reviews', { default: true, dir: '/work/elsewhere', effort: 'pair', model: 'opus' }),
    profile('p2', 'Thinker', { engine: 'local', thinking: false, temperature: 0.4 }), profile('p3', 'Solo', { effort: 'solo', effortLevel: 'high' })]))
  const dialog = page.getByRole('dialog', { name: 'New Session' }), field = name => dialog.locator(`[name="${name}"]`)
  const shown = async () => [await field('profile').inputValue(), await field('dir').inputValue(), await field('effortPick').inputValue(), await field('model').inputValue()]
  const engine = () => dialog.getByRole('group', { name: 'What it runs on' }).locator('button.on').innerText()
  await page.getByRole('button', { name: 'New Session', exact: true }).click()
  assert.deepEqual(await field('profile').locator('option').allInnerTexts(), ['No Profile', 'Reviews (default)', 'Thinker', 'Solo'])
  assert.deepEqual(await shown(), ['p1', '/work/elsewhere', 'pair', 'opus'])
  assert.equal(await field('prompt').evaluate(el => el === document.activeElement), true)
  await field('prompt').fill('Add a dead-letter queue')

  await field('profile').selectOption('p2')
  assert.deepEqual([await engine(), await field('temperature').inputValue(), await dialog.getByRole('group', { name: 'Thinking' }).locator('button.on').innerText()], ['fixture', '0.4', 'Off'])
  assert.deepEqual([await field('dir').inputValue(), await field('effortPick').inputValue(), await field('prompt').inputValue()], ['/work/elsewhere', 'pair', 'Add a dead-letter queue'])
  await field('profile').selectOption('p3')
  assert.deepEqual([await engine(), ...await shown(), await field('effortLevel').inputValue()], ['Claude Code', 'p3', '/work/solo', 'solo', '', 'high'])
  await field('profile').selectOption('')
  assert.deepEqual([...await shown(), await field('effortLevel').inputValue(), await field('prompt').inputValue()], ['', '/work/solo', 'solo', '', '', 'Add a dead-letter queue'])
  await field('profile').selectOption('p1')
  await field('effortPick').selectOption('solo')
  assert.equal(await field('dir').inputValue(), '/work/solo')
  await dialog.getByRole('button', { name: 'Start Session' }).click()
  await page.waitForFunction(() => !document.querySelector('.modal.start'))
  const [, body] = page.posts.find(([url]) => url === '/api/sessions')
  assert.deepEqual([body.prompt, body.dir, body.effort, body.model], ['Add a dead-letter queue', '/work/solo', 'solo', 'opus'])
  await page.keyboard.press('n')
  assert.deepEqual(await shown(), ['p1', '/work/elsewhere', 'pair', 'opus'])
  await page.keyboard.press('Escape')

  await page.goto(page.url().replace(/#.*/, '#efforts'))
  await page.locator('.ecard[data-id="fleet"] .eh').click()
  await page.locator('.ebig').getByRole('button', { name: 'New Session', exact: true }).click()
  assert.deepEqual(await shown(), ['p1', '/work/fleet', 'fleet', 'opus'])
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByRole('button', { name: /^Session Profiles/ }).click()
  await page.locator('.card.profile', { hasText: 'Solo' }).getByRole('button', { name: 'New Session' }).click()
  assert.deepEqual([...await shown(), await field('effortLevel').inputValue()], ['p3', '/work/solo', 'solo', '', 'high'])
})

test('an OpenCode profile chooses its agent, model and variant from what OpenCode offers where it starts', async t => {
  const offers = { agents: [{ name: 'build', description: 'Writes code' }, { name: 'review' }],
    models: [{ id: 'openai/gpt-5', name: 'GPT-5', provider: 'OpenAI', variants: ['high', 'low'] }, { id: 'zen/big-pickle', name: 'Big Pickle', provider: 'Zen', variants: [] }],
    defaults: { agentName: 'build', model: 'zen/big-pickle', variant: '' } }
  const page = await world(t, 'settings', { ...kept([profile('p1', 'Critical review', { engine: 'opencode', opencodeAgent: 'gone' }), profile('p2', 'Plain')]),
    capabilities: { opencode: { start: true, launchOptions: true } } }, { '/api/agents/opencode/launch-options': offers })
  await page.getByRole('button', { name: /^Session Profiles/ }).click()
  const card = page.locator('.card.profile').first(), pick = name => page.getByLabel(`Profile OpenCode ${name}`)
  await card.getByRole('button', { name: 'Edit' }).click()
  await page.locator('.pedit [role=status]', { hasText: 'your home directory' }).waitFor()
  assert.deepEqual(page.reads, ['/api/agents/opencode/launch-options?dir=~'])
  assert.deepEqual(await pick('agent').locator('option').allInnerTexts(), ['Native Default (build)', 'build — Writes code', 'review', 'gone (unavailable)'])
  assert.deepEqual(await pick('model').locator('option').allInnerTexts(), ['Native Default (zen/big-pickle)', 'GPT-5 · openai/gpt-5', 'Big Pickle · zen/big-pickle'])
  assert.deepEqual(await pick('variant').locator('option').allInnerTexts(), ['Native Default'])
  await pick('agent').selectOption('review')
  await pick('model').selectOption('openai/gpt-5')
  await page.waitForFunction(() => document.querySelectorAll('[aria-label="Profile OpenCode Variant"] option').length === 3)
  await pick('variant').selectOption('high')
  await page.waitForFunction(() => document.querySelector('.card.profile .why')?.textContent === 'OpenCode · review · openai/gpt-5 · high')
  assert.deepEqual(page.posts.map(([, sent]) => sent), [{ opencodeAgent: 'review' }, { opencodeModel: 'openai/gpt-5', opencodeVariant: '' }, { opencodeVariant: 'high' }])
  await pick('model').selectOption('zen/big-pickle')
  await page.waitForFunction(() => document.querySelector('.card.profile .why')?.textContent === 'OpenCode · review · zen/big-pickle')
  assert.deepEqual(page.posts.at(-1)[1], { opencodeModel: 'zen/big-pickle', opencodeVariant: '' })
  await pick('model').selectOption('openai/gpt-5')
  await page.waitForFunction(() => document.querySelectorAll('[aria-label="Profile OpenCode Variant"] option').length === 3)
  await pick('variant').selectOption('low')

  await page.getByLabel('Profile directory').fill('/work/pair')
  await page.getByLabel('Profile directory').press('Tab')
  await page.locator('.pedit [role=status]', { hasText: '/work/pair' }).waitFor()
  assert.deepEqual(page.reads, ['/api/agents/opencode/launch-options?dir=~', '/api/agents/opencode/launch-options?dir=/work/pair'])
  await page.waitForFunction(() => document.querySelector('.card.profile .meta')?.textContent.includes('/work/pair'))
  await card.getByRole('button', { name: 'New Session' }).click()
  const dialog = page.getByRole('dialog', { name: 'New Session' }), field = name => dialog.locator(`[name="${name}"]`)
  await page.waitForFunction(() => document.querySelector('.modal.start [name=variant]')?.value === 'low')
  assert.deepEqual([await field('profile').inputValue(), await field('agentName').inputValue(), await field('model').inputValue(), await field('dir').inputValue()], ['p1', 'review', 'openai/gpt-5', '/work/pair'])
  await field('prompt').fill('Review it')
  await page.waitForFunction(() => !document.querySelector('.modal.start button.go').disabled)
  await field('profile').selectOption('p2')
  assert.equal(await field('agentName').count(), 0)
  await field('profile').selectOption('p1')
  await page.waitForFunction(() => document.querySelector('.modal.start [name=variant]')?.value === 'low' && !document.querySelector('.modal.start button.go').disabled)
  await dialog.getByRole('button', { name: 'Start Session' }).click()
  await page.waitForFunction(() => !document.querySelector('.modal.start'))
  const [, body] = page.posts.find(([url]) => url === '/api/sessions')
  assert.deepEqual([body.agent, body.agentName, body.model, body.variant, body.dir], ['opencode', 'review', 'openai/gpt-5', 'low', '/work/pair'])
})

test('with no default profile the New Session dialog opens with none, and without profiles it does not ask for one', async t => {
  const page = await world(t, 'home', kept([profile('p1', 'Reviews', { model: 'opus' })]))
  const dialog = page.getByRole('dialog', { name: 'New Session' }), field = name => dialog.locator(`[name="${name}"]`)
  await page.getByRole('button', { name: 'New Session', exact: true }).click()
  assert.deepEqual([await field('profile').inputValue(), await field('model').inputValue()], ['', ''])
  await field('profile').selectOption('p1')
  assert.equal(await field('model').inputValue(), 'opus')
  const bare = await world(t, 'home')
  await bare.getByRole('button', { name: 'New Session', exact: true }).click()
  await bare.getByRole('dialog', { name: 'New Session' }).locator('[name=prompt]').waitFor()
  assert.equal(await bare.getByRole('dialog', { name: 'New Session' }).locator('[name=profile]').count(), 0)
})

test('the defaults a browser kept before there were profiles become the default profile, once', async t => {
  const page = await world(t, 'home')
  await page.locator('.home').waitFor()
  await page.evaluate(() => {
    const state = JSON.parse(localStorage.getItem('slorp.proto.v1') || '{}')
    localStorage.setItem('slorp.proto.v1', JSON.stringify({ ...state, startOpts: { engine: 'claude', dir: '/work/elsewhere', effort: 'pair', role: 'gone', model: 'opus', temperature: '' } }))
  })
  await page.reload()
  const dialog = page.getByRole('dialog', { name: 'New Session' }), field = name => dialog.locator(`[name="${name}"]`)
  await page.waitForFunction(() => !('startOpts' in JSON.parse(localStorage.getItem('slorp.proto.v1'))))
  assert.deepEqual(page.posts, [['/api/profiles', { name: 'Default', default: true, engine: 'claude', dir: '/work/elsewhere', effort: 'pair', role: '', terminal: false, model: 'opus', effortLevel: '', thinking: true, temperature: null }]])
  await page.getByRole('button', { name: 'New Session', exact: true }).click()
  await page.waitForFunction(() => document.querySelector('.modal.start [name=profile]')?.value === 'p1')
  assert.deepEqual([await field('dir').inputValue(), await field('effortPick').inputValue(), await field('model').inputValue()], ['/work/elsewhere', 'pair', 'opus'])
  await page.reload()
  await page.locator('.home').waitFor()
  assert.equal(page.posts.length, 1)
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
  await turn.getByRole('button', { name: 'Not Now' }).click()
  await turn.waitFor({ state: 'detached' })
  assert.equal(await page.locator('.qi.open .qh .t').innerText(), 'Which retry policy?')
  await page.getByRole('button', { name: 'Undo' }).click()
  await turn.locator('.qsaid').waitFor()
})

test('the legacy Attention view is there only while Settings keeps it, and the logo leads Home', async t => {
  const page = await world(t, 'settings')
  const keep = page.getByRole('group', { name: 'Show Legacy Attention View' }), tabs = () => page.locator('#nav button').allInnerTexts()
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
  const needs = page.locator('.ebig .sec').filter({ hasText: 'Needs You (4)' })
  await needs.waitFor()
  assert.deepEqual(await needs.locator('.row .t').allInnerTexts(), ['Question 1', 'Question 2', 'Question 3'])
  await needs.getByRole('button', { name: 'Show all four' }).click()
  assert.equal(await needs.locator('.row').count(), 4)
  await needs.getByRole('button', { name: 'Show Fewer' }).click()
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
  await pick('Move To', 'pair')
  await pick('', 'File With acme/widgets Again')
  await pick('File All of acme/widgets Under', 'fleet')
  await page.waitForFunction(() => document.querySelector('#toast.show'))
  assert.deepEqual(page.posts.filter(([url]) => /effort$/.test(url)), [
    ['/api/pulls/effort', { pull: 'acme/widgets#7', effort: 'pair' }],
    ['DELETE /api/pulls/effort', { pull: 'acme/widgets#7' }],
    ['/api/repos/effort', { repo: 'acme/widgets', effort: 'fleet' }],
  ])
})

const mac = { platform: 'darwin', terminal: { apps: [{ id: 'iterm', name: 'iTerm2' }, { id: 'terminal', name: 'Terminal' }], default: 'iterm' } }

test('a background session and a closed one are opened in the terminal Slorp was started from, or the one chosen', async t => {
  const page = await world(t, 'sessions', { ...mac,
    sessions: [{ ...session('bg', 'solo', 'Runs in the background', 'idle', 3), kind: 'background', job: 'j1' }, session('mine', 'solo', 'Runs in my terminal', 'idle', 5)],
    archive: [{ ...session('shut', 'solo', 'Closed yesterday', 'idle', 900), endedAt: ago(800) }] })
  const opened = () => page.posts.filter(([url]) => url.endsWith('/terminal'))
  const button = page.locator('.pane.detail').getByRole('button', { name: 'Open in iTerm2' })
  await page.locator('.pane.list .row', { hasText: 'Runs in the background' }).click()
  await button.click()
  await page.waitForFunction(() => document.querySelector('#toast.show')?.textContent.includes('Opened in iTerm2'))
  assert.deepEqual(opened(), [['/api/sessions/bg/terminal', {}]])
  await page.locator('.pane.list .row', { hasText: 'Runs in my terminal' }).click()
  await page.locator('.pane.detail h1', { hasText: 'Runs in my terminal' }).waitFor()
  assert.equal(await button.count(), 0)
  await page.locator('.pane.list .ghead .name', { hasText: 'Closed' }).click()
  await page.locator('.pane.list .row', { hasText: 'Closed yesterday' }).click()
  await button.click()
  await page.waitForFunction(() => document.querySelectorAll('#toast.show').length && window.fetch)
  assert.deepEqual(opened().map(([url]) => url), ['/api/sessions/bg/terminal', '/api/sessions/shut/terminal'])
  await page.getByRole('button', { name: 'Settings' }).click()
  assert.equal(await page.getByLabel('Terminal', { exact: true }).inputValue(), 'iterm')
  await page.getByLabel('Terminal', { exact: true }).selectOption('terminal')
  await page.waitForFunction(() => true)
  assert.deepEqual(page.posts.filter(([url]) => url === '/api/settings'), [['/api/settings', { terminal: 'terminal' }]])
})

test('nothing offers a terminal where Slorp knows of none', async t => {
  const page = await world(t, 'sessions', { platform: 'linux', terminal: { apps: [], default: '' },
    sessions: [{ ...session('bg', 'solo', 'Runs in the background', 'idle', 3), kind: 'background', job: 'j1' }] })
  await page.locator('.pane.detail h1', { hasText: 'Runs in the background' }).waitFor()
  assert.equal(await page.getByRole('button', { name: /^Open in / }).count(), 0)
  assert.equal(await page.getByRole('button', { name: 'New Session', exact: true }).getAttribute('title'), 'Start a session (n)')
  await page.keyboard.press('n')
  assert.equal(await page.getByRole('dialog', { name: 'New Session' }).locator('[name=terminal]').count(), 0)
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.locator('.card.set').first().waitFor()
  assert.equal(await page.getByLabel('Terminal', { exact: true }).count(), 0)
})

test('a new session is opened in a terminal once it is running, when the dialog or its profile asks for that', async t => {
  const page = await world(t, 'settings', { ...mac, sessions: [{ ...session('bg', 'solo', 'Runs in the background', 'idle', 3), kind: 'background', job: 'j1' }] },
    { '/api/sessions': { sessionId: 'bg' } })
  const dialog = page.getByRole('dialog', { name: 'New Session' }), tick = dialog.getByLabel('Open it in iTerm2 once it has started')
  await page.getByRole('button', { name: 'New Session', exact: true }).waitFor()
  await page.keyboard.press('n')
  assert.equal(await tick.isChecked(), false)
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: /^Session Profiles/ }).click()
  await page.locator('form[data-form="profiles"]').getByLabel('Name').fill('In a terminal')
  await page.getByRole('button', { name: 'Add Profile' }).click()
  await page.getByRole('group', { name: 'Default profile' }).getByRole('button', { name: 'On' }).click()
  await page.getByRole('group', { name: 'Open in Terminal' }).getByRole('button', { name: 'On' }).click()
  await page.waitForFunction(() => document.querySelector('.card.profile .meta')?.textContent === 'opened in iTerm2' && document.querySelector('.card.profile .tag'))
  await page.keyboard.press('n')
  assert.equal(await tick.isChecked(), true)
  await dialog.locator('[name=dir]').fill('/work/solo')
  await dialog.locator('[name=prompt]').fill('Carry on')
  assert.equal(await tick.isChecked(), true)
  await dialog.getByRole('button', { name: 'Start Session' }).click()
  await page.waitForFunction(() => document.querySelector('#toast.show')?.textContent.includes('Opened in iTerm2'))
  assert.deepEqual(page.posts.filter(([url]) => url.endsWith('/terminal')), [['/api/sessions/bg/terminal', {}]])
})

const spaces = { knowledge: { questions: [], context: [], routing: [], roles: [], workspaces: [{ id: 'w1', name: 'Pairing', dir: '/work/pair' }, { id: 'w2', name: 'Elsewhere', dir: '/work/elsewhere' }] } }

test('workspaces are kept under Knowledge, each saying where it files and leading to a session started there', async t => {
  const page = await world(t, 'knowledge', spaces)
  await page.getByRole('button', { name: /^Workspaces/ }).click()
  const cards = page.locator('.pane.detail .card')
  assert.deepEqual(await cards.locator('.nm').allInnerTexts(), ['Pairing', 'Elsewhere'])
  assert.deepEqual(await cards.locator('.why').allInnerTexts(), ['/work/pair', '/work/elsewhere'])
  assert.deepEqual(await cards.locator('.meta').allInnerTexts(), ['pair\ntwo open sessions', 'files under no effort\nno open sessions'])
  const form = page.locator('form[data-form="workspaces"]')
  await form.getByLabel('Name').fill('  Solo work ')
  await form.getByLabel('Directory').fill('/work/solo')
  await form.getByRole('button', { name: 'Add' }).click()
  await page.waitForFunction(() => document.querySelector('#toast.show')?.textContent.includes('Solo work is now a workspace'))
  await cards.nth(1).getByRole('button', { name: 'Remove' }).click()
  await page.waitForFunction(() => document.querySelector('#toast.show')?.textContent.includes('Removed'))
  assert.deepEqual(page.posts.filter(([url]) => url.includes('/api/kb/')), [['/api/kb/workspaces', { name: 'Solo work', dir: '/work/solo' }], ['DELETE /api/kb/workspaces/w2', {}]])
  await cards.first().getByRole('button', { name: 'New Session' }).click()
  const dialog = page.getByRole('dialog', { name: 'New Session' }), field = name => dialog.locator(`[name="${name}"]`)
  assert.deepEqual([await field('workspace').inputValue(), await field('dir').inputValue(), await field('effortPick').inputValue()], ['w1', '/work/pair', 'pair'])
})

test('the New Session dialog asks for a workspace, and for a directory only when none of them will do', async t => {
  const page = await world(t, 'home', spaces)
  const dialog = page.getByRole('dialog', { name: 'New Session' }), field = name => dialog.locator(`[name="${name}"]`)
  const typed = () => field('dir').isVisible()
  await page.getByRole('button', { name: 'New Session', exact: true }).click()
  assert.deepEqual(await field('workspace').locator('option').allInnerTexts(), ['Choose a Workspace…', 'Pairing — /work/pair', 'Elsewhere — /work/elsewhere', 'Another Directory…'])
  assert.deepEqual([await field('workspace').inputValue(), await typed()], ['', false])
  assert.equal(await field('workspace').evaluate(el => el === document.activeElement), true)
  assert.equal(await dialog.getByRole('button', { name: 'Start Session' }).isDisabled(), true)
  await field('prompt').fill('Carry on')
  await field('workspace').selectOption('w2')
  assert.deepEqual([await field('dir').inputValue(), await typed(), await field('effortPick').inputValue()], ['/work/elsewhere', false, ''])
  await field('workspace').selectOption('w1')
  assert.deepEqual([await field('dir').inputValue(), await typed(), await field('effortPick').inputValue(), await field('prompt').inputValue()], ['/work/pair', false, 'pair', 'Carry on'])
  assert.equal(await field('workspace').locator('option').first().innerText(), 'Pairing — /work/pair')

  await field('workspace').selectOption('__other')
  assert.deepEqual([await field('dir').inputValue(), await typed()], ['', true])
  assert.equal(await field('dir').evaluate(el => el === document.activeElement), true)
  await field('dir').fill('/work/elsewhere')
  assert.deepEqual([await field('workspace').inputValue(), await typed()], ['__other', true])
  await field('effortPick').selectOption('solo')
  assert.deepEqual([await field('dir').inputValue(), await field('workspace').inputValue(), await typed()], ['/work/solo', '__other', true])
  await field('effortPick').selectOption('pair')
  assert.deepEqual([await field('dir').inputValue(), await field('workspace').inputValue(), await typed()], ['/work/pair', 'w1', false])

  await field('workspace').selectOption('__other')
  await field('dir').fill('/work/thing/')
  await dialog.getByRole('button', { name: 'Start Session' }).click()
  await page.locator('#toast.show').getByRole('button', { name: 'Save its directory as a workspace' }).click()
  await page.waitForFunction(() => document.querySelector('#toast.show')?.textContent.includes('thing is now a workspace'))
  assert.deepEqual(page.posts.filter(([url]) => url.includes('/api/kb/')), [['/api/kb/workspaces', { name: 'thing', dir: '/work/thing/' }]])
  assert.equal(page.posts.find(([url]) => url === '/api/sessions')[1].dir, '/work/thing/')
})

test('the directory of a profile is a workspace, or one typed when none of them will do', async t => {
  const page = await world(t, 'settings', { knowledge: { ...spaces.knowledge, profiles: [profile('p1', 'Reviews', { default: true })] } })
  await page.getByRole('button', { name: /^Session Profiles/ }).click()
  await page.locator('.card.profile').getByRole('button', { name: 'Edit' }).click()
  const space = page.getByLabel('Profile workspace'), dir = page.getByLabel('Profile directory')
  const dialog = page.getByRole('dialog', { name: 'New Session' }), field = name => dialog.locator(`[name="${name}"]`)
  const opens = async () => {
    await page.keyboard.press('Control+n')
    const got = [await field('workspace').inputValue(), await field('dir').inputValue()]
    await page.keyboard.press('Escape')
    return got
  }
  const sent = () => page.posts.at(-1)[1].dir
  assert.deepEqual(await space.locator('option').allInnerTexts(), ['Left Open', 'Pairing — /work/pair', 'Elsewhere — /work/elsewhere', 'Another Directory…'])
  assert.deepEqual([await space.inputValue(), await dir.count()], ['', 0])
  await space.selectOption('w2')
  assert.deepEqual([await space.inputValue(), await dir.count(), sent()], ['w2', 0, '/work/elsewhere'])
  assert.deepEqual(await opens(), ['w2', '/work/elsewhere'])
  await space.selectOption('__other')
  await dir.waitFor()
  assert.equal(await dir.evaluate(el => el === document.activeElement), true)
  await dir.fill('/work/thing')
  await dir.press('Tab')
  assert.deepEqual([await space.inputValue(), await dir.inputValue(), sent()], ['__other', '/work/thing', '/work/thing'])
  assert.deepEqual(await opens(), ['__other', '/work/thing'])
  await dir.fill('/work/pair/')
  await dir.press('Tab')
  assert.deepEqual([await space.inputValue(), await dir.count()], ['w1', 0])
  await space.selectOption('')
  assert.deepEqual([sent(), await opens()], ['', ['', '']])
})

test('an OpenCode session Slorp runs is opened in a terminal where the service can attach to one', async t => {
  const run = { ...session('run', 'solo', 'Slorp runs this one', 'idle', 3), agent: 'opencode', job: 'j2', canStop: true }
  const mine = { ...session('mine', 'solo', 'This one is in my terminal', 'idle', 5), agent: 'opencode' }
  const page = await world(t, 'sessions', { ...mac, sessions: [run, mine], capabilities: { opencode: { attach: true } } })
  const button = page.locator('.pane.detail').getByRole('button', { name: 'Open in iTerm2' })
  await page.locator('.pane.list .row', { hasText: 'This one is in my terminal' }).click()
  await page.locator('.pane.detail h1', { hasText: 'This one is in my terminal' }).waitFor()
  assert.equal(await button.count(), 0)
  await page.locator('.pane.list .row', { hasText: 'Slorp runs this one' }).click()
  await button.click()
  await page.waitForFunction(() => document.querySelector('#toast.show')?.textContent.includes('Opened in iTerm2'))
  assert.deepEqual(page.posts.filter(([url]) => url.endsWith('/terminal')), [['/api/sessions/run/terminal', {}]])

  const without = await world(t, 'sessions', { ...mac, sessions: [run], capabilities: { opencode: { attach: false } } })
  await without.locator('.pane.detail h1', { hasText: 'Slorp runs this one' }).waitFor()
  assert.equal(await without.getByRole('button', { name: 'Open in iTerm2' }).count(), 0)
})
