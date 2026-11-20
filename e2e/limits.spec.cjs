const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createServer } = require('node:http')
const { join } = require('node:path')
const { test } = require('node:test')
const { pathToFileURL } = require('node:url')
const { chromium } = require('playwright')

const now = Date.parse('2026-11-20T12:00:00Z')
const at = minutes => new Date(now + minutes * 60000).toISOString()
const window = (overrides = {}) => ({ kind: 'five_hour', label: '5h', percentUsed: 42, resetsAt: at(60), windowSeconds: 18000, ...overrides })
const limit = (overrides = {}) => ({ sessionId: 'root-a', sessionLabel: 'Work terminal', provider: 'openai-codex', status: 'ok', stale: false,
  checkedAt: at(0), measuredAt: at(-2), windows: [window()], ...overrides })

async function world(t, records, { legacy = [], mock = false } = {}) {
  const data = { sessions: [], items: [], summarizer: { model: 'fixture' }, limits: legacy }
  if (records !== undefined) data.providerLimits = records
  const calls = []
  const server = createServer((req, res) => {
    calls.push([req.method, req.url])
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
  await page.clock.install({ time: new Date(now) })
  await page.clock.pauseAt(new Date(now))
  await page.goto(mock ? pathToFileURL(join(__dirname, '..', 'index.html')).href : `http://127.0.0.1:${server.address().port}/`)
  if (!mock) await page.waitForFunction(() => document.querySelector('#provider-limits') !== null)
  const strip = page.locator('#provider-limits')
  return { page, strip, data, calls }
}

test('providers and source roots stay distinct even with identical window labels and values', async t => {
  const { page, strip } = await world(t, [
    limit(), limit({ sessionId: 'root-b', sessionLabel: 'Personal terminal' }),
    limit({ provider: 'github-copilot' }), limit({ provider: 'opencode-go' }),
  ])
  assert.deepEqual(await strip.locator('.provider-name').allTextContents(), ['Codex', 'Codex', 'Copilot', 'OpenCode Go'])
  assert.deepEqual(await strip.locator('.window-label').allTextContents(), ['5h', '5h', '5h', '5h'])
  assert.equal(await strip.locator('.meter').count(), 4)
  const titles = await strip.locator('.provider-limit').evaluateAll(nodes => nodes.map(n => n.title))
  assert.match(titles[0], /^Codex via Work terminal\./)
  assert.match(titles[1], /^Codex via Personal terminal\./)
  assert.match(titles[2], /^Copilot via Work terminal\./)
  assert.match(titles[3], /^OpenCode Go via Work terminal\./)
  await strip.locator('.provider-limit').last().scrollIntoViewIfNeeded()
  assert.equal(await strip.locator('.provider-limit').last().isVisible(), true)
  assert.ok(await strip.evaluate(el => el.clientWidth <= 520 && el.scrollWidth > el.clientWidth))
  const scroll = await strip.evaluate(el => el.scrollLeft)
  assert.ok(scroll > 0)
  await page.evaluate(() => refresh(true))
  assert.equal(await strip.evaluate(el => el.scrollLeft), scroll)
  assert.ok(await page.getByRole('button', { name: 'Settings', exact: true }).evaluate(el => el.getBoundingClientRect().right <= innerWidth))
  await page.setViewportSize({ width: 800, height: 900 })
  assert.equal(await strip.locator('.lim').first().isVisible(), true)
  assert.match(await strip.locator('.lim').first().innerText(), /42%/)
})

test('fresh usage shows percentage, reset and measured age; a measured zero is valid', async t => {
  const { strip } = await world(t, [limit({ windows: [window({ percentUsed: 84.6 }), window({ label: 'Weekly', percentUsed: 0, resetsAt: at(1440) })] })])
  const meters = strip.locator('.lim')
  assert.match(await meters.nth(0).innerText(), /85%/)
  assert.equal(await meters.nth(0).locator('.meter i').getAttribute('style'), 'width:85%')
  assert.match(await meters.nth(0).getAttribute('title'), /Measured 2m ago\..*5h: 85% used\. Resets in 1h 0m \(2026-11-20T13:00:00\.000Z\)/)
  assert.match(await meters.nth(1).innerText(), /0%/)
  assert.match(await meters.nth(1).getAttribute('title'), /Resets in 1d 0h/)
  assert.equal(await strip.locator('.stale').count(), 0)
})

test('legacy Claude windows keep their markup and old payloads need no providerLimits', async t => {
  const legacy = [
    { agent: 'claude', kind: 'five_hour', percentUsed: 21, resetsAt: at(60), at: at(-2) },
    { agent: 'claude', kind: 'seven_day', percentUsed: 78, resetsAt: at(1440), at: at(-2) },
    { agent: 'claude', kind: 'spend_limit', percentUsed: 92, at: at(-2) },
  ]
  const { page, strip, data } = await world(t, undefined, { legacy })
  assert.equal(await strip.locator('.provider-limit').count(), 0)
  const claude = page.locator('#tools > .lim')
  assert.deepEqual(await claude.allTextContents(), ['5h21%', '7d78%', 'spend92%'])
  assert.match(await claude.first().getAttribute('title'), /Claude Code: 21% of your 5-hour allowance is used\. It starts over in 1h 0m\. Read 2m ago\./)
  const before = await claude.evaluateAll(nodes => nodes.map(n => n.outerHTML))
  const widths = await claude.evaluateAll(nodes => nodes.map(n => n.getBoundingClientRect().width))
  data.providerLimits = [limit()]
  await page.evaluate(() => refresh(true))
  assert.deepEqual(await claude.evaluateAll(nodes => nodes.map(n => n.outerHTML)), before)
  assert.equal(await strip.locator('.meter').count(), 1)
  data.providerLimits = [limit(), limit({ provider: 'github-copilot', status: 'unavailable' }), limit({ provider: 'opencode-go', status: 'auth_required' })]
  await page.evaluate(() => refresh(true))
  assert.ok(await strip.evaluate(el => el.clientWidth >= 100))
  assert.deepEqual(await claude.evaluateAll(nodes => nodes.map(n => n.getBoundingClientRect().width)), widths)
  assert.ok(await page.getByRole('button', { name: 'Settings', exact: true }).evaluate(el => el.getBoundingClientRect().right <= innerWidth))
  delete data.providerLimits
  await page.evaluate(() => refresh(true))
  assert.equal(await strip.locator('.provider-limit').count(), 0)
  assert.deepEqual(await claude.evaluateAll(nodes => nodes.map(n => n.outerHTML)), before)
})

test('failure chips include provider, source and retry without inventing gauges or login actions', async t => {
  const records = ['unavailable', 'throttled', 'unsupported', 'auth_required'].map((status, i) => limit({
    status, sessionId: `root-${i}`, sessionLabel: `Terminal ${i}`, provider: i % 2 ? 'github-copilot' : 'opencode-go',
    windows: [], measuredAt: undefined, retryAt: at(3),
  }))
  const { strip, calls } = await world(t, records)
  assert.equal(await strip.locator('.provider-limit').count(), 4)
  assert.equal(await strip.locator('.meter').count(), 0)
  assert.doesNotMatch(await strip.innerText(), /\d+%/)
  const messages = ['Unavailable', 'Throttled', 'Usage limits unsupported', 'Reconnect in OpenCode']
  for (let i = 0; i < records.length; i++) {
    const chip = strip.locator('.provider-limit').nth(i)
    assert.ok((await chip.innerText()).includes(messages[i]))
    const title = await chip.getAttribute('title')
    assert.ok(title.includes(`via Terminal ${i}.`))
    assert.match(title, /Retry in 3m \(2026-11-20T12:03:00\.000Z\)/)
  }
  assert.equal(await strip.locator('a, button, [data-act]').count(), 0)
  assert.ok(calls.every(([method, path]) => method === 'GET' && !/auth|login|token/.test(path)))
})

test('retained transient and stale windows show stale measured age, never fresh usage', async t => {
  const { strip } = await world(t, [
    limit({ stale: true }),
    limit({ status: 'unavailable' }),
    limit({ status: 'throttled', retryAt: at(3) }),
  ])
  assert.equal(await strip.locator('.stale').count(), 3)
  assert.equal(await strip.locator('.meter').count(), 3)
  for (const chip of await strip.locator('.provider-limit').all()) {
    assert.match(await chip.innerText(), /42%.*stale · 2m ago/s)
    assert.match(await chip.locator('.lim').getAttribute('title'), /Stale\. Measured 2m ago\./)
  }
  assert.match(await strip.locator('.provider-limit').nth(1).innerText(), /Unavailable/)
  assert.match(await strip.locator('.provider-limit').nth(2).innerText(), /Throttled/)
})

test('auth and unsupported states suppress even retained windows', async t => {
  const { strip } = await world(t, [limit({ status: 'auth_required' }), limit({ status: 'unsupported' })])
  assert.equal(await strip.locator('.meter').count(), 0)
  assert.doesNotMatch(await strip.innerText(), /42%/)
  assert.match(await strip.innerText(), /Reconnect in OpenCode/)
  assert.match(await strip.innerText(), /Usage limits unsupported/)
})

test('expired, reset and invalid windows never become fake zero gauges', async t => {
  const { page, strip } = await world(t, [
    limit({ measuredAt: at(-11), checkedAt: at(0) }),
    limit({ windows: [window({ resetsAt: at(0) }), window({ resetsAt: at(-1) }), window({ resetsAt: 'invalid' })] }),
    limit({ windows: [window({ percentUsed: null }), window({ percentUsed: '0' }), window({ percentUsed: -1 }), window({ percentUsed: undefined }), null] }),
    limit({ windows: [] }),
    limit({ measuredAt: undefined, status: 'throttled', stale: true }),
    limit({ measuredAt: 'invalid', checkedAt: 'invalid' }),
    limit({ measuredAt: 'invalid' }),
    limit({ windows: null }),
  ])
  assert.equal(await strip.locator('.provider-limit').count(), 8)
  assert.equal(await strip.locator('.meter').count(), 0)
  assert.doesNotMatch(await strip.innerText(), /0%|NaN|Infinity/)
  assert.match(await strip.locator('.provider-limit').first().innerText(), /stale · 11m ago/)
  await page.evaluate(() => {
    providerLimits = [providerLimits[2]]
    providerLimits[0].windows = [NaN, Infinity, -Infinity].map(percentUsed => ({ percentUsed, resetsAt: '2026-11-20T13:00:00Z' }))
    render()
  })
  assert.equal(await strip.locator('.meter').count(), 0)
})

test('local time marks fresh payloads stale after five minutes and hides gauges after ten', async t => {
  const { page, strip, calls } = await world(t, [limit({ measuredAt: at(0), windows: [window({ percentUsed: 95 })] })])
  const requests = calls.filter(([, path]) => path === '/api/sessions').length
  assert.equal(await strip.locator('.lim.full').count(), 1)
  await page.clock.runFor(5 * 60000)
  assert.equal(await strip.locator('.stale').count(), 0)
  await page.clock.runFor(60000)
  assert.equal(await strip.locator('.provider-limit.stale').count(), 1)
  assert.equal(await strip.locator('.lim.full, .lim.high').count(), 0)
  assert.equal(await strip.locator('.meter').count(), 1)
  assert.match(await strip.innerText(), /95%.*stale · 6m ago/s)
  assert.match(await strip.locator('.lim').getAttribute('title'), /Stale\. Measured 6m ago\./)
  assert.ok(await strip.locator('.meter i').evaluate(el => getComputedStyle(el).backgroundColor === getComputedStyle(el.closest('.provider-limit')).color))
  await page.clock.runFor(4 * 60000)
  assert.equal(await strip.locator('.meter').count(), 1)
  await page.clock.runFor(15000)
  assert.equal(await strip.locator('.meter').count(), 0)
  assert.match(await strip.innerText(), /No current usage.*stale · 10m ago/s)
  assert.equal(calls.filter(([, path]) => path === '/api/sessions').length, requests)
})

test('local time expires measurements and reset windows without another response or SSE', async t => {
  const { page, strip, calls } = await world(t, [
    limit({ measuredAt: at(-9) }),
    limit({ provider: 'github-copilot', measuredAt: at(0), windows: [window({ resetsAt: at(1) }), window({ label: 'Weekly', resetsAt: at(1440) })] }),
  ])
  const requests = calls.filter(([, path]) => path === '/api/sessions').length
  await page.clock.runFor(60000)
  assert.equal(await strip.locator('.provider-limit').first().locator('.meter').count(), 1)
  const copilot = strip.locator('.provider-limit').nth(1)
  assert.equal(await copilot.locator('.meter').count(), 1)
  assert.match(await copilot.locator('.lim').getAttribute('title'), /Measured 1m ago/)
  await page.clock.runFor(15000)
  assert.equal(await strip.locator('.provider-limit').first().locator('.meter').count(), 0)
  assert.match(await strip.locator('.provider-limit').first().innerText(), /No current usage.*stale · 10m ago/s)
  await page.clock.fastForward(10 * 60000)
  assert.equal(await strip.locator('.meter').count(), 0)
  assert.equal(calls.filter(([, path]) => path === '/api/sessions').length, requests)
})

test('a successful check can date usage without measuredAt; offline data is marked stale', async t => {
  const { page, strip } = await world(t, [limit({ measuredAt: undefined })])
  assert.equal(await strip.locator('.meter').count(), 1)
  assert.match(await strip.locator('.lim').getAttribute('title'), /Checked just now/)
  await page.evaluate(() => { isOffline = true; render() })
  assert.match(await strip.innerText(), /stale · just now/)
  await page.clock.fastForward(11 * 60000)
  assert.equal(await strip.locator('.meter').count(), 0)
})

test('hostile labels remain text and unused account credentials never enter the DOM', async t => {
  const hostile = '\"><img src=x onerror="globalThis.compromised=true"> & \' <script>globalThis.compromised=true</script>'
  const { page, strip } = await world(t, [limit({ sessionLabel: hostile, windows: [window({ label: hostile })],
    accessToken: 'secret-access-token', accountId: 'private-account-id', email: 'private@example.test', raw: { authorization: 'Bearer secret' },
  }), limit({ provider: hostile }), limit({ provider: '__proto__' })], { legacy: [
    { agent: hostile, kind: hostile, percentUsed: 15, resetsAt: 'bad', at: 'bad' },
    { agent: 'claude', kind: 'five_hour', percentUsed: null },
  ] })
  assert.equal(await strip.locator('.provider-limit').count(), 1)
  assert.equal(await strip.locator('.window-label').textContent(), hostile)
  assert.ok((await strip.locator('.lim').getAttribute('title')).includes(`via ${hostile}.`))
  assert.equal(await page.locator('#tools img, #tools script').count(), 0)
  assert.equal(await page.evaluate(() => globalThis.compromised), undefined)
  assert.doesNotMatch(await page.locator('body').innerHTML(), /secret-access-token|private-account-id|private@example\.test|Bearer secret/)
  assert.equal(await page.locator('#tools > .lim').count(), 1)
  assert.doesNotMatch(await page.locator('#tools > .lim').getAttribute('title'), /NaN/)
})

test('file demo still works without a service or provider limits', async t => {
  const { page, strip, calls } = await world(t, undefined, { mock: true })
  assert.equal(await strip.count(), 0)
  assert.equal(await page.getByLabel('Demo controls').count(), 1)
  await page.clock.fastForward(11 * 60000)
  assert.equal(await page.getByLabel('Demo controls').count(), 1)
  assert.deepEqual(calls, [])
})
