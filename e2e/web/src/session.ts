// <clipmux-player session-url>: the app's backend (here the fixture's
// /session) answers with the base URL, token, poster and tracks, so the page
// knows only the playback ID.
import '@clipmux/player/element'
import { config, expose } from './common.ts'

const params = new URLSearchParams(location.search)
const cfg = await config()
const state = { tokenEvents: 0, denied: false, errors: [] as number[], chapters: 0 }
expose(state)

const session = new URLSearchParams({ ttl: params.get('ttl') ?? '3600', n: crypto.randomUUID() })
const denyAfter = params.get('deny_after')
if (denyAfter) session.set('deny_after', denyAfter)

const el = document.createElement('clipmux-player')
el.setAttribute('session-url', `/session?${session}`)
el.setAttribute('playback-id', cfg.playback_id)
el.setAttribute('autoplay', '')
el.setAttribute('muted', '')
// A short buffer, so segments are still being fetched when the token expires.
el.hls = { maxBufferLength: 4, maxMaxBufferLength: 4 }
// The events are typed: no casts.
el.addEventListener('clipmux-token', (e) => {
  if (e.detail.token) state.tokenEvents++
})
el.addEventListener('clipmux-denied', () => {
  state.denied = true
})
el.addEventListener('clipmux-error', (e) => state.errors.push(e.detail.status))
el.addEventListener('clipmux-chapters', (e) => {
  state.chapters = e.detail.chapters.length
})
document.body.append(el)
