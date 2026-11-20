const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createServer } = require('node:http')
const { join } = require('node:path')
const { test } = require('node:test')
const { setTimeout: delay } = require('node:timers/promises')
const { chromium } = require('playwright')

const at = new Date().toISOString()
const session = (id, extra = {}) => ({ id, agent: 'opencode', kind: 'background', label: id, cwd: '/work/project', status: 'idle',
  startedAt: at, activeAt: at, lastPrompt: '', lastReply: '', used: {}, canReply: true, canStop: true, timeline: [], ...extra })
const catalog = () => ({
  agents: [{ name: 'build', description: 'Build things' }, { name: 'plan', description: 'Plan things' }],
  models: [{ id: 'acme/reasoner', name: 'Reasoner', provider: 'Acme', variants: ['high', 'low'] },
    { id: 'other/fast', name: 'Fast', provider: 'Other provider', variants: ['quick'] },
    { id: 'acme/plain', name: 'Plain', provider: 'Acme', variants: [] }],
  defaults: { agentName: 'build', model: 'acme/reasoner', variant: 'low' },
})
const json = (res, data, status = 200) => res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(data))
async function waitUntil(check) {
  const deadline = Date.now() + 5000
  while (!check() && Date.now() < deadline) await delay(10)
  assert.ok(check(), 'Expected server request did not arrive')
}

async function world(t, hash = 'sessions') {
  const data = { sessions: [session('owned'), session('terminal', { kind: 'interactive', canStop: false, job: 'not-ownership' })],
    efforts: [{ id: 'project', name: 'Project', about: '', dirs: ['/work/project'] }], items: [],
    archive: [{ ...session('archived'), endedAt: at }], summarizer: { model: 'fixture' },
    knowledge: { watch: [], questions: [], context: [], routing: [], roles: [{ id: 'reviewer', name: 'Reviewer', attention: 'all' }] },
    capabilities: { claude: { history: true, start: true, resume: true, resumeCommand: true, launchOptions: false },
      opencode: { history: true, start: true, resume: true, resumeCommand: true, launchOptions: true } } }
  const calls = [], events = new Set()
  const o = { data, calls, reads: 0, executions: [], journal: new Map(), catalog: (req, res) => json(res, catalog()), failure: {}, command: "cd '/work/a b' && '/opt/Open Code/bin/opencode' --session 'ses_quoted'" }
  o.makeLive = id => {
    const archived = data.archive.find(a => a.id === id)
    assert.ok(archived, 'Only an existing archived conversation can be resumed')
    data.archive = data.archive.filter(a => a.id !== id)
    data.sessions.unshift(session(id, { ...archived, endedAt: undefined, status: 'idle' }))
  }
  o.close = (id, endedAt = new Date(Date.now() + 1000).toISOString()) => {
    const live = data.sessions.find(s => s.id === id)
    assert.ok(live, 'Only a live conversation can close')
    data.sessions = data.sessions.filter(s => s.id !== id)
    data.archive = [{ ...live, endedAt }, ...data.archive.filter(a => a.id !== id)]
  }
  const server = createServer(async (req, res) => {
    const url = new URL(req.url, 'http://localhost')
    if (url.pathname === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write(': connected\n\n'); events.add(res)
      req.on('close', () => events.delete(res))
    } else if (req.method === 'GET' && url.pathname === '/api/sessions') {
      o.reads++
      if (o.failure.list) res.writeHead(503).end('Sessions unavailable')
      else json(res, data)
    }
    else if (url.pathname === '/api/agents/opencode/launch-options') {
      calls.push({ path: url.pathname, dir: url.searchParams.get('dir') }); o.catalog(req, res)
    } else if (url.pathname.endsWith('/resume-command')) {
      calls.push({ path: url.pathname })
      if (o.failure.command) res.writeHead(502).end(o.failure.command)
      else json(res, { cwd: '/work/a b', command: o.command })
    } else if (req.method === 'POST') {
      let text = ''; for await (const chunk of req) text += chunk
      const body = JSON.parse(text)
      calls.push({ path: url.pathname, body })
      if (o.post) { await o.post(req, res, body); return }
      if (o.failure.post) { res.writeHead(409).end(o.failure.post); return }
      if (url.pathname.endsWith('/stop')) { res.writeHead(204).end(); return }
      const resumeId = url.pathname.endsWith('/resume') ? decodeURIComponent(url.pathname.split('/')[3]) : ''
      const agent = resumeId ? (data.archive.find(a => a.id === resumeId) || data.sessions.find(s => s.id === resumeId))?.agent : body.agent || 'claude'
      const key = agent === 'opencode' && body.requestId ? `${url.pathname}:${body.requestId}` : ''
      const { requestId, ...fields } = body, fingerprint = JSON.stringify(fields)
      const recorded = key && o.journal.get(key)
      if (recorded) {
        assert.equal(recorded.fingerprint, fingerprint, 'A journal key cannot be reused for a different payload')
        json(res, recorded.body, recorded.status); return
      }
      o.executions.push({ path: url.pathname, body })
      const id = resumeId || 'new-session'
      const response = o.journalFailure ? { status: 502, body: { sessionId: id, error: o.journalFailure } }
        : { status: 202, body: { sessionId: id } }
      if (key) o.journal.set(key, { ...response, fingerprint })
      if (!o.journalFailure) {
        if (resumeId) { if (!o.deferResumeSnapshot) o.makeLive(resumeId) }
        else data.sessions.unshift(session(id, { agent }))
      }
      json(res, response.body, response.status)
    } else if (url.pathname.startsWith('/api/archive/')) json(res, { ...data.archive.find(a => a.id === decodeURIComponent(url.pathname.split('/').pop())), timeline: [] })
    else if (url.pathname.endsWith('/past')) json(res, { days: [], closed: [], closedCount: 0 })
    else if (url.pathname === '/') { res.writeHead(200, { 'content-type': 'text/html' }); res.end(readFileSync(join(__dirname, '..', 'index.html'))) }
    else res.writeHead(404).end('Not found')
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(() => { server.closeAllConnections(); server.close() })
  const browser = await chromium.launch({ headless: true })
  t.after(() => browser.close())
  const page = await browser.newPage({ viewport: { width: 1500, height: 1000 } })
  page.setDefaultTimeout(5000)
  await page.addInitScript(() => {
    window.copied = []
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: async text => { window.copied.push(text) } } })
  })
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  await page.goto(`http://127.0.0.1:${server.address().port}/#${hash}`)
  await page.waitForFunction(() => document.querySelector('#tools').textContent.includes('live · fixture'))
  o.page = page
  o.tick = async () => {
    const response = page.waitForResponse(r => r.url().endsWith('/api/sessions'))
    for (const res of events) res.write('data: changed\n\n')
    await response
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
  }
  o.open = async () => {
    await page.getByRole('button', { name: 'New Session', exact: true }).click()
    return page.getByRole('dialog', { name: 'New Session' })
  }
  return o
}

