// @clipmux/uploader uploads a file to ClipMux in parts, straight to storage
// (docs/api.md, "Integration API"). Your backend creates the upload with its
// API key (POST /v1/uploads) and hands the response to the browser; this
// sends the parts with the upload token, resumes from what storage already
// holds, and completes the upload.
//
// Or give it createUrl, a route of your backend (the @clipmux/node handler's
// /upload) that creates the upload and answers with it plus endpoint, the
// ClipMux URL. The upload is then remembered in localStorage, so a reload
// resumes it. <clipmux-uploader> (./element.ts) wraps this in a drop zone.

/** The 201 body of POST /v1/uploads, relayed by your backend. */
export interface CreatedUpload {
  upload_id: string
  upload_token: string
  part_size: number
  part_count: number
  asset_id?: string
  /** ClipMux's public URL, when the backend relays it (createUrl does). */
  endpoint?: string
}

/** The asset ClipMux returns once the upload completes. */
export interface Asset {
  id: string
  status: string
  version: number
  [field: string]: unknown
}

export interface Progress {
  bytesUploaded: number
  bytesTotal: number
  partsDone: number
  partsTotal: number
}

/** Uploads one part's bytes to its signed URL. */
export type PutPart = (
  url: string,
  body: Blob,
  options: { signal: AbortSignal; onBytes: (sent: number) => void },
) => Promise<{ status: number; etag: string | null; retryAfter: string | null }>

export interface UploadOptions {
  /**
   * ClipMux's public URL, e.g. https://video.example.com. Optional when
   * the upload carries endpoint, as createUrl's answer does.
   */
  endpoint?: string
  /** What POST /v1/uploads returned to your backend. */
  upload?: CreatedUpload
  /**
   * Instead of upload: your backend's URL that creates it. start() POSTs
   * {size, name, type} there (with the page's cookies) and uses the answer,
   * which must carry endpoint unless that option is set. 401 or 403 there
   * rejects with code create_denied.
   */
  createUrl?: string
  /**
   * Where a createUrl upload is remembered until it completes, so a reload
   * resumes it: localStorage by default; null to forget on reload.
   */
  store?: UploadStore | null
  /** Parts in flight at once (default 4). */
  concurrency?: number
  onProgress?: (progress: Progress) => void
  signal?: AbortSignal
  /** Tries per request before giving up (default 6). */
  maxAttempts?: number
  fetch?: typeof fetch
  put?: PutPart
  delay?: (ms: number, signal: AbortSignal) => Promise<void>
  random?: () => number
  now?: () => number
}

/** A failure the upload can't recover from by itself. */
export class ClipMuxUploadError extends Error {
  readonly code: string
  readonly status: number
  readonly retryable: boolean
  constructor(code: string, message: string, status = 0, retryable = false) {
    super(message)
    this.name = 'ClipMuxUploadError'
    this.code = code
    this.status = status
    this.retryable = retryable
  }
}

export interface PlannedPart {
  number: number
  start: number
  end: number
}

/** Splits size bytes into part_size parts; the last holds the remainder. */
export function planParts(size: number, partSize: number): PlannedPart[] {
  const parts: PlannedPart[] = []
  for (let start = 0, n = 1; start < size; start += partSize, n++) {
    parts.push({ number: n, start, end: Math.min(start + partSize, size) })
  }
  return parts
}

const SIGN_BATCH = 100
// A signed URL with less than this left is signed again before use.
const URL_MARGIN_MS = 5 * 60 * 1000

interface Signed {
  url: string
  expiresAt: number
}

class Paused extends Error {}

/** One file's upload. start() resolves with the asset once it completes. */
type Defaulted = 'concurrency' | 'maxAttempts' | 'fetch' | 'put' | 'delay' | 'random' | 'now'

