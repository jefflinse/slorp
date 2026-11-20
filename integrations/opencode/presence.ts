export const PRESENCE_PREFIX = 'slorp.presence.v1:'
export const PRESENCE_TTL = 10_000
const LIMIT = 4096

export type Presence = {
  version: 1
  terminalId: string
  sequence: number
  directory: string
  projectID?: string
  workspaceID?: string
  sessionId?: string
}

export function presenceID(value: unknown): value is string {
  return typeof value === 'string' && /^[A-Za-z0-9_-]{1,256}$/.test(value)
}

export function presenceDirectory(value: unknown): value is string {
  return typeof value === 'string' && Buffer.byteLength(value) <= 2048 &&
    /^(?:\/|[A-Za-z]:[\\/]|\\\\)/.test(value) && !/[\p{Cc}]/u.test(value)
}

export function parsePresence(command: unknown): Presence | undefined {
  if (typeof command !== 'string' || Buffer.byteLength(command) > LIMIT || !command.startsWith(PRESENCE_PREFIX)) return
  try {
    const p = JSON.parse(command.slice(PRESENCE_PREFIX.length))
    if (!p || Array.isArray(p) || p.version !== 1 ||
      typeof p.terminalId !== 'string' || !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(p.terminalId) ||
      !Number.isSafeInteger(p.sequence) || p.sequence <= 0 || !presenceDirectory(p.directory) ||
      Object.keys(p).some(key => !['version', 'terminalId', 'sequence', 'directory', 'projectID', 'workspaceID', 'sessionId'].includes(key))) return
    for (const key of ['projectID', 'workspaceID', 'sessionId']) if (p[key] !== undefined && !presenceID(p[key])) return
    return p
  } catch {
    return
  }
}

export type PresenceLease = {
  payload: Presence
  expires: number
  controller: AbortController
  freshUntil: number
  root?: string
}

/** Bounds terminal high-water marks; leases fence admission, never session lifetime. */
export class PresenceTracker {
  private readonly terminals = new Map<string, PresenceLease>()

  accept(payload: Presence): PresenceLease | undefined {
    const now = Date.now()
    const previous = this.terminals.get(payload.terminalId)
    if (previous && previous.payload.sequence >= payload.sequence) return
    if (!previous && this.terminals.size >= 256) {
      const expired = [...this.terminals].find(([, lease]) => lease.expires <= now)
      if (!expired) return
      expired[1].controller.abort()
      this.terminals.delete(expired[0])
    }
    const sameSelection = previous && (['directory', 'projectID', 'workspaceID', 'sessionId'] as const).every(key =>
      previous.payload[key] === payload[key])
    if (sameSelection && this.current(previous)) {
      previous.payload = payload
      previous.expires = now + PRESENCE_TTL
      return previous
    }
    previous?.controller.abort()
    const lease: PresenceLease = { payload, expires: now + PRESENCE_TTL, controller: new AbortController(),
      freshUntil: 0 }
    this.terminals.set(payload.terminalId, lease)
    return lease
  }

  current(lease: PresenceLease) {
    return this.terminals.get(lease.payload.terminalId) === lease && lease.expires > Date.now() && !lease.controller.signal.aborted
  }

  fresh(root: string) {
    return [...this.terminals.values()].some(lease => this.current(lease) && lease.freshUntil > Date.now() && (!lease.root || lease.root === root))
  }

  confirm(lease: PresenceLease, root?: string) {
    if (!this.current(lease)) return
    lease.root = root
    lease.freshUntil = lease.expires
  }

  dispose() {
    for (const lease of this.terminals.values()) lease.controller.abort()
    this.terminals.clear()
  }
}
