// <clipmux-player> (Video.js 10, the shipped player) with short-lived tokens.
import '@clipmux/player/element'
import { config, expose, token } from './common.ts'

const params = new URLSearchParams(location.search)
const ttl = Number(params.get('ttl') ?? 3600)
const deny = params.get('deny') === '1'
const cfg = await config()
const state = {
  getTokenCalls: 0,
  getTokenArgs: [] as unknown[],
  tokenEvents: 0,
  denied: false,
  errors: 0,
  chapters: [] as unknown[],
}
expose(state)

const el = document.createElement('clipmux-player')
el.setAttribute('base-url', cfg.base_url)
el.setAttribute('playback-id', cfg.playback_id)
el.setAttribute('autoplay', '')
el.setAttribute('muted', '')
// A short buffer, so segments are still being fetched when the token expires.
el.hls = { maxBufferLength: 4, maxMaxBufferLength: 4 }
el.addEventListener('clipmux-token', () => state.tokenEvents++)
el.addEventListener('clipmux-denied', () => {
  state.denied = true
})
el.addEventListener('clipmux-error', () => state.errors++)
// ?chapters=1 names the asset's chapters track (P5-T18); ?chapters=late
// names it once playback is past 10 s, after an 8 s token has expired.
if (params.get('chapters') === '1') el.setAttribute('chapters-track', cfg.chapters_track)
if (params.get('chapters') === 'late') {
  const wait = setInterval(() => {
    if ((el.media?.currentTime ?? 0) > 10) {
      clearInterval(wait)
      el.setAttribute('chapters-track', cfg.chapters_track)
    }
  }, 200)
}
el.addEventListener('clipmux-chapters', (e) => {
  state.chapters = (e as CustomEvent<{ chapters: unknown[] }>).detail.chapters
})
el.getToken = async (request) => {
  state.getTokenCalls++
  state.getTokenArgs.push(request)
  if (deny && state.getTokenCalls > 1) return null
  return token(ttl)
}
// The element asks for its first token, then refreshes on expiry.
document.body.append(el)
