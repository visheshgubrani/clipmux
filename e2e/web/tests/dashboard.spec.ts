import { expect, test } from '@playwright/test'
import { expectSignedIn, go, noHorizontalScroll, watchCSP } from './csp.ts'

// First boot in the dashboard (docs/architecture.md, "First boot"): the
// owner creates the account with the setup token, sets the URLs, and the
// setup test puts the sample through the whole path; then the sample plays,
// and Home lists what's left to set up.
test('first visit creates the account, saves the URLs and plays the sample', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = await (await request.get('/config')).json()
  await page.goto(cfg.dashboard)
  await expect(page.getByRole('heading', { name: 'Create your account' })).toBeVisible()
  await noHorizontalScroll(page)
  await page.getByLabel('Setup token').fill(cfg.setup_token)
  await page.getByLabel('Email').fill('owner@example.com')
  await page.getByLabel('Password', { exact: true }).fill('correct horse battery staple')
  await page.getByRole('button', { name: 'Create account' }).click()

  // The public URL is prefilled with where the owner opened the dashboard,
  // and nothing has been tested against the placeholder default yet.
  await expect(page.getByLabel('Public URL', { exact: true })).toHaveValue(cfg.dashboard)
  await expect(page.getByText(/first.boot/)).toHaveCount(0)
  await noHorizontalScroll(page)
  await page.getByRole('button', { name: 'Save and test' }).click()
  await expect(page.getByText(/All \d+ checks passed/)).toBeVisible({ timeout: 180_000 })
  await page.getByRole('button', { name: 'Details' }).click()
  await expect(page.getByText('✓ stored_segment_encrypted')).toBeVisible()
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
      { timeout: 60_000 },
    )
    .toBeGreaterThan(2)
  await noHorizontalScroll(page)

  // Home: the setup test is done, the optional steps are left, and the
  // sample is among the recent videos. No number tiles.
  await page.getByRole('link', { name: 'Go to your dashboard' }).click()
  await expectSignedIn(page)
  const setup = page.getByRole('region', { name: 'Finish setting up' })
  await expect(setup.getByRole('link', { name: /Run the setup test/ })).toContainText('Run the setup test')
  await expect(setup.getByRole('link', { name: /Connect Cloudflare/ })).toBeVisible()
  await expect(page.getByRole('link', { name: /clipmux-setup-test/ }).first()).toBeVisible()
  await noHorizontalScroll(page)

  // Videos lists the sample; on a loopback URL a key is offered, for apps on this computer.
  await go(page, 'Videos')
  await expect(page.getByRole('link', { name: /clipmux-setup-test/ }).first()).toBeVisible()
  await page.getByRole('button', { name: 'List view' }).click()
  await expect(page.getByRole('row', { name: /clipmux-setup-test/ }).first()).toContainText('Setup test')
  await go(page, 'Apps & API')
  await page.getByRole('link', { name: /Dashboard/ }).click()
  await page.getByRole('button', { name: 'Create API key' }).click()
  await expect(page.getByRole('dialog', { name: 'Create API key' })).toContainText(
    'This key is for apps on this computer',
  )
  await page.keyboard.press('Escape')
  await noHorizontalScroll(page)
  await noViolations()
})
