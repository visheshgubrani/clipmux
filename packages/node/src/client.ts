// The ClipMux client: one object, configured from the environment `init`
// writes, with a method per integration API operation (the OpenAPI
// operationIds), offline playback tokens and webhook verification.

import { ClipMuxConfigError, ClipMuxError } from './errors.ts'
import { EventWindow } from './events.ts'
import type * as api from './schema.gen.ts'
import { type Audience, importSigningKey, seconds, signToken, type TokenClaims } from './tokens.ts'
import { Transport } from './transport.ts'
import { type HeadersLike, type RawBody, type VerifyOptions, verifyWebhook } from './webhooks.ts'

export const VERSION = '0.2.1'

const ENV = {
  url: 'CLIPMUX_URL',
  apiKey: 'CLIPMUX_API_KEY',
  signingKid: 'CLIPMUX_SIGNING_KID',
  signingKey: 'CLIPMUX_SIGNING_KEY',
  webhookSecret: 'CLIPMUX_WEBHOOK_SECRET',
} as const

export interface ClipMuxOptions {
  /** The instance's public URL. Default `CLIPMUX_URL`. */
  url?: string
  /** The app's API key (cmx_…). Default `CLIPMUX_API_KEY`. */
  apiKey?: string
  /** The signing key's ID (kid_…). Default `CLIPMUX_SIGNING_KID`. */
  signingKid?: string
  /** The signing key's PKCS #8 PEM. Default `CLIPMUX_SIGNING_KEY`. */
  signingKey?: string
  /** The webhook endpoint's secret (whsec_…). Default `CLIPMUX_WEBHOOK_SECRET`. */
  webhookSecret?: string
  /** Where the defaults come from; default `process.env` where there is one. */
  env?: Record<string, string | undefined>
  fetch?: typeof fetch
  /** Retries of a temporary failure; default 3. */
  maxRetries?: number
  /** Per-request timeout; default 30 s. */
  timeoutMs?: number
}

/**
 * What uploads.create returns: the upload with its token and `endpoint`,
 * the object `new ClipMuxUpload(file, { upload })` takes as it is.
 */
export type CreatedUpload = api.Upload & { upload_token: string; endpoint: string }

/** What a player needs to play one playback ID: the session-url answer. */
export interface PlaybackSession {
  playback_id: string
  base_url: string
  /** Empty for a public playback ID. */
  token: string
  expires_at: string | null
  poster_url: string
  title?: string
  duration_seconds: number | null
  /** The asset's ready tracks. */
  tracks: api.Track[]
}

export interface TokenOptions {
  /** Seconds, or `90s`, `10m`, `4h`, `7d`. Default 15 minutes. */
  expiresIn?: number | string | undefined
  /** `video` (default) plays everything; `poster` fetches only the poster. */
  audience?: Audience | undefined
  /** A stable ID for this viewer, for unique-viewer analytics (D103). */
  viewerId?: string | undefined
  /**
   * The player plays HLS itself and can't refresh (getToken's `native`):
   * the token lasts the larger of twice `durationSeconds` and 4 hours (12
   * hours without a duration), within the server's maximum. Overrides
   * `expiresIn`.
   */
  native?: boolean | undefined
  /** The video's duration, for a native token. */
  durationSeconds?: number | null | undefined
}

export interface SessionOptions {
  viewerId?: string | undefined
  /**
   * The browser plays HLS itself (iOS before 17.1, AirPlay), so the token
   * can't be refreshed: it is signed for the larger of twice the video's
   * duration and 4 hours, within the server's maximum. `<clipmux-player>`
   * sends this.
   */
  native?: boolean | undefined
  /** For a refreshable player; default 15 minutes. */
  expiresIn?: number | string | undefined
}

type Target = api.Asset | string

const DEFAULT_TTL = 15 * 60
const FALLBACK_MAX_TTL = 7 * 86400

function readEnv(): Record<string, string | undefined> {
  const g = globalThis as { process?: { env?: Record<string, string | undefined> } }
  return g.process?.env ?? {}
}

