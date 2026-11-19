import assert from 'node:assert/strict'
import { test } from 'node:test'
import { BroadcastChannel } from 'node:worker_threads'
import plugin from './tui.ts'

test('the TUI announces startup and selection changes and stops on disposal', async t => {
  const heard: unknown[] = []
  const channel = new BroadcastChannel('slorp.opencode.presence')
  channel.onmessage = event => heard.push(event.data)
  t.after(() => channel.close())
  t.mock.timers.enable({ apis: ['setInterval'] })
  const api = {
    route: { current: { name: 'session', params: { sessionID: 'first' } } },
    state: { path: { directory: '/work/proj' } },
    lifecycle: { onDispose: (fn: () => void) => { dispose = fn } },
  }
  let dispose = () => {}
  await plugin.tui(api as any, undefined, {} as any)
  t.after(() => dispose())
  const settle = async () => { for (let i = 0; i < 20; i++) await new Promise(resolve => setImmediate(resolve)) }
  await settle()
  assert.deepEqual(heard, [{ directory: '/work/proj', sessionId: 'first' }])
  api.route.current.params.sessionID = 'second'
  t.mock.timers.tick(2000)
  await settle()
  assert.deepEqual(heard.at(-1), { directory: '/work/proj', sessionId: 'second' })
  api.route.current.name = 'home'
  t.mock.timers.tick(2000)
  await settle()
  assert.equal(heard.length, 3)
  assert.deepEqual(heard.at(-1), { directory: '/work/proj' })
  dispose()
  api.route.current.name = 'session'
  t.mock.timers.tick(2000)
  await settle()
  assert.equal(heard.length, 3)
})
