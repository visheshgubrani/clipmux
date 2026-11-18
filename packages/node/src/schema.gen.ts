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
  /** The playback ID (pid_...) that goes in playback URLs. */
  id: string
  /** signed plays only with a token your backend signs; public plays for anyone with the URL. */
  policy: 'signed' | 'public'
  /** Whether the ID plays now: false until the asset has an output, and once revoked. */
  playable: boolean
  /** active, revoking while its routes are unpublished, then revoked. */
  status: 'active' | 'revoking' | 'revoked'
  /** Present when a share link owns this ID. */
  share?: {
    id: string
    name: string
  }
}

export type Track = {
  id: string
  /** subtitles, or chapters. */
  kind: 'subtitles' | 'chapters'
  /** generated: made by speech recognition or an LLM. */
  source: string
  /** preparing, ready or errored. */
  status: 'preparing' | 'ready' | 'errored'
  /** The detected language code for subtitles, for example en or hi. */
  language?: string
  name?: string
  /** Why the track errored, for example no_audio or llm_not_connected. */
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
  /** The asset ID (ast_...). */
  id: string
  /** The app the asset belongs to. */
  app_id: string
  /** waiting_upload until the file arrives, processing while it encodes, then ready or errored; deleted once deleted. */
  status: 'waiting_upload' | 'processing' | 'ready' | 'errored' | 'deleted'
  /** Rises on every change; keep the highest seen. */
  version: number
  /** Your own reference, such as lesson_42. Absent when unset. */
  passthrough?: string
  /** For people: not unique and never part of a URL. One line, up to 200 characters. Absent when unset. */
  title?: string
  /** The renditions this asset's outputs are encoded at; absent means every rung up to the source. */
  renditions?: Renditions
  /** Why the asset is errored, for example upload_cancelled, import_failed or unsupported_source. */
  error_code?: string
  /** The media origin playback URLs start with. */
  base_url: string
  /** Every playback ID, including revoked ones and those a share link owns. */
  playback_ids: PlaybackID[]
  /** Null until the first output exists. */
  duration_seconds: number | null
  /** Subtitles and chapters tracks; retired and deleted tracks are not listed. */
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
  /** The file size in bytes. */
  size: number
  /** The policy of the playback ID made with the asset. Default signed. */
  playback_policy?: 'signed' | 'public'
  /** Your own reference; it comes back on the asset and its webhooks. */
  passthrough?: string
  /** One line, up to 200 characters. */
  title?: string
  /** Ask for subtitles and chapters once the video is ready; chapters imply subtitles. */
  generate?: Generate
  renditions?: Renditions
}

export type Upload = {
  /** The upload ID (upl_...). */
  upload_id: string
  /** The asset this upload fills. */
  asset_id: string
  /** Bytes in every part but the last. */
  part_size: number
  /** How many parts to send. */
  part_count: number
  /** Hand this to the browser; it reaches only this upload. */
  upload_token?: string
  /** The asset's playback IDs, already made. */
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
  /** The page to send: {public_url}/s/{secret}. */
  url: string
  /** The signed playback ID the link owns. */
  playback_id: string
  /** When the link stops working, or null for never. */
  expires_at: string | null
  /** active, revoked or expired. */
  status: 'active' | 'revoked' | 'expired'
  created_at: string
  /** Views over the last 30 UTC days, in lists. */
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
  /** Exactly what a webhook endpoint receives. */
  body: WebhookEvent
}

export type EventList = {
  data: Event[]
}

export type WebhookEvent = {
  /** The event ID (evt_...), the same as X-Webhook-Id. Dedupe on it. */
  id: string
  type: 'video.asset.ready' | 'video.asset.updated' | 'video.asset.errored' | 'video.asset.reencode_failed' | 'video.asset.deleted' | 'video.asset.track.ready' | 'video.asset.track.errored' | 'video.asset.track.deleted'
  created_at: string
  /** The asset when the event was made. Re-fetch it before acting. */
  data: Asset
  /** The job behind the event, for re-encodes. */
  job?: {
    id: string
    kind: string
    error_code?: string
  }
  /** The track, for track events. */
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
  /** Where the API is. */
  public_url: string
  /** The media origin playback URLs start with. */
  base_url: string
  /** The longest a playback token may last: exp - iat. */
  max_token_lifetime_seconds: number
  /** Where a browser sends upload parts: this server (filesystem storage) and each bucket's public endpoint. With base_url, what a page with the uploader and player must allow in its Content-Security-Policy. */
  upload_origins: string[]
  /** The keys that still verify tokens, newest first. */
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
  /** Show it; the owner checks it matches on verify_url. Any case, the dash optional. */
  code: string
  poll_token: string
  verify_url: string
  expires_at: string
  /** How often to poll. */
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
  /** A new API key with the default scopes. Shown once. */
  api_key: string
  signing_kid: string
  /** PKCS #8 PEM. */
  signing_key: string
  webhook_endpoint_id?: string
  /** Present when a webhook URL was given. */
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
