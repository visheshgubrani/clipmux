import { expect, test } from '@playwright/test'
import { go, noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// Analytics (D100): until its API exists the page says it isn't collecting,
// on the page and on a video's tab, and never shows sample numbers.
test('analytics says it is not collecting yet', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  await go(page, 'Analytics')
  await expect(page.getByText("Analytics isn't collecting yet")).toBeVisible()
  await page.getByRole('button', { name: '30 days' }).click()
  await expect(page.getByText("Analytics isn't collecting yet")).toBeVisible()
  await expect(page.getByText("Couldn't load")).toHaveCount(0)
  await page.goto(`${cfg.ops}/videos/${cfg.asset_id}?app=${cfg.app_id}&tab=analytics`)
  await expect(page.getByText("Analytics isn't collecting yet")).toBeVisible()
  await noHorizontalScroll(page)
  await noViolations()
})
