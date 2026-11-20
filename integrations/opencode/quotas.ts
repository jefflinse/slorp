import { createHash, randomUUID } from 'node:crypto'
import type { Credential, Plugin } from '@opencode/plugin'
import type { ConnectionInfo, ModelInfo, ModelRef, ProviderInfo } from '@opencode/client'

export type ProviderLimit = {
  provider: 'openai-codex' | 'github-copilot' | 'opencode-go'
  status: 'ok' | 'unsupported' | 'auth_required' | 'unavailable' | 'throttled'
  checkedAt: number
  measuredAt?: number
  retryAt?: number
  windows: QuotaWindow[]
}

type QuotaWindow = { kind: string; label: string; percentUsed: number; resetsAt: string; windowSeconds?: number }
type QuotaFetch = (url: string, init: RequestInit) => Promise<Response>
export type Source = {
  provider: ProviderLimit['provider']
  identity: unknown[]
  fetch?: QuotaFetch
  key?: string
  status: ProviderLimit['status']
}
type Observation = { sequence: number; source?: Source }
type Entry = { observation: Observation; source: Source; report: ProviderLimit }
type Slot = { entry?: Entry; pending?: { origin: string; sequence: number; report: ProviderLimit }; controller?: AbortController; timer?: ReturnType<typeof setTimeout>; running: boolean; next: number; failures: number }
type Root = { id: string; origins: Map<string, Observation>; slots: Map<ProviderLimit['provider'], Slot>; revision: number }

const PERIOD = 60_000
const DEADLINE = 5000
const MAX_BODY = 64 * 1024
const MAX_BACKOFF = 15 * 60_000
// Retry-After is capped at one day for both scheduling and the bridge wire contract.
const MAX_RETRY = 24 * 60 * 60_000
const URLS = {
  'openai-codex': 'https://chatgpt.com/backend-api/wham/usage',
  'github-copilot': 'https://api.github.com/copilot_internal/user',
  'opencode-go': 'https://opencode.ai/zen/go/v1/usage',
} as const
const PACKAGES = {
  'openai-codex': ['@opencode/ai/providers/openai', '@opencode/ai/providers/openai/responses'],
  'github-copilot': [
    '@opencode/ai/providers/openai', '@opencode/ai/providers/openai/chat', '@opencode/ai/providers/openai/responses',
    '@opencode/ai/providers/openai-compatible', '@opencode/ai/providers/openai-compatible/responses',
    '@opencode/ai/providers/openai-compatible-responses', '@opencode/ai/providers/anthropic',
  ],
  'opencode-go': [
    '@opencode/ai/providers/openai', '@opencode/ai/providers/openai/chat', '@opencode/ai/providers/openai/responses',
    '@opencode/ai/providers/openai-compatible', '@opencode/ai/providers/openai-compatible/responses',
    '@opencode/ai/providers/openai-compatible-responses', '@opencode/ai/providers/anthropic',
  ],
}
const COPILOT_BASES = [
  'https://api.githubcopilot.com', 'https://api.individual.githubcopilot.com',
  'https://api.business.githubcopilot.com', 'https://api.enterprise.githubcopilot.com',
]

function record(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : undefined
}

function headerContext(value: unknown) {
  try {
    const headers = new Headers(value as RequestInit['headers'])
    const auth = ['authorization', 'proxy-authorization', 'x-api-key', 'api-key', 'cookie', 'copilot-session-token'].some(key => headers.has(key))
    return { auth, account: headers.get('chatgpt-account-id'), identity: fingerprint([...headers].sort(([a], [b]) => a.localeCompare(b))) }
  } catch {
    return { auth: true, account: null, identity: '' }
  }
}

function fingerprint(value: unknown) {
  return createHash('sha256').update(JSON.stringify(value)).digest('hex')
}

function connectionIdentity(connection: ConnectionInfo | undefined) {
  return connection ? JSON.stringify(connection.type === 'credential' ?
    [connection.type, connection.id, connection.method] : [connection.type, connection.name]) : undefined
}

