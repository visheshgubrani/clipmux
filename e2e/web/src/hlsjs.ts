// Plain hls.js against ClipMux delivery: the development and test player (D014).
import Hls from 'hls.js'
import { config, expose, token } from './common.ts'

const cfg = await config()
const tok = await token()
const video = document.createElement('video')
video.muted = true
document.body.append(video)
const state = { levels: 0, audioTracks: 0, keysLoaded: 0, errors: [] as string[], url: '', tok }
expose(state)
const hls = new Hls()
hls.on(Hls.Events.MANIFEST_PARSED, (_e, d) => {
  state.levels = d.levels.length
})
hls.on(Hls.Events.AUDIO_TRACKS_UPDATED, (_e, d) => {
  state.audioTracks = d.audioTracks.length
})
hls.on(Hls.Events.KEY_LOADED, () => {
  state.keysLoaded++
})
// Non-fatal errors are hls.js recovering (a key load cancelled by a level
// switch, say); only fatal ones stop playback.
hls.on(Hls.Events.ERROR, (_e, d) => {
  if (d.fatal) state.errors.push(d.details)
})
state.url = `${cfg.base_url}/v/${tok}/${cfg.playback_id}/master.m3u8`
hls.loadSource(state.url)
hls.attachMedia(video)
void video.play()
