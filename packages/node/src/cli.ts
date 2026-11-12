// npx @clipmux/node <command>: init, doctor, listen, upload.
//
// Written for people and coding agents alike: it never prints a secret
// (an agent's transcript is a log), every step says what it did, and the
// exit code says whether it worked.

import { execFile } from 'node:child_process'
import { existsSync, openAsBlob } from 'node:fs'
import { chmod, readFile, writeFile } from 'node:fs/promises'
import { hostname } from 'node:os'
import { basename, resolve } from 'node:path'
import { parseArgs } from 'node:util'
import { ClipMuxUpload } from '@clipmux/uploader'
import { ClipMux, VERSION } from './client.ts'
import { mergeEnv, parseEnv } from './envfile.ts'
import { ClipMuxConfigError, ClipMuxError } from './errors.ts'
import type { App, Credentials, Pairing, PairingStatus } from './schema.gen.ts'
import { signWebhook } from './webhooks.ts'

const USAGE = `Usage: npx @clipmux/node <command> [options]

Commands:
  init     Connect this app to a ClipMux server: the owner approves in the
           dashboard, and the credentials go to an env file (never printed).
             --server URL      the ClipMux public URL (default CLIPMUX_URL)
             --env-file FILE   where to write (default .env.local, or .env if it exists)
             --name NAME       the name the owner sees (default package.json's name)
             --no-open         print the approval URL instead of opening a browser
  doctor   Check the env file: the server, the API key, the signing key, the webhook secret.
  listen   Forward this app's webhook events to a local URL, signed with
           CLIPMUX_WEBHOOK_SECRET, so the app's handler verifies them unchanged.
             --forward-to URL  e.g. localhost:3000/api/clipmux/webhook
             --events LIST     only these types, comma separated
  upload   Upload a file: npx @clipmux/node upload FILE [--passthrough REF] [--title T] [--public] [--wait]

Every command takes --env-file FILE.
`

export interface IO {
  out: (line: string) => void
  err: (line: string) => void
  env: Record<string, string | undefined>
  cwd: string
  fetch: typeof fetch
  sleep: (ms: number) => Promise<void>
  open: (url: string) => void
  signal?: AbortSignal
}

const ok = '✓'
const bad = '✗'
const warn = '!'

/** Runs a command and returns its exit code. */
export async function main(argv: string[], io: IO): Promise<number> {
  const [command, ...rest] = argv
  try {
    switch (command) {
      case 'init':
        return await init(rest, io)
      case 'doctor':
        return await doctor(rest, io)
      case 'listen':
        return await listen(rest, io)
      case 'upload':
        return await upload(rest, io)
      case undefined:
      case '-h':
      case '--help':
      case 'help':
        io.out(USAGE)
        return command === undefined ? 2 : 0
      case '--version':
        io.out(VERSION)
        return 0
      default:
        io.err(`unknown command ${command}\n\n${USAGE}`)
        return 2
    }
  } catch (err) {
    if (err instanceof ClipMuxError || err instanceof ClipMuxConfigError || err instanceof TypeError) {
      io.err(`${bad} ${err.message}`)
      return 1
    }
    throw err
  }
}

function envPath(io: IO, flag: string | undefined): string {
  if (flag) return resolve(io.cwd, flag)
  for (const name of ['.env.local', '.env'])
    if (existsSync(resolve(io.cwd, name))) return resolve(io.cwd, name)
  return resolve(io.cwd, '.env.local')
}

// loadEnv reads the env file under the process environment, which wins, as
// dotenv and Next.js do.
async function loadEnv(io: IO, flag: string | undefined): Promise<Record<string, string | undefined>> {
  const path = envPath(io, flag)
  const file = existsSync(path) ? parseEnv(await readFile(path, 'utf8')) : {}
  return { ...file, ...io.env }
}

function normalizeServer(s: string): string {
  const url = /^https?:\/\//.test(s) ? s : `https://${s}`
  return new URL(url).origin
}