function credentialIdentity(provider: Source['provider'], credential: Credential.Value) {
  return fingerprint([credential.type, credential.metadata?.accountID, credential.metadata?.enterpriseUrl,
    credential.metadata?.apiEndpoint, credential.metadata?.orgID,
    credential.type === 'key' ? credential.key : credential.methodID,
    provider === 'github-copilot' && credential.type === 'oauth' ? credential.refresh : undefined])
}

export async function quotaSource(ctx: Plugin.Context, p: ProviderInfo, m: ModelInfo, directFetch: QuotaFetch = fetch, selection?: ModelRef): Promise<Source | undefined> {
  const id = p.id
  const name = id === 'openai' ? 'openai-codex' : id === 'github-copilot' || id === 'github-copilot-enterprise' ? 'github-copilot' : id === 'opencode-go' ? id : undefined
  if (!name) return
  const variantID = selection?.variant === 'default' ? undefined : selection?.variant
  const selectedVariant = variantID === undefined ? undefined : m.variants?.find(v => v.id === variantID)
  const variants = selection ? selectedVariant ? [selectedVariant] : [] : m.variants ?? []
  const settings = [p.settings, m.settings, ...variants.map(v => v.settings)].map(value => value ?? {})
  const headers = [p.headers, m.headers, ...variants.map(v => v.headers)].map(headerContext)
  const packages = PACKAGES[name]
  const valid = m.providerID === id &&
    (!selection || selection.providerID === id && selection.id === m.id && (variantID === undefined || selectedVariant !== undefined)) &&
    settings.every(options => options.apiKey === undefined) &&
    !headers.some(h => h.auth) && packages.includes(p.package) && (!m.package || packages.includes(m.package))
  const s: Source = {
    provider: name, status: 'unsupported',
    identity: [id, p.integrationID, p.package, m.package, fingerprint(settings), ...headers.map(h => h.identity)],
  }
  if (!valid || id === 'github-copilot-enterprise') return s
  const integration = p.integrationID ?? id
  const active = await ctx.integration.connection.active(integration)
  if (!active) { s.status = 'auth_required'; return s }
  const connection = { ...active }, identity = connectionIdentity(connection)
  s.identity.push(identity)
  const credential = await ctx.integration.connection.resolve(connection)
  if (connectionIdentity(await ctx.integration.connection.active(integration)) !== identity) {
    s.status = 'unavailable'
    return s
  }
  if (!credential) { s.status = 'auth_required'; return s }
  if (name === 'openai-codex' && credential.type !== 'oauth' ||
    name === 'github-copilot' && credential.type !== 'oauth' ||
    name === 'opencode-go' && credential.type !== 'key') return s
  if (name === 'github-copilot' && credential.type === 'oauth' && credential.metadata?.enterpriseUrl) return s
  const endpoint = credential.metadata?.apiEndpoint
  const base = typeof endpoint === 'string' ? endpoint.replace(/\/$/, '') : COPILOT_BASES[0]!
  if (name === 'github-copilot' && (endpoint !== undefined && typeof endpoint !== 'string' || !COPILOT_BASES.includes(base))) return s
  const bases = name === 'openai-codex' ? ['https://chatgpt.com/backend-api/codex', 'https://api.openai.com/v1'] :
    name === 'github-copilot' ? [COPILOT_BASES[0]!, base] : ['https://opencode.ai/zen/go/v1']
  const official = (url: unknown, index: number) => url === undefined || bases.some(base => url === base || url === base + '/' ||
    name === 'github-copilot' && index > 0 && (m.package ?? p.package) === '@opencode/ai/providers/anthropic' &&
      (url === base + '/v1' || url === base + '/v1/'))
  if (!settings.every((options, index) => official(options.baseURL, index))) return s
  const account = credential.type === 'oauth' && typeof credential.metadata?.accountID === 'string' ? credential.metadata.accountID : undefined
  if (headers.some(h => h.account !== null && (name !== 'openai-codex' || h.account !== account))) return s
  const credentialID = credentialIdentity(name, credential)
  s.identity.push(credentialID)
  s.fetch = async (url, init) => {
    if (url !== URLS[name] || init.method !== 'GET') throw new Error('Unsupported quota request')
    if (init.signal?.aborted) throw new Error('Aborted')
    if (connectionIdentity(await ctx.integration.connection.active(integration)) !== identity) return new Response(null, { status: 401 })
    const current = await ctx.integration.connection.resolve(connection)
    if (init.signal?.aborted) throw new Error('Aborted')
    if (connectionIdentity(await ctx.integration.connection.active(integration)) !== identity || init.signal?.aborted)
      throw new Error('Credential changed')
    if (!current) return new Response(null, { status: 401 })
    if (credentialIdentity(name, current) !== credentialID) throw new Error('Credential changed')
    const token = current?.type === 'oauth' ? name === 'github-copilot' ? current.refresh : current.access : current?.type === 'key' ? current.key : undefined
    if (!token) return new Response(null, { status: 401 })
    const response = await directFetch(url, { ...init, redirect: 'error', headers: { authorization: `Bearer ${token}`,
      ...(name === 'openai-codex' && account ? { 'ChatGPT-Account-Id': account } : {}),
      ...(name === 'github-copilot' ? { accept: 'application/json', 'user-agent': `opencode/${ctx.app.version}`, 'X-GitHub-Api-Version': '2025-04-01' } : {}) } })
    if (connectionIdentity(await ctx.integration.connection.active(integration)) !== identity || init.signal?.aborted) {
      void response.body?.cancel().catch(() => {})
      throw new Error('Credential changed')
    }
    return response
  }
  s.status = 'unavailable'
  return s
}

