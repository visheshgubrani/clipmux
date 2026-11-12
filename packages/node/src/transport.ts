// HTTP to the ClipMux API: JSON in and out, an Idempotency-Key on every
// POST (reused across its own retries, so a retry can't do the work twice),
// and retries for what the API says is temporary: 408, 429 (after
// Retry-After), 5xx, idempotency_in_progress and network errors.

import { ClipMuxError } from './errors.ts'

export interface TransportOptions {
  baseUrl: string
  /** Returns the API key, throwing when it isn't configured. */
  apiKey: () => string
  fetch: typeof fetch
  maxRetries: number
  timeoutMs: number
  userAgent: string
  /** Waits; tests replace it. */
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>
}

export interface RequestOptions {
  method: 'GET' | 'POST' | 'PATCH' | 'DELETE'
  path: string
  query?: Record<string, string | number | boolean | undefined> | undefined
  body?: unknown
  /** `key` (default) sends the API key; `none` sends nothing. */
  auth?: 'key' | 'none'
  idempotencyKey?: string | undefined
  signal?: AbortSignal | undefined
  /** Longer than timeoutMs, for long-polls. */
  timeoutMs?: number
}

export interface Response<T> {
  status: number
  body: T
}

const sleepFor = (ms: number, signal?: AbortSignal) =>
  new Promise<void>((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason)
    const t = setTimeout(resolve, ms)
    signal?.addEventListener(
      'abort',
      () => {
        clearTimeout(t)
        reject(signal.reason)
      },
      { once: true },
    )
  })

export class Transport {
  readonly #o: TransportOptions
  constructor(o: TransportOptions) {
    this.#o = o
  }

  get baseUrl(): string {
    return this.#o.baseUrl
  }

  async request<T>(r: RequestOptions): Promise<Response<T>> {
    const url = new URL(this.#o.baseUrl.replace(/\/+$/, '') + r.path)
    for (const [k, v] of Object.entries(r.query ?? {}))
      if (v !== undefined) url.searchParams.set(k, String(v))
    const name = `${r.method} ${r.path}`
    const headers: Record<string, string> = { Accept: 'application/json', 'User-Agent': this.#o.userAgent }
    if ((r.auth ?? 'key') === 'key') headers.Authorization = `Bearer ${this.#o.apiKey()}`
    if (r.body !== undefined) headers['Content-Type'] = 'application/json'
    if (r.method === 'POST') headers['Idempotency-Key'] = r.idempotencyKey ?? crypto.randomUUID()
    const sleep = this.#o.sleep ?? sleepFor
    const body = r.body === undefined ? undefined : JSON.stringify(r.body)

    for (let attempt = 0; ; attempt++) {
      const timeout = AbortSignal.timeout(r.timeoutMs ?? this.#o.timeoutMs)
      const signal = r.signal ? AbortSignal.any([r.signal, timeout]) : timeout
      let res: globalThis.Response
      try {
        res = await this.#o.fetch(url, { method: r.method, headers, body: body ?? null, signal })
      } catch (err) {
        if (r.signal?.aborted) throw r.signal.reason
        if (attempt < this.#o.maxRetries) {
          await sleep(backoff(attempt), r.signal)
          continue
        }
        const why = timeout.aborted ? 'timed out' : err instanceof Error ? err.message : String(err)
        throw new ClipMuxError(0, 'network', `could not reach ${url.origin} (${why})`, name)
      }
      const text = await res.text()
      let parsed: unknown = null
      if (text) {
        try {
          parsed = JSON.parse(text)
        } catch {
          parsed = null
        }
      }
      if (res.ok) return { status: res.status, body: parsed as T }

      const e = (parsed as { error?: Record<string, string> } | null)?.error
      const code = e?.code ?? `http_${res.status}`
      if (attempt < this.#o.maxRetries && retryable(res.status, code)) {
        await res.body?.cancel().catch(() => {})
        await sleep(retryAfter(res) ?? backoff(attempt), r.signal)
        continue
      }
      const { code: _c, message, ...details } = e ?? {}
      throw new ClipMuxError(
        res.status,
        code,
        message ?? (text.slice(0, 200) || res.statusText),
        name,
        details,
      )
    }
  }
}

function retryable(status: number, code: string): boolean {
  return status === 408 || status === 429 || status >= 500 || code === 'idempotency_in_progress'
}

// backoff is 0.5 s, 1 s, 2 s, … with up to half again of jitter, at most 30 s.
function backoff(attempt: number): number {
  const base = Math.min(30_000, 500 * 2 ** attempt)
  return base + Math.floor(Math.random() * base * 0.5)
}

function retryAfter(res: globalThis.Response): number | null {
  const v = res.headers.get('Retry-After')
  if (!v) return null
  const s = Number(v)
  if (Number.isFinite(s) && s >= 0) return Math.min(s, 60) * 1000
  return null
}
