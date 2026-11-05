import assert from 'node:assert/strict'
import { test } from 'node:test'
import { ClipMuxUpload, type ClipMuxUploadError, type PutPart, planParts, rememberUpload } from './index.ts'

// A fake ClipMux: the upload endpoints and the storage behind part URLs,
// with hooks to script failures. Parts are a few bytes each.
function fakeClipMux(opts: { partSize?: number; size?: number; stored?: number[] } = {}) {
  const partSize = opts.partSize ?? 4
  const size = opts.size ?? 10
  const data = new Uint8Array(size).map((_, i) => i)
  const file = new Blob([data])
  const stored = new Map<number, { etag: string; size: number }>()
  for (const n of opts.stored ?? []) {
    const p = planParts(size, partSize)[n - 1]
    if (p) stored.set(n, { etag: `"etag-${n}"`, size: p.end - p.start })
  }
  const calls: string[] = []
  const signed: number[][] = []
  const delays: number[] = []
  let now = Date.parse('2026-11-05T10:00:00Z')
  let expires = now + 60 * 60 * 1000
  let inFlight = 0
  let maxInFlight = 0
  const state = {
    file,
    calls,
    signed,
    delays,
    get maxInFlight() {
      return maxInFlight
    },
    stored,
    listStatus: 200,
    signStatus: 200,
    putScript: [] as Array<{ status: number; retryAfter?: string; noEtag?: boolean }>,
    completeScript: [] as number[],
    putGate: null as Promise<void> | null,
    setExpiry(ms: number) {
      expires = ms
    },
    advance(ms: number) {
      now += ms
    },
    now: () => now,
  }
  const json = (status: number, body: unknown, headers: Record<string, string> = {}) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json', ...headers },
    })
  const fetch = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(String(input))
    const method = init?.method ?? 'GET'
    calls.push(`${method} ${url.pathname}`)
    assert.equal((init?.headers as Record<string, string> | undefined)?.Authorization, 'Bearer tok')
    if (url.pathname.endsWith('/parts') && method === 'GET') {
      if (state.listStatus !== 200)
        return json(state.listStatus, { error: { code: 'upload_not_active', message: 'x' } })
      return json(200, { parts: [...stored].map(([part_number, s]) => ({ part_number, ...s })) })
    }
    if (url.pathname.endsWith('/parts')) {
      if (state.signStatus !== 200)
        return json(state.signStatus, { error: { code: 'unauthorized', message: 'x' } })
      const numbers = (JSON.parse(String(init?.body)) as { part_numbers: number[] }).part_numbers
      signed.push(numbers)
      return json(200, {
        parts: numbers.map((n) => ({
          part_number: n,
          url: `https://storage.test/part/${n}?sig=${signed.length}`,
        })),
        expires_at: new Date(expires).toISOString(),
      })
    }
    if (url.pathname.endsWith('/complete')) {
      const scripted = state.completeScript.shift()
      if (scripted) return json(scripted, { error: { code: 'invalid_parts', message: 'x' } })
      const body = JSON.parse(String(init?.body)) as { parts: { part_number: number; etag: string }[] }
      calls.push(`complete ${JSON.stringify(body.parts)}`)
      return json(200, { id: 'ast_1', status: 'processing', version: 2 })
    }
    if (url.pathname.endsWith('/cancel')) return json(200, { id: 'ast_1', status: 'errored', version: 2 })
    return json(404, {})
  }) as typeof globalThis.fetch
  const put: PutPart = async (url, body, { signal, onBytes }) => {
    const n = Number(new URL(url).pathname.split('/').pop())
    calls.push(`PUT ${n}`)
    inFlight++
    maxInFlight = Math.max(maxInFlight, inFlight)
    try {
      if (state.putGate) {
        await Promise.race([
          state.putGate,
          new Promise((_, reject) =>
            signal.addEventListener('abort', () => reject(signal.reason), { once: true }),
          ),
        ])
      }
      if (signal.aborted) throw signal.reason
      const scripted = state.putScript.shift()
      if (scripted && scripted.status !== 200) {
        return { status: scripted.status, etag: null, retryAfter: scripted.retryAfter ?? null }
      }
      const bytes = new Uint8Array(await body.arrayBuffer())
      stored.set(n, { etag: `"etag-${n}"`, size: bytes.length })
      onBytes(bytes.length)
      return { status: 200, etag: scripted?.noEtag ? null : `"etag-${n}"`, retryAfter: null }
    } finally {
      inFlight--
    }
  }
  const delay = async (ms: number) => {
    delays.push(ms)
  }
  const upload = (extra: Partial<ConstructorParameters<typeof ClipMuxUpload>[1]> = {}) =>
    new ClipMuxUpload(file, {
      endpoint: 'https://video.example.com/',
      upload: {
        upload_id: 'upl_1',
        upload_token: 'tok',
        part_size: partSize,
        part_count: planParts(size, partSize).length,
      },
      fetch,
      put,
      delay,
      random: () => 0,
      now: state.now,
      concurrency: 1,
      ...extra,
    })
  return Object.assign(state, { upload, state })
}