function percent(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 && value <= 100
}

function positive(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0
}

function denominator(value: unknown): number | undefined {
  const n = typeof value === 'string' && /^\d+(?:\.\d+)?$/.test(value) ? Number(value) : value
  return typeof n === 'number' && Number.isFinite(n) && n >= 0 ? n : undefined
}

function iso(ms: number): string | undefined {
  return Number.isFinite(ms) && ms > 0 && ms <= 253402300799999 ? new Date(ms).toISOString() : undefined
}

function date(value: unknown): string | undefined {
  if (typeof value !== 'string' || !/^\d{4}-\d\d-\d\d(?:T\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d))?$/.test(value)) return
  const ms = Date.parse(value)
  // Date.parse normalizes impossible calendar dates, such as February 30.
  if (!Number.isFinite(ms) || new Date(value.slice(0, 10)).toISOString().slice(0, 10) !== value.slice(0, 10)) return
  return iso(ms)
}

function duration(seconds: number) {
  for (const [unit, size] of [['day', 86400], ['hour', 3600], ['minute', 60]] as const) {
    if (seconds % size === 0) return `${seconds / size}-${unit}`
  }
  return `${seconds}-second`
}

function normalize(provider: Source['provider'], body: unknown, now: number): Pick<ProviderLimit, 'status' | 'windows'> {
  const data = record(body)
  const windows: QuotaWindow[] = []
  let invalid = false
  if (provider === 'openai-codex') {
    const limits = record(data?.rate_limit)
    if (!limits) return { status: 'unavailable', windows }
    for (const kind of ['primary_window', 'secondary_window']) {
      if (limits[kind] === undefined || limits[kind] === null) continue
      const w = record(limits[kind])
      const reset = typeof w?.reset_at === 'number' ? iso(w.reset_at * 1000) :
        typeof w?.reset_after_seconds === 'number' && Number.isFinite(w.reset_after_seconds) && w.reset_after_seconds >= 0 ? iso(now + w.reset_after_seconds * 1000) : undefined
      if (!w || !percent(w.used_percent) || !positive(w.limit_window_seconds) || !reset) { invalid = true; continue }
      windows.push({ kind, label: duration(w.limit_window_seconds), percentUsed: w.used_percent, resetsAt: reset, windowSeconds: w.limit_window_seconds })
    }
  } else if (provider === 'opencode-go') {
    const usage = record(data?.usage)
    if (!usage) return { status: 'unavailable', windows }
    for (const [kind, label] of [['rolling', 'Rolling'], ['weekly', 'Weekly'], ['monthly', 'Monthly']] as const) {
      const w = record(usage[kind]), reset = date(w?.resetsAt)
      if (!w || !percent(w.percent) || !reset || w.status !== 'ok' && w.status !== 'rate-limited') return { status: 'unavailable', windows: [] }
      windows.push({ kind, label, percentUsed: w.percent, resetsAt: reset })
    }
  } else {
    const quotas = record(data?.quota_snapshots)
    if (!quotas) return { status: 'unavailable', windows }
    for (const [kind, label] of [['premium_interactions', 'Premium requests'], ['chat', 'Chat'], ['completions', 'Completions']] as const) {
      if (quotas[kind] === undefined || quotas[kind] === null) continue
      const w = record(quotas[kind])
      if (!w) { invalid = true; continue }
      const entitlement = denominator(w.entitlement)
      if (w.unlimited === true || entitlement === 0) continue
      // Missing/pooled entitlements do not establish a finite denominator.
      if (w.entitlement === undefined) continue
      const reset = w.quota_reset_at === undefined || w.quota_reset_at === null ? date(data?.quota_reset_date_utc ?? data?.quota_reset_date) :
        positive(w.quota_reset_at) ? iso(w.quota_reset_at * 1000) : undefined
      if (w.unlimited !== false || entitlement === undefined || !percent(w.percent_remaining) || !reset ||
        w.quota_remaining !== undefined && (typeof w.quota_remaining !== 'number' || !Number.isFinite(w.quota_remaining) || w.quota_remaining < 0 || w.quota_remaining > entitlement)) {
        invalid = true
        continue
      }
      windows.push({ kind, label, percentUsed: 100 - w.percent_remaining, resetsAt: reset })
    }
  }
  return { status: windows.length ? 'ok' : invalid ? 'unavailable' : 'unsupported', windows: windows.slice(0, 8) }
}

