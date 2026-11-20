import type { TuiPlugin } from '@opencode-ai/plugin/tui'
import { randomUUID } from 'node:crypto'
import { PRESENCE_PREFIX, parsePresence, presenceDirectory, presenceID, type Presence } from './presence.ts'

/** Announces terminal presence through the client's authenticated local or attach transport. */
const tui: TuiPlugin = async api => {
  const terminalId = randomUUID()
  let sequence = 0
  let disposed = false
  let active: AbortController | undefined
  const sample = () => {
    const route = api.route.current
    const sessionId = route.name === 'session' ? route.params?.sessionID : undefined
    const info = typeof sessionId === 'string' ? api.state.session.get(sessionId) : undefined
    return {
      ready: api.state.ready, route: route.name, sessionId, directory: api.state.path.directory,
      info: info && { id: info.id, directory: info.directory, projectID: info.projectID, workspaceID: info.workspaceID },
    }
  }
  const announce = async () => {
    if (disposed || active || sequence === Number.MAX_SAFE_INTEGER) return
    const initial = sample()
    if (!initial.ready || !['home', 'session'].includes(initial.route)) return
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
          const res = await api.client.session.get({ sessionID: initial.sessionId, directory: info?.directory || initial.directory,
            ...(info?.workspaceID ? { workspace: info.workspaceID } : {}) }, { signal: controller.signal })
          if (!current() || res.error || !res.data || res.data.id !== initial.sessionId ||
            !presenceDirectory(res.data.directory) || !presenceID(res.data.projectID) ||
            info?.projectID && info.projectID !== res.data.projectID ||
            info?.workspaceID && info.workspaceID !== res.data.workspaceID ||
            info?.directory && info.directory !== res.data.directory) return
          info = { id: res.data.id, directory: res.data.directory, projectID: res.data.projectID, workspaceID: res.data.workspaceID }
        }
      }
      if (!current()) return
      const payload: Presence = {
        version: 1, terminalId, sequence: next, directory: info?.directory ?? initial.directory,
        ...(info ? { projectID: info.projectID, workspaceID: info.workspaceID } : {}),
        ...(initial.route === 'session' ? { sessionId: initial.sessionId as string } : {}),
      }
      const command = PRESENCE_PREFIX + JSON.stringify(payload)
      if (!parsePresence(command)) return
      const res = await api.client.tui.publish({ directory: payload.directory,
        ...(payload.workspaceID ? { workspace: payload.workspaceID } : {}),
        body: { type: 'tui.command.execute', properties: { command } } }, { signal: controller.signal })
      if (res.error) throw res.error
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
  api.lifecycle.onDispose(() => { disposed = true; clearInterval(timer); active?.abort() })
  void announce().catch(() => {})
}

export default { id: 'slorp', tui }
