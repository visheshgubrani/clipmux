// clipmuxHandler: the three routes an app needs, as one Fetch handler
// (Request → Response), so it drops into Next.js route handlers, Hono, Bun,
// Deno, Cloudflare Workers, SvelteKit and Remix as is, and into Express,
// Fastify or Koa through @clipmux/node/express.
//
//   POST …/upload    <clipmux-uploader create-url>  → upload(req, {file})
//   POST …/playback  <clipmux-player session-url>   → playback(req, {playbackId})
//   POST …/webhook   ClipMux's webhook              → asset(asset, event)
//
// Every route is default-deny: a callback that isn't given is a 404, and a
// callback that returns null is a 403. The browser routes take only JSON,
// which a cross-site form can't send without a CORS preflight.

import { ClipMux } from './client.ts'
import { ClipMuxError, WebhookVerificationError } from './errors.ts'
import type { Asset, CreateUpload } from './schema.gen.ts'
import type { WebhookEvent } from './webhooks.ts'

/** The file the browser is about to upload, as it describes it. */
export interface UploadFile {
  size: number
  name: string
  type: string
}

/** How to create the upload: everything but its size. */
export type UploadDecision = Omit<CreateUpload, 'size'>

export interface PlaybackDecision {
  /** A stable ID for this viewer, for unique-viewer analytics. */
  viewerId?: string | undefined
  /** The asset, to include its tracks and duration (and a longer native token). */
  asset?: Asset | undefined
}

export interface HandlerOptions {
  /** Default: `new ClipMux()`, configured from the environment. */
  clipmux?: ClipMux
  /**
   * Decide whether this request may upload, and how: return the upload's
   * passthrough, title, policy and generate, or null to refuse. The body is
   * already read: decide from your session, the URL and `file`.
   */
  upload?: (req: Request, ctx: { file: UploadFile }) => Promise<UploadDecision | null> | UploadDecision | null
  /**
   * Decide whether this viewer may watch this playback ID: return
   * `{viewerId}` (and the asset, if you have it), or null to refuse.
   */
  playback?: (
    req: Request,
    ctx: { playbackId: string; native: boolean },
  ) => Promise<PlaybackDecision | null> | PlaybackDecision | null
  /**
   * A verified webhook, with the asset re-fetched so it is current. Store
   * what you need by `asset.passthrough`. Throw to have ClipMux retry the
   * delivery (it does for 24 hours); deliveries are at least once.
   */
  asset?: (asset: Asset, event: WebhookEvent) => Promise<void> | void
  /** Every verified webhook event as it arrived, before `asset`. */
  event?: (event: WebhookEvent) => Promise<void> | void
  /**
   * Called with errors the handler turned into a 4xx or 5xx; default
   * console.error for 5xx. ClipMux's 400, 409, 413 and 422 pass through
   * with their code, 429 with Retry-After; a wrong key or app is 500
   * `clipmux_misconfigured`; ClipMux unreachable or failing is 502
   * `clipmux_unavailable`.
   */
  onError?: (err: unknown, req: Request) => void
}

const ACTIONS = new Set(['upload', 'playback', 'webhook'])

function reply(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' },
  })
}
const fail = (status: number, code: string, message: string) => reply(status, { error: { code, message } })

async function readJSON(req: Request): Promise<Record<string, unknown> | Response> {
  const type = req.headers.get('Content-Type') ?? ''
  if (!/^application\/json\b/i.test(type)) {
    return fail(415, 'unsupported_media_type', 'send Content-Type: application/json')
  }
  try {
    const v: unknown = await req.json()
    if (typeof v !== 'object' || v === null || Array.isArray(v))
      return fail(400, 'invalid_request', 'body must be a JSON object')
    return v as Record<string, unknown>
  } catch {
    return fail(400, 'invalid_request', 'body is not JSON')
  }
}

// The caller's own mistakes ClipMux reports, passed on as they are: the
// browser must not retry them.
const PASS_THROUGH = new Set([400, 409, 413, 422])

// failure answers an error a callback or ClipMux raised. The browser
// elements retry 5xx and 429, so only what can pass later is retryable:
// ClipMux unreachable or failing is 502; this server's own key or app being
// wrong is 500 (retrying won't fix it, and it is logged); ClipMux refusing
// the request itself keeps its status and code.
function failure(err: unknown): Response {
  if (!(err instanceof ClipMuxError)) return fail(500, 'internal', 'internal error')
  const message = err.message.startsWith(`${err.request}: `)
    ? err.message.slice(err.request.length + 2)
    : err.message
  if (PASS_THROUGH.has(err.status)) return fail(err.status, err.code, message)
  if (err.status === 429) {
    const res = fail(429, err.code, message)
    const after = err.details.retry_after
    if (after && /^\d+$/.test(after)) res.headers.set('Retry-After', after)
    return res
  }
  if (err.status === 0 || err.status === 408 || err.status >= 500)
    return fail(502, 'clipmux_unavailable', `ClipMux could not answer: ${err.code}`)
  return fail(500, 'clipmux_misconfigured', `ClipMux refused this server's request: ${err.code}`)
}