const field = (dialog, name) => dialog.locator(`[name="${name}"]`)
const submit = dialog => dialog.getByRole('button', { name: 'Start session', exact: true })
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i
function launchFields(body) {
  const { requestId, ...fields } = body
  assert.match(requestId, uuid)
  return fields
}
async function ready(dialog) {
  await dialog.locator('select[name="model"] optgroup option[value="acme/reasoner"]').waitFor({ state: 'attached' })
}
async function openCode(o) {
  const dialog = await o.open()
  await field(dialog, 'dir').fill('/work/project')
  await field(dialog, 'prompt').fill('Do the work')
  await dialog.getByRole('button', { name: 'OpenCode', exact: true }).click()
  await ready(dialog)
  return dialog
}

for (const entry of ['sessions', 'effort']) test(`start OpenCode from ${entry} with agent, provider/model, variant and Slorp role`, async t => {
  const o = await world(t, entry === 'effort' ? 'efforts' : 'sessions')
  if (entry === 'effort') {
    await o.page.locator('[data-act="effort"][data-id="project"]').first().click()
  }
  const dialog = await o.open()
  assert.equal(await field(dialog, 'dir').inputValue(), entry === 'effort' ? '/work/project' : '')
  await field(dialog, 'dir').fill('/work/chosen directory')
  await field(dialog, 'prompt').fill('Review the implementation')
  await field(dialog, 'name').fill('Review task')
  await field(dialog, 'role').selectOption('reviewer')
  await dialog.getByRole('button', { name: 'OpenCode', exact: true }).click()
  await ready(dialog)
  assert.deepEqual(await dialog.locator('optgroup').evaluateAll(groups => groups.map(g => g.label)), ['Acme', 'Other provider'])
  await field(dialog, 'agentName').selectOption('plan')
  await field(dialog, 'model').selectOption('other/fast')
  await field(dialog, 'variant').selectOption('quick')
  await o.tick()
  assert.equal(await field(dialog, 'agentName').inputValue(), 'plan')
  assert.equal(await field(dialog, 'variant').inputValue(), 'quick')
  await submit(dialog).click()
  await o.page.locator('.pane.detail h1').filter({ hasText: 'new-session' }).waitFor()
  assert.deepEqual(launchFields(o.calls.find(c => c.path === '/api/sessions').body), { agent: 'opencode', dir: '/work/chosen directory', name: 'Review task', prompt: 'Review the implementation', role: 'reviewer', effort: entry === 'effort' ? 'project' : '', agentName: 'plan', model: 'other/fast', variant: 'quick' })
  assert.ok(o.calls.some(c => c.dir === '/work/chosen directory'))
})

test('OpenCode native defaults remain empty in the payload', async t => {
  const o = await world(t), dialog = await openCode(o)
  for (const name of ['agentName', 'model', 'variant']) assert.equal(await field(dialog, name).inputValue(), '')
  assert.match(await field(dialog, 'model').innerText(), /Native default \(acme\/reasoner\)/)
  await submit(dialog).click()
  await dialog.waitFor({ state: 'hidden' })
  const body = o.calls.find(c => c.path === '/api/sessions').body
  assert.deepEqual([body.agentName, body.model, body.variant], ['', '', ''])
  assert.equal(body.effortLevel, undefined)
  assert.equal(body.local, undefined)
})

test('engine switches and SSE preserve independent options and common form values', async t => {
  const o = await world(t), dialog = await o.open()
  await field(dialog, 'dir').fill('/work/project')
  await field(dialog, 'prompt').fill('Keep this prompt')
  await field(dialog, 'name').fill('Keep this name')
  await field(dialog, 'role').selectOption('reviewer')
  await field(dialog, 'model').selectOption('sonnet')
  await field(dialog, 'effortLevel').selectOption('high')
  await dialog.getByRole('button', { name: 'OpenCode', exact: true }).click()
  await ready(dialog)
  await field(dialog, 'agentName').selectOption('plan')
  await field(dialog, 'model').selectOption('other/fast')
  await field(dialog, 'variant').selectOption('quick')
  await dialog.getByRole('button', { name: 'fixture', exact: true }).click()
  await field(dialog, 'temperature').fill('0.75')
  await dialog.getByRole('button', { name: 'Off', exact: true }).click()
  await dialog.getByRole('button', { name: 'Claude Code', exact: true }).click()
  assert.equal(await field(dialog, 'model').inputValue(), 'sonnet')
  assert.equal(await field(dialog, 'effortLevel').inputValue(), 'high')
  await dialog.getByRole('button', { name: 'OpenCode', exact: true }).click()
  await o.tick()
  assert.deepEqual(await dialog.locator('select').evaluateAll(els => Object.fromEntries(els.map(el => [el.name, el.value]))), { role: 'reviewer', agentName: 'plan', model: 'other/fast', variant: 'quick' })
  assert.equal(await field(dialog, 'prompt').inputValue(), 'Keep this prompt')
  assert.equal(await field(dialog, 'name').inputValue(), 'Keep this name')
  await dialog.getByRole('button', { name: 'fixture', exact: true }).click()
  assert.equal(await field(dialog, 'temperature').inputValue(), '0.75')
  assert.equal(await dialog.getByRole('button', { name: 'Off', exact: true }).getAttribute('aria-pressed'), 'true')
  await submit(dialog).click()
  await dialog.waitFor({ state: 'hidden' })
  const body = o.calls.find(c => c.path === '/api/sessions').body
  assert.deepEqual(body.local, { thinking: false, temperature: 0.75 })
  for (const key of ['model', 'variant', 'agentName', 'effortLevel']) assert.equal(body[key], undefined)
})

test('legacy Claude capabilities and launch fields remain compatible', async t => {
  const o = await world(t)
  o.data.capabilities = { claude: { history: true } }
  await o.tick()
  const dialog = await o.open()
  assert.equal(await dialog.getByRole('button', { name: 'OpenCode', exact: true }).isDisabled(), true)
  await field(dialog, 'dir').fill('/work/project')
  await field(dialog, 'prompt').fill('Claude task')
  await field(dialog, 'model').selectOption('opus')
  await field(dialog, 'effortLevel').selectOption('max')
  await submit(dialog).click()
  await dialog.waitFor({ state: 'hidden' })
  assert.deepEqual(o.calls.find(c => c.path === '/api/sessions').body, { dir: '/work/project', name: '', prompt: 'Claude task', role: '', effort: '', model: 'opus', effortLevel: 'max' })
})