test('the part plan for 73400320 bytes is three 32 MiB parts and the remainder', () => {
  assert.deepEqual(planParts(73400320, 32 << 20), [
    { number: 1, start: 0, end: 33554432 },
    { number: 2, start: 33554432, end: 67108864 },
    { number: 3, start: 67108864, end: 73400320 },
  ])
})

test('every part is uploaded and the upload completes with the ETags as received', async () => {
  const f = fakeClipMux()
  const asset = await f.upload().start()
  assert.equal(asset.id, 'ast_1')
  assert.deepEqual(f.calls, [
    'GET /v1/uploads/upl_1/parts',
    'POST /v1/uploads/upl_1/parts',
    'PUT 1',
    'PUT 2',
    'PUT 3',
    'POST /v1/uploads/upl_1/complete',
    'complete [{"part_number":1,"etag":"\\"etag-1\\""},{"part_number":2,"etag":"\\"etag-2\\""},{"part_number":3,"etag":"\\"etag-3\\""}]',
  ])
})

test('a resumed upload sends only the parts storage does not hold', async () => {
  const f = fakeClipMux({ stored: [1, 2] })
  await f.upload().start()
  assert.deepEqual(
    f.calls.filter((c) => c.startsWith('PUT')),
    ['PUT 3'],
  )
  assert.deepEqual(f.signed, [[3]])
})

test('an upload another tab already finished resolves with its asset', async () => {
  const f = fakeClipMux()
  f.state.listStatus = 409
  const asset = await f.upload().start()
  assert.equal(asset.id, 'ast_1')
  assert.equal(f.calls.filter((c) => c.startsWith('PUT')).length, 0)
})

test('a file that does not match the upload is refused before any byte is sent', async () => {
  const f = fakeClipMux()
  const up = new ClipMuxUpload(new Blob([new Uint8Array(30)]), {
    endpoint: 'https://video.example.com',
    upload: { upload_id: 'upl_1', upload_token: 'tok', part_size: 4, part_count: 3 },
    fetch: globalThis.fetch,
  })
  await assert.rejects(up.start(), (e: ClipMuxUploadError) => e.code === 'file_mismatch')
  assert.deepEqual(f.calls, [])
})

test('250 parts are signed in windows of 100', async () => {
  const f = fakeClipMux({ partSize: 1, size: 250 })
  await f.upload({ concurrency: 4 }).start()
  assert.deepEqual(
    f.signed.map((w) => `${w[0]}-${w.at(-1)}`),
    ['1-100', '101-200', '201-250'],
  )
})

test('a URL about to expire is signed again before it is used', async () => {
  const f = fakeClipMux()
  f.state.setExpiry(f.now() + 4 * 60 * 1000) // less than the 5-minute margin
  await assert.rejects(f.upload().start(), (e: ClipMuxUploadError) => e.code === 'sign_failed')
  f.state.setExpiry(f.now() + 6 * 60 * 1000)
  f.calls.length = 0
  f.signed.length = 0
  await f.upload().start()
  assert.deepEqual(f.signed, [[1, 2, 3]])
})

test('storage refusing a signature is signed again once, then fatal', async () => {
  const f = fakeClipMux()
  f.state.putScript.push({ status: 403 })
  await f.upload().start()
  assert.deepEqual(f.signed, [[1, 2, 3], [1]])
  const g = fakeClipMux()
  g.state.putScript.push({ status: 403 }, { status: 403 })
  await assert.rejects(g.upload().start(), (e: ClipMuxUploadError) => e.code === 'part_url_rejected')
})

test('server errors are retried with backoff', async () => {
  const f = fakeClipMux()
  f.state.putScript.push({ status: 503 }, { status: 503 })
  await f.upload().start()
  assert.deepEqual(f.delays, [1000, 2000])
})

