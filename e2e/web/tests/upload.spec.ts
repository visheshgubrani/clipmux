import { execFileSync } from 'node:child_process'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'
import { go, noHorizontalScroll, type OpsConfig, signIn, watchCSP } from './csp.ts'

// Upload to delete (P6-T13): a titled public upload from the dialog, its
// poster in the grid, search, the privacy switch, and delete, which asks.
test('the owner uploads a titled public video, finds it, makes it private and deletes it', async ({
  page,
  request,
}) => {
  const noViolations = await watchCSP(page)
  const cfg = (await (await request.get('/config')).json()) as OpsConfig
  const dir = mkdtempSync(join(tmpdir(), 'clipmux-upload-'))
  const file = join(dir, 'garden-tour.mp4')
  execFileSync(process.env.CLIPMUX_FFMPEG ?? 'ffmpeg', [
    ...['-loglevel', 'error', '-f', 'lavfi', '-i', 'testsrc2=duration=4:size=640x360:rate=25'],
    ...['-f', 'lavfi', '-i', 'sine=duration=4', '-shortest', '-c:v', 'libx264', '-pix_fmt', 'yuv420p'],
    ...['-c:a', 'aac', file],
  ])
  const title = `Garden tour ${test.info().project.name}`
  await signIn(page, cfg)
  await go(page, 'Videos')

  await page.getByRole('button', { name: 'Upload', exact: true }).first().click()
  const dialog = page.getByRole('dialog', { name: 'Upload videos' })
  await dialog.getByLabel('Video file').setInputFiles(file)
  await expect(dialog.getByLabel('Title for garden-tour.mp4')).toHaveValue('garden tour')
  await dialog.getByLabel('Title for garden-tour.mp4').fill(title)
  await dialog.getByRole('button', { name: 'Public' }).click()
  await dialog.getByRole('button', { name: 'Upload video' }).click()
  await expect(dialog).toContainText('Uploaded', { timeout: 60_000 })
  await dialog.getByRole('link', { name: 'Open video' }).click()

  // Its page: the title, public, and ready once encoded.
  await expect(page.getByRole('heading', { name: title, level: 1 })).toBeVisible()
  await expect(page.getByText('Ready', { exact: true })).toBeVisible({ timeout: 120_000 })
  await noHorizontalScroll(page)

  // The grid shows its poster, and search finds it by title.
  await go(page, 'Videos')
  await page.getByLabel('Search videos').fill(title)
  const card = page.getByRole('link', { name: new RegExp(title) })
  await expect(card).toHaveCount(1)
  await expect(card.locator('img')).toHaveJSProperty('complete', true)
  await expect(card.getByLabel('Public')).toBeVisible()

  // Private again, which asks first, then delete, which asks too.
  await card.click()
  await page.getByRole('tab', { name: 'Sharing' }).click()
  await page.getByText('Private', { exact: true }).click()
  await page.getByRole('alertdialog').getByRole('button', { name: 'Make private' }).click()
  await expect(page.getByText('Video is private')).toBeVisible()
  await page.getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: 'Delete video' }).click()
  await expect(page.getByRole('alertdialog')).toContainText(`Delete "${title}"?`)
  await page.getByRole('alertdialog').getByRole('button', { name: 'Delete video' }).click()
  await expect(page.getByText('Deleted', { exact: true })).toBeVisible({ timeout: 60_000 })
  await go(page, 'Videos')
  await page.getByLabel('Search videos').fill(title)
  await expect(page.getByText('No videos match')).toBeVisible()
  await noViolations()
})
