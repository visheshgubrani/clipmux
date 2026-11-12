import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { ClipMux } from './client.ts'
import { ClipMuxConfigError, ClipMuxError } from './errors.ts'
import type { Asset } from './schema.gen.ts'
import { signWebhook } from './webhooks.ts'

const vectors = JSON.parse(
  readFileSync(new URL('../../../internal/delivery/testdata/vectors.json', import.meta.url), 'utf8'),
)
const signingKey = vectors.signing_keys[0].private_key_pem as string

interface Call {
  method: string
  url: URL
  headers: Headers
  body: unknown
}

// fake answers each request with the next of answers, recording it.
function fake(...answers: (Response | (() => Response))[]) {
  const calls: Call[] = []
  const fetch = (async (input: URL, init: RequestInit) => {
    calls.push({
      method: init.method ?? 'GET',
      url: new URL(input),
      headers: new Headers(init.headers),
      body: init.body ? JSON.parse(init.body as string) : undefined,
    })
    const a = answers.shift()
    if (!a) throw new Error('no answer left')
    return typeof a === 'function' ? a() : a
  }) as typeof globalThis.fetch
  return { calls, fetch }
}
const json = (status: number, body: unknown, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } })

const env = {
  CLIPMUX_URL: 'https://video.example.com/',
  CLIPMUX_API_KEY: 'cmx_test',
  CLIPMUX_SIGNING_KID: 'kid_app_a',
  CLIPMUX_SIGNING_KEY: signingKey,
  CLIPMUX_WEBHOOK_SECRET: 'whsec_test',
}

const asset = (over: Partial<Asset> = {}): Asset => ({
  id: 'ast_1',
  app_id: 'app_1',
  status: 'ready',
  version: 3,
  base_url: 'https://media.example.net',
  playback_ids: [
    {
      id: 'pid_share',
      policy: 'signed',
      playable: true,
      status: 'active',
      share: { id: 'shr_1', name: 'x' },
    },
    { id: 'pid_pub', policy: 'public', playable: true, status: 'active' },
    { id: 'pid_signed', policy: 'signed', playable: true, status: 'active' },
  ],
  duration_seconds: 3 * 3600,
  tracks: [
    { id: 'trk_ready', kind: 'subtitles', source: 'generated', status: 'ready' },
    { id: 'trk_prep', kind: 'chapters', source: 'generated', status: 'preparing' },
  ],
  created_at: '2027-01-01T00:00:00Z',
  updated_at: '2027-01-01T00:00:00Z',
  ...over,
})

const claims = (token: string) =>
  JSON.parse(Buffer.from(token.split('.')[1] as string, 'base64url').toString())

test('uploads.create sends the key and an idempotency key, and adds endpoint for the browser', async () => {
  const f = fake(
    json(201, { upload_id: 'upl_1', asset_id: 'ast_1', part_size: 1, part_count: 1, playback_ids: [] }),
  )
  const cm = new ClipMux({ env, fetch: f.fetch })
  const up = await cm.uploads.create({ size: 10, passthrough: 'lesson_42' })
  assert.equal(up.endpoint, 'https://video.example.com')
  const c = f.calls[0] as Call
  assert.equal(c.url.href, 'https://video.example.com/v1/uploads')
  assert.equal(c.headers.get('Authorization'), 'Bearer cmx_test')
  assert.match(c.headers.get('Idempotency-Key') ?? '', /^[0-9a-f-]{36}$/)
  assert.deepEqual(c.body, { size: 10, passthrough: 'lesson_42' })
})

test('a retry reuses the idempotency key and honours Retry-After', async () => {
  const f = fake(
    json(503, { error: { code: 'unavailable', message: 'x' } }),
    json(
      429,
      { error: { code: 'rate_limited', message: 'slow down', retry_after: '0' } },
      { 'Retry-After': '0' },
    ),
    json(202, { id: 'job_1', kind: 'reencode', status: 'queued' }),
  )
  const cm = new ClipMux({ env, fetch: f.fetch })
  const job = await cm.assets.reencode('ast_1')
  assert.equal(job.id, 'job_1')
  assert.equal(f.calls.length, 3)
  const keys = new Set(f.calls.map((c) => c.headers.get('Idempotency-Key')))
  assert.equal(keys.size, 1)
})

test('an API error is a ClipMuxError with the code, details and a hint', async () => {
  const f = fake(json(409, { error: { code: 'job_active', message: 'a job is running', job_id: 'job_9' } }))
  const cm = new ClipMux({ env, fetch: f.fetch })
  await assert.rejects(cm.assets.reencode('ast_1'), (e: unknown) => {
    assert.ok(e instanceof ClipMuxError)
    assert.equal(e.status, 409)
    assert.equal(e.code, 'job_active')
    assert.deepEqual(e.details, { job_id: 'job_9' })
    assert.match(e.message, /POST \/v1\/assets\/ast_1\/reencode: job_active: a job is running .*supersede/)
    return true
  })
})

test('a missing setting fails only the call that needs it, naming the variable', async () => {
  const cm = new ClipMux({ env: { CLIPMUX_URL: 'https://v.example' } })
  await assert.rejects(
    cm.assets.get('ast_1'),
    (e: unknown) => e instanceof ClipMuxConfigError && e.variable === 'CLIPMUX_API_KEY',
  )
  await assert.rejects(cm.playback.token('pid_1'), /CLIPMUX_SIGNING_KID is not set.*npx @clipmux\/node init/)
  await assert.rejects(cm.webhooks.verify('{}', {}), /CLIPMUX_WEBHOOK_SECRET/)
  assert.throws(() => new ClipMux({ env: { CLIPMUX_URL: 'video.example.com' } }), /must be an http\(s\) URL/)
})

