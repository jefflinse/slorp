import type { Plugin } from '@opencode-ai/plugin'
import { BroadcastChannel } from 'node:worker_threads'

const AGENT = 'opencode'
const VERSION = '0.2.0'
const DEFAULT_URL = 'http://127.0.0.1:4747'
const POLL_MS = 2000
const REQUEST_MS = 5000
const SUMMARY_LIMIT = 400
/** Inputs longer than this are too much to judge in Slorp; the terminal asks. */
const INPUT_LIMIT = 8000
/** How long Slorp may answer a prompt before only the terminal can. */
const ANSWER_MAX_MS = 60 * 60_000
/** The title OpenCode gives a session before it has written one. */
const UNTITLED = /^New session - \d{4}-/

type Reply = { id: string; text: string }

/** The fields read from OpenCode's own record of a session. */
type Info = {
  id: string
  parentID?: string
  title?: string
  directory?: string
  agent?: string
  model?: { id?: string; providerID?: string; variant?: string }
  time?: { created: number; updated: number }
}

type Usage = { input: number; output: number; reasoning?: number; total?: number; cache: { read: number; write: number } }
type Message = {
  id?: string
  role: string
  time?: { created: number; completed?: number }
  agent?: string
  model?: { providerID: string; modelID: string; variant?: string }
  providerID?: string
  modelID?: string
  variant?: string
  tokens?: Usage
  cost?: number
  summary?: boolean
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
  waitingFor?: string
  startedAt?: number
  activeAt?: number
  promptAt?: number
  contextWindow?: number
  contextTokens?: number
  measuredAt?: number
  compacted?: { at: string; auto?: boolean; before?: number }
  compacting?: { auto: boolean; before: number }
  usage?: Map<string, { tokens: Usage; cost: number }>
  hydrated?: boolean
  hydrating?: Promise<void>
  needsAnswer?: boolean
}

/** A permission prompt or question OpenCode is waiting on. */
type Ask = {
  sessionId: string
  slorpId?: string
  epoch?: string
  settled: AbortController
  running?: boolean
  resolved?: boolean
  origin: string
  tool: string
  full: string
  canAnswer: boolean
  deadline: number
}

type Part = { type: string; text?: string; synthetic?: boolean }

function textOf(parts: readonly Part[]) {
  return parts
    .filter(p => p.type === 'text' && !p.synthetic && p.text?.trim())
    .map(p => p.text!.trim())
    .join('\n\n')
}

/** What a permission prompt asks to do, as the user should read it in Slorp. */
function describe(request: { patterns?: string[]; metadata?: Record<string, unknown> }) {
  const meta = request.metadata ?? {}
  if (typeof meta.command === 'string') return meta.command
  if (typeof meta.diff === 'string') return meta.diff
  const rest = Object.keys(meta).length > 0 ? [JSON.stringify(meta, null, 2)] : []
  return [...(request.patterns ?? []), ...rest].join('\n')
}

/**
 * Bridges the sessions of one OpenCode instance to the Slorp service at the
 * `url` option, or at SLORP_URL when that is set. A top-level session is
 * reported once something happens in it, with its current state and usage
 * repeated on every poll. Replies are submitted under its selected agent,
 * model and variant. Permissions and questions, including subagents', can
 * be answered in Slorp or OpenCode; Slorp only ever permits a single call.
 */