export class ClipMuxUpload {
  readonly #file: Blob
  readonly #opts: Required<Pick<UploadOptions, Defaulted>> & Omit<UploadOptions, Defaulted>
  #created: CreatedUpload | null
  #api = ''
  #memory: ReturnType<typeof rememberUpload> | null = null
  #fromMemory = false
  #run: AbortController | null = null
  #paused = false
  #resumed: (() => void) | null = null
  #cancelled = false
  #sent = new Map<number, number>()
  #done = new Set<number>()

  constructor(file: Blob, options: UploadOptions) {
    this.#file = file
    const f = options.fetch ?? globalThis.fetch.bind(globalThis)
    this.#opts = {
      concurrency: 4,
      maxAttempts: 6,
      fetch: f,
      put: options.put ?? defaultPut(f),
      delay: sleep,
      random: Math.random,
      now: Date.now,
      ...options,
    }
    this.#created = null
    if (options.upload) this.#use(options.upload)
    else if (!options.createUrl) throw new TypeError('ClipMuxUpload needs upload or createUrl')
    if (options.createUrl) {
      const store = options.store === undefined ? defaultStore() : options.store
      this.#memory = store ? rememberUpload(store) : null
    }
  }

  /** The upload in use: given, remembered or created; null until start() has one. */
  get upload(): CreatedUpload | null {
    return this.#created
  }

  #use(upload: CreatedUpload): void {
    const endpoint = this.#opts.endpoint ?? upload.endpoint
    if (!endpoint) throw new TypeError('ClipMuxUpload needs endpoint, as an option or on the upload')
    this.#created = upload
    this.#api = `${endpoint.replace(/\/+$/, '')}/v1/uploads/${encodeURIComponent(upload.upload_id)}`
  }

  /**
   * Uploads what storage doesn't hold yet and completes the upload. After a
   * rejection, calling it again retries from what storage holds.
   */
  async start(): Promise<Asset> {
    for (;;) {
      try {
        const asset = await this.#attempt()
        this.#memory?.forget(this.#file)
        return asset
      } catch (err) {
        if (this.#fromMemory && stale(err) && !this.#cancelled) {
          // The remembered upload is gone (expired, cancelled, another
          // file's): forget it and create a new one, once.
          this.#memory?.forget(this.#file)
          this.#fromMemory = false
          this.#created = null
          this.#done.clear()
          this.#sent.clear()
          continue
        }
        if (!(err instanceof Paused) || this.#cancelled) throw err
        await new Promise<void>((resolve) => {
          this.#resumed = resolve
        })
      }
    }
  }

  // createUrl: the remembered upload of this file, or a new one.
  async #prepare(signal: AbortSignal): Promise<void> {
    if (this.#created) return
    const remembered = this.#memory?.load(this.#file)
    if (remembered && (remembered.endpoint || this.#opts.endpoint)) {
      this.#fromMemory = true
      return this.#use(remembered)
    }
    const created = await this.#create(signal)
    this.#use(created)
    this.#memory?.save(this.#file, created)
  }

  async #create(signal: AbortSignal): Promise<CreatedUpload> {
    const { maxAttempts, fetch } = this.#opts
    const file = this.#file as Blob & { name?: string }
    const body = JSON.stringify({ size: file.size, name: file.name ?? '', type: file.type })
    let last: unknown
    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      let res: Response
      try {
        res = await fetch(this.#opts.createUrl as string, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body,
          signal,
        })
      } catch (err) {
        if (signal.aborted) throw signal.reason
        last = new ClipMuxUploadError('network', 'the create URL could not be reached', 0, true)
        await this.#backoff(attempt, null, signal, err)
        continue
      }
      if (res.ok) {
        const created = (await res.json().catch(() => null)) as CreatedUpload | null
        if (!created || typeof created.upload_id !== 'string' || typeof created.upload_token !== 'string') {
          throw new ClipMuxUploadError(
            'create_invalid',
            'the create URL answered without an upload',
            res.status,
          )
        }
        return created
      }
      if (res.status >= 500 || res.status === 429) {
        last = await apiError(res)
        await this.#backoff(attempt, res.headers.get('Retry-After'), signal)
        continue
      }
      const err = await apiError(res)
      if (res.status === 401 || res.status === 403) {
        throw new ClipMuxUploadError('create_denied', err.message, res.status)
      }
      throw err
    }
    throw last
  }

  /** Stops sending; parts already stored stay, and resume() continues. */
  pause(): void {
    this.#paused = true
    this.#run?.abort(new Paused())
  }

  resume(): void {
    this.#paused = false
    this.#resumed?.()
    this.#resumed = null
  }

  /** Stops for good and tells ClipMux to abort the upload. */
  async cancel(): Promise<void> {
    this.#cancelled = true
    this.#run?.abort(new ClipMuxUploadError('cancelled', 'the upload was cancelled'))
    this.#resumed?.()
    this.#memory?.forget(this.#file)
    if (!this.#created) return
    for (let attempt = 0; attempt < 3; attempt++) {
      try {
        const res = await this.#opts.fetch(`${this.#api}/cancel`, {
          method: 'POST',
          headers: this.#headers(),
          body: '{}',
        })
        if (res.ok || res.status < 500) return
      } catch {
        // best effort
      }
    }
  }

  #headers(): Record<string, string> {
    return { Authorization: `Bearer ${this.#created?.upload_token}`, 'Content-Type': 'application/json' }
  }

  #report(): void {
    const plan = planParts(this.#file.size, (this.#created as CreatedUpload).part_size)
    let bytes = 0
    for (const p of plan)
      bytes += this.#done.has(p.number) ? p.end - p.start : (this.#sent.get(p.number) ?? 0)
    this.#opts.onProgress?.({
      bytesUploaded: bytes,
      bytesTotal: this.#file.size,
      partsDone: this.#done.size,
      partsTotal: plan.length,
    })
  }

  async #attempt(): Promise<Asset> {
    if (this.#cancelled) throw new ClipMuxUploadError('cancelled', 'the upload was cancelled')
    if (this.#paused) throw new Paused()
    const run = new AbortController()
    this.#run = run
    const outer = this.#opts.signal
    const onOuter = () =>
      run.abort(outer?.reason ?? new ClipMuxUploadError('aborted', 'the upload was aborted'))
    if (outer?.aborted) onOuter()
    outer?.addEventListener('abort', onOuter, { once: true })
    try {
      await this.#prepare(run.signal)
      return await this.#upload(run.signal)
    } catch (err) {
      throw run.signal.aborted ? (run.signal.reason ?? err) : err
    } finally {
      outer?.removeEventListener('abort', onOuter)
      this.#run = null
    }
  }

  async #upload(signal: AbortSignal): Promise<Asset> {
    const upload = this.#created as CreatedUpload
    const plan = planParts(this.#file.size, upload.part_size)
    if (plan.length !== upload.part_count) {
      throw new ClipMuxUploadError(
        'file_mismatch',
        `this file is ${plan.length} parts of ${upload.part_size} bytes, but the upload was created for ${upload.part_count}`,
      )
    }
    let relisted = false
    for (;;) {
      const stored = await this.#listParts(signal)
      if (stored === 'finished') return this.#complete(new Map(), signal)
      for (const p of plan) {
        const s = stored.get(p.number)
        if (s && s.size !== p.end - p.start) {
          throw new ClipMuxUploadError(
            'part_size_mismatch',
            `stored part ${p.number} is ${s.size} bytes, not ${p.end - p.start}`,
          )
        }
      }
      const etags = new Map<number, string>()
      for (const [n, s] of stored) {
        etags.set(n, s.etag)
        this.#done.add(n)
      }
      this.#report()
      await this.#sendMissing(
        plan.filter((p) => !stored.has(p.number)),
        etags,
        signal,
      )
      try {
        return await this.#complete(etags, signal)
      } catch (err) {
        // Storage disagreed about what it holds: list again once and fill in.
        if (err instanceof ClipMuxUploadError && err.code === 'invalid_parts' && !relisted) {
          relisted = true
          this.#done.clear()
          continue
        }
        throw err
      }
    }
  }

  async #listParts(signal: AbortSignal): Promise<Map<number, { etag: string; size: number }> | 'finished'> {
    const res = await this.#request(`${this.#api}/parts`, { method: 'GET' }, signal)
    if (res.status === 409) {
      // The sweeper (or another tab) may have finished it: completing again
      // returns the asset when it did.
      return 'finished'
    }
    const body = (await res.json()) as { parts: { part_number: number; etag: string; size: number }[] }
    return new Map(body.parts.map((p) => [p.part_number, { etag: p.etag, size: p.size }]))
  }

  async #complete(etags: Map<number, string>, signal: AbortSignal): Promise<Asset> {
    const parts = [...etags].sort(([a], [b]) => a - b).map(([part_number, etag]) => ({ part_number, etag }))
    const res = await this.#request(
      `${this.#api}/complete`,
      { method: 'POST', body: JSON.stringify({ parts }) },
      signal,
    )
    return (await res.json()) as Asset
  }

  /** A control-plane call, retried on network errors, 5xx and 429. */
  async #request(url: string, init: RequestInit, signal: AbortSignal): Promise<Response> {
    const { maxAttempts, fetch } = this.#opts
    let last: unknown
    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      let res: Response
      try {
        res = await fetch(url, { ...init, headers: this.#headers(), signal })
      } catch (err) {
        if (signal.aborted) throw signal.reason
        last = new ClipMuxUploadError('network', 'ClipMux could not be reached', 0, true)
        await this.#backoff(attempt, null, signal, err)
        continue
      }
      if (res.ok || res.status === 409) {
        if (res.status === 409 && !url.endsWith('/parts')) throw await apiError(res)
        return res
      }
      if (res.status >= 500 || res.status === 429) {
        last = await apiError(res)
        await this.#backoff(attempt, res.headers.get('Retry-After'), signal)
        continue
      }
      throw await apiError(res)
    }
    throw last
  }

  async #backoff(
    attempt: number,
    retryAfter: string | null,
    signal: AbortSignal,
    _cause?: unknown,
  ): Promise<void> {
    if (attempt >= this.#opts.maxAttempts) return
    await waitOnline(signal)
    const seconds = retryAfter === null ? Number.NaN : Number(retryAfter)
    const ms =
      Number.isFinite(seconds) && seconds >= 0
        ? seconds * 1000
        : Math.min(30_000, 1000 * 2 ** (attempt - 1)) + Math.floor(this.#opts.random() * 1000)
    await this.#opts.delay(ms, signal)
  }

  async #sendMissing(missing: PlannedPart[], etags: Map<number, string>, signal: AbortSignal): Promise<void> {
    const urls = new Map<number, Signed>()
    const unsent = [...missing] // not yet taken by a worker
    const unfinished = new Set(missing.map((p) => p.number)) // not yet stored, in order
    let signing: Promise<void> | null = null
    // One sign call at a time, for up to 100 unfinished parts without a URL
    // that will still be good in five minutes.
    const sign = (): Promise<void> => {
      if (signing) return signing
      const cutoff = this.#opts.now() + URL_MARGIN_MS
      const want = [...unfinished].filter((n) => (urls.get(n)?.expiresAt ?? 0) <= cutoff).slice(0, SIGN_BATCH)
      signing = (async () => {
        try {
          const res = await this.#request(
            `${this.#api}/parts`,
            { method: 'POST', body: JSON.stringify({ part_numbers: want }) },
            signal,
          )
          const body = (await res.json()) as {
            parts: { part_number: number; url: string }[]
            expires_at: string
          }
          const expiresAt = Date.parse(body.expires_at)
          for (const p of body.parts) urls.set(p.part_number, { url: p.url, expiresAt })
        } finally {
          signing = null
        }
      })()
      return signing
    }
    const urlFor = async (n: number, fresh: boolean): Promise<string> => {
      if (fresh) urls.delete(n)
      for (let tries = 0; tries < 3; tries++) {
        const u = urls.get(n)
        if (u && u.expiresAt > this.#opts.now() + URL_MARGIN_MS) return u.url
        await sign()
      }
      throw new ClipMuxUploadError('sign_failed', `ClipMux signed no URL for part ${n}`)
    }
    const fail = new AbortController()
    const both = AbortSignal.any([signal, fail.signal])
    const work = async (): Promise<void> => {
      for (let part = unsent.shift(); part; part = unsent.shift()) {
        etags.set(part.number, await this.#putPart(part, urlFor, both))
        unfinished.delete(part.number)
        urls.delete(part.number)
      }
    }
    const workers = Array.from({ length: Math.min(this.#opts.concurrency, missing.length) }, () =>
      work().catch((err: unknown) => {
        // The first fatal error stops the other parts.
        if (!fail.signal.aborted) fail.abort(err)
        throw err
      }),
    )
    const results = await Promise.allSettled(workers)
    if (results.some((r) => r.status === 'rejected'))
      throw signal.aborted ? signal.reason : fail.signal.reason
  }

  async #putPart(
    part: PlannedPart,
    urlFor: (n: number, fresh: boolean) => Promise<string>,
    signal: AbortSignal,
  ): Promise<string> {
    const body = this.#file.slice(part.start, part.end)
    let resigned = false
    let fresh = false
    for (let attempt = 1; ; ) {
      // A pause or a sibling's failure between parts stops here.
      if (signal.aborted) throw signal.reason
      const url = await urlFor(part.number, fresh)
      fresh = false
      this.#sent.set(part.number, 0)
      let res: Awaited<ReturnType<PutPart>>
      try {
        res = await this.#opts.put(url, body, {
          signal,
          onBytes: (sent) => {
            this.#sent.set(part.number, sent)
            this.#report()
          },
        })
      } catch (err) {
        if (signal.aborted) throw signal.reason
        if (attempt >= this.#opts.maxAttempts) {
          throw new ClipMuxUploadError('network', `part ${part.number} could not be sent`, 0, true)
        }
        await this.#backoff(attempt++, null, signal, err)
        continue
      }
      if (res.status >= 200 && res.status < 300) {
        if (!res.etag) {
          throw new ClipMuxUploadError(
            'etag_not_exposed',
            'storage answered without an ETag header; its CORS rules must expose ETag',
            res.status,
          )
        }
        this.#done.add(part.number)
        this.#sent.delete(part.number)
        this.#report()
        return res.etag
      }
      if (res.status === 403 && !resigned) {
        // An expired signature: sign this part again, once, without
        // counting a retry.
        resigned = true
        fresh = true
        continue
      }
      if (res.status === 403)
        throw new ClipMuxUploadError('part_url_rejected', `storage refused part ${part.number}`, 403)
      if (
        (res.status >= 500 || res.status === 408 || res.status === 429) &&
        attempt < this.#opts.maxAttempts
      ) {
        await this.#backoff(attempt++, res.retryAfter, signal)
        continue
      }
      throw new ClipMuxUploadError(
        'part_rejected',
        `storage refused part ${part.number} (HTTP ${res.status})`,
        res.status,
      )
    }
  }
}