async function bodyJSON(response: Response, signal: AbortSignal) {
  const reader = response.body?.getReader()
  if (!reader) throw new Error('Missing body')
  const cancel = () => { void reader.cancel().catch(() => {}) }
  signal.addEventListener('abort', cancel, { once: true })
  const chunks: Uint8Array[] = []
  let size = 0
  try {
    while (!signal.aborted) {
      const { done, value } = await reader.read()
      if (done) return JSON.parse(Buffer.concat(chunks).toString('utf8')) as unknown
      size += value.byteLength
      if (size > MAX_BODY) throw new Error('Body too large')
      chunks.push(value)
    }
    throw new Error('Aborted')
  } finally {
    signal.removeEventListener('abort', cancel)
    cancel()
  }
}

function retryAfter(value: string | null, now: number) {
  if (!value) return 0
  const at = /^\d+(?:\.\d+)?$/.test(value) ? now + Number(value) * 1000 : Date.parse(value)
  return Number.isFinite(at) && at > now ? Math.min(at, now + MAX_RETRY) : 0
}

async function read(s: Source, signal: AbortSignal): Promise<Pick<ProviderLimit, 'status' | 'windows'> & { retryAt: number }> {
  const url = URLS[s.provider]
  const response = await s.fetch!(url, {
    method: 'GET', redirect: 'error', signal,
    ...(s.key ? { headers: { Authorization: `Bearer ${s.key}` } } : {}),
  })
  if (signal.aborted || response.redirected || response.url && response.url !== url || !response.ok) {
    void response.body?.cancel().catch(() => {})
    const status = response.status === 401 || response.status === 403 ? 'auth_required' : response.status === 429 ? 'throttled' : 'unavailable'
    return { status, windows: [] as QuotaWindow[], retryAt: status === 'throttled' ? retryAfter(response.headers.get('retry-after'), Date.now()) : 0 }
  }
  return { ...normalize(s.provider, await bodyJSON(response, signal), Date.now()), retryAt: 0 }
}