export class ClipMux {
  readonly uploads
  readonly assets
  readonly tracks
  readonly playbackIds
  readonly shares
  readonly events
  readonly analytics
  readonly app
  readonly playback
  readonly webhooks

  readonly #o: Required<Pick<ClipMuxOptions, 'fetch' | 'maxRetries' | 'timeoutMs'>> & ClipMuxOptions
  readonly #env: Record<string, string | undefined>
  readonly #http: Transport
  #key: Promise<CryptoKey> | undefined
  #app: Promise<api.App> | undefined

  constructor(options: ClipMuxOptions = {}) {
    this.#env = options.env ?? readEnv()
    this.#o = {
      maxRetries: 3,
      timeoutMs: 30_000,
      ...options,
      fetch: options.fetch ?? globalThis.fetch.bind(globalThis),
    }
    const url = this.#setting('url')
    if (url !== undefined && !/^https?:\/\//.test(url)) {
      throw new ClipMuxConfigError(ENV.url, `${ENV.url} must be an http(s) URL, got ${JSON.stringify(url)}.`)
    }
    this.#http = new Transport({
      baseUrl: url ?? 'http://clipmux.invalid',
      apiKey: () => {
        if (url === undefined) this.#require('url')
        return this.#require('apiKey')
      },
      fetch: this.#o.fetch,
      maxRetries: this.#o.maxRetries,
      timeoutMs: this.#o.timeoutMs,
      userAgent: `clipmux-node/${VERSION}`,
    })
    const h = this.#http
    const id = encodeURIComponent

    this.uploads = {
      /**
       * Creates an upload for a browser to send. Hand the result to
       * `@clipmux/uploader`; it carries `endpoint`, so the browser needs no
       * other configuration, and its token reaches only this upload.
       */
      create: async (body: api.CreateUpload, o: { idempotencyKey?: string } = {}): Promise<CreatedUpload> => {
        const r = await h.request<api.Upload>({ method: 'POST', path: '/v1/uploads', body, ...o })
        // A replay of an idempotency key whose upload has since completed,
        // been cancelled or expired carries no token: nothing can be sent.
        if (!r.body.upload_token) {
          throw new ClipMuxError(
            r.status,
            'upload_not_active',
            'this idempotency key made an upload that is no longer active, so it has no upload_token',
            'POST /v1/uploads',
          )
        }
        return { ...r.body, upload_token: r.body.upload_token, endpoint: h.baseUrl.replace(/\/+$/, '') }
      },
      /** Cancels an upload; its asset becomes `errored` / `upload_cancelled`. */
      cancel: async (uploadId: string) =>
        (await h.request<api.Asset>({ method: 'POST', path: `/v1/uploads/${id(uploadId)}/cancel` })).body,
    }

    this.assets = {
      get: async (assetId: string) =>
        (await h.request<api.Asset>({ method: 'GET', path: `/v1/assets/${id(assetId)}` })).body,
      /** Your own reference's assets, newest first. */
      findByPassthrough: async (passthrough: string) =>
        (await h.request<api.AssetList>({ method: 'GET', path: '/v1/assets', query: { passthrough } })).body
          .data,
      /**
       * Every asset changed since a time, oldest first, deleted ones
       * included: the catch-up for missed webhooks.
       */
      changes: async function* (o: { since: Date | string; limit?: number }) {
        const since = typeof o.since === 'string' ? o.since : o.since.toISOString()
        let cursor: string | undefined
        do {
          const page = (
            await h.request<api.AssetList>({
              method: 'GET',
              path: '/v1/assets',
              query: { updated_since: since, cursor, limit: o.limit ?? 100 },
            })
          ).body
          yield* page.data
          cursor = page.next_cursor
        } while (cursor)
      },
      /** Sets the title; `null` clears it. */
      update: async (assetId: string, body: api.UpdateAsset) =>
        (await h.request<api.Asset>({ method: 'PATCH', path: `/v1/assets/${id(assetId)}`, body })).body,
      /** Playback stops at once; files go with the delete job. */
      delete: async (assetId: string) =>
        (await h.request<api.Asset>({ method: 'DELETE', path: `/v1/assets/${id(assetId)}` })).body,
      reencode: async (assetId: string, body: api.Reencode = {}) =>
        (await h.request<api.Job>({ method: 'POST', path: `/v1/assets/${id(assetId)}/reencode`, body })).body,
      /** Imports a public http(s) URL. */
      importUrl: async (
        body: Extract<api.CreateAsset, { url: string }>,
        o: { idempotencyKey?: string } = {},
      ) => (await h.request<api.Asset>({ method: 'POST', path: '/v1/assets', body, ...o })).body,
      /**
       * Resolves once the asset is ready, from the events long-poll, and
       * rejects with code `asset_errored` if it fails. For scripts, tests
       * and agents; an app should use the webhook.
       */
      waitUntilReady: async (assetId: string, o: { timeoutMs?: number; signal?: AbortSignal } = {}) => {
        const deadline = Date.now() + (o.timeoutMs ?? 30 * 60_000)
        const win = new EventWindow(new Date(Date.now() - 5000))
        for (;;) {
          const a = await this.assets.get(assetId)
          if (a.status === 'ready') return a
          if (a.status === 'errored' || a.status === 'deleted') {
            throw new ClipMuxError(409, 'asset_errored', `the asset is ${a.status}`, `wait ${assetId}`, {
              error_code: a.error_code ?? '',
            })
          }
          const wait = Math.min(25, Math.ceil((deadline - Date.now()) / 1000))
          if (wait <= 0)
            throw new ClipMuxError(0, 'timeout', 'the asset did not become ready in time', `wait ${assetId}`)
          const page = await this.events.poll({ since: win.since, after: win.after, wait, signal: o.signal })
          win.advance(page.data)
        }
      },
    }

    this.tracks = {
      /** Makes subtitles (whisper) or chapters (an LLM) for an asset. */
      create: async (assetId: string, body: api.CreateTrack) =>
        (await h.request<api.TrackJob>({ method: 'POST', path: `/v1/assets/${id(assetId)}/tracks`, body }))
          .body,
      get: async (assetId: string, trackId: string) =>
        (
          await h.request<api.TrackDetail>({
            method: 'GET',
            path: `/v1/assets/${id(assetId)}/tracks/${id(trackId)}`,
          })
        ).body,
      /** A ready subtitles track's transcript, with word timings if asked. */
      transcript: async (assetId: string, trackId: string, o: { words?: boolean } = {}) =>
        (
          await h.request<api.Transcript>({
            method: 'GET',
            path: `/v1/assets/${id(assetId)}/tracks/${id(trackId)}/transcript`,
            query: { words: o.words || undefined },
          })
        ).body,
      delete: async (assetId: string, trackId: string) =>
        (
          await h.request<api.Asset>({
            method: 'DELETE',
            path: `/v1/assets/${id(assetId)}/tracks/${id(trackId)}`,
          })
        ).body,
    }

    this.playbackIds = {
      create: async (assetId: string, body: api.CreatePlaybackID) =>
        (
          await h.request<api.PlaybackID>({
            method: 'POST',
            path: `/v1/assets/${id(assetId)}/playback-ids`,
            body,
          })
        ).body,
      delete: async (assetId: string, playbackId: string) =>
        (
          await h.request<api.PlaybackID>({
            method: 'DELETE',
            path: `/v1/assets/${id(assetId)}/playback-ids/${id(playbackId)}`,
          })
        ).body,
    }

    this.shares = {
      create: async (assetId: string, body: api.CreateShare = {}) =>
        (await h.request<api.Share>({ method: 'POST', path: `/v1/assets/${id(assetId)}/shares`, body })).body,
      list: async (assetId: string) =>
        (await h.request<api.ShareList>({ method: 'GET', path: `/v1/assets/${id(assetId)}/shares` })).body
          .data,
      delete: async (shareId: string) =>
        (await h.request<api.Share>({ method: 'DELETE', path: `/v1/shares/${id(shareId)}` })).body,
      streamUrl: async (shareId: string) =>
        (await h.request<api.StreamURL>({ method: 'POST', path: `/v1/shares/${id(shareId)}/stream-url` }))
          .body,
    }

    this.events = {
      /** Long-polls the app's events (what its webhooks receive). */
      poll: async (
        o: {
          since?: string | undefined
          after?: string | undefined
          wait?: number | undefined
          signal?: AbortSignal | undefined
        } = {},
      ) =>
        (
          await h.request<api.EventList>({
            method: 'GET',
            path: '/v1/events',
            query: { since: o.since, after: o.after, wait: o.wait },
            ...(o.signal ? { signal: o.signal } : {}),
            timeoutMs: ((o.wait ?? 25) + 15) * 1000,
          })
        ).body,
    }

    this.analytics = {
      /** Needs a key with the analytics:read scope. */
      get: async (range: '24h' | '7d' | '30d' | '90d') =>
        (await h.request<api.Analytics>({ method: 'GET', path: '/v1/analytics', query: { range } })).body,
      asset: async (assetId: string, range: '24h' | '7d' | '30d' | '90d') =>
        (
          await h.request<api.Analytics>({
            method: 'GET',
            path: `/v1/analytics/assets/${id(assetId)}`,
            query: { range },
          })
        ).body,
    }

    this.app = {
      /** The key's app: scopes, URLs, the longest token, signing keys. Cached. */
      get: (o: { fresh?: boolean } = {}) => {
        if (o.fresh || !this.#app) {
          this.#app = h.request<api.App>({ method: 'GET', path: '/v1/app' }).then((r) => r.body)
          this.#app.catch(() => {
            this.#app = undefined
          })
        }
        return this.#app
      },
    }

    this.playback = {
      /** Signs a playback token offline. */
      token: (playbackId: string, o: TokenOptions = {}) => this.#token(playbackId, o),
      /** Playback URLs for an asset (its first active ID) or a known base URL and ID. */
      urls: (
        target: api.Asset | { baseUrl: string; playbackId: string; policy?: 'signed' | 'public' },
        token = '',
      ) => urls(target, token),
      /**
       * Everything `<clipmux-player session-url>` needs, as the answer to
       * its POST. Pass the asset to include its tracks and duration.
       */
      session: (target: Target, o: SessionOptions = {}) => this.#session(target, o),
    }

    this.webhooks = {
      /**
       * Verifies a webhook request against CLIPMUX_WEBHOOK_SECRET and
       * returns its event. Pass the raw body as it arrived.
       */
      verify: async (rawBody: RawBody, headers: HeadersLike, o: VerifyOptions = {}) =>
        verifyWebhook(this.#require('webhookSecret'), rawBody, headers, o),
    }
  }

  #setting(name: keyof typeof ENV): string | undefined {
    const v = this.#o[name] ?? this.#env[ENV[name]]
    return v === undefined || v === '' ? undefined : v
  }

  #require(name: keyof typeof ENV): string {
    const v = this.#setting(name)
    if (v === undefined) throw new ClipMuxConfigError(ENV[name], `${ENV[name]} is not set.`)
    return v
  }

  // The server's longest token, read from /v1/app once. Signing stays
  // offline in spirit: if the API is slow or down, it waits at most 3 s and
  // uses the default maximum (delivery has the last word either way).
  async #maxTtl(): Promise<number> {
    let timer: Parameters<typeof clearTimeout>[0]
    const late = new Promise<undefined>((resolve) => {
      timer = setTimeout(resolve, 3000)
    })
    try {
      const app = await Promise.race([this.app.get().catch(() => undefined), late])
      return app?.max_token_lifetime_seconds ?? FALLBACK_MAX_TTL
    } finally {
      clearTimeout(timer)
    }
  }

  async #token(playbackId: string, o: TokenOptions): Promise<string> {
    const max = await this.#maxTtl()
    // Without a duration, 12 hours covers any lesson-length video.
    const d = o.durationSeconds
    const ttl = o.native
      ? Math.min(max, Math.max(4 * 3600, d ? Math.ceil(2 * d) : 12 * 3600))
      : seconds(o.expiresIn ?? DEFAULT_TTL)
    if (ttl > max) throw new RangeError(`expiresIn ${ttl} s is longer than the server allows (${max} s)`)
    const iat = Math.floor(Date.now() / 1000)
    return this.#sign({
      sub: playbackId,
      aud: o.audience === 'poster' ? 't' : 'v',
      iat,
      exp: iat + ttl,
      vid: o.viewerId,
    })
  }

  async #sign(claims: TokenClaims): Promise<string> {
    const kid = this.#require('signingKid')
    this.#key ??= importSigningKey(this.#require('signingKey'))
    this.#key.catch(() => {
      this.#key = undefined
    })
    return signToken(await this.#key, kid, claims)
  }

  async #session(target: Target, o: SessionOptions): Promise<PlaybackSession> {
    const asset = typeof target === 'string' ? null : target
    const pick = asset ? choose(asset) : { id: target as string, policy: 'signed' as const }
    if (!pick)
      throw new ClipMuxError(409, 'no_playback_id', 'the asset has no active playback ID', 'playback.session')
    const base = (asset?.base_url ?? (await this.app.get()).base_url).replace(/\/+$/, '')
    const duration = asset?.duration_seconds ?? null
    let token = ''
    let posterToken = ''
    let expiresAt: string | null = null
    if (pick.policy === 'signed') {
      token = await this.#token(pick.id, {
        expiresIn: o.expiresIn,
        viewerId: o.viewerId,
        native: o.native,
        durationSeconds: duration,
      })
      const c = claimsOf(token)
      expiresAt = new Date(c.exp * 1000).toISOString()
      // The poster gets a token that fetches only the poster, so a poster
      // URL that leaks (an og:image, a log) can't play the video.
      posterToken = await this.#sign({ sub: pick.id, aud: 't', iat: c.iat, exp: c.exp })
    }
    const s: PlaybackSession = {
      playback_id: pick.id,
      base_url: base,
      token,
      expires_at: expiresAt,
      poster_url: posterToken
        ? `${base}/v/${posterToken}/${pick.id}/poster.jpg`
        : `${base}/p/${pick.id}/poster.jpg`,
      duration_seconds: duration,
      tracks: (asset?.tracks ?? []).filter((t) => t.status === 'ready'),
    }
    if (asset?.title) s.title = asset.title
    return s
  }
}

