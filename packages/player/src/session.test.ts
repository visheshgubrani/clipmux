import assert from 'node:assert/strict'
import { test } from 'node:test'
import { chaptersTrackOf, fetchSession, type SessionError } from './session.ts'

// What POST /s/{secret}/playback answers, which the app's session URL copies.
const session = {
  playback_id: 'pid_X',
  base_url: 'https://media.example.net',
  token: 'a.b.c',
  expires_at: '2026-11-12T12:00:00Z',
  poster_url: 'https://media.example.net/v/a.b.c/pid_X/poster.jpg',
  tracks: [
    { id: 'trk_S', kind: 'subtitles', status: 'ready', language: 'en' },
    { id: 'trk_P', kind: 'chapters', status: 'preparing' },
    { id: 'trk_C', kind: 'chapters', status: 'ready' },
  ],
}

function backend(status: number, body: unknown) {
  const requests: { url: string; init: RequestInit | undefined }[] = []
  const fetchFn = (async (input: string | URL | Request, init?: RequestInit) => {
    requests.push({ url: String(input), init })
    return new Response(typeof body === 'string' ? body : JSON.stringify(body), { status })
  }) as typeof fetch
  return { requests, fetchFn }
}

test('the session URL is asked for the playback ID and whether playback is native', async () => {
  const b = backend(200, session)
  assert.deepEqual(await fetchSession('/api/clipmux/playback', 'pid_X', true, b.fetchFn), session)
  const [req] = b.requests
  assert.equal(req?.url, '/api/clipmux/playback')
  assert.equal(req?.init?.method, 'POST')
  assert.deepEqual(JSON.parse(String(req?.init?.body)), { playback_id: 'pid_X', native: true })
  assert.deepEqual(req?.init?.headers, { 'Content-Type': 'application/json' })
})

test('401 and 403 deny the viewer', async () => {
  for (const status of [401, 403]) {
    assert.equal(await fetchSession('/s', 'pid_X', false, backend(status, { error: {} }).fetchFn), null)
  }
})

test('anything else that is not a session fails with its status', async () => {
  const failures: [number, unknown][] = [
    [500, { error: {} }],
    [404, ''],
    [200, 'not json'],
    [200, { token: 'a.b.c' }],
  ]
  for (const [status, body] of failures) {
    await assert.rejects(
      fetchSession('/s', 'pid_X', false, backend(status, body).fetchFn),
      (e: SessionError) => e.name === 'SessionError' && e.status === status,
    )
  }
  const down = (async () => {
    throw new TypeError('fetch failed')
  }) as typeof fetch
  await assert.rejects(fetchSession('/s', 'pid_X', false, down), (e: SessionError) => e.status === 0)
})

test('a session without its playback ID plays the one asked for', async () => {
  const { playback_id: _, ...rest } = session
  assert.equal((await fetchSession('/s', 'pid_X', false, backend(200, rest).fetchFn))?.playback_id, 'pid_X')
})

test('the first ready chapters track is the session default', () => {
  assert.equal(chaptersTrackOf(session), 'trk_C')
  assert.equal(chaptersTrackOf({ ...session, tracks: [] }), '')
  assert.equal(chaptersTrackOf(null), '')
})
