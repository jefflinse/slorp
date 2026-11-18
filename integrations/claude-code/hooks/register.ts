import type { EngineInterface, Register } from 'claude-code'

const AGENT = 'claude'
const POLL_MS = 2000
const SUMMARY_LIMIT = 400
/** Inputs longer than this are too much to judge in Slorp; the terminal asks. */
const INPUT_LIMIT = 8000
/** How long Slorp may answer a prompt before only the terminal can. */
const ANSWER_MAX_MS = 60 * 60_000
/** Tools whose prompt only the terminal's own dialog can answer. */
const NEEDS_PERSON = new Set(['ExitPlanMode'])

type Reply = { id: string; text: string }

let base = ''
let cwd = ''
/** What the session's model can take, in tokens; zero until the engine says. */
let contextWindow = 0
/** The engine's estimate of the session's cost so far, in US dollars. */
let costUsd = 0
/** The account's usage windows as last read, and when; zero for a reading of unknown age. */
let rateLimits: object[] = []
let measuredAt = 0
let isPolling = false

async function post($: EngineInterface, path: string, body: object) {
  const res = await $.http.fetch(base + path, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  })
  return res.ok && res.text ? JSON.parse(res.text) : undefined
}

function report($: EngineInterface, type: string, fields: object = {}) {
  void $.session
    .id()
    .then(sessionId => post($, '/api/bridge/event', { agent: AGENT, sessionId, type, ...fields }))
    .catch(() => {})
}

function measured(usage: { context: { window: number }; cost?: { usd: number }; rateLimits: object[] }, at: number) {
  contextWindow = usage.context.window
  costUsd = usage.cost?.usd ?? costUsd
  if (usage.rateLimits.length) [rateLimits, measuredAt] = [usage.rateLimits, at]
}

async function deliver($: EngineInterface, reply: Reply) {
  const sent = await $.prompt.submit({ text: reply.text, asUser: true })
  const state = sent.drop === undefined ? 'delivered' : 'dropped'
  await post($, '/api/bridge/ack', { id: reply.id, state })
}

async function poll($: EngineInterface) {
  if (isPolling) return
  isPolling = true
  try {
    const sessionId = await $.session.id()
    const claimed = await post($, '/api/bridge/poll', { agent: AGENT, sessionId, cwd, contextWindow, costUsd, rateLimits, measuredAt })
    for (const reply of (claimed?.replies ?? []) as Reply[]) {
      void deliver($, reply).catch(() => {})
    }
  } catch {
    // Slorp is not running; the next tick tries again.
  } finally {
    isPolling = false
  }
}

/**
 * Offers a permission prompt to Slorp while the session's own dialog is also
 * asking, and resolves to the user's `allow` or `deny` from Slorp. Resolves to
 * `terminal` when the answer will not come from Slorp: the dialog was
 * answered or the turn interrupted, the tool needs the person, the input is
 * too long to judge there, or Slorp is not running.
 */
async function askSlorp($: EngineInterface, tool: string, input: unknown, abandoned: AbortSignal) {
  const { command, questions } = (input ?? {}) as { command?: unknown; questions?: unknown }
  const full = typeof command === 'string' ? command : JSON.stringify(input, null, 2)
  const asks = tool === 'AskUserQuestion' && Array.isArray(questions) ? questions.map(q => String(q?.question ?? '')).join(' ') : full
  const asked = await post($, '/api/bridge/permission', {
    agent: AGENT,
    sessionId: await $.session.id(),
    tool,
    summary: asks.slice(0, SUMMARY_LIMIT),
    input: full.slice(0, INPUT_LIMIT),
    canAnswer: !NEEDS_PERSON.has(tool) && full.length <= INPUT_LIMIT,
    question: tool === 'AskUserQuestion',
  })
  if (!asked?.answerable) return { state: 'terminal' }

  // Answering in the terminal abandons this hook; tell Slorp at once so it
  // stops offering a request that is already decided.
  const settled = () => void post($, '/api/bridge/permission/done', { id: asked.id }).catch(() => {})
  abandoned.addEventListener('abort', settled)
  for (const deadline = Date.now() + ANSWER_MAX_MS; !abandoned.aborted && Date.now() < deadline; ) {
    const answer = await post($, '/api/bridge/permission/wait', { id: asked.id })
    if (answer?.state !== 'waiting') return answer ?? { state: 'terminal' }
  }
  settled()
  return { state: 'terminal' }
}

/**
 * Bridges one Claude Code session to the Slorp service at the `url` option,
 * or at SLORP_URL when that is set. Every POLL_MS it
 * claims replies queued for this session and submits each as the user's own
 * prompt; alongside, it reports turn events. A permission prompt is offered
 * to Slorp as well as the terminal's own dialog, and whichever the user
 * answers first decides it; a question is answered there with the options
 * the user picked. Each poll also carries the size of the model's context
 * window, the engine's estimate of the session's cost, and the account's
 * usage windows as this session last read them.
 */
export const register: Register = (on, options) => {
  on('session.start', async ($, e, next) => {
    base = ((await $.env.get('SLORP_URL')) || String(options.url)).replace(/\/+$/, '')
    cwd = e.cwd
    void $.session
      .usage()
      .then(usage => measured(usage, 0))
      .catch(() => {})
    $.clock.every(POLL_MS, () => void poll($))
    return next(e)
  })

  on('session.measure', ($, e, next) => {
    measured(e, Date.now())
    return next(e)
  })

  on('turn.start', ($, e, next) => {
    if (e.agentId === undefined) report($, 'turn.start', { text: e.text })
    return next(e)
  })

  on('turn.complete', ($, e, next) => {
    if (e.agentId === undefined) report($, 'turn.complete', { text: e.answer })
    return next(e)
  })

  on('classic.PermissionRequest', async ($, e, next) => {
    const answer = await askSlorp($, e.tool_name, e.tool_input, next.signal).catch(() => ({ state: 'terminal' }))
    if (answer.state === 'allow' && e.tool_name === 'AskUserQuestion') {
      if (!answer.answers) return next(e)
      return { decision: { behavior: 'allow' as const, updatedInput: { ...(e.tool_input as object), answers: answer.answers } } }
    }
    if (answer.state === 'allow') return { decision: { behavior: 'allow' as const } }
    if (answer.state === 'deny') {
      const why = answer.message ? `The user denied this in Slorp and said: ${answer.message}` : 'The user denied this in Slorp.'
      return { decision: { behavior: 'deny' as const, message: why } }
    }
    return next(e)
  }).catch(($, e, next) => next(e))

  on('classic.PostToolUse', ($, e, next) => {
    report($, 'tool.done')
    return next(e)
  })

  on('classic.PostToolUseFailure', ($, e, next) => {
    report($, 'tool.done')
    return next(e)
  })

  on('classic.PermissionDenied', ($, e, next) => {
    report($, 'tool.done')
    return next(e)
  })
}
