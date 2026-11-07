import assert from 'node:assert/strict'
import { test } from 'node:test'
import { rewriteTokenUrl, TokenSession } from './refresh.ts'
import { fetchTrack, parseChapters, trackUrl } from './tracks.ts'

const media = 'https://media.example.net'
const A = 'a.a.a'
const B = 'b.b.b'

// The exact bytes serve writes for two chapters (internal/chapters).
const chaptersVTT =
  'WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n\n' +
  '1\n00:00:00.000 --> 00:00:21.234\nIntro\n\n' +
  '2\n00:00:21.234 --> 01:00:30.789\nQ&amp;A &lt;live&gt; नमस्ते\n\n'

test('chapters convert to seconds with their titles unescaped', () => {
  assert.deepEqual(parseChapters(chaptersVTT), [
    { startTime: 0, endTime: 21.234, title: 'Intro' },
    { startTime: 21.234, endTime: 3630.789, title: 'Q&A <live> नमस्ते' },
  ])
  assert.deepEqual(parseChapters('WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n\n'), [])
  assert.throws(() => parseChapters('<html>'))
})

test('a track URL is the sidecar URL, signed or public', () => {
  assert.equal(trackUrl(`${media}/`, 'pid_X', 'trk_1', A), `${media}/v/${A}/pid_X/tracks/trk_1.vtt`)
  assert.equal(trackUrl(media, 'pid_X', 'trk_1'), `${media}/p/pid_X/tracks/trk_1.vtt`)
})

test('subtitle requests carry the refreshed token like the video', () => {
  // hls.js loads subtitle playlists and VTT segments through config.loader.
  assert.equal(
    rewriteTokenUrl(`${media}/v/${A}/pid_X/tracks/trk_1.m3u8`, media, 'pid_X', B),
    `${media}/v/${B}/pid_X/tracks/trk_1.m3u8`,
  )
  assert.equal(
    rewriteTokenUrl(`${media}/v/${A}/pid_X/tracks/trk_1.vtt`, media, 'pid_X', B),
    `${media}/v/${B}/pid_X/tracks/trk_1.vtt`,
  )
})

test('the chapters VTT is fetched again with a fresh token after a 401', async () => {
  const asked: string[] = []
  const fake = (async (url: string) => {
    asked.push(url)
    return url.includes(`/v/${A}/`)
      ? new Response('{"error":{"code":"token_expired"}}', { status: 401 })
      : new Response(chaptersVTT, { status: 200 })
  }) as typeof fetch
  let refreshed = 0
  const s = new TokenSession(A, async () => {
    refreshed++
    return B
  })
  const body = await fetchTrack((t) => trackUrl(media, 'pid_X', 'trk_1', t), s, undefined, fake)
  assert.equal(body, chaptersVTT)
  assert.deepEqual(asked, [
    `${media}/v/${A}/pid_X/tracks/trk_1.vtt`,
    `${media}/v/${B}/pid_X/tracks/trk_1.vtt`,
  ])
  assert.equal(refreshed, 1)
})

test('a refused track fails without a refresh', async () => {
  const fake = (async () => new Response('', { status: 403 })) as unknown as typeof fetch
  let refreshed = 0
  const s = new TokenSession(A, async () => {
    refreshed++
    return B
  })
  await assert.rejects(
    fetchTrack((t) => trackUrl(media, 'pid_X', 'trk_1', t), s, undefined, fake),
    /HTTP 403/,
  )
  assert.equal(refreshed, 0)
})