async function projectName(cwd: string): Promise<string> {
  try {
    const pkg = JSON.parse(await readFile(resolve(cwd, 'package.json'), 'utf8')) as { name?: string }
    if (pkg.name) return pkg.name.slice(0, 100)
  } catch {
    // no package.json: fall back to the directory's name
  }
  return basename(cwd).slice(0, 100)
}

async function api<T>(io: IO, server: string, path: string, body: unknown): Promise<T> {
  const res = await io.fetch(`${server}${path}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'User-Agent': `clipmux-node/${VERSION}` },
    body: JSON.stringify(body),
  })
  const parsed = (await res.json().catch(() => null)) as { error?: { code: string; message: string } } | null
  if (!res.ok) {
    throw new ClipMuxError(
      res.status,
      parsed?.error?.code ?? `http_${res.status}`,
      parsed?.error?.message ?? res.statusText,
      `POST ${path}`,
    )
  }
  return parsed as T
}

async function init(args: string[], io: IO): Promise<number> {
  const { values } = parseArgs({
    args,
    options: {
      server: { type: 'string' },
      'env-file': { type: 'string' },
      name: { type: 'string' },
      'no-open': { type: 'boolean' },
    },
  })
  const file = envPath(io, values['env-file'])
  const existing = existsSync(file) ? await readFile(file, 'utf8') : ''
  const raw = values.server ?? parseEnv(existing).CLIPMUX_URL ?? io.env.CLIPMUX_URL
  if (!raw) {
    io.err(`${bad} which ClipMux server? Pass --server https://video.example.com`)
    return 2
  }
  const server = normalizeServer(raw)
  const name = values.name ?? (await projectName(io.cwd))

  const p = await api<Pairing>(io, server, '/v1/app-pairings', {
    name,
    hostname: hostname().slice(0, 253),
    client: `clipmux-node/${VERSION} init`,
  })
  io.out(`Connecting "${name}" to ${server}.`)
  io.out('')
  io.out(`  Code:    ${p.code}`)
  io.out(`  Approve: ${p.verify_url}`)
  io.out('')
  io.out('The ClipMux owner opens that page, checks the code matches, picks the app and approves.')
  if (!values['no-open']) io.open(p.verify_url)
  io.out(`Waiting for approval (until ${new Date(p.expires_at).toLocaleTimeString()})…`)

  let creds: Credentials | undefined
  for (;;) {
    await io.sleep(Math.max(1, p.interval_seconds) * 1000)
    if (io.signal?.aborted) return 130
    const s = await api<PairingStatus>(io, server, '/v1/app-pairings/poll', { poll_token: p.poll_token })
    if (s.status === 'pending') continue
    if (s.status === 'denied') {
      io.err(`${bad} the owner denied this pairing.`)
      return 1
    }
    if (s.status === 'expired') {
      io.err(`${bad} the code expired before it was approved; run init again.`)
      return 1
    }
    if (s.status === 'approved' && s.credentials) {
      creds = s.credentials
      break
    }
  }

  const vars: Record<string, string> = {
    CLIPMUX_URL: creds.url,
    CLIPMUX_API_KEY: creds.api_key,
    CLIPMUX_SIGNING_KID: creds.signing_kid,
    CLIPMUX_SIGNING_KEY: creds.signing_key.trimEnd(),
  }
  let localSecret = false
  if (creds.webhook_secret) vars.CLIPMUX_WEBHOOK_SECRET = creds.webhook_secret
  else if (!parseEnv(existing).CLIPMUX_WEBHOOK_SECRET) {
    // No endpoint yet: a local secret lets `listen` and the handler agree.
    vars.CLIPMUX_WEBHOOK_SECRET = `whsec_local_${crypto.randomUUID().replaceAll('-', '')}`
    localSecret = true
  }
  const created = !existsSync(file)
  await writeFile(file, mergeEnv(existing, vars, `# ClipMux: ${creds.app_name} (${creds.app_id})`))
  if (created) await chmod(file, 0o600)
  io.out('')
  io.out(`${ok} Approved for app "${creds.app_name}" (${creds.app_id}).`)
  io.out(`${ok} Wrote ${Object.keys(vars).join(', ')} to ${basename(file)}.`)
  if (localSecret) {
    io.out(
      `${warn} No webhook endpoint yet: CLIPMUX_WEBHOOK_SECRET is a local secret for \`npx @clipmux/node listen\`. In production, add your webhook URL in the dashboard and use its secret.`,
    )
  }
  if (!(await gitIgnored(file, io.cwd))) {
    io.out(`${warn} ${basename(file)} is not gitignored: add it to .gitignore before committing.`)
  }
  io.out('')
  io.out(await nextSteps(io.cwd))
  return 0
}

