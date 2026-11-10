import { createHmac } from 'node:crypto'
import { type Browser, expect, type Page, test } from '@playwright/test'
import { expectSignedIn, expectSignedOut, noHorizontalScroll, watchCSP } from './csp.ts'

// The TOTP drill (P6-T04, D071; P5 check 9): on an install of its own, the
// owner turns on two-factor sign-in from Settings › Account with a code
// computed here as an authenticator app would, signs in with the next code,
// then with a recovery code, which works once.

interface Config {
  totp: string
  ops_email: string
  ops_password: string
}

// RFC 6238 with the dashboard's parameters: HMAC-SHA1, six digits, 30 s.
function base32(s: string): Buffer {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  let bits = ''
  for (const c of s.replace(/[\s=]/g, '').toUpperCase()) {
    const v = alphabet.indexOf(c)
    if (v < 0) throw new Error(`not base32: ${c}`)
    bits += v.toString(2).padStart(5, '0')
  }
  const bytes = []
  for (let i = 0; i + 8 <= bits.length; i += 8) bytes.push(Number.parseInt(bits.slice(i, i + 8), 2))
  return Buffer.from(bytes)
}
const step = () => Math.floor(Date.now() / 30_000)
function code(secret: string, at: number): string {
  const counter = Buffer.alloc(8)
  counter.writeBigUInt64BE(BigInt(at))
  const mac = createHmac('sha1', base32(secret)).update(counter).digest()
  const off = (mac.at(-1) ?? 0) & 0x0f
  return ((mac.readUInt32BE(off) & 0x7fffffff) % 1_000_000).toString().padStart(6, '0')
}

async function signIn(page: Page, cfg: Config, second?: { code?: string; recovery?: string }) {
  await page.goto(`${cfg.totp}/login`)
  await page.getByLabel('Email').fill(cfg.ops_email)
  await page.getByLabel('Password', { exact: true }).fill(cfg.ops_password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  if (!second) return
  await expect(page.getByLabel('Code from your authenticator app')).toBeVisible()
  if (second.recovery) {
    await page.getByRole('button', { name: 'Use a recovery code' }).click()
    await page.getByLabel('Recovery code').fill(second.recovery)
  } else {
    await page.getByLabel('Code from your authenticator app').fill(second.code ?? '')
  }
  await page.getByRole('button', { name: 'Sign in' }).click()
}

async function fresh(browser: Browser) {
  const context = await browser.newContext({ viewport: test.info().project.use.viewport ?? null })
  return { context, page: await context.newPage() }
}

test('the owner turns on TOTP, signs in with a code, and with a recovery code once', async ({
  browser,
  page,
  request,
}) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as Config
  await signIn(page, cfg)
  await expectSignedIn(page)
  // The old Account address still lands on the tab.
  await page.goto(`${cfg.totp}/account`)
  await expect(page.getByRole('tab', { name: 'Account', selected: true })).toBeVisible()
  await noHorizontalScroll(page)

  await page.getByLabel('Password', { exact: true }).fill(cfg.ops_password)
  await page.getByRole('button', { name: 'Set up two-factor sign-in' }).click()
  const secret = (
    (await page.getByRole('region', { name: 'Authenticator key' }).locator('pre').textContent()) ?? ''
  ).trim()
  await expect(page.getByRole('link', { name: /authenticator app/ })).toHaveAttribute(
    'href',
    /^otpauth:\/\/totp\//,
  )
  const enrolled = step()
  await page.getByLabel('Code', { exact: true }).fill(code(secret, enrolled))
  await page.getByRole('button', { name: 'Turn on' }).click()
  const codes = (
    (await page.getByRole('region', { name: 'Recovery codes' }).locator('pre').textContent()) ?? ''
  )
    .trim()
    .split(/\s+/)
  expect(codes).toHaveLength(10)
  const recovery = codes[0] ?? ''
  await noViolations()

  // The password alone isn't enough any more.
  const bare = await fresh(browser)
  await signIn(bare.page, cfg)
  await expect(bare.page.getByLabel('Code from your authenticator app')).toBeVisible()
  await expectSignedOut(bare.page)
  await bare.context.close()

  // The enrolling code was spent; the next step's is accepted (one step of
  // drift either way).
  const withCode = await fresh(browser)
  await signIn(withCode.page, cfg, { code: code(secret, Math.max(step(), enrolled + 1)) })
  await expectSignedIn(withCode.page)
  await withCode.context.close()

  // A wrong code is refused.
  const wrong = await fresh(browser)
  await signIn(wrong.page, cfg, { code: code(secret, step() + 5) })
  await expectSignedOut(wrong.page)
  await expect(wrong.page.getByRole('alert')).toBeVisible()
  await wrong.context.close()

  // A recovery code signs in once.
  const recovered = await fresh(browser)
  await signIn(recovered.page, cfg, { recovery })
  await expectSignedIn(recovered.page)
  await recovered.context.close()
  const again = await fresh(browser)
  await signIn(again.page, cfg, { recovery })
  await expectSignedOut(again.page)
  await expect(again.page.getByRole('alert')).toBeVisible()
  await again.context.close()
})
