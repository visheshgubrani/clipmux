import { expect, type Page, test } from '@playwright/test'

// <clipmux-player session-url>: the page knows only the playback ID; the
// fixture's /session stands in for the app's backend handler.

interface State {
  tokenEvents: number
  denied: boolean
  errors: number[]
  chapters: number
}

const state = (page: Page) => page.evaluate(() => (globalThis as unknown as { __state: State }).__state)

function sessions(page: Page): unknown[] {
  const bodies: unknown[] = []
  page.on('request', (r) => {
    if (new URL(r.url()).pathname === '/session') bodies.push(r.postDataJSON())
  })
  return bodies
}

async function h264(page: Page): Promise<void> {
  const ok = await page.evaluate(() => MediaSource.isTypeSupported('video/mp4; codecs="avc1.64001f"'))
  expect(ok, 'this Chromium build has no H.264; set CLIPMUX_E2E_CHANNEL=chrome').toBe(true)
}

const media = (page: Page) =>
  page.evaluate(() => {
    const v = document.querySelector('hlsjs-video') as unknown as HTMLVideoElement
    return {
      currentTime: v.currentTime,
      poster: v.poster,
      played: Array.from({ length: v.played.length }, (_, i) => [v.played.start(i), v.played.end(i)]),
    }
  })

test('session-url plays a signed video and refreshes an expiring token through it', async ({ page }) => {
  const bodies = sessions(page)
  const statuses: number[] = []
  let first = true
  await page.route(
    (url) => url.pathname === '/session',
    async (route) => {
      if (!first) return route.continue()
      first = false
      const response = await route.fetch()
      await route.fulfill({ response, json: { ...(await response.json()), tracks: [] } })
    },
  )
  page.on('response', (r) => {
    if (r.url().includes('/v/')) statuses.push(r.status())
  })
  await page.goto('/session.html?ttl=8')
  await h264(page)
  await expect.poll(async () => (await media(page)).currentTime, { timeout: 120_000 }).toBeGreaterThan(20)
  const cfg = await (await page.request.get('/config')).json()
  expect(statuses, 'the 8 s token must have expired mid-playback').toContain(401)
  expect(bodies.length).toBeGreaterThanOrEqual(2)
  for (const body of bodies) expect(body).toEqual({ playback_id: cfg.playback_id, native: false })
  const s = await state(page)
  expect(s.tokenEvents).toBeGreaterThanOrEqual(1)
  expect(s.errors).toEqual([])
  expect(s.chapters, "the renewed session's ready chapters track is marked").toBeGreaterThan(0)
  await expect(page.locator('.media-time-slider-chapter')).toHaveCount(2)
  const m = await media(page)
  expect(m.poster).toMatch(new RegExp(`/v/[^/]+/${cfg.playback_id}/poster\\.jpg$`))
  expect(m.played.length, `played ranges ${JSON.stringify(m.played)}: one, from the start`).toBe(1)
  expect(m.played[0]?.[0]).toBeLessThan(1)
})

test('a session URL that refuses the viewer fires clipmux-denied before anything loads', async ({ page }) => {
  const loads: string[] = []
  page.on('request', (r) => {
    if (r.url().includes('/v/')) loads.push(r.url())
  })
  await page.goto('/session.html?deny_after=0')
  await expect.poll(async () => (await state(page)).denied, { timeout: 30_000 }).toBe(true)
  expect(loads).toEqual([])
})

test('a session URL that refuses a refresh stops playback', async ({ page }) => {
  await page.goto('/session.html?ttl=6&deny_after=1')
  await h264(page)
  await expect.poll(async () => (await state(page)).denied, { timeout: 120_000 }).toBe(true)
  expect(
    await page.evaluate(() => (document.querySelector('hlsjs-video') as unknown as HTMLVideoElement).paused),
  ).toBe(true)
})
