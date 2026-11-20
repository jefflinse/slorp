import type { Plugin } from '@opencode/plugin'
import type { SessionInfo, SessionMessageInfo, SessionMessagesResponse, SessionActiveOutput, FormInfo, FormAnswer, OpenCodeClient } from '@opencode/client'
import { randomUUID } from 'node:crypto'
import { discover, headers } from './service.js'
type Endpoint = NonNullable<Awaited<ReturnType<typeof discover>>>

export type NativeRequest = { id: string; terminalId: string; method: string; path: string; body?: object }
export const maxRelayInFlight = 32

function localURL(value: string) {
  const url = new URL(value)
  if (!['http:', 'https:'].includes(url.protocol) || !['127.0.0.1', '[::1]', 'localhost'].includes(url.hostname) ||
    url.username || url.password || url.search || url.hash || url.pathname !== '/') throw new Error('Slorp: native server must be a loopback origin')
  return url.origin
}

/** Dispatches only the public V2 operations missing from the server context. */
export async function relayRequest(client: OpenCodeClient, request: NativeRequest, signal: AbortSignal) {
  const { method, body } = request
  const url = new URL(request.path, 'http://opencode')
  if (method === 'GET' && url.pathname === '/api/session/active') return { data: await client.session.active({ signal }) }
  const match = /^\/api\/session\/([^/]+)\/(message|form)(?:\/([^/]+))?(\/reply)?$/.exec(url.pathname)
  if (!match) throw new Error('Unsupported relay operation')
  const sessionID = decodeURIComponent(match[1]!), resource = match[2], id = match[3] && decodeURIComponent(match[3])
  if (resource === 'message' && method === 'GET') {
    if (id) return { data: await client.session.message.get({ sessionID, messageID: id }, { signal }) }
    const cursor = url.searchParams.get('cursor')
    return client.message.list({ sessionID, limit: Number(url.searchParams.get('limit') ?? 100),
      ...(cursor ? { cursor } : { order: url.searchParams.get('order') === 'desc' ? 'desc' as const : 'asc' as const }) }, { signal })
  }
  if (resource === 'form') {
    if (method === 'GET' && !id) return { data: await client.session.form.list({ sessionID }, { signal }) }
    if (id && method === 'POST' && match[4]) {
      await client.session.form.reply({ sessionID, formID: id, answer: (body as { answer: FormAnswer }).answer }, { signal })
      return null
    }
    if (id && method === 'DELETE') {
      await client.session.form.cancel({ sessionID, formID: id, message: url.searchParams.get('message') ?? undefined }, { signal })
      return null
    }
  }
  throw new Error('Unsupported relay operation')
}

function listenerURL() {
  const args = process.argv
  if (!args.includes('serve')) return
  const argument = (name: string) => {
    const at = args.indexOf(name)
    return at >= 0 ? args[at + 1] : args.find(arg => arg.startsWith(name + '='))?.slice(name.length + 1)
  }
  const port = Number(argument('--port'))
  if (!Number.isInteger(port) || port <= 0 || port > 65535) return
  const host = argument('--hostname')
  return `http://${host === '::' || host === '::1' ? '[::1]' : '127.0.0.1'}:${port}`
}

export type Info = Omit<SessionInfo, 'location' | 'cost' | 'tokens' | 'time' | 'projectID'> & {
  directory?: string
  projectID?: string
  time?: SessionInfo['time']
}
export type Usage = { input: number; output: number; reasoning?: number; total?: number; cache: { read: number; write: number } }
export type Message = {
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
  compaction?: { auto: boolean }
  error?: unknown
}
export type Part = { type: string; text?: string; synthetic?: boolean; ignored?: boolean; [key: string]: unknown }
export type TranscriptMessage = { info: Message; parts: Part[] }

export function sessionInfo(info: SessionInfo): Info {
  return { ...info, directory: info.location.directory }
}

