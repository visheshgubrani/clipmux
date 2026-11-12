import { expect, test } from '@playwright/test'
import { go, noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// /connect and "Connect your app" (P6-T23): `npx @clipmux/node init` starts
// an app pairing, shows its code and opens this page; the owner approves it
// and the CLI's next poll collects the app's credentials, once. Here the
// test is the CLI.
async function pairing(request: import('@playwright/test').APIRequestContext, ops: string, name: string) {
  const res = await request.post(`${ops}/v1/app-pairings`, {
    data: { name, hostname: `${name}.local`, client: '@clipmux/node 0.1.0' },
  })
  expect(res.status()).toBe(201)
  const body = (await res.json()) as { code: string; poll_token: string; verify_url: string }
  expect(body.verify_url).toMatch(new RegExp(`/connect\\?code=${body.code}$`))
  return body
}

async function poll(request: import('@playwright/test').APIRequestContext, ops: string, token: string) {
  return request.post(`${ops}/v1/app-pairings/poll`, { data: { poll_token: token } })
}

test('the owner connects one app and denies another', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  const project = test.info().project.name
  await signIn(page, cfg)

  const yes = await pairing(request, cfg.ops, `lms-${project}`)
  await page.goto(`${cfg.ops}/connect?code=${yes.code}`)
  await expect(page.getByRole('heading', { name: 'Connect this app?' })).toBeVisible()
  // Both screens show the code, so the owner can match them.
  await expect(page.getByText(yes.code, { exact: true })).toBeVisible()
  await expect(page.getByText(`lms-${project}.local`)).toBeVisible()
  await expect(page.getByText('@clipmux/node 0.1.0')).toBeVisible()
  await noHorizontalScroll(page)

  await page.getByLabel('App', { exact: true }).selectOption({ label: 'New app…' })
  await page.getByLabel('New app name').fill(`LMS ${project}`)
  await page.getByLabel('Webhook URL (optional)').fill('https://lms.example.test/api/clipmux/webhook')
  // The fixture's public URL is loopback: the owner confirms the app is on
  // this computer before it gets keys (D108).
  const connect = page.getByRole('button', { name: 'Connect', exact: true })
  await expect(connect).toBeDisabled()
  await page.getByRole('checkbox', { name: 'This app runs on this computer' }).check()
  await connect.click()
  await expect(page.getByRole('heading', { name: 'Connected' })).toBeVisible()

  // The CLI collects the credentials once.
  const collected = await poll(request, cfg.ops, yes.poll_token)
  const body = (await collected.json()) as { status: string; credentials: { env: string; app_name: string } }
  expect(body.status).toBe('approved')
  expect(body.credentials.app_name).toBe(`LMS ${project}`)
  for (const v of [
    'CLIPMUX_URL=',
    'CLIPMUX_API_KEY=cmx_',
    'CLIPMUX_SIGNING_KID=kid_',
    'CLIPMUX_SIGNING_KEY="-----BEGIN',
    'CLIPMUX_WEBHOOK_SECRET=whsec_',
  ])
    expect(body.credentials.env).toContain(v)
  expect((await poll(request, cfg.ops, yes.poll_token)).status()).toBe(404)
  // The page links to the app it connected.
  await page.getByRole('paragraph').getByRole('link', { name: 'Apps & API' }).click()
  await expect(page.getByRole('heading', { name: `LMS ${project}`, level: 1 })).toBeVisible()

  // A code typed by hand, then denied.
  const no = await pairing(request, cfg.ops, `stranger-${project}`)
  await page.goto(`${cfg.ops}/connect`)
  await page.getByLabel('Pairing code').fill(no.code.toLowerCase())
  await page.getByRole('button', { name: 'Continue' }).click()
  await page.getByRole('button', { name: 'Deny' }).click()
  await expect(page.getByRole('heading', { name: 'Not connected' })).toBeVisible()
  expect(((await (await poll(request, cfg.ops, no.poll_token)).json()) as { status: string }).status).toBe(
    'denied',
  )

  // A code nobody is waiting with says so.
  await page.goto(`${cfg.ops}/connect?code=ZZZZ-ZZZZ`)
  await expect(page.getByText('No app is waiting with that code.')).toBeVisible()
  await noViolations()
})

test('Connect your app shows the init command and credentials once', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  await go(page, 'Apps & API')

  // The one-liner names this server.
  const command = page.getByRole('textbox', { name: 'Connect command' })
  await expect(command).toHaveValue(/^npx @clipmux\/node init --server https?:\/\/\S+[^/]$/)

  const name = `Backend ${test.info().project.name}`
  await page.getByRole('button', { name: 'Create app' }).click()
  const create = page.getByRole('dialog', { name: 'Create app' })
  await create.getByLabel('Name').fill(name)
  await create.getByRole('button', { name: 'Create app' }).click()
  await page.getByRole('link', { name: new RegExp(name) }).click()
  await expect(page.getByRole('heading', { name, level: 1 })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Connect your app' })).toBeVisible()
  await expect(page.getByRole('textbox', { name: 'Connect command' })).toHaveValue(/^npx @clipmux\/node init/)

  await page.getByRole('button', { name: 'Create credentials' }).click()
  const dialog = page.getByRole('dialog', { name: 'Create credentials' })
  const submit = dialog.getByRole('button', { name: 'Create credentials' })
  await expect(submit).toBeDisabled()
  await dialog.getByRole('checkbox', { name: /for apps on this computer/ }).check()
  await submit.click()

  const shown = page.getByRole('dialog', { name: `Credentials for ${name}` })
  await expect(shown).toContainText("It's shown once")
  await expect(shown).toContainText('CLIPMUX_API_KEY=cmx_')
  await expect(shown).toContainText('CLIPMUX_SIGNING_KEY="-----BEGIN PRIVATE KEY-----\\n')
  await expect(shown).not.toContainText('CLIPMUX_WEBHOOK_SECRET')
  await noHorizontalScroll(page)
  await shown.getByRole('button', { name: "I've saved it" }).click()
  await expect(page.locator('body')).not.toContainText('CLIPMUX_API_KEY')
  // The key it made is listed like any other.
  await expect(page.getByRole('row', { name: /integration/ })).toBeVisible()
  await noViolations()
})
