const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createServer } = require('node:http')
const { join } = require('node:path')
const { test } = require('node:test')
const { chromium } = require('playwright')

const REVIEW = 'The builder has finished. Please review this and report:\n\n---\n{{response}}\n---'
const wrapped = said => REVIEW.replace('{{response}}', () => said)

/* A service with a builder that has said something, a reviewer in the same effort, a session elsewhere and one
   that cannot be replied to. It keeps handoffs as Slorp's does and records what is sent to each session. */
async function world(t, { handoffs = [] } = {}) {
  const at = new Date().toISOString()
  const session = (id, label, more) => ({ id, agent: 'claude', kind: 'background', label, cwd: '/work/app', branch: 'feature', status: 'idle', startedAt: at, activeAt: at,
    lastPrompt: '', lastReply: '', used: {}, canReply: true, timeline: [], effort: 'e1', ...more })
  const builder = session('ses_builder', 'Builder', { lastPrompt: 'Build the cache', lastReply: 'Done. I added the cache.\n\nIt costs $& and renders {{prompt}} as written.' })
  const reviewer = session('ses_reviewer', 'Reviewer', { agent: 'opencode', lastReply: 'Two problems found.' })
  const data = { sessions: [builder, reviewer, session('ses_other', 'Elsewhere', { effort: '' }), session('ses_mute', 'Unanswerable', { canReply: false })], items: [],
    efforts: [{ id: 'e1', name: 'Cache', dirs: ['/work/app'] }], knowledge: { questions: [], context: [], routing: [], roles: [], workspaces: [], profiles: [], handoffs: structuredClone(handoffs) },
    journal: [], plugins: { claude: { installed: true, enabled: true }, opencode: { installed: true, enabled: true } }, summarizer: { model: 'lab/fixture-model' }, capabilities: {}, actions: [], settings: {} }
  /* whole is all of what the builder last said, as its record has it, and why what keeps the record from being read. */
  const world = { data, builder, reviewer, sent: [], started: [], seq: 0, whole: '', why: '', reads: 0, release: null }
  const read = async req => { let body = ''; for await (const chunk of req) body += chunk; return JSON.parse(body || '{}') }
  const server = createServer(async (req, res) => {
    const json = (code, v) => { res.writeHead(code, { 'content-type': 'application/json' }); res.end(JSON.stringify(v)) }
    const kept = data.knowledge.handoffs, one = /^\/api\/handoffs\/([^/]+)$/.exec(req.url), reply = /^\/api\/sessions\/([^/]+)\/reply$/.exec(req.url)
    if (req.url === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write(': connected\n\n')
    } else if (req.url === '/api/sessions' && req.method === 'GET') {
      json(200, data)
    } else if (req.url === '/api/sessions' && req.method === 'POST') {
      world.started.push(await read(req)); json(200, { sessionId: 'ses_new' })
    } else if (req.url === '/api/sessions/ses_builder/response') {
      world.reads++
      await new Promise(resolve => { if (world.hold) world.release = resolve; else resolve() })
      json(200, world.why ? { text: builder.lastReply, whole: false, why: world.why } : { text: world.whole, whole: true })
    } else if (reply && req.method === 'POST') {
      world.sent.push({ to: reply[1], ...(await read(req)) }); json(202, {})
    } else if (req.url === '/api/handoffs' && req.method === 'POST') {
      const said = await read(req)
      if (kept.some(h => h.name.toLowerCase() === said.name.trim().toLowerCase())) { res.writeHead(400); res.end('a handoff already has that name'); return }
      const made = { id: `h${++world.seq}`, name: said.name.trim(), text: said.text.trim() }
      kept.push(made); json(200, { id: made.id })
    } else if (one && req.method === 'POST') {
      Object.assign(kept.find(h => h.id === one[1]), await read(req)); res.writeHead(204); res.end()
    } else if (one && req.method === 'DELETE') {
      kept.splice(kept.findIndex(h => h.id === one[1]), 1); res.writeHead(204); res.end()
    } else {
      res.setHeader('content-type', 'text/html'); res.end(readFileSync(join(__dirname, '..', 'index.html')))
    }
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(() => { server.closeAllConnections(); server.close() })
  const browser = await chromium.launch({ headless: true })
  t.after(() => browser.close())
  const page = await browser.newPage({ viewport: { width: 1500, height: 900 } })
  await page.addInitScript(() => {
    window.copied = []
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: async text => { window.copied.push(text) } } })
  })
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  await page.goto(`http://127.0.0.1:${server.address().port}/#sessions`)
  await page.waitForFunction(() => typeof live !== 'undefined' && live !== null)
  const dialog = page.locator('#modal .modal')
  /* Opens a session's page and then its Send To dialog. */
  const sendFrom = async id => {
    await page.evaluate(id => { state.actSel = id; render() }, id)
    await page.getByRole('button', { name: 'Send To…', exact: true }).click()
    await dialog.getByRole('heading', { name: 'Send To Another Session' }).waitFor()
  }
  return Object.assign(world, { page, dialog, sendFrom, toast: page.locator('#toast'), message: dialog.locator('textarea'),
    handoff: dialog.getByLabel('Handoff', { exact: true }), to: dialog.getByLabel('Send to', { exact: true }), send: dialog.locator('button.go') })
}

