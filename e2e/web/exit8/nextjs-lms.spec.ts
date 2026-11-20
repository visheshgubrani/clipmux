import { type ChildProcess, spawn } from 'node:child_process'
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { parseEnv } from 'node:util'
import { type Browser, expect, type Page, test } from '@playwright/test'

// Launch exit check 8 (P6-T28, D112), in examples/nextjs-lms against the
// real binary: `init` approved through /connect writes the env file, the
// uploader element uploads, the webhook (and separately `listen`) marks the
// lesson ready, the player plays it signed through session-url across a
// 30 s token refresh, a denied viewer gets clipmux-denied, `doctor` passes,
// and no secret appears in the CLI's output.

interface Config {
  ops: string
  ops_email: string
  ops_password: string
  video: string
}

const example = resolve(import.meta.dirname, '../../../examples/nextjs-lms')

/** A command the suite runs, with everything it printed. */
class Run {
  readonly proc: ChildProcess
  output = ''
  readonly exited: Promise<number | null>

  constructor(cmd: string, args: string[], env: Record<string, string | undefined> = {}) {
    // Its own process group, so stop() reaches what npx starts as well.
    this.proc = spawn(cmd, args, { cwd: example, env: { ...process.env, ...env }, detached: true })
    const add = (b: Buffer) => {
      this.output += b.toString()
    }
    this.proc.stdout?.on('data', add)
    this.proc.stderr?.on('data', add)
    this.exited = new Promise((done) => this.proc.on('exit', (code) => done(code)))
  }

  async waitFor(re: RegExp, timeout = 30_000): Promise<RegExpMatchArray> {
    let m: RegExpMatchArray | null = null
    try {
      await expect.poll(() => (m = this.output.match(re)), { timeout }).not.toBeNull()
    } catch (err) {
      throw new Error(`no ${re} in the output:\n${this.output}`, { cause: err })
    }
    return m as unknown as RegExpMatchArray
  }

  async stop(): Promise<number | null> {
    const group = (signal: NodeJS.Signals) => {
      try {
        if (this.proc.pid) process.kill(-this.proc.pid, signal)
      } catch {
        // already gone
      }
    }
    if (this.proc.exitCode === null && this.proc.signalCode === null) {
      group('SIGINT')
      const killed = setTimeout(() => group('SIGKILL'), 5_000)
      await this.exited
      clearTimeout(killed)
    }
    return this.exited
  }
}

// `npx @clipmux/node …` from the example, as its README says; --no keeps
// npx from installing anything: the workspace's build is what runs.
const cli = (args: string[]) => new Run('npx', ['--no', '@clipmux/node', ...args])

function freePort(): Promise<number> {
  return new Promise((done, fail) => {
    const s = createServer()
    s.on('error', fail)
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address() as { port: number }
      s.close(() => done(port))
    })
  })
}

/** A browser of one of the example's users, signed in through its header. */
async function as(browser: Browser, app: string, user: string): Promise<Page> {
  const page = await (await browser.newContext({ baseURL: app })).newPage()
  await page.goto('/')
  await page.getByLabel('Signed in as').selectOption(user)
  // The switch is a server action that sets a cookie: wait for it, then
  // check the server renders the page for this user.
  const action = page.waitForResponse((r) => r.request().method() === 'POST')
  await page.getByRole('button', { name: 'Switch' }).click()
  await action
  await page.reload()
  await expect(page.getByLabel('Signed in as')).toHaveValue(user)
  return page
}

async function addLessonAndUpload(page: Page, title: string, video: string): Promise<void> {
  await page.goto('/')
  await page.getByLabel('Title').fill(title)
  await page.getByRole('button', { name: 'Add lesson' }).click()
  await expect(page.getByRole('heading', { name: title, level: 1 })).toBeVisible()
  const uploader = page.locator('clipmux-uploader')
  await uploader.locator('input[type=file]').setInputFiles(video)
  await expect(uploader).toHaveAttribute('state', 'done', { timeout: 120_000 })
}

const media = (page: Page) =>
  page.evaluate(() => {
    const v = document.querySelector('hlsjs-video') as unknown as HTMLVideoElement | null
    if (!v) return null
    return {
      currentTime: v.currentTime,
      played: Array.from({ length: v.played.length }, (_, i) => [v.played.start(i), v.played.end(i)]),
    }
  })