// Whether a failure means the upload itself is gone or isn't this file's,
// rather than the network or storage failing.
function stale(err: unknown): boolean {
  if (!(err instanceof ClipMuxUploadError)) return false
  return (
    err.code === 'file_mismatch' ||
    err.code === 'upload_token_invalid' ||
    err.status === 404 ||
    err.status === 409 ||
    err.status === 410
  )
}

function defaultStore(): UploadStore | null {
  try {
    return (globalThis as { localStorage?: UploadStore }).localStorage ?? null
  } catch {
    return null // storage is blocked
  }
}

async function apiError(res: Response): Promise<ClipMuxUploadError> {
  let code = `http_${res.status}`
  let message = `ClipMux answered HTTP ${res.status}`
  try {
    const body = (await res.json()) as { error?: { code?: string; message?: string } }
    if (body.error?.code) code = body.error.code
    if (body.error?.message) message = body.error.message
  } catch {
    // not JSON
  }
  if (res.status === 401) code = 'upload_token_invalid'
  return new ClipMuxUploadError(code, message, res.status, res.status >= 500 || res.status === 429)
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) return reject(signal.reason)
    const t = setTimeout(() => {
      signal.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    const onAbort = () => {
      clearTimeout(t)
      reject(signal.reason)
    }
    signal.addEventListener('abort', onAbort, { once: true })
  })
}

