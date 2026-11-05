// <clipmux-player> (Video.js 10, the shipped player) with short-lived tokens.
import '@clipmux/player/element'
import { config, expose, token } from './common.ts'

const params = new URLSearchParams(location.search)
const ttl = Number(params.get('ttl') ?? 3600)
const deny = params.get('deny') === '1'
const cfg = await config()
const state = { getTokenCalls: 0, tokenEvents: 0, denied: false, errors: 0 }
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
el.getToken = async () => {
  state.getTokenCalls++
  if (deny && state.getTokenCalls > 1) return null
  return token(ttl)
}
// The element asks for its first token, then refreshes on expiry.
document.body.append(el)