function gitIgnored(file: string, cwd: string): Promise<boolean> {
  return new Promise((done) => {
    execFile('git', ['check-ignore', '-q', file], { cwd }, (err) => {
      if (!err) return done(true)
      // 1: not ignored. 128 or no git: not a repository, nothing to commit.
      done((err as { code?: number }).code !== 1)
    })
  })
}

async function nextSteps(cwd: string): Promise<string> {
  let deps: Record<string, string> = {}
  try {
    const pkg = JSON.parse(await readFile(resolve(cwd, 'package.json'), 'utf8'))
    deps = { ...pkg.dependencies, ...pkg.devDependencies }
  } catch {
    // no package.json
  }
  const handler = `clipmuxHandler({
  upload: async (req, { file }) => ({ passthrough: 'lesson_42', title: file.name }), // null refuses
  playback: async (req, { playbackId }) => ({ viewerId: 'user_1' }),                 // null refuses
  asset: async (asset) => { /* store asset.status and asset.playback_ids[0].id by asset.passthrough */ },
})`
  let route: string
  if (deps.next) {
    route = `Next steps (Next.js):
  1. app/api/clipmux/[action]/route.ts
       import { clipmuxHandler } from '@clipmux/node/handlers'
       export const POST = ${handler.replaceAll('\n', '\n       ')}`
  } else if (deps.express) {
    route = `Next steps (Express), before app.use(express.json()):
  1. import { clipmuxHandler } from '@clipmux/node/handlers'
     import { toNodeListener } from '@clipmux/node/express'
     app.post('/api/clipmux/:action', toNodeListener(${handler.replaceAll('\n', '\n     ')}))`
  } else {
    route = `Next steps:
  1. Route POST /api/clipmux/{upload,playback,webhook} to a Fetch handler (Hono, Bun, Workers, …):
     import { clipmuxHandler } from '@clipmux/node/handlers'
     const handle = ${handler.replaceAll('\n', '\n     ')}`
  }
  return `${route}
  2. In the page: <clipmux-uploader create-url="/api/clipmux/upload"> (@clipmux/uploader/element)
                  <clipmux-player playback-id="pid_…" session-url="/api/clipmux/playback"> (@clipmux/player/element)
  3. Webhooks while developing: npx @clipmux/node listen --forward-to localhost:3000/api/clipmux/webhook
  4. Check it all: npx @clipmux/node doctor`
}

