import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createServer } from 'node:http'
import type { AddressInfo } from 'node:net'
import { test } from 'node:test'
import { ClipMux } from './client.ts'
import { toNodeListener } from './express.ts'
import { clipmuxHandler } from './handlers.ts'
import type { Asset } from './schema.gen.ts'
import { signWebhook } from './webhooks.ts'

const vectors = JSON.parse(
  readFileSync(new URL('../../../internal/delivery/testdata/vectors.json', import.meta.url), 'utf8'),
)
const env = {
  CLIPMUX_URL: 'https://video.example.com',
  CLIPMUX_API_KEY: 'cmx_test',
  CLIPMUX_SIGNING_KID: 'kid_app_a',
  CLIPMUX_SIGNING_KEY: vectors.signing_keys[0].private_key_pem as string,
  CLIPMUX_WEBHOOK_SECRET: 'whsec_test',
}

const asset = (over: Partial<Asset> = {}): Asset => ({
  id: 'ast_1',
  app_id: 'app_1',
  status: 'ready',
  version: 3,
  passthrough: 'lesson_42',
  base_url: 'https://media.example.net',
  playback_ids: [
    { id: 'pid_a', policy: 'signed', playable: true, status: 'active' },
    { id: 'pid_b', policy: 'signed', playable: true, status: 'active' },
  ],
  duration_seconds: 600,
  tracks: [],
  created_at: '2027-01-01T00:00:00Z',
  updated_at: '2027-01-01T00:00:00Z',
  ...over,
})

// api answers ClipMux API calls by path.
function api(routes: Record<string, () => Response>) {
  const calls: { method: string; path: string; body: unknown }[] = []
  const fetch = (async (input: URL, init: RequestInit) => {
    const path = new URL(input).pathname
    calls.push({
      method: init.method ?? 'GET',
      path,
      body: init.body ? JSON.parse(init.body as string) : undefined,
    })
    const r = routes[`${init.method ?? 'GET'} ${path}`]
    if (!r) return Response.json({ error: { code: 'not_found', message: 'x' } }, { status: 404 })
    return r()
  }) as typeof globalThis.fetch
  return { calls, clipmux: new ClipMux({ env, fetch, maxRetries: 0 }) }
}

const post = (
  path: string,
  body: unknown,
  headers: Record<string, string> = { 'Content-Type': 'application/json' },
) =>
  new Request(`https://lms.example${path}`, {
    method: 'POST',
    headers,
    body: typeof body === 'string' ? body : JSON.stringify(body),
  })

test('upload creates the upload the callback decides, signed by default, with endpoint', async () => {
  const a = api({
    'POST /v1/uploads': () =>
      Response.json(
        {
          upload_id: 'upl_1',
          asset_id: 'ast_1',
          part_size: 1,
          part_count: 1,
          upload_token: 'ut',
          playback_ids: [],
        },
        { status: 201 },
      ),
  })
  const h = clipmuxHandler({
    clipmux: a.clipmux,
    upload: (req, { file }) => ({
      passthrough: new URL(req.url).searchParams.get('lesson') ?? '',
      title: file.name,
    }),
  })
  const res = await h(
    post('/api/clipmux/upload?lesson=lesson_42', { size: 1234, name: 'intro.mp4', type: 'video/mp4' }),
  )
  assert.equal(res.status, 201)
  assert.equal(res.headers.get('Cache-Control'), 'no-store')
  const body = await res.json()
  assert.equal(body.endpoint, 'https://video.example.com')
  assert.equal(body.upload_token, 'ut')
  assert.deepEqual(a.calls[0]?.body, {
    playback_policy: 'signed',
    passthrough: 'lesson_42',
    title: 'intro.mp4',
    size: 1234,
  })
})

test('every route is default-deny', async () => {
  const a = api({})
  const none = clipmuxHandler({ clipmux: a.clipmux })
  for (const action of ['upload', 'playback', 'webhook']) {
    assert.equal((await none(post(`/api/clipmux/${action}`, {}))).status, 404, action)
  }
  const refuse = clipmuxHandler({ clipmux: a.clipmux, upload: () => null, playback: () => null })
  assert.equal((await refuse(post('/x/upload', { size: 1 }))).status, 403)
  const denied = await refuse(post('/x/playback', { playback_id: 'pid_a' }))
  assert.equal(denied.status, 403)
  assert.equal((await denied.json()).error.code, 'denied')
  assert.equal(a.calls.length, 0, 'nothing reaches ClipMux')
})

