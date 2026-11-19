const assert = require('node:assert/strict')
const { readFileSync } = require('node:fs')
const { createServer } = require('node:http')
const { join } = require('node:path')
const { test } = require('node:test')
const { chromium } = require('playwright')

const pageHTML = readFileSync(join(__dirname, '..', 'index.html'))

async function world(t, questions, ordered = true) {
  const at = new Date().toISOString()
  const session = {
    id: 'ses_browser', agent: ordered ? 'opencode' : 'claude', kind: 'interactive',
    name: 'Question test', label: 'Question test', cwd: '/work/project', branch: 'main',
    status: 'waiting', startedAt: at, activeAt: at, lastPrompt: 'Help me choose', lastReply: '',
    effort: '', role: '', canReply: true, used: { in: 0, out: 0, cacheRead: 0, cacheWrite: 0 },
    pending: { id: 'ask_browser', tool: 'question', question: true, ordered, answerable: true, input: JSON.stringify({ questions }), at },
  }
  const received = []
  const server = createServer(async (req, res) => {
    if (req.url === '/api/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' })
      res.write(': connected\n\n')
    } else if (req.url === '/api/sessions') {
      res.setHeader('content-type', 'application/json')
      res.end(JSON.stringify({ sessions: [session], items: [], summarizer: { off: true } }))
    } else if (req.url === '/api/permissions/ask_browser') {
      let body = ''
      for await (const chunk of req) body += chunk
      received.push(JSON.parse(body))
      session.pending = null
      session.status = 'working'
      res.writeHead(204).end()
    } else {
      res.setHeader('content-type', 'text/html')
      res.end(pageHTML)
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
  await page.goto(`http://127.0.0.1:${server.address().port}/#sessions`)
  await page.getByRole('button', { name: 'Send answer', exact: true }).waitFor()
  return { page, received }
}

test('custom answers survive rerenders, and choosing a single option clears custom text', async t => {
  const { page, received } = await world(t, [
    { question: 'Which?', custom: true, options: [{ label: 'Postgres', description: 'A database' }] },
  ])
  const input = page.locator('[data-question-custom]')
  await input.pressSequentially('SQLite')
  assert.equal(await input.inputValue(), 'SQLite')
  assert.equal(await input.evaluate(el => el.selectionStart), 6)
  await page.evaluate(() => refresh(true))
  assert.equal(await input.inputValue(), 'SQLite')
  await page.getByRole('button', { name: /Postgres/ }).click()
  assert.equal(await input.inputValue(), '')
  await page.getByRole('button', { name: 'Send answer', exact: true }).click()
  await page.waitForFunction(() => !document.querySelector('[data-act="liveAnswer"]'))
  assert.deepEqual(received, [{ selections: [['Postgres']] }])
})

test('multi-select, comma labels, repeated questions and custom text reach the API intact', async t => {
  const { page, received } = await world(t, [
    { question: 'Choose', custom: true, multiSelect: true, options: [{ label: 'A, B' }, { label: 'C' }] },
    { question: 'Choose', custom: true, options: [] },
  ])
  assert.equal(await page.getByRole('button', { name: 'Send answer', exact: true }).isDisabled(), true)
  await page.getByRole('button', { name: /A, B/ }).click()
  await page.locator('[data-question-custom]').nth(0).fill('D')
  await page.locator('[data-question-custom]').nth(1).fill('my own answer')
  await page.getByRole('button', { name: 'Send answer', exact: true }).click()
  await page.waitForFunction(() => !document.querySelector('[data-act="liveAnswer"]'))
  assert.deepEqual(received, [{ selections: [['A, B', 'D'], ['my own answer']] }])
})

test('questions can be dismissed without selecting an answer', async t => {
  const { page, received } = await world(t, [{ question: 'Which?', options: [{ label: 'A' }] }])
  await page.getByRole('button', { name: 'Dismiss', exact: true }).click()
  await page.waitForFunction(() => !document.querySelector('[data-act="liveAnswer"]'))
  assert.deepEqual(received, [{ behavior: 'deny', message: '' }])
})

test('Claude question answers keep their existing wire format', async t => {
  const { page, received } = await world(t, [{ question: 'Which?', options: [{ label: 'Postgres' }] }], false)
  assert.equal(await page.locator('[data-question-custom]').count(), 0)
  await page.getByRole('button', { name: /Postgres/ }).click()
  await page.getByRole('button', { name: 'Send answer', exact: true }).click()
  await page.waitForFunction(() => !document.querySelector('[data-act="liveAnswer"]'))
  assert.deepEqual(received, [{ answers: { 'Which?': ['Postgres'] } }])
})
