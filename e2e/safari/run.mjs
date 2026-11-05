// Drives Safari through safaridriver (WebDriver) against TestPlayerFixture:
// native HLS plays the signed, encrypted video with its separate audio, and
// <clipmux-player> (hls.js on MSE) refreshes an 8 s token mid-playback.
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
