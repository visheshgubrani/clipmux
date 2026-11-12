import { readFileSync, statSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { expect, test } from '@playwright/test'
import { expectSignedIn, go } from './csp.ts'

// These tests mutate only the dedicated retained staging install, and run
// before playback/uploader so those exercise the real Worker and R2.
test('connects Cloudflare twice and passes the setup test at the edge', async ({ page }) => {
  test.setTimeout(600_000)
  const path = process.env.CLIPMUX_E2E_LIVE_CONFIG
  if (!path) throw new Error('live config required')
  const state = dirname(resolve(path))
  const tokenPath = resolve(state, 'cloudflare-token')
  for (const file of [path, tokenPath, resolve(state, 'owner.json')]) {
    if (statSync(file).mode & 0o077) throw new Error('staging files must have permissions 0600')
  }
  const cfg = JSON.parse(readFileSync(path, 'utf8'))
  const owner = JSON.parse(readFileSync(resolve(state, 'owner.json'), 'utf8'))
  await page.goto(`${cfg.api}/login`)
  await page.getByLabel('Email').fill(owner.email)
  await page.getByLabel('Password', { exact: true }).fill(owner.password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expectSignedIn(page)
  await go(page, 'Delivery')
  for (let pass = 0; pass < 2; pass++) {
    // The Delivery card opens Connect in a dialog.
    await page.getByRole('button', { name: /^(Connect Cloudflare|Connect again)$/ }).click()
    // The pasted token is inspected (D109); the account, zone and bucket
    // are picked from what it reaches.
    const dialog = page.getByRole('dialog')
    await dialog.getByLabel('API token').fill(readFileSync(tokenPath, 'utf8').trim())
    const account = dialog.getByLabel('Account')
    const zone = dialog.getByLabel('Zone')
    await expect(account.or(zone).first()).toBeVisible({ timeout: 30_000 })
    if (await account.isVisible()) {
      await account.selectOption('49b962322ece3363f6495da349dd9d9e')
    }
    await dialog.getByLabel('Zone').selectOption('clipmux.com', { timeout: 30_000 })
    await dialog.getByLabel('Subdomain').fill('media-test')
    await dialog.getByLabel('R2 bucket').selectOption('clipmux-staging')
    const responsePromise = page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' &&
        new URL(response.url()).pathname === '/v1/cloudflare/connect',
      { timeout: 180_000 },
    )
    await dialog.getByRole('button', { name: /^(Connect Cloudflare|Connect again)$/ }).click()
    const response = await responsePromise
    const result = await response.json()
    expect(response.status()).toBe(200)
    expect(
      result.connected,
      JSON.stringify(
        result.steps?.map((step: { name: string; ok: boolean }) => ({ name: step.name, ok: step.ok })),
      ),
    ).toBe(true)
    await expect(dialog.getByRole('button', { name: 'Connect again', exact: true })).toBeEnabled({
      timeout: 180_000,
    })
    await page.keyboard.press('Escape')
  }
  await expect
    .poll(
      async () => {
        const response = await page.request.get(`${cfg.api}/v1/setup-test/latest`)
        const body = await response.json()
        return body.status
      },
      { timeout: 300_000 },
    )
    .toBe('passed')
})