test('directory catalog is debounced, blocks launch and ignores stale success and error responses', async t => {
  const o = await world(t), pending = new Map()
  o.catalog = (req, res) => pending.set(new URL(req.url, 'http://localhost').searchParams.get('dir'), res)
  const dialog = await o.open()
  await dialog.getByRole('button', { name: 'OpenCode', exact: true }).click()
  await field(dialog, 'prompt').fill('Keep me')
  await field(dialog, 'dir').fill('/work/old')
  await waitUntil(() => pending.has('/work/old'))
  assert.ok(pending.has('/work/old'))
  assert.equal(await submit(dialog).isDisabled(), true)
  await field(dialog, 'dir').fill('/work/intermediate')
  await field(dialog, 'dir').fill('/work/new')
  await waitUntil(() => pending.has('/work/new'))
  assert.ok(pending.has('/work/new'))
  assert.equal(pending.has('/work/intermediate'), false)
  json(pending.get('/work/new'), catalog())
  await ready(dialog)
  await field(dialog, 'model').selectOption('other/fast')
  await field(dialog, 'variant').selectOption('quick')
  json(pending.get('/work/old'), { agents: [], models: [], defaults: {} })
  await o.tick()
  assert.equal(await field(dialog, 'model').inputValue(), 'other/fast')
  assert.equal(await submit(dialog).isDisabled(), false)
  await field(dialog, 'dir').fill('/work/failing-old')
  await waitUntil(() => pending.has('/work/failing-old'))
  await field(dialog, 'dir').fill('/work/final')
  await waitUntil(() => pending.has('/work/final'))
  json(pending.get('/work/final'), catalog())
  await ready(dialog)
  pending.get('/work/failing-old').writeHead(500).end('Stale failure')
  await o.tick()
  assert.equal(await dialog.getByText('Stale failure', { exact: false }).count(), 0)
  assert.equal(await field(dialog, 'variant').inputValue(), 'quick')
  assert.equal(await field(dialog, 'prompt').inputValue(), 'Keep me')
})

test('catalog errors can retry and unavailable choices never silently fall back', async t => {
  const o = await world(t), dialog = await openCode(o)
  await field(dialog, 'agentName').selectOption('plan')
  await field(dialog, 'model').selectOption('other/fast')
  await field(dialog, 'variant').selectOption('quick')
  o.catalog = (req, res) => res.writeHead(503).end('Catalog unavailable')
  await field(dialog, 'dir').fill('/work/changed')
  await dialog.getByRole('button', { name: 'Retry launch options' }).waitFor()
  assert.match(await dialog.getByRole('status').innerText(), /503.*Catalog unavailable/)
  assert.equal(await submit(dialog).isDisabled(), true)
  o.catalog = (req, res) => json(res, { agents: [{ name: 'build' }], models: [catalog().models[0]], defaults: catalog().defaults })
  await dialog.getByRole('button', { name: 'Retry launch options' }).click()
  await ready(dialog)
  assert.equal(await field(dialog, 'model').inputValue(), 'other/fast')
  assert.equal(await field(dialog, 'agentName').inputValue(), 'plan')
  assert.equal(await field(dialog, 'variant').inputValue(), 'quick')
  assert.equal(await submit(dialog).isDisabled(), true)
  await field(dialog, 'agentName').selectOption('')
  await field(dialog, 'model').selectOption('acme/reasoner')
  assert.equal(await field(dialog, 'variant').inputValue(), '')
  assert.deepEqual(await field(dialog, 'variant').locator('option').evaluateAll(els => els.map(el => el.value)), ['', 'high', 'low'])
  assert.equal(await submit(dialog).isDisabled(), false)
})

test('variants track the selected model, including no variants and native default', async t => {
  const o = await world(t), dialog = await openCode(o)
  await field(dialog, 'variant').selectOption('high')
  await field(dialog, 'model').selectOption('acme/plain')
  assert.deepEqual(await field(dialog, 'variant').locator('option').evaluateAll(els => els.map(el => el.value)), [''])
  await field(dialog, 'model').selectOption('')
  await field(dialog, 'variant').selectOption('low')
  assert.equal(await submit(dialog).isDisabled(), false)
})

test('explicit capabilities disable launch and remain authoritative across SSE', async t => {
  const o = await world(t), dialog = await openCode(o)
  o.data.capabilities.opencode.start = false
  await o.tick()
  assert.equal(await submit(dialog).isDisabled(), true)
  o.data.capabilities.opencode.start = true
  o.data.capabilities.opencode.launchOptions = false
  await o.tick()
  assert.equal(await submit(dialog).isDisabled(), true)
  await dialog.getByRole('button', { name: 'Claude Code', exact: true }).click()
  o.data.capabilities.claude.start = false
  await o.tick()
  assert.equal(await submit(dialog).isDisabled(), true)
  assert.equal(await dialog.getByRole('button', { name: 'fixture', exact: true }).isDisabled(), true)
  assert.equal(o.calls.filter(c => c.path === '/api/sessions').length, 0)
})

test('owned OpenCode can stop, terminal cannot, and explicit canStop overrides Claude job compatibility', async t => {
  const o = await world(t), page = o.page
  await page.locator('[data-act="actSel"][data-id="owned"]').click()
  await page.locator('#acts > summary').click()
  await page.getByRole('button', { name: 'Stop session', exact: true }).click()
  await page.locator('#toast').getByRole('button', { name: 'Stop', exact: true }).click()
  await page.waitForFunction(() => document.querySelector('#toast').textContent.includes('Stopped.'))
  assert.deepEqual(o.calls.find(c => c.path.endsWith('/stop')), { path: '/api/sessions/owned/stop', body: {} })
  await page.locator('[data-act="actSel"][data-id="terminal"]').click()
  assert.equal(await page.getByRole('button', { name: 'Stop session', exact: true }).count(), 0)
  o.data.sessions[1].agent = 'claude'
  await o.tick()
  assert.equal(await page.getByRole('button', { name: 'Stop session', exact: true }).count(), 0)
  delete o.data.sessions[1].canStop
  await o.tick()
  assert.equal(await page.locator('[data-act="stop"]').count(), 1)
})

