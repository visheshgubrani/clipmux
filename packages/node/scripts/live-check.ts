// The SDK release gate against a live ClipMux (docs/operations.md, "Release
// gate"). It drives @clipmux/node and @clipmux/uploader as an app would,
// with a dedicated test app's credentials, and prints one line per check.
// It never prints a secret, a token or a signed URL.
//
//   CLIPMUX_URL=… CLIPMUX_API_KEY=… CLIPMUX_SIGNING_KID=… CLIPMUX_SIGNING_KEY=… \
//     node scripts/live-check.ts --env-file .env.live CLIP.mp4
//
// It imports the packages by name, so run it where the versions under test
// are installed: here (the workspace build) or in a project with the packed
// tarballs or the published packages. Every asset it makes is deleted at
// the end. Exit 0 means every check passed.

import { spawn } from 'node:child_process'
import { openAsBlob } from 'node:fs'
import { readFile } from 'node:fs/promises'
import { createServer } from 'node:http'
import type { AddressInfo } from 'node:net'
import { basename } from 'node:path'
import { parseArgs } from 'node:util'
import { type Asset, ClipMux, ClipMuxError, verifyWebhook, type WebhookEvent } from '@clipmux/node'
import { clipmuxHandler } from '@clipmux/node/handlers'
import { ClipMuxUpload } from '@clipmux/uploader'

const { values, positionals } = parseArgs({
  allowPositionals: true,
  options: { 'env-file': { type: 'string' }, 'skip-tracks': { type: 'boolean' } },
})
const clip = positionals[0]
if (!clip) {
  console.error('usage: node scripts/live-check.ts [--env-file FILE] [--skip-tracks] CLIP.mp4')
  process.exit(2)
}

