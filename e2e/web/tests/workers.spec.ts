import { expect, type Page, test } from '@playwright/test'
import { watchCSP } from './csp.ts'

// The Workers and Jobs pages (D047, D053–D058), against the real serve the
// fixture boots: its owner signs in, finds a connected PC, chooses routing,
// connects RunPod (a fake API and a fake private registry), and sees on the
// Jobs page where each job is routed and why. Runs after the other suites,
// and puts routing back, because the fixture's PC is not a real worker.

interface Config {
  ops: string
  ops_email: string
  ops_password: string
  runpod_key: string
  registry_username: string
  registry_password: string
}

async function signIn(page: Page, cfg: Config) {
  await page.goto(`${cfg.ops}/login`)
  await page.getByLabel('Email').fill(cfg.ops_email)
  await page.getByLabel('Password').fill(cfg.ops_password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('navigation')).toBeVisible()
}

test('the owner routes jobs, connects RunPod and sees why each job went where it did', async ({
  page,
  request,
}) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as Config
  await signIn(page, cfg)
  await page.goto(`${cfg.ops}/workers`)

  // The connected PC, its encoders, and its scope.
  const pc = page.getByRole('row', { name: /studio-pc/ })
  await expect(pc).toContainText('online')
  await expect(pc).toContainText('h264_vaapi, libx264')
  await expect(pc).toContainText('sandboxed')
  await pc.getByRole('combobox').selectOption('local')
  await page.reload()
  await expect(page.getByRole('row', { name: /studio-pc/ }).getByRole('combobox')).toHaveValue('local')

  // "My PC, else this server", after 5 minutes in the queue.
  await page.getByLabel('Primary executor', { exact: true }).selectOption('agents')
  await page.getByLabel('Fallback executor', { exact: true }).selectOption('builtin')
  await page.getByLabel('Queue-age fallback', { exact: true }).check()
  await page.getByLabel('Queue-age minutes').fill('5')
  await page.getByRole('button', { name: 'Save routing' }).click()
  await expect(page.getByText('Saved.')).toBeVisible()
  await page.reload()
  await expect(page.getByLabel('Primary executor', { exact: true })).toHaveValue('agents')
  await expect(page.getByLabel('Fallback executor', { exact: true })).toHaveValue('builtin')
  await expect(page.getByLabel('Queue-age minutes')).toHaveValue('5')
  // RunPod isn't a choice until it is connected and verified.
  await expect(
    page.getByLabel('Fallback executor', { exact: true }).locator('option[value="runpod"]'),
  ).toHaveCount(0)

  // Connect RunPod: every step passes, the image is pinned by digest, and it
  // waits for its setup test.
  await page.getByText('Connect RunPod', { exact: true }).first().click()
  const form = page.getByRole('form', { name: 'Connect RunPod' })
  await form.getByLabel('RunPod API key').fill(cfg.runpod_key)
  await form.getByPlaceholder('Registry username (private image)').fill(cfg.registry_username)
  await form.getByPlaceholder('Registry token with read:packages').fill(cfg.registry_password)
  await form.getByLabel('At most this many machines').fill('2')
  await form.getByLabel('Monthly limit ($)').fill('50')
  await form.getByLabel('Rate ($ per machine-hour)').fill('0.45')
  await form.getByRole('button', { name: 'Connect RunPod' }).click()
  for (const step of [
    'Credentials',
    'Public URL reachable',
    'Worker image',
    'Registry access',
    'Saved',
    'Setup test',
  ]) {
    await expect(page.getByText(`✓ ${step}`)).toBeVisible()
  }
  await expect(page.getByText(/pinned to sha256:[0-9a-f]{64}/)).toBeVisible()
  const runpod = page.locator('[data-provider="runpod"]')
  await expect(runpod).toContainText('waiting for its setup test to pass')
  await expect(runpod).toContainText('of $50.00 this month')
  await expect(runpod).toContainText('$0.45 per machine-hour')
  // The API key never comes back to the page.
  await expect(page.locator('body')).not.toContainText(cfg.runpod_key)

  // The setup test's sample is routed to RunPod alone, and a machine is
  // starting for it.
  await page.getByRole('navigation').getByRole('link', { name: 'Jobs' }).click()
  await expect(page.getByText('Routed to RunPod: a setup test through this executor')).toBeVisible({
    timeout: 30_000,
  })
  await expect(runpodRow(page)).toBeVisible()

  // Revoking the PC asks first, then marks it revoked.
  await page.getByRole('navigation').getByRole('link', { name: 'Workers' }).click()
  page.once('dialog', (d) => void d.accept())
  await page
    .getByRole('row', { name: /studio-pc/ })
    .getByRole('button', { name: 'Revoke' })
    .click()
  await expect(page.getByRole('row', { name: /studio-pc/ })).toContainText('revoked')

  // Back to the default for whatever runs next.
  await page.getByLabel('Primary executor', { exact: true }).selectOption('workers')
  await page.getByLabel('Fallback executor', { exact: true }).selectOption('')
  await page.getByLabel('Queue-age fallback', { exact: true }).uncheck()
  await page.getByRole('button', { name: 'Save routing' }).click()
  await expect(page.getByText('Saved.')).toBeVisible()
  await noViolations()
})

// The sample's job: queued, not claimed yet.
const runpodRow = (page: Page) =>
  page.getByRole('row').filter({ hasText: 'Routed to RunPod' }).filter({ hasText: 'not claimed yet' })