async function openArchive(o) {
  if (!await o.page.locator('[data-act="actSel"][data-id="archived"]').count()) await o.page.locator('[data-act="collapse"][data-key="act:closed"]').click()
  await o.page.locator('[data-act="actSel"][data-id="archived"]').click()
}
async function waitResumed(o) {
  await o.page.locator('[data-act="reopen"][data-id="archived"]').waitFor({ state: 'hidden' })
  await o.page.locator('.pane.detail h1').filter({ hasText: 'archived' }).waitFor()
  assert.ok(o.data.sessions.some(s => s.id === 'archived'))
  assert.ok(!o.data.archive.some(s => s.id === 'archived'))
}

for (const prompt of ['', 'Continue with the tests']) test(`reopen archived OpenCode ${prompt ? 'with' : 'without'} a prompt`, async t => {
  const o = await world(t)
  await openArchive(o)
  if (prompt) await o.page.getByRole('textbox', { name: 'Reopen prompt' }).fill(prompt)
  await o.tick()
  await o.page.getByRole('button', { name: 'Reopen in the background' }).click()
  await waitResumed(o)
  assert.deepEqual(launchFields(o.calls.find(c => c.path.endsWith('/resume')).body), prompt ? { prompt } : {})
})

test('copy uses the backend command verbatim for live and archived sessions and retries failures', async t => {
  const o = await world(t), page = o.page
  await page.locator('[data-act="actSel"][data-id="owned"]').click()
  await page.locator('#acts > summary').click()
  o.failure.command = 'Command temporarily unavailable'
  await page.getByRole('button', { name: 'Copy resume command' }).click()
  await page.waitForFunction(() => document.querySelector('#toast').textContent.includes('502'))
  assert.deepEqual(await page.evaluate(() => window.copied), [])
  delete o.failure.command
  await page.locator('#acts > summary').click()
  await page.getByRole('button', { name: 'Copy resume command' }).click()
  await page.waitForFunction(() => window.copied.length === 1)
  await openArchive(o)
  await page.getByRole('button', { name: 'Copy resume command' }).click()
  await page.waitForFunction(() => window.copied.length === 2)
  assert.deepEqual(await page.evaluate(() => window.copied), [o.command, o.command])
  assert.ok(o.calls.some(c => c.path === '/api/sessions/owned/resume-command'))
  assert.ok(o.calls.some(c => c.path === '/api/sessions/archived/resume-command'))
  assert.equal(await page.getByText('claude --resume', { exact: false }).count(), 0)
})

test('start and reopen errors retain input for retry; resume actions follow capabilities', async t => {
  const o = await world(t), dialog = await openCode(o)
  o.failure.post = 'Session busy'
  await submit(dialog).click()
  await o.page.waitForFunction(() => document.querySelector('#toast').textContent.includes('409'))
  assert.equal(await field(dialog, 'prompt').inputValue(), 'Do the work')
  assert.equal(await submit(dialog).isDisabled(), false)
  delete o.failure.post
  await submit(dialog).click()
  await dialog.waitFor({ state: 'hidden' })
  assert.equal(o.calls.filter(c => c.path === '/api/sessions').length, 2)
  await openArchive(o)
  o.failure.post = 'Session busy'
  await o.page.getByRole('textbox', { name: 'Reopen prompt' }).fill('Retry me')
  await o.page.getByRole('button', { name: 'Reopen in the background' }).click()
  await o.page.waitForFunction(() => document.querySelector('#toast').textContent.includes('Could not reopen'))
  assert.equal(await o.page.getByRole('textbox', { name: 'Reopen prompt' }).inputValue(), 'Retry me')
  o.data.capabilities.opencode.resume = false
  o.data.capabilities.opencode.resumeCommand = false
  await o.tick()
  assert.equal(await o.page.getByRole('button', { name: 'Reopen in the background' }).count(), 0)
  assert.equal(await o.page.getByRole('button', { name: 'Copy resume command' }).count(), 0)
  delete o.failure.post
  o.data.capabilities.opencode.resume = true
  await o.tick()
  await o.page.getByRole('button', { name: 'Reopen in the background' }).click()
  await waitResumed(o)
  assert.equal(o.calls.filter(c => c.path.endsWith('/resume')).length, 2)
})

test('a directory that drops only the selected variant requires an explicit replacement', async t => {
  const o = await world(t), dialog = await openCode(o)
  await field(dialog, 'model').selectOption('acme/reasoner')
  await field(dialog, 'variant').selectOption('high')
  o.catalog = (req, res) => { const data = catalog(); data.models[0].variants = ['low']; json(res, data) }
  await field(dialog, 'dir').fill('/work/limited')
  await ready(dialog)
  assert.equal(await field(dialog, 'variant').inputValue(), 'high')
  assert.match(await dialog.getByRole('status').innerText(), /Choose an available variant/)
  assert.equal(await submit(dialog).isDisabled(), true)
  await field(dialog, 'variant').selectOption('')
  assert.equal(await submit(dialog).isDisabled(), false)
})

test('Claude attach and legacy resume convenience survive while declared commands use the endpoint', async t => {
  const o = await world(t), page = o.page
  o.data.sessions[0] = session('owned', { agent: 'claude', job: 'job-123' })
  o.data.archive[0].agent = 'claude'
  o.data.capabilities.claude = { history: true }
  await o.tick()
  await page.locator('[data-act="actSel"][data-id="owned"]').click()
  await page.locator('#acts > summary').click()
  await page.getByRole('button', { name: 'Copy attach command' }).click()
  await page.waitForFunction(() => window.copied.length === 1)
  assert.equal(await page.evaluate(() => window.copied[0]), 'claude attach job-123')
  await openArchive(o)
  await page.getByRole('button', { name: 'Copy resume command' }).click()
  await page.waitForFunction(() => window.copied.length === 2)
  assert.equal(await page.evaluate(() => window.copied[1]), 'cd /work/project && claude --resume archived')
  assert.equal(o.calls.filter(c => c.path.endsWith('/resume-command')).length, 0)
  o.data.capabilities.claude.resumeCommand = true
  await o.tick()
  await page.getByRole('button', { name: 'Copy resume command' }).click()
  await page.waitForFunction(() => window.copied.length === 3)
  assert.equal(await page.evaluate(() => window.copied[2]), o.command)
})

test('empty native catalogs and malformed catalog retries do not invent launch choices', async t => {
  const o = await world(t)
  o.catalog = (req, res) => json(res, {})
  const dialog = await o.open()
  await field(dialog, 'dir').fill('/work/project')
  await dialog.getByRole('button', { name: 'OpenCode', exact: true }).click()
  await dialog.getByRole('button', { name: 'Retry launch options' }).waitFor()
  assert.match(await dialog.getByRole('status').innerText(), /Invalid launch options response/)
  o.catalog = (req, res) => json(res, { agents: [], models: [], defaults: {} })
  await dialog.getByRole('button', { name: 'Retry launch options' }).click()
  await dialog.getByRole('button', { name: 'Retry launch options' }).waitFor({ state: 'hidden' })
  await field(dialog, 'prompt').fill('Use native settings')
  for (const name of ['agentName', 'model', 'variant']) assert.equal(await field(dialog, name).locator('option').count(), 1)
  await submit(dialog).click()
  await dialog.waitFor({ state: 'hidden' })
  const body = o.calls.find(c => c.path === '/api/sessions').body
  assert.deepEqual([body.agentName, body.model, body.variant], ['', '', ''])
})

