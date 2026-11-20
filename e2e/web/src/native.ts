// The browser's own HLS (Safari): a plain <video> with the signed URL. The
// subtitles track the master lists is turned on as soon as Safari adds it,
// and the first cue that becomes active is recorded with the playhead's time
// then (P6-T03, P5 check 14 and open question 8).
import { config, expose, token } from './common.ts'

const cfg = await config()
const video = document.createElement('video')
video.muted = true
video.playsInline = true
document.body.append(video)
const state = { native: video.canPlayType('application/vnd.apple.mpegurl') !== '', error: '' }
let cue: { text: string; start: number; at: number } | null = null

function watch(t: TextTrack): void {
  if (t.kind !== 'subtitles') return
  t.mode = 'showing'
  t.addEventListener('cuechange', () => {
    const c = t.activeCues?.[0] as VTTCue | undefined
    if (!cue && c?.text) cue = { text: c.text, start: c.startTime, at: video.currentTime }
  })
}
for (const t of Array.from(video.textTracks)) watch(t)
video.textTracks.addEventListener('addtrack', (e) => {
  if (e.track) watch(e.track)
})

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
  /** Every text track Safari listed, as kind:language:label:mode. */
  get textTracks() {
    return Array.from(video.textTracks).map((t) => `${t.kind}:${t.language}:${t.label}:${t.mode}`)
  },
  /** The first subtitle cue that became active while playing. */
  get cue() {
    return cue
  },
  /** The cue active now on the showing subtitles track, for a check after a seek. */
  get activeCue() {
    const t = Array.from(video.textTracks).find((t) => t.kind === 'subtitles' && t.mode === 'showing')
    const c = t?.activeCues?.[0] as VTTCue | undefined
    return c ? { text: c.text, start: c.startTime, at: video.currentTime } : null
  },
  /** Pauses at t, so a check after the seek reads the cue at that time. */
  seek(t: number) {
    video.pause()
    video.currentTime = t
  },
  cueText: cfg.cue_text,
  cueStart: cfg.cue_start,
})
video.src = `${cfg.base_url}/v/${await token()}/${cfg.playback_id}/master.m3u8`
video.play().catch((e: Error) => {
  state.error = e.message
})
