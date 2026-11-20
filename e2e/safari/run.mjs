// Drives Safari through safaridriver (WebDriver) against TestPlayerFixture:
// native HLS plays the signed, encrypted video with its separate audio and
// shows the subtitles track with the fake speech API's cue at its time
// (P6-T03), and <clipmux-player> (hls.js on MSE) refreshes an 8 s token
// mid-playback. exit-safari.yml runs it; on a Mac by hand, with Postgres
// (TEST_DATABASE_URL), ffmpeg and Shaka Packager on PATH:
//   pnpm install && pnpm -r run build
//   CLIPMUX_UNSANDBOXED_WORKER=true CLIPMUX_E2E=1 CLIPMUX_E2E_FIXTURE=1 \
//     go test ./internal/e2e -run TestPlayerFixture -count=1 -timeout 60m -v &
//   (wait until curl -sf http://127.0.0.1:8099/config answers)
//   sudo safaridriver --enable && safaridriver -p 4444 &
//   node e2e/safari/run.mjs
const driver = process.env.WEBDRIVER_URL ?? 'http://127.0.0.1:4444'
const pages = process.env.FIXTURE_URL ?? 'http://127.0.0.1:8099'

async function wd(method, path, body) {
  const res = await fetch(driver + path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  })
  const json = await res.json()
  if (!res.ok) throw new Error(`${method} ${path}: ${JSON.stringify(json)}`)
  return json.value
}

async function waitFor(session, script, ok, timeoutMs, label) {
  const deadline = Date.now() + timeoutMs
  let last
  while (Date.now() < deadline) {
    last = await wd('POST', `/session/${session}/execute/sync`, { script, args: [] })
    if (ok(last)) return last
    await new Promise((r) => setTimeout(r, 1000))
  }
  throw new Error(`${label}: still ${JSON.stringify(last)} after ${timeoutMs / 1000} s`)
}

// checkCue returns what is wrong with a cue seen on the page, or ''.
function checkCue(c, text, start) {
  if (!c) return 'no cue'
  if (c.text !== text) return `text ${JSON.stringify(c.text)}, want ${JSON.stringify(text)}`
  if (Math.abs(c.start - start) > 0.5) return `cue starts at ${c.start} s, want ${start} ± 0.5 s`
  // The cue runs 1–3 s; it is active while the playhead is there.
  if (c.at < start - 0.5 || c.at > 3.5) return `active at ${c.at} s, want ${start - 0.5}–3.5 s`
  return ''
}

// subtitles checks the native page's subtitles once it has played past 8 s:
// the cue recorded while playing, or, when none was, the cue active after
// seeking back into it, which still shows the playlist was accepted.
async function subtitles(session) {
  const s = await wd('POST', `/session/${session}/execute/sync`, {
    script: 'return {tracks: __state.textTracks, cue: __state.cue, text: __state.cueText, start: __state.cueStart}',
    args: [],
  })
  if (!s.tracks.some((t) => t.startsWith('subtitles:en:English:'))) throw new Error(`no English subtitles track in ${JSON.stringify(s.tracks)}`)
  if (s.cue) {
    const bad = checkCue(s.cue, s.text, s.start)
    if (bad) throw new Error(`${bad}: ${JSON.stringify(s.cue)}`)
    console.log(`ok native subtitles: "${s.cue.text}" from ${s.cue.start} s, active at ${s.cue.at.toFixed(2)} s while playing`)
    return
  }
  await wd('POST', `/session/${session}/execute/sync`, { script: `__state.seek(${s.start + 0.5})`, args: [] })
  const c = await waitFor(session, 'return __state.activeCue', (v) => v !== null, 15_000, `no cue while playing; after seeking to ${s.start + 0.5} s`)
  const bad = checkCue(c, s.text, s.start)
  if (bad) throw new Error(`no cue while playing; after a seek, ${bad}: ${JSON.stringify(c)}`)
  console.log(`ok native subtitles (after a seek; none became active while playing from 0): "${c.text}" from ${c.start} s`)
}

const { sessionId } = await wd('POST', '/session', { capabilities: { alwaysMatch: { browserName: 'safari' } } })
const failures = []
try {
  const version = await wd('POST', `/session/${sessionId}/execute/sync`, { script: 'return navigator.userAgent', args: [] })
  console.log(`Safari: ${version}`)

  await wd('POST', `/session/${sessionId}/url`, { url: `${pages}/native.html` })
  try {
    const s = await waitFor(sessionId, 'return globalThis.__state && {t: __state.currentTime, audio: __state.audioTracks, err: __state.error, native: __state.native}', (v) => v && (v.t > 8 || v.err), 120_000, 'native HLS')
    if (!s.native || s.err || s.audio !== 1) failures.push(`native HLS: ${JSON.stringify(s)}`)
    else console.log(`ok native HLS: played ${s.t.toFixed(1)} s with ${s.audio} audio track`)
  } catch (e) {
    failures.push(e.message)
  }
  // The subtitles track from the master, a single-segment WebVTT playlist as
  // long as the video (P5 open question 8): the cue the fake speech API heard
  // at 1–3 s is active within ±0.5 s of its time, as hls.js shows it in
  // e2e/web/tests/tracks.spec.ts (P5 check 14).
  try {
    await subtitles(sessionId)
  } catch (e) {
    failures.push(`native subtitles: ${e.message}`)
  }

  await wd('POST', `/session/${sessionId}/url`, { url: `${pages}/player.html?ttl=8` })
  try {
    const s = await waitFor(
      sessionId,
      'const v = document.querySelector("hlsjs-video"); return globalThis.__state && {t: v ? v.currentTime : 0, calls: __state.getTokenCalls, errors: __state.errors, played: v ? v.played.length : 0, engine: v ? v.engineName : ""}',
      (v) => v && v.t > 20,
      180_000,
      'clipmux-player refresh',
    )
    if (s.calls < 2 || s.errors > 0 || s.played !== 1) failures.push(`clipmux-player: ${JSON.stringify(s)}`)
    else console.log(`ok clipmux-player (${s.engine}): ${s.calls} tokens, one played range to ${s.t.toFixed(1)} s`)
  } catch (e) {
    failures.push(e.message)
  }
} finally {
  await wd('DELETE', `/session/${sessionId}`).catch(() => {})
}
if (failures.length) {
  console.error(failures.join('\n'))
  process.exit(1)
}