/** Converts persisted V2 messages to the bridge's transcript records. */
export function transcript(message: SessionMessageInfo): TranscriptMessage {
  const info: Message = { id: message.id, role: message.type, time: message.time }
  const part = (value: Part, index: number): Part => ({ ...value, id: `${message.id}:${index}`, messageID: message.id })
  if (message.type === 'assistant') {
    Object.assign(info, { agent: message.agent, model: { providerID: message.model.providerID, modelID: message.model.id, variant: message.model.variant },
      providerID: message.model.providerID, modelID: message.model.id, variant: message.model.variant,
      tokens: message.tokens, cost: message.cost, error: message.error })
    return { info, parts: message.content.map((value, i) => {
      if (value.type !== 'tool') return part({ type: value.type, text: value.text }, i)
      const state = value.state
      const metadata = 'metadata' in state ? state.metadata : undefined
      const input = typeof state.input === 'string' ? {} : state.input
      return part({ type: 'tool', tool: value.name === 'subagent' ? 'task' : value.name, callID: value.id,
        state: { status: state.status === 'streaming' ? 'pending' : state.status,
          input: value.name === 'subagent' ? { ...input, subagent_type: input.agent, task_id: input.sessionID, background: true } : input,
          metadata: value.name === 'subagent' ? { ...metadata, sessionId: metadata?.sessionID, background: true } : metadata,
          output: 'content' in state ? state.content?.filter(c => c.type === 'text').map(c => c.text).join('\n') : undefined,
          time: { start: value.time.ran ?? value.time.created, end: value.time.completed } } }, i)
    }) }
  }
  if (message.type === 'user' || message.type === 'synthetic') {
    const meta = message.metadata
    const subagent = message.type === 'synthetic' && meta?.source === 'subagent' && typeof meta.childID === 'string' &&
      typeof meta.state === 'string' && ['running', 'completed', 'error', 'cancelled'].includes(meta.state)
      ? { id: meta.childID, state: meta.state, text: message.text.replace(/^<subagent[^\n]*>\n?/, '').replace(/\n?<\/subagent>\s*$/, '') } : undefined
    return { info, parts: [part({ type: 'text', text: message.text, synthetic: message.type === 'synthetic', ...(subagent ? { subagent } : {}) }, 0)] }
  }
  if (message.type === 'compaction') {
    info.summary = message.status === 'completed'
    if (message.status === 'completed') info.compaction = { auto: message.reason === 'auto' }
    if ('tokens' in message) info.tokens = message.tokens
    if ('cost' in message) info.cost = message.cost
  }
  return { info, parts: [] }
}

/** V2 operations absent from the 2.3.2 server plugin context. */
export class Native {
  private disposed = false
  private url?: string
  private verified?: Promise<void>
  private endpointHeaders?: Record<string, string>
  private discovery?: Promise<void>
  private nextDiscovery = 0
  private readonly discoverEndpoint: () => Promise<Endpoint | undefined>
  private terminal?: { id: string; expires: number }
  private emit?: (type: 'request' | 'cancel', data: Record<string, unknown>) => Promise<void>
  private readonly pending = new Map<string, { terminalId: string; resolve: (value: unknown) => void; reject: (error: Error) => void; cleanup: () => void }>()
  constructor(context: Plugin.Context, url?: string, discovery?: () => Promise<Endpoint | undefined>) {
    // Store no runtime SDK objects: the embedded package uses type-only imports.
    this.url = url || process.env.SLORP_OPENCODE_URL || listenerURL()
    this.discoverEndpoint = discovery ?? (() => discover({ version: context.app.version }))
  }
  setURL(url: string) {
    url = localURL(url)
    if (url === this.url) return
    this.url = url
    this.verified = undefined
    this.endpointHeaders = undefined
  }

  relay(emit: NonNullable<Native['emit']>) { this.emit = emit }
  advertise(terminalId: string) { this.terminal = { id: terminalId, expires: Date.now() + 10000 } }
  result(value: unknown) {
    if (!value || typeof value !== 'object') return
    const result = value as { id: string; terminalId: string; value?: unknown; error?: string }
    const pending = this.pending.get(result.id)
    if (!pending || pending.terminalId !== result.terminalId) return
    this.pending.delete(result.id)
    pending.cleanup()
    if (result.error) pending.reject(new Error(result.error))
    else pending.resolve(result.value)
  }

