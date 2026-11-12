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
  await form.getByLabel('At most this many machines').fill('2')
  await form.getByLabel('Monthly limit ($)').fill('40')
  await form.getByLabel('Rate ($ per machine-hour)').fill('0.90')
  const popup = page.waitForEvent('popup')
  await form.getByRole('button', { name: 'Connect with Modal' }).click()
  const tab = await popup
  await expect(tab).toHaveURL(/\/approve-here\?flow=tf-1$/)
  expect(await tab.evaluate(() => window.opener)).toBeNull()
  await expect(form).toContainText('AB12')
  await expect(form.getByText('✓ Credentials')).toBeVisible({ timeout: 15_000 })
  expect(started).toEqual([{ max_concurrent: 2, monthly_limit_cents: 4000, rate_cents_per_hour: 90 }])
  await tab.close()

  // Pasting instead: the command Modal shows fills both fields.
  await form.getByRole('button', { name: 'Paste a key instead' }).click()
  await form
    .getByLabel('Modal token ID')
    .fill('modal token set --token-id ak-abc123 --token-secret as-def456')
  await expect(form.getByLabel('Modal token ID')).toHaveValue('ak-abc123')
  await expect(form.getByLabel('Modal token secret')).toHaveValue('as-def456')
  await noHorizontalScroll(page)
  await noViolations()
})

// RunPod's console showed the key on its own page on a real run, rather
// than handing it to the server: the dialog turns to pasting it.
test('a key RunPod shows on its page is pasted instead', async ({ page, request, baseURL }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  await page
    .context()
    .route('**/approve-here**', (route) =>
      route.fulfill({ contentType: 'text/html', body: '<title>RunPod</title><p>Paste this key</p>' }),
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
  await page.route('**/v1/executors/runpod/authorize/apr_rp', (route) =>
    route.fulfill({ json: { id: 'apr_rp', expires_at: new Date().toISOString(), status: 'shown' } }),
  )
  await go(page, 'Encoding')
  await page.locator('[data-provider="runpod"]').getByRole('button', { name: 'Connect RunPod' }).click()
  const form = page.getByRole('form', { name: 'Connect RunPod' })
  await form.getByLabel('Monthly limit ($)').fill('40')
  await form.getByLabel('Rate ($ per machine-hour)').fill('0.45')
  const popup = page.waitForEvent('popup')
  await form.getByRole('button', { name: 'Connect with RunPod' }).click()
  await (await popup).close()
  await expect(form.getByRole('status')).toContainText('RunPod showed the key on its own page')
  await expect(form.getByLabel('RunPod API key')).toBeVisible()
  await noViolations()
})
