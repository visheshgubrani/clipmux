// The bundled Worker (dist/worker.js) in workerd, Cloudflare's runtime, with
// a real R2 binding and the Cache API: what the Node fakes can't prove.
// Run `pnpm build` first; `pnpm test` does.

import assert from 'node:assert/strict'
import { existsSync } from 'node:fs'
import { after, before, test } from 'node:test'
import { Miniflare } from 'miniflare'
import { vectors } from './fixture.test.ts'

const bundle = new URL('../dist/worker.js', import.meta.url)
const keys = JSON.parse(
  (await import('node:fs')).readFileSync(
    new URL('../../internal/delivery/testdata/worker-bucket.json', import.meta.url),
    'utf8',
  ),
)

let mf: Miniflare
const enc = new TextEncoder()
const b64url = (b: Uint8Array) => Buffer.from(b).toString('base64url')

// sign makes a token valid now, with the vectors' test key for app_a.
async function sign(sub: string, aud = 'v', vid?: string): Promise<string> {
  const pem: string = vectors.signing_keys.find((k: { kid: string }) => k.kid === 'kid_app_a').private_key_pem
  const der = Buffer.from(pem.replace(/-----[^-]+-----|\s/g, ''), 'base64')
  const key = await crypto.subtle.importKey('pkcs8', der, { name: 'ECDSA', namedCurve: 'P-256' }, false, [
    'sign',
  ])
  const now = Math.floor(Date.now() / 1000)
  const input = `${b64url(enc.encode(JSON.stringify({ alg: 'ES256', kid: 'kid_app_a', typ: 'JWT' })))}.${b64url(
    enc.encode(JSON.stringify({ sub, aud, iat: now, exp: now + 3600, ...(vid ? { vid } : {}) })),
  )}`
  const sig = await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, key, enc.encode(input))
  return `${input}.${b64url(new Uint8Array(sig))}`
}

before(async () => {
  assert.ok(existsSync(bundle), 'dist/worker.js is missing: run pnpm build')
  mf = new Miniflare({
    modules: true,
    scriptPath: bundle.pathname,
    compatibilityDate: '2026-07-01',
    r2Buckets: ['MEDIA'],
    bindings: {
      CONTENT_KEYS: JSON.stringify({ 1: vectors.content_key_secret_hex }),
      PROJECT_ID: 'prj_default',
      ANALYTICS_KEY: vectors.analytics_key_hex,
    },
    analyticsEngineDatasets: { ANALYTICS: { dataset: 'clipmux_delivery' } },
  })
  const r2 = await mf.getR2Bucket('MEDIA')
  for (const [pid, r] of Object.entries(keys.routes)) await r2.put(`_routes/${pid}.json`, JSON.stringify(r))
  await r2.put('_keys.json', JSON.stringify(keys.keys))
  for (const o of vectors.outputs) {
    for (const [name, body] of Object.entries(vectors.files)) {
      await r2.put(`media/${o.asset_id}/${o.id}/${name}`, body as string)
    }
  }
  for (const t of vectors.tracks ?? []) {
    if (t.sealed_hex) await r2.put(`media/${t.asset_id}/tracks/${t.id}.vtt`, Buffer.from(t.sealed_hex, 'hex'))
  }
})

test('a sealed VTT decrypts with WebCrypto in workerd, twice through the edge cache', async () => {
  const tok = await sign('pid_tracks')
  const want = vectors.tracks.find((t: { id: string }) => t.id === 'trk_hi').vtt
  for (let i = 0; i < 2; i++) {
    const res = await mf.dispatchFetch(`https://media.example/v/${tok}/pid_tracks/tracks/trk_hi.vtt`)
    assert.equal(res.status, 200)
    assert.equal(res.headers.get('Content-Type'), 'text/vtt; charset=utf-8')
    assert.equal(await res.text(), want)
  }
  const master = await mf.dispatchFetch(`https://media.example/v/${tok}/pid_tracks/master.m3u8`)
  assert.match(await master.text(), /SUBTITLES="subs"/)
})

after(async () => {
  await mf?.dispose()
})

test('a signed master plays from R2 in workerd', async () => {
  const tok = await sign('pid_signed')
  const res = await mf.dispatchFetch(`https://media.example/v/${tok}/pid_signed/master.m3u8`)
  assert.equal(res.status, 200)
  assert.equal(res.headers.get('Cache-Control'), 'no-store')
  assert.match(await res.text(), /out_v\/360p\/index\.m3u8/)
  const none = await mf.dispatchFetch('https://media.example/p/pid_signed/master.m3u8')
  assert.equal(none.status, 403)
  await none.arrayBuffer()
})

test('the content key derives in workerd as in Go', async () => {
  const tok = await sign('pid_signed')
  const res = await mf.dispatchFetch(`https://media.example/v/${tok}/pid_signed/out_v/key/1`)
  const want = vectors.cases.find((c: { name: string }) => c.name === 'the content key').body_hex
  assert.equal(Buffer.from(await res.arrayBuffer()).toString('hex'), want)
})

test('R2 serves a byte range', async () => {
  const tok = await sign('pid_signed')
  const res = await mf.dispatchFetch(`https://media.example/v/${tok}/pid_signed/out_v/360p/seg_00001.m4s`, {
    headers: { Range: 'bytes=0-6' },
  })
  assert.equal(res.status, 206)
  assert.equal(await res.text(), 'segment')
})

test('a pinned segment is served from the edge cache once fetched', async () => {
  const tok = await sign('pid_signed')
  const url = `https://media.example/v/${tok}/pid_signed/out_v_old/360p/seg_00001.m4s`
  const first = await mf.dispatchFetch(url)
  assert.equal(await first.text(), 'segment-one')
  const r2 = await mf.getR2Bucket('MEDIA')
  await r2.delete('media/ast_v/out_v_old/360p/seg_00001.m4s')
  // Another token, same storage key: the cache isn't fragmented by tokens.
  const again = await mf.dispatchFetch(
    `https://media.example/v/${await sign('pid_signed')}/pid_signed/out_v_old/360p/seg_00001.m4s`,
  )
  assert.equal(again.status, 200)
  assert.equal(await again.text(), 'segment-one')
  assert.equal(again.headers.get('Cache-Control'), 'private, max-age=31536000, immutable')
})

// workerd has no Buffer and a real Analytics Engine binding: counting a
// viewer's master and a ranged audio segment must not cost the response.
// (Miniflare can't read data points back; the Node vectors check them.)
test('counting runs in workerd without touching the response', async () => {
  const tok = await sign('pid_signed', 'v', 'stu_8f3a2c')
  const master = await mf.dispatchFetch(`https://media.example/v/${tok}/pid_signed/master.m3u8`)
  assert.equal(master.status, 200)
  assert.match(await master.text(), /out_v\/360p\/index\.m3u8/)
  const seg = await mf.dispatchFetch(`https://media.example/v/${tok}/pid_signed/out_v/audio/seg_00001.m4s`, {
    headers: { Range: 'bytes=0-4' },
  })
  assert.equal(seg.status, 206)
  assert.equal(await seg.text(), 'audio')
})
