import { expect, test } from '@playwright/test'
import { expectSignedOut, go, noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// Settings (P6-T12, T13): the URLs out of the first-run flow, and changing
// the password, which keeps this browser signed in and ends the others.
test('the owner changes the password and signs in with it, then changes it back', async ({
  browser,
  page,
  request,
}) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  // A second browser, signed in before the change.
  const other = await browser.newContext({ viewport: test.info().project.use.viewport ?? null })
  const otherPage = await other.newPage()
  await signIn(otherPage, cfg)

  await go(page, 'Settings')
  await expect(page.getByLabel('Public URL', { exact: true })).toHaveValue(cfg.ops)
  await expect(page.getByRole('region', { name: 'Setup test' })).toBeVisible()
  await page.getByRole('tab', { name: 'Account' }).click()
  await noHorizontalScroll(page)

  // A wrong current password is refused, and says so.
  const next = 'another long test password'
  await page.getByLabel('Current password').fill('not the password at all')
  await page.getByLabel('New password').fill(next)
  await page.getByRole('button', { name: 'Change password' }).click()
  await expect(page.getByText("The current password isn't right.")).toBeVisible()

  await page.getByLabel('Current password').fill(cfg.ops_password)
  await page.getByLabel('New password').fill(next)
  await page.getByRole('button', { name: 'Change password' }).click()
  await expect(page.getByText('Password changed')).toBeVisible()

  // The other browser was signed out; this one stays.
  await otherPage.reload()
  await expectSignedOut(otherPage)
  await other.close()
  await page.reload()
  await expect(page.getByRole('tab', { name: 'Account', selected: true })).toBeVisible()
  // The new password signs in a fresh browser.
  const fresh = await browser.newContext({ viewport: test.info().project.use.viewport ?? null })
  await signIn(await fresh.newPage(), cfg, next)
  await fresh.close()

  // Back to the fixture's password for the suites after this one.
  await page.goto(`${cfg.ops}/settings?tab=account`)
  await page.getByLabel('Current password').fill(next)
  await page.getByLabel('New password').fill(cfg.ops_password)
  await page.getByRole('button', { name: 'Change password' }).click()
  await expect(page.getByText('Password changed')).toBeVisible()
  await noViolations()
})
