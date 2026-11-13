import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { mkdtemp, readFile, stat, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { type IO, main } from './cli.ts'
import { mergeEnv, parseEnv } from './envfile.ts'
import { verifyWebhook } from './webhooks.ts'

const vectors = JSON.parse(
  readFileSync(new URL('../../../internal/delivery/testdata/vectors.json', import.meta.url), 'utf8'),
)
const keyA = vectors.signing_keys.find((k: { kid: string }) => k.kid === 'kid_app_a')
const keyB = vectors.signing_keys.find((k: { kid: string }) => k.kid === 'kid_app_b')

type Route = (body: unknown, url: URL, init: RequestInit) => Response | Promise<Response>

function harness(cwd: string, routes: Record<string, Route | Route[]>, env: Record<string, string> = {}) {
  const out: string[] = []
  const opened: string[] = []
  const requests: { url: URL; init: RequestInit }[] = []
  const ac = new AbortController()
  const io: IO = {
    out: (l) => out.push(l),
    err: (l) => out.push(l),
    env,
    cwd,
    fetch: (async (input: string | URL, init: RequestInit = {}) => {
      const url = new URL(input)
      requests.push({ url, init })
      const key = `${init.method ?? 'GET'} ${url.host === 'app.local' ? url.href : url.pathname}`
      let r = routes[key]
      if (Array.isArray(r)) r = r.length > 1 ? r.shift() : r[0]
      if (!r) return Response.json({ error: { code: 'not_found', message: key } }, { status: 404 })
      return r(init.body ? JSON.parse(init.body as string) : undefined, url, init)
    }) as typeof fetch,
    sleep: async () => {},
    open: (u) => opened.push(u),
    signal: ac.signal,
  }
  return { io, out, opened, requests, ac }
}

const credentials = {
  url: 'https://video.example.com',
  app_id: 'app_lms',
  app_name: 'Course site',
  api_key_id: 'key_1',
  api_key: 'cmx_SECRETAPIKEY',
  signing_kid: 'kid_app_a',
  signing_key: keyA.private_key_pem,
  env: '…',
}
const pairing = {
  code: 'KXFT-9M2Q',
  poll_token: 'poll_SECRET',
  verify_url: 'https://video.example.com/connect?code=KXFT-9M2Q',
  expires_at: '2030-01-01T00:00:00Z',
  interval_seconds: 2,
}

test('init pairs, writes the env file beside existing lines, and prints no secret', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'cmx-'))
  await writeFile(join(dir, '.env.local'), 'DATABASE_URL=postgres://x\nCLIPMUX_URL=https://old.example\n')
  await writeFile(join(dir, 'package.json'), JSON.stringify({ name: 'lms', dependencies: { next: '15' } }))
  let started: unknown
  const h = harness(dir, {
    'POST /v1/app-pairings': (b) => {
      started = b
      return Response.json(pairing, { status: 201 })
    },
    'POST /v1/app-pairings/poll': [
      () => Response.json({ status: 'pending' }),
      () =>
        Response.json({ status: 'approved', credentials: { ...credentials, webhook_secret: 'whsec_REAL' } }),
    ],
  })
  assert.equal(await main(['init', '--server', 'video.example.com'], h.io), 0)
  assert.equal((started as { name: string }).name, 'lms')
  assert.deepEqual(h.opened, [pairing.verify_url])
  assert.equal(h.requests[0]?.url.origin, 'https://video.example.com', 'a bare host means https')

  const text = await readFile(join(dir, '.env.local'), 'utf8')
  const env = parseEnv(text)
  assert.equal(env.DATABASE_URL, 'postgres://x')
  assert.equal(env.CLIPMUX_URL, 'https://video.example.com')
  assert.equal(env.CLIPMUX_API_KEY, 'cmx_SECRETAPIKEY')
  assert.equal(env.CLIPMUX_WEBHOOK_SECRET, 'whsec_REAL')
  assert.equal(env.CLIPMUX_SIGNING_KEY?.replace(/\\n/g, '\n'), keyA.private_key_pem.trimEnd())
  assert.equal(text.match(/CLIPMUX_URL=/g)?.length, 1, 'the old CLIPMUX_URL line is replaced, not duplicated')

  const printed = h.out.join('\n')
  for (const secret of [
    'cmx_SECRETAPIKEY',
    'whsec_REAL',
    'poll_SECRET',
    'PRIVATE KEY',
    keyA.private_key_pem.split('\n')[1],
  ]) {
    assert.ok(!printed.includes(secret), `printed ${secret}`)
  }
  assert.match(printed, /KXFT-9M2Q/)
  assert.match(printed, /app\/api\/clipmux\/\[action\]\/route\.ts/, 'Next.js next steps')
})

