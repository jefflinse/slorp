import type { Plugin } from '@opencode-ai/plugin'

const AGENT = 'opencode'
const DEFAULT_URL = 'http://127.0.0.1:4747'
const POLL_MS = 2000
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
  model?: { id?: string; providerID?: string }
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
}

/** A permission prompt or question OpenCode is waiting on. */
type Ask = { sessionId: string; slorpId?: string; settled: AbortController }

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
 * reported once something happens in it: its turns as they start and end,
 * and its title and directory with every poll. Every POLL_MS the plugin
 * claims the replies queued for each such session and submits them as the
 * user's own prompts. A permission prompt, a subagent's included, is offered
 * to Slorp as well as OpenCode's own dialog, and whichever the user answers
 * first decides it; Slorp only ever allows the one call. A question is
 * reported but can only be answered in OpenCode.
 */
export const Slorp: Plugin = async ({ client, directory }, options) => {
  const base = (process.env.SLORP_URL || String(options?.url ?? DEFAULT_URL)).replace(/\/+$/, '')
  const sessions = new Map<string, Known>()
  const asks = new Map<string, Ask>()
  let branch = ''
  let isPolling = false
  let sending: Promise<unknown> = Promise.resolve()

  async function post(path: string, body: object, signal?: AbortSignal) {
    const res = await fetch(base + path, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
      signal,
    })
    const text = res.ok ? await res.text() : ''
    return text ? JSON.parse(text) : undefined
  }

  /** Runs the steps given to it one at a time, so Slorp hears of turns in the order they happened. */
  function inOrder(step: () => Promise<unknown>) {
    sending = sending.then(step).catch(() => {})
  }

  function report(sessionId: string, type: string, text = '') {
    inOrder(() => post('/api/bridge/event', { agent: AGENT, sessionId, type, text }))
  }

  function learn(info: Info) {
    const s = sessions.get(info.id) ?? { title: '', cwd: directory, prompt: '', reply: '', isBusy: false, isLive: false }
    s.parent = info.parentID
    if (info.title !== undefined) s.title = UNTITLED.test(info.title) ? '' : info.title
    if (info.directory) s.cwd = info.directory
    if (info.agent) s.agent = info.agent
    if (info.model?.providerID && info.model.id) s.model = { providerID: info.model.providerID, modelID: info.model.id }
    sessions.set(info.id, s)
    return s
  }

  /** Resolves a session to the top-level session it runs under, which is itself unless it is a subagent's. */
  async function top(id: string): Promise<[string, Known]> {
    for (let hops = 0; ; hops++) {
      const s = sessions.get(id) ?? learn(((await client.session.get({ path: { id } })).data as Info | undefined) ?? { id })
      if (!s.parent || hops === 20) return [id, s]
      id = s.parent
    }
  }

  /** The text the session's last turn ended on, or nothing if the turn wrote none. */
  async function lastAnswer(id: string) {
    const res = await client.session.messages({ path: { id }, query: { limit: 10 } })
    for (const m of [...(res.data ?? [])].reverse()) {
      if (m.info.role !== 'assistant') break
      const text = textOf(m.parts)
      if (text) return text
    }
    return ''
  }

  /** Tells a Slorp that had not heard of a session where the session stands. */
  function retell(id: string, s: Known) {
    if (s.prompt) report(id, 'turn.start', s.prompt)
    if (!s.isBusy) report(id, 'turn.complete', s.reply)
  }

  async function deliver(id: string, s: Known, reply: Reply) {
    const sent = await client.session.promptAsync({
      path: { id },
      body: { agent: s.agent, model: s.model, parts: [{ type: 'text', text: reply.text }] },
    })
    await post('/api/bridge/ack', { id: reply.id, state: sent.error ? 'dropped' : 'delivered' })
  }

  async function poll() {
    if (isPolling) return
    isPolling = true
    try {
      for (const [id, s] of sessions) {
        if (!s.isLive) continue
        const claimed = await post('/api/bridge/poll', { agent: AGENT, sessionId: id, cwd: s.cwd, title: s.title, branch })
        if (claimed?.fresh) retell(id, s)
        for (const reply of (claimed?.replies ?? []) as Reply[]) {
          void deliver(id, s, reply).catch(() => {})
        }
      }
    } catch {
      // Slorp is not running; the next tick tries again.
    } finally {
      isPolling = false
    }
  }

  /** Answers a permission prompt for this one call, or rejects it. */
  async function answer(sessionId: string, requestId: string, allow: boolean, message?: string) {
    const raw = (client as unknown as { _client?: { post?: (o: object) => Promise<{ error?: unknown }> } })._client
    const body = allow ? { reply: 'once' } : message ? { reply: 'reject', message } : { reply: 'reject' }
    const sent = await raw?.post?.({ url: '/permission/{requestID}/reply', path: { requestID: requestId }, body, headers: { 'content-type': 'application/json' } })
    if (sent && !sent.error) return
    // An OpenCode without that route has this one, which cannot carry the message.
    await client.postSessionIdPermissionsPermissionId({
      path: { id: sessionId, permissionID: requestId },
      body: { response: allow ? 'once' : 'reject' },
    })
  }

  /** Records that OpenCode is no longer waiting on a request, and tells Slorp if it knew of it. */
  function settle(requestId: string) {
    const ask = asks.get(requestId)
    if (!ask) return
    asks.delete(requestId)
    ask.settled.abort()
    if (ask.slorpId) void post('/api/bridge/permission/done', { id: ask.slorpId }).catch(() => {})
  }

  /**
   * Shows Slorp a request the session `origin` is waiting on, under the
   * top-level session it belongs to. When Slorp may answer it, waits for the
   * user's allow or deny there and passes it on, unless the request is
   * settled in OpenCode first.
   */
  async function offer(requestId: string, origin: string, tool: string, full: string, canAnswer: boolean) {
    const [sessionId, s] = await top(origin)
    s.isLive = true
    const ask: Ask = { sessionId, settled: new AbortController() }
    asks.set(requestId, ask)
    const asked = await post('/api/bridge/permission', {
      agent: AGENT,
      sessionId,
      tool,
      summary: full.slice(0, SUMMARY_LIMIT),
      input: full.slice(0, INPUT_LIMIT),
      canAnswer: canAnswer && full.length <= INPUT_LIMIT,
    })
    if (!asked) return
    if (ask.settled.signal.aborted) {
      await post('/api/bridge/permission/done', { id: asked.id })
      return
    }
    ask.slorpId = asked.id
    if (!asked.answerable) return

    for (const deadline = Date.now() + ANSWER_MAX_MS; !ask.settled.signal.aborted && Date.now() < deadline; ) {
      const decided = await post('/api/bridge/permission/wait', { id: asked.id }, ask.settled.signal)
      if (decided?.state === 'waiting') continue
      if (decided?.state === 'allow' || decided?.state === 'deny') {
        asks.delete(requestId)
        const said = decided.message ? `The user denied this in Slorp and said: ${decided.message}` : undefined
        await answer(origin, requestId, decided.state === 'allow', said)
      }
      return
    }
    settle(requestId)
  }

  const timer = setInterval(() => void poll(), POLL_MS)
  timer.unref?.()
  void client.vcs
    .get()
    .then(res => (branch = res.data?.branch ?? ''))
    .catch(() => {})

  return {
    dispose: async () => clearInterval(timer),

    'chat.message': async (input, output) => {
      const [id, s] = await top(input.sessionID)
      if (id !== input.sessionID) return
      s.isLive = true
      s.isBusy = true
      s.prompt = textOf(output.parts as Part[])
      report(id, 'turn.start', s.prompt)
    },

    event: async ({ event }) => {
      const e = event as { type: string; properties?: Record<string, any> }
      const p = e.properties ?? {}
      switch (e.type) {
        case 'session.created':
        case 'session.updated':
          if (p.info?.id) learn(p.info)
          break
        case 'session.deleted':
          sessions.delete(p.info?.id ?? p.sessionID)
          break
        case 'vcs.branch.updated':
          branch = p.branch ?? ''
          break
        case 'session.idle': {
          const s = sessions.get(p.sessionID)
          if (!s?.isLive) break
          s.isBusy = false
          for (const [requestId, ask] of asks) {
            if (ask.sessionId === p.sessionID) settle(requestId)
          }
          inOrder(async () => {
            s.reply = (await lastAnswer(p.sessionID)) || s.reply
            await post('/api/bridge/event', { agent: AGENT, sessionId: p.sessionID, type: 'turn.complete', text: s.reply })
          })
          break
        }
        case 'permission.asked':
          void offer(p.id, p.sessionID, p.permission, describe(p), true).catch(() => {})
          break
        case 'question.asked': {
          const asked = (p.questions ?? []).map((q: { question?: string }) => q.question ?? '').join('\n')
          void offer(p.id, p.sessionID, 'question', asked, false).catch(() => {})
          break
        }
        case 'permission.replied':
        case 'question.replied':
        case 'question.rejected':
          settle(p.requestID)
          break
      }
    },
  }
}