test('Retry-After is honoured', async () => {
  const f = fakeClipMux()
  f.state.putScript.push({ status: 429, retryAfter: '7' })
  await f.upload().start()
  assert.deepEqual(f.delays, [7000])
})

test('a rejected upload token is not retried', async () => {
  const f = fakeClipMux()
  f.state.signStatus = 401
  await assert.rejects(
    f.upload().start(),
    (e: ClipMuxUploadError) => e.code === 'upload_token_invalid' && !e.retryable,
  )
  assert.deepEqual(f.delays, [])
})

test('a refused part stops the others and leaves the upload resumable', async () => {
  const f = fakeClipMux({ partSize: 1, size: 8 })
  f.state.putScript.push({ status: 400 })
  await assert.rejects(
    f.upload({ concurrency: 4 }).start(),
    (e: ClipMuxUploadError) => e.code === 'part_rejected',
  )
  assert.ok(!f.calls.some((c) => c.includes('/cancel')), 'a failure never cancels the server upload')
  assert.ok(f.calls.filter((c) => c.startsWith('PUT')).length < 8, 'parts after the failure are not sent')
})

test('a missing ETag header names the CORS fix', async () => {
  const f = fakeClipMux()
  f.state.putScript.push({ status: 200, noEtag: true })
  await assert.rejects(f.upload().start(), (e: ClipMuxUploadError) => e.code === 'etag_not_exposed')
})

test('at most concurrency parts are in flight', async () => {
  const f = fakeClipMux({ partSize: 1, size: 20 })
  await f.upload({ concurrency: 4 }).start()
  assert.equal(f.maxInFlight, 4)
})

test('pause stops sending and resume finishes from what storage holds', async () => {
  const f = fakeClipMux()
  let release!: () => void
  f.state.putGate = new Promise((r) => {
    release = r
  })
  const up = f.upload()
  const done = up.start()
  await new Promise((r) => setTimeout(r, 10))
  up.pause()
  f.state.putGate = null
  release()
  await new Promise((r) => setTimeout(r, 10))
  assert.equal(f.stored.size, 0, 'nothing is stored while paused')
  up.resume()
  const asset = await done
  assert.equal(asset.id, 'ast_1')
  assert.equal(f.stored.size, 3)
})

test('cancel aborts and tells ClipMux', async () => {
  const f = fakeClipMux()
  f.state.putGate = new Promise(() => {})
  const up = f.upload()
  const done = up.start()
  await new Promise((r) => setTimeout(r, 10))
  await up.cancel()
  await assert.rejects(done, (e: ClipMuxUploadError) => e.code === 'cancelled')
  assert.ok(f.calls.includes('POST /v1/uploads/upl_1/cancel'))
})

test('progress counts stored bytes', async () => {
  const f = fakeClipMux()
  const seen: number[] = []
  await f.upload({ onProgress: (p) => seen.push(p.bytesUploaded) }).start()
  assert.deepEqual([...new Set(seen)], [0, 4, 8, 10])
})

test('rememberUpload keeps only what resuming needs', () => {
  const items = new Map<string, string>()
  const store = {
    getItem: (k: string) => items.get(k) ?? null,
    setItem: (k: string, v: string) => void items.set(k, v),
    removeItem: (k: string) => void items.delete(k),
  }
  const file = Object.assign(new Blob(['abc']), { name: 'lesson.mp4', lastModified: 1700000000000 })
  const memory = rememberUpload(store)
  memory.save(file, {
    upload_id: 'upl_1',
    upload_token: 'tok',
    part_size: 4,
    part_count: 1,
    asset_id: 'ast_1',
  })
  assert.deepEqual([...items.keys()], ['clipmux:upload:lesson.mp4:3:1700000000000'])
  assert.deepEqual(memory.load(file), {
    upload_id: 'upl_1',
    upload_token: 'tok',
    part_size: 4,
    part_count: 1,
  })
  memory.forget(file)
  assert.equal(memory.load(file), null)
})

test('a pause between parts stops before the next part is sent', async () => {
  const f = fakeClipMux()
  const up = f.upload({
    onProgress: (p) => {
      if (p.partsDone === 1) up.pause()
    },
  })
  void up.start()
  await new Promise((r) => setTimeout(r, 20))
  assert.deepEqual(
    f.calls.filter((c) => c.startsWith('PUT')),
    ['PUT 1'],
  )
})
