import { expect, test } from '@playwright/test'
import { dismissToast, noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// Share links on a video's Sharing tab (P7-T08, D104): the owner makes a
// link with a name and an expiry, someone with no session watches it, its
// view shows on the row, its stream URL plays, and revoking it asks first
// and leaves the video's own IDs alone.

test('the owner shares a video, sees its view, and revokes the link', async ({ page, browser, request }) => {
  test.setTimeout(240_000)
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  await signIn(page, cfg)
  await page.goto(`${cfg.ops}/videos/${cfg.asset_id}?app=${cfg.app_id}&tab=sharing`)
  const name = `Board ${Date.now()}`

  await page.getByRole('button', { name: 'New share link' }).click()
  const dialog = page.getByRole('dialog', { name: 'New share link' })
  await dialog.getByLabel('Name').fill(name)
  await expect(dialog.getByLabel('Expires')).toHaveValue('7')
  await dialog.getByRole('button', { name: 'Create link' }).click()
  const ready = page.getByRole('dialog', { name: 'Share link ready' })
  const url = await ready.getByLabel('share link', { exact: true }).inputValue()
  expect(url).toMatch(new RegExp(`^${cfg.ops}/s/[A-Z2-7]{26}$`))
  await ready.getByRole('button', { name: 'Done' }).click()
  await dismissToast(page, 'Share link created')

  const row = page.getByRole('list', { name: 'Share links' }).getByRole('listitem').filter({ hasText: name })
  await expect(row).toContainText('Active')
  await expect(row).toContainText('0 views in 30 days')
  // The playback IDs table names the link's own ID, without a revoke of its own.
  const idRow = page.getByRole('row').filter({ hasText: name })
  await expect(idRow).toContainText('Share link')
  await expect(idRow.getByRole('button', { name: 'Revoke' })).toHaveCount(0)
  await noHorizontalScroll(page)

  // Someone with no session opens the link and it plays.
  const viewer = await browser.newContext()
  const watch = await viewer.newPage()
  await watch.goto(url)
  await expect
    .poll(
      () =>
        watch.evaluate(() => {
          const v = document.querySelector('hlsjs-video') as unknown as HTMLVideoElement | null
          if (!v) return 0
          v.muted = true
          if (v.paused) void v.play().catch(() => {})
          return v.currentTime
        }),
      { timeout: 60_000 },
    )
    .toBeGreaterThan(1)
  await viewer.close()

  // Its view lands on the row once serve flushes its counts (15 s).
  await expect
    .poll(
      async () => {
        await page.reload()
        return (await row.textContent()) ?? ''
      },
      { timeout: 90_000, intervals: [5_000] },
    )
    .toContain('1 view in 30 days')

  // The stream URL plays in other players: a signed master playlist.
  await row.getByRole('button', { name: `More for ${name}` }).click()
  await page.getByRole('menuitem', { name: 'Stream URL for other players' }).click()
  const stream = page.getByRole('dialog', { name: 'Stream URL' })
  const hls = await stream.getByLabel('stream URL', { exact: true }).inputValue()
  expect(hls).toMatch(/\/v\/[^/]+\/pid_[a-z0-9]+\/master\.m3u8$/)
  const playlist = await request.get(hls)
  expect(playlist.status()).toBe(200)
  expect(await playlist.text()).toContain('#EXTM3U')
  await stream.getByRole('button', { name: 'Close' }).first().click()

  // Revoking asks first, then the link reads revoked and its URL stops.
  await row.getByRole('button', { name: `More for ${name}` }).click()
  await page.getByRole('menuitem', { name: 'Revoke link' }).click()
  const ask = page.getByRole('alertdialog')
  await expect(ask).toContainText(name)
  await ask.getByRole('button', { name: 'Revoke link' }).click()
  await expect(row).toContainText('Revoked')
  await expect(row.getByRole('button', { name: 'Copy link' })).toHaveCount(0)
  expect((await request.get(hls)).status()).toBe(404)
  // The video's own playback stays as it was.
  await expect(page.getByRole('radio', { name: /Private/ })).toBeChecked()
  await noHorizontalScroll(page)
  await noViolations()
})