function same(a: Source, b: Source) {
  return a.provider === b.provider && a.status === b.status && a.identity.length === b.identity.length && a.identity.every((v, i) => v === b.identity[i])
}

/** Tracks only observed runtime capabilities; snapshots contain no credentials or provider objects. */
export class QuotaCollector {
  private roots = new Map<string, Root>()
  private epoch = randomUUID()
  private sequence = 0
  private disposed = false
  private isOpen: (root: string) => boolean

  constructor(isOpen: (root: string) => boolean) {
    this.isOpen = isOpen
  }

  /** Allocates hook-entry order and fences prior OAuth reads for an already associated origin. */
  nextSequence(origin?: string) {
    const sequence = ++this.sequence
    if (origin === undefined || this.disposed) return sequence
    for (const root of this.roots.values()) {
      const previous = root.origins.get(origin)
      if (!previous?.source) continue
      const slot = root.slots.get(previous.source.provider), entry = slot?.entry
      if (!slot || !entry || entry.source.provider === 'opencode-go' || !entry.source.fetch) continue
      slot.controller?.abort()
      clearTimeout(slot.timer)
      slot.pending = { origin, sequence, report: slot.pending?.report ?? entry.report }
      slot.entry = { ...entry, report: { provider: entry.source.provider, status: 'unavailable', checkedAt: Date.now(), windows: [] } }
      this.bump(root)
    }
    return sequence
  }

  observe(rootID: string, origin: string, next: Source | undefined, defer = false, sequence = this.nextSequence()) {
    if (this.disposed || !this.isOpen(rootID) && !defer) return
    let root = this.roots.get(rootID)
    if (!root && !next) return
    if (!root) { root = { id: rootID, origins: new Map(), slots: new Map(), revision: 0 }; this.roots.set(rootID, root) }
    if (sequence <= (root.origins.get(origin)?.sequence ?? 0)) return
    root.origins.set(origin, { sequence, source: next })
    this.reconcile(root)
  }

  private reconcile(root: Root) {
    const selected = new Map<Source['provider'], Observation>()
    for (const observation of root.origins.values()) {
      const provider = observation.source?.provider
      if (provider && observation.sequence > (selected.get(provider)?.sequence ?? 0)) selected.set(provider, observation)
    }
    for (const provider of selected.keys()) if (!root.slots.has(provider)) root.slots.set(provider, { running: false, next: 0, failures: 0 })
    let changed = false
    for (const [provider, slot] of root.slots) {
      const observation = selected.get(provider), next = observation?.source, entry = slot.entry
      const report = slot.pending?.report ?? entry?.report
      if (slot.pending) {
        const pending = root.origins.get(slot.pending.origin)
        if (pending && pending.sequence < slot.pending.sequence && (observation?.sequence ?? 0) < slot.pending.sequence) continue
        slot.pending = undefined
      }
      if (!entry && !next) continue
      const equivalent = entry && next && same(entry.source, next)
      if (equivalent &&
        (provider === 'opencode-go' || !next.fetch || entry.observation === observation)) {
        entry.observation = observation!
        continue
      }
      slot.controller?.abort()
      clearTimeout(slot.timer)
      slot.entry = next ? {
        observation: observation!, source: next,
        report: equivalent && report ? report : { provider, status: next.status, checkedAt: Date.now(), windows: [] },
      } : undefined
      if (!equivalent) {
        slot.next = 0
        slot.failures = 0
      }
      changed = true
    }
    if (changed) this.bump(root)
    if (this.isOpen(root.id)) this.activate(root.id)
  }

  activate(rootID: string) {
    const root = this.roots.get(rootID)
    if (!root || !this.isOpen(rootID)) return
    for (const slot of root.slots.values()) this.start(root, slot)
  }