test('init makes a new env file private and a local webhook secret when there is no endpoint', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'cmx-'))
  const h = harness(dir, {
    'POST /v1/app-pairings': () => Response.json(pairing, { status: 201 }),
    'POST /v1/app-pairings/poll': () => Response.json({ status: 'approved', credentials }),
  })
  assert.equal(await main(['init', '--server', 'https://video.example.com', '--no-open'], h.io), 0)
  assert.deepEqual(h.opened, [])
  const file = join(dir, '.env.local')
  assert.equal((await stat(file)).mode & 0o777, 0o600)
  assert.match(
    parseEnv(await readFile(file, 'utf8')).CLIPMUX_WEBHOOK_SECRET ?? '',
    /^whsec_local_[0-9a-f]{32}$/,
  )
  assert.match(h.out.join('\n'), /local secret for `npx @clipmux\/node listen`/)
})

test('init stops on a denial or an expiry, and needs a server', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'cmx-'))
  for (const status of ['denied', 'expired']) {
    const h = harness(dir, {
      'POST /v1/app-pairings': () => Response.json(pairing, { status: 201 }),
      'POST /v1/app-pairings/poll': () => Response.json({ status }),
    })
    assert.equal(await main(['init', '--server', 'https://v.example', '--no-open'], h.io), 1, status)
  }
  assert.equal(await main(['init'], harness(dir, {}).io), 2)
})

function app(over: Record<string, unknown> = {}) {
  return () =>
    Response.json({
      id: 'app_lms',
      name: 'Course site',
      scopes: ['uploads:write', 'assets:read', 'assets:write'],
      public_url: 'https://video.example.com',
      base_url: 'https://media.example.net',
      max_token_lifetime_seconds: 604800,
      signing_keys: [
        { kid: 'kid_app_a', public_key_pem: keyA.public_key_pem, created_at: '2027-01-01T00:00:00Z' },
      ],
      ...over,
    })
}
const goodEnv = {
  CLIPMUX_URL: 'https://video.example.com',
  CLIPMUX_API_KEY: 'cmx_SECRETAPIKEY',
  CLIPMUX_SIGNING_KID: 'kid_app_a',
  CLIPMUX_SIGNING_KEY: keyA.private_key_pem.replace(/\n/g, '\\n'),
  CLIPMUX_WEBHOOK_SECRET: 'whsec_REAL',
}

test('doctor passes a matching setup and prints no secret', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'cmx-'))
  await writeFile(join(dir, '.env'), mergeEnv('', goodEnv, '# test'))
  const h = harness(dir, { 'GET /v1/app': app() })
  assert.equal(await main(['doctor'], h.io), 0, h.out.join('\n'))
  const printed = h.out.join('\n')
  assert.match(printed, /CLIPMUX_SIGNING_KEY matches kid_app_a/)
  assert.ok(
    !printed.includes('cmx_SECRETAPIKEY') && !printed.includes('whsec_REAL') && !printed.includes('PRIVATE'),
  )
})