test('browser routes take only JSON, POST and a valid body', async () => {
  const h = clipmuxHandler({ clipmux: api({}).clipmux, upload: () => ({}), playback: () => ({}) })
  assert.equal(
    (await h(post('/x/upload', 'size=1', { 'Content-Type': 'application/x-www-form-urlencoded' }))).status,
    415,
  )
  assert.equal((await h(post('/x/upload', { size: -1 }))).status, 400)
  assert.equal((await h(post('/x/playback', { playback_id: '../x' }))).status, 400)
  assert.equal((await h(new Request('https://lms.example/x/playback'))).status, 405)
  assert.equal((await h(post('/x/other', {}))).status, 404)
})

test("ClipMux's refusals reach the browser as what they are, so it retries only what can pass", async () => {
  const err =
    (status: number, code: string, headers: Record<string, string> = {}) =>
    () =>
      Response.json({ error: { code, message: 'x', retry_after: '7' } }, { status, headers })
  const cases: [Response | (() => Response) | 'network', number, string][] = [
    // The caller's request: the same answer, not retryable.
    [err(400, 'invalid_request')(), 400, 'invalid_request'],
    [err(409, 'idempotency_key_reused')(), 409, 'idempotency_key_reused'],
    [err(413, 'too_large')(), 413, 'too_large'],
    [err(422, 'unprocessable')(), 422, 'unprocessable'],
    // Throttled: the browser waits and retries.
    [err(429, 'rate_limited', { 'Retry-After': '7' })(), 429, 'rate_limited'],
    // This server's key or app is wrong: its own fault, not ClipMux being down.
    [err(401, 'unauthorized')(), 500, 'clipmux_misconfigured'],
    [err(403, 'insufficient_scope')(), 500, 'clipmux_misconfigured'],
    // ClipMux down or unreachable: a gateway error, retryable.
    [err(503, 'unavailable')(), 502, 'clipmux_unavailable'],
    ['network', 502, 'clipmux_unavailable'],
  ]
  for (const [answer, status, code] of cases) {
    const fetch = (async () => {
      if (answer === 'network') throw new TypeError('fetch failed')
      return typeof answer === 'function' ? answer() : answer
    }) as typeof globalThis.fetch
    const handle = clipmuxHandler({
      clipmux: new ClipMux({ env, fetch, maxRetries: 0 }),
      upload: () => ({ passthrough: 'lesson_42' }),
      onError: () => {},
    })
    const res = await handle(post('/api/clipmux/upload', { size: 10 }))
    const body = (await res.json()) as { error: { code: string } }
    assert.equal(res.status, status, code)
    assert.equal(body.error.code, code)
    if (status === 429) assert.equal(res.headers.get('Retry-After'), '7')
  }
})

test('playback signs the asked ID for the viewer the callback names', async () => {
  const h = clipmuxHandler({
    clipmux: api({}).clipmux,
    playback: (_req, { playbackId }) => ({ viewerId: 'stu_7', asset: asset({ title: `for ${playbackId}` }) }),
  })
  const res = await h(post('/api/clipmux/playback', { playback_id: 'pid_b', native: false }))
  assert.equal(res.status, 200)
  const s = await res.json()
  assert.equal(s.playback_id, 'pid_b')
  assert.equal(s.base_url, 'https://media.example.net')
  const c = JSON.parse(Buffer.from(s.token.split('.')[1], 'base64url').toString())
  assert.deepEqual([c.sub, c.aud, c.vid, c.exp - c.iat], ['pid_b', 'v', 'stu_7', 900])
})

test('playback signs for the lifetime the callback names, but a native player keeps its long token', async () => {
  const a = api({
    'GET /v1/app': () => Response.json({ base_url: 'https://m', max_token_lifetime_seconds: 604800 }),
  })
  const h = clipmuxHandler({ clipmux: a.clipmux, playback: () => ({ asset: asset(), expiresIn: '30s' }) })
  const ttl = async (native: boolean) => {
    const s = await (await h(post('/x/playback', { playback_id: 'pid_a', native }))).json()
    const c = JSON.parse(Buffer.from(s.token.split('.')[1], 'base64url').toString())
    return c.exp - c.iat
  }
  assert.equal(await ttl(false), 30)
  assert.equal(await ttl(true), 4 * 3600)
})

