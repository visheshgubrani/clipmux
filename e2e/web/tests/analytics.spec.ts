import { expect, test } from '@playwright/test'
import { go, noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// Analytics (D100, D103; P7-T08): the page shows what this server counted,
// on the page and on a video's tab. The fixture's video was played by the
// other suites, so it has views; nothing says "not collecting". Cloudflare
// delivery isn't on in the fixture, so the edge states are served by the
// test from the API's own answers.

test('analytics shows what this server counted', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  // A play counts once serve flushes (15 s): make one with a fresh token.
  const tok = await (await request.get('/token')).text()
  const play = cfg as OpsConfig & { base_url: string; playback_id: string }
  expect((await request.get(`${play.base_url}/v/${tok}/${play.playback_id}/master.m3u8`)).status()).toBe(200)

  await go(page, 'Analytics')
  const panel = page.getByRole('region', { name: 'Analytics' })
  await expect(panel).toBeVisible()
  await expect(page.getByText("Analytics isn't collecting yet")).toHaveCount(0)
  await expect(page.getByText('Playback through Cloudflare isn’t counted yet')).toHaveCount(0)
  // The view lands once serve flushes; the Views total is then above 0.
  await expect
    .poll(
      async () => {
        await page.reload()
        await page.getByRole('button', { name: '30 days' }).click()
        const text = (await panel.getByRole('button', { name: /^Views/ }).textContent()) ?? ''
        return Number(/Views\s*([\d,]+)/.exec(text)?.[1]?.replaceAll(',', '') ?? 0)
      },
      { timeout: 90_000, intervals: [5_000] },
    )
    .toBeGreaterThan(0)
  await expect(page.getByText("Couldn't load")).toHaveCount(0)

  await page.goto(`${cfg.ops}/videos/${cfg.asset_id}?app=${cfg.app_id}&tab=analytics`)
  await expect(page.getByRole('region', { name: 'Analytics' })).toBeVisible()
  await noHorizontalScroll(page)
  await noViolations()
})

test('with Cloudflare delivery but no edge token, analytics says the edge is not counted', async ({
  page,
  request,
}) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  await page.route('**/v1/analytics?*', async (route) => {
    const res = await route.fetch()
    const body = await res.json()
    await route.fulfill({
      response: res,
      json: { ...body, collecting: { builtin: true, edge: 'not_connected' } },
    })
  })
  await go(page, 'Analytics')
  await expect(page.getByText('Playback through Cloudflare isn’t counted yet')).toBeVisible()
  await page.getByRole('link', { name: 'Connect edge analytics' }).click()
  await expect(page).toHaveURL(/\/delivery#edge-analytics$/)
  await noHorizontalScroll(page)
  await noViolations()
})

test('the edge analytics card asks for a read-only token and shows a refusal', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  // Built-in delivery: no card.
  await go(page, 'Delivery')
  await expect(page.getByText('In use').first()).toBeVisible()
  await expect(page.getByRole('region', { name: 'Edge analytics' })).toHaveCount(0)

  // As if Cloudflare delivery were on: the card offers its token field, and
  // the real API refuses the token because Cloudflare isn't connected here.
  await page.route('**/v1/cloudflare', async (route) => {
    const res = await route.fetch()
    const body = await res.json()
    await route.fulfill({
      response: res,
      json: {
        ...body,
        connected: true,
        storage_ok: true,
        hostname: 'media.example.com',
        bucket: 'clipmux-media',
      },
    })
  })
  await page.reload()
  const card = page.getByRole('region', { name: 'Edge analytics' })
  await expect(card).toContainText('never by address')
  await card.getByLabel('Read-only API token').fill('cf-analytics-read')
  await card.getByRole('button', { name: 'Connect edge analytics' }).click()
  await expect(card).toContainText('connect Cloudflare delivery first')
  await noHorizontalScroll(page)
  await noViolations()
})
