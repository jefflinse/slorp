import type { Plugin } from '@opencode/plugin/tui'
import { randomUUID } from 'node:crypto'
import { PresenceRPC, PRESENCE_PREFIX, parsePresence, presenceDirectory, presenceID, type Presence } from './presence.ts'
import { maxRelayInFlight, relayRequest, type NativeRequest } from './native.ts'

/** Announces terminal presence through the client's authenticated local or attach transport. */
const setup: Plugin.Definition['setup'] = async api => {
  const terminalId = randomUUID()
  let sequence = 0
  let disposed = false
  let active: AbortController | undefined
  const requests = new Map<string, AbortController>()
  const rpc = api.client.rpc(PresenceRPC)
  const unsubscribe = rpc.events.on('request', event => {
    const request = event.data as NativeRequest
    if (disposed || request.terminalId !== terminalId || requests.has(request.id)) return
    if (requests.size >= maxRelayInFlight) {
      void rpc.result({ id: request.id, terminalId, error: 'Slorp: relay busy; retry later' },
        { location: event.location }).catch(() => {})
      return
    }
    const controller = new AbortController()
    requests.set(request.id, controller)
    const timeout = setTimeout(() => controller.abort(), 5000)
    timeout.unref?.()
    void relayRequest(api.client, request, controller.signal).then(value => {
      if (!disposed && !controller.signal.aborted)
        return rpc.result({ id: request.id, terminalId, value }, { location: event.location, signal: controller.signal })
    }, () => {
      if (!disposed && !controller.signal.aborted)
        return rpc.result({ id: request.id, terminalId, error: 'OpenCode request failed' }, { location: event.location, signal: controller.signal })
    })
      .catch(() => {}).finally(() => { clearTimeout(timeout); requests.delete(request.id) })
  })
  const stopCancellation = rpc.events.on('cancel', event => {
    if (event.data.terminalId === terminalId && typeof event.data.id === 'string') requests.get(event.data.id)?.abort()
  })
  const sample = () => {
    const route = api.ui.router.current()
    const sessionId = route.type === 'session' ? route.sessionID : undefined
    const info = typeof sessionId === 'string' ? api.data.session.get(sessionId) : undefined
    return {
      route: route.type, sessionId, directory: (api.location ?? api.data.location.default()).directory,
      info: info && { id: info.id, directory: info.location.directory, projectID: info.projectID },
    }
  }
  const announce = async () => {
    if (disposed || active || sequence === Number.MAX_SAFE_INTEGER) return
    const initial = sample()
    if (!['home', 'session'].includes(initial.route)) return
    const controller = active = new AbortController()
    const timeout = setTimeout(() => controller.abort(), 5000)
    timeout.unref?.()
    const current = () => !disposed && !controller.signal.aborted && JSON.stringify(sample()) === JSON.stringify(initial)
    const send = async () => {
      const next = ++sequence
      let info = initial.info
      if (initial.route === 'session') {
        if (!presenceID(initial.sessionId)) return
        if (!info || info.id !== initial.sessionId || !presenceDirectory(info.directory) || !presenceID(info.projectID)) {
          if (!presenceDirectory(initial.directory)) return
           const res = await api.client.session.get({ sessionID: initial.sessionId }, { signal: controller.signal })
           if (!current() || !res || res.id !== initial.sessionId ||
             !presenceDirectory(res.location.directory) || !presenceID(res.projectID) ||
             info?.projectID && info.projectID !== res.projectID ||
             info?.directory && info.directory !== res.location.directory) return
           info = { id: res.id, directory: res.location.directory, projectID: res.projectID }
        }
      }
      if (!current()) return
      const payload: Presence = {
        version: 1, terminalId, sequence: next, directory: info?.directory ?? initial.directory,
        ...(info ? { projectID: info.projectID } : {}),
        ...(initial.route === 'session' ? { sessionId: initial.sessionId as string } : {}),
      }
      const command = PRESENCE_PREFIX + JSON.stringify(payload)
      if (!parsePresence(command)) return
      await api.client.rpc(PresenceRPC).presence({ command }, { location: { directory: payload.directory }, signal: controller.signal })
    }
    let abort = () => {}
    const aborted = new Promise<void>(resolve => { abort = () => resolve(); controller.signal.addEventListener('abort', abort, { once: true }) })
    try {
      await Promise.race([send(), aborted])
    } catch {
      // A fresh sample and sequence retry on the next tick, including SDK error responses.
    } finally {
      clearTimeout(timeout)
      controller.signal.removeEventListener('abort', abort)
      if (active === controller) active = undefined
    }
  }
  const timer = setInterval(() => { void announce().catch(() => {}) }, 2000)
  timer.unref?.()
  void announce().catch(() => {})
  return () => {
    disposed = true; clearInterval(timer); active?.abort()
    unsubscribe(); stopCancellation()
    for (const controller of requests.values()) controller.abort()
    requests.clear()
  }
}

export default { id: 'slorp', setup } satisfies Plugin.Definition