test('local launch keeps native temperature and prevents invalid input', async t => {
  const o = await world(t), dialog = await o.open()
  await dialog.getByRole('button', { name: 'fixture', exact: true }).click()
  assert.equal(await submit(dialog).isDisabled(), true)
  await field(dialog, 'dir').fill('/work/project')
  await field(dialog, 'prompt').fill('Local work')
  await field(dialog, 'temperature').fill('3')
  assert.equal(await submit(dialog).isDisabled(), true)
  await field(dialog, 'temperature').fill('0.03')
  assert.equal(await submit(dialog).isDisabled(), true)
  await field(dialog, 'temperature').fill('')
  assert.equal(await submit(dialog).isDisabled(), false)
  await submit(dialog).click()
  await dialog.waitFor({ state: 'hidden' })
  assert.deepEqual(o.calls.find(c => c.path === '/api/sessions').body.local, { thinking: true, temperature: null })
})

test('catalog display strings are text, not markup, and only choice IDs reach launch', async t => {
  const o = await world(t)
  o.catalog = (req, res) => {
    const data = catalog()
    data.agents[1].description = '<img src=x onerror="window.injected=true">'
    data.models[0].name = '<script>window.injected=true</script>'
    data.models[0].provider = 'Provider " & <img src=x>'
    json(res, data)
  }
  const dialog = await openCode(o)
  await field(dialog, 'agentName').selectOption('plan')
  await field(dialog, 'model').selectOption('acme/reasoner')
  assert.equal(await dialog.locator('img, script').count(), 0)
  assert.equal(await o.page.evaluate(() => window.injected), undefined)
  assert.match(await field(dialog, 'agentName').innerText(), /<img/)
  await submit(dialog).click()
  await dialog.waitFor({ state: 'hidden' })
  const body = o.calls.find(c => c.path === '/api/sessions').body
  assert.equal(body.agentName, 'plan')
  assert.equal(body.model, 'acme/reasoner')
  assert.equal(body.provider, undefined)
})

for (const location of ['live', 'archive']) test(`partial start blocks duplicate creation and opens the retained ${location} after a fresh read`, async t => {
  const o = await world(t), dialog = await openCode(o)
  const error = 'Session was created but its effort/role could not be bound; conversation retained'
  o.post = (req, res) => json(res, { sessionId: 'retained', error }, 500)
  await submit(dialog).click()
  const retained = o.page.getByRole('dialog', { name: 'Conversation retained' })
  await retained.waitFor()
  assert.match(await retained.innerText(), /500/)
  assert.ok((await retained.innerText()).includes(error))
  assert.ok((await retained.innerText()).includes('retained'))
  assert.equal(await submit(retained).count(), 0)
  await o.tick()
  await retained.getByRole('button', { name: 'Copy conversation ID' }).click()
  assert.deepEqual(await o.page.evaluate(() => window.copied), ['retained'])
  await retained.getByRole('button', { name: 'Close', exact: true }).click()
  await o.open()
  await retained.waitFor()
  assert.equal(await o.page.locator('[data-form="start"]').count(), 0)
  const entry = session('retained', { label: 'Retained conversation' })
  if (location === 'live') o.data.sessions.unshift(entry)
  else o.data.archive.unshift({ ...entry, endedAt: at })
  const reads = o.reads
  await retained.getByRole('button', { name: 'View existing conversation' }).click()
  await o.page.locator('.pane.detail h1').filter({ hasText: 'Retained conversation' }).waitFor()
  assert.ok(o.reads > reads)
  assert.equal(await retained.count(), 0)
  assert.equal(await o.page.getByRole('button', { name: 'Reopen in the background' }).count(), location === 'archive' ? 1 : 0)
  assert.deepEqual(o.calls.filter(c => c.body).map(c => c.path), ['/api/sessions'])
})

test('partial start only restores the launch draft after an explicit new-session reset', async t => {
  const o = await world(t), dialog = await openCode(o)
  await field(dialog, 'name').fill('Original task')
  await field(dialog, 'role').selectOption('reviewer')
  await field(dialog, 'agentName').selectOption('plan')
  await field(dialog, 'model').selectOption('other/fast')
  await field(dialog, 'variant').selectOption('quick')
  o.post = (req, res) => json(res, { sessionId: 'retained', error: 'Activating OpenCode bridge failed' }, 502)
  await submit(dialog).click()
  const retained = o.page.getByRole('dialog', { name: 'Conversation retained' })
  await retained.waitFor()
  await o.tick()
  await retained.getByRole('button', { name: 'Prepare a new session' }).click()
  await dialog.waitFor()
  assert.equal(o.calls.filter(c => c.body).length, 1)
  assert.deepEqual(await dialog.locator('select').evaluateAll(els => Object.fromEntries(els.map(el => [el.name, el.value]))), { role: 'reviewer', agentName: 'plan', model: 'other/fast', variant: 'quick' })
  assert.equal(await field(dialog, 'name').inputValue(), 'Original task')
  assert.equal(await field(dialog, 'dir').inputValue(), '/work/project')
  assert.equal(await field(dialog, 'prompt').inputValue(), 'Do the work')
  delete o.post
  await submit(dialog).click()
  await dialog.waitFor({ state: 'hidden' })
  const starts = o.calls.filter(c => c.path === '/api/sessions')
  assert.equal(starts.length, 2)
  assert.deepEqual(launchFields(starts[1].body), launchFields(starts[0].body))
  assert.notEqual(starts[1].body.requestId, starts[0].body.requestId)
})

