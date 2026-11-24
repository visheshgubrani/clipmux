import { type APIRequestContext, expect, type Page, request as playwright, test } from '@playwright/test'
import { noHorizontalScroll, type OpsConfig, watchCSP } from './csp.ts'

// The share page (P7-T07, D104): a link to the fixture's signed video plays
// in a fresh browser with no session, with its subtitles, and says plainly
// when it has expired, been revoked or never existed. The owner makes the
// links over the API with their session, acting as the video's app; each
// test revokes its links, so the other suites see the asset as it was.

interface Link {
  id: string
  url: string
  playback_id: string
  status: string
}

let owner: APIRequestContext
let cfg: OpsConfig
const made: string[] = []

test.beforeEach(async ({ request }) => {
  cfg = (await (await request.get('/config')).json()) as OpsConfig
  owner = await playwright.newContext({ baseURL: cfg.ops })
  const res = await owner.post('/v1/session', { data: { email: cfg.ops_email, password: cfg.ops_password } })
  expect(res.ok(), await res.text()).toBe(true)
})

test.afterEach(async () => {
  for (const id of made.splice(0)) {
    await owner.delete(`/v1/shares/${id}`, { headers: { 'ClipMux-App': cfg.app_id } })
  }
  await owner.dispose()
})

async function share(body: { name?: string; expires_at?: string } = {}): Promise<Link> {
  const res = await owner.post(`/v1/assets/${cfg.asset_id}/shares`, {
    data: body,
    headers: { 'ClipMux-App': cfg.app_id },
  })
  expect(res.status(), await res.text()).toBe(201)
  const link = (await res.json()) as Link
  made.push(link.id)
  return link
}

async function playing(page: Page): Promise<void> {
  const h264 = await page.evaluate(() => MediaSource.isTypeSupported('video/mp4; codecs="avc1.64001f"'))
  expect(h264, 'this Chromium build has no H.264; set CLIPMUX_E2E_CHANNEL=chrome').toBe(true)
  await expect
    .poll(
      () =>
        page.evaluate(() => {
          const v = document.querySelector('hlsjs-video') as unknown as HTMLVideoElement | null
          if (!v) return 0
          v.muted = true
          if (v.paused) void v.play().catch(() => {})
          return v.currentTime
        }),
      { timeout: 60_000, message: 'the shared video plays' },
    )
    .toBeGreaterThan(1)
}

test('a share link plays in a fresh browser, with its subtitles and chapters', async ({ page }) => {
  const link = await share({ name: 'For the board' })
  expect(link.url).toBe(`${cfg.ops}/s/${link.url.split('/s/')[1]}`)
  const noViolations = await watchCSP(page)
  const playback = page.waitForResponse(
    (r) => r.url().endsWith('/playback') && r.request().method() === 'POST',
  )
  await page.goto(link.url)
  const res = await playback
  expect(res.status()).toBe(200)
  // A per-browser viewer ID goes with it, for unique viewers.
  expect(res.request().postDataJSON().viewer_id).toMatch(/^[0-9a-f]{32}$/)
  await playing(page)
  await expect
    .poll(() =>
      page.evaluate(() => {
        const v = document.querySelector('hlsjs-video') as unknown as HTMLVideoElement
        return Array.from(v.textTracks).some((t) => t.kind === 'subtitles' && t.language === 'en')
      }),
    )
    .toBe(true)
  await expect(page.locator('clipmux-player .media-time-slider-chapter')).toHaveCount(2)
  await expect(page.getByText('Shared with ClipMux')).toBeVisible()
  // The page carries no session: nothing of the dashboard's.
  expect((await page.context().cookies()).length).toBe(0)
  await noHorizontalScroll(page)
  await noViolations()
})

test('a revoked link says it no longer works', async ({ page }) => {
  const link = await share()
  const noViolations = await watchCSP(page)
  await page.goto(link.url)
  await playing(page)
  const res = await owner.delete(`/v1/shares/${link.id}`, { headers: { 'ClipMux-App': cfg.app_id } })
  expect(res.status()).toBe(200)
  await page.reload()
  await expect(page.getByRole('heading', { name: 'This link doesn’t work' })).toBeVisible()
  await expect(page.locator('hlsjs-video')).toHaveCount(0)
  await noHorizontalScroll(page)
  await noViolations()
})

test('an expired link says so', async ({ page }) => {
  const link = await share({ expires_at: new Date(Date.now() + 3_000).toISOString() })
  await new Promise((r) => setTimeout(r, 4_000))
  const noViolations = await watchCSP(page)
  const playback = page.waitForResponse((r) => r.url().endsWith('/playback'))
  await page.goto(link.url)
  expect((await playback).status()).toBe(410)
  await expect(page.getByRole('heading', { name: 'This link has expired' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Try again' })).toHaveCount(0)
  await noHorizontalScroll(page)
  await noViolations()
})

test('a link that never existed says it doesn’t work', async ({ page }) => {
  const noViolations = await watchCSP(page)
  await page.goto(`${cfg.ops}/s/NOTALINKNOTALINKNOTALINKNO`)
  await expect(page.getByRole('heading', { name: 'This link doesn’t work' })).toBeVisible()
  await noHorizontalScroll(page)
  await noViolations()
})