test('a handoff is added with a message to start from, changed, and removed under Settings', async t => {
  const o = await world(t)
  await o.page.getByRole('button', { name: 'Settings', exact: true }).click()
  await o.page.locator('#main .kbnav').getByRole('button', { name: /^Handoffs/ }).click()
  await o.page.locator('#main').getByText('No handoffs yet.').waitFor()
  await o.page.getByLabel('Name', { exact: true }).fill('Review Request')
  await o.page.getByRole('button', { name: 'Add Handoff' }).click()
  const form = o.page.locator('#main form[data-form="handoff"]')
  await form.waitFor()
  assert.match(await form.locator('textarea').inputValue(), /\{\{response\}\}/)
  await form.getByLabel('Handoff name').fill('Review')
  await form.locator('textarea').fill(REVIEW)
  await form.getByRole('button', { name: 'Save' }).click()
  await o.toast.getByText('Saved').waitFor()
  assert.deepEqual(o.data.knowledge.handoffs, [{ id: 'h1', name: 'Review', text: REVIEW }])
  await o.page.locator('#main .card[data-handoff="h1"] .nm').getByText('Review', { exact: true }).waitFor()
  assert.equal(await form.locator('textarea').inputValue(), REVIEW)
  await o.page.locator('#main .card[data-handoff="h1"]').getByRole('button', { name: 'Remove' }).click()
  await o.page.locator('#main').getByText('No handoffs yet.').waitFor()
  assert.deepEqual(o.data.knowledge.handoffs, [])
})

test('a response is sent to another session wrapped in the handoff chosen, once it has somewhere to go', async t => {
  const o = await world(t, { handoffs: [{ id: 'h1', name: 'Review', text: REVIEW }, { id: 'h2', name: 'Fix', text: 'From {{session}} on {{branch}} in {{directory}}, asked "{{prompt}}":\n{{response}}\n{{unknown}}' }] })
  await o.sendFrom('ses_builder')
  assert.equal(await o.message.inputValue(), wrapped(o.builder.lastReply))
  assert.equal(await o.to.inputValue(), '')
  assert.ok(await o.send.isDisabled())
  assert.deepEqual(await o.to.locator('option').allTextContents().then(all => all.map(x => x.split(' · ')[0])), ['Choose a Session…', 'Reviewer', 'Elsewhere', 'A New Session…'])
  assert.deepEqual(await o.to.locator('optgroup').evaluateAll(all => all.map(g => g.label)), ['In Cache', 'Elsewhere'])

  await o.handoff.selectOption('h2')
  assert.equal(await o.message.inputValue(), `From Builder on feature in /work/app, asked "Build the cache":\n${o.builder.lastReply}\n{{unknown}}`)
  await o.handoff.selectOption('')
  assert.equal(await o.message.inputValue(), o.builder.lastReply)
  await o.handoff.selectOption('h1')
  await o.message.fill(`${await o.message.inputValue()}\n\nBe strict.`)
  await o.to.selectOption('ses_reviewer')
  assert.equal(await o.message.inputValue(), `${wrapped(o.builder.lastReply)}\n\nBe strict.`)
  await o.send.click()
  await o.toast.getByText('Sent to Reviewer').waitFor()
  assert.deepEqual(o.sent, [{ to: 'ses_reviewer', text: `${wrapped(o.builder.lastReply)}\n\nBe strict.` }])
  assert.equal(await o.dialog.count(), 0)

  /* From the builder again, the same handoff and session are offered; from the reviewer, the builder is. */
  await o.sendFrom('ses_builder')
  assert.deepEqual([await o.handoff.inputValue(), await o.to.inputValue()], ['h1', 'ses_reviewer'])
  await o.dialog.getByRole('button', { name: 'Cancel' }).click()
  await o.sendFrom('ses_reviewer')
  assert.equal(await o.to.inputValue(), 'ses_builder')
  await o.handoff.selectOption('')
  await o.message.press('ControlOrMeta+Enter')
  await o.toast.getByText('Sent to Builder').waitFor()
  assert.deepEqual(o.sent[1], { to: 'ses_builder', text: 'Two problems found.' })
})