test('a pending start cannot be duplicated by closing the modal, and ambiguous prompt failure never resends', async t => {
  const o = await world(t), dialog = await openCode(o)
  let pending
  o.post = (req, res) => { pending = res }
  await submit(dialog).click()
  await waitUntil(() => pending)
  await o.tick()
  assert.equal(await dialog.getByRole('button', { name: 'Starting…' }).isDisabled(), true)
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
  await o.open()
  assert.equal(await dialog.getByRole('button', { name: 'Starting…' }).isDisabled(), true)
  assert.equal(await field(dialog, 'prompt').inputValue(), 'Do the work')
  json(pending, { sessionId: 'prompt-pending', error: 'Prompt acknowledgement timed out; delivery may be pending' }, 502)
  const retained = o.page.getByRole('dialog', { name: 'Conversation retained' })
  await retained.waitFor()
  assert.match(await retained.innerText(), /502.*Prompt acknowledgement timed out/)
  assert.match(await retained.innerText(), /prompt may have been accepted despite the error/i)
  await retained.getByRole('button', { name: 'View existing conversation' }).click()
  await retained.getByRole('status').filter({ hasText: 'not listed yet' }).waitFor()
  assert.equal(await o.page.locator('[data-form="start"]').count(), 0)
  o.data.archive.unshift({ ...session('prompt-pending', { label: 'Prompt may have run', lastPrompt: 'Do the work' }), endedAt: at })
  await retained.getByRole('button', { name: 'View existing conversation' }).click()
  await o.page.locator('.pane.detail h1').filter({ hasText: 'Prompt may have run' }).waitFor()
  assert.equal(await o.page.getByRole('textbox', { name: 'Reopen prompt' }).inputValue(), '')
  assert.deepEqual(o.calls.filter(c => c.body).map(c => c.path), ['/api/sessions'])
})

test('viewing a retained conversation retries only the read when the service is unavailable', async t => {
  const o = await world(t), dialog = await openCode(o)
  o.post = (req, res) => json(res, { sessionId: 'retained', error: 'Setup failed' }, 502)
  await submit(dialog).click()
  const retained = o.page.getByRole('dialog', { name: 'Conversation retained' })
  await retained.waitFor()
  o.failure.list = true
  await retained.getByRole('button', { name: 'View existing conversation' }).click()
  await retained.getByRole('status').filter({ hasText: 'Could not refresh sessions' }).waitFor()
  assert.equal(await retained.getByRole('button', { name: 'View existing conversation' }).isDisabled(), false)
  delete o.failure.list
  o.data.sessions.unshift(session('retained', { label: 'Recovered live session' }))
  await retained.getByRole('button', { name: 'View existing conversation' }).click()
  await o.page.locator('.pane.detail h1').filter({ hasText: 'Recovered live session' }).waitFor()
  assert.deepEqual(o.calls.filter(c => c.body).map(c => c.path), ['/api/sessions'])
})

test('plain Claude and malformed errors without a valid session ID remain retryable', async t => {
  for (const body of ['Claude executable not found', '{"sessionId":', 'null', '{"sessionId":17,"error":{"message":"bad"}}', '{"error":"Invalid launch options"}']) {
    await t.test(body, async t => {
      const o = await world(t), dialog = await o.open()
      await field(dialog, 'dir').fill('/work/project')
      await field(dialog, 'prompt').fill('Claude task')
      o.post = (req, res) => res.writeHead(502).end(body)
      await submit(dialog).click()
      await o.page.waitForFunction(() => document.querySelector('#toast').textContent.includes('Could not start session: Request failed (502)'))
      assert.equal(await submit(dialog).isDisabled(), false)
      assert.equal(await field(dialog, 'prompt').inputValue(), 'Claude task')
      assert.equal(await o.page.getByRole('dialog', { name: 'Conversation retained' }).count(), 0)
      delete o.post
      await submit(dialog).click()
      await dialog.waitFor({ state: 'hidden' })
      assert.equal(o.calls.filter(c => c.path === '/api/sessions').length, 2)
    })
  }
})

test('a valid retained ID still blocks creation when the error field is malformed and display values are escaped', async t => {
  const o = await world(t), dialog = await openCode(o)
  const id = 'retained-<img src=x onerror="window.injected=true">'
  o.post = (req, res) => json(res, { sessionId: id, error: { message: '<script>window.injected=true</script>' } }, 502)
  await submit(dialog).click()
  const retained = o.page.getByRole('dialog', { name: 'Conversation retained' })
  await retained.waitFor()
  assert.ok((await retained.innerText()).includes(id))
  assert.equal(await retained.locator('img, script').count(), 0)
  assert.equal(await submit(retained).count(), 0)
  await retained.getByRole('button', { name: 'Copy conversation ID' }).click()
  assert.deepEqual(await o.page.evaluate(() => window.copied), [id])
  assert.equal(await o.page.evaluate(() => window.injected), undefined)
  assert.deepEqual(o.calls.filter(c => c.body).map(c => c.path), ['/api/sessions'])
})

for (const operation of ['start', 'resume']) test(`OpenCode ${operation} reuses its request ID after a network failure, including navigation and whitespace-only edits`, async t => {
  const o = await world(t), page = o.page
  const path = operation === 'start' ? '/api/sessions' : '/api/archive/archived/resume'
  let dialog, lost
  if (operation === 'start') dialog = await openCode(o)
  else {
    await openArchive(o)
    await page.getByRole('textbox', { name: 'Reopen prompt' }).fill('Do the work')
  }
  await page.route(`**${path}`, async route => {
    if (route.request().method() === 'POST' && !lost) {
      lost = route.request().postDataJSON()
      await route.abort('failed')
    } else await route.continue()
  })
  const attempt = () => operation === 'start' ? submit(dialog) : page.getByRole('button', { name: 'Reopen in the background' })
  await attempt().click()
  await page.waitForFunction(() => /Could not (start|reopen) session/.test(document.querySelector('#toast').textContent))
  assert.match(lost.requestId, uuid)
  await o.tick()
  if (operation === 'start') {
    await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
    dialog = await o.open()
    assert.equal(await field(dialog, 'prompt').inputValue(), 'Do the work')
    await field(dialog, 'prompt').fill('  Do the work  ')
    await field(dialog, 'dir').fill('  /work/project  ')
  } else {
    await page.locator('[data-act="actSel"][data-id="owned"]').click()
    await openArchive(o)
    assert.equal(await page.getByRole('textbox', { name: 'Reopen prompt' }).inputValue(), 'Do the work')
    await page.getByRole('textbox', { name: 'Reopen prompt' }).fill('  Do the work  ')
  }
  await attempt().click()
  if (operation === 'start') await page.locator('.pane.detail h1').filter({ hasText: 'new-session' }).waitFor()
  else await waitResumed(o)
  const retried = o.calls.find(c => c.path === path).body
  assert.deepEqual(retried, lost)
  assert.equal(retried.prompt, 'Do the work')
  if (operation === 'start') dialog = await openCode(o)
  else {
    o.close('archived')
    await o.tick()
    await openArchive(o)
    await page.getByRole('textbox', { name: 'Reopen prompt' }).fill('Do the work')
  }
  await attempt().click()
  await waitUntil(() => o.calls.filter(c => c.path === path).length === 2)
  assert.notEqual(o.calls.filter(c => c.path === path)[1].body.requestId, lost.requestId)
})

