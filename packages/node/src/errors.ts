// Errors the SDK throws. Each carries a stable code to branch on and a
// message that says what to do next, because the reader is as often a
// coding agent as a person.

const HINTS: Record<string, string> = {
  unauthorized: 'Check CLIPMUX_API_KEY (a cmx_… key of this app); run `npx @clipmux/node doctor`.',
  insufficient_scope: 'The API key lacks the scope this call needs; create one with the default scopes.',
  rate_limited: 'Too many requests for this key; the SDK already waited and retried.',
  idempotency_key_reused: 'The same Idempotency-Key was sent with a different request; use a new key.',
  asset_not_ready: 'Wait for video.asset.ready (or assets.waitUntilReady) before this call.',
  asset_deleted: 'The asset is deleted; nothing more can change on it.',
  job_active: 'A job is already running on this asset; pass supersede: true to replace it, or wait.',
  not_found: 'No such resource for this app: check the ID, and that the key belongs to the app that owns it.',
  too_large: 'The upload is larger than this server accepts.',
  public_url_required:
    'The server still has a loopback public URL; set it in Settings, or confirm the key is local.',
  import_url_refused: 'Only public http(s) URLs without credentials can be imported.',
  chapters_not_connected: 'Chapters need an LLM connected on the server (Captions settings).',
}

/** The API answered with an error, or could not be reached (status 0). */
export class ClipMuxError extends Error {
  /** HTTP status, or 0 when the request never got an answer. */
  readonly status: number
  /** The API's error code, e.g. `not_found`, `asset_not_ready`, `network`. */
  readonly code: string
  /** The error's other fields, e.g. `retry_after` or `job_id`. */
  readonly details: Record<string, string>
  /** The request, as `METHOD /path`, for logs. */
  readonly request: string

  constructor(
    status: number,
    code: string,
    message: string,
    request: string,
    details: Record<string, string> = {},
  ) {
    const hint = HINTS[code]
    super(`${request}: ${code}: ${message}${hint ? ` ${hint}` : ''}`)
    this.name = 'ClipMuxError'
    this.status = status
    this.code = code
    this.details = details
    this.request = request
  }
}

/** A setting the call needs is missing or malformed. */
export class ClipMuxConfigError extends Error {
  /** The environment variable (or option) at fault. */
  readonly variable: string
  constructor(variable: string, message: string) {
    super(`${message} Set ${variable}, or run \`npx @clipmux/node init\` to write it.`)
    this.name = 'ClipMuxConfigError'
    this.variable = variable
  }
}

/** Why a webhook was refused. */
export type WebhookFailure = 'missing_header' | 'stale' | 'bad_signature' | 'invalid_body'

/** A webhook request that must not be trusted. Answer it with 400. */
export class WebhookVerificationError extends Error {
  readonly reason: WebhookFailure
  constructor(reason: WebhookFailure, message: string) {
    super(`webhook ${reason}: ${message}`)
    this.name = 'WebhookVerificationError'
    this.reason = reason
  }
}