test('playback.token signs sub, aud, a 15-minute default and the viewer', async () => {
  const cm = new ClipMux({ env, fetch: fake().fetch })
  const c = claims(await cm.playback.token('pid_signed', { viewerId: 'stu_1' }))
  assert.equal(c.sub, 'pid_signed')
  assert.equal(c.aud, 'v')
  assert.equal(c.exp - c.iat, 900)
  assert.equal(c.vid, 'stu_1')
  assert.equal(
    claims(await cm.playback.token('pid_signed', { audience: 'poster', expiresIn: '2h' })).aud,
    't',
  )
  await assert.rejects(cm.playback.token('pid_signed', { expiresIn: '8d' }), /longer than the server allows/)
})

test("playback.session picks the asset's own signed ID, signs it, and lists ready tracks", async () => {
  const cm = new ClipMux({ env, fetch: fake().fetch })
  const s = await cm.playback.session(asset({ title: 'Lesson 42' }), { viewerId: 'stu_1' })
  assert.equal(s.playback_id, 'pid_signed')
  assert.equal(s.base_url, 'https://media.example.net')
  assert.equal(claims(s.token).exp - claims(s.token).iat, 900)
  assert.equal(s.poster_url, `https://media.example.net/v/${s.token}/pid_signed/poster.jpg`)
  assert.deepEqual(
    s.tracks.map((t) => t.id),
    ['trk_ready'],
  )
  assert.equal(s.title, 'Lesson 42')
  assert.equal(s.duration_seconds, 3 * 3600)
})

test('a native session outlives the video: twice its duration, at least 4 h, within the server maximum', async () => {
  const app = { max_token_lifetime_seconds: 604800, base_url: 'https://media.example.net' }
  const f = fake(json(200, app))
  const cm = new ClipMux({ env, fetch: f.fetch })
  const ttl = async (a: Asset) => {
    const t = claims((await cm.playback.session(a, { native: true })).token)
    return t.exp - t.iat
  }
  assert.equal(await ttl(asset()), 6 * 3600)
  assert.equal(await ttl(asset({ duration_seconds: 600 })), 4 * 3600)
  assert.equal(await ttl(asset({ duration_seconds: null })), 12 * 3600)
  assert.equal(f.calls.length, 1, '/v1/app is fetched once and cached')
})

test('a session for a bare playback ID takes base_url from /v1/app', async () => {
  const f = fake(json(200, { base_url: 'https://cdn.example/', max_token_lifetime_seconds: 604800 }))
  const cm = new ClipMux({ env, fetch: f.fetch })
  const s = await cm.playback.session('pid_signed')
  assert.equal(s.base_url, 'https://cdn.example')
  assert.equal(f.calls[0]?.url.pathname, '/v1/app')
})

test('a public-only asset plays without a token', async () => {
  const cm = new ClipMux({ env, fetch: fake().fetch })
  const s = await cm.playback.session(
    asset({ playback_ids: [{ id: 'pid_pub', policy: 'public', playable: true, status: 'active' }] }),
  )
  assert.equal(s.token, '')
  assert.equal(s.poster_url, 'https://media.example.net/p/pid_pub/poster.jpg')
  assert.equal(
    cm.playback.urls(
      asset({ playback_ids: [{ id: 'pid_pub', policy: 'public', playable: true, status: 'active' }] }),
    ).hls,
    'https://media.example.net/p/pid_pub/master.m3u8',
  )
})

test('assets.changes pages until next_cursor runs out', async () => {
  const f = fake(
    json(200, { data: [asset({ id: 'a' })], next_cursor: 'c2' }),
    json(200, { data: [asset({ id: 'b' })] }),
  )
  const cm = new ClipMux({ env, fetch: f.fetch })
  const ids: string[] = []
  for await (const a of cm.assets.changes({ since: new Date('2027-01-01T00:00:00Z') })) ids.push(a.id)
  assert.deepEqual(ids, ['a', 'b'])
  assert.equal(f.calls[0]?.url.searchParams.get('updated_since'), '2027-01-01T00:00:00.000Z')
  assert.equal(f.calls[1]?.url.searchParams.get('cursor'), 'c2')
})

test('waitUntilReady long-polls events until the asset is ready, and fails on errored', async () => {
  const f = fake(
    json(200, asset({ status: 'processing' })),
    json(200, { data: [{ id: 'evt_1', created_at: '', body: {} }] }),
    json(200, asset()),
  )
  const cm = new ClipMux({ env, fetch: f.fetch })
  assert.equal((await cm.assets.waitUntilReady('ast_1')).status, 'ready')
  assert.equal(f.calls[1]?.url.pathname, '/v1/events')
  const g = fake(json(200, asset({ status: 'errored', error_code: 'transcode_failed' })))
  await assert.rejects(new ClipMux({ env, fetch: g.fetch }).assets.waitUntilReady('ast_1'), (e: unknown) => {
    return (
      e instanceof ClipMuxError && e.code === 'asset_errored' && e.details.error_code === 'transcode_failed'
    )
  })
})

test('webhooks.verify uses CLIPMUX_WEBHOOK_SECRET', async () => {
  const cm = new ClipMux({ env })
  const body = '{"id":"evt_1","type":"video.asset.ready"}'
  const now = Math.floor(Date.now() / 1000)
  const sig = await signWebhook('whsec_test', now, body)
  const ev = await cm.webhooks.verify(body, {
    'x-webhook-timestamp': String(now),
    'x-webhook-signature': sig,
  })
  assert.equal(ev.type, 'video.asset.ready')
})
