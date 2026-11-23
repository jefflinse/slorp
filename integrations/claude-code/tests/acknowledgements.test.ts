import type { On } from 'claude-code'
import { describe, expect, mock, test } from 'claude-code/testing'

type Ack = { id: string; state: string }

/**
 * Slorp as the bridge sees it: each queued reply handed out by one poll only,
 * and each acknowledgement answered as answer, called with how many came
 * before it, says. A thrown answer is a request that never arrived.
 */
function slorp(on: On, queued: { id: string; text: string }[], answer: (n: number) => number) {
  const acks: Ack[] = []
  const tried: Ack[] = []
  on('http.fetch', ($, e) => {
    const path = new URL(e.url).pathname
    const body = JSON.parse(String(e.init?.body ?? '{}'))
    const respond = (status: number, value?: object) => ({ value: { status, ok: status < 300, headers: {}, text: value ? JSON.stringify(value) : '' } })
    if (path === '/api/bridge/poll') return respond(200, { replies: queued.splice(0) })
    if (path === '/api/bridge/ack') {
      tried.push(body)
      const status = answer(tried.length - 1)
      if (status < 300) acks.push(body)
      return respond(status)
    }
    return respond(204)
  })
  on('session.id', () => ({ value: 'ses_1' }))
  on('agent.list', () => ({ value: [] }))
  on('session.start', ($, e) => ({ cwd: e.cwd }))
  return { acks, tried }
}

const notTaken: [string, () => number][] = [
  ['never arrived', () => { throw new Error('connection refused') }],
  ['timed out', () => 408],
  ['too many', () => 429],
  ['failed', () => 503],
]

describe('acknowledging a reply', () => {
  for (const [why, first] of notTaken) test(`one Slorp did not take (${why}) is told again on the next poll, and the reply is not submitted again`, async ($, on) => {
    const clock = mock.clock(on)
    mock.env(on, {})
    const { acks, tried } = slorp(on, [{ id: 'r1', text: 'carry on' }], n => (n === 0 ? first() : 204))
    const submitted: string[] = []
    on('prompt.submit', ($, e) => {
      submitted.push(e.text)
      return { text: e.text }
    })
    await $.session.start({ cwd: '/w', surface: null, isInteractive: false })
    await clock.advance(2000)
    expect(tried).toHaveLength(1)
    expect(acks).toHaveLength(0)
    await clock.advance(2000)
    expect(acks).toEqual([{ id: 'r1', state: 'delivered' }])
    await clock.advance(2000)
    expect(acks).toHaveLength(1)
    expect(submitted).toEqual(['carry on'])
  })

  test('a reply the session dropped is told again as dropped', async ($, on) => {
    const clock = mock.clock(on)
    mock.env(on, {})
    const { acks } = slorp(on, [{ id: 'r1', text: 'carry on' }], n => (n === 0 ? 503 : 204))
    on('prompt.submit', () => ({ drop: 'a Stop stands' }))
    await $.session.start({ cwd: '/w', surface: null, isInteractive: false })
    await clock.advance(2000)
    expect(acks).toHaveLength(0)
    await clock.advance(2000)
    expect(acks).toEqual([{ id: 'r1', state: 'dropped' }])
  })

  test('one Slorp refuses is not told again', async ($, on) => {
    const clock = mock.clock(on)
    mock.env(on, {})
    const { tried } = slorp(on, [{ id: 'r1', text: 'carry on' }], () => 409)
    on('prompt.submit', ($, e) => ({ text: e.text }))
    await $.session.start({ cwd: '/w', surface: null, isInteractive: false })
    await clock.advance(2000)
    await clock.advance(2000)
    await clock.advance(2000)
    expect(tried).toHaveLength(1)
  })
})
