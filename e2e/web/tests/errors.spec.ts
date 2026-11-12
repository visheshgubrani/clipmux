import { expect, test } from '@playwright/test'
import { type OpsConfig, signIn, watchCSP } from './csp.ts'

// P6 exit check 6: a load that fails at the API shows what failed and Try
// again, never an endless "Loading…"; Try again recovers once the API does.
const pages: [string, string, RegExp][] = [
  ['/', 'your videos', /\/v1\/library/],
  ['/videos', 'your videos', /\/v1\/library\?/],
  ['/jobs', 'jobs', /\/v1\/jobs\?/],
  ['/encoding', 'encoding', /\/v1\/settings\/routing/],
  ['/delivery', 'delivery', /\/v1\/cloudflare$/],
  ['/captions', 'the speech settings', /\/v1\/settings\/speech$/],
  ['/apps', 'your apps', /\/v1\/apps$/],
]

test('every page shows an error with Try again when its API fails, and recovers', async ({
  page,
  request,
}) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  for (const [path, what, url] of pages) {
    let failing = true
    await page.route(url, (route) =>
      failing
        ? route.fulfill({
            status: 503,
            contentType: 'application/json',
            body: JSON.stringify({ error: { code: 'unavailable', message: 'database unavailable' } }),
          })
        : route.fallback(),
    )
    await page.goto(`${cfg.ops}${path}`)
    const error = page.getByText(`Couldn't load ${what}`).first()
    await expect(error, path).toBeVisible({ timeout: 20_000 })
    await expect(page.getByText('database unavailable').first()).toBeVisible()
    failing = false
    await page.getByRole('button', { name: 'Try again' }).first().click()
    await expect(error, `${path} recovers`).toHaveCount(0)
    await page.unroute(url)
  }
  await noViolations()
})