/** Waits while the browser reports being offline, without using up tries. */
function waitOnline(signal: AbortSignal): Promise<void> {
  const nav = (globalThis as { navigator?: { onLine?: boolean } }).navigator
  if (nav?.onLine !== false || typeof globalThis.addEventListener !== 'function') return Promise.resolve()
  return new Promise((resolve, reject) => {
    const done = () => {
      globalThis.removeEventListener('online', done)
      signal.removeEventListener('abort', aborted)
      resolve()
    }
    const aborted = () => {
      globalThis.removeEventListener('online', done)
      reject(signal.reason)
    }
    globalThis.addEventListener('online', done, { once: true })
    signal.addEventListener('abort', aborted, { once: true })
  })
}

/** PUTs with XMLHttpRequest where there is one (byte progress), else fetch. */
function defaultPut(f: typeof fetch): PutPart {
  if (typeof XMLHttpRequest === 'undefined') {
    return async (url, body, { signal, onBytes }) => {
      const res = await f(url, { method: 'PUT', body, signal })
      if (res.ok) onBytes(body.size)
      await res.body?.cancel()
      return { status: res.status, etag: res.headers.get('ETag'), retryAfter: res.headers.get('Retry-After') }
    }
  }
  return (url, body, { signal, onBytes }) =>
    new Promise((resolve, reject) => {
      if (signal.aborted) return reject(signal.reason)
      const xhr = new XMLHttpRequest()
      xhr.open('PUT', url)
      xhr.upload.onprogress = (e) => onBytes(e.loaded)
      xhr.onload = () =>
        resolve({
          status: xhr.status,
          etag: xhr.getResponseHeader('ETag'),
          retryAfter: xhr.getResponseHeader('Retry-After'),
        })
      xhr.onerror = () => reject(new Error('network'))
      xhr.onabort = () => reject(signal.reason)
      signal.addEventListener('abort', () => xhr.abort(), { once: true })
      xhr.send(body)
    })
}

