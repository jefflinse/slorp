import type { EngineInterface, Register } from 'claude-code'

const DEFAULT_URL = 'http://127.0.0.1:4747'
const POLL_MS = 2000
const SUMMARY_LIMIT = 400

type Reply = { id: string; text: string }

let base = DEFAULT_URL
let cwd = ''
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
    .then(sessionId => post($, '/api/bridge/event', { sessionId, type, ...fields }))
    .catch(() => {})
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
    const claimed = await post($, '/api/bridge/poll', { sessionId, cwd })
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
 * Bridges one Claude Code session to the Slorp service. Every POLL_MS it
 * claims replies queued for this session and submits each as the user's own
 * prompt; alongside, it reports turn and permission events. Nothing here
 * decides anything: every hook passes its event on unchanged.
 */
export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    base = (await $.env.get('SLORP_URL')) ?? DEFAULT_URL
    cwd = e.cwd
    $.clock.every(POLL_MS, () => void poll($))
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

  on('classic.PermissionRequest', ($, e, next) => {
    const input = e.tool_input as { command?: unknown }
    const summary = typeof input?.command === 'string' ? input.command : JSON.stringify(e.tool_input)
    report($, 'permission', { tool: e.tool_name, summary: summary.slice(0, SUMMARY_LIMIT) })
    return next(e)
  })

  on('classic.PostToolUse', ($, e, next) => {
    report($, 'tool.done')
    return next(e)
  })

  on('classic.PostToolUseFailure', ($, e, next) => {
    report($, 'tool.done')
    return next(e)
  })
}
