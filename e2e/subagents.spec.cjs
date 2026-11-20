const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createServer } = require('node:http')
const { join } = require('node:path')
const { test } = require('node:test')
const { chromium } = require('playwright')

const pageHTML = readFileSync(join(__dirname, '..', 'index.html'))
const missing = 'No record is available for this subagent.'
const failed = 'Could not load what this subagent was asked and reported. Try again.'
const loading = 'Fetching what it was asked and reported…'
const report = text => ({ body: { prompt: 'Review the implementation', report: text, handed: true } })
function deferred() {
  let resolve
  const promise = new Promise(done => { resolve = done })
  return { promise, resolve }
}

async function world(t, { archived = false, children } = {}) {
  const startedAt = new Date(Date.now() - 120000).toISOString()
  const activeAt = new Date(Date.now() - 60000).toISOString()
  const subagents = children || [
    { id: 'child', description: 'Implementation review', type: 'reviewer', depth: 1, status: 'completed', done: true, reportRevision: 1, startedAt, activeAt, tools: 3, used: { in: 100, out: 50, cacheWrite: 20, cacheRead: 900 }, samples: [100, 170] },
    { id: 'nested', parentId: 'child', description: 'Check edge cases', type: 'tester', depth: 2, status: 'completed', done: true, reportRevision: 1, startedAt, activeAt, tools: 2, used: { in: 80, out: 20 } },
  ]
  const session = { id: 'ses_subagents', agent: 'opencode', kind: 'interactive', label: 'Subagent fixture', cwd: '/work', status: 'idle', startedAt, activeAt, used: { in: 500, out: 170 }, timeline: [], subagents }
  const data = { sessions: [session], archive: [], items: [], summarizer: { off: true } }
  const o = { session, data, requests: [], reply: id => report(`Report for ${id}`) }
  o.archive = () => {
    session.endedAt = new Date().toISOString()
    data.sessions = []
    data.archive = [{ ...session, subagents: undefined }]
  }
  if (archived) o.archive()
  const server = createServer(async (req, res) => {
    if (req.url === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write(': connected\n\n')
    } else if (req.url === '/api/sessions') {
      res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(data))
    } else if (req.url === `/api/archive/${session.id}`) {
      res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(session))
    } else if (req.url.startsWith(`/api/sessions/${session.id}/subagents/`)) {
      const id = decodeURIComponent(req.url.split('/').pop())
      o.requests.push({ id, url: req.url })
      const reply = await o.reply(id, o.requests.length)
      if (reply.disconnect) { res.destroy(); return }
      res.writeHead(reply.status || 200, { 'content-type': 'application/json' })
      res.end(reply.raw ?? JSON.stringify(reply.body || {}))
    } else if (req.url.startsWith('/api/')) {
      res.writeHead(404).end()
    } else {
      res.writeHead(200, { 'content-type': 'text/html' }).end(pageHTML)
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
  page.setDefaultTimeout(10000)
  await page.goto(`http://127.0.0.1:${server.address().port}/#sessions`)
  if (archived) await page.locator('[data-act="collapse"][data-key="act:closed"]').click()
  else await page.getByRole('button', { name: /^Subagents\d/ }).click()
  await page.locator('.lane').first().waitFor()
  o.page = page
  o.lane = id => page.locator('.lane').filter({ has: page.locator('.t', { hasText: subagents.find(a => a.id === id).description }) })
  o.refresh = () => page.evaluate(() => refresh(true))
  o.text = text => page.locator('.subdoc').getByText(text, { exact: true }).waitFor()
  o.retry = () => page.getByRole('button', { name: 'Retry loading report', exact: true })
  return o
}

test('OpenCode nested children reuse lanes, parent context, usage, timing and lazy reports', async t => {
  const o = await world(t)
  assert.equal(o.requests.length, 0)
  assert.equal(await o.page.locator('.lane').count(), 2)
  const nested = o.lane('nested')
  assert.match(await nested.innerText(), /started by Implementation review/)
  assert.equal(await nested.evaluate(el => el.style.paddingLeft), '16px')
  assert.equal(await o.lane('child').locator('.num').innerText(), '1m 00s\n170')
  assert.match(await o.page.locator('.tabbody > p').innerText(), /270 tokens.*400 in the conversation itself/)
  await nested.click()
  await o.text('Report for nested')
  await o.text('What it was asked')
  await o.text('What it reported')
  assert.deepEqual(o.requests.map(r => r.id), ['nested'])
  await nested.click()
  await nested.click()
  await o.refresh()
  assert.deepEqual(o.requests.map(r => r.id), ['nested'])
})

test('a completed retained report replaces an earlier 404 without an activity timestamp change', async t => {
  const o = await world(t)
  o.reply = () => ({ status: 404 })
  await o.lane('child').click()
  await o.text(missing)
  assert.equal(await o.retry().isVisible(), true)
  assert.doesNotMatch(await o.page.locator('.subdoc').innerText(), /Claude Code|Could not load/)
  await o.refresh()
  assert.equal(o.requests.length, 1)
  o.reply = () => report('The complete retained report')
  o.session.subagents[0].reportRevision++
  await o.refresh()
  await o.text('The complete retained report')
  assert.equal(o.requests.length, 2)
  assert.equal(await o.retry().count(), 0)
})

test('lifecycle and activity changes invalidate cached reports even without a revision', async t => {
  const o = await world(t)
  const child = o.session.subagents[0]
  delete child.reportRevision
  child.status = 'running'
  child.done = false
  await o.refresh()
  o.reply = () => ({ body: { prompt: 'Review', report: 'Work in progress', handed: false } })
  await o.lane('child').click()
  await o.text('The last thing it has said')
  for (const [field, value, text] of [
    ['status', 'idle', 'Idle update'],
    ['done', true, 'Final reply'],
    ['activeAt', new Date().toISOString(), 'Transcript update'],
  ]) {
    child[field] = value
    o.reply = () => report(text)
    await o.refresh()
    await o.text(text)
  }
  assert.equal(o.requests.length, 4)
})

test('a new revision hides the cached report and shows refresh failures with a retry', async t => {
  const o = await world(t)
  await o.lane('child').click()
  await o.text('Report for child')
  o.reply = () => ({ status: 502 })
  o.session.subagents[0].reportRevision++
  await o.refresh()
  await o.text(failed)
  assert.doesNotMatch(await o.page.locator('.subdoc').innerText(), /Report for child/)
  o.reply = () => report('Updated report')
  await o.retry().click()
  await o.text('Updated report')
  assert.equal(o.requests.length, 3)
})

for (const [name, failure] of [['HTTP', { status: 503 }], ['network', { disconnect: true }], ['malformed JSON', { raw: '{broken' }]]) {
  test(`${name} failures stay retryable instead of looking missing`, async t => {
    const o = await world(t)
    o.reply = () => failure
    await o.lane('child').click()
    await o.text(failed)
    assert.doesNotMatch(await o.page.locator('.subdoc').innerText(), /No record/)
    const attempts = o.requests.length
    await o.refresh()
    assert.equal(o.requests.length, attempts)
    o.reply = () => report('Recovered report')
    await o.retry().click()
    await o.text('Recovered report')
    assert.equal(o.requests.length, attempts + 1)
    assert.equal(await o.retry().count(), 0)
  })
}

test('missing reports can be retried manually without a new revision', { timeout: 15000 }, async t => {
  const o = await world(t)
  o.reply = () => ({ status: 404 })
  await o.lane('child').click()
  await o.text(missing)
  const started = deferred(), response = deferred()
  o.reply = () => { started.resolve(); return response.promise }
  await o.retry().click()
  await started.promise
  await o.text(loading)
  assert.equal(await o.retry().count(), 0)
  await o.refresh()
  assert.equal(o.requests.length, 2)
  response.resolve(report('Found on retry'))
  await o.text('Found on retry')
})

for (const oldResult of [report('Stale report'), { status: 404 }, { status: 502 }]) {
  test(`an older inflight ${oldResult.status || 'success'} cannot overwrite a newer revision`, { timeout: 15000 }, async t => {
    const o = await world(t)
    const started = deferred(), response = deferred()
    o.reply = (id, count) => {
      if (count === 1) { started.resolve(); return response.promise }
      return report('Latest report')
    }
    await o.lane('child').click()
    await started.promise
    await o.text(loading)
    o.session.subagents[0].reportRevision++
    await o.refresh()
    await o.text('Latest report')
    const finished = o.page.waitForEvent('requestfinished', { predicate: req => req.url().includes('/subagents/child') })
    response.resolve(oldResult)
    await finished
    await o.refresh()
    await o.text('Latest report')
    assert.equal(o.requests.length, 2)
    assert.equal(await o.retry().count(), 0)
  })
}

test('an older response cannot clear a newer inflight request or display stale content', { timeout: 15000 }, async t => {
  const o = await world(t)
  const started = [deferred(), deferred()], responses = [deferred(), deferred()]
  o.reply = (id, count) => {
    if (count > 2) return report('Unexpected duplicate request')
    started[count - 1].resolve()
    return responses[count - 1].promise
  }
  await o.lane('child').click()
  await started[0].promise
  o.session.subagents[0].reportRevision++
  await o.refresh()
  await started[1].promise
  const finished = o.page.waitForEvent('requestfinished', { predicate: req => req.url().includes('/subagents/child') })
  responses[0].resolve(report('Stale report'))
  await finished
  await o.refresh()
  await o.text(loading)
  assert.equal(o.requests.length, 2)
  responses[1].resolve(report('Latest report'))
  await o.text('Latest report')
  assert.equal(o.requests.length, 2)
})

test('closing a root reloads its report and freezes persisted running children', async t => {
  const o = await world(t)
  const child = o.session.subagents[0]
  child.status = 'running'
  child.done = false
  child.doing = 'Still checking'
  await o.refresh()
  await o.lane('child').click()
  await o.text('Report for child')
  assert.match(await o.lane('child').locator('.stand').innerText(), /running/)
  o.reply = () => report('Archived retained report')
  o.archive()
  await o.refresh()
  await o.text('Archived retained report')
  assert.equal(o.requests.length, 2)
  assert.equal(await o.lane('child').locator('.stand').innerText(), 'did not finish')
  assert.equal(await o.lane('child').locator('.num').innerText(), '1m 00s\n170')
  assert.equal(await o.page.locator('.lane .running, .lane .doing, .lane .sparkl').count(), 0)
})

test('archived reports load from a fresh page and nonterminal statuses never appear active', async t => {
  const statuses = ['running', 'waiting', 'pending', 'idle', 'completed', 'failed', 'killed']
  const children = statuses.map(status => ({ id: status, description: `Child ${status}`, status, done: ['completed', 'failed', 'killed'].includes(status), reportRevision: 1 }))
  const o = await world(t, { archived: true, children })
  for (let i = 0; i < statuses.length; i++) {
    const lane = o.lane(statuses[i])
    assert.equal(await lane.locator('.stand').innerText(), ['did not finish', 'did not finish', 'did not finish', 'did not finish', 'finished', 'failed', 'stopped'][i])
  }
  await o.lane('completed').click()
  await o.text('Report for completed')
  assert.deepEqual(o.requests.map(r => r.url), ['/api/sessions/ses_subagents/subagents/completed'])
  assert.equal(await o.page.locator('.lane .running, .lane .waiting').count(), 0)
})

test('subagent metadata, prompts, report markup and identifier attributes are safely escaped', async t => {
  const payload = '<img src=x onerror="window.injected=true">'
  const id = 'child/" data-injected="yes'
  const children = [{ id, description: payload, type: payload, status: 'completed', done: true, depth: 1, reportRevision: 1 }]
  const o = await world(t, { children })
  const lane = o.page.locator('.lane')
  assert.equal(await lane.getAttribute('data-id'), `sub:ses_subagents/${id}`)
  o.reply = () => ({ status: 404 })
  await lane.click()
  await o.text(missing)
  assert.equal(await o.retry().getAttribute('data-id'), `ses_subagents/${id}`)
  o.reply = () => ({ body: { prompt: payload, report: `${payload}\n\n**Safe emphasis**\n\n[unsafe](javascript:window.injected=true)`, handed: true } })
  await o.retry().click()
  await o.text('Safe emphasis')
  assert.equal(await o.page.locator('.subdoc .said .tx').innerText(), payload)
  assert.match(await o.page.locator('.subdoc .md').innerText(), /<img src=x/)
  assert.equal(await o.page.locator('.subdoc .said button').getAttribute('data-id'), `said:sub:ses_subagents/${id}`)
  assert.equal(await o.page.locator('.lane img, .subdoc img, [data-injected], .subdoc a[href^="javascript:"]').count(), 0)
  assert.equal(await o.page.evaluate(() => window.injected), undefined)
  assert.deepEqual(o.requests.map(r => r.id), [id, id])
})
