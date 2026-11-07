// Subtitles and chapters of a ClipMux video (docs/api.md, "Tracks").
//
// Subtitles are in the master playlist, so hls.js lists them and loads
// their playlist and VTT through the same refresh loader as the video. The
// chapters VTT is never listed there: the app names its track, and it is
// fetched here with the session's token (refreshed on a 401, like any
// other request) and parsed into chapters in seconds.
import type { TokenSession } from './refresh.ts'

/** One chapter: start and end in seconds, as players use them. */
export interface Chapter {
  startTime: number
  endTime: number
  title: string
}

/** The URL of a track's VTT: the sidecar URL, private or public. */
export function trackUrl(baseUrl: string, playbackId: string, trackId: string, token?: string): string {
  const base = baseUrl.replace(/\/+$/, '')
  const pid = encodeURIComponent(playbackId)
  const trk = encodeURIComponent(trackId)
  return token ? `${base}/v/${token}/${pid}/tracks/${trk}.vtt` : `${base}/p/${pid}/tracks/${trk}.vtt`
}

const timing = /^(\d{2,}):(\d{2}):(\d{2})\.(\d{3}) --> (\d{2,}):(\d{2}):(\d{2})\.(\d{3})/
const entities: Record<string, string> = { '&amp;': '&', '&lt;': '<', '&gt;': '>' }

// seconds reads the four numbers of a timing match from index at.
function seconds(m: RegExpExecArray, at: number): number {
  const [h, min, s, ms] = [m[at], m[at + 1], m[at + 2], m[at + 3]].map(Number) as [
    number,
    number,
    number,
    number,
  ]
  return (h * 3600_000 + min * 60_000 + s * 1000 + ms) / 1000
}

/**
 * Parses the chapters VTT ClipMux writes: one cue per chapter, its title on
 * one line with &, < and > escaped.
 */
export function parseChapters(vtt: string): Chapter[] {
  const out: Chapter[] = []
  const lines = vtt.replace(/\r\n?/g, '\n').split('\n')
  if (!lines[0]?.startsWith('WEBVTT')) throw new Error('not a WebVTT file')
  for (let i = 0; i < lines.length; i++) {
    const m = timing.exec(lines[i] ?? '')
    if (!m) continue
    const text: string[] = []
    while (i + 1 < lines.length && lines[i + 1] !== '') text.push(lines[++i] ?? '')
    out.push({
      startTime: seconds(m, 1),
      endTime: seconds(m, 5),
      title: text.join(' ').replace(/&(amp|lt|gt);/g, (e) => entities[e] ?? e),
    })
  }
  return out
}

/**
 * Fetches a track's VTT with the session's current token. A 401 gets one
 * fresh token and a retry, as the refresh loader does for the video.
 */
export async function fetchTrack(
  url: (token: string | undefined) => string,
  session: TokenSession | null,
  token: string | undefined,
  fetchFn: typeof fetch = fetch,
): Promise<string> {
  let current = session?.current ?? token
  for (let retried = false; ; retried = true) {
    const res = await fetchFn(url(current), { credentials: 'omit' })
    if (res.ok) return res.text()
    if (res.status === 401 && session && current && !retried) {
      const next = await session.refresh(current)
      if (next) {
        current = next
        continue
      }
    }
    throw new Error(`track: HTTP ${res.status}`)
  }
}
