import type { Plugin } from '@opencode/plugin'
import type { OpenCodeEvent, FormInfo, ModelRef } from '@opencode/client'
import { QuotaCollector, quotaSource } from './quotas.ts'
import { PresenceRPC, parsePresence, presenceID, PresenceTracker, type PresenceLease } from './presence.ts'
import { Native, sessionInfo, type Info, type Message, type Usage, type Part, type TranscriptMessage } from './native.ts'
import { formQuestions, formAnswer } from './forms.ts'
import { constants } from 'node:fs'
import { open } from 'node:fs/promises'

const AGENT = 'opencode'
const VERSION = '0.7.0'
const DEFAULT_URL = 'http://127.0.0.1:4747'
const POLL_MS = 2000
const REQUEST_MS = 5000
const DISCOVERY_CONCURRENCY = 4
const CONTROL_LIMIT = 1024
const SUMMARY_LIMIT = 400
/** How much of what a session last said is passed on, in characters. */
const REPLY_LIMIT = 30000
/** How many of the things said in answer to one prompt are passed on. */
const SAID_LIMIT = 40
/** Inputs longer than this are too much to judge in Slorp; the terminal asks. */
const INPUT_LIMIT = 8000
/** How long Slorp may answer a prompt before only the terminal can. */
const ANSWER_MAX_MS = 60 * 60_000
/** The title OpenCode gives a session before it has written one. */
const UNTITLED = /^New session - \d{4}-/

type Reply = { id: string; text: string }

class BridgeHTTPError extends Error {
  readonly status: number

  constructor(status: number) {
    super(`Slorp: HTTP ${status}`)
    this.name = 'BridgeHTTPError'
    this.status = status
  }
}

/** What is known of one session; for a top-level one, enough to tell Slorp again. */
type Known = {
  parent?: string
  title: string
  cwd: string
  agent?: string
  model?: { providerID: string; modelID: string }
  prompt: string
  reply: string
  isBusy: boolean
  /** Whether Slorp is told about the session, which it is once something happens in it. */
  isLive: boolean
  variant?: string
  status?: 'working' | 'idle' | 'failed'
  outcome?: 'completed' | 'failed' | 'killed'
  waitingFor?: string
  startedAt?: number
  activeAt?: number
  promptAt?: number
  promptID?: string
  replyID?: string
  /** What has been said in answer to the prompt, by message, oldest first. */
  said?: Map<string, string>
  contextWindow?: number
  contextTokens?: number
  contextClearedAt?: number
  measuredAt?: number
  metadataAt?: number
  /** Last selection change, ordered independently of native timestamps. */
  selectionRevision?: number
  compacted?: { id?: string; at: string; auto?: boolean; before?: number }
  compacting?: { id?: string; auto: boolean; before: number }
  usage?: Map<string, { tokens: Usage; cost: number }>
  hydrated?: boolean
  hydrating?: Promise<void>
  historyGeneration?: number
  needsAnswer?: boolean
  messages?: Map<string, Message>
  parts?: Map<string, Part>
  tasks?: Map<string, Task>
  notifications?: Map<string, { result: NonNullable<Task['result']>; messageID: string; at: number }>
  deleted?: boolean
  treeReady?: boolean
  treeGeneration?: number
  treeHydrating?: Promise<void>
  refreshing?: boolean
  polling?: boolean
  statusKnown?: boolean
  retainedChildren?: Set<string>
  epoch?: string
  retained?: { fingerprint: string; revision: number; delivered?: boolean }
  deliveredReport?: string
}

type Task = {
  id: string
  child: string
  start: number
  end?: number
  type: string
  description: string
  prompt: string
  background: boolean
  model?: { providerID: string; modelID: string }
  status: string
  result?: { id: string; state: string; text: string }
}

type ToolState = {
  status?: string
  input?: Record<string, unknown>
  metadata?: { sessionId?: string; parentSessionId?: string; background?: boolean; truncated?: boolean; model?: { providerID: string; modelID: string } }
  output?: string
  time?: { start?: number; end?: number }
}

type Discovery = { root: Known; generation: number }

/** A permission prompt or question OpenCode is waiting on. */
type Ask = {
  sessionId: string
  slorpId?: string
  epoch?: string
  settled: AbortController
  running?: boolean
  attempt?: symbol
  resolved?: boolean
  origin: string
  tool: string
  full: string
  canAnswer: boolean
  deadline: number
  form?: FormInfo
}

function textOf(parts: readonly Part[]) {
  return parts
    .filter(p => p.type === 'text' && !p.synthetic && !p.ignored && p.text?.trim())
    .map(p => p.text!.trim())
    .join('\n\n')
}

/**
 * The latest of what was said in answer to one prompt, oldest first: at most
 * SAID_LIMIT of them and, the last aside, at most REPLY_LIMIT characters of
 * them. saidEarlier is how many earlier ones are left out.
 */
function kept(said: readonly string[]) {
  let from = said.length - 1
  for (let room = REPLY_LIMIT; from > 0 && said.length - from < SAID_LIMIT; from--) {
    room -= Math.min(said[from - 1]?.length ?? 0, REPLY_LIMIT)
    if (room < 0) break
  }
  from = Math.max(from, 0)
  return { said: said.slice(from).map(text => text.slice(-REPLY_LIMIT)), saidEarlier: from }
}

function runes(text: string, limit: number) {
  return Array.from(text).slice(0, limit).join('')
}

function validID(id: unknown): id is string {
  return typeof id === 'string' && id !== '' && id !== '.' && id !== '..' && Buffer.byteLength(id) <= 256 && !/[\s\p{Cc}/\\]/u.test(id)
}

async function managedSession(path: string) {
  const file = await open(path, constants.O_RDONLY | constants.O_NONBLOCK)
  try {
    const stat = await file.stat()
    if (!stat.isFile() || stat.size > CONTROL_LIMIT) return
    const buffer = Buffer.alloc(CONTROL_LIMIT + 1)
    let size = 0
    while (size < buffer.length) {
      const { bytesRead } = await file.read(buffer, size, buffer.length - size, null)
      if (!bytesRead) break
      size += bytesRead
    }
    if (size > CONTROL_LIMIT) return
    const value = JSON.parse(buffer.subarray(0, size).toString('utf8'))
    const id: unknown = value?.sessionId
    if (validID(id) && /^ses[A-Za-z0-9_-]+$/.test(id)) return { id, url: typeof value.opencodeURL === 'string' ? value.opencodeURL : undefined }
  } finally {
    await file.close()
  }
}

/** What a permission prompt asks to do, as the user should read it in Slorp. */
function describe(request: { resources?: readonly string[]; metadata?: Record<string, unknown> }) {
  const meta = request.metadata ?? {}
  if (typeof meta.command === 'string') return meta.command
  if (typeof meta.diff === 'string') return meta.diff
  const rest = Object.keys(meta).length > 0 ? [JSON.stringify(meta, null, 2)] : []
  return [...(request.resources ?? []), ...rest].join('\n')
}

/**
 * Bridges the sessions of one OpenCode instance to the Slorp service at the
 * `url` option, or at SLORP_URL when that is set. A top-level session is
 * reported once something happens in it, with its current state and usage
 * repeated on every poll. Replies are submitted under its selected agent,
 * model and variant. Permissions and questions, including subagents', can
 * be answered in Slorp or OpenCode; Slorp only ever permits a single call.
 */
