import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { test } from 'node:test'
import { PRESENCE_PREFIX, PRESENCE_TTL, parsePresence, PresenceTracker, type Presence } from './presence.ts'

const payload = (): Presence => ({ version: 1, terminalId: randomUUID(), sequence: 1, directory: '/server/project', sessionId: 'ses_root' })
const parse = (value: unknown) => parsePresence(PRESENCE_PREFIX + JSON.stringify(value))

test('presence accepts only bounded versioned markers with safe IDs and server directories', () => {
  const p = payload()
  assert.deepEqual(parse(p), p)
  assert.ok(parse({ ...p, directory: 'C:\\work\\project', projectID: 'global', workspaceID: 'wrk_1' }))
  assert.ok(parse({ ...p, sessionId: undefined }))
  for (const command of [undefined, '', 'session.new', 'slorp.presence.v2:{}', PRESENCE_PREFIX + '{', PRESENCE_PREFIX + 'null', PRESENCE_PREFIX + '[]']) {
    assert.equal(parsePresence(command), undefined)
  }
  for (const change of [
    { version: 2 }, { version: '1' }, { terminalId: 'one' }, { terminalId: randomUUID() + 'extra' },
    ...[0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1, '1', null].map(sequence => ({ sequence })),
    ...['', '.', 'relative/project', '/bad\npath', '/' + 'x'.repeat(2048)].map(directory => ({ directory })),
    ...['../other', '.', 'white space', 'bad\\path', 'x'.repeat(257), null].map(sessionId => ({ sessionId })),
    { projectID: {} }, { workspaceID: [] }, { unexpected: true },
  ]) assert.equal(parse({ ...p, ...change }), undefined, JSON.stringify(change))
  assert.equal(parsePresence(PRESENCE_PREFIX + JSON.stringify(p) + ' '.repeat(4096)), undefined)
  assert.equal(parse({ ...p, directory: '/' + 'é'.repeat(1024) }), undefined, 'directory bound is bytes, not code units')
})

test('sequences reject duplicates and out-of-order markers without extending leases', t => {
  t.mock.timers.enable({ apis: ['Date'] })
  const tracker = new PresenceTracker()
  const p = payload()
  const first = tracker.accept({ ...p, sequence: 3 })!
  tracker.confirm(first, p.sessionId)
  t.mock.timers.tick(9000)
  assert.equal(tracker.accept({ ...p, sequence: 2 }), undefined)
  assert.equal(tracker.accept({ ...p, sequence: 3 }), undefined)
  assert.equal(tracker.fresh(p.sessionId!), true)
  t.mock.timers.tick(1000)
  assert.equal(tracker.current(first), false)
  assert.equal(tracker.fresh(p.sessionId!), false)
  assert.equal(tracker.accept({ ...p, sequence: 1 }), undefined, 'expired high-water marks survive while capacity allows')
  const second = tracker.accept({ ...p, sequence: 4 })!
  assert.notEqual(second, first, 'an expired selection cannot be renewed in place')
  assert.equal(first.controller.signal.aborted, true)
  assert.equal(tracker.current(second), true)
  assert.equal(tracker.current(first), false)
  tracker.confirm(first, p.sessionId)
  assert.equal(tracker.fresh(p.sessionId!), false, 'expired selection continuations cannot confirm a replacement')
  tracker.dispose()
  assert.equal(second.controller.signal.aborted, true)
  assert.equal(tracker.current(second), false)
})

test('256 terminals are bounded without evicting live terminals, and expired slots can be reclaimed', t => {
  t.mock.timers.enable({ apis: ['Date'] })
  const tracker = new PresenceTracker()
  const leases = Array.from({ length: 256 }, () => tracker.accept(payload())!)
  assert.ok(leases.every(Boolean))
  assert.equal(tracker.accept(payload()), undefined)
  assert.ok(leases.every(lease => tracker.current(lease)))
  t.mock.timers.tick(PRESENCE_TTL)
  const next = tracker.accept(payload())!
  assert.ok(next)
  assert.equal(leases[0]!.controller.signal.aborted, true)
  assert.ok(leases.every(lease => !tracker.current(lease)))
  tracker.dispose()
})

test('independent terminal leases keep other roots and home freshness independent', () => {
  const tracker = new PresenceTracker()
  const first = tracker.accept(payload())!
  const second = tracker.accept(payload())!
  tracker.confirm(first, 'ses_first')
  tracker.confirm(second, 'ses_second')
  tracker.accept({ ...first.payload, sequence: 2, sessionId: 'ses_changed' })
  assert.equal(tracker.fresh('ses_first'), false)
  assert.equal(tracker.fresh('ses_second'), true)
  const home = tracker.accept({ ...first.payload, sequence: 3, sessionId: undefined })!
  tracker.confirm(home)
  assert.equal(tracker.fresh('ses_first'), true)
  assert.equal(tracker.fresh('ses_second'), true)
  tracker.dispose()
})

test('unconfirmed repeated heartbeats retain but do not extend previously verified companion freshness', t => {
  t.mock.timers.enable({ apis: ['Date'] })
  const tracker = new PresenceTracker()
  const p = payload()
  const first = tracker.accept(p)!
  tracker.confirm(first, p.sessionId)
  t.mock.timers.tick(9000)
  const next = tracker.accept({ ...p, sequence: 2 })!
  assert.equal(next, first)
  assert.equal(tracker.current(first), true)
  assert.equal(first.controller.signal.aborted, false)
  assert.equal(first.payload.sequence, 2)
  assert.equal(first.expires, 19000)
  assert.equal(first.freshUntil, 10000)
  assert.equal(tracker.accept({ ...p, sequence: 1 }), undefined)
  assert.equal(tracker.accept({ ...p, sequence: 2 }), undefined)
  assert.equal(tracker.fresh(p.sessionId!), true)
  t.mock.timers.tick(1000)
  assert.equal(tracker.fresh(p.sessionId!), false)
  tracker.confirm(first, p.sessionId)
  assert.equal(tracker.fresh(p.sessionId!), true, 'ongoing validation can confirm the renewed selection')
  assert.equal(first.freshUntil, 19000)
  tracker.dispose()
})

test('selection, home and scope changes replace the generation and cancel its pending work', () => {
  for (const change of [
    { sessionId: 'ses_other' }, { sessionId: undefined }, { directory: '/other/project' },
    { projectID: 'other' }, { workspaceID: 'wrk_other' },
  ]) {
    const tracker = new PresenceTracker()
    const p = payload()
    const first = tracker.accept(p)!
    tracker.confirm(first, p.sessionId)
    const next = tracker.accept({ ...p, sequence: 2, ...change })!
    assert.notEqual(next, first)
    assert.equal(tracker.current(first), false)
    assert.equal(first.controller.signal.aborted, true)
    assert.equal(next.freshUntil, 0)
    tracker.confirm(first, p.sessionId)
    assert.equal(tracker.fresh(p.sessionId!), false)
    tracker.dispose()
  }
})