  private start(root: Root, slot: Slot) {
    const entry = slot.entry
    if (this.disposed || !entry?.source.fetch || slot.running || slot.pending) return
    if (!this.isOpen(root.id)) { this.clear(root.id); return }
    if (Date.now() < slot.next) { this.schedule(root, slot, entry); return }
    clearTimeout(slot.timer)
    slot.running = true
    slot.next = Date.now() + PERIOD
    const controller = slot.controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), DEADLINE)
    timer.unref?.()
    let onAbort: () => void
    const aborted = new Promise<never>((_, reject) => {
      onAbort = () => reject(new Error('Aborted'))
      controller.signal.addEventListener('abort', onAbort, { once: true })
    })
    let finished = false
    const resume = () => {
      if (slot.entry === entry) this.schedule(root, slot, entry)
      else this.start(root, slot)
    }
    // A slot outlives its presentation and holds the lock until the transport actually settles.
    const operation = read(entry.source, controller.signal).then(result => {
      if (slot.entry === entry) slot.next = Math.max(slot.next, result.retryAt)
      return result
    }).finally(() => {
      slot.running = false
      if (finished) resume()
    })
    void Promise.race([operation, aborted]).catch(() => ({ status: 'unavailable' as const, windows: [], retryAt: 0 })).then(result => {
      if (this.disposed) return
      if (slot.entry !== entry) return
      const now = Date.now()
      const transient = result.status === 'unavailable' || result.status === 'throttled'
      slot.failures = transient ? Math.min(slot.failures + 1, 5) : 0
      slot.next = Math.max(slot.next, now + Math.min(MAX_BACKOFF, PERIOD * 2 ** Math.max(0, slot.failures - 1)), result.retryAt)
      if (!this.isOpen(root.id)) { this.clear(root.id); return }
      const retain = transient && entry.source.provider === 'opencode-go'
      entry.report = {
        provider: entry.source.provider, status: result.status, checkedAt: now,
        ...(result.status === 'ok' ? { measuredAt: now } : retain && entry.report.measuredAt !== undefined ? { measuredAt: entry.report.measuredAt } : {}),
        ...(transient ? { retryAt: Math.min(slot.next, now + MAX_RETRY) } : {}),
        windows: retain ? entry.report.windows : result.windows,
      }
      this.bump(root)
    }).finally(() => {
      clearTimeout(timer)
      controller.signal.removeEventListener('abort', onAbort!)
      finished = true
      resume()
    })
  }

  private schedule(root: Root, slot: Slot, entry: Entry) {
    if (slot.entry !== entry) return
    clearTimeout(slot.timer)
    if (this.disposed || slot.running || slot.pending || !slot.entry?.source.fetch) return
    if (!this.isOpen(root.id)) { this.clear(root.id); return }
    slot.timer = setTimeout(() => {
      if (slot.entry === entry) this.start(root, slot)
    }, Math.max(1, slot.next - Date.now()))
    slot.timer.unref?.()
  }

  private bump(root: Root) {
    root.revision = Math.min(Number.MAX_SAFE_INTEGER, root.revision + 1)
  }

  snapshot(root: string): ProviderLimit[] | undefined {
    const state = this.roots.get(root)
    if (!state) return
    return [...state.slots.values()].flatMap(({ entry }) => entry ? [{ ...entry.report, windows: entry.report.windows.map(w => ({ ...w })) }] : [])
  }

  metadata(root: string) {
    const state = this.roots.get(root)
    if (!state) return
    return { providerLimitsEpoch: this.epoch, providerLimitsRevision: state.revision }
  }

  remove(...origins: string[]) {
    for (const root of this.roots.values()) {
      let changed = false
      for (const origin of origins) if (root.origins.delete(origin)) changed = true
      if (changed) this.reconcile(root)
    }
  }

  clear(rootID: string) {
    const root = this.roots.get(rootID)
    if (!root) return
    for (const slot of root.slots.values()) {
      slot.controller?.abort()
      clearTimeout(slot.timer)
      slot.entry = undefined
      slot.pending = undefined
      slot.next = 0
      slot.failures = 0
    }
    root.origins.clear()
    this.bump(root)
  }

  dispose() {
    this.disposed = true
    for (const id of this.roots.keys()) this.clear(id)
    this.roots.clear()
  }
}