export async function Slorp(ctx: Plugin.Context, options = ctx.options) {
  const { directory, project } = ctx.location
  const native = new Native(ctx, typeof options.opencodeURL === 'string' ? options.opencodeURL : undefined)
  const base = (process.env.SLORP_URL || String(options?.url ?? DEFAULT_URL)).replace(/\/+$/, '')
  const managedFile = process.env.SLORP_MANAGED_FILE
  const managedRun = process.env.SLORP_MANAGED_RUN
  const managed = managedFile !== undefined || managedRun !== undefined
  let managedRoot: string | undefined
  let managedAvailable = false
  let managedVerified = false
  let managedActivity = new AbortController()
  let readingControl = false
  let managedDiscovery: Promise<void> | undefined
  const sessions = new Map<string, Known>()
  const deletedSessions = new Set<string>()
  const asks = new Map<string, Ask>()
  let branch = ''
  let sending: Promise<unknown> = Promise.resolve()
  const stopped = new AbortController()
  const acknowledgements = new Map<string, string>()
  const settlements = new Set<string>()
  const events = new Map<string, Promise<void>>()
  let settling = false
  let acknowledging = false
  const signal = (activity?: AbortSignal) => AbortSignal.any([stopped.signal, AbortSignal.timeout(REQUEST_MS), ...(activity ? [activity] : [])])
  const presence = new PresenceTracker()
  const quotas = new QuotaCollector(openRoot)
  const quotaObservations = new Map<string, number>()
  const presencePending = new Set<string>()
  const discovering = new Set<string>()
  let selectionRevision = 0
  const received = new Set<string>()
  const waitingScans = new Set<string>()
  let settlementRevision = 0
  const settledRequests = new Map<string, number>()
  const activeScans = new Set<number>()
  const refreshingHistory = new Set<string>()
  const dirtyHistory = new Set<string>()
  let activeDiscovery = false
  let nextActiveDiscovery = 0

  function receivePresence(command: unknown) {
    const payload = parsePresence(command)
    if (!payload || stopped.signal.aborted) return
    const lease = presence.accept(payload)
    if (!lease || payload.directory !== directory || payload.projectID && project?.id && payload.projectID !== project.id) return
    native.advertise(payload.terminalId)
    if (!managed) void discoverActive()
    if (!payload.sessionId) {
      if (!managed) presence.confirm(lease)
      return
    }
    if (managed && !managedOpen()) return
    if (presencePending.has(payload.terminalId)) return
    presencePending.add(payload.terminalId)
    void enqueue(payload.sessionId, () => discoverPresence(lease)).catch(() => {
      // Lookup failures retry on the terminal's next advertisement.
    }).finally(() => { presencePending.delete(payload.terminalId) })
  }

  async function discoverPresence(lease: PresenceLease) {
    const p = lease.payload
    const current = () => !stopped.signal.aborted && presence.current(lease)
    const records: { info: Info; revision: number }[] = []
    const seen = new Set<string>()
    let id = p.sessionId!
    while (current() && seen.size <= 64) {
      if (!presenceID(id) || seen.has(id) || deletedSessions.has(id)) return
      seen.add(id)
      const revision = selectionRevision
      const info = sessionInfo(await ctx.session.get({ sessionID: id }, { signal: signal(lease.controller.signal) }))
      if (!current() || info.id !== id || info.directory !== directory ||
        !presenceID(info.projectID) || project?.id && info.projectID !== project.id ||
        p.projectID && info.projectID !== p.projectID || p.workspaceID !== undefined ||
        records.length && info.projectID !== records[0]!.info.projectID) return
      records.push({ info, revision })
      if (info.parentID) { id = info.parentID; continue }
      const root = id
      const activate = async () => {
        if (!current() || records.some(({ info }) => deletedSessions.has(info.id) || sessions.get(info.id)?.deleted)) return
        if (managed) {
          if (managedOpen(root)) presence.confirm(lease, root)
          return
        }
        let s = sessions.get(root)
        const wasLive = s?.isLive
        if (!s?.isLive) {
          for (const { info, revision } of records) {
            if (!sessions.get(info.id)?.isLive) learn(info, undefined, revision)
          }
          s = sessions.get(root)!
          await hydrate(root, s)
          if (!current() || s.deleted || s.parent || !s.hydrated) return
          if (!s.isLive) {
            const statuses = await native.active(signal(lease.controller.signal))
            if (!current() || s.deleted || s.parent) return
            if (!s.isLive) {
              const status = statuses[root]
              s.isBusy = status?.type === 'running'
              s.status = s.isBusy ? 'working' : 'idle'
              s.waitingFor = ''
              s.isLive = true
            }
          }
        }
        if (!current() || !localRoot(root)) return
        presence.confirm(lease, root)
        if (!wasLive) await poll()
      }
      if (root === p.sessionId) await activate()
      else await enqueue(root, activate)
      return
    }
  }

  function managedOpen(id?: string) {
    return !managed || managedAvailable && managedVerified && (id === undefined || id === managedRoot)
  }

  async function tick() {
    if (!managed) { void discoverActive(); return poll() }
    if (readingControl || stopped.signal.aborted || !managedFile || !managedRun) return
    readingControl = true
    let control: Awaited<ReturnType<typeof managedSession>>
    try {
      control = await managedSession(managedFile)
    } catch {
      // Missing, malformed, or unreadable control files retry only on the next tick.
    } finally {
      readingControl = false
    }
    if (stopped.signal.aborted) return
    try { if (control?.url) native.setURL(control.url) } catch { control = undefined }
    const id = control?.id
    if (id && !managedRoot) {
      managedRoot = id
      for (const known of sessions.keys()) if (known !== id) quotas.clear(known)
    }
    if (!id || id !== managedRoot) {
      managedAvailable = false
      managedVerified = false
      managedActivity.abort()
      for (const id of sessions.keys()) quotas.clear(id)
      quotaObservations.clear()
      for (const ask of asks.values()) {
        ask.attempt = undefined
        ask.running = false
        if (ask.slorpId) finish(ask.slorpId)
        ask.slorpId = undefined
        ask.epoch = undefined
      }
      return
    }
    if (!managedAvailable) managedActivity = new AbortController()
    managedAvailable = true
    if (!managedVerified) {
      if (!managedDiscovery) {
        const root = id
        managedDiscovery = enqueue(root, () => discover(root)).catch(() => {}).finally(() => { managedDiscovery = undefined })
      }
    } else {
      void poll()
    }
  }

  async function discoverActive() {
    if (activeDiscovery || stopped.signal.aborted || Date.now() < nextActiveDiscovery) return
    activeDiscovery = true
    try {
      const active = await native.active(signal())
      nextActiveDiscovery = Date.now() + 10000
      await Promise.all(Object.keys(active).map(id => discover(id).catch(() => {})))
    } catch {
      // Presence and later ticks retry without starting or changing a service.
    } finally {
      activeDiscovery = false
    }
  }

  function enqueue(id: string, step: () => Promise<void>) {
    const next = (events.get(id) ?? Promise.resolve()).then(step)
    events.set(id, next.catch(() => {}))
    return next
  }

  async function discover(id: string) {
    if (managed && (!managedAvailable || id !== managedRoot)) return
    if ((sessions.get(id)?.isLive && (!managed || managedVerified)) || deletedSessions.has(id) || discovering.has(id)) return
    const activity = managedActivity
    discovering.add(id)
    try {
      const selectionReadAt = selectionRevision
      const info = sessionInfo(await ctx.session.get({ sessionID: id }, { signal: signal() }))
      if (stopped.signal.aborted || deletedSessions.has(id) || info.directory !== directory || info.projectID !== project.id) return
      if (managed && (info.id !== id || info.parentID || activity.signal.aborted || activity !== managedActivity)) return
      const known = learn(info, undefined, selectionReadAt)
      if (managed) {
        await hydrate(id, known, undefined, true)
        if (!known.hydrated) return
      }
      const [root, s] = await top(id)
      const activate = async () => {
        const statuses = await native.active(signal())
        if (stopped.signal.aborted || (s.isLive && (!managed || managedVerified)) || s.deleted) return
        if (managed && (!managedAvailable || activity !== managedActivity || activity.signal.aborted || root !== managedRoot)) return
        const status = statuses[root]
        s.isBusy = status?.type === 'running'
        s.status = s.isBusy ? 'working' : 'idle'
        s.waitingFor = ''
        s.isLive = true
        if (managed) managedVerified = true
        await poll()
      }
      if (root === id) await activate()
      else await enqueue(root, activate)
    } finally {
      discovering.delete(id)
    }
  }

  async function post(path: string, body: object, signal?: AbortSignal) {
    const id = (body as { sessionId?: string }).sessionId
    if (stopped.signal.aborted || !managedOpen(id) || id && (deletedSessions.has(id) || sessions.get(id)?.deleted)) throw new Error('Slorp: session closed')
    const activity = managedActivity
    const res = await fetch(base + path, {
      method: 'POST',
      headers: { 'content-type': 'application/json', ...(managed ? { 'X-Slorp-Run': managedRun! } : {}) },
      body: JSON.stringify(body),
      signal: AbortSignal.any([stopped.signal, signal ?? AbortSignal.timeout(REQUEST_MS), ...(managed ? [activity.signal] : [])]),
    })
    if (!res.ok) throw new BridgeHTTPError(res.status)
    const text = await res.text()
    if (managed && (activity.signal.aborted || activity !== managedActivity || !managedOpen(id))) throw new Error('Slorp: session closed')
    return text ? JSON.parse(text) : undefined
  }

  /** Runs the steps given to it one at a time, so Slorp hears of turns in the order they happened. */
  function inOrder<T>(step: () => Promise<T>): Promise<T | undefined> {
    const next = sending.then(step).catch(() => undefined)
    sending = next
    return next
  }

  function report(sessionId: string, type: string, text = '', at?: number) {
    inOrder(async () => {
      if (!openRoot(sessionId)) return
      await post('/api/bridge/event', { agent: AGENT, sessionId, type, text, ...(at ? { at } : {}) })
    })
  }

  function openRoot(id: string) {
    return localRoot(id) && managedOpen(id)
  }

  function localRoot(id: string) {
    const s = sessions.get(id)
    return !!s && !s.parent && s.cwd === directory && !s.deleted && !deletedSessions.has(id) && !stopped.signal.aborted
  }

  function invalidateTree(id?: string) {
    const seen = new Set<string>()
    while (id && !seen.has(id)) {
      seen.add(id)
      const s = sessions.get(id)
      if (!s) break
      s.treeReady = false
      s.treeGeneration = (s.treeGeneration ?? 0) + 1
      id = s.parent
    }
  }

  function learn(info: Info, discovery?: Discovery, selectionReadAt = selectionRevision) {
    const existed = sessions.has(info.id)
    const s = sessions.get(info.id) ?? { title: '', cwd: directory, prompt: '', reply: '', isBusy: false, isLive: false }
    const previousParent = s.parent
    s.parent = info.parentID ?? s.parent
    if (deletedSessions.has(info.id) || s.parent && sessions.get(s.parent)?.deleted) s.deleted = true
    if (info.title !== undefined) s.title = UNTITLED.test(info.title) ? '' : info.title
    if (info.directory) s.cwd = info.directory
    if ((info.agent || info.model) && (s.selectionRevision ?? 0) <= selectionReadAt &&
      (info.time?.updated === undefined || info.time.updated >= (s.metadataAt ?? 0))) {
      if (info.agent) s.agent = info.agent
      if (info.model?.providerID && info.model.id) model(s, { providerID: info.model.providerID, modelID: info.model.id })
      if (info.model) s.variant = info.model.variant
      s.metadataAt = Math.max(s.metadataAt ?? 0, info.time?.updated ?? 0)
      s.selectionRevision = ++selectionRevision
    }
    if (info.time) {
      s.startedAt = info.time.created
      s.activeAt = Math.max(s.activeAt ?? 0, info.time.updated)
    }
    if (!s.statusKnown && info.time) {
      s.isBusy = (info.time.busy ?? 0) > (info.time.idle ?? 0)
      if (info.outcome) s.outcome = info.outcome === 'succeeded' ? 'completed' : info.outcome === 'interrupted' ? 'killed' : 'failed'
    }
    sessions.set(info.id, s)
    if (!existed || previousParent !== s.parent) {
      const before = discovery?.root.treeGeneration ?? 0
      invalidateTree(info.id)
      if (previousParent !== s.parent) invalidateTree(previousParent)
      if (discovery) discovery.generation += (discovery.root.treeGeneration ?? 0) - before
    }
    return s
  }

  function model(s: Known, next: Known['model']) {
    if (!next) return
    const changed = s.model?.modelID !== next.modelID || s.model?.providerID !== next.providerID
    if (changed) s.contextWindow = 0
    s.model = { providerID: next.providerID, modelID: next.modelID }
    if (changed) void ctx.model.list().then(catalog => {
      if (stopped.signal.aborted || s.model?.providerID !== next.providerID || s.model.modelID !== next.modelID) return
      const selected = catalog.data.find(m => m.providerID === next.providerID && m.id === next.modelID)
      if (selected) s.contextWindow = selected.limit.context
    }).catch(() => {})
  }

  /** Resolves a session to the top-level session it runs under, which is itself unless it is a subagent's. */
  async function top(id: string): Promise<[string, Known]> {
    for (let hops = 0; ; hops++) {
      let s = sessions.get(id)
      if (!s) {
        const selectionReadAt = selectionRevision
        const info = sessionInfo(await ctx.session.get({ sessionID: id }, { signal: signal() }))
        if (!info.parentID && (info.directory !== directory || info.projectID !== project.id)) throw new Error('Slorp: different location')
        s = learn(info, undefined, selectionReadAt)
      }
      if (!s.parent || hops === 64) {
        await hydrate(id, s)
        return [id, s]
      }
      id = s.parent
    }
  }

  function measured(s: Known, info: Message) {
    if (!info.id || !info.tokens) return
    ;(s.usage ??= new Map()).set(info.id, { tokens: info.tokens, cost: info.cost ?? 0 })
    if (info.role !== 'assistant' || info.summary) return
    const at = info.time?.created ?? 0
    if (at < (s.contextClearedAt ?? 0)) return
    if (at < (s.measuredAt ?? 0)) return
    s.measuredAt = at
    if (at >= (s.metadataAt ?? 0) && ((info.providerID && info.modelID) || info.variant !== undefined)) {
      if (info.providerID && info.modelID) model(s, { providerID: info.providerID, modelID: info.modelID })
      s.variant = info.variant ?? s.variant
      s.metadataAt = at
      s.selectionRevision = ++selectionRevision
    }
    const t = info.tokens
    s.contextTokens = t.total ?? t.input + t.output + (t.reasoning ?? 0) + t.cache.read + t.cache.write
  }

  function rememberMessage(s: Known, info: Message, historical = false) {
    if (!info.id) return
    if (historical && s.messages?.has(info.id)) return
    ;(s.messages ??= new Map()).set(info.id, info)
    for (const notification of s.notifications?.values() ?? []) if (notification.messageID === info.id) notification.at = info.time?.created ?? notification.at
    measured(s, info)
    s.activeAt = Math.max(s.activeAt ?? 0, info.time?.completed ?? info.time?.created ?? 0)
    if (s.parent && (info.time?.created ?? 0) >= (s.metadataAt ?? 0)) {
      s.metadataAt = info.time?.created ?? 0
      if (info.model) model(s, info.model)
      else if (info.providerID && info.modelID) model(s, { providerID: info.providerID, modelID: info.modelID })
      if (info.agent) s.agent = info.agent
      s.variant = info.variant ?? info.model?.variant ?? s.variant
      s.selectionRevision = ++selectionRevision
    }
  }

  function rememberPart(id: string, s: Known, part: Part, historical = false, discovery?: Discovery) {
    if (typeof part.id !== 'string') return
    const partID = part.id
    const current = s.parts?.get(partID)
    if (historical && current) {
      if (part.type !== 'tool' || part.tool !== 'task') return
      const earlier = part.state as ToolState | undefined
      const latest = current.state as ToolState | undefined
      part = { ...current, state: { ...earlier, ...latest, metadata: latest?.metadata ?? earlier?.metadata, input: latest?.input ?? earlier?.input } }
    }
    ;(s.parts ??= new Map()).set(partID, part)
    if (part.type === 'text' && part.synthetic && part.text) {
      const result = part.subagent as Task['result']
      if (result && result.state !== 'running') {
        const messageID = String(part.messageID)
        ;(s.notifications ??= new Map()).set(partID, { result, messageID, at: s.messages?.get(messageID)?.time?.created ?? 0 })
      }
    }
    if (part.type !== 'tool' || part.tool !== 'task') return
    const state = part.state as ToolState | undefined
    if (!state) return
    const old = s.tasks?.get(partID)
    if (old && ['completed', 'error'].includes(old.status) && !['completed', 'error'].includes(state.status ?? '')) return
    const resumed = typeof state.input?.task_id === 'string' && sessions.get(state.input.task_id)?.parent === id ? state.input.task_id : undefined
    const child = state.metadata?.sessionId ?? old?.child ?? resumed
    if (!validID(child) || child === id || (state.metadata?.parentSessionId && state.metadata.parentSessionId !== id)) return
    if (sessions.get(child)?.parent && sessions.get(child)?.parent !== id) return
    if (sessions.get(child)?.deleted) return
    const input = state.input ?? {}
    const background = state.metadata?.background === true || input.background === true || old?.background === true
    const truncated = state.status === 'completed' && state.metadata?.truncated === true && !background && typeof state.output === 'string'
    const str = (key: string, fallback = '') => typeof input[key] === 'string' ? input[key] as string : fallback
    const task: Task = {
      id: partID, child,
      start: state.time?.start ?? old?.start ?? s.messages?.get(String(part.messageID))?.time?.created ?? Date.now(),
      end: state.time?.end ?? old?.end,
      type: str('subagent_type', old?.type), description: str('description', old?.description),
      prompt: str('prompt', old?.prompt), background,
      model: state.metadata?.model ?? old?.model,
      status: state.status ?? old?.status ?? 'pending', result: truncated ? { id: child, state: 'completed', text: state.output! } : old?.result,
    }
    ;(s.tasks ??= new Map()).set(task.id, task)
    learn({ id: child, parentID: id }, discovery)
  }

  function descendants(root: string) {
    const out: { id: string; s: Known; depth: number }[] = []
    const seen = new Set([root])
    const visit = (parent: string, depth: number) => {
      if (depth > 64) return
      for (const [id, s] of sessions) {
        if (s.parent !== parent || seen.has(id)) continue
        seen.add(id)
        out.push({ id, s, depth })
        visit(id, depth + 1)
      }
    }
    visit(root, 1)
    return out
  }

  function hydrateTree(root: string, s: Known) {
    if (s.treeHydrating) return s.treeHydrating
    s.treeReady = false
    const discovery: Discovery = { root: s, generation: s.treeGeneration ?? 0 }
    s.treeHydrating = (async () => {
      let complete = true
      const seen = new Set([root])
      const pending = [{ id: root, node: s, depth: 0 }]
      const visit = async ({ id, node, depth }: typeof pending[number]) => {
        if (!openRoot(root)) return
        if (!node.deleted) {
          await hydrate(id, node, discovery)
          if (!openRoot(root)) return
          if (!node.deleted) {
            try {
              const selectionReadAt = selectionRevision
              const children: Info[] = []
              let cursor: string | undefined
              do {
                const page = await ctx.session.list(cursor ? { limit: 100, cursor } : { parentID: id, limit: 100 }, { signal: signal() })
                children.push(...page.data.map(sessionInfo))
                cursor = page.cursor.next ?? undefined
              } while (cursor && openRoot(root))
              if (!openRoot(root)) return
              if (!node.deleted) {
                for (const info of children) {
                  if (!validID(info.id) || info.id === root || info.parentID !== id) continue
                  learn(info, discovery, selectionReadAt)
                }
              }
            } catch {
              complete = false
            }
          }
        }
        complete &&= !!node.hydrated
        for (const [child, known] of sessions) {
          if (known.parent !== id || seen.has(child)) continue
          if (depth === 64) { complete = false; continue }
          seen.add(child)
          pending.push({ id: child, node: known, depth: depth + 1 })
        }
      }
      while (pending.length && openRoot(root)) await Promise.all(pending.splice(0, DISCOVERY_CONCURRENCY).map(visit))
      if (!openRoot(root)) return
      if (descendants(root).some(({ s }) => !s.statusKnown && !s.deleted)) {
        try {
          const statuses = await native.active(signal())
          if (!openRoot(root)) return
          for (const { id, s } of descendants(root)) {
            if (s.statusKnown || s.deleted) continue
            const status = statuses[id]
            s.isBusy = status?.type === 'running'
            s.waitingFor = ''
            s.statusKnown = true
          }
        } catch {
          // Events and the next poll can still establish live child status.
        }
      }
      s.treeReady = openRoot(root) && complete && discovery.generation === (s.treeGeneration ?? 0) &&
        descendants(root).every(({ id, s }) => seen.has(id) && s.hydrated)
    })().finally(() => { s.treeHydrating = undefined })
    return s.treeHydrating
  }

  function totals(nodes: Known[]) {
    const used = { in: 0, out: 0, cacheRead: 0, cacheWrite: 0 }
    let costUsd = 0
    for (const s of nodes) for (const { tokens: t, cost } of s.usage?.values() ?? []) {
      used.in += t.input
      used.out += t.output + (t.reasoning ?? 0)
      used.cacheRead += t.cache.read
      used.cacheWrite += t.cache.write
      costUsd += cost
    }
    return { used, costUsd }
  }

  function childRecord(root: string, id: string, s: Known, depth: number) {
    const parent = sessions.get(s.parent!)
    const tasks = [...(parent?.tasks?.values() ?? [])].filter(t => t.child === id).sort((a, b) => a.start - b.start || a.id.localeCompare(b.id))
    const task = tasks.at(-1)
    const messages = [...(s.messages?.values() ?? [])].sort((a, b) => (a.time?.created ?? 0) - (b.time?.created ?? 0) || (a.id ?? '').localeCompare(b.id ?? ''))
    const parts = [...(s.parts?.values() ?? [])]
    const texts = new Map<string, string[]>()
    for (const part of parts) {
      if (typeof part.messageID !== 'string') continue
      const text = textOf([part])
      if (text) {
        const group = texts.get(part.messageID) ?? []
        group.push(text)
        texts.set(part.messageID, group)
      }
    }
    const text = (m: Message) => texts.get(m.id ?? '')?.join('\n\n') ?? ''
    const users = messages.filter(m => m.role === 'user' && text(m))
    const runAt = Math.max(task?.start ?? 0, users.at(-1)?.time?.created ?? 0)
    let result = task?.result?.id === id && task.result.state !== 'running' && task.status === 'completed' && (task.end ?? 0) >= runAt ? task.result : undefined
    let resultAt = result ? task?.end ?? 0 : 0
    for (const { result: returned, at } of parent?.notifications?.values() ?? []) {
      if (returned.id === id && at >= runAt && at >= resultAt) {
        result = returned
        resultAt = at
      }
    }
    let status = s.isBusy ? 'running' : 'idle'
    if (task && ['pending', 'running'].includes(task.status)) status = task.status === 'pending' ? 'pending' : 'running'
    if (task?.background && task.status === 'completed' && (!task.result || task.result.state === 'running')) status = 'running'
    if (task?.status === 'completed' && !task.background && task.result?.state !== 'running' && (task.end ?? 0) >= runAt) status = 'completed'
    if (result) status = result.state === 'error' ? 'failed' : result.state === 'cancelled' ? 'killed' : 'completed'
    if (task?.status === 'error' && (task.end ?? 0) >= runAt) status = 'failed'
    const failed = messages.filter(m => m.role === 'assistant' && m.error).findLast(m => (m.time?.completed ?? m.time?.created ?? 0) >= Math.max(runAt, resultAt))
    if (failed) status = 'failed'
    if (s.deleted && !['completed', 'failed', 'killed'].includes(status)) status = 'killed'
    if (s.outcome && !s.isBusy) status = s.outcome
    const done = ['completed', 'failed', 'killed'].includes(status)
    const waiting = [...asks.values()].find(a => a.origin === id && !a.settled.signal.aborted)
    const tools = parts.filter(p => p.type === 'tool')
    const activeTool = tools.findLast(p => ['running', 'pending'].includes((p.state as ToolState)?.status ?? '') &&
      Math.max((p.state as ToolState)?.time?.start ?? 0, s.messages?.get(String(p.messageID))?.time?.created ?? 0) >= runAt)
    const doing = done ? '' : waiting ? `Waiting for ${waiting.tool}` : s.waitingFor || (activeTool ? String((activeTool.state as { title?: string })?.title || activeTool.tool || '') : '')
    if (!done && (waiting || s.waitingFor)) status = 'waiting'
    const fallback = messages.findLast(m => m.role === 'assistant' && !m.summary && (m.time?.created ?? 0) >= runAt && text(m))
    const selectedModel = s.model ?? task?.model
    const firstTask = tasks.find(t => t.prompt)
    const firstUser = users[0]
    const prompt = firstTask && (!firstUser || firstTask.start <= (firstUser.time?.created ?? 0)) ? firstTask.prompt : firstUser ? text(firstUser) : ''
    return {
      subagent: {
        id, parentId: s.parent === root ? '' : s.parent!, depth,
        type: runes(task?.type || s.agent || '', 80), description: runes(task?.description || s.title, 1000),
        background: task?.background ?? false, done, status, doing: runes(doing, 300),
        startedAt: new Date(s.startedAt ?? tasks[0]?.start ?? messages[0]?.time?.created ?? 0).toISOString(),
        activeAt: new Date(Math.max(s.activeAt ?? 0, task?.end ?? task?.start ?? 0, resultAt)).toISOString(),
        model: runes(selectedModel ? `${selectedModel.providerID}/${selectedModel.modelID}` : '', 200), effort: runes(s.variant ?? '', 80),
        tools: tools.length, ...totals([s]),
      },
      report: {
        prompt: runes(prompt, 60000),
        report: runes(result?.text ?? (fallback ? text(fallback) : ''), 60000), handed: !!result,
      },
    }
  }

  function childRecords(root: string) {
    const selected = sessions.get(root)!.retainedChildren ??= new Set<string>()
    const children = descendants(root).filter(({ id, s }) => s.hydrated && validID(root) && validID(id) && validID(s.parent))
    for (const { id } of children) if (selected.size < 200) selected.add(id)
    return children.filter(({ id }) => selected.has(id)).map(({ id, s, depth }) => ({ s, ...childRecord(root, id, s, depth) }))
  }

  async function sendChildren(root: string, epoch?: string) {
    if (!openRoot(root)) return
    const parent = sessions.get(root)!
    if (epoch && parent.epoch !== epoch) {
      for (const { s } of descendants(root)) {
        if (s.retained) s.retained.delivered = false
        s.deliveredReport = undefined
      }
    }
    parent.epoch = epoch ?? parent.epoch
    for (const { s, ...record } of childRecords(root)) {
      if (!openRoot(root)) return
      const fingerprint = JSON.stringify(record)
      if (s.retained?.fingerprint !== fingerprint) s.retained = { fingerprint, revision: Math.max(1, Date.now(), (s.retained?.revision ?? 0) + 1) }
      const retained = s.retained
      if (retained.delivered) continue
      const reportFingerprint = JSON.stringify(record.report)
      const body = { agent: AGENT, sessionId: root, revision: retained.revision, subagent: record.subagent,
        ...(reportFingerprint !== s.deliveredReport ? { report: record.report } : {}) }
      // Two 60k-rune strings escape to at most 720k bytes, below the 1 MiB envelope.
      const delivered = await inOrder(async () => {
        if (!openRoot(root)) return
        await post('/api/bridge/subagent', body)
        return true
      })
      if (delivered) {
        retained.delivered = true
        s.deliveredReport = reportFingerprint
      }
    }
  }

  function history(id: string, messages: readonly TranscriptMessage[]) {
    const s = sessions.get(id)
    return messages.map(m => ({
      info: s?.messages?.get(m.info.id ?? '') ?? m.info,
      parts: m.parts.map(p => p.type === 'tool' && p.tool === 'task' ? p : s?.parts?.get(String(p.id)) ?? p),
    }))
  }

  async function hydrate(id: string, s: Known, discovery?: Discovery, refresh = false) {
    if (refresh) {
      await s.hydrating
      s.hydrated = false
    }
    if (s.hydrated || s.deleted || stopped.signal.aborted) return
    if (s.hydrating) return s.hydrating
    s.hydrating = (async () => {
      const generation = s.historyGeneration ?? 0
      const promptBefore = s.promptID
      const messagesBefore = refresh ? new Map(s.messages) : undefined
      const partsBefore = refresh ? new Map(s.parts) : undefined
      const records = await native.messages(id, signal())
      if (s.deleted || stopped.signal.aborted || generation !== (s.historyGeneration ?? 0)) return
      // Refresh persisted records without overwriting events received during the read.
      if (refresh) {
        for (const [key, message] of messagesBefore ?? []) if (s.messages?.get(key) === message) {
          s.messages.delete(key)
          s.usage?.delete(key)
        }
        for (const [key, part] of partsBefore ?? []) if (s.parts?.get(key) === part) {
          s.parts.delete(key)
          s.tasks?.delete(key)
          s.notifications?.delete(key)
        }
        if (s.promptID === promptBefore) {
          s.promptID = undefined; s.promptAt = undefined; s.prompt = ''; s.reply = ''; s.replyID = undefined
          s.said = undefined
        }
      }
      const messages = history(id, records).sort((a, b) => (a.info.time?.created ?? 0) - (b.info.time?.created ?? 0))
      for (const m of messages) {
        const info = m.info as Message
        rememberMessage(s, info, true)
        if (info.compaction && (info.time?.created ?? 0) >= (s.contextClearedAt ?? 0)) {
          s.contextClearedAt = info.time?.created ?? 0
          s.contextTokens = 0
          s.compacted = { id: info.id, auto: info.compaction.auto, at: new Date(s.contextClearedAt).toISOString() }
        }
        for (const part of m.parts) rememberPart(id, s, part, true, discovery)
        if (s.parent) continue
        const original = m
        const at = original.info.time?.created ?? 0
        if (info.role === 'user' && at > (s.promptAt ?? 0)) {
          const prompt = textOf(original.parts)
          if (prompt) {
            s.prompt = prompt
            s.promptID = original.info.id
            s.promptAt = at
            if (at >= (s.metadataAt ?? 0)) {
              s.metadataAt = at
              s.agent = info.agent ?? s.agent
              model(s, info.model)
              s.variant = info.model?.variant ?? s.variant
              s.selectionRevision = ++selectionRevision
            }
            s.reply = ''
            s.said = undefined
          }
        }
        if (info.role === 'assistant' && !info.summary && at >= (s.promptAt ?? 0) && textOf(m.parts)) {
          s.reply = textOf(m.parts)
          s.replyID = info.id
          if (info.id) (s.said ??= new Map()).set(info.id, s.reply)
        }
      }
      s.hydrated = true
    })().catch(() => {}).finally(() => { s.hydrating = undefined })
    return s.hydrating
  }

  function snapshot(id: string, s: Known) {
    const children = descendants(id).map(child => child.s)
    return {
      status: s.status ?? (s.isBusy ? 'working' : 'idle'), waitingFor: s.waitingFor ?? '',
      startedAt: s.startedAt, activeAt: s.activeAt, promptAt: s.promptAt,
      lastPrompt: s.prompt.slice(0, 8000), lastReply: s.reply.slice(-REPLY_LIMIT), ...kept([...(s.said?.values() ?? [])]),
      model: s.model ? `${s.model.providerID}/${s.model.modelID}` : '', reasoning: s.variant ?? '',
      contextWindow: s.contextWindow ?? 0, contextTokens: s.contextTokens ?? 0,
      ...(s.hydrated && s.treeReady && children.every(child => child.hydrated) ? totals([s, ...children]) : {}), compacted: s.compacted,
    }
  }

  function tell(id: string, s: Known) {
    inOrder(async () => {
      if (!openRoot(id)) return
      await post('/api/bridge/event', { agent: AGENT, sessionId: id, type: 'snapshot', snapshot: snapshot(id, s) })
    })
  }

  /** The text the session's last turn ended on, or nothing if the turn wrote none. */
  async function lastAnswer(id: string) {
    const messages = await native.messages(id, signal(), 20)
    for (const m of messages.reverse()) {
      if (m.info.role === 'user') break
      if (m.info.role !== 'assistant') continue
      const text = m.info.summary ? '' : textOf(m.parts)
      if (text) return { text, id: m.info.id }
    }
    return { text: '', id: undefined }
  }

  async function refreshAnswer(id: string, s: Known) {
    if (!s.needsAnswer || !openRoot(id)) return
    const at = s.promptAt
    const answer = await lastAnswer(id)
    if (!openRoot(id) || s.isBusy || s.promptAt !== at) return
    s.reply = answer.text
    s.replyID = answer.id
    if (answer.id && answer.text) (s.said ??= new Map()).set(answer.id, answer.text)
    s.needsAnswer = false
  }

  async function deliver(id: string, s: Known, reply: Reply) {
    let state = 'dropped'
    try {
      if (!openRoot(id)) return
      await ctx.session.prompt({ sessionID: id, text: reply.text }, { signal: signal(managed ? managedActivity.signal : undefined) })
      state = 'delivered'
    } finally {
      acknowledgements.set(reply.id, state)
    }
  }

  async function pollSession(id: string, s: Known) {
    if (!s.isLive || !openRoot(id) || s.polling) return
    quotas.activate(id)
    s.polling = true
    try {
      const polled = await inOrder(async () => {
        if (!openRoot(id)) return
        const agents = childRecords(id).map(({ subagent: a }) => ({ id: a.id, status: a.status, doing: a.doing, parentId: a.parentId }))
        const providerLimits = quotas.snapshot(id)
        const claimed = await post('/api/bridge/poll', {
          agent: AGENT, sessionId: id, cwd: s.cwd, title: s.title, branch, snapshot: snapshot(id, s),
          ...(agents.length ? { agents } : {}),
          pluginVersion: VERSION, pluginRevision: String(options?.revision ?? ''),
          pluginTui: presence.fresh(id),
          ...(providerLimits !== undefined ? { providerLimits, ...quotas.metadata(id) } : {}),
        })
        return { claimed }
      })
      if (!openRoot(id) || !polled) return
      const claimed = polled.claimed
      if (claimed) {
        for (const ask of asks.values()) {
          if (ask.sessionId === id && (claimed.epoch ? ask.epoch !== claimed.epoch : claimed.fresh)) ask.slorpId = undefined
        }
      }
      for (const reply of (claimed?.replies ?? []) as Reply[]) {
        if (!openRoot(id)) return
        await deliver(id, s, reply).catch(() => {})
      }
      if (openRoot(id)) await sendChildren(id, claimed?.epoch)
    } finally {
      await flushAcknowledgements()
      s.polling = false
      for (const [id, ask] of asks) if (!ask.running) void offerAgain(id, ask).catch(() => {})
    }
  }

  async function refreshRoot(id: string, s: Known) {
    if (!s.isLive || !openRoot(id)) return
    if (s.refreshing) return pollSession(id, s).catch(() => {})
    s.refreshing = true
    try {
      await hydrateTree(id, s)
      for (const node of [{ id, s }, ...descendants(id)]) {
        if (node.s.deleted || waitingScans.has(node.id)) continue
        waitingScans.add(node.id)
        const scan = ++settlementRevision
        activeScans.add(scan)
        void Promise.allSettled([
          ctx.permission.list({ sessionID: node.id }, { signal: signal() }).then(requests => {
            for (const p of requests) if (!stopped.signal.aborted && (settledRequests.get(p.id) ?? 0) <= scan)
              void offer(p.id, p.sessionID, p.action, describe(p), true).catch(() => {})
          }),
          native.forms(node.id, signal()).then(forms => {
            for (const form of forms) if (!stopped.signal.aborted && (settledRequests.get(form.id) ?? 0) <= scan) offerForm(form)
          }),
        ]).then(results => {
          if (results.some(result => result.status === 'rejected')) waitingScans.delete(node.id)
        }).finally(() => {
          activeScans.delete(scan)
          const oldest = Math.min(...activeScans)
          for (const [id, revision] of settledRequests) if (revision <= oldest) settledRequests.delete(id)
        })
      }
      if (!openRoot(id)) return
      await refreshAnswer(id, s).catch(() => {})
      if (!openRoot(id)) return
      await pollSession(id, s)
    } catch {
      // Other roots refresh independently; this one retries on the next tick.
    } finally {
      s.refreshing = false
    }
  }

  async function poll() {
    for (const [id, ask] of asks) if (!ask.running) void offerAgain(id, ask).catch(() => {})
    void flushSettlements()
    void flushAcknowledgements()
    await Promise.all([...sessions].map(([id, s]) => refreshRoot(id, s)))
  }

  async function flushAcknowledgements() {
    if (acknowledging) return
    acknowledging = true
    try {
      for (const [id, state] of acknowledgements) {
        try {
          await post('/api/bridge/ack', { id, state })
        } catch (error) {
          if (!(error instanceof BridgeHTTPError && error.status === 409)) throw error
        }
        if (acknowledgements.get(id) === state) acknowledgements.delete(id)
      }
    } catch {
      // Other failures retry without resubmitting replies.
    } finally {
      acknowledging = false
    }
  }

  /** Answers a permission prompt for this one call, or rejects it. */
  async function answer(sessionId: string, requestId: string, allow: boolean, message?: string) {
    const activity = managed ? managedActivity.signal : undefined
    await ctx.permission.reply({ sessionID: sessionId, requestID: requestId, decision: allow ? 'once' : 'reject', message }, { signal: signal(activity) })
  }

  /** Records that OpenCode is no longer waiting on a request, and tells Slorp if it knew of it. */
  function settle(requestId: string) {
    if (activeScans.size) settledRequests.set(requestId, ++settlementRevision)
    const ask = asks.get(requestId)
    if (!ask) return
    asks.delete(requestId)
    ask.settled.abort()
    if (ask.slorpId) finish(ask.slorpId)
  }

  function finish(id: string) {
    settlements.add(id)
    void flushSettlements()
  }

  async function flushSettlements() {
    if (settling) return
    settling = true
    try {
      for (const id of settlements) {
        try {
          await post('/api/bridge/permission/done', { id })
        } catch (error) {
          if (!(error instanceof BridgeHTTPError && error.status === 409)) throw error
        }
        settlements.delete(id)
      }
    } catch {
      // Other failures keep settlements queued for retry.
    } finally {
      settling = false
    }
  }

  /**
   * Shows Slorp a request the session `origin` is waiting on, under the
   * top-level session it belongs to. When Slorp may answer it, waits for the
   * user's allow or deny there and passes it on, unless the request is
   * settled in OpenCode first.
   */
  async function offer(requestId: string, origin: string, tool: string, full: string, canAnswer: boolean, form?: FormInfo) {
    if (asks.has(requestId)) return
    const ask: Ask = { sessionId: origin, origin, tool, full, canAnswer, form, settled: new AbortController(), deadline: Date.now() + ANSWER_MAX_MS }
    asks.set(requestId, ask)
    await offerAgain(requestId, ask)
  }

  async function offerAgain(requestId: string, ask: Ask) {
    if (!managedOpen()) return
    if (Date.now() >= ask.deadline) { settle(requestId); return }
    if (ask.running || ask.slorpId || ask.settled.signal.aborted) return
    ask.running = true
    const attempt = ask.attempt = Symbol()
    try {
      await present(requestId, ask, attempt)
    } catch {
      if (ask.attempt === attempt) {
        if (ask.slorpId && !ask.settled.signal.aborted) finish(ask.slorpId)
        ask.slorpId = undefined
      }
    } finally {
      if (ask.attempt === attempt) {
        ask.running = false
        ask.attempt = undefined
      }
    }
  }

  async function present(requestId: string, ask: Ask, attempt: symbol) {
    const activity = managedActivity
    const current = () => ask.attempt === attempt && !ask.settled.signal.aborted && !stopped.signal.aborted &&
      (!managed || activity === managedActivity && !activity.signal.aborted)
    if (!ask.resolved) {
      const [id, s] = await top(ask.origin)
      if (!current()) return
      if (s.parent || s.deleted || sessions.get(ask.origin)?.deleted) { settle(requestId); return }
      if (!openRoot(id)) return
      ask.sessionId = id
      ask.resolved = true
      s.isLive = true
    }
    if ([...asks.values()].some(other => other !== ask && other.resolved && other.sessionId === ask.sessionId && (other.running || other.slorpId))) return
    const { sessionId, origin, tool, full, canAnswer } = ask
    const questions = tool === 'question' ? JSON.parse(full).questions : undefined
    const summary = questions ? questions.map((q: { question: string }) => q.question).join('\n') : full
    const asked = await post('/api/bridge/permission', {
      agent: AGENT,
      sessionId,
      tool,
      summary: summary.slice(0, SUMMARY_LIMIT),
      input: full.slice(0, INPUT_LIMIT),
      canAnswer: canAnswer && full.length <= INPUT_LIMIT,
      question: tool === 'question',
      ...(questions ? { ordered: true } : {}),
      ...(origin !== sessionId ? { agentId: origin } : {}),
    })
    if (!asked) return
    if (!current()) {
      if (asked.id !== ask.slorpId || ask.settled.signal.aborted) finish(asked.id)
      return
    }
    ask.slorpId = asked.id
    ask.epoch = asked.epoch
    if (!asked.answerable || !canAnswer || full.length > INPUT_LIMIT) return

    for (; current() && ask.slorpId === asked.id && Date.now() < ask.deadline; ) {
      const decided = await post('/api/bridge/permission/wait', { id: asked.id }, AbortSignal.any([ask.settled.signal, AbortSignal.timeout(25_000)]))
      if (!current() || ask.slorpId !== asked.id || !openRoot(sessionId)) return
      if (decided?.state === 'waiting') continue
      if (decided?.state === 'allow' || decided?.state === 'deny') {
        const said = decided.message ? `The user denied this in Slorp and said: ${decided.message}` : undefined
        if (ask.form) {
          if (decided.state === 'allow' && !Array.isArray(decided.selections)) throw new Error('Missing question selections')
          await native.form(origin, requestId, decided.state === 'allow' ? formAnswer(ask.form, decided.selections) : undefined,
            signal(AbortSignal.any([ask.settled.signal, ...(managed ? [activity.signal] : [])])), said)
        } else {
          await answer(origin, requestId, decided.state === 'allow', said)
        }
        if (current()) settle(requestId)
      } else {
        ask.slorpId = undefined
      }
      return
    }
    if (current() && ask.slorpId === asked.id) settle(requestId)
  }

  const timer = setInterval(() => tick().catch(() => {}), POLL_MS)
  timer.unref?.()
  void ctx.vcs
    .get()
    .then(res => (branch = res.data?.branch?.current ?? ''))
    .catch(() => {})

  const bridge = {
    dispose: async () => {
      clearInterval(timer)
      presence.dispose()
      stopped.abort()
      native.dispose()
      quotas.dispose()
      quotaObservations.clear()
      for (const id of asks.keys()) settle(id)
    },

    prompt: async (input: { sessionID: string; agent?: string; model?: Known['model']; variant?: string }, output: { message: Message & { id: string; time: { created: number } }; parts: Part[] }) => {
      const [id, s] = await top(input.sessionID)
      if (!localRoot(id)) return
      const origin = sessions.get(input.sessionID)!
      if (origin.deleted) return
      const message = { ...output.message, role: 'user' }
      rememberMessage(origin, message)
      const parts: Part[] = []
      for (const [i, part] of output.parts.entries()) {
        const partID = String(part.id ?? `${message.id}:${i}`)
        parts.push(part as Part)
        rememberPart(input.sessionID, origin, { ...part, id: partID, messageID: message.id } as Part)
      }
      if (id !== input.sessionID) {
        s.isLive = true
        origin.isBusy = true
        origin.statusKnown = true
        origin.waitingFor = ''
        model(origin, input.model ?? output.message?.model)
        origin.variant = input.variant ?? origin.variant
        origin.selectionRevision = ++selectionRevision
        return
      }
      if (!textOf(parts) && parts.some(p => p.type === 'text' && p.synthetic)) return
      s.isLive = true
      s.isBusy = true
      s.status = 'working'
      s.waitingFor = ''
      s.agent = input.agent ?? output.message?.agent ?? s.agent
      model(s, input.model ?? output.message?.model)
      s.variant = input.variant ?? s.variant
      s.prompt = textOf(parts)
      s.promptID = message.id
      s.promptAt = output.message?.time.created ?? Date.now()
      s.metadataAt = Math.max(s.metadataAt ?? 0, s.promptAt)
      s.selectionRevision = ++selectionRevision
      s.activeAt = s.promptAt
      s.reply = ''
      s.said = undefined
      s.needsAnswer = false
      report(id, 'turn.start', s.prompt, s.promptAt)
    },

    context: async (input: { sessionID: string; model: ModelRef }) => {
      const observation = quotas.nextSequence(input.sessionID)
      quotaObservations.set(input.sessionID, observation)
      void top(input.sessionID).then(async ([id, s]) => {
        if (!localRoot(id) || sessions.get(input.sessionID)?.deleted || quotaObservations.get(input.sessionID) !== observation) return
        const models = await ctx.model.list()
        const selected = models.data.find(m => m.providerID === input.model.providerID && m.id === input.model.id)
        if (!selected) return
        if (id === input.sessionID) s.contextWindow = selected.limit.context
        const provider = await ctx.provider.get({ providerID: input.model.providerID })
        const source = await quotaSource(ctx, provider.data, selected, undefined, input.model)
        if (quotaObservations.get(input.sessionID) !== observation || stopped.signal.aborted) return
        const deferred = managed && !managedActivity.signal.aborted && (managedRoot === undefined || managedAvailable && managedRoot === id)
        quotas.observe(id, input.sessionID, source, deferred, observation)
      }).catch(() => {})
    },

    transition: async (e: { type: 'session.deleted' | 'session.compacted' | 'session.status' | 'session.idle'; data: { sessionID: string; status?: { type: string; message?: string } } }) => {
      const p = e.data
      const pendingAtIdle = e.type === 'session.idle' ? [...asks.entries()] : []
      const key = p.sessionID
      const apply = async () => {
        switch (e.type) {
          case 'session.deleted': {
            const id = p.sessionID
            const deleted = new Set([id, ...descendants(id).map(child => child.id)])
            for (const [requestId, ask] of asks) if (deleted.has(ask.origin)) settle(requestId)
            for (const id of deleted) {
              quotas.clear(id)
              quotaObservations.delete(id)
              deletedSessions.add(id)
              const s = sessions.get(id)
              if (s) { s.deleted = true; s.isBusy = false }
            }
            quotas.remove(...deleted)
            break
          }
          case 'session.compacted': {
            const s = sessions.get(p.sessionID)
            if (!s?.isLive || s.parent || s.deleted) break
            s.compacted = { id: s.compacting?.id, at: new Date().toISOString(), auto: s.compacting?.auto, before: s.compacting?.before }
            s.compacting = undefined
            s.contextTokens = 0
            tell(p.sessionID, s)
            break
          }
          case 'session.status': {
            if (!p.sessionID) break
            const [id, s] = await top(p.sessionID)
            if (!localRoot(id)) break
            if (id !== p.sessionID) {
              const origin = sessions.get(p.sessionID)!
              if (origin.deleted) break
              s.isLive = true
              origin.isBusy = p.status?.type !== 'idle'
              origin.statusKnown = true
              origin.waitingFor = p.status?.type === 'retry' ? `Retrying: ${p.status.message ?? ''}` : ''
              origin.activeAt = Date.now()
              break
            }
            if (p.status?.type === 'idle') break
            s.isLive = true
            s.isBusy = true
            s.status = 'working'
            s.waitingFor = p.status?.type === 'retry' ? `Retrying: ${p.status.message ?? ''}` : ''
            s.activeAt = Date.now()
            tell(id, s)
            break
          }
          case 'session.idle': {
            const s = sessions.get(p.sessionID)
            if (s?.parent) {
              if (!s.deleted) { s.isBusy = false; s.waitingFor = ''; s.statusKnown = true }
              for (const [requestId, ask] of pendingAtIdle) if (asks.get(requestId) === ask && ask.origin === p.sessionID) settle(requestId)
              break
            }
            if (!s?.isLive || s.deleted) break
            s.isBusy = false
            if (s.status !== 'failed') s.status = 'idle'
            if (s.status !== 'failed') s.waitingFor = ''
            s.activeAt = Date.now()
            s.needsAnswer = true
            for (const [requestId, ask] of pendingAtIdle) {
              if (asks.get(requestId) === ask && ask.origin === p.sessionID) settle(requestId)
            }
            inOrder(async () => {
              if (!openRoot(p.sessionID)) return
              await refreshAnswer(p.sessionID, s)
              if (!openRoot(p.sessionID)) return
              await post('/api/bridge/event', { agent: AGENT, sessionId: p.sessionID, type: 'snapshot', snapshot: snapshot(p.sessionID, s) })
            })
            break
          }
        }
      }
      if (e.type === 'session.deleted') { await apply(); return }
      await enqueue(key, apply)
    },
  }

  function offerForm(form: FormInfo) {
    const { questions, canAnswer } = formQuestions(form)
    void offer(form.id, form.sessionID, 'question', JSON.stringify({ questions }), canAnswer, form).catch(() => {})
  }

  function refreshHistory(sessionID: string) {
    dirtyHistory.add(sessionID)
    if (refreshingHistory.has(sessionID)) return
    refreshingHistory.add(sessionID)
    void enqueue(`native:${sessionID}`, async () => {
      dirtyHistory.delete(sessionID)
      if (stopped.signal.aborted || deletedSessions.has(sessionID)) return
      const [root, parent] = await top(sessionID)
      const s = sessions.get(sessionID)!
      parent.isLive = true
      invalidateTree(sessionID)
      await hydrate(sessionID, s, undefined, true)
      if (openRoot(root)) tell(root, parent)
    }).catch(() => {}).finally(() => {
      refreshingHistory.delete(sessionID)
      if (dirtyHistory.has(sessionID) && !stopped.signal.aborted && !deletedSessions.has(sessionID)) refreshHistory(sessionID)
    })
  }

  async function receive(event: OpenCodeEvent) {
    if (stopped.signal.aborted) return
    const origin = 'sessionID' in event.data && typeof event.data.sessionID === 'string' ? event.data.sessionID : undefined
    if (event.location && event.location.directory !== directory && !sessions.get(origin ?? '')?.parent &&
      !(event.type === 'session.moved' && sessions.has(origin ?? ''))) return
    if (event.id && received.has(event.id)) return
    if (event.id) {
      received.add(event.id)
      if (received.size > 4096) received.delete(received.values().next().value!)
    }
    switch (event.type) {
      case 'permission.replied': settle(event.data.requestID); return
      case 'form.replied':
      case 'form.cancelled': settle(event.data.id); return
      case 'form.created': offerForm(event.data.form); return
      case 'permission.asked':
        void offer(event.data.id, event.data.sessionID, event.data.action, describe(event.data), true).catch(() => {})
        return
      case 'session.created':
        if (event.data.location.directory !== directory && !sessions.has(event.data.parentID ?? '')) return
        learn({ id: event.data.sessionID, ...event.data, directory: event.data.location.directory,
          time: { created: event.created, updated: event.created } })
        return
      case 'session.deleted':
        await bridge.transition({ type: 'session.deleted', data: event.data })
        return
      case 'vcs.branch.updated':
        branch = event.data.branch ?? ''
        return
      case 'credential.updated':
      case 'credential.switched':
      case 'provider.updated':
      case 'integration.updated':
      case 'model.updated':
        for (const id of sessions.keys()) quotas.clear(id)
        quotaObservations.clear()
        return
    }
    if (!('sessionID' in event.data) || typeof event.data.sessionID !== 'string') return
    const sessionID = event.data.sessionID
    if (deletedSessions.has(sessionID)) return
    if (event.type === 'session.moved') {
      const s = sessions.get(sessionID)
      if (s) {
        s.cwd = event.data.location.directory
        s.historyGeneration = (s.historyGeneration ?? 0) + 1
        s.hydrated = false
        if (!s.parent && s.cwd !== directory) quotas.clear(sessionID)
      }
    }
    if (['session.step.ended', 'session.step.failed', 'session.tool.success', 'session.tool.failed',
      'session.tool.progress', 'session.synthetic', 'session.text.ended'].includes(event.type)) {
      refreshHistory(sessionID)
      return
    }
    if (event.type === 'session.revert.committed') {
      const s = sessions.get(sessionID)
      if (s) { s.historyGeneration = (s.historyGeneration ?? 0) + 1; s.hydrated = false }
    }
    if (!['session.inbox.delivered', 'session.execution.started', 'session.execution.succeeded', 'session.execution.failed',
      'session.execution.interrupted', 'session.retry.scheduled', 'session.status', 'session.renamed', 'session.agent.selected',
      'session.model.selected', 'session.moved', 'session.compaction.started', 'session.compaction.ended',
      'session.compaction.failed', 'session.step.started', 'session.revert.committed'].includes(event.type)) return
    const ended = event.type === 'session.execution.succeeded' || event.type === 'session.execution.failed' || event.type === 'session.execution.interrupted'
    if (ended) for (const [id, ask] of asks) if (ask.origin === sessionID) settle(id)
    await enqueue(`native:${sessionID}`, async () => {
      if (stopped.signal.aborted || deletedSessions.has(sessionID)) return
      const [root, parent] = await top(sessionID)
      const s = sessions.get(sessionID)!
      if (s.deleted) return
      switch (event.type) {
        case 'session.inbox.delivered': {
          const message = await native.message(sessionID, event.data.inboxID, signal())
          if (message.info.role !== 'user' || !message.info.id || !message.info.time) return
          await bridge.prompt({ sessionID }, { message: { ...message.info, id: message.info.id, time: message.info.time }, parts: message.parts })
          break
        }
        case 'session.execution.started':
        case 'session.retry.scheduled':
        case 'session.status':
          await bridge.transition({ type: 'session.status', data: { sessionID, status:
            event.type === 'session.status' ? event.data.status :
            event.type === 'session.retry.scheduled' ? { type: 'retry', message: event.data.error.message } : { type: 'busy' } } })
          break
        case 'session.renamed':
          s.title = event.data.title
          break
        case 'session.agent.selected':
          s.agent = event.data.agent
          s.metadataAt = Math.max(s.metadataAt ?? 0, event.created)
          s.selectionRevision = ++selectionRevision
          break
        case 'session.model.selected':
          model(s, { providerID: event.data.model.providerID, modelID: event.data.model.id })
          s.variant = event.data.model.variant
          s.metadataAt = Math.max(s.metadataAt ?? 0, event.created)
          s.selectionRevision = ++selectionRevision
          break
        case 'session.moved':
          s.cwd = event.data.location.directory
          break
        case 'session.compaction.started':
          s.compacting = { id: event.id, auto: event.data.reason === 'auto', before: s.contextTokens ?? 0 }
          break
        case 'session.compaction.ended':
          s.contextClearedAt = event.created
          await bridge.transition({ type: 'session.compacted', data: { sessionID } })
          s.hydrated = false
          invalidateTree(sessionID)
          break
        case 'session.compaction.failed':
          s.compacting = undefined
          break
        case 'session.execution.failed':
        case 'session.execution.interrupted':
          s.status = event.type === 'session.execution.failed' ? 'failed' : 'idle'
          s.waitingFor = event.type === 'session.execution.failed' ? event.data.error.message : ''
          s.isBusy = false
          if (s.parent) s.outcome = event.type === 'session.execution.failed' ? 'failed' : 'killed'
          // Fall through to hydrate the final persisted answer and usage.
        case 'session.execution.succeeded':
          if (event.type === 'session.execution.succeeded' && s.parent) s.outcome = 'completed'
          parent.isLive = true
          await hydrate(sessionID, s, undefined, true)
          await bridge.transition({ type: 'session.idle', data: { sessionID } })
          invalidateTree(sessionID)
          break
        case 'session.step.started':
          parent.isLive = true
          s.outcome = undefined
          s.isBusy = true
          s.statusKnown = true
          await bridge.context({ sessionID, model: event.data.model })
          break
        case 'session.revert.committed':
          s.messages?.clear()
          s.parts?.clear()
          s.usage?.clear()
          s.tasks?.clear()
          s.notifications?.clear()
          s.prompt = ''; s.reply = ''; s.promptAt = undefined
          s.promptID = undefined; s.replyID = undefined; s.said = undefined
          s.contextTokens = 0; s.measuredAt = undefined; s.contextClearedAt = undefined
          s.compacted = undefined; s.compacting = undefined
          await hydrate(sessionID, s, undefined, true)
          invalidateTree(sessionID)
          break
      }
      if (openRoot(root)) tell(root, parent)
    })
  }

  try {
    const rpc = await ctx.rpc.register(PresenceRPC, { presence: async input => {
      receivePresence((input as { command: unknown }).command)
      return null
    }, result: async input => { native.result(input); return null } })
    native.relay((type, data) => rpc.events.emit(type, data))
    await ctx.session.hook('context', input => bridge.context(input))
  } catch (error) {
    await bridge.dispose()
    throw error
  }
  void (async () => {
    while (!stopped.signal.aborted) {
      try {
        for await (const event of ctx.event.subscribe({ signal: stopped.signal })) void receive(event).catch(() => {})
      } catch {
        // Reconnect after a transient subscription failure.
      }
      if (stopped.signal.aborted) break
      await new Promise<void>(resolve => {
        const done = () => { clearTimeout(timer); stopped.signal.removeEventListener('abort', done); resolve() }
        const timer = setTimeout(done, POLL_MS)
        timer.unref?.()
        stopped.signal.addEventListener('abort', done, { once: true })
      })
    }
  })()
  if (!managed) void discoverActive()
  return bridge.dispose
}

export default { id: 'slorp', setup: Slorp } satisfies Plugin.Plugin
