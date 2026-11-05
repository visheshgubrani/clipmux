import { expect, type Page, test } from '@playwright/test'

interface Seen {
  url: string
  status: number
}

function watch(page: Page): Seen[] {
  const seen: Seen[] = []
  page.on('response', (r) => {
    if (r.url().includes('/v/') || r.url().includes('/p/')) seen.push({ url: r.url(), status: r.status() })
  })
  return seen
}

async function h264(page: Page): Promise<void> {
  const ok = await page.evaluate(() => MediaSource.isTypeSupported('video/mp4; codecs="avc1.64001f"'))
  expect(ok, 'this Chromium build has no H.264; set CLIPMUX_E2E_CHANNEL=chrome').toBe(true)
}

async function playedPast(page: Page, seconds: number): Promise<void> {
  await expect
    .poll(
      () =>
        page.evaluate(
          () =>
            (document.querySelector('hlsjs-video') as unknown as HTMLVideoElement | null)?.currentTime ?? 0,
        ),
      {
        timeout: 120_000,
      },
    )
    .toBeGreaterThan(seconds)
}

test('hls.js plays every rendition, separate audio and the key', async ({ page, request }) => {
  const seen = watch(page)
  await page.goto('/hlsjs.html')
  await h264(page)
  await expect
    .poll(() => page.evaluate(() => (document.querySelector('video') as HTMLVideoElement).currentTime), {
      timeout: 120_000,
    })
    .toBeGreaterThan(8)
  const state = await page.evaluate(
    () => (globalThis as unknown as { __state: Record<string, unknown> }).__state,
  )
  expect(state.levels).toBe(3)
  expect(state.audioTracks).toBe(1)
  expect(state.keysLoaded).toBeGreaterThan(0)
  expect(state.errors, `hls.js errors: ${JSON.stringify(state.errors)}`).toEqual([])
  expect(
    await page.evaluate(
      () =>
        (document.querySelector('video') as unknown as { webkitAudioDecodedByteCount: number })
          .webkitAudioDecodedByteCount,
    ),
  ).toBeGreaterThan(0)

  const key = seen.find((s) => /\/key\/1$/.test(s.url))
  const segment = seen.find((s) => /\/seg_\d+\.m4s$/.test(s.url))
  expect(key?.status).toBe(200)
  expect(segment?.status).toBe(200)
  const keyBytes = await (await request.get(key!.url)).body()
  expect(keyBytes.length).toBe(16)
  // What travels is ciphertext: not an MP4 box stream.
  const wire = await (await request.get(segment!.url)).body()
  expect(wire.subarray(4, 8).toString('latin1')).not.toBe('styp')

  // A thumbnail token gets neither the segment nor the key; no token, nothing.
  const tToken = await (await request.get('/token?aud=t')).text()
  const vToken = String(state.tok)
  expect((await request.get(segment!.url.replace(`/v/${vToken}/`, `/v/${tToken}/`))).status()).toBe(403)
  expect((await request.get(key!.url.replace(`/v/${vToken}/`, `/v/${tToken}/`))).status()).toBe(403)
  const config = await (await request.get('/config')).json()
  expect((await request.get(`${config.base_url}/p/${config.playback_id}/master.m3u8`)).status()).toBe(403)
})

test('the Video.js player refreshes an expiring token and keeps its place', async ({ page }) => {
  const seen = watch(page)
  await page.goto('/player.html?ttl=8')
  await h264(page)
  await playedPast(page, 20)
  const state = await page.evaluate(
    () => (globalThis as unknown as { __state: Record<string, number | boolean> }).__state,
  )
  expect(
    seen.some((s) => s.status === 401),
    'the 8 s token must have expired mid-playback',
  ).toBe(true)
  expect(state.getTokenCalls).toBeGreaterThanOrEqual(2)
  expect(state.tokenEvents).toBeGreaterThanOrEqual(1)
  expect(state.errors).toBe(0)
  const played = await page.evaluate(() => {
    const v = document.querySelector('hlsjs-video') as unknown as HTMLVideoElement
    return Array.from({ length: v.played.length }, (_, i) => [v.played.start(i), v.played.end(i)])
  })
  expect(played.length, `played ranges ${JSON.stringify(played)}: one, from the start`).toBe(1)
  expect(played[0]![0]).toBeLessThan(1)
})

test('a denied refresh stops playback', async ({ page }) => {
  await page.goto('/player.html?ttl=6&deny=1')
  await h264(page)
  await expect
    .poll(
      () => page.evaluate(() => (globalThis as unknown as { __state: { denied: boolean } }).__state.denied),
      { timeout: 120_000 },
    )
    .toBe(true)
})