async function doctor(args: string[], io: IO): Promise<number> {
  const { values } = parseArgs({ args, options: { 'env-file': { type: 'string' } } })
  const env = await loadEnv(io, values['env-file'])
  let failed = false
  const fail = (line: string) => {
    failed = true
    io.out(`${bad} ${line}`)
  }

  const url = env.CLIPMUX_URL
  if (!url || !/^https?:\/\//.test(url)) {
    fail('CLIPMUX_URL is not an http(s) URL; run `npx @clipmux/node init --server URL`.')
    return 1
  }
  io.out(`${ok} CLIPMUX_URL is ${new URL(url).origin}`)
  if (/^https?:\/\/(localhost|127\.|\[::1\])/.test(url)) {
    io.out(`${warn} the server is on loopback: browsers elsewhere can't upload or play.`)
  }
  const cm = new ClipMux({ env, fetch: io.fetch, maxRetries: 1 })
  let app: App
  try {
    app = await cm.app.get()
  } catch (err) {
    fail(
      err instanceof ClipMuxError || err instanceof ClipMuxConfigError
        ? err.message
        : `GET /v1/app failed: ${String(err)}`,
    )
    return 1
  }
  io.out(`${ok} CLIPMUX_API_KEY works for app "${app.name}" (${app.id})`)
  const missing = ['uploads:write', 'assets:read', 'assets:write'].filter((s) => !app.scopes.includes(s))
  if (missing.length) fail(`the API key lacks ${missing.join(', ')}; create a key with the default scopes.`)
  else io.out(`${ok} scopes: ${app.scopes.join(', ')}`)
  io.out(
    `${ok} media URL ${app.base_url}; tokens may live up to ${Math.round(app.max_token_lifetime_seconds / 3600)} h`,
  )

  const kid = env.CLIPMUX_SIGNING_KID
  const key = app.signing_keys.find((k) => k.kid === kid)
  if (!kid) fail('CLIPMUX_SIGNING_KID is not set.')
  else if (!key) fail(`CLIPMUX_SIGNING_KID ${kid} is not an active signing key of this app.`)
  else if (!env.CLIPMUX_SIGNING_KEY) fail('CLIPMUX_SIGNING_KEY is not set.')
  else if (!(await keysMatch(env.CLIPMUX_SIGNING_KEY, key.public_key_pem))) {
    fail(`CLIPMUX_SIGNING_KEY is not the private half of ${kid}.`)
  } else {
    io.out(`${ok} CLIPMUX_SIGNING_KEY matches ${kid}${key.retired_at ? ` (retiring ${key.retired_at})` : ''}`)
  }

  const secret = env.CLIPMUX_WEBHOOK_SECRET
  if (!secret) fail('CLIPMUX_WEBHOOK_SECRET is not set: webhooks cannot be verified.')
  else if (secret.startsWith('whsec_local_')) {
    io.out(`${warn} CLIPMUX_WEBHOOK_SECRET is a local secret, for \`listen\` only.`)
  } else io.out(`${ok} CLIPMUX_WEBHOOK_SECRET is set`)
  io.out(failed ? `\n${bad} Fix the lines above.` : `\n${ok} Ready.`)
  return failed ? 1 : 0
}

async function keysMatch(privateText: string, publicPEM: string): Promise<boolean> {
  try {
    const der = (s: string) =>
      Uint8Array.from(atob(s.replace(/\\n/g, '\n').replace(/-----[A-Z ]+-----|\s/g, '')), (c) =>
        c.charCodeAt(0),
      )
    const alg = { name: 'ECDSA', namedCurve: 'P-256' }
    const priv = await crypto.subtle.importKey('pkcs8', der(privateText), alg, true, ['sign'])
    const pub = await crypto.subtle.importKey('spki', der(publicPEM), alg, true, ['verify'])
    const a = await crypto.subtle.exportKey('jwk', priv)
    const b = await crypto.subtle.exportKey('jwk', pub)
    return a.x === b.x && a.y === b.y
  } catch {
    return false
  }
}