test('a start retry changes its ID only for a changed normalized payload or explicit new-session reset', async t => {
  const o = await world(t), dialog = await openCode(o)
  o.failure.post = 'Outcome pending'
  const attempt = async () => {
    const response = o.page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith('/api/sessions'))
    await submit(dialog).click()
    await response
    await dialog.locator('button.go:not([disabled])').waitFor()
    return o.calls.filter(c => c.path === '/api/sessions').at(-1).body
  }
  let previous = await attempt()
  assert.match(previous.requestId, uuid)
  await field(dialog, 'prompt').fill(' Do the work ')
  assert.deepEqual(await attempt(), previous)
  for (const [name, value] of [['name', 'Named task'], ['role', 'reviewer'], ['agentName', 'plan'], ['model', 'other/fast'], ['variant', 'quick'], ['prompt', 'Different work'], ['dir', '/work/different']]) {
    if (['name', 'prompt', 'dir'].includes(name)) await field(dialog, name).fill(value)
    else await field(dialog, name).selectOption(value)
    if (name === 'dir') await ready(dialog)
    const next = await attempt()
    assert.match(next.requestId, uuid)
    assert.notEqual(next.requestId, previous.requestId, `${name} must create a new logical attempt`)
    assert.deepEqual(await attempt(), next)
    previous = next
  }
  await dialog.getByRole('button', { name: 'Prepare a new session' }).click()
  const count = o.calls.filter(c => c.path === '/api/sessions').length
  assert.equal(count, 16)
  const next = await attempt()
  assert.deepEqual(launchFields(next), launchFields(previous))
  assert.notEqual(next.requestId, previous.requestId)
})

for (const operation of ['start', 'resume']) test(`unknown ${operation} outcome retains the ID and offers only explicit recovery actions`, async t => {
  const o = await world(t), page = o.page
  let dialog
  const id = operation === 'start' ? 'retained-unknown' : 'archived'
  const path = operation === 'start' ? '/api/sessions' : '/api/archive/archived/resume'
  if (operation === 'start') dialog = await openCode(o)
  else {
    await openArchive(o)
    await page.getByRole('textbox', { name: 'Reopen prompt' }).fill('Acknowledge the review')
  }
  o.post = (req, res) => json(res, { sessionId: id, error: 'Request outcome unknown; the prompt may already be running', outcome: 'unknown' }, 409)
  const attempt = () => operation === 'start' ? submit(dialog) : page.getByRole('button', { name: 'Reopen in the background' })
  await attempt().click()
  const retained = page.getByRole('dialog', { name: 'Conversation retained' })
  await retained.waitFor()
  assert.match(await retained.innerText(), /Outcome: unknown/)
  assert.equal(await retained.getByRole('button', { name: 'Reopen in the background' }).count(), 0)
  const first = o.calls.find(c => c.path === path).body
  assert.match(first.requestId, uuid)
  await o.tick()
  if (operation === 'start') o.data.archive.unshift({ ...session(id, { label: 'Unknown outcome' }), endedAt: at })
  await retained.getByRole('button', { name: 'View existing conversation' }).click()
  await retained.waitFor({ state: 'hidden' })
  assert.equal(o.calls.filter(c => c.body).length, 1)
  if (operation === 'resume') {
    assert.equal(await page.getByRole('textbox', { name: 'Reopen prompt' }).inputValue(), 'Acknowledge the review')
    await attempt().click()
    await retained.waitFor()
    assert.deepEqual(o.calls.filter(c => c.path === path)[1].body, first)
    await retained.getByRole('button', { name: 'Close', exact: true }).click()
    await page.getByRole('textbox', { name: 'Reopen prompt' }).fill('A different explicit prompt')
    await attempt().click()
    await retained.waitFor()
    const changed = o.calls.filter(c => c.path === path)[2].body
    assert.notEqual(changed.requestId, first.requestId)
    assert.equal(changed.prompt, 'A different explicit prompt')
  } else {
    await o.open()
    await retained.waitFor()
    assert.equal(await page.locator('[data-form="start"]').count(), 0)
  }
})

test('resume with no prompt keeps its ID on a pending response and Claude resumes omit requestId', async t => {
  const o = await world(t), page = o.page
  await openArchive(o)
  o.post = (req, res) => json(res, { sessionId: 'archived', error: 'Request still pending', outcome: 'pending' }, 409)
  await page.getByRole('button', { name: 'Reopen in the background' }).click()
  const retained = page.getByRole('dialog', { name: 'Conversation retained' })
  await retained.waitFor()
  assert.match(await retained.innerText(), /Outcome: pending/)
  await retained.getByRole('button', { name: 'Close', exact: true }).click()
  await page.getByRole('textbox', { name: 'Reopen prompt' }).fill('   ')
  await page.getByRole('button', { name: 'Reopen in the background' }).click()
  await retained.waitFor()
  const requests = o.calls.filter(c => c.path.endsWith('/resume'))
  assert.deepEqual(launchFields(requests[0].body), {})
  assert.deepEqual(requests[1].body, requests[0].body)
  await retained.getByRole('button', { name: 'Close', exact: true }).click()
  o.data.archive[0].agent = 'claude'
  await o.tick()
  delete o.post
  await page.getByRole('button', { name: 'Reopen in the background' }).click()
  await waitResumed(o)
  assert.deepEqual(o.calls.filter(c => c.path.endsWith('/resume'))[2].body, {})
})

async function loseAcceptedResume(o) {
  let lost = false
  await o.page.route('**/api/archive/archived/resume', async route => {
    if (lost) { await route.continue(); return }
    lost = true
    const response = await route.fetch()
    assert.equal(response.status(), 202)
    await route.abort('failed')
  })
  await o.page.getByRole('button', { name: 'Reopen in the background' }).click()
  await o.page.waitForFunction(() => document.querySelector('#toast').textContent.includes('Could not reopen session'))
  assert.equal(o.executions.length, 1)
}

