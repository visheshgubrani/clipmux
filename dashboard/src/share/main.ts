// The share page (D104): {public_url}/s/{secret} plays one video for anyone
// holding the link. The HTML is static; this script reads the secret from
// the path and asks POST /s/{secret}/playback for a token, again whenever
// the player needs a fresh one. It shares none of the dashboard's bundle.
import '@fontsource-variable/archivo'
import '@clipmux/player/element'
import { chaptersTrackOf, type PlaybackSession } from '@clipmux/player'
import './share.css'

interface Playback extends PlaybackSession {
  duration_seconds: number
  expires_at: string
  poster_url: string
}

type Outcome = { ok: true; playback: Playback } | { ok: false; status: number; code: string }

const secret = /^\/s\/([^/]+)\/?$/.exec(location.pathname)?.[1] ?? ''
const byId = (id: string) => document.getElementById(id) as HTMLElement
const stage = byId('stage')
const heading = byId('title')
const notice = byId('notice')
const retry = byId('retry') as HTMLButtonElement

// A random 128-bit ID kept in this browser, so the owner's analytics count
// unique viewers (D103). Without storage (a private window) there is none.
function viewerID(): string | undefined {
  try {
    let id = localStorage.getItem('clipmux-viewer')
    if (!id || !/^[0-9a-f]{32}$/.test(id)) {
      id = Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) =>
        b.toString(16).padStart(2, '0'),
      ).join('')
      localStorage.setItem('clipmux-viewer', id)
    }
    return id
  } catch {
    return undefined
  }
}
const viewer = viewerID()

async function playback(): Promise<Outcome> {
  if (!secret) return { ok: false, status: 404, code: 'share_not_found' }
  try {
    const res = await fetch(`/s/${encodeURIComponent(secret)}/playback`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(viewer ? { viewer_id: viewer } : {}),
      credentials: 'omit',
      cache: 'no-store',
    })
    const body = await res.json().catch(() => ({}))
    if (res.ok) return { ok: true, playback: body as Playback }
    return { ok: false, status: res.status, code: (body as { error?: { code?: string } }).error?.code ?? '' }
  } catch {
    return { ok: false, status: 0, code: 'network' }
  }
}

const messages: Record<string, [string, string]> = {
  share_not_found: [
    'This link doesn’t work',
    'It may have been turned off, or the video deleted. Ask whoever shared it for a new link.',
  ],
  share_expired: ['This link has expired', 'Ask whoever shared it for a new link.'],
  asset_not_ready: [
    'This video is still processing',
    'It plays here once it’s ready. This page checks again every 15 seconds.',
  ],
  rate_limited: ['Too many requests from this network', 'Wait a moment, then try again.'],
  failed: ['The video couldn’t load', 'Check your connection, then try again.'],
}

let waiting: number | undefined

function show(o: Extract<Outcome, { ok: false }>): void {
  const key = o.code in messages ? o.code : 'failed'
  const [title, text] = messages[key] as [string, string]
  stage.replaceChildren()
  stage.hidden = true
  heading.hidden = true
  byId('notice-title').textContent = title
  byId('notice-text').textContent = text
  retry.hidden = key !== 'rate_limited' && key !== 'failed'
  notice.hidden = false
  notice.dataset.state = key
  document.title = title
  window.clearTimeout(waiting)
  if (key === 'asset_not_ready') waiting = window.setTimeout(() => void start(), 15_000)
}

function play(p: Playback): void {
  notice.hidden = true
  stage.hidden = false
  stage.removeAttribute('aria-busy')
  heading.textContent = p.title ?? ''
  heading.hidden = !p.title
  document.title = p.title || 'Shared video'
  const el = document.createElement('clipmux-player')
  const setChapters = (grant: Playback) => {
    const track = chaptersTrackOf(grant)
    if (track) el.setAttribute('chapters-track', track)
    else el.removeAttribute('chapters-track')
  }
  el.token = p.token
  // On a 401 the player asks for a fresh token; a link that stopped
  // meanwhile answers why, and the page says so.
  el.getToken = async () => {
    const o = await playback()
    if (o.ok) {
      if (o.playback.playback_id === p.playback_id) setChapters(o.playback)
      return o.playback.token
    }
    show(o)
    return null
  }
  // A link revoked mid-play is refused by delivery: ask the page's endpoint
  // what happened.
  const recheck = async () => {
    const o = await playback()
    if (!o.ok) show(o)
  }
  el.addEventListener('clipmux-denied', () => void recheck())
  el.addEventListener('clipmux-error', (e) => {
    const status = (e as CustomEvent<{ status?: number }>).detail?.status
    if (status === 403 || status === 404) void recheck()
  })
  el.setAttribute('poster', p.poster_url)
  setChapters(p)
  el.setAttribute('base-url', p.base_url)
  el.setAttribute('playback-id', p.playback_id)
  stage.replaceChildren(el)
}

async function start(): Promise<void> {
  const o = await playback()
  if (o.ok) play(o.playback)
  else show(o)
}

retry.addEventListener('click', () => void start())
void start()
