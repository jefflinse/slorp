const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createServer } = require('node:http')
const { join } = require('node:path')
const { test } = require('node:test')
const { chromium } = require('playwright')

async function world(t, release) {
  const ready = { installed: true, enabled: true }
  const data = { sessions: [], items: [], plugins: { claude: ready, opencode: ready }, summarizer: { off: true }, release }
  const calls = []
  let failure = ''
  const server = createServer(async (req, res) => {
    if (req.url === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write(': connected\n\n')
    } else if (req.url === '/api/sessions') {
      res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(data))
    } else if (req.url === '/api/upgrade') {
      calls.push([req.method, req.url])
      if (req.headers['content-type'] !== 'application/json') res.writeHead(415).end('content type must be application/json')
      else if (failure) res.writeHead(502).end(failure)
      else res.writeHead(204).end()
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
  await page.goto(`http://127.0.0.1:${server.address().port}/`)
  await page.waitForFunction(() => typeof live !== 'undefined' && live !== null)
  return { page, notice: page.locator('#notice'), calls, data, fail: message => { failure = message } }
}

test('a newer release is offered, installed on one click, and the page loads again from it', async t => {
  const { page, notice, calls, data } = await world(t, { version: 'v0.2.0', latest: 'v0.3.0' })
  assert.match(await notice.innerText(), /Slorp v0\.3\.0 is out\. This is v0\.2\.0\./)
  await page.evaluate(() => { window.loadedBefore = true })
  await notice.getByRole('button', { name: 'Upgrade…' }).click()
  const dialog = page.getByRole('alertdialog', { name: 'Upgrade Slorp' })
  assert.match(await dialog.innerText(), /Upgrade Slorp to v0\.3\.0\?[\s\S]*restarts[\s\S]*OpenCode sessions Slorp started are stopped/)
  assert.deepEqual(calls, [])
  await dialog.getByRole('button', { name: 'Upgrade and restart' }).click()
  await page.waitForFunction(() => document.querySelector('#toast').innerText.includes('restarting'))
  assert.deepEqual(calls, [['POST', '/api/upgrade']])
  assert.equal(await dialog.count(), 0)
  assert.equal(await notice.getByRole('button', { name: 'Upgrading…' }).isDisabled(), true)
  data.release = { version: 'v0.3.0' }
  await page.evaluate(() => { refresh(true) })
  await page.waitForFunction(() => !window.loadedBefore && typeof live !== 'undefined' && live !== null)
  assert.equal(await notice.innerText(), '')
})

test('a failed upgrade says why and can be tried again', async t => {
  const { page, notice, calls, fail } = await world(t, { version: 'v0.2.0', latest: 'v0.3.0' })
  fail('build failed: no space left on device')
  await notice.getByRole('button', { name: 'Upgrade…' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Upgrade and restart' }).click()
  await page.waitForFunction(() => document.querySelector('#toast').innerText.includes('no space left'))
  assert.equal(await notice.getByRole('button', { name: 'Upgrade…' }).isEnabled(), true)
  assert.deepEqual(calls, [['POST', '/api/upgrade']])
})

test('an upgrade that is not confirmed installs nothing', async t => {
  const { page, notice, calls } = await world(t, { version: 'v0.2.0', latest: 'v0.3.0' })
  const dialog = page.getByRole('alertdialog', { name: 'Upgrade Slorp' })
  await notice.getByRole('button', { name: 'Upgrade…' }).click()
  await dialog.getByRole('button', { name: 'Cancel' }).click()
  await dialog.waitFor({ state: 'detached' })
  await notice.getByRole('button', { name: 'Upgrade…' }).click()
  await dialog.waitFor()
  await page.keyboard.press('Escape')
  await dialog.waitFor({ state: 'detached' })
  assert.equal(await notice.getByRole('button', { name: 'Upgrade…' }).isEnabled(), true)
  assert.deepEqual(calls, [])
})

test('a release Slorp cannot install itself is explained in Settings, with no button to install it', async t => {
  const { page, notice, calls } = await world(t, { version: 'v0.2.0', latest: 'v0.3.0', manual: 'Slorp runs from /opt/bin, which is not where Go installs commands (/home/user/go/bin)' })
  assert.equal(await page.getByRole('button', { name: /^Upgrade/ }).count(), 0)
  await notice.getByRole('button', { name: 'How to upgrade' }).click()
  const row = page.locator('.card.set', { hasText: 'v0.3.0 is out' })
  await row.waitFor()
  assert.match(await row.innerText(), /runs from \/opt\/bin.*upgrade it the way it was installed/)
  assert.equal(await page.getByRole('button', { name: /^Upgrade/ }).count(), 0)
  assert.deepEqual(calls, [])
})

test('a dismissed release stays out of the way but can still be installed from Settings', async t => {
  const { page, notice, calls } = await world(t, { version: 'v0.2.0', latest: 'v0.3.0' })
  await notice.getByRole('button', { name: 'Dismiss' }).click()
  await page.waitForFunction(() => document.querySelector('#notice').innerText === '')
  await page.evaluate(() => refresh(true))
  assert.equal(await notice.innerText(), '')
  await page.goto(new URL('/#settings', page.url()).href)
  const row = page.locator('.card.set', { hasText: 'v0.3.0 is out' })
  await row.getByRole('button', { name: 'Upgrade…' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Upgrade and restart' }).click()
  await page.waitForFunction(() => document.querySelector('#toast').innerText.includes('restarting'))
  assert.deepEqual(calls, [['POST', '/api/upgrade']])
})

test('a release with nothing newer, and a development build, offer nothing', async t => {
  const { page, notice, data } = await world(t, { version: 'v0.2.0' })
  assert.equal(await notice.innerText(), '')
  await page.goto(new URL('/#settings', page.url()).href)
  await page.locator('.card.set', { hasText: 'This is v0.2.0. No newer release has been found.' }).waitFor()
  data.release = {}
  await page.evaluate(() => refresh(true))
  await page.locator('.card.set', { hasText: 'development build' }).waitFor()
  assert.equal(await page.getByRole('button', { name: /^Upgrade/ }).count(), 0)
})
