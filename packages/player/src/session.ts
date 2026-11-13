// Playback sessions from the app's backend (session-url).
//
// The element POSTs {playback_id, native} to a URL of the app (for example
// the @clipmux/node handler's /playback route), which checks the viewer
// and answers with everything the player needs, in the shape of
// POST /s/{secret}/playback:
//
//   {playback_id, base_url, token, expires_at, poster_url, tracks}
//
// The browser then needs neither the ClipMux URL nor a token of its own.
// native says the browser will play HLS itself, so the backend signs a
// token that outlives the session (refresh can't reach native playback).

/** A track of the playback, as GET /v1/assets/{id} lists it. */
export interface SessionTrack {
  id: string
  kind: 'subtitles' | 'chapters' | (string & {})
  status: 'preparing' | 'ready' | 'errored' | (string & {})
  language?: string
  [field: string]: unknown
}

/**
 * What the session URL answers: `@clipmux/node`'s `playback.session()`
 * and POST /s/{secret}/playback both fit it.
 */
export interface PlaybackSession {
  playback_id: string
  base_url: string
  /** The playback token; empty for a public playback ID. */
  token: string
  /** When the token expires; null or absent for a public playback ID. */
  expires_at?: string | null
  poster_url?: string
  title?: string
  duration_seconds?: number | null
  tracks?: SessionTrack[]
}

/** The session URL answered something other than a session or a denial. */
export class SessionError extends Error {
  readonly status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'SessionError'
    this.status = status
  }
}

/**
 * Asks the app's session URL for a playback session. Resolves null when the
 * app denies this viewer (401 or 403), and rejects with a SessionError on
 * anything else that isn't a session, which TokenSession treats as
 * transient and retries.
 */
export async function fetchSession(
  url: string,
  playbackId: string,
  native: boolean,
  fetchFn: typeof fetch = fetch,
): Promise<PlaybackSession | null> {
  let res: Response
  try {
    res = await fetchFn(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ playback_id: playbackId, native }),
      cache: 'no-store',
    })
  } catch {
    throw new SessionError(0, 'the session URL could not be reached')
  }
  if (res.status === 401 || res.status === 403) {
    await res.body?.cancel()
    return null
  }
  if (!res.ok) {
    await res.body?.cancel()
    throw new SessionError(res.status, `session: HTTP ${res.status}`)
  }
  let body: Partial<PlaybackSession>
  try {
    body = (await res.json()) as Partial<PlaybackSession>
  } catch {
    throw new SessionError(res.status, 'session: the response is not JSON')
  }
  if (typeof body.base_url !== 'string' || !body.base_url || typeof body.token !== 'string') {
    throw new SessionError(res.status, 'session: the response has no base_url or token')
  }
  return {
    ...body,
    playback_id: typeof body.playback_id === 'string' && body.playback_id ? body.playback_id : playbackId,
    base_url: body.base_url,
    token: body.token,
  }
}

/** The first ready chapters track of a session, if any. */
export function chaptersTrackOf(session: PlaybackSession | null): string {
  return session?.tracks?.find((t) => t.kind === 'chapters' && t.status === 'ready')?.id ?? ''
}