for (const prompt of ['', 'Continue the same task']) {
  test(`an accepted but lost resume response replays within the same closure (${prompt ? 'same prompt' : 'no prompt'})`, async t => {
    const o = await world(t), page = o.page
    o.deferResumeSnapshot = true
    await openArchive(o)
    await page.getByRole('textbox', { name: 'Reopen prompt' }).fill(prompt)
    await loseAcceptedResume(o)
    await o.tick()
    await page.getByRole('button', { name: 'Reopen in the background' }).click()
    await page.waitForFunction(() => document.querySelector('#toast').textContent.includes('Reopened in the background'))
    const calls = o.calls.filter(c => c.path.endsWith('/resume'))
    assert.equal(calls.length, 2)
    assert.equal(o.executions.length, 1)
    assert.deepEqual(calls[1].body, calls[0].body)
    assert.deepEqual([...o.journal.values()][0].body, { sessionId: 'archived' })
    await page.getByRole('button', { name: 'Reopen in the background' }).click()
    await waitUntil(() => o.calls.filter(c => c.path.endsWith('/resume')).length === 3)
    assert.equal(o.executions.length, 1)
    assert.deepEqual(o.calls.filter(c => c.path.endsWith('/resume'))[2].body, calls[0].body)
  })

  for (const transition of ['observed live, same timestamp', 'new timestamp without live observation']) {
    test(`a new archived incarnation gets a fresh resume after a lost response (${transition}, ${prompt ? 'same prompt' : 'no prompt'})`, async t => {
      const o = await world(t), page = o.page
      await openArchive(o)
      await page.getByRole('textbox', { name: 'Reopen prompt' }).fill(prompt)
      await loseAcceptedResume(o)
      if (transition.startsWith('observed')) {
        await o.tick()
        assert.equal(await page.getByRole('textbox', { name: 'Reopen prompt' }).count(), 0)
      }
      o.close('archived', transition.startsWith('observed') ? at : new Date(Date.parse(at) + 1000).toISOString())
      await o.tick()
      await openArchive(o)
      await page.getByRole('textbox', { name: 'Reopen prompt' }).fill(prompt)
      await page.getByRole('button', { name: 'Reopen in the background' }).click()
      await waitUntil(() => o.calls.filter(c => c.path.endsWith('/resume')).length === 2)
      assert.equal(o.executions.length, 2)
      const [first, next] = o.executions
      assert.deepEqual(launchFields(next.body), launchFields(first.body))
      assert.notEqual(next.body.requestId, first.body.requestId)
      assert.ok(o.data.sessions.some(s => s.id === 'archived'))
      assert.ok(!o.data.archive.some(s => s.id === 'archived'))
    })
  }
}

for (const prompt of ['', 'This prompt may already have run']) test(`a durable resume failure requires a confirmed new attempt after the cause is fixed (${prompt ? 'with prompt' : 'without prompt'})`, async t => {
  const o = await world(t), page = o.page
  await openArchive(o)
  await page.getByRole('textbox', { name: 'Reopen prompt' }).fill(prompt)
  o.journalFailure = 'Provider temporarily unavailable'
  await page.getByRole('button', { name: 'Reopen in the background' }).click()
  const retained = page.getByRole('dialog', { name: 'Conversation retained' })
  await retained.waitFor()
  assert.equal(o.executions.length, 1)
  delete o.journalFailure
  await retained.getByRole('button', { name: 'Close', exact: true }).click()
  await page.getByRole('button', { name: 'Reopen in the background' }).click()
  await retained.waitFor()
  assert.match(await retained.innerText(), /Provider temporarily unavailable/)
  const calls = o.calls.filter(c => c.path.endsWith('/resume'))
  assert.equal(calls.length, 2)
  assert.equal(o.executions.length, 1)
  assert.deepEqual(calls[1].body, calls[0].body)
  await retained.getByRole('button', { name: 'Prepare a new resume attempt', exact: true }).click()
  assert.match(await retained.innerText(), /prompt may already have run/i)
  assert.equal(o.calls.filter(c => c.body).length, 2)
  await retained.getByRole('button', { name: 'Cancel reset', exact: true }).click()
  await retained.getByRole('button', { name: 'Close', exact: true }).click()
  await page.getByRole('button', { name: 'Reopen in the background' }).click()
  await retained.waitFor()
  assert.deepEqual(o.calls.filter(c => c.path.endsWith('/resume'))[2].body, calls[0].body)
  await retained.getByRole('button', { name: 'Prepare a new resume attempt', exact: true }).click()
  await retained.getByRole('button', { name: 'Confirm new resume attempt', exact: true }).click()
  await retained.waitFor({ state: 'hidden' })
  await page.locator('[data-act="actSel"][data-id="owned"]').click()
  await openArchive(o)
  assert.equal(await page.getByRole('textbox', { name: 'Reopen prompt' }).inputValue(), prompt)
  assert.equal(o.calls.filter(c => c.body).length, 3)
  assert.equal(o.executions.length, 1)
  await page.getByRole('button', { name: 'Reopen in the background' }).click()
  await waitUntil(() => o.executions.length === 2)
  assert.notEqual(o.executions[1].body.requestId, o.executions[0].body.requestId)
  assert.deepEqual(launchFields(o.executions[1].body), launchFields(o.executions[0].body))
  assert.ok(o.data.sessions.some(s => s.id === 'archived'))
  assert.equal(o.calls.filter(c => c.path === '/api/sessions').length, 0)
})

test('a resume reset requires a fresh archived state and cannot reset while offline or already live', async t => {
  const o = await world(t), page = o.page
  await openArchive(o)
  o.journalFailure = 'Setup failed'
  await page.getByRole('button', { name: 'Reopen in the background' }).click()
  const retained = page.getByRole('dialog', { name: 'Conversation retained' })
  await retained.waitFor()
  await retained.getByRole('button', { name: 'Prepare a new resume attempt', exact: true }).click()
  o.failure.list = true
  await retained.getByRole('button', { name: 'Confirm new resume attempt', exact: true }).click()
  await retained.getByRole('status').filter({ hasText: 'Could not refresh sessions' }).waitFor()
  delete o.failure.list
  await retained.getByRole('button', { name: 'Close', exact: true }).click()
  await page.getByRole('button', { name: 'Reopen in the background' }).click()
  await retained.waitFor()
  const calls = o.calls.filter(c => c.path.endsWith('/resume'))
  assert.equal(calls.length, 2)
  assert.deepEqual(calls[1].body, calls[0].body)
  assert.equal(o.executions.length, 1)
  await retained.getByRole('button', { name: 'Prepare a new resume attempt', exact: true }).click()
  o.makeLive('archived')
  await retained.getByRole('button', { name: 'Confirm new resume attempt', exact: true }).click()
  await retained.getByRole('status').filter({ hasText: 'already live' }).waitFor()
  await retained.getByRole('button', { name: 'View existing conversation' }).click()
  await retained.waitFor({ state: 'hidden' })
  assert.equal(o.calls.filter(c => c.body).length, 2)
  assert.equal(o.executions.length, 1)
})
