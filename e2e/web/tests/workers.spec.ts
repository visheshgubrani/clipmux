import { expect, type Page, test } from '@playwright/test'
import { expectSignedIn, go, noHorizontalScroll, watchCSP } from './csp.ts'

// The Encoding and Jobs pages (D116), against the real serve the fixture
// boots: its owner signs in, finds this server encoding, connects RunPod (a
// fake API and a fake private registry), watches it being checked, chooses
// it once the fixture stands in for its sample, and sees on the Jobs page
// where each job is routed and why. Puts this server back at the end.

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

test('the owner connects RunPod, chooses it once checked, and sees why each job went where it did', async ({
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

  // One choice: this server, or a cloud GPU once it's connected and checked.
  const where = page.getByRole('radiogroup', { name: 'Where videos are encoded' })
  await expect(where.getByRole('radio', { name: 'This server' })).toBeChecked()
  await expect(where.getByRole('radio', { name: 'RunPod' })).toBeDisabled()
  await expect(where.getByRole('radio', { name: 'Modal' })).toBeDisabled()
  await expect(page.getByText(/Connect a PC|Your computers/)).toHaveCount(0)

  // Connect RunPod: every step passes, the image is pinned by digest, and
  // a sample goes to RunPod first.
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
  await expect(runpod).toContainText('Checking')
  await expect(runpod).toContainText('A sample video is encoding on RunPod')
  await expect(runpod).toContainText('$0.00 of $50.00')
  await expect(runpod).toContainText('$0.43 per machine-hour')
  await expect(where.getByRole('radio', { name: 'RunPod' })).toBeDisabled()
  // The API key never comes back to the page.
  await expect(page.locator('body')).not.toContainText(cfg.runpod_key)

  // The setup test's sample is routed to RunPod alone, and a machine is
  // starting for it.
  await go(page, 'Jobs')
  await expect(page.getByText('Routed to RunPod: a setup test through this executor')).toBeVisible({
    timeout: 30_000,
  })
  await expect(runpodRow(page)).toBeVisible()

  // The fixture stands in for the sample encoding on a pod: RunPod is
  // ready, and choosing it puts this server behind it.
  expect((await request.post('/verify-runpod')).status()).toBe(204)
  await go(page, 'Encoding')
  await page.reload()
  await expect(runpod).toContainText('Ready')
  await where.getByRole('radio', { name: 'RunPod' }).click()
  await expect(where.getByRole('radio', { name: 'RunPod' })).toBeChecked()
  const fallback = page.getByRole('checkbox', {
    name: 'If RunPod can’t start a machine, encode on this server instead',
  })
  await expect(fallback).toBeChecked()
  await fallback.click()
  await expect(fallback).not.toBeChecked()
  await page.reload()
  await expect(where.getByRole('radio', { name: 'RunPod' })).toBeChecked()
  await expect(fallback).not.toBeChecked()
  await go(page, 'Home')
  await expect(page.locator('footer')).toContainText('Encoding: RunPod')

  // Back to this server for whatever runs next.
  await go(page, 'Encoding')
  await where.getByRole('radio', { name: 'This server' }).click()
  await expect(where.getByRole('radio', { name: 'This server' })).toBeChecked()
  await expect(fallback).toHaveCount(0)
  await noViolations()
})

// The sample's job: queued for RunPod, not claimed yet.
const runpodRow = (page: Page) =>
  page
    .getByRole('list', { name: 'Jobs' })
    .getByRole('listitem')
    .filter({ hasText: 'Routed to RunPod' })
    .filter({ hasText: 'Queued' })
