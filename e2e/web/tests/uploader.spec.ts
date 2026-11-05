import { expect, test } from '@playwright/test'

test('an upload closed after one part resumes with only the rest', async ({ page }) => {
  await page.goto('/uploader.html')
  await expect
    .poll(
      () => page.evaluate(() => (globalThis as unknown as { __state: { paused: boolean } }).__state.paused),
      { timeout: 60_000 },
    )
    .toBe(true)
  const puts: string[] = []
  page.on('request', (r) => {
    if (r.method() === 'PUT' && r.url().includes('/_storage/parts/')) puts.push(r.url())
  })
  await page.reload()
  await expect
    .poll(
      () =>
        page.evaluate(
          () => (globalThis as unknown as { __state: { status: string; error: string } }).__state,
        ),
      { timeout: 120_000 },
    )
    .toMatchObject({ status: 'processing', error: '' })
  expect(
    await page.evaluate(() => (globalThis as unknown as { __state: { resumed: boolean } }).__state.resumed),
  ).toBe(true)
  expect(puts.length, 'parts 2 and 3 only').toBe(2)
})
