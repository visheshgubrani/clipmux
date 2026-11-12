// The dashboard calls ClipMux's public API with the owner's session cookie
// (docs/api.md); it has no private path into the server.

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  constructor(status: number, code: string, message: string) {
    super(message)
    this.status = status
    this.code = code
  }
}

/** Called when a request finds the session gone; the app sends the owner to sign in. */
let onSignedOut: (() => void) | undefined
export function whenSignedOut(fn: () => void) {
  onSignedOut = fn
}

export async function api<T>(
  path: string,
  init: RequestInit & { app?: string | undefined } = {},
): Promise<T> {
  const { app, headers: extra, ...rest } = init
  const headers = new Headers(extra)
  if (!headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  if (app) headers.set('ClipMux-App', app)
  const res = await fetch(path, { ...rest, headers, credentials: 'same-origin' })
  if (res.status === 204) return undefined as T
  const body = await res.json().catch(() => ({}))
  if (!res.ok) {
    const e = (body as { error?: { code?: string; message?: string } }).error
    const err = new ApiError(res.status, e?.code ?? `http_${res.status}`, e?.message ?? `HTTP ${res.status}`)
    // A management route answering 401 means the session ended (expired,
    // signed out elsewhere, password changed). Sign-in itself handles its own.
    if (res.status === 401 && err.code === 'unauthorized' && path !== '/v1/session') onSignedOut?.()
    throw err
  }
  return body as T
}

export const post = <T>(path: string, body?: unknown, app?: string) =>
  api<T>(path, { method: 'POST', body: JSON.stringify(body ?? {}), ...(app ? { app } : {}) })

export const put = <T>(path: string, body?: unknown, app?: string) =>
  api<T>(path, { method: 'PUT', body: JSON.stringify(body ?? {}), ...(app ? { app } : {}) })

export const patch = <T>(path: string, body?: unknown, app?: string) =>
  api<T>(path, { method: 'PATCH', body: JSON.stringify(body ?? {}), ...(app ? { app } : {}) })

export const del = <T>(path: string, app?: string) =>
  api<T>(path, { method: 'DELETE', ...(app ? { app } : {}) })

/** A message for the person: the API's own words, or what went wrong. */
export function describe(err: unknown): string {
  if (err instanceof ApiError) return err.message
  if (err instanceof Error) return err.message
  return String(err)
}

export interface Session {
  email: string
  totp_enabled: boolean
}
export interface URLs {
  public_url: string
  media_url: string
  confirmed: boolean
  pinned: boolean
}
export interface Setup {
  owner_exists: boolean
  urls: URLs
  keys_allowed: boolean
  setup_test: { id: string; status: string } | null
}
export interface Check {
  name: string
  ok: boolean
  detail?: string
}
export interface SetupTest {
  id: string
  status: string
  trigger: string
  asset_id?: string
  checks: Check[]
}
export interface PlaybackID {
  id: string
  policy: 'signed' | 'public'
  playable: boolean
  status: string
  /** The share link this ID was made for (D104); absent for the asset's own IDs. */
  share?: { id: string; name: string }
}
/** A share link (docs/api.md, "Share links"): a page that plays one video without a token. */
export interface ShareLink {
  id: string
  asset_id: string
  name: string
  url: string
  playback_id: string
  status: 'active' | 'expired' | 'revoked'
  expires_at: string | null
  created_at: string
  views_30d?: number
}
export interface Track {
  id: string
  kind: 'subtitles' | 'chapters'
  source: string
  status: 'preparing' | 'ready' | 'errored'
  language?: string
  name?: string
  error_code?: string
}
export type AssetStatus = 'waiting_upload' | 'processing' | 'ready' | 'errored' | 'deleted'
export interface Asset {
  id: string
  app_id: string
  status: AssetStatus
  version: number
  title?: string
  passthrough?: string
  error_code?: string
  base_url: string
  duration_seconds: number | null
  playback_ids: PlaybackID[]
  tracks: Track[]
  created_at: string
  updated_at: string
}
/** A library row: the asset view plus a poster any viewer of the page can load. */
export interface LibraryAsset extends Asset {
  poster_url: string | null
}
export interface Page<T> {
  data: T[]
  next_cursor?: string
}
export interface Counts {
  ready: number
  processing: number
  errored: number
  waiting_upload: number
}
export interface Step {
  name: string
  ok: boolean
  detail?: string
}
export interface App {
  id: string
  name: string
  builtin: boolean
  disabled: boolean
  created_at: string
}
export interface APIKey {
  id: string
  name: string
  scopes: string[]
  created_at: string
  last_used_at?: string
  revoked_at?: string
}
export interface SigningKey {
  kid: string
  created_at: string
  retired_at?: string
  revoked_at?: string
}
export interface WebhookEndpoint {
  id: string
  url: string
  created_at: string
  disabled_at?: string
  consecutive_failures: number
  skipped_until?: string
  pending_secret_since?: string
}
export interface WebhookDelivery {
  id: string
  event_id: string
  event_type: string
  status: string
  attempts: number
  last_status?: number
  last_error?: string
  created_at: string
  finished_at?: string
  next_attempt_at?: string
}
export interface Attempt {
  id: string
  number: number
  executor: string
  route_reason?: string
  agent_name?: string
  status: string
  error_code?: string
  phase?: string
  progress?: number
  encoder?: { stage: string; fallbacks?: string[] }
}
export interface Job {
  id: string
  asset_id: string
  app_id?: string
  kind: string
  status: 'queued' | 'running' | 'succeeded' | 'failed' | 'cancelled'
  phase?: string
  progress?: number
  pinned_agent_id?: string
  lost_count: number
  failed_count: number
  error_code?: string
  routed_to?: string
  route_reason?: string
  created_at: string
  attempts: Attempt[]
}
export interface Agent {
  id: string
  name: string
  status: string
  scope: string
  online: boolean
  sandboxed: boolean
  hostname: string
  platform: string
  last_ip?: string
  last_seen_at: string | null
  capabilities: {
    ffmpeg_version?: string
    packager_version?: string
    encoders?: string[]
    speech?: { backends?: string[]; model?: string }
  }
}
export interface Invitation {
  code: string
  expires_at: string
  command: string
  docker_command: string
  install_command: string
}
export interface Pairing {
  status: 'pending' | 'approved' | 'denied' | 'consumed' | 'expired'
  agent_started: boolean
  name: string
  hostname: string
  platform: string
  ip?: string
  agent_id?: string
  expires_at: string
}
/** An app pairing started by `npx @clipmux/node init`, as /connect shows it. */
export interface AppPairing {
  status: 'pending' | 'approved' | 'denied' | 'consumed' | 'expired'
  name: string
  hostname: string
  client: string
  ip?: string
  app_id?: string
  expires_at: string
}
/** An app's credentials, shown once (POST /v1/apps/{id}/credentials). */
export interface Credentials {
  url: string
  app_id: string
  app_name: string
  api_key_id: string
  api_key: string
  signing_kid: string
  signing_key: string
  webhook_endpoint_id?: string
  webhook_secret?: string
  env: string
}
export interface Routing {
  primary_executor: string
  fallback_executor: string | null
  queue_age_fallback_minutes: number | null
  builtin_worker: { last_seen_at?: string; online: boolean }
  providers_ready: string[]
}
export interface Provider {
  kind: string
  connected: boolean
  verified: boolean
  config: { image?: string; gpu?: string; gpu_type_ids?: string[] }
  max_concurrent: number
  monthly_limit_cents: number
  rate_cents_per_hour: number
  month_cents: number
  machines: number
  backoff_until?: string
}
export interface SpeechStatus {
  mode: 'local' | 'api' | 'local_then_api'
  connection: { base_url: string; model: string; has_key: boolean; verified_at: string } | null
}
export interface ChaptersStatus {
  connection: {
    provider: 'openai_compatible' | 'anthropic'
    base_url?: string
    model: string
    response_format?: string
    has_key: boolean
    verified_at: string
  } | null
}
export interface Cloudflare {
  connected: boolean
  hostname?: string
  bucket?: string
  account_id?: string
  worker_version?: string
  current_version: string
  needs_redeploy: boolean
  storage_ok: boolean
  /** Create-token links that pre-fill Cloudflare's form (D109). */
  token_url: string
  r2_token_url: string
  analytics_token_url: string
}
/** What a pasted Cloudflare token reaches (POST /v1/cloudflare/inspect, D109). */
export interface CloudflareInspection {
  accounts: { id: string; name: string }[]
  account_id?: string
  zones?: string[]
  buckets?: string[]
  workers_paid: boolean
  unreadable?: string[]
}
/** A step-by-step result: Connect Cloudflare, Add R2 storage, connect a provider. */
export interface StepResult {
  connected: boolean
  steps: Step[]
}
/** A GPU a provider rents, with its price (GET /v1/executors/modal/gpus, POST …/runpod/gpus; D111). */
export interface GPU {
  id: string
  name: string
  memory_gb: number
  cents_per_hour: number
  recommended?: boolean
}
/** A provider's browser approval (POST /v1/executors/{kind}/authorize, D109). */
export interface Approval {
  id: string
  url?: string
  code?: string
  expires_at: string
  /** `shown`: the provider showed the key on its own page; paste it (RunPod does). */
  status: 'pending' | 'approved' | 'denied' | 'expired' | 'shown'
  result?: StepResult
}
/** Edge analytics (P7-T05): the read-only token serve pulls the Worker's counts with. */
export interface EdgeAnalytics {
  connected: boolean
  account_id?: string
  pulled_through?: string
  last_pull_at?: string
  last_error?: string
}
export interface Moves {
  default_kind: string
  not_on_default: number
  active: number
  moved: number
  failed: number
}
export interface StorageConfig {
  id: string
  kind: 'fs' | 'r2' | 's3'
  bucket?: string
  endpoint?: string
  public_endpoint?: string
  region?: string
  default: boolean
  asset_count: number
  cloudflare_delivery: boolean
  created_at: string
}
export interface Transcript {
  track_id: string
  language: string
  duration_ms: number
  text: string
  segments: { id: string; start_ms: number; end_ms: number; text: string }[]
}
export interface Chapter {
  start_ms: number
  end_ms: number
  title: string
}
export interface TrackDetail extends Track {
  chapters?: Chapter[]
}

/** The analytics contract (docs/api.md, "Analytics"; D100, D103). */
export type Range = '24h' | '7d' | '30d' | '90d'
export interface AnalyticsTotals {
  views: number
  watch_seconds: number
  unique_viewers: number | null
  bytes_delivered: number
}
export interface Analytics {
  /** What is being counted: this server always; the edge once its counts are pulled in. */
  collecting?: { builtin: boolean; edge: 'connected' | 'not_connected' | 'not_used' }
  totals: AnalyticsTotals
  previous: AnalyticsTotals
  series: { t: string; views: number; watch_seconds: number; bytes_delivered: number }[]
  top_assets?: {
    asset_id: string
    title?: string
    views: number
    watch_seconds: number
    bytes_delivered: number
  }[]
}
