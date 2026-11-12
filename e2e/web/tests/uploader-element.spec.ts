import { expect, type Page, test } from '@playwright/test'

// <clipmux-uploader create-url>: the fixture's /create-upload stands in for
// the app's backend handler, which creates the upload with its API key.

interface State {
  assetID: string
  status: string
  progress: number
  errors: string[]
}

const state = (page: Page) => page.evaluate(() => (globalThis as unknown as { __state: State }).__state)

const size = 3 * 1024 * 1024
const video = { name: 'lesson.mp4', mimeType: 'video/mp4', buffer: Buffer.alloc(size, 7) }

function creates(page: Page): unknown[] {
  const bodies: unknown[] = []
  page.on('request', (r) => {
    if (new URL(r.url()).pathname === '/create-upload') bodies.push(r.postDataJSON())
  })
  return bodies
}

test('a chosen file uploads through the create URL', async ({ page }) => {
  const bodies = creates(page)
  await page.goto('/uploader-element.html')
  const chooser = page.waitForEvent('filechooser')
  await page.locator('clipmux-uploader').getByRole('button', { name: 'Choose a file' }).click()
  await (await chooser).setFiles(video)
  await expect(page.locator('clipmux-uploader')).toHaveAttribute('state', 'done', { timeout: 60_000 })
  const s = await state(page)
  expect(s).toMatchObject({ status: 'processing', progress: size, errors: [] })
  expect(s.assetID).toMatch(/^ast_/)
  expect(bodies).toEqual([{ size, name: 'lesson.mp4', type: 'video/mp4' }])
  await expect(page.locator('clipmux-uploader').locator('[part=status]')).toHaveText('Uploaded lesson.mp4')
  expect(
    await page.evaluate(() => Object.keys(localStorage).filter((k) => k.startsWith('clipmux:upload:'))),
    'a finished upload is forgotten',
  ).toEqual([])
})

test('a file over max-size or of another type is refused before anything is created', async ({ page }) => {
  const bodies = creates(page)
  await page.goto('/uploader-element.html?max=1000000')
  const input = page.locator('clipmux-uploader input[type=file]')
  await input.setInputFiles(video)
  await expect(page.locator('clipmux-uploader')).toHaveAttribute('state', 'error')
  await input.setInputFiles({ name: 'notes.txt', mimeType: 'text/plain', buffer: Buffer.from('hi') })
  expect((await state(page)).errors).toEqual(['file_too_large', 'file_type_rejected'])
  await expect(page.locator('clipmux-uploader').getByRole('button', { name: 'Retry' })).toBeHidden()
  expect(bodies).toEqual([])
})

test('the backend refusing the upload shows its error', async ({ page }) => {
  await page.goto('/uploader-element.html?deny=1')
  await page.locator('clipmux-uploader input[type=file]').setInputFiles(video)
  await expect(page.locator('clipmux-uploader')).toHaveAttribute('state', 'error')
  expect((await state(page)).errors).toEqual(['create_denied'])
  await expect(page.locator('clipmux-uploader').locator('[part=status]')).toHaveText(
    'you may not upload here',
  )
})

test('a refused part can be retried, and cancel tells ClipMux', async ({ page }) => {
  // Storage refuses the first PUT: a fatal part error, shown with Retry.
  let refused = false
  await page.route(
    (url) => url.pathname.includes('/parts/') || url.searchParams.has('partNumber'),
    async (route) => {
      if (route.request().method() === 'PUT' && !refused) {
        refused = true
        return route.fulfill({ status: 400, body: 'no' })
      }
      return route.fallback()
    },
  )
  const bodies = creates(page)
  await page.goto('/uploader-element.html')
  const el = page.locator('clipmux-uploader')
  await el.locator('input[type=file]').setInputFiles(video)
  await expect(el).toHaveAttribute('state', 'error', { timeout: 60_000 })
  expect((await state(page)).errors).toEqual(['part_rejected'])
  await el.getByRole('button', { name: 'Retry' }).click()
  await expect(el).toHaveAttribute('state', 'done', { timeout: 60_000 })
  expect(bodies.length, 'retry resumes the same upload').toBe(1)

  // A PUT that never answers, then Cancel.
  await page.unrouteAll()
  await page.route(
    (url) => url.pathname.includes('/parts/') || url.searchParams.has('partNumber'),
    (route) => (route.request().method() === 'PUT' ? undefined : route.fallback()),
  )
  const cancelled = page.waitForRequest(
    (r) => r.method() === 'POST' && /\/v1\/uploads\/[^/]+\/cancel$/.test(r.url()),
  )
  const sending = page.waitForRequest((r) => r.method() === 'PUT')
  await el.locator('input[type=file]').setInputFiles({ ...video, name: 'second.mp4' })
  await expect(el).toHaveAttribute('state', 'uploading')
  await sending // the upload exists and a part is in flight
  await el.getByRole('button', { name: 'Cancel' }).click()
  await cancelled
  await expect(el).toHaveAttribute('state', 'idle')
  await expect(el.locator('[part=status]')).toHaveText('Cancelled')
})
