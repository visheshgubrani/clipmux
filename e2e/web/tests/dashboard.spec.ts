import { expect, test } from '@playwright/test'
import { watchCSP } from './csp.ts'

// First boot in the dashboard (docs/architecture.md, "First boot"): the
// owner registers with the setup token, sets the URLs, and the setup test
// puts the sample through the whole path; then the sample plays.
test('first visit registers, saves the URLs and plays the sample', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = await (await request.get('/config')).json()
  await page.goto(cfg.dashboard)
  await page.getByLabel('Setup token').fill(cfg.setup_token)
  await page.getByLabel('Email').fill('owner@example.com')
  await page.getByLabel(/^Password/).fill('correct horse battery staple')
  await page.getByRole('button', { name: 'Create owner' }).click()

  // The public URL is prefilled with where the owner opened the dashboard.
  await expect(page.getByLabel('Public URL', { exact: true })).toHaveValue(cfg.dashboard)
  await page.getByRole('button', { name: 'Save and test' }).click()
  await expect(page.getByText('✓ stored_segment_encrypted')).toBeVisible({ timeout: 180_000 })
  // The owner presses play on the sample in the built-in player.
  await page.locator('clipmux-player').hover()
  await page.locator('clipmux-player').getByRole('button', { name: /play/i }).first().click()
  await expect
    .poll(
      () =>
        page.evaluate(
          () =>
            (document.querySelector('hlsjs-video') as unknown as HTMLVideoElement | null)?.currentTime ?? 0,
        ),
      {
        timeout: 60_000,
      },
    )
    .toBeGreaterThan(2)

  // The library lists the sample; apps can't get API keys on a loopback URL.
  await page.getByRole('navigation').getByRole('link', { name: 'Library' }).click()
  await expect(page.getByText('clipmux-setup-test').first()).toBeVisible()
  await page.getByRole('navigation').getByRole('link', { name: 'Apps' }).click()
  await page.getByRole('link', { name: 'Dashboard' }).click()
  await expect(page.getByRole('button', { name: 'Create API key' })).toBeDisabled()
  await noViolations()
})