test("playback with native asks for a token that outlives the video's session", async () => {
  const a = api({
    'GET /v1/app': () => Response.json({ base_url: 'https://m', max_token_lifetime_seconds: 604800 }),
  })
  const h = clipmuxHandler({ clipmux: a.clipmux, playback: () => ({ asset: asset() }) })
  const s = await (await h(post('/x/playback', { playback_id: 'pid_a', native: true }))).json()
  const c = JSON.parse(Buffer.from(s.token.split('.')[1], 'base64url').toString())
  assert.equal(c.exp - c.iat, 4 * 3600)
})

async function signed(body: string, secret = 'whsec_test') {
  const ts = Math.floor(Date.now() / 1000)
  return post('/api/clipmux/webhook', body, {
    'Content-Type': 'application/json',
    'X-Webhook-Event': 'video.asset.ready',
    'X-Webhook-Id': 'evt_1',
    'X-Webhook-Timestamp': String(ts),
    'X-Webhook-Signature': await signWebhook(secret, ts, body),
  })
}

test('webhook verifies, re-fetches the asset and hands over the current one', async () => {
  const a = api({ 'GET /v1/assets/ast_1': () => Response.json(asset({ version: 5 })) })
  const got: [number, string][] = []
  const h = clipmuxHandler({ clipmux: a.clipmux, asset: (x, ev) => void got.push([x.version, ev.type]) })
  const body = JSON.stringify({
    id: 'evt_1',
    type: 'video.asset.ready',
    created_at: '',
    data: asset({ version: 4 }),
  })
  const res = await h(await signed(body))
  assert.equal(res.status, 200)
  assert.deepEqual(got, [[5, 'video.asset.ready']])

  const forged = await h(await signed(body, 'whsec_wrong'))
  assert.equal(forged.status, 400)
  assert.equal((await forged.json()).error.code, 'bad_signature')
  assert.equal(got.length, 1)
})

test('webhook drops a re-fetch older than one already handed over, and retries on a callback error', async () => {
  let version = 7
  const a = api({ 'GET /v1/assets/ast_1': () => Response.json(asset({ version })) })
  const got: number[] = []
  let fail = false
  const h = clipmuxHandler({
    clipmux: a.clipmux,
    asset: (x) => {
      if (fail) throw new Error('db down')
      got.push(x.version)
    },
    onError: () => {},
  })
  const body = JSON.stringify({ id: 'evt_1', type: 'video.asset.updated', created_at: '', data: asset() })
  await h(await signed(body))
  version = 6
  await h(await signed(body))
  assert.deepEqual(got, [7])
  fail = true
  version = 8
  assert.equal((await h(await signed(body))).status, 500, 'ClipMux retries a delivery that failed')
})

test('webhook of a deleted asset hands over the event copy when the asset is gone', async () => {
  const got: string[] = []
  const h = clipmuxHandler({ clipmux: api({}).clipmux, asset: (x) => void got.push(x.status) })
  const body = JSON.stringify({
    id: 'e',
    type: 'video.asset.deleted',
    created_at: '',
    data: asset({ status: 'deleted' }),
  })
  assert.equal((await h(await signed(body))).status, 200)
  assert.deepEqual(got, ['deleted'])
})

test('toNodeListener serves the handler on node:http with the raw webhook body', async () => {
  const got: string[] = []
  const a = api({ 'GET /v1/assets/ast_1': () => Response.json(asset()) })
  const handler = clipmuxHandler({ clipmux: a.clipmux, asset: (x) => void got.push(x.id) })
  const server = createServer(toNodeListener(handler))
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  try {
    const port = (server.address() as AddressInfo).port
    const body = `{"id":"evt_1","type":"video.asset.ready","created_at":"","data":${JSON.stringify(asset())}}`
    const req = await signed(body)
    const res = await fetch(`http://127.0.0.1:${port}/api/clipmux/webhook`, {
      method: 'POST',
      headers: req.headers,
      body,
    })
    assert.equal(res.status, 200)
    assert.deepEqual(got, ['ast_1'])
  } finally {
    server.close()
  }
})

test('toNodeListener refuses a body another middleware already parsed', async () => {
  const listener = toNodeListener(clipmuxHandler({ clipmux: api({}).clipmux, asset: () => {} }))
  const errs: unknown[] = []
  const req = Object.assign(Object.create(null), {
    headers: { host: 'x' },
    method: 'POST',
    url: '/w/webhook',
    body: { parsed: true },
    socket: {},
  })
  await new Promise<void>((resolve) => {
    listener(req, {} as never, (err) => {
      errs.push(err)
      resolve()
    })
  })
  assert.match(String(errs[0]), /already parsed .*express\.raw/)
})
