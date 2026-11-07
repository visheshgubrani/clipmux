// The delivery contract (internal/delivery/testdata/vectors.json), run on the
// Worker. Its bucket holds exactly what the Go route publisher writes for
// those vectors (testdata/worker-bucket.json, checked by the Go side), so a
// pass means the two adapters agree from the database to the response.

import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createWorker, rewriteMaster } from './delivery.ts'
import { vectorEnv, vectors } from './fixture.test.ts'

const worker = createWorker({ now: () => vectors.now, cache: null })
const { env } = vectorEnv()

for (const c of vectors.cases) {
  test(c.name, async () => {
    let path: string = c.path
    for (const [name, tok] of Object.entries(vectors.tokens))
      path = path.replaceAll(`{${name}}`, tok as string)
    const res = await worker.fetch(
      new Request(`https://media.example${path}`, { method: c.method ?? 'GET' }),
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
  })
}

test('the master rewrite matches the Go handler on CRLF', () => {
  assert.equal(
    rewriteMaster('#EXTM3U\r\n#EXT-X-MEDIA:URI="audio/index.m3u8"\r\n360p/index.m3u8\r\n', 'out_1'),
    '#EXTM3U\n#EXT-X-MEDIA:URI="out_1/audio/index.m3u8"\nout_1/360p/index.m3u8\n',
  )
})
