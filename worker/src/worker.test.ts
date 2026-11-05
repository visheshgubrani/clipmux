// Worker behaviour the shared vectors don't cover: ranges, HEAD, the route
// cache's revocation window, the origin backend, and the content key
// derivation vector from internal/contentkey.

import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createWorker, type Env, resolveRange } from './delivery.ts'
import { vectorEnv, vectors } from './fixture.test.ts'

const now = () => vectors.now
const signed = (path: string) => `https://media.example/v/${vectors.tokens.v}/pid_signed/${path}`

test('a single byte range is served as 206 with its Content-Range', async () => {
  const { env } = vectorEnv()
  const w = createWorker({ now, cache: null })
  const res = await w.fetch(
    new Request(signed('out_v/360p/seg_00001.m4s'), { headers: { Range: 'bytes=2-5' } }),
    env,
  )
  assert.equal(res.status, 206)
  assert.equal(res.headers.get('Content-Range'), 'bytes 2-5/11')
  assert.equal(await res.text(), 'gmen') // "segment-one"[2:6]
  const tail = await w.fetch(
    new Request(signed('out_v/360p/seg_00001.m4s'), { headers: { Range: 'bytes=-3' } }),
    env,
  )
  assert.equal(await tail.text(), 'one')
  const bad = await w.fetch(
    new Request(signed('out_v/360p/seg_00001.m4s'), { headers: { Range: 'bytes=99-' } }),
    env,
  )
  assert.equal(bad.status, 416)
  assert.equal(bad.headers.get('Content-Range'), 'bytes */11')
})

test('HEAD answers like GET without a body', async () => {
  const { env } = vectorEnv()
  const res = await createWorker({ now, cache: null }).fetch(
    new Request(signed('out_v/360p/seg_00001.m4s'), { method: 'HEAD' }),
    env,
  )
  assert.equal(res.status, 200)
  assert.equal(res.headers.get('Content-Length'), '11')
  assert.equal((await res.arrayBuffer()).byteLength, 0)
})

test('ranges follow http.ServeContent for one range', () => {
  assert.deepEqual(resolveRange('bytes=0-', 10), { start: 0, end: 9 })
  assert.deepEqual(resolveRange('bytes=5-100', 10), { start: 5, end: 9 })
  assert.equal(resolveRange('bytes=0-1,4-5', 10), null) // several: whole object
  assert.equal(resolveRange('bytes=10-', 10), 'unsatisfiable')
})

// Revocation deletes the route object; the Worker reuses a route for at most
// its TTL (60 s in production), so a revoked ID stops within a minute.
test('a deleted route stops serving once the cached copy expires', async () => {
  const { env, objects } = vectorEnv()
  const w = createWorker({ now, cache: null, ttlMs: 40 })
  assert.equal((await w.fetch(new Request(signed('master.m3u8')), env)).status, 200)
  objects.delete('_routes/pid_signed.json')
  assert.equal(
    (await w.fetch(new Request(signed('master.m3u8')), env)).status,
    200,
    'served from the route cache',
  )
  await new Promise((r) => setTimeout(r, 60))
  assert.equal((await w.fetch(new Request(signed('master.m3u8')), env)).status, 404)
})

test('the content key matches the Go derivation vector', async () => {
  const { env, objects } = vectorEnv()
  const secret = [...Array(32).keys()].map((b) => b.toString(16).padStart(2, '0')).join('')
  objects.set(
    '_routes/pid_k.json',
    new TextEncoder().encode(
      JSON.stringify({
        v: 1,
        app_id: 'app_a',
        asset_id: 'ast_1',
        output_id: 'out_1',
        policy: 'public',
        backend: 'r2',
        outputs: { out_1: 1 },
      }),
    ),
  )
  const res = await createWorker({ now, cache: null }).fetch(
    new Request('https://media.example/p/pid_k/out_1/key/1'),
    {
      ...env,
      CONTENT_KEYS: JSON.stringify({ 1: secret }),
    },
  )
  assert.equal(Buffer.from(await res.arrayBuffer()).toString('hex'), '8b60ee274c0f0c75195f891d401f5c7f')
})

test('the origin backend proxies only requests that passed every check', async () => {
  const { env, objects } = vectorEnv()
  const route = JSON.parse(new TextDecoder().decode(objects.get('_routes/pid_signed.json')))
  objects.set(
    '_routes/pid_signed.json',
    new TextEncoder().encode(JSON.stringify({ ...route, backend: 'origin' })),
  )
  const seen: string[] = []
  const w = createWorker({
    now,
    cache: null,
    fetchOrigin: async (req) => {
      seen.push(new URL(req.url).pathname)
      return new Response('from-origin', { headers: { 'Content-Type': 'text/plain' } })
    },
  })
  const e: Env = { ...env, ORIGIN_URL: 'https://app.example/' }
  const seg = await w.fetch(new Request(signed('out_v/360p/seg_00001.m4s')), e)
  assert.equal(seg.status, 200)
  assert.equal(await seg.text(), 'from-origin')
  assert.equal(seg.headers.get('Content-Type'), 'video/iso.segment', 'the type comes from the extension')
  assert.equal(seg.headers.get('Cache-Control'), 'private, max-age=31536000, immutable')
  // Refused here: the origin never sees it.
  const refused = await w.fetch(new Request('https://media.example/p/pid_signed/master.m3u8'), e)
  assert.equal(refused.status, 403)
  const key = await w.fetch(new Request(signed('out_v/key/1')), e)
  assert.equal(key.status, 200, 'keys are derived at the edge, never fetched')
  assert.deepEqual(seen, [`/v/${vectors.tokens.v}/pid_signed/out_v/360p/seg_00001.m4s`])
})