export const Slorp: Plugin = async ({ client, directory }, options) => {
  const base = (process.env.SLORP_URL || String(options?.url ?? DEFAULT_URL)).replace(/\/+$/, '')
  const sessions = new Map<string, Known>()
  const asks = new Map<string, Ask>()
  let branch = ''
  let isPolling = false
  let sending: Promise<unknown> = Promise.resolve()
  const stopped = new AbortController()
  const acknowledgements = new Map<string, string>()
  const settlements = new Set<string>()
  const events = new Map<string, Promise<void>>()
  let settling = false
  const signal = () => AbortSignal.any([stopped.signal, AbortSignal.timeout(REQUEST_MS)])
  const presence = new BroadcastChannel('slorp.opencode.presence')
  const discovering = new Set<string>()
  let tuiSeenAt: number | undefined
  presence.unref()
  presence.onmessage = ({ data }) => {
    if (data?.directory !== directory || stopped.signal.aborted) return
    tuiSeenAt = Date.now()
    if (typeof data.sessionId === 'string') void enqueue(data.sessionId, () => discover(data.sessionId)).catch(() => {})
  }

  function enqueue(id: string, step: () => Promise<void>) {
    const next = (events.get(id) ?? Promise.resolve()).then(step)
    events.set(id, next.catch(() => {}))
    return next
  }

  async function discover(id: string) {
    if (sessions.get(id)?.isLive || discovering.has(id)) return
    discovering.add(id)
    try {
      const info = await client.session.get({ path: { id }, signal: signal() })
      if (info.error || !info.data || stopped.signal.aborted) return
      learn(info.data)
      const [root, s] = await top(id)
      const activate = async () => {
        const statuses = await client.session.status({ signal: signal() })
        if (statuses.error || stopped.signal.aborted || s.isLive) return
        const status = statuses.data?.[root]
        s.isBusy = status?.type === 'busy' || status?.type === 'retry'
        s.status = s.isBusy ? 'working' : 'idle'
        s.waitingFor = status?.type === 'retry' ? `Retrying: ${status.message}` : ''
        s.isLive = true
        await poll()
      }
      if (root === id) await activate()
      else await enqueue(root, activate)
    } finally {
      discovering.delete(id)
    }
  }

  async function post(path: string, body: object, signal?: AbortSignal) {
    const res = await fetch(base + path, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
      signal: AbortSignal.any([stopped.signal, signal ?? AbortSignal.timeout(REQUEST_MS)]),
    })
    if (!res.ok) throw new Error(`Slorp: HTTP ${res.status}`)
    const text = await res.text()
    return text ? JSON.parse(text) : undefined
  }

  /** Runs the steps given to it one at a time, so Slorp hears of turns in the order they happened. */
  function inOrder<T>(step: () => Promise<T>): Promise<T | undefined> {
    const next = sending.then(step).catch(() => undefined)
    sending = next
    return next
  }

  function report(sessionId: string, type: string, text = '', at?: number) {
    inOrder(() => post('/api/bridge/event', { agent: AGENT, sessionId, type, text, ...(at ? { at } : {}) }))
  }

  function learn(info: Info) {
    const s = sessions.get(info.id) ?? { title: '', cwd: directory, prompt: '', reply: '', isBusy: false, isLive: false }
    s.parent = info.parentID
    if (info.title !== undefined) s.title = UNTITLED.test(info.title) ? '' : info.title
    if (info.directory) s.cwd = info.directory
    if (info.agent) s.agent = info.agent
    if (info.model?.providerID && info.model.id) model(s, { providerID: info.model.providerID, modelID: info.model.id })
    if (info.model) s.variant = info.model.variant
    if (info.time) {
      s.startedAt = info.time.created
      s.activeAt = Math.max(s.activeAt ?? 0, info.time.updated)
    }
    sessions.set(info.id, s)
    return s
  }

  function model(s: Known, next: Known['model']) {
    if (!next) return
    if (s.model?.modelID !== next.modelID || s.model?.providerID !== next.providerID) s.contextWindow = 0
    s.model = { providerID: next.providerID, modelID: next.modelID }
  }

  /** Resolves a session to the top-level session it runs under, which is itself unless it is a subagent's. */
  async function top(id: string): Promise<[string, Known]> {
    for (let hops = 0; ; hops++) {
      const s = sessions.get(id) ?? learn(((await client.session.get({ path: { id }, signal: signal() })).data as Info | undefined) ?? { id })
      if (!s.parent || hops === 20) {
        await hydrate(id, s)
        return [id, s]
      }
      id = s.parent
    }
  }

  function measured(s: Known, info: Message) {
    if (!info.id || info.role !== 'assistant' || !info.tokens) return
    ;(s.usage ??= new Map()).set(info.id, { tokens: info.tokens, cost: info.cost ?? 0 })
    const at = info.time?.created ?? 0
    if (at < (s.measuredAt ?? 0)) return
    s.measuredAt = at
    if (info.providerID && info.modelID) model(s, { providerID: info.providerID, modelID: info.modelID })
    s.variant = info.variant ?? s.variant
    const t = info.tokens
    s.contextTokens = t.total ?? t.input + t.output + (t.reasoning ?? 0) + t.cache.read + t.cache.write
  }

  async function hydrate(id: string, s: Known) {
    if (s.hydrated) return
    if (s.hydrating) return s.hydrating
    s.hydrating = (async () => {
      const res = await client.session.messages({ path: { id }, signal: signal() })
      if (res.error) throw res.error
      for (const m of res.data ?? []) {
        const info = m.info as Message
        if (!s.usage?.has(info.id ?? '')) measured(s, info)
        const at = info.time?.created ?? 0
        if (info.role === 'user' && at > (s.promptAt ?? 0)) {
          const prompt = textOf(m.parts)
          if (prompt) {
            s.prompt = prompt
            s.promptAt = at
            s.agent = info.agent ?? s.agent
            model(s, info.model)
            s.variant = info.model?.variant ?? s.variant
            s.reply = ''
          }
        }
        if (info.role === 'assistant' && !info.summary && at >= (s.promptAt ?? 0)) s.reply = textOf(m.parts) || s.reply
      }
      s.hydrated = true
    })().catch(() => {}).finally(() => { s.hydrating = undefined })
    return s.hydrating
  }

  function snapshot(s: Known) {
    const used = { in: 0, out: 0, cacheRead: 0, cacheWrite: 0 }
    let costUsd = 0
    for (const { tokens: t, cost } of s.usage?.values() ?? []) {
      used.in += t.input
      used.out += t.output + (t.reasoning ?? 0)
      used.cacheRead += t.cache.read
      used.cacheWrite += t.cache.write
      costUsd += cost
    }
    return {
      status: s.status ?? (s.isBusy ? 'working' : 'idle'), waitingFor: s.waitingFor ?? '',
      startedAt: s.startedAt, activeAt: s.activeAt, promptAt: s.promptAt,
      lastPrompt: s.prompt.slice(0, 8000), lastReply: s.reply.slice(-30000),
      model: s.model ? `${s.model.providerID}/${s.model.modelID}` : '', reasoning: s.variant ?? '',
      contextWindow: s.contextWindow ?? 0, contextTokens: s.contextTokens ?? 0,
      ...(s.hydrated ? { used, costUsd } : {}), compacted: s.compacted,
    }
  }

  function tell(id: string, s: Known) {
    const state = snapshot(s)
    inOrder(() => post('/api/bridge/event', { agent: AGENT, sessionId: id, type: 'snapshot', snapshot: state }))
  }

  /** The text the session's last turn ended on, or nothing if the turn wrote none. */
  async function lastAnswer(id: string) {
    const res = await client.session.messages({ path: { id }, query: { limit: 10 }, signal: signal() })
    if (res.error) throw res.error
    for (const m of [...(res.data ?? [])].reverse()) {
      if (m.info.role !== 'assistant') break
      const text = m.info.summary ? '' : textOf(m.parts)
      if (text) return text
    }
    return ''
  }

  async function refreshAnswer(id: string, s: Known) {
    if (!s.needsAnswer) return
    const at = s.promptAt
    const answer = await lastAnswer(id)
    if (s.isBusy || s.promptAt !== at) return
    s.reply = answer
    s.needsAnswer = false
  }

  async function deliver(id: string, s: Known, reply: Reply) {
    let state = 'dropped'
    try {
      const body = { agent: s.agent, model: s.model, ...(s.variant ? { variant: s.variant } : {}), parts: [{ type: 'text' as const, text: reply.text }] }
      const sent = await client.session.promptAsync({ path: { id }, body, signal: signal() })
      state = sent.error ? 'dropped' : 'delivered'
    } finally {
      acknowledgements.set(reply.id, state)
    }
  }

  async function poll() {
    if (isPolling) return
    isPolling = true
    try {
      await sending
      await flushSettlements()
      for (const [id, s] of sessions) {
        if (!s.isLive) continue
        try {
          await hydrate(id, s)
          await refreshAnswer(id, s).catch(() => {})
          const claimed = await inOrder(() => post('/api/bridge/poll', {
            agent: AGENT, sessionId: id, cwd: s.cwd, title: s.title, branch, snapshot: snapshot(s),
            pluginVersion: VERSION, pluginRevision: String(options?.revision ?? ''),
            pluginTui: tuiSeenAt !== undefined && Date.now() - tuiSeenAt < 10_000,
          }))
          if (claimed) {
            for (const ask of asks.values()) {
              if (ask.sessionId === id && (claimed.epoch ? ask.epoch !== claimed.epoch : claimed.fresh)) ask.slorpId = undefined
            }
          }
          for (const reply of (claimed?.replies ?? []) as Reply[]) await deliver(id, s, reply).catch(() => {})
        } catch {
          // Other sessions still get their chance to report.
        }
      }
      for (const [id, state] of acknowledgements) {
        await post('/api/bridge/ack', { id, state })
        acknowledgements.delete(id)
      }
      for (const [id, ask] of asks) if (!ask.running) void offerAgain(id, ask).catch(() => {})
    } catch {
      // Slorp is not running; the next tick tries again.
    } finally {
      isPolling = false
    }
  }

  async function request(url: string, requestId: string, body?: object) {
    const raw = (client as unknown as { _client?: { post?: (o: object) => Promise<{ error?: unknown; response?: Response }> } })._client
    return raw?.post?.({ url, path: { requestID: requestId }, body, headers: { 'content-type': 'application/json' }, signal: signal() })
  }

  /** Answers a permission prompt for this one call, or rejects it. */
  async function answer(sessionId: string, requestId: string, allow: boolean, message?: string) {
    const body = allow ? { reply: 'once' } : message ? { reply: 'reject', message } : { reply: 'reject' }
    const sent = await request('/permission/{requestID}/reply', requestId, body)
    if (sent && !sent.error) return
    if (sent?.response && sent.response.status !== 404) throw sent.error
    // An OpenCode without that route has this one, which cannot carry the message.
    const fallback = await client.postSessionIdPermissionsPermissionId({
      path: { id: sessionId, permissionID: requestId },
      body: { response: allow ? 'once' : 'reject' },
      signal: signal(),
    })
    if (fallback.error) throw fallback.error
  }

  /** Records that OpenCode is no longer waiting on a request, and tells Slorp if it knew of it. */
  function settle(requestId: string) {
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
        await post('/api/bridge/permission/done', { id })
        settlements.delete(id)
      }
    } catch {
      // Keep settlements until Slorp acknowledges them.
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
  async function offer(requestId: string, origin: string, tool: string, full: string, canAnswer: boolean) {
    if (asks.has(requestId)) return
    const ask: Ask = { sessionId: origin, origin, tool, full, canAnswer, settled: new AbortController(), deadline: Date.now() + ANSWER_MAX_MS }
    asks.set(requestId, ask)
    await offerAgain(requestId, ask)
  }

  async function offerAgain(requestId: string, ask: Ask) {
    if (Date.now() >= ask.deadline) { settle(requestId); return }
    if (ask.running || ask.slorpId || ask.settled.signal.aborted) return
    ask.running = true
    try {
      await present(requestId, ask)
    } catch {
      if (ask.slorpId && !ask.settled.signal.aborted) finish(ask.slorpId)
      ask.slorpId = undefined
    } finally {
      ask.running = false
    }
  }

  async function present(requestId: string, ask: Ask) {
    if (!ask.resolved) {
      const [id, s] = await top(ask.origin)
      if (ask.settled.signal.aborted) return
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
    if (ask.settled.signal.aborted) {
      finish(asked.id)
      return
    }
    ask.slorpId = asked.id
    ask.epoch = asked.epoch
    if (!asked.answerable) return

    for (; !ask.settled.signal.aborted && ask.slorpId === asked.id && Date.now() < ask.deadline; ) {
      const decided = await post('/api/bridge/permission/wait', { id: asked.id }, AbortSignal.any([ask.settled.signal, AbortSignal.timeout(25_000)]))
      if (ask.settled.signal.aborted || ask.slorpId !== asked.id) return
      if (decided?.state === 'waiting') continue
      if (decided?.state === 'allow' || decided?.state === 'deny') {
        const said = decided.message ? `The user denied this in Slorp and said: ${decided.message}` : undefined
        if (questions) {
          if (decided.state === 'allow' && !Array.isArray(decided.selections)) throw new Error('Missing question selections')
          const sent = await request(`/question/{requestID}/${decided.state === 'allow' ? 'reply' : 'reject'}`, requestId,
            decided.state === 'allow' ? { answers: decided.selections } : undefined)
          if (!sent || sent.error) throw new Error('OpenCode did not accept the answer')
        } else {
          await answer(origin, requestId, decided.state === 'allow', said)
        }
        asks.delete(requestId)
      } else {
        ask.slorpId = undefined
      }
      return
    }
    if (ask.slorpId === asked.id) settle(requestId)
  }

  const timer = setInterval(() => void poll(), POLL_MS)
  timer.unref?.()
  void client.vcs
    .get()
    .then(res => (branch = res.data?.branch ?? ''))
    .catch(() => {})

  return {
    dispose: async () => {
      clearInterval(timer)
      presence.close()
      for (const id of asks.keys()) settle(id)
      stopped.abort()
    },

    'chat.message': async (input, output) => {
      const [id, s] = await top(input.sessionID)
      if (id !== input.sessionID) return
      s.isLive = true
      s.isBusy = true
      s.status = 'working'
      s.waitingFor = ''
      s.agent = input.agent ?? output.message?.agent ?? s.agent
      model(s, input.model ?? output.message?.model)
      s.variant = input.variant
      s.prompt = textOf(output.parts as Part[])
      s.promptAt = output.message?.time.created ?? Date.now()
      s.activeAt = s.promptAt
      s.reply = ''
      s.needsAnswer = false
      report(id, 'turn.start', s.prompt, s.promptAt)
    },

    'chat.params': async (input) => {
      const [id, s] = await top(input.sessionID)
      if (id !== input.sessionID) return
      s.contextWindow = input.model.limit.context
    },

    event: async ({ event }) => {
      const e = event as { type: string; properties?: Record<string, any> }
      const p = e.properties ?? {}
      if (['permission.replied', 'question.replied', 'question.rejected'].includes(e.type)) { settle(p.requestID); return }
      const pendingAtIdle = e.type === 'session.idle' ? [...asks.entries()] : []
      const key = p.sessionID ?? p.info?.sessionID ?? p.info?.id ?? p.part?.sessionID ?? ''
      const apply = async () => {
        switch (e.type) {
          case 'session.created':
          case 'session.updated':
            if (p.info?.id) learn(p.info)
            break
          case 'session.deleted':
            for (const [id, ask] of asks) if (ask.origin === (p.info?.id ?? p.sessionID)) settle(id)
            sessions.delete(p.info?.id ?? p.sessionID)
            break
          case 'message.updated': {
            if (!p.info?.sessionID) break
            const [id, s] = await top(p.info.sessionID)
            if (id !== p.info.sessionID) break
            measured(s, p.info)
            s.activeAt = Math.max(s.activeAt ?? 0, p.info.time?.completed ?? p.info.time?.created ?? 0)
            break
          }
          case 'message.removed':
            sessions.get(p.sessionID)?.usage?.delete(p.messageID)
            break
          case 'message.part.updated': {
            const part = p.part
            const s = sessions.get(part?.sessionID)
            if (s && part.type === 'compaction') s.compacting = { auto: part.auto, before: s.contextTokens ?? 0 }
            break
          }
          case 'session.compacted': {
            const s = sessions.get(p.sessionID)
            if (!s?.isLive) break
            s.compacted = { at: new Date().toISOString(), auto: s.compacting?.auto, before: s.compacting?.before }
            s.compacting = undefined
            s.contextTokens = 0
            tell(p.sessionID, s)
            break
          }
          case 'session.status': {
            if (!p.sessionID || p.status?.type === 'idle') break
            const [id, s] = await top(p.sessionID)
            if (id !== p.sessionID) break
            s.isLive = true
            s.isBusy = true
            s.status = 'working'
            s.waitingFor = p.status?.type === 'retry' ? `Retrying: ${p.status.message ?? ''}` : ''
            s.activeAt = Date.now()
            tell(id, s)
            break
          }
          case 'session.error': {
            const s = sessions.get(p.sessionID)
            if (!s?.isLive) break
            s.status = p.error?.name === 'MessageAbortedError' ? 'idle' : 'failed'
            s.waitingFor = s.status === 'failed' ? String(p.error?.data?.message ?? 'OpenCode failed') : ''
            s.isBusy = false
            s.activeAt = Date.now()
            tell(p.sessionID, s)
            break
          }
          case 'vcs.branch.updated':
            branch = p.branch ?? ''
            break
          case 'session.idle': {
            const s = sessions.get(p.sessionID)
            if (!s?.isLive) break
            s.isBusy = false
            if (s.status !== 'failed') s.status = 'idle'
            if (s.status !== 'failed') s.waitingFor = ''
            s.activeAt = Date.now()
            s.needsAnswer = true
            for (const [requestId, ask] of pendingAtIdle) {
              if (asks.get(requestId) === ask && ask.sessionId === p.sessionID) settle(requestId)
            }
            inOrder(async () => {
              await refreshAnswer(p.sessionID, s)
              await post('/api/bridge/event', { agent: AGENT, sessionId: p.sessionID, type: 'snapshot', snapshot: snapshot(s) })
            })
            break
          }
          case 'permission.asked':
            void offer(p.id, p.sessionID, p.permission, describe(p), true).catch(() => {})
            break
          case 'question.asked': {
            const questions = (p.questions ?? []).map((q: { question?: string; header?: string; options?: unknown[]; multiple?: boolean; custom?: boolean }) => ({
              question: q.question ?? '', header: q.header, options: q.options ?? [], multiSelect: q.multiple ?? false, custom: q.custom !== false,
            }))
            void offer(p.id, p.sessionID, 'question', JSON.stringify({ questions }), true).catch(() => {})
            break
          }
        }
      }
      if (e.type === 'permission.asked' || e.type === 'question.asked') { await apply(); return }
      await enqueue(key, apply)
    },
  }
}
