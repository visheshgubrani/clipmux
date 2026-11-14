// Generated from internal/openapi/openapi.json by scripts/generate.ts. Do not edit.
// Regenerate with: pnpm --filter @clipmux/node run generate

/** Every error. Details sit beside code and message, all strings (e.g. retry_after, job_id). */
export type Error = {
  error: {
    code: string
    message: string
    [detail: string]: string | undefined
  }
}

export type PlaybackID = {
  id: string
  policy: 'signed' | 'public'
  playable: boolean
  status: 'active' | 'revoking' | 'revoked'
  share?: {
    id: string
    name: string
  }
}

export type Track = {
  id: string
  kind: 'subtitles' | 'chapters'
  source: string
  status: 'preparing' | 'ready' | 'errored'
  language?: string
  name?: string
  error_code?: string
}

export type Chapter = {
  start_ms: number
  end_ms: number
  title: string
}

export type TrackDetail = Track & {
  duration_ms?: number
  chapters?: Chapter[]
}

export type Word = {
  id: string
  start_ms: number
  end_ms: number
  text: string
}

export type Segment = {
  id: string
  start_ms: number
  end_ms: number
  text: string
  words?: Word[]
}

export type Transcript = {
  track_id: string
  language: string
  duration_ms: number
  text: string
  segments: Segment[]
}

export type Asset = {
  id: string
  app_id: string
  status: 'waiting_upload' | 'processing' | 'ready' | 'errored' | 'deleted'
  /** Rises on every change; keep the highest seen. */
  version: number
  passthrough?: string
  title?: string
  /** The renditions this asset's outputs are encoded at; absent means every rung up to the source. */
  renditions?: Renditions
  error_code?: string
  /** The media origin playback URLs start with. */
  base_url: string
  playback_ids: PlaybackID[]
  duration_seconds: number | null
  tracks: Track[]
  created_at: string
  updated_at: string
}

export type AssetList = {
  data: Asset[]
  next_cursor?: string
}

export type Generate = {
  subtitles?: boolean
  /** Implies subtitles. */
  chapters?: boolean
}

export type CreateUpload = {
  size: number
  playback_policy?: 'signed' | 'public'
  /** Your own reference; it comes back on the asset and its webhooks. */
  passthrough?: string
  title?: string
  generate?: Generate
  renditions?: Renditions
}

export type Upload = {
  upload_id: string
  asset_id: string
  part_size: number
  part_count: number
  /** Hand this to the browser; it reaches only this upload. */
  upload_token?: string
  playback_ids: PlaybackID[]
}

export type PartNumbers = {
  part_numbers: number[]
}

export type SignedParts = {
  parts: {
    part_number: number
    url: string
  }[]
  expires_at: string
}

export type StoredParts = {
  parts: {
    part_number: number
    etag: string
    size: number
  }[]
}

export type CompleteUpload = {
  parts: {
    part_number: number
    etag: string
  }[]
}

export type CreateAsset = {
  url: string
  playback_policy?: 'signed' | 'public'
  passthrough?: string
  title?: string
  generate?: Generate
  renditions?: Renditions
} | {
  agent_source_id: string
  playback_policy?: 'signed' | 'public'
  passthrough?: string
  title?: string
  generate?: Generate
  renditions?: Renditions
}

export type UpdateAsset = {
  title: string | null
}

export type Job = {
  id: string
  kind: string
  status: string
}

export type Reencode = {
  supersede?: boolean
  /** Replaces the asset's choice for this output and the ones after it; leave it out to keep it. */
  renditions?: Renditions
}

export type CreateTrack = {
  kind: 'subtitles' | 'chapters'
  supersede?: boolean
}

export type TrackJob = {
  track: Track
  job: Job
}

export type CreatePlaybackID = {
  policy: 'signed' | 'public'
}

export type Share = {
  id: string
  asset_id: string
  name: string
  url: string
  playback_id: string
  expires_at: string | null
  status: 'active' | 'revoked' | 'expired'
  created_at: string
  views_30d?: number
}

export type CreateShare = {
  name?: string
  expires_at?: string
}

export type ShareList = {
  data: Share[]
}

export type StreamURL = {
  url: string
  expires_at: string
}

export type Event = {
  id: string
  created_at: string
  body: WebhookEvent
}

export type EventList = {
  data: Event[]
}

export type WebhookEvent = {
  id: string
  type: 'video.asset.ready' | 'video.asset.updated' | 'video.asset.errored' | 'video.asset.reencode_failed' | 'video.asset.deleted' | 'video.asset.track.ready' | 'video.asset.track.errored' | 'video.asset.track.deleted'
  created_at: string
  data: Asset
  job?: {
    id: string
    kind: string
    error_code?: string
  }
  track?: {
    id: string
    kind: string
    error_code?: string
  }
}

export type App = {
  id: string
  name: string
  scopes: string[]
  public_url: string
  base_url: string
  max_token_lifetime_seconds: number
  /** Where a browser sends upload parts: this server (filesystem storage) and each bucket's public endpoint. With base_url, what a page with the uploader and player must allow in its Content-Security-Policy. */
  upload_origins: string[]
  signing_keys: {
    kid: string
    public_key_pem: string
    created_at: string
    retired_at?: string
  }[]
}

export type AnalyticsTotals = {
  views: number
  watch_seconds: number
  unique_viewers: number | null
  bytes_delivered: number
}

export type Analytics = {
  totals: AnalyticsTotals
  previous: AnalyticsTotals
  series: {
    t: string
    views: number
    watch_seconds: number
    bytes_delivered: number
  }[]
  top_assets?: {
    asset_id: string
    title?: string
    views: number
    watch_seconds: number
    bytes_delivered: number
  }[]
  collecting: {
    builtin: boolean
    edge: 'not_used' | 'not_connected' | 'connected'
  }
}

/** Also the shape @clipmux/player's session-url expects from an app's own handler. */
export type SharePlayback = {
  title?: string
  duration_seconds?: number | null
  base_url: string
  playback_id: string
  token: string
  expires_at: string
  poster_url: string
  tracks: Track[]
}

export type StartPairing = {
  name?: string
  hostname?: string
  client?: string
}

export type Pairing = {
  code: string
  poll_token: string
  verify_url: string
  expires_at: string
  interval_seconds: number
}

export type PollPairing = {
  poll_token: string
}

export type Credentials = {
  url: string
  app_id: string
  app_name: string
  api_key_id: string
  api_key: string
  signing_kid: string
  /** PKCS #8 PEM. */
  signing_key: string
  webhook_endpoint_id?: string
  webhook_secret?: string
  /** The same values as a .env block. */
  env: string
}

export type PairingStatus = {
  status: 'pending' | 'approved' | 'denied' | 'expired'
  credentials?: Credentials
}

/** The ladder rungs to encode, e.g. ["1080p", "480p"]. Rungs above the source are skipped; if none chosen fits, the largest rung that fits is encoded. Leave it out for every rung up to the source. Shown largest first. */
export type Renditions = ('2160p' | '1440p' | '1080p' | '720p' | '480p' | '360p')[]

/** Every webhook event type the spec lists. */
export type WebhookEventType = 'video.asset.ready' | 'video.asset.updated' | 'video.asset.errored' | 'video.asset.reencode_failed' | 'video.asset.deleted' | 'video.asset.track.ready' | 'video.asset.track.errored' | 'video.asset.track.deleted'
