// Launch exit check 7 (P6-T17, D112): an agent integrates from the skills
// alone. CLIPMUX_EVAL_APP is a copy of skills/evals/lms-starter that a coding
// agent changed, with only the installed skills, to upload lesson videos,
// take ClipMux's webhook and play them signed. Its env file is what `init`
// wrote. This suite only uses the app as its users would; it doesn't read
// the agent's code, so any working integration passes:
//   - the instructor uploads a lesson's video through the page;
//   - the webhook marks it ready, and the page then shows a player;
//   - an enrolled student plays it; a visitor who isn't enrolled loads no media;
//   - no CLIPMUX_* secret is in the browser bundle or in any response the
//     browser gets from the app.
import { readdir, readFile, stat } from 'node:fs/promises'
import { join } from 'node:path'
import { parseEnv } from 'node:util'
import { type Browser, expect, type Page, test } from '@playwright/test'
import { Run } from '../runner.ts'

interface Config {
  ops: string
  video: string
}

const app = process.env.CLIPMUX_EVAL_APP ?? ''
const port = Number(process.env.CLIPMUX_EVAL_APP_PORT ?? 3000)
const origin = `http://127.0.0.1:${port}`

async function files(dir: string): Promise<string[]> {
  const out: string[] = []
  for (const e of await readdir(dir, { withFileTypes: true })) {
    const p = join(dir, e.name)
    if (e.isDirectory()) out.push(...(await files(p)))
    else out.push(p)
  }
  return out
}

async function envOf(dir: string): Promise<Record<string, string | undefined>> {
  for (const name of ['.env.local', '.env']) {
    const f = join(dir, name)
    if (
      await stat(f).then(
        () => true,
        () => false,
      )
    )
      return parseEnv(await readFile(f, 'utf8'))
  }
  throw new Error(`no .env.local or .env in ${dir}: did init run?`)
}

/** A browser of one of the starter's users, signed in through its header. */
async function as(browser: Browser, user: string, seen: string[]): Promise<Page> {
  const page = await (await browser.newContext({ baseURL: origin })).newPage()
  page.on('response', async (r) => {
    if (!r.url().startsWith(origin)) return
    try {
      seen.push(await r.text())
    } catch {
      // a redirect or an aborted request has no body
    }
  })
  await page.goto('/')
  await page.getByLabel('Signed in as').selectOption(user)
  const action = page.waitForResponse((r) => r.request().method() === 'POST')
  await page.getByRole('button', { name: 'Switch' }).click()
  await action
  return page
}

const media = (page: Page) =>
  page.evaluate(() => {
    const v = document.querySelector('clipmux-player')?.media ?? document.querySelector('hlsjs-video')
    return v ? { currentTime: (v as HTMLVideoElement).currentTime } : null
  })

test('an agent with only the skills integrated ClipMux into lms-starter', async ({ browser, request }) => {
  expect(app, 'set CLIPMUX_EVAL_APP to the app the agent changed').not.toBe('')
  const cfg = (await (await request.get('/config')).json()) as Config
  const env = await envOf(app)
  expect(env.CLIPMUX_URL).toBe(cfg.ops)
  const pemBody = (env.CLIPMUX_SIGNING_KEY ?? '').split(/\\n|\n/).filter((l) => l && !l.startsWith('-----'))
  const secrets = [env.CLIPMUX_API_KEY, env.CLIPMUX_WEBHOOK_SECRET, ...pemBody].filter(
    (s): s is string => !!s,
  )
  expect(
    secrets.length,
    'init wrote the API key, the webhook secret and the signing key',
  ).toBeGreaterThanOrEqual(3)

  const runs: Run[] = []
  const seen: string[] = []
  try {
    await test.step('the app builds, and its browser bundle holds no secret', async () => {
      const build = new Run(app, 'npm', ['run', 'build'])
      runs.push(build)
      expect(await build.exited, build.output).toBe(0)
      for (const f of await files(join(app, '.next', 'static'))) {
        const text = await readFile(f, 'utf8')
        for (const s of secrets) expect(text.includes(s), `${f} holds a CLIPMUX_* secret`).toBe(false)
      }
    })

    const next = new Run(app, 'npx', [
      '--no',
      'next',
      'start',
      '--port',
      String(port),
      '--hostname',
      '127.0.0.1',
    ])
    runs.push(next)
    await next.waitFor(/Ready in|started server/i, 60_000)

    const ada = await as(browser, 'ada', seen)
    await test.step('the instructor uploads a lesson video', async () => {
      await ada.goto('/')
      await ada.getByLabel('Title').fill('Lesson one')
      await ada.getByRole('button', { name: 'Add lesson' }).click()
      await expect(ada.getByRole('heading', { name: 'Lesson one', level: 1 })).toBeVisible()
      const uploader = ada.locator('clipmux-uploader')
      await uploader.locator('input[type=file]').setInputFiles(cfg.video)
      await expect(uploader).toHaveAttribute('state', 'done', { timeout: 120_000 })
    })
    const lesson = new URL(ada.url()).pathname

    await test.step('the webhook marks it ready, and the page shows a player', async () => {
      await expect
        .poll(
          async () => {
            await ada.goto(lesson)
            return ada.locator('clipmux-player').count()
          },
          { timeout: 300_000, intervals: [5_000] },
        )
        .toBeGreaterThan(0)
    })

    await test.step('an enrolled student plays it', async () => {
      const ben = await as(browser, 'ben', seen)
      await ben.goto(lesson)
      const ok = await ben.evaluate(() => MediaSource.isTypeSupported('video/mp4; codecs="avc1.64001f"'))
      expect(ok, 'this Chromium build has no H.264; set CLIPMUX_E2E_CHANNEL=chrome').toBe(true)
      await expect.poll(() => media(ben), { timeout: 30_000 }).not.toBeNull()
      await ben.evaluate(() => {
        const p = document.querySelector('clipmux-player')
        const v = (p?.media ?? document.querySelector('hlsjs-video')) as HTMLVideoElement
        v.muted = true
        void v.play()
      })
      await expect
        .poll(async () => (await media(ben))?.currentTime ?? 0, { timeout: 120_000 })
        .toBeGreaterThan(5)
    })

    await test.step('a visitor who is not enrolled loads no media', async () => {
      const cy = await as(browser, 'cy', seen)
      const loads: string[] = []
      cy.on('request', (r) => {
        if (r.url().includes('/v/')) loads.push(r.url())
      })
      await cy.goto(lesson)
      await cy.waitForTimeout(10_000)
      expect(loads).toEqual([])
    })

    await test.step('no response the browser got from the app holds a secret', async () => {
      expect(seen.length).toBeGreaterThan(0)
      for (const body of seen)
        for (const s of secrets) expect(body.includes(s), 'a response holds a secret').toBe(false)
    })
  } finally {
    await Promise.all(runs.map((r) => r.stop()))
    await test.info().attach('output', { body: runs.map((r) => r.output).join('\n----\n') })
  }
})
