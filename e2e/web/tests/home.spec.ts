import { expect, test } from '@playwright/test'
import { go, noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// Home (docs/design.md): what's left to set up, as rows; the newest videos;
// a quiet status line. No number tiles.
test('home lists the optional steps and the newest videos', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  await go(page, 'Home')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText(/^Good (morning|afternoon|evening)$/)
  const setup = page.getByRole('region', { name: 'Finish setting up' })
  // The fixture's PC is connected and an LLM is: those steps show done.
  await expect(setup.getByRole('link', { name: /Connect a PC/ }).getByLabel('Done')).toBeVisible()
  await expect(setup.getByRole('link', { name: /Turn on chapters/ }).getByLabel('Done')).toBeVisible()
  await expect(setup.getByRole('link', { name: /Connect Cloudflare/ }).getByLabel('Not done')).toBeVisible()
  const recent = page.getByRole('region', { name: 'Recent videos' })
  await expect(recent.getByRole('link').first()).toBeVisible()
  await expect(page.locator('footer')).toContainText('Delivery: built in')
  await noHorizontalScroll(page)

  // Hiding the steps sticks.
  await setup.getByRole('button', { name: 'Hide' }).click()
  await page.reload()
  await expect(page.getByRole('region', { name: 'Recent videos' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Finish setting up' })).toHaveCount(0)
  await page.evaluate(() => localStorage.removeItem('clipmux-hide-setup'))
  await noViolations()
})
