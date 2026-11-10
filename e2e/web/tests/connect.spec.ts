import { expect, test } from '@playwright/test'
import { noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// /workers/connect (D042): `clipmux worker --server URL` shows a code and
// opens this page; the owner approves or denies it. Here the test is the PC.
async function pairing(request: import('@playwright/test').APIRequestContext, ops: string, name: string) {
  const res = await request.post(`${ops}/worker/v1/pairings`, {
    data: { name, hostname: `${name}.lan`, platform: 'linux/amd64' },
  })
  expect(res.status()).toBe(201)
  return (await res.json()) as { code: string; poll_token: string }
}

test('the owner approves one PC and denies another', async ({ page, request }) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  const project = test.info().project.name
  await signIn(page, cfg)

  const yes = await pairing(request, cfg.ops, `laptop-${project}`)
  await page.goto(`${cfg.ops}/workers/connect?code=${yes.code}`)
  await expect(page.getByRole('heading', { name: 'Connect this PC?' })).toBeVisible()
  await expect(page.getByText(`laptop-${project}.lan`)).toBeVisible()
  await noHorizontalScroll(page)
  await page.getByRole('button', { name: 'Connect', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Connected' })).toBeVisible()
  const approved = await request.post(`${cfg.ops}/worker/v1/pairings/poll`, {
    data: { poll_token: yes.poll_token },
  })
  expect((await approved.json()).status).toBe('approved')

  // A code typed by hand, then denied.
  const no = await pairing(request, cfg.ops, `stranger-${project}`)
  await page.goto(`${cfg.ops}/workers/connect`)
  await page.getByLabel('Pairing code').fill(no.code)
  await page.getByRole('button', { name: 'Continue' }).click()
  await page.getByRole('button', { name: 'Deny' }).click()
  await expect(page.getByRole('heading', { name: 'Not connected' })).toBeVisible()
  const denied = await request.post(`${cfg.ops}/worker/v1/pairings/poll`, {
    data: { poll_token: no.poll_token },
  })
  expect((await denied.json()).status).toBe('denied')

  // A code nobody is waiting with says so.
  await page.goto(`${cfg.ops}/workers/connect?code=ZZZZ-ZZZZ`)
  await expect(page.getByText('No PC is waiting with that code.')).toBeVisible()
  await noViolations()
})
