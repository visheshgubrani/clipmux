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

export async function api<T>(path: string, init: RequestInit & { app?: string } = {}): Promise<T> {
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  if (init.app) headers['ClipMux-App'] = init.app
  const res = await fetch(path, { ...init, headers, credentials: 'same-origin' })
  if (res.status === 204) return undefined as T
  const body = await res.json().catch(() => ({}))
  if (!res.ok) {
    const e = (body as { error?: { code?: string; message?: string } }).error
    throw new ApiError(res.status, e?.code ?? `http_${res.status}`, e?.message ?? `HTTP ${res.status}`)
  }
  return body as T
}

export const post = <T>(path: string, body?: unknown, app?: string) =>
  api<T>(path, { method: 'POST', body: JSON.stringify(body ?? {}), ...(app ? { app } : {}) })

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
  policy: string
  playable: boolean
  status: string
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
export interface Asset {
  id: string
  app_id: string
  status: string
  version: number
  passthrough?: string
  error_code?: string
  base_url: string
  duration_seconds: number | null
  playback_ids: PlaybackID[]
  tracks: Track[]
  created_at: string
  updated_at: string
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