/** Where an unfinished upload is remembered across a reload. */
export interface UploadStore {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

/**
 * Remembers an upload per file in storage (e.g. localStorage), so a reload
 * can resume it. Only the upload's ID, token and part layout are kept;
 * ClipMux itself knows which parts it holds.
 */
export function rememberUpload(store: UploadStore) {
  const key = (file: Blob & { name?: string; lastModified?: number }) =>
    `clipmux:upload:${file.name ?? ''}:${file.size}:${file.lastModified ?? 0}`
  return {
    save(file: Blob, upload: CreatedUpload): void {
      const { upload_id, upload_token, part_size, part_count, endpoint } = upload
      try {
        store.setItem(key(file), JSON.stringify({ upload_id, upload_token, part_size, part_count, endpoint }))
      } catch {
        // full or blocked: the upload just won't resume after a reload
      }
    },
    load(file: Blob): CreatedUpload | null {
      try {
        const v = JSON.parse(store.getItem(key(file)) ?? 'null') as CreatedUpload | null
        return v && typeof v.upload_id === 'string' && typeof v.upload_token === 'string' ? v : null
      } catch {
        return null
      }
    },
    forget(file: Blob): void {
      try {
        store.removeItem(key(file))
      } catch {
        // blocked
      }
    },
  }
}