test('the four-step integration works in examples/nextjs-lms', async ({ page, browser, request }) => {
  const cfg = (await (await request.get('/config')).json()) as Config
  const dir = await mkdtemp(join(tmpdir(), 'clipmux-exit8-'))
  const envFile = join(dir, '.env.local')
  const port = await freePort()
  const app = `http://127.0.0.1:${port}`
  const runs: Run[] = []
  try {
    // 1. init, approved by the owner on /connect, writes the env file. Node
    // itself reads any `--env-file FILE` in argv, even after the script, and
    // exits when FILE is missing, so it exists (empty) first (known gaps).
    await writeFile(envFile, '', { mode: 0o600 })
    const init = cli(['init', '--server', cfg.ops, '--env-file', envFile, '--no-open'])
    runs.push(init)
    const [, approve] = await test.step('init prints the approval URL', () => init.waitFor(/Approve: (\S+)/))
    const code = init.output.match(/Code: +(\S+)/)?.[1] ?? ''
    await test.step('the owner approves on /connect', async () => {
      await page.goto(`${cfg.ops}/login`)
      await page.getByLabel('Email').fill(cfg.ops_email)
      await page.getByLabel('Password', { exact: true }).fill(cfg.ops_password)
      await page.getByRole('button', { name: 'Sign in' }).click()
      await expect(page.getByRole('button', { name: 'Upload', exact: true }).first()).toBeVisible()
      await page.goto(approve as string)
      await expect(page.getByRole('heading', { name: 'Connect this app?' })).toBeVisible()
      // Both screens show the code, so the owner can match them.
      await expect(page.getByText(code, { exact: true })).toBeVisible()
      await page.getByLabel('App', { exact: true }).selectOption({ label: 'New app…' })
      await page.getByLabel('New app name').fill('Course site')
      await page.getByLabel('Webhook URL (optional)').fill(`${app}/api/clipmux/webhook`)
      await page.getByRole('checkbox', { name: 'This app runs on this computer' }).check()
      await page.getByRole('button', { name: 'Connect', exact: true }).click()
      await expect(page.getByRole('heading', { name: 'Connected' })).toBeVisible()
    })
    expect(await init.exited, init.output).toBe(0)
    const appID = init.output.match(/Approved for app "Course site" \((app_\w+)\)/)?.[1]
    expect(appID, init.output).toBeTruthy()
    const env = parseEnv(await readFile(envFile, 'utf8'))
    expect(Object.keys(env).sort()).toEqual([
      'CLIPMUX_API_KEY',
      'CLIPMUX_SIGNING_KEY',
      'CLIPMUX_SIGNING_KID',
      'CLIPMUX_URL',
      'CLIPMUX_WEBHOOK_SECRET',
    ])
    expect(env.CLIPMUX_URL).toBe(cfg.ops)
    expect(env.CLIPMUX_WEBHOOK_SECRET).toMatch(/^whsec_(?!local_)/)

    // 6. doctor passes on what init wrote.
    const doctor = cli(['doctor', '--env-file', envFile])
    runs.push(doctor)
    expect(await doctor.exited, doctor.output).toBe(0)
    expect(doctor.output).toContain('✓ Ready.')

    // The example starts on the env file init wrote, with 30 s tokens.
    const next = new Run(
      'npx',
      ['--no', 'next', 'start', '--port', String(port), '--hostname', '127.0.0.1'],
      {
        ...env,
        LMS_DATA_DIR: join(dir, 'data'),
        LMS_TOKEN_TTL: '30s',
      },
    )
    runs.push(next)
    await next.waitFor(/Ready in|started server/i, 60_000)

    // 2 and 3. The instructor uploads through the element; ClipMux's webhook
    // marks the lesson ready.
    const ada = await as(browser, app, 'ada')
    await test.step('the uploader uploads and the webhook marks the lesson ready', async () => {
      await addLessonAndUpload(ada, 'Lesson one', cfg.video)
      await expect(ada.getByLabel('Video status')).toHaveText('Ready', { timeout: 300_000 })
    })
    const lessonOne = new URL(ada.url()).pathname

    // 3, separately: with the endpoint disabled, only `listen` delivers.
    await test.step('listen marks a second lesson ready', async () => {
      const off = await request.post(`/disable-webhooks?app=${appID}`)
      expect(await off.json()).toEqual({ disabled: 1 })
      const listen = cli([
        'listen',
        '--forward-to',
        `127.0.0.1:${port}/api/clipmux/webhook`,
        '--env-file',
        envFile,
      ])
      runs.push(listen)
      await listen.waitFor(/Forwarding "Course site" events/)
      await addLessonAndUpload(ada, 'Lesson two', cfg.video)
      await expect(ada.getByLabel('Video status')).toHaveText('Ready', { timeout: 300_000 })
      await listen.waitFor(/✓ video\.asset\.ready ast_\w+ → 200/)
      await listen.stop()
    })

    // 4. An enrolled student plays it signed through session-url, past the
    // 30 s token's expiry.
    await test.step('the player plays across a 30 s token refresh', async () => {
      const ben = await as(browser, app, 'ben')
      // A short buffer, as in session.spec.ts: hls.js otherwise fetches the
      // whole clip at once, before the token can expire. The element takes
      // hls.js options before it loads; the example sets none.
      await ben.addInitScript(() => {
        const define = customElements.define.bind(customElements)
        customElements.define = (name, cls, options) => {
          if (name === 'clipmux-player') {
            const connected = cls.prototype.connectedCallback
            cls.prototype.connectedCallback = function (this: HTMLElement & { hls: object }) {
              this.hls = { maxBufferLength: 4, maxMaxBufferLength: 4 }
              connected?.call(this)
            }
          }
          define(name, cls, options)
        }
      })
      const sessions: { ttl: number }[] = []
      ben.on('response', async (r) => {
        if (new URL(r.url()).pathname !== '/api/clipmux/playback' || r.status() !== 200) return
        const { token } = (await r.json()) as { token: string }
        const c = JSON.parse(Buffer.from(token.split('.')[1] ?? '', 'base64url').toString())
        sessions.push({ ttl: c.exp - c.iat })
      })
      await ben.goto(lessonOne)
      const ok = await ben.evaluate(() => MediaSource.isTypeSupported('video/mp4; codecs="avc1.64001f"'))
      expect(ok, 'this Chromium build has no H.264; set CLIPMUX_E2E_CHANNEL=chrome').toBe(true)
      await expect.poll(() => media(ben), { timeout: 30_000 }).not.toBeNull()
      await ben.evaluate(() => {
        const v = document.querySelector('hlsjs-video') as unknown as HTMLVideoElement
        v.muted = true
        void v.play()
      })
      await expect
        .poll(async () => (await media(ben))?.currentTime ?? 0, { timeout: 120_000 })
        .toBeGreaterThan(40)
      expect(sessions.length, 'the player asked the route again for a fresh token').toBeGreaterThanOrEqual(2)
      for (const s of sessions) expect(s.ttl).toBe(30)
      const m = await media(ben)
      expect(m?.played.length, `played ranges ${JSON.stringify(m?.played)}: one, from the start`).toBe(1)
      expect(m?.played[0]?.[0]).toBeLessThan(1)
    })

    // 5. A visitor who isn't enrolled is refused: clipmux-denied, nothing loads.
    await test.step('a viewer who is not enrolled gets clipmux-denied', async () => {
      const cy = await as(browser, app, 'cy')
      const loads: string[] = []
      cy.on('request', (r) => {
        if (r.url().includes('/v/')) loads.push(r.url())
      })
      await cy.goto(lessonOne)
      await expect(cy.getByText("You aren't enrolled")).toBeVisible({ timeout: 20_000 })
      expect(loads).toEqual([])
    })

    // 7. No command printed a secret.
    const printed = runs
      .filter((r) => r !== next)
      .map((r) => r.output)
      .join('\n')
    const pemBody = (env.CLIPMUX_SIGNING_KEY ?? '').split(/\\n|\n/).filter((l) => l && !l.startsWith('-----'))
    expect(pemBody.length).toBeGreaterThan(0)
    for (const secret of [env.CLIPMUX_API_KEY, env.CLIPMUX_WEBHOOK_SECRET, ...pemBody]) {
      expect(secret && secret.length >= 16).toBe(true)
      expect(printed.includes(secret as string), 'the CLI printed a secret').toBe(false)
    }
  } finally {
    await Promise.all(runs.map((r) => r.stop()))
    // What each command printed, for a failure's report (it holds no secret:
    // step 7 checks the CLI's, and the example logs none).
    await test.info().attach('output', { body: runs.map((r) => r.output).join('\n----\n') })
    await rm(dir, { recursive: true, force: true })
  }
})