/** Returns the Fetch handler for upload, playback and webhook routes. */
export function clipmuxHandler(options: HandlerOptions): (req: Request) => Promise<Response> {
  let cm = options.clipmux
  const client = () => {
    cm ??= new ClipMux()
    return cm
  }
  // The highest version handed to options.asset per asset, so a slow
  // delivery that re-fetched earlier can't overwrite a newer one.
  const seen = new Map<string, number>()
  const report = (err: unknown, req: Request, status: number) => {
    if (options.onError) options.onError(err, req)
    else if (status >= 500) console.error('clipmux handler:', err)
  }

  return async (req: Request): Promise<Response> => {
    const action = new URL(req.url).pathname.replace(/\/+$/, '').split('/').pop() ?? ''
    if (!ACTIONS.has(action))
      return fail(404, 'not_found', `no ClipMux route ${action}; use upload, playback or webhook`)
    if (req.method !== 'POST') return fail(405, 'method_not_allowed', 'POST only')
    try {
      if (action === 'upload') return await upload(req)
      if (action === 'playback') return await playback(req)
      return await webhook(req)
    } catch (err) {
      const res = failure(err)
      report(err, req, res.status)
      return res
    }
  }

  async function upload(req: Request): Promise<Response> {
    if (!options.upload)
      return fail(404, 'not_found', 'uploads are not enabled: pass upload() to clipmuxHandler')
    const body = await readJSON(req)
    if (body instanceof Response) return body
    const size = body.size
    if (typeof size !== 'number' || !Number.isSafeInteger(size) || size <= 0) {
      return fail(400, 'invalid_request', 'size must be a positive integer')
    }
    const file: UploadFile = {
      size,
      name: typeof body.name === 'string' ? body.name : '',
      type: typeof body.type === 'string' ? body.type : '',
    }
    const decision = await options.upload(req, { file })
    if (!decision) return fail(403, 'denied', 'this upload is not allowed')
    const created = await client().uploads.create({ playback_policy: 'signed', ...decision, size })
    return reply(201, created)
  }

  async function playback(req: Request): Promise<Response> {
    if (!options.playback)
      return fail(404, 'not_found', 'playback is not enabled: pass playback() to clipmuxHandler')
    const body = await readJSON(req)
    if (body instanceof Response) return body
    const playbackId = body.playback_id
    if (typeof playbackId !== 'string' || !/^[A-Za-z0-9_-]{1,64}$/.test(playbackId)) {
      return fail(400, 'invalid_request', 'playback_id is required')
    }
    const native = body.native === true
    const decision = await options.playback(req, { playbackId, native })
    if (!decision) return fail(403, 'denied', 'this viewer may not watch this video')
    let target: Asset | string = playbackId
    if (decision.asset) {
      // Sign exactly the ID that was asked for and allowed.
      const ids = decision.asset.playback_ids.filter((p) => p.id === playbackId && p.status === 'active')
      if (ids.length === 0)
        throw new Error(`playback(): the asset returned has no active playback ID ${playbackId}`)
      target = { ...decision.asset, playback_ids: ids.map(({ share: _s, ...p }) => p) }
    }
    const session = await client().playback.session(target, { viewerId: decision.viewerId, native })
    return reply(200, session)
  }

  async function webhook(req: Request): Promise<Response> {
    if (!options.asset && !options.event) {
      return fail(404, 'not_found', 'webhooks are not enabled: pass asset() or event() to clipmuxHandler')
    }
    let event: WebhookEvent
    try {
      event = await client().webhooks.verify(await req.arrayBuffer(), req.headers)
    } catch (err) {
      if (!(err instanceof WebhookVerificationError)) throw err
      report(err, req, 400)
      return fail(400, err.reason, err.message)
    }
    await options.event?.(event)
    if (options.asset && event.data?.id) {
      let asset: Asset
      try {
        asset = await client().assets.get(event.data.id)
      } catch (err) {
        if (!(err instanceof ClipMuxError && err.status === 404)) throw err
        asset = event.data // gone since; the event's copy is the last word
      }
      if ((seen.get(asset.id) ?? -1) <= asset.version) {
        seen.set(asset.id, asset.version)
        await options.asset(asset, event)
      }
    }
    return reply(200, { received: true })
  }
}
