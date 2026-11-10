import { expect, test } from '@playwright/test'
import { dismissToast, go, noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// Delivery & storage (P6-T12, T13): built-in delivery, and a generic S3
// bucket added (checked before it's saved), its keys rotated, and removed.
interface Config extends OpsConfig {
  s3: { endpoint: string; bucket: string; access_key_id: string; secret_access_key: string } | null
}

test('the owner adds S3 storage, rotates its keys and removes it', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as Config
  await signIn(page, cfg)
  await go(page, 'Delivery')
  await expect(page.getByText('In use').first()).toBeVisible()
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
