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
