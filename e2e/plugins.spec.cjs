const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createServer } = require('node:http')
const { join } = require('node:path')
const { test } = require('node:test')
const { chromium } = require('playwright')

async function world(t, plugin, sessions = []) {
  const data = { sessions, items: [], plugins: { claude: { installed: true, enabled: true }, opencode: plugin }, summarizer: { off: true } }
  const calls = []
  let failure = ''
  const server = createServer(async (req, res) => {
    if (req.url === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write(': connected\n\n')
    } else if (req.url === '/api/sessions') {
      res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(data))
    } else if (req.url.startsWith('/api/plugins/')) {
      calls.push([req.method, req.url])
      if (failure) { res.writeHead(502).end(failure); return }
      data.plugins.opencode = req.method === 'DELETE' ? { installed: false } : { installed: true, enabled: true, version: '0.2.0', source: plugin.source || 'managed', path: plugin.path || '/home/user/.slorp/plugins/opencode', revision: 'new' }
      res.writeHead(204).end()
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
  await page.goto(`http://127.0.0.1:${server.address().port}/#settings`)
  await page.getByRole('button', { name: /^Integrations/ }).click()
  const card = page.locator('[data-plugin="opencode"]')
  await card.waitFor()
  return { page, card, calls, data, fail: message => { failure = message } }
}

test('Settings installs the bundled plugin and explains automatic loading', async t => {
  const { page, card, calls } = await world(t, { installed: false })
  await card.getByRole('button', { name: 'Install', exact: true }).click()
  await card.getByRole('button', { name: 'Uninstall', exact: true }).waitFor()
  assert.deepEqual(calls, [['POST', '/api/plugins/opencode']])
  assert.match(await card.innerText(), /installed/i)
  assert.match(await card.innerText(), /bundled with Slorp/)
  assert.match(await page.locator('#toast').innerText(), /loads the plugin automatically/)
})

test('a local package offers companion setup without managed update or removal', async t => {
  const { card, calls } = await world(t, { installed: true, source: 'local', needsSetup: true, path: '/dev/slorp/plugin', version: '0.2.0' })
  assert.equal(await card.getByRole('button', { name: 'Uninstall', exact: true }).count(), 0)
  await card.getByRole('button', { name: 'Finish Setup' }).click()
  await card.getByRole('button', { name: 'Finish Setup' }).waitFor({ state: 'hidden' })
  assert.deepEqual(calls, [['POST', '/api/plugins/opencode']])
})

test('managed updates distinguish an old running bridge from a reloaded one', async t => {
  const at = new Date().toISOString()
  const session = { id: 's', agent: 'opencode', cwd: '/work', status: 'idle', label: 'session', startedAt: at, activeAt: at, canReply: true, pluginRevision: 'old', pluginVersion: '0.2.0', pluginTui: true, used: {} }
  const { page, card, calls, data } = await world(t, { installed: true, source: 'managed', revision: 'old', updateAvailable: true }, [session])
  await card.getByRole('button', { name: 'Update', exact: true }).click()
  await page.waitForFunction(() => document.querySelector('[data-plugin="opencode"]').textContent.includes('reload needed'))
  assert.deepEqual(calls, [['POST', '/api/plugins/opencode/update']])
  data.sessions[0].pluginRevision = 'new'
  await page.evaluate(() => refresh(true))
  assert.match(await card.innerText(), /reporting/i)
  await card.getByRole('button', { name: 'Uninstall', exact: true }).click()
  await page.locator('#toast').getByRole('button', { name: 'Uninstall', exact: true }).click()
  await card.getByRole('button', { name: 'Install', exact: true }).waitFor()
  assert.deepEqual(calls.at(-1), ['DELETE', '/api/plugins/opencode'])
})

test('installation errors remain visible and allow retry', async t => {
  const { page, card, fail, calls } = await world(t, { installed: false })
  fail('OpenCode rejected this configuration')
  await card.getByRole('button', { name: 'Install', exact: true }).click()
  await page.waitForFunction(() => document.querySelector('#toast').innerText.includes('rejected'))
  assert.equal(await card.getByRole('button', { name: 'Install', exact: true }).isEnabled(), true)
  assert.equal(calls.length, 1)
})
