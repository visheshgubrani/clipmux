import { expect, type Page, test } from '@playwright/test'
import { watchCSP } from './csp.ts'

// Subtitles and chapters (P5-T18, exit checks 14 and 15) on the real serve
// the fixture boots, whose video was transcribed by a fake speech API and
// chaptered by a fake LLM: the cue the fake heard at 1–3 s, the chapters
// on the time slider, the VTT after a refreshed token, and the dashboard's
// track pages.

interface Config {
  base_url: string
  playback_id: string
  asset_id: string
  app_id: string
  subtitles_track: string
  chapters_track: string
  cue_text: string
  cue_start: number
  ops: string
  ops_email: string
  ops_password: string
  llm_url: string
  llm_key: string
}

const config = async (page: Page) => (await (await page.request.get('/config')).json()) as Config

async function h264(page: Page): Promise<void> {
  const ok = await page.evaluate(() => MediaSource.isTypeSupported('video/mp4; codecs="avc1.64001f"'))
  expect(ok, 'this Chromium build has no H.264; set CLIPMUX_E2E_CHANNEL=chrome').toBe(true)
}

// showSubtitles turns on the subtitles track hls.js listed from the master.
async function showSubtitles(page: Page): Promise<void> {
  await expect
    .poll(() =>
      page.evaluate(() => {
        const v = document.querySelector('hlsjs-video') as unknown as HTMLVideoElement | null
        const t = Array.from(v?.textTracks ?? []).find((t) => t.kind === 'subtitles')
        if (t) t.mode = 'showing'
        return t ? `${t.language}:${t.label}` : ''
      }),
    )
    .toBe('en:English')
}

// activeCue waits for a subtitle cue to be active and returns it with the
// playhead's time then.
async function activeCue(page: Page): Promise<{ text: string; start: number; at: number }> {
  let seen = { text: '', start: -1, at: -1 }
  await expect
    .poll(
      async () => {
        seen = await page.evaluate(() => {
          const v = document.querySelector('hlsjs-video') as unknown as HTMLVideoElement
          const t = Array.from(v.textTracks).find((t) => t.kind === 'subtitles' && t.mode === 'showing')
          const c = t?.activeCues?.[0] as VTTCue | undefined
          return { text: c?.text ?? '', start: c?.startTime ?? -1, at: v.currentTime }
        })
        return seen.text
      },
      { timeout: 120_000, intervals: [100] },
    )
    .not.toBe('')
  return seen
}

test('hls.js shows the subtitle cue at its known time', async ({ page }) => {
  const cfg = await config(page)
  await page.goto('/player.html')
  await h264(page)
  await showSubtitles(page)
  const cue = await activeCue(page)
  expect(cue.text).toBe(cfg.cue_text)
  expect(Math.abs(cue.start - cfg.cue_start)).toBeLessThanOrEqual(0.5)
  // The cue runs 1–3 s; it is active while the playhead is there.
  expect(cue.at).toBeGreaterThanOrEqual(cfg.cue_start - 0.5)
  expect(cue.at).toBeLessThanOrEqual(3.5)
  // The skin's captions button lists it.
  await expect(page.locator('media-captions-button')).toHaveCount(1)
})

test('chapters mark the time slider and convert to seconds', async ({ page }) => {
  await page.goto('/player.html?chapters=1')
  await h264(page)
  await expect
    .poll(() =>
      page.evaluate(() => (globalThis as unknown as { __state: { chapters: unknown[] } }).__state.chapters),
    )
    .toEqual([
      { startTime: 0, endTime: 6, title: 'Opening' },
      { startTime: 6, endTime: expect.any(Number), title: 'Chapter two' },
    ])
  await expect
    .poll(() =>
      page.evaluate(() => {
        const v = document.querySelector('hlsjs-video') as unknown as HTMLVideoElement
        const t = Array.from(v.textTracks).find((t) => t.kind === 'chapters')
        return t?.cues?.length ?? 0
      }),
    )
    .toBe(2)
  // The chapters track never appears in the captions menu.
  const subtitleTracks = await page.evaluate(() => {
    const v = document.querySelector('hlsjs-video') as unknown as HTMLVideoElement
    return Array.from(v.textTracks).filter((t) => t.kind === 'subtitles').length
  })
  expect(subtitleTracks).toBe(1)
  await expect(page.locator('.media-time-slider-chapter')).toHaveCount(2)
})

test('a token refreshed mid-play still loads a track VTT', async ({ page }) => {
  const seen: { status: number; url: string }[] = []
  page.on('response', (r) => {
    if (r.url().includes('/v/')) seen.push({ status: r.status(), url: r.url() })
  })
  const token = (url: string) => url.split('/v/')[1]?.split('/')[0] ?? ''
  // The chapters track is named, and subtitles turned on, once the 8 s
  // token has expired and been refreshed.
  await page.goto('/player.html?ttl=8&chapters=late')
  await h264(page)
  await expect
    .poll(
      () =>
        page.evaluate(
          () => (globalThis as unknown as { __state: { chapters: unknown[] } }).__state.chapters.length,
        ),
      { timeout: 120_000 },
    )
    .toBe(2)
  const cfg = await config(page)
  expect(
    seen.some((r) => r.status === 401),
    'the 8 s token must have expired mid-playback',
  ).toBe(true)
  const first = token(seen[0]?.url ?? '')
  const chapters = seen.filter((r) => r.url.includes(`/tracks/${cfg.chapters_track}.vtt`))
  expect(chapters.length).toBeGreaterThan(0)
  expect(chapters.at(-1)?.status).toBe(200)
  expect(token(chapters.at(-1)?.url ?? ''), 'the VTT went with the refreshed token').not.toBe(first)
  // Subtitles load once shown: turned on now, they go with the new token too.
  await showSubtitles(page)
  const subsLoaded = () => seen.filter((r) => r.url.includes(`/tracks/${cfg.subtitles_track}.vtt`))
  await expect.poll(() => subsLoaded().length, { timeout: 30_000 }).toBeGreaterThan(0)
  expect(subsLoaded().at(-1)?.status).toBe(200)
  expect(token(subsLoaded().at(-1)?.url ?? '')).not.toBe(first)
  const state = await page.evaluate(() => (globalThis as unknown as { __state: { errors: number } }).__state)
  expect(state.errors).toBe(0)
})

