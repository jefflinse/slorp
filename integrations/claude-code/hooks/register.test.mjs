import assert from 'node:assert/strict'
import { test } from 'node:test'
import { register } from './register.ts'

const cases = [
  { name: 'default', tool: 'Bash', input: { command: 'echo hello' }, canAnswer: true },
  { name: 'empty pattern', pattern: '', tool: 'Bash', input: { command: 'echo hello' }, canAnswer: true },
  { name: 'nonmatching command', pattern: '^true$', tool: 'Bash', input: { command: 'echo hello' }, canAnswer: true },
  { name: 'matching command', pattern: '^echo ', tool: 'Bash', input: { command: 'echo hello' }, canAnswer: false },
  { name: 'tool name', pattern: '^Bash$', tool: 'Bash', input: { command: 'true' }, canAnswer: false },
  { name: 'literal trailing space', pattern: '^true ', tool: 'Bash', input: { command: 'true' }, canAnswer: true },
  { name: 'whitespace-only pattern', pattern: ' ', tool: 'Bash', input: { command: 'echo hello' }, canAnswer: false },
  { name: 'JSON input', pattern: 'example.txt', tool: 'Read', input: { file_path: 'example.txt' }, canAnswer: false },
  { name: 'invalid pattern', pattern: '[', tool: 'Bash', input: { command: 'true' }, canAnswer: false },
]

for (const { name, pattern, tool, input, canAnswer } of cases) {
  test(`terminalOnly: ${name}`, async () => {
    const handlers = new Map()
    register((event, handler) => {
      handlers.set(event, handler)
      return { catch() {} }
    }, { url: 'http://slorp.invalid', terminalOnly: pattern })

    const calls = []
    const engine = {
      env: { get: async () => undefined },
      session: { id: async () => 'test', usage: async () => ({ context: { window: 0 }, rateLimits: [] }) },
      clock: { every() {} },
      http: { fetch: async (url, request) => {
        const path = new URL(url).pathname
        calls.push({ path, body: JSON.parse(request.body) })
        const response = path.endsWith('/wait') ? { state: 'allow' } : { id: 'prompt', answerable: true }
        return { ok: true, text: JSON.stringify(response) }
      } },
    }
    const next = Object.assign(event => event, { signal: new AbortController().signal })
    await handlers.get('session.start')(engine, { cwd: '/project' }, next)
    const event = { tool_name: tool, tool_input: input }
    const result = await handlers.get('classic.PermissionRequest')(engine, event, next)

    const expectedPaths = ['/api/bridge/permission']
    if (canAnswer) expectedPaths.push('/api/bridge/permission/wait')
    assert.deepEqual(calls.map(call => call.path), expectedPaths)
    assert.equal(calls[0].body.canAnswer, canAnswer)
    assert.deepEqual(result, canAnswer ? { decision: { behavior: 'allow' } } : event)
  })
}