// claimsOf reads the claims back from a token this client just signed.
function claimsOf(token: string): TokenClaims {
  const body = token.split('.')[1] ?? ''
  return JSON.parse(atob(body.replace(/-/g, '+').replace(/_/g, '/'))) as TokenClaims
}

// choose is the playback ID a session plays: an active signed one of the
// asset's own (not a share link's), else an active public one.
function choose(a: api.Asset): { id: string; policy: 'signed' | 'public' } | null {
  const own = a.playback_ids.filter((p) => p.status === 'active' && !p.share)
  const p = own.find((x) => x.policy === 'signed') ?? own.find((x) => x.policy === 'public')
  return p ? { id: p.id, policy: p.policy } : null
}

function urls(
  target: api.Asset | { baseUrl: string; playbackId: string; policy?: 'signed' | 'public' },
  token: string,
): { hls: string; poster: string; track: (trackId: string) => string } {
  let base: string
  let pid: string
  let policy: 'signed' | 'public'
  if ('baseUrl' in target) {
    base = target.baseUrl
    pid = target.playbackId
    policy = target.policy ?? (token ? 'signed' : 'public')
  } else {
    const p = choose(target)
    if (!p)
      throw new ClipMuxError(409, 'no_playback_id', 'the asset has no active playback ID', 'playback.urls')
    base = target.base_url
    pid = p.id
    policy = p.policy
  }
  base = base.replace(/\/+$/, '')
  if (policy === 'signed' && !token) throw new TypeError('a signed playback ID needs a token')
  const root = policy === 'signed' ? `${base}/v/${token}/${pid}` : `${base}/p/${pid}`
  return {
    hls: `${root}/master.m3u8`,
    poster: `${root}/poster.jpg`,
    track: (trackId: string) => `${root}/tracks/${trackId}.vtt`,
  }
}