// The env file, then the environment, as the SDK's CLI reads it.
const env: Record<string, string | undefined> = { ...process.env }
if (values['env-file']) {
  for (const line of (await readFile(values['env-file'], 'utf8')).split('\n')) {
    const m = /^\s*(?:export\s+)?([A-Z0-9_]+)\s*=\s*(.*)$/.exec(line)
    if (m?.[1]) env[m[1]] = (m[2] ?? '').replace(/^(['"])(.*)\1$/, '$2').replace(/\\n/g, '\n')
  }
}
// listen and the handler need a webhook secret; a local one does for the gate.
env.CLIPMUX_WEBHOOK_SECRET ||= `whsec_local_${crypto.randomUUID().replace(/-/g, '')}`

const cm = new ClipMux({ env })
const results: { name: string; ok: boolean; detail: string }[] = []
const made: string[] = []

async function check(name: string, fn: () => Promise<string | undefined>): Promise<boolean> {
  try {
    const detail = (await fn()) ?? ''
    results.push({ name, ok: true, detail })
    console.log(`✓ ${name}${detail ? `: ${detail}` : ''}`)
    return true
  } catch (err) {
    const detail = err instanceof Error ? err.message : String(err)
    results.push({ name, ok: false, detail })
    console.log(`✗ ${name}: ${detail}`)
    return false
  }
}

function assert(cond: unknown, message: string): asserts cond {
  if (!cond) throw new Error(message)
}

const claims = (token: string) =>
  JSON.parse(Buffer.from(token.split('.')[1] ?? '', 'base64url').toString()) as {
    sub: string
    aud: string
    iat: number
    exp: number
    vid?: string
  }

// status fetches a URL and returns only its status: signed URLs stay unprinted.
const status = async (url: string, init?: RequestInit) => (await fetch(url, init)).status

// --- The app and its credentials -------------------------------------------

const app = await cm.app.get()
await check('GET /v1/app', async () => {
  for (const s of ['uploads:write', 'assets:read', 'assets:write'])
    assert(app.scopes.includes(s), `the key lacks ${s}`)
  assert(Array.isArray(app.upload_origins) && app.upload_origins.length > 0, 'no upload_origins')
  return `app "${app.name}", media ${new URL(app.base_url).origin}, uploads to ${app.upload_origins.join(' ')}`
})

await check('doctor', async () => {
  if (!values['env-file']) return 'skipped (needs --env-file)'
  const bin = new URL(import.meta.resolve('@clipmux/node')).pathname.replace(/index\.js$/, 'bin.js')
  const out = await new Promise<{ code: number; text: string }>((resolve) => {
    const p = spawn(process.execPath, [bin, 'doctor', '--env-file', values['env-file'] as string], {
      env: { ...process.env, CLIPMUX_WEBHOOK_SECRET: env.CLIPMUX_WEBHOOK_SECRET },
    })
    let text = ''
    p.stdout.on('data', (d) => {
      text += d
    })
    p.stderr.on('data', (d) => {
      text += d
    })
    p.on('close', (code) => resolve({ code: code ?? 1, text }))
  })
  // A local webhook secret is a warning, not a failure.
  assert(/connect-src /.test(out.text), 'doctor printed no CSP sources')
  const failures = out.text.split('\n').filter((l) => l.startsWith('✗') && !/WEBHOOK_SECRET/.test(l))
  assert(failures.length === 0, failures.join('; '))
  for (const k of ['CLIPMUX_API_KEY', 'CLIPMUX_SIGNING_KEY'])
    assert(!env[k] || !out.text.includes(env[k] as string), `doctor printed ${k}`)
  return out.text.match(/connect-src .*/)?.[0] ?? ''
})

// --- The handler passes ClipMux's refusals on (A3) ------------------------

const handler = clipmuxHandler({
  clipmux: cm,
  upload: (req) => {
    const policy = new URL(req.url).searchParams.get('policy')
    return { passthrough: 'live-check', playback_policy: (policy ?? 'signed') as 'signed' }
  },
  onError: () => {},
})
const post = (path: string, body: unknown) =>
  handler(
    new Request(`https://app.example${path}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }),
  )

await check('handler passes 413 too_large through', async () => {
  const res = await post('/api/clipmux/upload', { size: 2 ** 50 })
  const body = (await res.json()) as { error?: { code: string } }
  assert(res.status === 413 && body.error?.code === 'too_large', `got ${res.status} ${body.error?.code}`)
  return '413 too_large'
})
await check('handler passes 400 invalid_request through', async () => {
  const res = await post('/api/clipmux/upload?policy=bogus', { size: 1000 })
  const body = (await res.json()) as { error?: { code: string } }
  assert(
    res.status === 400 && body.error?.code === 'invalid_request',
    `got ${res.status} ${body.error?.code}`,
  )
  return '400 invalid_request'
})

// --- Webhooks through listen (A1), into a receiver that verifies -----------

const events: WebhookEvent[] = []
const receiver = createServer((req, res) => {
  const chunks: Buffer[] = []
  req.on('data', (c: Buffer) => chunks.push(c))
  req.on('end', async () => {
    const raw = Buffer.concat(chunks)
    const headers = new Headers()
    for (const [k, v] of Object.entries(req.headers)) if (typeof v === 'string') headers.set(k, v)
    try {
      events.push(await verifyWebhook(env.CLIPMUX_WEBHOOK_SECRET as string, raw, headers))
      res.writeHead(200).end()
    } catch {
      res.writeHead(400).end()
    }
  })
})
await new Promise<void>((r) => receiver.listen(0, '127.0.0.1', r))
const port = (receiver.address() as AddressInfo).port
const bin = new URL(import.meta.resolve('@clipmux/node')).pathname.replace(/index\.js$/, 'bin.js')
const listener = spawn(process.execPath, [bin, 'listen', '--forward-to', `127.0.0.1:${port}/hook`], {
  env: { ...process.env, ...(env as Record<string, string>) },
  stdio: ['ignore', 'pipe', 'pipe'],
})
let listenLog = ''
listener.stdout.on('data', (d) => {
  listenLog += d
})
listener.stderr.on('data', (d) => {
  listenLog += d
})
await new Promise((r) => setTimeout(r, 3000))

// --- Upload (A2) and the part-length bound (finding 5) ----------------------

const file = await openAsBlob(clip)
let asset: Asset | undefined
await check('uploads.create → ClipMuxUpload', async () => {
  const upload = await cm.uploads.create({
    size: file.size,
    title: `live-check ${basename(clip)}`,
    passthrough: `live-check-${Date.now()}`,
    playback_policy: 'signed',
    ...(values['skip-tracks'] ? {} : { generate: { subtitles: true } }),
  })
  made.push(upload.asset_id as string)
  const parts = new Set<string>()
  const done = await new ClipMuxUpload(file, {
    upload,
    fetch: (input, init) => {
      const u = new URL(String(input))
      if (init?.method === 'PUT') parts.add(u.origin)
      return fetch(input, init)
    },
  }).start()
  return `asset ${done.id}, parts to ${[...parts].join(' ')}`
})

await check('a part of the wrong length is refused', async () => {
  const size = 5 * 1024 * 1024 + 7
  const upload = await cm.uploads.create({ size, passthrough: 'live-check-length' })
  made.push(upload.asset_id as string)
  const res = await fetch(`${upload.endpoint}/v1/uploads/${upload.upload_id}/parts`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${upload.upload_token}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ part_numbers: [1] }),
  })
  assert(res.ok, `signing a part: ${res.status}`)
  const { parts } = (await res.json()) as { parts: { url: string }[] }
  const url = parts[0]?.url as string
  const expected = Math.min(upload.part_size, size)
  const wrong = await status(url, { method: 'PUT', body: new Uint8Array(expected + 1) })
  assert(wrong >= 400 && wrong < 500, `a part 1 byte too long got ${wrong}`)
  await cm.uploads.cancel(upload.upload_id)
  return `${expected + 1} bytes for a ${expected}-byte part: ${wrong}`
})

await check('waitUntilReady', async () => {
  asset = await cm.assets.waitUntilReady(made[0] as string, { timeoutMs: 20 * 60_000 })
  return `ready, ${asset.duration_seconds?.toFixed(1)} s`
})

// --- Playback: session, poster token (A5), delivery, native tokens (A4) -----

await check('playback.session and delivery', async () => {
  assert(asset, 'no ready asset')
  const s = await cm.playback.session(asset, { viewerId: 'live-check' })
  const c = claims(s.token)
  assert(c.aud === 'v' && c.exp - c.iat === 900 && c.vid === 'live-check', 'playback token claims')
  const posterToken = new URL(s.poster_url).pathname.split('/')[2] as string
  const p = claims(posterToken)
  assert(p.aud === 't' && p.exp === c.exp, `poster token aud ${p.aud}`)
  const root = `${s.base_url}/v/${s.token}/${s.playback_id}`
  const master = await (await fetch(`${root}/master.m3u8`)).text()
  assert(master.startsWith('#EXTM3U'), 'master playlist')
  const variant = master.split('\n').find((l) => l && !l.startsWith('#')) as string
  const media = await (await fetch(`${root}/${variant}`)).text()
  const segment = media.split('\n').find((l) => l && !l.startsWith('#')) as string
  const key = /URI="([^"]+)"/.exec(media)?.[1]
  const base = `${root}/${variant.replace(/[^/]+$/, '')}`
  assert((await status(`${base}${segment}`)) === 200, 'segment')
  if (key) assert((await status(new URL(key, `${base}x`).href)) === 200, 'key')
  assert((await status(s.poster_url)) === 200, 'poster')
  const posterOnMaster = await status(`${s.base_url}/v/${posterToken}/${s.playback_id}/master.m3u8`)
  assert(posterOnMaster === 403, `the poster token played the master: ${posterOnMaster}`)
  return `master, segment, ${key ? 'key, ' : ''}poster 200; poster token on the master ${posterOnMaster}`
})

await check('playback.token native lasts the session', async () => {
  assert(asset, 'no ready asset')
  const pid = asset.playback_ids.find((x) => x.policy === 'signed' && x.status === 'active')?.id as string
  const t = claims(await cm.playback.token(pid, { native: true, durationSeconds: asset.duration_seconds }))
  assert(t.exp - t.iat === Math.min(app.max_token_lifetime_seconds, 4 * 3600), `ttl ${t.exp - t.iat}`)
  return `${(t.exp - t.iat) / 3600} h`
})

// --- Subtitles and the transcript -------------------------------------------

if (!values['skip-tracks']) {
  await check('subtitles track and transcript', async () => {
    assert(asset, 'no ready asset')
    const deadline = Date.now() + 15 * 60_000
    for (;;) {
      const a = await cm.assets.get(asset.id)
      const t = a.tracks.find((x) => x.kind === 'subtitles')
      if (t?.status === 'ready') {
        const tr = await cm.tracks.transcript(a.id, t.id)
        return `track ${t.id} ready, transcript ${tr.text?.length ?? 0} chars`
      }
      assert(t?.status !== 'errored', `the subtitles track errored: ${t?.error_code}`)
      assert(Date.now() < deadline, 'no subtitles after 15 minutes')
      await new Promise((r) => setTimeout(r, 10_000))
    }
  })
}

await check('listen forwarded verified events', async () => {
  const deadline = Date.now() + 60_000
  while (events.length < 2 && Date.now() < deadline) await new Promise((r) => setTimeout(r, 1000))
  assert(events.length >= 2, `${events.length} events; listen said: ${listenLog.trim().split('\n').at(-1)}`)
  assert(listener.exitCode === null, 'listen exited')
  return [...new Set(events.map((e) => e.type))].join(', ')
})

// --- Clean up ---------------------------------------------------------------

listener.kill()
receiver.close()
for (const id of made) {
  await cm.assets.delete(id).catch((err: unknown) => {
    if (!(err instanceof ClipMuxError && (err.status === 404 || err.code === 'asset_deleted'))) throw err
  })
}
const failed = results.filter((r) => !r.ok)
console.log(
  `\n${results.length - failed.length}/${results.length} passed; ${made.length} test assets deleted.`,
)
process.exit(failed.length ? 1 : 0)