async function listen(args: string[], io: IO): Promise<number> {
  const { values } = parseArgs({
    args,
    options: {
      'forward-to': { type: 'string' },
      events: { type: 'string' },
      'env-file': { type: 'string' },
    },
  })
  if (!values['forward-to']) {
    io.err(`${bad} --forward-to is required, e.g. --forward-to localhost:3000/api/clipmux/webhook`)
    return 2
  }
  const target = /^https?:\/\//.test(values['forward-to'])
    ? values['forward-to']
    : `http://${values['forward-to']}`
  const only = values.events ? new Set(values.events.split(',').map((s) => s.trim())) : null
  const env = await loadEnv(io, values['env-file'])
  const secret = env.CLIPMUX_WEBHOOK_SECRET
  if (!secret) throw new ClipMuxConfigError('CLIPMUX_WEBHOOK_SECRET', 'CLIPMUX_WEBHOOK_SECRET is not set.')
  const cm = new ClipMux({ env, fetch: io.fetch })
  const app = await cm.app.get()
  io.out(`Forwarding "${app.name}" events to ${target}, signed with CLIPMUX_WEBHOOK_SECRET. Ctrl-C stops.`)

  const since = new Date().toISOString()
  let after: string | undefined
  const sent = new Set<string>()
  while (!io.signal?.aborted) {
    let page: Awaited<ReturnType<typeof cm.events.poll>>
    try {
      page = await cm.events.poll({ since, after, wait: 25, signal: io.signal })
    } catch (err) {
      if (io.signal?.aborted) break
      if (err instanceof ClipMuxError && err.status >= 400 && err.status < 500 && err.status !== 429)
        throw err
      await io.sleep(5000)
      continue
    }
    for (const ev of page.data) {
      after = ev.id
      if (sent.has(ev.id) || (only && !only.has(ev.body.type))) continue
      sent.add(ev.id)
      const status = await forward(io, target, secret, ev.id, ev.body.type, JSON.stringify(ev.body))
      io.out(
        `${status >= 200 && status < 300 ? ok : bad} ${ev.body.type} ${ev.body.data?.id ?? ''} → ${status || 'unreachable'}`,
      )
    }
  }
  return 0
}

async function forward(
  io: IO,
  url: string,
  secret: string,
  id: string,
  type: string,
  body: string,
): Promise<number> {
  let status = 0
  for (let attempt = 0; attempt < 4; attempt++) {
    if (attempt) await io.sleep(1000 * 2 ** (attempt - 1))
    const ts = Math.floor(Date.now() / 1000)
    try {
      const res = await io.fetch(url, {
        method: 'POST',
        redirect: 'manual',
        headers: {
          'Content-Type': 'application/json',
          'User-Agent': `clipmux-node/${VERSION} listen`,
          'X-Webhook-Event': type,
          'X-Webhook-Id': id,
          'X-Webhook-Timestamp': String(ts),
          'X-Webhook-Signature': await signWebhook(secret, ts, body),
        },
        body,
      })
      await res.body?.cancel()
      status = res.status
      if (status < 500) return status
    } catch {
      status = 0
    }
  }
  return status
}

async function upload(args: string[], io: IO): Promise<number> {
  const { values, positionals } = parseArgs({
    args,
    allowPositionals: true,
    options: {
      passthrough: { type: 'string' },
      title: { type: 'string' },
      public: { type: 'boolean' },
      wait: { type: 'boolean' },
      'env-file': { type: 'string' },
    },
  })
  const path = positionals[0]
  if (!path) {
    io.err(`${bad} which file? npx @clipmux/node upload FILE`)
    return 2
  }
  const env = await loadEnv(io, values['env-file'])
  const cm = new ClipMux({ env, fetch: io.fetch })
  const file = await openAsBlob(resolve(io.cwd, path))
  const created = await cm.uploads.create({
    size: file.size,
    playback_policy: values.public ? 'public' : 'signed',
    title: values.title ?? basename(path).slice(0, 200),
    ...(values.passthrough ? { passthrough: values.passthrough } : {}),
  })
  if (!created.upload_token) throw new TypeError('the server returned an upload without an upload_token')
  let last = -1
  const asset = await new ClipMuxUpload(file, {
    upload: { ...created, upload_token: created.upload_token },
    fetch: io.fetch,
    onProgress: (p) => {
      const pct = Math.floor((100 * p.bytesUploaded) / Math.max(1, p.bytesTotal))
      if (pct >= last + 10) {
        last = pct
        io.out(`  ${pct}%`)
      }
    },
  }).start()
  io.out(`${ok} uploaded ${basename(path)}: asset ${asset.id} (${asset.status})`)
  if (values.wait) {
    io.out('Waiting until it is ready…')
    const ready = await cm.assets.waitUntilReady(asset.id)
    const pid = ready.playback_ids.find((p) => p.status === 'active')
    io.out(`${ok} ready: ${ready.duration_seconds ?? '?'} s, playback ID ${pid?.id ?? 'none'}`)
  }
  return 0
}