test('doctor names what is wrong', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'cmx-'))
  const wrongKey = { ...goodEnv, CLIPMUX_SIGNING_KEY: keyB.private_key_pem }
  let h = harness(dir, { 'GET /v1/app': app() }, wrongKey)
  assert.equal(await main(['doctor'], h.io), 1)
  assert.match(h.out.join('\n'), /not the private half of kid_app_a/)

  h = harness(dir, { 'GET /v1/app': app({ signing_keys: [] }) }, goodEnv)
  assert.equal(await main(['doctor'], h.io), 1)
  assert.match(h.out.join('\n'), /not an active signing key/)

  h = harness(
    dir,
    {
      'GET /v1/app': () =>
        Response.json({ error: { code: 'unauthorized', message: 'bad key' } }, { status: 401 }),
    },
    goodEnv,
  )
  assert.equal(await main(['doctor'], h.io), 1)
  assert.match(h.out.join('\n'), /unauthorized.*CLIPMUX_API_KEY/)
})

// eventsRoute answers GET /v1/events as internal/assets/events.go does:
// since and after must be RFC 3339 times, and the page is every event from
// since, oldest first, at most 100.
const rfc3339 = /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)$/
function eventsRoute(visible: () => { id: string; created_at: string; body: unknown }[], afters: string[]) {
  return (_b: unknown, url: URL) => {
    for (const name of ['since', 'after']) {
      const v = url.searchParams.get(name)
      if (v !== null && !rfc3339.test(v))
        return Response.json(
          { error: { code: 'invalid_request', message: `${name} must be an RFC 3339 time` } },
          { status: 400 },
        )
    }
    afters.push(url.searchParams.get('after') ?? '')
    const since = Date.parse(url.searchParams.get('since') ?? '1970-01-01T00:00:00Z')
    return Response.json({
      data: visible()
        .filter((e) => Date.parse(e.created_at) >= since)
        .slice(0, 100),
    })
  }
}

test('listen forwards each event once, signed so the app verifies it, across polls', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'cmx-'))
  const at = (msAgo: number) => new Date(Date.now() - msAgo).toISOString()
  const e1 = { id: 'evt_1', type: 'video.asset.ready', created_at: at(120_000), data: { id: 'ast_1' } }
  const e2 = { id: 'evt_2', type: 'video.asset.updated', created_at: at(60_000), data: { id: 'ast_1' } }
  const forwarded: { headers: Headers; body: string }[] = []
  const afters: string[] = []
  let polls = 0
  const route = eventsRoute(() => {
    polls++
    if (polls === 4) h.ac.abort()
    // evt_2 commits after the first poll; the overlap re-reads evt_1.
    const all = polls === 1 ? [e1] : [e1, e2]
    return all.map((e) => ({ id: e.id, created_at: e.created_at, body: e }))
  }, afters)
  const h = harness(
    dir,
    {
      'GET /v1/app': app(),
      'GET /v1/events': route,
      'POST http://app.local/api/clipmux/webhook': (_b, _u, init) => {
        forwarded.push({ headers: new Headers(init.headers), body: init.body as string })
        return new Response(null, { status: 200 })
      },
    },
    goodEnv,
  )
  assert.equal(
    await main(['listen', '--forward-to', 'app.local/api/clipmux/webhook', '--since', '5m'], h.io),
    0,
    h.out.join('\n'),
  )
  assert.deepEqual(
    forwarded.map((f) => f.headers.get('X-Webhook-Id')),
    ['evt_1', 'evt_2'],
  )
  assert.deepEqual(afters.slice(0, 3), ['', e1.created_at, e2.created_at])
  const f = forwarded[1] as { headers: Headers; body: string }
  assert.deepEqual(await verifyWebhook('whsec_REAL', f.body, f.headers), e2)
})

test('env files: quoted values, comments, export, and merging keeps other lines', () => {
  assert.deepEqual(parseEnv('export A=1\nB="x\\ny" \n# c\nC=plain # note\nD=\'q\''), {
    A: '1',
    B: 'x\\ny',
    C: 'plain',
    D: 'q',
  })
  assert.equal(mergeEnv('A=1\nB=2\n', { B: '3', C: 'has space' }, '# h'), 'A=1\nB=3\n\n# h\nC="has space"\n')
})