  async request<T>(method: string, path: string, signal: AbortSignal, body?: object): Promise<T> {
    if (this.disposed) throw new Error('Disposed')
    signal.throwIfAborted()
    if (!this.url && !process.env.SLORP_MANAGED_FILE && !process.env.SLORP_MANAGED_RUN &&
      (this.discovery || Date.now() >= this.nextDiscovery)) {
      this.discovery ??= (async () => {
        this.nextDiscovery = Date.now() + 10000
        const endpoint = await this.discoverEndpoint()
        if (!endpoint || this.url) return
        const url = localURL(endpoint.url)
        const auth = headers(endpoint)
        const response = await fetch(url + '/api/info', { headers: auth, signal, redirect: 'error' })
        if (!response.ok || (await response.json() as { pid?: number }).pid !== process.pid || this.url) return
        this.url = url
        this.endpointHeaders = auth
        this.verified = Promise.resolve()
      })().catch(() => {}).finally(() => { this.discovery = undefined })
      await this.discovery
      signal.throwIfAborted()
    }
    if (this.disposed) throw new Error('Disposed')
    if (this.url) {
      const url = localURL(this.url)
      const password = process.env.OPENCODE_PASSWORD ?? process.env.OPENCODE_SERVER_PASSWORD
      const auth = this.endpointHeaders ?? (password ? { authorization: `Basic ${Buffer.from(`opencode:${password}`).toString('base64')}` } : {})
      this.verified ??= fetch(url + '/api/info', { signal, redirect: 'error',
        headers: auth })
        .then(async response => {
          if (!response.ok || (await response.json() as { pid?: number }).pid !== process.pid) throw new Error('Slorp: native URL is not this OpenCode server')
        }).catch(error => { this.verified = undefined; throw error })
      await this.verified
      signal.throwIfAborted()
      if (url !== localURL(this.url)) throw new Error('Slorp: native endpoint changed')
      const response = await fetch(url + path, { method, signal, redirect: 'error',
        headers: { ...auth,
          ...(body ? { 'content-type': 'application/json' } : {}) },
        ...(body ? { body: JSON.stringify(body) } : {}) })
      if (!response.ok) throw new Error(`OpenCode: HTTP ${response.status}`)
      const text = await response.text()
      signal.throwIfAborted()
      return (text ? JSON.parse(text) : undefined) as T
    }
    if (!this.emit || !this.terminal || this.terminal.expires <= Date.now()) throw new Error('Slorp: waiting for terminal transport')
    if (this.pending.size >= maxRelayInFlight) throw new Error('Slorp: relay busy; retry later')
    const emit = this.emit, terminalId = this.terminal.id, id = randomUUID()
    return await new Promise<unknown>((resolve, reject) => {
      const abort = () => {
        this.pending.delete(id)
        signal.removeEventListener('abort', abort)
        void emit('cancel', { id, terminalId }).catch(() => {})
        reject(new Error('Aborted'))
      }
      signal.addEventListener('abort', abort, { once: true })
      this.pending.set(id, { terminalId, resolve, reject, cleanup: () => signal.removeEventListener('abort', abort) })
      void emit('request', { id, terminalId, method, path, ...(body ? { body } : {}) }).catch(error => {
        this.pending.delete(id)
        signal.removeEventListener('abort', abort)
        reject(error)
      })
    }) as T
  }

  dispose() {
    this.disposed = true
    for (const [id, pending] of this.pending) {
      void this.emit?.('cancel', { id, terminalId: pending.terminalId }).catch(() => {})
      pending.cleanup()
      pending.reject(new Error('Disposed'))
    }
    this.pending.clear()
  }

  async messages(sessionID: string, signal: AbortSignal, limit?: number) {
    const messages: TranscriptMessage[] = []
    let cursor: string | null | undefined
    const seen = new Set<string>()
    do {
      const query = new URLSearchParams({ limit: String(limit ?? 100) })
      if (cursor) query.set('cursor', cursor)
      else query.set('order', limit ? 'desc' : 'asc')
      const page = await this.request<SessionMessagesResponse>('GET', `/api/session/${encodeURIComponent(sessionID)}/message?${query}`, signal)
      messages.push(...page.data.map(transcript))
      cursor = page.cursor.next
      if (cursor && seen.has(cursor)) throw new Error('OpenCode: repeated message cursor')
      if (cursor) seen.add(cursor)
    } while (cursor && !limit)
    return limit ? messages.reverse() : messages
  }

  async active(signal: AbortSignal) {
    const result = await this.request<{ data: SessionActiveOutput }>('GET', '/api/session/active', signal)
    return result.data
  }

  async message(sessionID: string, messageID: string, signal: AbortSignal) {
    const result = await this.request<{ data: SessionMessageInfo }>('GET', `/api/session/${encodeURIComponent(sessionID)}/message/${encodeURIComponent(messageID)}`, signal)
    return transcript(result.data)
  }

  async forms(sessionID: string, signal: AbortSignal) {
    return (await this.request<{ data: FormInfo[] }>('GET', `/api/session/${encodeURIComponent(sessionID)}/form`, signal)).data
  }

  async form(sessionID: string, formID: string, answer: FormAnswer | undefined, signal: AbortSignal, message?: string) {
    const suffix = answer ? '/reply' : message !== undefined ? `?${new URLSearchParams({ message })}` : ''
    await this.request(answer ? 'POST' : 'DELETE', `/api/session/${encodeURIComponent(sessionID)}/form/${encodeURIComponent(formID)}${suffix}`,
      signal, answer ? { answer } : undefined)
  }
}
