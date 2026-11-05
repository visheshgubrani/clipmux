// The browser's own HLS (Safari): a plain <video> with the signed URL.
import { config, expose, token } from './common.ts'

const cfg = await config()
const video = document.createElement('video')
video.muted = true
video.playsInline = true
document.body.append(video)
const state = { native: video.canPlayType('application/vnd.apple.mpegurl') !== '', error: '' }
expose({
  get currentTime() {
    return video.currentTime
  },
  get audioTracks() {
    return (video as HTMLVideoElement & { audioTracks?: { length: number } }).audioTracks?.length ?? -1
  },
  get error() {
    return video.error?.message ?? state.error
  },
  native: state.native,
})
video.src = `${cfg.base_url}/v/${await token()}/${cfg.playback_id}/master.m3u8`
video.play().catch((e: Error) => {
  state.error = e.message
})
