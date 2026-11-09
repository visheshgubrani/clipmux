import { expect, test } from '@playwright/test'

test('an upload closed after one part resumes with only the rest', async ({ page }) => {
  await page.goto('/uploader.html')
  await expect
    .poll(
      () =>
        page.evaluate(
          () => (globalThis as unknown as { __state: { paused: boolean } }).__state?.paused ?? false,
        ),
      { timeout: 60_000 },
    )
    .toBe(true)
  const puts: number[] = []
  page.on('request', (r) => {
    if (r.method() !== 'PUT') return
    const url = new URL(r.url())
    const part = url.searchParams.get('partNumber') ?? url.pathname.match(/\/parts\/[^/]+\/(\d+)$/)?.[1]
    if (part) puts.push(Number(part))
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
  expect(puts.sort(), 'parts 2 and 3 only').toEqual([2, 3])
  const cfg = await (await page.request.get('/config')).json()
  if (cfg.live) {
    const id = await page.evaluate(
      () => (globalThis as unknown as { __state: { assetID: string } }).__state.assetID,
    )
    await expect
      .poll(async () => (await (await page.request.get(`/asset?id=${id}`)).json()).status, {
        timeout: 300_000,
      })
      .toBe('ready')
  }
})
