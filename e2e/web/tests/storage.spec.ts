import { expect, test } from '@playwright/test'
import { dismissToast, go, noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// Storage (P6-T13, D110): where uploads go. A generic S3 bucket is checked
// before it's saved, takes new uploads, hands them back to this server's
// disk, has its keys rotated and is removed. An R2 bucket comes from a
// pasted Cloudflare token and a bucket picked from what it reaches.
interface Config extends OpsConfig {
  s3: { endpoint: string; bucket: string; access_key_id: string; secret_access_key: string } | null
}

test('the owner sends uploads to S3, back to disk, and removes the bucket', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as Config
  await signIn(page, cfg)
  await go(page, 'Storage')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Storage')
  await expect(page.getByText("New uploads go to This server's disk.")).toBeVisible()
  const storage = page.getByRole('list', { name: 'Storage' })
  await expect(storage.locator('[data-storage="fs"]')).toContainText('Default')
  test.skip(!cfg.s3, 'no S3 test store (CLIPMUX_TEST_S3_ENDPOINT)')
  const s3 = cfg.s3 as NonNullable<Config['s3']>

  // Wrong keys fail the check and nothing is saved.
  await page.getByRole('button', { name: 'Add S3 storage' }).click()
  const form = page.getByRole('form', { name: 'Add S3 storage' })
  await form.getByLabel('Endpoint', { exact: true }).fill(s3.endpoint)
  await form.getByLabel('Bucket').fill(s3.bucket)
  await form.getByLabel('Access key ID').fill(s3.access_key_id)
  await form.getByLabel('Secret access key').fill('wrong-secret')
  await form.getByRole('button', { name: 'Check and add' }).click()
  await expect(form.getByRole('alert')).toBeVisible({ timeout: 30_000 })
  await form.getByLabel('Secret access key').fill(s3.secret_access_key)
  await form.getByRole('button', { name: 'Check and add' }).click()
  const added = storage.locator('[data-storage="s3"]').filter({ hasText: s3.bucket })
  await expect(added).toContainText('0 videos', { timeout: 30_000 })
  // On a phone the toast sits over the list; it closes like any other.
  await dismissToast(page, 'Storage added')
  await noHorizontalScroll(page)

  // New uploads go to the bucket, then back to this server's disk.
  await added.getByRole('button', { name: 'Use for new uploads' }).click()
  await expect(added).toContainText('Default')
  await expect(page.getByText(`New uploads go to S3 (${s3.bucket}).`)).toBeVisible()
  await dismissToast(page, `New uploads go to S3 (${s3.bucket})`)
  const disk = storage.locator('[data-storage="fs"]')
  await disk.getByRole('button', { name: 'Use for new uploads' }).click()
  await expect(disk).toContainText('Default')
  await expect(page.getByText("New uploads go to This server's disk.")).toBeVisible()
  await dismissToast(page, "New uploads go to This server's disk")

  // Rotating checks the new keys before swapping them in.
  await added.getByRole('button', { name: 'Rotate keys' }).click()
  await page.getByLabel('New access key ID').fill(s3.access_key_id)
  await page.getByLabel('New secret access key').fill(s3.secret_access_key)
  await page.getByRole('button', { name: 'Check and rotate' }).click()
  await expect(page.getByText('Keys rotated')).toBeVisible({ timeout: 30_000 })
  await dismissToast(page, 'Keys rotated')

  // Removing asks first.
  await added.getByRole('button', { name: 'Remove' }).click()
  await expect(page.getByRole('alertdialog')).toContainText(`Remove ${s3.bucket}?`)
  await page.getByRole('alertdialog').getByRole('button', { name: 'Remove storage' }).click()
  await expect(added).toHaveCount(0)
  await noViolations()
})

test('an R2 bucket comes from a pasted token and a bucket it reaches', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  // The fixture has no Cloudflare: the token check and the add answer as
  // Cloudflare would, and the test reads what the dashboard sent.
  const sent: Record<string, unknown>[] = []
  await page.route('**/v1/cloudflare/inspect', (route) =>
    route.fulfill({
      json: {
        accounts: [{ id: 'acct1', name: 'Lessons Ltd' }],
        account_id: 'acct1',
        buckets: ['lessons-archive'],
        workers_paid: false,
        unreadable: ['zones', 'billing'],
      },
    }),
  )
  await page.route('**/v1/storage/r2', (route) => {
    sent.push(route.request().postDataJSON())
    return route.fulfill({
      json: {
        connected: false,
        steps: [
          { name: 'token', ok: true, detail: 'active' },
          { name: 'bucket', ok: false, detail: 'cloudflare create bucket: HTTP 403' },
        ],
      },
    })
  })
  await go(page, 'Storage')
  await page.getByRole('button', { name: 'Add R2 bucket' }).click()
  const form = page.getByRole('form', { name: 'Add R2 bucket' })
  const link = form.getByRole('link', { name: 'Create token on Cloudflare' })
  await expect(link).toHaveAttribute('href', /^https:\/\/dash\.cloudflare\.com\/\?.*workers_r2/)
  await expect(link).toHaveAttribute('target', '_blank')
  await form.getByLabel('API token').fill('cf-storage-token')
  const bucket = form.getByLabel('Bucket')
  await expect(bucket).toHaveValue('clipmux-media')
  await expect(bucket.locator('option')).toHaveText(['clipmux-media (new)', 'lessons-archive'])
  await bucket.selectOption('lessons-archive')
  await form.getByRole('button', { name: 'Check and add' }).click()
  await expect(form.getByText('cloudflare create bucket: HTTP 403')).toBeVisible()
  expect(sent).toEqual([
    { token: 'cf-storage-token', bucket: 'lessons-archive', account_id: 'acct1', default: true },
  ])
  await noHorizontalScroll(page)
  await noViolations()
})