async function signIn(page: Page, cfg: Config) {
  await page.goto(`${cfg.ops}/login`)
  await page.getByLabel('Email').fill(cfg.ops_email)
  await page.getByLabel('Password').fill(cfg.ops_password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('navigation')).toBeVisible()
}

test('the dashboard shows tracks, their transcript and chapters, and makes and deletes them', async ({
  page,
}) => {
  const noViolations = await watchCSP(page)
  const cfg = await config(page)
  await signIn(page, cfg)
  await page.goto(`${cfg.ops}/assets/${cfg.asset_id}?app=${cfg.app_id}`)
  const tracks = page.getByLabel('Tracks')
  const subs = tracks.locator('[data-track="subtitles"]')
  await expect(subs).toContainText('English')
  await expect(subs).toContainText('ready')
  await subs.getByRole('button', { name: 'Transcript' }).click()
  await expect(page.getByLabel('Transcript')).toContainText(cfg.cue_text)
  const chapters = tracks.locator('[data-track="chapters"]')
  await chapters.getByRole('button', { name: 'Chapters' }).click()
  await expect(page.getByLabel('Chapter list')).toContainText('0:00 Opening')
  await expect(page.getByLabel('Chapter list')).toContainText('0:06 Chapter two')

  // Regenerating keeps one chapters track listed: the old one until the new is ready.
  await tracks.getByRole('button', { name: 'Regenerate chapters' }).click()
  await expect(chapters.filter({ hasNotText: cfg.chapters_track })).toContainText('ready', {
    timeout: 60_000,
  })
  await expect(chapters).toHaveCount(1)
  await expect(chapters).not.toContainText(cfg.chapters_track)
  page.once('dialog', (d) => void d.accept())
  await chapters.getByRole('button', { name: 'Delete' }).click()
  await expect(chapters).toHaveCount(0)
  await noViolations()
})

test('the preview plays with sound and offers new subtitles without interrupting', async ({ page }) => {
  const noViolations = await watchCSP(page)
  const cfg = await config(page)
  await signIn(page, cfg)
  await page.goto(`${cfg.ops}/assets/${cfg.asset_id}?app=${cfg.app_id}`)
  // Only an autoplaying preview (the setup wizard's) starts muted.
  await expect
    .poll(() =>
      page.evaluate(() => (document.querySelector('hlsjs-video') as unknown as HTMLVideoElement | null)?.muted),
    )
    .toBe(false)
  const tracks = page.getByLabel('Tracks')
  await tracks.getByRole('button', { name: 'Regenerate subtitles' }).click()
  // The open player keeps playing; once the new subtitles are ready the
  // owner is offered a reload instead of having it happen under them.
  await expect(page.getByText('New subtitles are ready.')).toBeVisible({ timeout: 60_000 })
  await page.getByRole('button', { name: 'Reload the player' }).click()
  await expect(page.getByText('New subtitles are ready.')).toHaveCount(0)
  await noViolations()
})

test('the owner connects an LLM and sees which PCs can transcribe', async ({ page }) => {
  const noViolations = await watchCSP(page)
  const cfg = await config(page)
  await signIn(page, cfg)
  await page.getByRole('navigation').getByRole('link', { name: 'Captions' }).click()
  await expect(page.getByLabel('Transcribe')).toHaveValue('api')
  await expect(page.locator('[data-connected="chapters"]')).toContainText('gpt-test')
  await page.getByText('Connect an LLM', { exact: true }).click()
  const form = page.getByRole('form', { name: 'Connect an LLM' })
  await form.getByLabel('LLM provider').selectOption('openai_compatible')
  await form.getByLabel('LLM API URL').fill(cfg.llm_url)
  await form.getByLabel('LLM model').fill('gpt-test')
  await form.getByLabel('LLM API key').fill(cfg.llm_key)
  await form.getByRole('button', { name: 'Connect' }).click()
  for (const step of ['Credentials', 'Sample chapters', 'Saved']) {
    await expect(page.getByText(`✓ ${step}`)).toBeVisible()
  }
  await expect(page.locator('body')).not.toContainText(cfg.llm_key)

  await page.getByRole('navigation').getByRole('link', { name: 'Upload' }).click()
  await page.getByLabel(/Generate chapters/).check()
  await expect(page.getByLabel('Generate subtitles')).toBeChecked()

  await page.getByRole('navigation').getByRole('link', { name: 'Workers' }).click()
  await expect(page.getByRole('row', { name: /studio-pc/ })).toContainText('vulkan, cpu')
  await noViolations()
})
