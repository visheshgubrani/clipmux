import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  createRefreshLoader,
  type LoaderCallbacksLike,
  type LoaderLike,
  rewriteTokenUrl,
  TokenSession,
} from './refresh.ts'

const media = 'https://media.example.net'
const A = 'a.a.a'
const B = 'b.b.b'
const C = 'c.c.c'

test('the token in a signed URL of the playback ID is replaced', () => {
  assert.equal(
    rewriteTokenUrl(`${media}/v/${A}/pid_X/out_Y/720p/seg_00001.m4s?x=1`, media, 'pid_X', B),
    `${media}/v/${B}/pid_X/out_Y/720p/seg_00001.m4s?x=1`,
  )
  for (const unchanged of [
    `${media}/p/pid_X/master.m3u8`,
    `https://other.example.net/v/${A}/pid_X/master.m3u8`,
    `${media}/v/${A}/pid_Z/master.m3u8`,
    `${media}/v/${A}/pid_X0/master.m3u8`,
    'not a url',
  ]) {
    assert.equal(rewriteTokenUrl(unchanged, media, 'pid_X', B), unchanged)
  }
})

test('one refresh serves every request that failed with the same token', async () => {
  let calls = 0
  const s = new TokenSession(A, async () => {
    calls++
    return B
  })
  const [x, y] = await Promise.all([s.refresh(A), s.refresh(A)])
  assert.deepEqual([x, y, calls, s.current], [B, B, 1, B])
  assert.equal(await s.refresh(A), B, 'a request that failed with an old token retries with the current one')
  assert.equal(calls, 1)
})

test('null means denied: the session stops and never asks again', async () => {
  let calls = 0
  let denied = 0
  const s = new TokenSession(
    A,
    async () => {
      calls++
      return null
    },
    { onDenied: () => denied++ },
  )
  assert.equal(await s.refresh(A), null)
  assert.equal(await s.refresh(A), null)
  assert.deepEqual([calls, denied, s.denied], [1, 1, true])
})

test('a failing getToken is retried after 1, 2 and 4 s, then the error stands', async () => {
  const waits: number[] = []
  let calls = 0
  const s = new TokenSession(
    A,
    async () => {
      calls++
      if (calls < 3) throw new Error('backend down')
      return B
    },
    { delay: async (ms) => void waits.push(ms) },
  )
  assert.equal(await s.refresh(A), B)
  assert.deepEqual(waits, [1000, 2000])
  const t = new TokenSession(A, async () => Promise.reject(new Error('down')), { delay: async () => {} })
  await assert.rejects(t.refresh(A), /down/)
  await assert.rejects(new TokenSession(A, async () => 'not-a-jwt').refresh(A), /JWT/)
})

// A scripted stand-in for hls.js's loader: each load answers with the next
// status for its URL's token.
function fakeLoader(answers: Record<string, number[]>) {
  const loads: string[] = []
  class Fake implements LoaderLike<{ url: string }> {
    context: { url: string } | null = null
    stats = { loaded: 0 }
    destroyed = false
    load(context: { url: string }, _config: unknown, cb: LoaderCallbacksLike<{ url: string }>): void {
      this.context = context
      loads.push(context.url)
      const token = new URL(context.url).pathname.split('/')[2] ?? ''
      const status = answers[token]?.shift() ?? 200
      queueMicrotask(() => {
        if (status === 200) cb.onSuccess({ url: context.url, data: 'ok' }, this.stats, context, null)
        else cb.onError({ code: status, text: 'x' }, context, null, this.stats)
      })
    }
    abort(): void {}
    destroy(): void {
      this.destroyed = true
    }
  }
  return { Fake, loads }
}

function load(Loader: new (c: unknown) => LoaderLike<{ url: string }>, url: string): Promise<string> {
  return new Promise((resolve) => {
    new Loader({}).load(
      { url },
      {},
      {
        onSuccess: () => resolve('success'),
        onError: (e) => resolve(`error ${e.code}`),
        onTimeout: () => resolve('timeout'),
      },
    )
  })
}

test('a 401 is answered with a new token and a retry the player never sees', async () => {
  const { Fake, loads } = fakeLoader({ [A]: [401] })
  const s = new TokenSession(A, async () => B)
  const Loader = createRefreshLoader(Fake, s, media, 'pid_X')
  assert.equal(await load(Loader, `${media}/v/${A}/pid_X/out_Y/720p/seg_00002.m4s`), 'success')
  assert.deepEqual(loads, [
    `${media}/v/${A}/pid_X/out_Y/720p/seg_00002.m4s`,
    `${media}/v/${B}/pid_X/out_Y/720p/seg_00002.m4s`,
  ])
  // Later requests (the key, other renditions) start with the new token.
  assert.equal(await load(Loader, `${media}/v/${A}/pid_X/out_Y/key/1`), 'success')
  assert.equal(loads.at(-1), `${media}/v/${B}/pid_X/out_Y/key/1`)
})

test('a second 401 for the same request reaches the player instead of looping', async () => {
  const { Fake } = fakeLoader({ [A]: [401], [B]: [401] })
  const Loader = createRefreshLoader(Fake, new TokenSession(A, async () => B), media, 'pid_X')
  assert.equal(await load(Loader, `${media}/v/${A}/pid_X/master.m3u8`), 'error 401')
})

test('a 403 stops the session; later requests fail without a request', async () => {
  const { Fake, loads } = fakeLoader({ [A]: [403] })
  let refreshed = 0
  const s = new TokenSession(A, async () => {
    refreshed++
    return C
  })
  const Loader = createRefreshLoader(Fake, s, media, 'pid_X')
  assert.equal(await load(Loader, `${media}/v/${A}/pid_X/master.m3u8`), 'error 403')
  assert.equal(s.denied, true)
  assert.equal(await load(Loader, `${media}/v/${A}/pid_X/out_Y/720p/index.m3u8`), 'error 403')
  assert.deepEqual([loads.length, refreshed], [1, 0])
})

test('a 404 passes through without asking for a token', async () => {
  const { Fake } = fakeLoader({ [A]: [404] })
  let refreshed = 0
  const Loader = createRefreshLoader(
    Fake,
    new TokenSession(A, async () => {
      refreshed++
      return B
    }),
    media,
    'pid_X',
  )
  assert.equal(await load(Loader, `${media}/v/${A}/pid_X/master.m3u8`), 'error 404')
  assert.equal(refreshed, 0)
})
