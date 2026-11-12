import { expect, type Page, test } from '@playwright/test'
import { expectSignedIn, go, noHorizontalScroll, watchCSP } from './csp.ts'

// The Encoding and Jobs pages (D047, D053–D058), against the real serve the
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
  await page.getByLabel('Password', { exact: true }).fill(cfg.ops_password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expectSignedIn(page)
}

test('the owner routes jobs, connects RunPod and sees why each job went where it did', async ({
  page,
  request,
}) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as Config
  await signIn(page, cfg)
  // The old Workers address lands on Encoding.
  await page.goto(`${cfg.ops}/workers`)
  await expect(page.getByRole('heading', { name: 'Encoding', level: 1 })).toBeVisible()
  await noHorizontalScroll(page)

  // The connected PC, its encoders, and its scope.
  const pc = studio(page)
  await expect(pc).toContainText('Online')
  await expect(pc).toContainText('h264_vaapi, libx264')
  await expect(pc).toContainText('Sandboxed')
  await pc.getByRole('combobox').selectOption('local')
  await page.reload()
  await expect(studio(page).getByRole('combobox')).toHaveValue('local')

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
  await page.locator('[data-provider="runpod"]').getByRole('button', { name: 'Connect RunPod' }).click()
  const form = page.getByRole('form', { name: 'Connect RunPod' })
  await form.getByLabel('RunPod API key').fill(cfg.runpod_key)
  // RunPod's NVENC GPUs on Secure Cloud, priced from its catalog, the
  // recommended one first and the first choices ticked (D111).
  const gpus = form.getByRole('list', { name: 'GPUs' })
  await expect(gpus.getByRole('listitem')).toHaveCount(2)
  await expect(gpus.getByRole('listitem').first()).toContainText('L4')
  await expect(gpus.getByRole('listitem').first()).toContainText('Recommended')
  await expect(gpus.getByRole('listitem').first()).toContainText('$0.43/hr')
  await expect(form.getByText('One machine at a time, at most $50.00 a month.')).toBeVisible()
  await form.getByRole('button', { name: 'Advanced' }).click()
  await form.getByLabel('Registry username').fill(cfg.registry_username)
  await form.getByLabel('Registry token').fill(cfg.registry_password)
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
  await page.keyboard.press('Escape')
  const runpod = page.locator('[data-provider="runpod"]')
  await expect(runpod).toContainText('Setup test pending')
  await expect(runpod).toContainText('$0.00 of $50.00')
  await expect(runpod).toContainText('$0.43 per machine-hour')
  // The API key never comes back to the page.
  await expect(page.locator('body')).not.toContainText(cfg.runpod_key)

  // The setup test's sample is routed to RunPod alone, and a machine is
  // starting for it.
  await go(page, 'Jobs')
  await expect(page.getByText('Routed to RunPod: a setup test through this executor')).toBeVisible({
    timeout: 30_000,
  })
  await expect(runpodRow(page)).toBeVisible()

  // Revoking the PC asks first, in a dialog, then marks it removed.
  await go(page, 'Encoding')
  await studio(page).getByRole('button', { name: 'Revoke' }).click()
  const ask = page.getByRole('alertdialog')
  await expect(ask).toContainText('Remove studio-pc?')
  await ask.getByRole('button', { name: 'Cancel' }).click()
  await expect(studio(page)).toContainText('Online')
  await studio(page).getByRole('button', { name: 'Revoke' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Remove PC' }).click()
  await expect(studio(page)).toContainText('Removed')

  // Back to the default for whatever runs next.
  await page.getByLabel('Primary executor', { exact: true }).selectOption('workers')
  await page.getByLabel('Fallback executor', { exact: true }).selectOption('')
  await page.getByLabel('Queue-age fallback', { exact: true }).uncheck()
  await page.getByRole('button', { name: 'Save routing' }).click()
  await expect(page.getByText('Saved.')).toBeVisible()
  await noViolations()
})

// The sample's job: queued for RunPod, not claimed yet.
const runpodRow = (page: Page) =>
  page
    .getByRole('list', { name: 'Jobs' })
    .getByRole('listitem')
    .filter({ hasText: 'Routed to RunPod' })
    .filter({ hasText: 'Queued' })

const studio = (page: Page) => page.getByRole('listitem', { name: 'studio-pc' })
