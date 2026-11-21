import { expect, test } from '@playwright/test'
import { go, noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// Connecting a cloud GPU by approving in the browser (D109): the limits
// are set first, the provider's page opens in a tab that can't reach back
// to the dashboard, and the dashboard polls until the provider's key has
// connected. The fixture can't reach Modal, so the approval routes answer
// as the server would; TestConnectRunPodByApprovingInTheBrowser covers the
// server against a fake provider.
test('the owner connects Modal by approving it on Modal’s page', async ({ page, request, baseURL }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  const started: Record<string, unknown>[] = []
  let polls = 0
  await page
    .context()
    .route('**/approve-here**', (route) =>
      route.fulfill({ contentType: 'text/html', body: '<title>Modal</title><p>Approve ClipMux?</p>' }),
    )
  await page.route('**/v1/executors/modal/authorize', (route) => {
    started.push(route.request().postDataJSON())
    return route.fulfill({
      status: 201,
      json: {
        id: 'apr_test',
        url: `${baseURL}/approve-here?flow=tf-1`,
        code: 'AB12',
        expires_at: new Date(Date.now() + 600_000).toISOString(),
        status: 'pending',
      },
    })
  })
  await page.route('**/v1/executors/modal/authorize/apr_test', (route) => {
    polls++
    return route.fulfill({
      json:
        polls < 3
          ? { id: 'apr_test', expires_at: new Date().toISOString(), status: 'pending' }
          : {
              id: 'apr_test',
              expires_at: new Date().toISOString(),
              status: 'approved',
              result: {
                connected: true,
                steps: [{ name: 'credentials', ok: true, detail: 'app clipmux-x' }],
              },
            },
    })
  })

  await go(page, 'Encoding')
  await page.locator('[data-provider="modal"]').getByRole('button', { name: 'Connect Modal' }).click()
  const form = page.getByRole('form', { name: 'Connect Modal' })
  // Modal's NVENC GPUs with its prices (D111); the L4 and A10 are ticked,
  // not the T4, which can't decode AV1 (D132).
  const gpus = form.getByRole('list', { name: 'GPUs' })
  await expect(gpus.getByRole('listitem').first()).toContainText('L4')
  await expect(gpus.getByRole('listitem').first()).toContainText('Recommended')
  await expect(gpus.getByRole('listitem').first()).toContainText('$1.12/hr')
  await expect(gpus.getByRole('checkbox', { name: 'A10' })).toBeChecked()
  await expect(gpus.getByRole('checkbox', { name: 'T4' })).not.toBeChecked()
  await gpus.getByRole('checkbox', { name: 'A10' }).uncheck()
  await gpus.getByRole('checkbox', { name: 'L40S' }).check()
  await form.getByRole('button', { name: 'Change' }).click()
  await form.getByLabel('Monthly limit ($)').fill('40')
  const popup = page.waitForEvent('popup')
  await form.getByRole('button', { name: 'Connect with Modal' }).click()
  const tab = await popup
  await expect(tab).toHaveURL(/\/approve-here\?flow=tf-1$/)
  expect(await tab.evaluate(() => window.opener)).toBeNull()
  await expect(form).toContainText('AB12')
  await expect(form.getByText('✓ Credentials')).toBeVisible({ timeout: 15_000 })
  expect(started).toEqual([{ max_concurrent: 10, monthly_limit_cents: 4000, gpu: 'L4,L40S' }])
  await tab.close()

  // Pasting instead: the command Modal shows fills both fields.
  await form.getByRole('button', { name: 'Paste a token instead' }).click()
  await form
    .getByLabel('Modal token ID')
    .fill('modal token set --token-id ak-abc123 --token-secret as-def456')
  await expect(form.getByLabel('Modal token ID')).toHaveValue('ak-abc123')
  await expect(form.getByLabel('Modal token secret')).toHaveValue('as-def456')
  await noHorizontalScroll(page)
  await noViolations()
})

// RunPod's console shows the key on its own page (D109), so its dialog is a
// key field: "Get a key from RunPod" opens that page, and the pasted key
// lists RunPod's GPUs from the fixture's fake catalog.
test('a RunPod key from RunPod’s page lists its GPUs', async ({ page, request, baseURL }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig & { runpod_key: string }
  await signIn(page, cfg)
  await page
    .context()
    .route('**/approve-here**', (route) =>
      route.fulfill({ contentType: 'text/html', body: '<title>RunPod</title><p>Your key</p>' }),
    )
  await page.route('**/v1/executors/runpod/authorize', (route) =>
    route.fulfill({
      status: 201,
      json: {
        id: 'apr_rp',
        url: `${baseURL}/approve-here?request=r1`,
        expires_at: new Date(Date.now() + 600_000).toISOString(),
        status: 'pending',
      },
    }),
  )
  await go(page, 'Encoding')
  await page.locator('[data-provider="runpod"]').getByRole('button', { name: 'Connect RunPod' }).click()
  const form = page.getByRole('form', { name: 'Connect RunPod' })
  await expect(form.getByRole('button', { name: 'Connect RunPod' })).toBeDisabled()
  const popup = page.waitForEvent('popup')
  await form.getByRole('button', { name: 'Get a key from RunPod' }).click()
  const tab = await popup
  await expect(tab).toHaveURL(/\/approve-here\?request=r1$/)
  expect(await tab.evaluate(() => window.opener)).toBeNull()
  await tab.close()
  await form.getByLabel('RunPod API key').fill('rpa_WRONG_KEY')
  await expect(form.getByText('RunPod refused the API key')).toBeVisible()
  await form.getByLabel('RunPod API key').fill(cfg.runpod_key)
  const gpus = form.getByRole('list', { name: 'GPUs' })
  await expect(gpus.getByRole('listitem')).toHaveCount(2)
  await expect(gpus.getByRole('checkbox', { name: 'L4' })).toBeChecked()
  await expect(gpus.getByRole('listitem').last()).toContainText('$0.25/hr')
  await expect(form.getByRole('button', { name: 'Connect RunPod' })).toBeEnabled()
  await noHorizontalScroll(page)
  await noViolations()
})