test('with no handoffs a response goes as it was written, and to a new session as its prompt', async t => {
  const o = await world(t)
  o.data.sessions = [o.builder]
  await o.page.evaluate(() => refresh(true))
  await o.sendFrom('ses_builder')
  await o.dialog.getByText('You have no handoffs yet.').waitFor()
  assert.equal(await o.message.inputValue(), o.builder.lastReply)
  assert.equal(await o.to.inputValue(), '__new')
  await o.dialog.getByRole('button', { name: 'Continue…' }).click()
  await o.dialog.getByRole('heading', { name: 'New Session' }).waitFor()
  assert.equal(await o.dialog.locator('textarea[name="prompt"]').inputValue(), o.builder.lastReply)
  assert.equal(await o.dialog.locator('[name="dir"]').inputValue(), '/work/app')
  await o.dialog.getByRole('button', { name: 'Start Session' }).click()
  await o.toast.getByText('Started.').waitFor()
  assert.equal(o.started.length, 1)
  assert.deepEqual([o.started[0].prompt, o.started[0].dir, o.started[0].effort], [o.builder.lastReply, '/work/app', 'e1'])
  assert.deepEqual(o.sent, [])
  assert.equal(await o.page.evaluate(() => state.handoffTo.ses_new), 'ses_builder')
})

test('a response longer than Slorp keeps is read whole from the session\'s record before it is sent', async t => {
  const o = await world(t, { handoffs: [{ id: 'h1', name: 'Review', text: REVIEW }] })
  o.whole = `It starts here. ${'y'.repeat(30000)}`
  o.builder.lastReply = `…${'y'.repeat(30000)}`
  o.hold = true
  await o.page.evaluate(() => refresh(true))
  await o.sendFrom('ses_builder')
  await o.dialog.getByText('Reading the whole response').waitFor()
  assert.equal(await o.message.count(), 0)
  await o.to.selectOption('ses_reviewer')
  assert.ok(await o.send.isDisabled())
  while (!o.release) await new Promise(resolve => setTimeout(resolve, 10))
  o.release()
  await o.message.waitFor()
  assert.equal(await o.message.inputValue(), wrapped(o.whole))
  assert.equal(await o.dialog.getByText('Only the end of the response').count(), 0)
  await o.handoff.selectOption('')
  assert.equal(await o.message.inputValue(), o.whole)
  await o.send.click()
  await o.toast.getByText('Sent to Reviewer').waitFor()
  assert.deepEqual(o.sent, [{ to: 'ses_reviewer', text: o.whole }])
  assert.equal(o.reads, 1)
})

test('the dialog says when the rest of a response could not be read, or the session is working again', async t => {
  const o = await world(t)
  await o.sendFrom('ses_builder')
  assert.equal(o.reads, 0)
  assert.equal(await o.dialog.locator('[role="status"]').count(), 0)
  await o.dialog.getByRole('button', { name: 'Cancel' }).click()

  o.builder.lastReply = `…${'y'.repeat(30000)}`
  o.builder.status = 'working'
  o.why = 'no transcript was found'
  await o.page.evaluate(() => refresh(true))
  await o.sendFrom('ses_builder')
  await o.dialog.getByText('Only the end of the response is here').waitFor()
  await o.dialog.getByText('(no transcript was found)').waitFor()
  await o.dialog.getByText('working again').waitFor()
  assert.equal(await o.message.inputValue(), o.builder.lastReply)
  o.why = ''
  o.whole = `All of it. ${'y'.repeat(30000)}`
  await o.dialog.getByRole('button', { name: 'Read It Again' }).click()
  await o.page.waitForFunction(whole => document.querySelector('#modal textarea')?.value === whole, o.whole)
  assert.equal(await o.dialog.getByText('Only the end of the response').count(), 0)
  assert.equal(o.reads, 2)
})

test('Copy takes a response longer than Slorp keeps whole from the session\'s record, and says when it could not', async t => {
  const o = await world(t)
  const copy = o.page.getByRole('button', { name: 'Copy', exact: true }), copied = () => o.page.evaluate(() => window.copied)
  await o.page.evaluate(() => { state.actSel = 'ses_builder'; render() })
  await copy.click()
  await o.toast.getByText('Copied', { exact: true }).waitFor()
  assert.deepEqual(await copied(), [o.builder.lastReply])
  assert.equal(o.reads, 0)

  o.whole = `It starts here. ${'y'.repeat(30000)}`
  o.builder.lastReply = 'y'.repeat(30000)
  o.hold = true
  await o.page.evaluate(() => refresh(true))
  await copy.click()
  const copying = o.page.getByRole('button', { name: 'Copying…', exact: true })
  await copying.waitFor()
  assert.ok(await copying.isDisabled())
  while (!o.release) await new Promise(resolve => setTimeout(resolve, 10))
  o.release()
  await copy.waitFor()
  assert.deepEqual((await copied()).slice(1), [o.whole])
  assert.equal(await o.toast.textContent(), 'Copied')

  o.hold = false
  o.why = 'no transcript was found'
  await copy.click()
  await o.toast.getByText('the rest could not be read (no transcript was found), so it starts part-way').waitFor()
  assert.deepEqual((await copied()).slice(2), [o.builder.lastReply])
  assert.equal(o.reads, 2)
})
