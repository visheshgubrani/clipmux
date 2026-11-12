import { expect, type Page } from '@playwright/test'

// P5 exit check 9: with the dashboard's CSP enforced, nothing it does is
// refused. Call before the first navigation; call the result at the end.
export async function watchCSP(page: Page): Promise<() => Promise<void>> {
  await page.addInitScript(() => {
    const seen: string[] = []
    ;(globalThis as unknown as { __csp: string[] }).__csp = seen
    document.addEventListener('securitypolicyviolation', (e) =>
      seen.push(`${e.violatedDirective} ${e.blockedURI}`),
    )
  })
  const all: string[] = []
  page.on('console', (m) => {
    if (m.type() === 'error' && m.text().includes('Content Security Policy')) all.push(m.text())
  })
  return async () => {
    const inPage = await page.evaluate(() => (globalThis as unknown as { __csp?: string[] }).__csp ?? [])
    expect([...all, ...inPage], 'CSP violations').toEqual([])
  }
}

// P6 exit check 6: no page scrolls sideways, on a phone or a desktop. Wide
// content (tables, code) scrolls inside its own box instead.
export async function noHorizontalScroll(page: Page) {
  await expect
    .poll(
      () => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth),
      { message: `horizontal scroll on ${page.url()}` },
    )
    .toBeLessThanOrEqual(0)
}

/** Opens a page from the main navigation, through the menu sheet on a phone. */
export async function go(page: Page, name: string) {
  const menu = page.getByRole('button', { name: 'Open menu' })
  if (await menu.isVisible()) await menu.click()
  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name, exact: true }).click()
  await noHorizontalScroll(page)
}

/** Signed in: the shell's Upload button shows (the sidebar's, or the phone bar's). */
export async function expectSignedIn(page: Page) {
  await expect(page.getByRole('button', { name: 'Upload', exact: true }).first()).toBeVisible()
}

export async function expectSignedOut(page: Page) {
  await expect(page.getByRole('button', { name: 'Upload', exact: true })).toHaveCount(0)
}

export interface OpsConfig {
  ops: string
  ops_email: string
  ops_password: string
  asset_id: string
  app_id: string
}

/** Signs in as the fixture's owner on the main install. */
export async function signIn(page: Page, cfg: OpsConfig, password = cfg.ops_password) {
  await page.goto(`${cfg.ops}/login`)
  await page.getByLabel('Email').fill(cfg.ops_email)
  await page.getByLabel('Password', { exact: true }).fill(password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expectSignedIn(page)
}

/** Closes a toast by its title: on a phone it sits over the content. */
export async function dismissToast(page: Page, title: string) {
  // It may close itself (5 s) before the click lands; either way it goes.
  const toast = page.getByRole('dialog', { name: title })
  await toast
    .getByRole('button', { name: 'Close toast' })
    .click({ timeout: 2_000 })
    .catch(() => page.mouse.move(0, 0))
  await expect(toast).toHaveCount(0, { timeout: 15_000 })
}
