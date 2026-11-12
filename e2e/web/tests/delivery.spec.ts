import { expect, test } from '@playwright/test'
import { go, noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// Delivery (P6-T12, D109): built in until Cloudflare is connected. Connect
// starts from a pre-filled create-token link; the pasted token is inspected
// and the owner picks the account, the hostname under one of its zones and
// the bucket, instead of typing IDs.
test('connecting Cloudflare picks the hostname and bucket from the token', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  // The fixture has no Cloudflare: inspect and connect answer as Cloudflare
  // would, and the test reads what the dashboard sent.
  const inspected: Record<string, unknown>[] = []
  const sent: Record<string, unknown>[] = []
  await page.route('**/v1/cloudflare/inspect', (route) => {
    const body = route.request().postDataJSON()
    inspected.push(body)
    return route.fulfill({
      json: body.account_id
        ? {
            accounts: [
              { id: 'acct1', name: 'Lessons Ltd' },
              { id: 'acct2', name: 'Personal' },
            ],
            account_id: body.account_id,
            zones: ['example.com', 'lessons.dev'],
            buckets: ['clipmux-media', 'old-videos'],
            workers_paid: true,
          }
        : {
            accounts: [
              { id: 'acct1', name: 'Lessons Ltd' },
              { id: 'acct2', name: 'Personal' },
            ],
            workers_paid: false,
          },
    })
  })
  await page.route('**/v1/cloudflare/connect', (route) => {
    sent.push(route.request().postDataJSON())
    return route.fulfill({
      json: {
        connected: false,
        steps: [
          { name: 'token', ok: true, detail: 'active' },
          { name: 'zone', ok: false, detail: 'stop here' },
        ],
      },
    })
  })
  await go(page, 'Delivery')
  await expect(page.getByText('In use').first()).toBeVisible()
  await page.getByRole('button', { name: 'Connect Cloudflare' }).click()
  const dialog = page.getByRole('dialog', { name: 'Connect Cloudflare' })
  const link = dialog.getByRole('link', { name: 'Create token on Cloudflare' })
  await expect(link).toHaveAttribute('href', /^https:\/\/dash\.cloudflare\.com\/\?.*workers_scripts/)
  await expect(dialog.getByRole('button', { name: 'Connect Cloudflare' })).toBeDisabled()

  await dialog.getByLabel('API token').fill('cf-connect-token')
  await dialog.getByLabel('Account').selectOption('acct1')
  await expect(dialog.getByLabel('Zone')).toHaveValue('example.com')
  await expect(dialog.getByLabel('Subdomain')).toHaveValue('media')
  await dialog.getByLabel('Zone').selectOption('lessons.dev')
  await dialog.getByLabel('Subdomain').fill('video')
  await expect(dialog).toContainText('https://video.lessons.dev')
  await expect(dialog.getByLabel('R2 bucket')).toHaveValue('clipmux-media')
  await expect(dialog.getByRole('checkbox', { name: 'Store new uploads in R2' })).toBeChecked()
  await dialog.getByRole('checkbox', { name: 'Store new uploads in R2' }).uncheck()
  await dialog.getByRole('button', { name: 'Connect Cloudflare' }).click()
  await expect(dialog.getByText('stop here')).toBeVisible()

  expect(inspected.at(-1)).toEqual({ token: 'cf-connect-token', account_id: 'acct1' })
  expect(sent).toEqual([
    {
      token: 'cf-connect-token',
      hostname: 'video.lessons.dev',
      bucket: 'clipmux-media',
      account_id: 'acct1',
      default_storage: false,
    },
  ])
  await noHorizontalScroll(page)
  await noViolations()
})
