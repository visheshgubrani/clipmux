import { expect, type Page, test } from '@playwright/test'
import { watchCSP } from './csp.ts'

// The SDK READMEs' Content-Security-Policy, enforced on an app's page (the
// fixture's ?csp=app): the player and the uploader work under it, and the
// browser refuses nothing. The page's origin is not ClipMux's, as in an app.

const state = <T>(page: Page) => page.evaluate(() => (globalThis as unknown as { __state: T }).__state)

test('the player plays, with its poster and chapters, under the CSP the README gives', async ({ page }) => {
  const done = await watchCSP(page)
  const res = await page.goto('/session.html?csp=app')
  expect(res?.headers()['content-security-policy']).toContain('media-src blob:')
  const h264 = await page.evaluate(() => MediaSource.isTypeSupported('video/mp4; codecs="avc1.64001f"'))
  expect(h264, 'this Chromium build has no H.264; set CLIPMUX_E2E_CHANNEL=chrome').toBe(true)
  const time = () =>
    page.evaluate(() => (document.querySelector('hlsjs-video') as unknown as HTMLVideoElement).currentTime)
  await expect.poll(time, { timeout: 120_000 }).toBeGreaterThan(5)
  await expect.poll(async () => (await state<{ chapters: number }>(page)).chapters).toBeGreaterThan(0)
  const poster = await page.evaluate(
    () => (document.querySelector('hlsjs-video') as unknown as HTMLVideoElement).poster,
  )
  const loaded = await page.evaluate(
    (src) =>
      new Promise<boolean>((resolve) => {
        const img = new Image()
        img.onload = () => resolve(true)
        img.onerror = () => resolve(false)
        img.src = src
      }),
    poster,
  )
  expect(loaded, 'the poster loads under img-src').toBe(true)
  await done()
})

test('the uploader uploads under the CSP the README gives', async ({ page }) => {
  const done = await watchCSP(page)
  await page.goto('/uploader-element.html?csp=app')
  const chooser = page.waitForEvent('filechooser')
  await page.locator('clipmux-uploader').getByRole('button', { name: 'Choose a file' }).click()
  await (await chooser).setFiles({
    name: 'lesson.mp4',
    mimeType: 'video/mp4',
    buffer: Buffer.alloc(3 << 20, 7),
  })
  await expect(page.locator('clipmux-uploader')).toHaveAttribute('state', 'done', { timeout: 60_000 })
  expect((await state<{ errors: string[] }>(page)).errors).toEqual([])
  await done()
})
