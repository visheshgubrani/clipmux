import { expect, test } from '@playwright/test'
import { go, noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// Apps & API (D025, D069): an app with its signing key and webhook, each
// secret shown once; a webhook's deliveries and two-step secret rotation;
// every destructive action asks first.
test('the owner creates an app, its keys and a webhook, and disables it', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  await go(page, 'Apps & API')

  const name = `Course site ${test.info().project.name}`
  await page.getByRole('button', { name: 'Create app' }).click()
  const create = page.getByRole('dialog', { name: 'Create app' })
  await create.getByLabel('Name').fill(name)
  await create.getByRole('button', { name: 'Create app' }).click()
  await page.getByRole('link', { name: new RegExp(name) }).click()
  await expect(page.getByRole('heading', { name, level: 1 })).toBeVisible()
  await noHorizontalScroll(page)

  // The fixture's public URL is loopback: a key needs the owner to confirm
  // it's for apps on this computer, and its secret is shown once (D108).
  await page.getByRole('button', { name: 'Create API key' }).click()
  const naming = page.getByRole('dialog', { name: 'Create API key' })
  await naming.getByLabel('Name').fill('Laptop')
  await expect(naming.getByRole('button', { name: 'Create key' })).toBeDisabled()
  await naming.getByRole('checkbox', { name: 'This key is for apps on this computer' }).check()
  await naming.getByRole('button', { name: 'Create key' }).click()
  const apiSecret = page.getByRole('dialog', { name: 'API key "Laptop"' })
  await expect(apiSecret.getByRole('textbox', { name: 'API key' })).toHaveValue(/^cmx_/)
  await apiSecret.getByRole('button', { name: "I've saved it" }).click()
  await expect(page.getByRole('row', { name: /Laptop/ })).toBeVisible()

  // A signing key's private half is shown once.
  await page.getByRole('tab', { name: 'Signing keys' }).click()
  await page.getByRole('button', { name: 'Create signing key' }).click()
  const secret = page.getByRole('dialog', { name: /^Signing key/ })
  await expect(secret).toContainText("It's shown once")
  await expect(secret).toContainText('BEGIN PRIVATE KEY')
  await secret.getByRole('button', { name: "I've saved it" }).click()
  await expect(page.locator('body')).not.toContainText('BEGIN PRIVATE KEY')
  await expect(
    page.getByRole('region', { name: 'Signing keys' }).getByRole('button', { name: 'Revoke' }),
  ).toBeVisible()

  // A webhook: its secret once, its deliveries, and a rotation in two steps.
  await page.getByRole('tab', { name: 'Webhooks' }).click()
  await expect(page.getByText('No webhooks yet')).toBeVisible()
  await page.getByRole('button', { name: 'Add endpoint' }).click()
  const add = page.getByRole('dialog', { name: 'Add endpoint' })
  await add.getByLabel('URL').fill('https://app.example.test/hooks/clipmux')
  await add.getByRole('button', { name: 'Add endpoint' }).click()
  const hookSecret = page.getByRole('dialog', { name: 'Webhook signing secret' })
  await expect(hookSecret).toContainText("It's shown once")
  await hookSecret.getByRole('button', { name: "I've saved it" }).click()
  const hook = page.locator('[data-endpoint]')
  await expect(hook).toContainText('https://app.example.test/hooks/clipmux')
  await expect(hook).toContainText('Healthy')
  await hook.getByRole('button', { name: 'Deliveries' }).click()
  await expect(page.getByRole('dialog', { name: 'Deliveries' })).toContainText('Nothing sent yet.')
  await page.keyboard.press('Escape')
  await hook.getByRole('button', { name: 'Rotate secret' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Create secret' }).click()
  await page
    .getByRole('dialog', { name: 'New webhook secret' })
    .getByRole('button', { name: "I've saved it" })
    .click()
  await expect(hook).toContainText('A new secret is waiting')
  await hook.getByRole('button', { name: 'Activate new secret' }).click()
  await expect(hook.getByRole('button', { name: 'Rotate secret' })).toBeVisible()

  // Removing the endpoint asks first; cancelling keeps it.
  await hook.getByRole('button', { name: 'Remove' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Cancel' }).click()
  await expect(hook).toBeVisible()
  await hook.getByRole('button', { name: 'Remove' }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Remove endpoint' }).click()
  await expect(page.getByText('No webhooks yet')).toBeVisible()

  // Disabling the app asks first.
  await page.getByRole('tab', { name: 'Danger zone' }).click()
  await page.getByRole('button', { name: 'Disable' }).click()
  await expect(page.getByRole('alertdialog')).toContainText(`Disable ${name}?`)
  await page.getByRole('alertdialog').getByRole('button', { name: 'Disable app' }).click()
  await expect(page.getByText('Disabled', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Enable' }).click()
  await expect(page.getByText('Enabled', { exact: true })).toBeVisible()
  await noViolations()
})
