const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createServer } = require('node:http')
const { join } = require('node:path')
const { test } = require('node:test')
const { chromium } = require('playwright')

/* Slorp's own instructions, as the service gives them. */
const OWN = { describe: 'You write entries for a dashboard.\n\ntitle: at most 9 words.', lookup: 'Decide what to look up.', file: 'You file sessions under efforts.',
  role: 'You sort sessions by kind.', note: 'You write the status of one effort.', brief: 'You brief a developer on one pull request.',
  wrapup: 'You write end-of-day notes.', backfill: 'Say where each exchange left things.' }

/* A service that keeps what the local model is told as Slorp's does: trimmed, each save changing only what it
   names, and nothing kept for a task left empty or given Slorp's own instructions. */
async function world(t, { defaults = true } = {}) {
  const ready = { installed: true, enabled: true }
  const data = { sessions: [], items: [], efforts: [], knowledge: { questions: [], context: [], routing: [], roles: [], workspaces: [], profiles: [] }, journal: [],
    plugins: { claude: ready, opencode: ready }, summarizer: { model: 'lab/fixture-model' }, capabilities: {}, actions: [],
    settings: { answerPermissions: true, model: {}, prompts: { global: '', tasks: {} } } }
  const world = { data, saves: [], reads: 0, defaults }
  const server = createServer((req, res) => {
    if (req.url === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write(': connected\n\n')
    } else if (req.url === '/api/sessions' && req.method === 'GET') {
      res.setHeader('content-type', 'application/json'); res.end(JSON.stringify(data))
    } else if (req.url === '/api/prompts') {
      world.reads++
      if (!world.defaults) { res.writeHead(500); res.end('no'); return }
      res.setHeader('content-type', 'application/json'); res.end(JSON.stringify({ defaults: OWN }))
    } else if (req.url === '/api/settings' && req.method === 'POST') {
      let body = ''
      req.on('data', chunk => { body += chunk })
      req.on('end', () => {
        const said = JSON.parse(body)
        world.saves.push(said)
        const kept = data.settings.prompts
        if (said.prompts?.global != null) kept.global = said.prompts.global.trim()
        for (const [task, text] of Object.entries(said.prompts?.tasks || {})) {
          if (text.trim() && text.trim() !== OWN[task]) kept.tasks[task] = text.trim()
          else delete kept.tasks[task]
        }
        res.writeHead(204); res.end()
      })
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
  await page.goto(`http://127.0.0.1:${server.address().port}/`)
  await page.waitForFunction(() => typeof live !== 'undefined' && live !== null)
  await page.getByRole('button', { name: 'Settings', exact: true }).click()
  const nav = page.locator('#main .kbnav').getByRole('button', { name: /^Prompts/ })
  await nav.click()
  const row = id => page.locator(`#main .card[data-prompt="${id}"]`)
  const box = id => page.locator(`#main form[data-id="${id}"] textarea`)
  const save = id => page.locator(`#main form[data-id="${id}"]`).getByRole('button', { name: 'Save' }).click()
  const saved = n => page.waitForFunction(n => typeof reads !== 'undefined' && document.querySelectorAll('#main .card.prompt .tag').length === n, n)
  return Object.assign(world, { page, nav, row, box, save, saved })
}

test('a task goes by Slorp\'s own instructions until others are saved for it, and back to them when reset', async t => {
  const w = await world(t)
  const { page, row, box } = w
  assert.equal(await page.locator('#main .card.prompt').count(), 9)
  assert.match(await page.locator('#main').innerText(), /Before Every Task[\s\S]*Session Summaries[\s\S]*Lookups[\s\S]*Filing[\s\S]*Role Guesses[\s\S]*Effort Notes[\s\S]*Pull Request Briefs[\s\S]*Wrap-ups[\s\S]*Earlier History/)
  assert.equal(await page.locator('#main .card.prompt .tag').count(), 0)
  assert.equal(w.reads, 0, 'Slorp\'s own instructions are not read until a task is opened')

  await row('describe').getByRole('button', { name: 'Edit' }).click()
  await box('describe').waitFor()
  assert.equal(await box('describe').inputValue(), OWN.describe)
  assert.match(await page.locator('#main form[data-id="describe"]').innerText(), /Slorp's own/)
  assert.equal(await page.getByRole('button', { name: 'Reset to Default' }).count(), 0)

  await box('describe').fill('  Describe it in one terse line.\n')
  await w.save('describe')
  await w.saved(1)
  assert.deepEqual(w.saves, [{ prompts: { tasks: { describe: '  Describe it in one terse line.\n' } } }])
  assert.match(await row('describe').innerText(), /Customized/i)
  assert.equal(await box('describe').inputValue(), 'Describe it in one terse line.', 'the box shows what the service kept')
  assert.match(await page.locator('#main form[data-id="describe"]').innerText(), /Yours, in place of Slorp's own/)
  assert.match(await w.nav.innerText(), /Prompts\s*1/)

  await row('note').getByRole('button', { name: 'Edit' }).click()
  assert.equal(await box('note').inputValue(), OWN.note)
  assert.equal(await box('describe').count(), 0, 'one task is open at a time')
  assert.equal(w.reads, 1, 'Slorp\'s own instructions are read once')
  await row('note').getByRole('button', { name: 'Done' }).click()
  assert.equal(await box('note').count(), 0)

  await row('describe').getByRole('button', { name: 'Edit' }).click()
  await box('describe').fill('Half-typed and never saved')
  await page.getByRole('button', { name: 'Reset to Default' }).click()
  await w.saved(0)
  assert.deepEqual(w.saves[1], { prompts: { tasks: { describe: '' } } })
  assert.equal(await box('describe').inputValue(), OWN.describe)
  assert.equal(await page.getByRole('button', { name: 'Reset to Default' }).count(), 0)
  assert.doesNotMatch(await w.nav.innerText(), /\d/)
})

test('what goes ahead of every task is saved on its own, and keeps what is typed while a task is saved', async t => {
  const w = await world(t)
  const { page, row, box } = w
  assert.equal(await box('global').inputValue(), '')
  await box('global').fill('Write in British English.')
  await row('file').getByRole('button', { name: 'Edit' }).click()
  await box('file').fill('File it carefully.')
  await page.keyboard.press('Control+Enter')
  await w.saved(1)
  assert.deepEqual(w.saves, [{ prompts: { tasks: { file: 'File it carefully.' } } }])
  assert.equal(await box('global').inputValue(), 'Write in British English.', 'saving a task leaves what is typed elsewhere')

  await w.save('global')
  await w.saved(2)
  assert.deepEqual(w.saves[1], { prompts: { global: 'Write in British English.' } })
  assert.match(await row('global').innerText(), /Customized/i)
  assert.match(await w.nav.innerText(), /Prompts\s*2/)

  await box('global').fill('')
  await w.save('global')
  await w.saved(1)
  assert.deepEqual(w.data.settings.prompts, { global: '', tasks: { file: 'File it carefully.' } })
})

test('a task whose own instructions cannot be read says so and can be asked for again', async t => {
  const w = await world(t, { defaults: false })
  const { page, row, box } = w
  await row('wrapup').getByRole('button', { name: 'Edit' }).click()
  const retry = page.getByRole('button', { name: 'Retry' })
  await retry.waitFor()
  assert.equal(await box('wrapup').count(), 0)
  assert.equal(w.reads, 1, 'a read that failed is not tried again unasked')
  w.defaults = true
  await retry.click()
  await box('wrapup').waitFor()
  assert.equal(await box('wrapup').inputValue(), OWN.wrapup)
})
