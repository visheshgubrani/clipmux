// Token refresh for ClipMux playback, independent of any player UI.
//
// A signed playback URL carries its token in the path:
//   {base_url}/v/{token}/{playback_id}/…
// and every playlist, segment and key URI below it is relative, so the token
// rides along. When it expires, delivery answers 401 token_expired; the
// loader here asks the app for a new token once, swaps it into the path of
// that request and every later one, and retries, so the player never sees
// the error and keeps its position.

/** A ClipMux playback token: an ES256 JWT. */
const jwt = /^[\w-]+\.[\w-]+\.[\w-]+$/

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

/**
 * Puts token into a signed playback URL of playbackId on mediaOrigin. Any
 * other URL (another origin, another playback ID, a public /p/ URL) is
 * returned unchanged.
 */
export function rewriteTokenUrl(url: string, mediaOrigin: string, playbackId: string, token: string): string {
  let u: URL
  try {
    u = new URL(url)
  } catch {
    return url
  }
  if (u.origin !== new URL(mediaOrigin).origin) return url
  const path = u.pathname.replace(new RegExp(`^/v/[^/]+/(?=${escapeRegExp(playbackId)}/)`), `/v/${token}/`)
  if (path === u.pathname) return url
  u.pathname = path
  return u.toString()
}

/** Your callback for a fresh token; null means this viewer may not watch. */
export type GetToken = () => Promise<string | null>

export interface SessionOptions {
  delay?: (ms: number) => Promise<void>
  onToken?: (token: string) => void
  onDenied?: () => void
}

/**
 * The current token of one playback. refresh() calls getToken at most once
 * however many requests failed together, and a request that failed with an
 * older token simply retries with the newer one.
 */
export class TokenSession {
  #current: string
  #getToken: GetToken | undefined
  #pending: Promise<string | null> | null = null
  #denied = false
  readonly #opts: SessionOptions

  constructor(token: string, getToken?: GetToken, options: SessionOptions = {}) {
    this.#current = token
    this.#getToken = getToken
    this.#opts = options
  }

  get current(): string {
    return this.#current
  }

  get denied(): boolean {
    return this.#denied
  }

  /** Stops refreshing: delivery said 403, or the app said no. */
  deny(): void {
    if (this.#denied) return
    this.#denied = true
    this.#opts.onDenied?.()
  }

  /**
   * A new token to retry with after one that was rejected (seen), or null
   * when there is none: the app denied it, or there is no getToken.
   */
  refresh(seen: string): Promise<string | null> {
    if (this.#denied || !this.#getToken) return Promise.resolve(null)
    if (seen !== this.#current) return Promise.resolve(this.#current)
    this.#pending ??= this.#fetch().finally(() => {
      this.#pending = null
    })
    return this.#pending
  }

  async #fetch(): Promise<string | null> {
    const delay = this.#opts.delay ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)))
    const getToken = this.#getToken as GetToken
    let failure: unknown
    for (const wait of [0, 1000, 2000, 4000]) {
      if (wait) await delay(wait)
      let token: string | null
      try {
        token = await getToken()
      } catch (err) {
        failure = err // a transient failure: try again
        continue
      }
      if (token === null) {
        this.deny()
        return null
      }
      if (!jwt.test(token)) throw new Error('getToken must return a ClipMux playback token (a JWT) or null')
      this.#current = token
      this.#opts.onToken?.(token)
      return token
    }
    throw failure
  }
}

// The slice of hls.js's loader interface the wrapper needs, so this module
// type-checks without hls.js and tests can use a fake.
export interface LoaderContextLike {
  url: string
}
export interface LoaderError {
  code: number
  text: string
}
export interface LoaderCallbacksLike<C extends LoaderContextLike> {
  onSuccess: (response: unknown, stats: unknown, context: C, networkDetails: unknown) => void
  onError: (error: LoaderError, context: C, networkDetails: unknown, stats: unknown) => void
  onTimeout: (stats: unknown, context: C, networkDetails: unknown) => void
  onAbort?: (stats: unknown, context: C, networkDetails: unknown) => void
  onProgress?: (stats: unknown, context: C, data: unknown, networkDetails: unknown) => void
}
export interface LoaderLike<C extends LoaderContextLike> {
  context: C | null
  stats: unknown
  load(context: C, config: unknown, callbacks: LoaderCallbacksLike<C>): void
  abort(): void
  destroy(): void
  getCacheAge?: (() => number | null) | undefined
  getResponseHeader?: ((name: string) => string | null) | undefined
}
export type LoaderClass<C extends LoaderContextLike> = new (config: unknown) => LoaderLike<C>

/**
 * Wraps a loader class (hls.js's DefaultConfig.loader) so every request
 * carries the session's current token, and a 401 is answered with a fresh
 * token and a retry instead of an error. hls.js loads playlists, segments
 * and AES-128 keys through the same loader, so this covers them all.
 */
export function createRefreshLoader<C extends LoaderContextLike>(
  Base: LoaderClass<C>,
  session: TokenSession,
  mediaOrigin: string,
  playbackId: string,
): LoaderClass<C> {
  return class RefreshLoader implements LoaderLike<C> {
    readonly #config: unknown
    #inner: LoaderLike<C>
    #aborted = false

    constructor(config: unknown) {
      this.#config = config
      this.#inner = new Base(config)
    }

    get context(): C | null {
      return this.#inner.context
    }

    set context(c: C | null) {
      this.#inner.context = c
    }

    get stats(): unknown {
      return this.#inner.stats
    }

    get getCacheAge() {
      return this.#inner.getCacheAge?.bind(this.#inner)
    }

    get getResponseHeader() {
      return this.#inner.getResponseHeader?.bind(this.#inner)
    }

    load(context: C, config: unknown, callbacks: LoaderCallbacksLike<C>): void {
      this.#aborted = false
      if (session.denied) {
        callbacks.onError({ code: 403, text: 'denied' }, context, null, this.stats)
        return
      }
      const send = (token: string, retried: boolean) => {
        context.url = rewriteTokenUrl(context.url, mediaOrigin, playbackId, token)
        this.#inner.load(context, config, {
          ...callbacks,
          onError: (error, ctx, details, stats) => {
            if (error.code === 401 && !retried && !this.#aborted) {
              session.refresh(token).then(
                (next) => {
                  if (this.#aborted) return
                  if (!next) return callbacks.onError(error, ctx, details, stats)
                  // A fresh inner loader: the failed one is spent.
                  this.#inner.destroy()
                  this.#inner = new Base(this.#config)
                  send(next, true)
                },
                () => {
                  if (!this.#aborted) callbacks.onError(error, ctx, details, stats)
                },
              )
              return
            }
            if (error.code === 403) session.deny()
            callbacks.onError(error, ctx, details, stats)
          },
        })
      }
      send(session.current, false)
    }

    abort(): void {
      this.#aborted = true
      this.#inner.abort()
    }

    destroy(): void {
      this.#aborted = true
      this.#inner.destroy()
    }
  }
}
