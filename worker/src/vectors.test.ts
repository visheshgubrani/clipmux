// The delivery contract (internal/delivery/testdata/vectors.json), run on the
// Worker. Its bucket holds exactly what the Go route publisher writes for
// those vectors (testdata/worker-bucket.json, checked by the Go side), so a
// pass means the two adapters agree from the database to the response.

import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createWorker, kindOf, rewriteMaster } from './delivery.ts'
import { vectorEnv, vectors } from './fixture.test.ts'

const worker = createWorker({ now: () => vectors.now, cache: null })
const { env, points } = vectorEnv()

interface Expected {
  app_id: string
  asset_id: string
  playback_id: string
  kind: string
  bytes: number
  views: number
  watch_seconds: number
  vid?: string
}

// The data point a counted case writes (D103): the viewer key from the
// viewer_keys vectors, never the raw vid, with stratified indexes.
function want(reqPath: string, a: Expected) {
  const viewer = a.vid
    ? vectors.viewer_keys.find(
        (k: { app_id: string; vid: string }) => k.app_id === a.app_id && k.vid === a.vid,
      )?.key_hex
    : ''
  assert.notEqual(viewer, undefined, `no viewer_keys vector for ${a.app_id}/${a.vid}`)
  const trimmed = reqPath.replace(/^\/(?:v\/[^/]+|p)\/[^/]+\//, '')
  const rendition = kindOf(trimmed)?.rendition ?? ''
  const index =
    a.views > 0
      ? `${a.asset_id}:m`
      : a.watch_seconds > 0
        ? `${a.asset_id}:w`
        : a.kind === 'segment'
          ? `${a.asset_id}:s:${rendition}`
          : `${a.asset_id}:o`
  return {
    blobs: ['prj_default', a.app_id, a.asset_id, a.playback_id, a.kind, viewer, 'ins_test'],
    doubles: [a.views, a.watch_seconds, a.bytes],
    indexes: [index],
  }
}

for (const c of vectors.cases) {
  test(c.name, async () => {
    let path: string = c.path
    for (const [name, tok] of Object.entries(vectors.tokens))
      path = path.replaceAll(`{${name}}`, tok as string)
    points.length = 0
    const res = await worker.fetch(
      new Request(`https://media.example${path}`, {
        method: c.method ?? 'GET',
        headers: c.range ? { Range: c.range } : {},
      }),
      env,
    )
    const body = new Uint8Array(await res.arrayBuffer())
    assert.equal(res.status, c.status, new TextDecoder().decode(body))
    assert.equal(res.headers.get('X-Content-Type-Options'), 'nosniff')
    assert.equal(res.headers.get('Access-Control-Allow-Origin'), '*')
    if (c.content_type) assert.equal(res.headers.get('Content-Type'), c.content_type)
    if (c.cache_control) assert.equal(res.headers.get('Cache-Control'), c.cache_control)
    if (c.body) assert.equal(new TextDecoder().decode(body), c.body)
    if (c.body_hex) assert.equal(Buffer.from(body).toString('hex'), c.body_hex)
    assert.deepEqual(points, c.analytics ? [want(path, c.analytics)] : [], 'analytics data points')
  })
}

test('the master rewrite matches the Go handler on CRLF', () => {
  assert.equal(
    rewriteMaster('#EXTM3U\r\n#EXT-X-MEDIA:URI="audio/index.m3u8"\r\n360p/index.m3u8\r\n', 'out_1'),
    '#EXTM3U\n#EXT-X-MEDIA:URI="out_1/audio/index.m3u8"\nout_1/360p/index.m3u8\n',
  )
})
