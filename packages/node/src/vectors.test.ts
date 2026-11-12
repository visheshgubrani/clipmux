// The SDK against the shared contracts (D112): tokens it signs must pass the
// delivery vectors through the Worker's verifier exactly as the Go-signed
// ones do, and its webhook check must agree with every webhook vector.

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { createWorker } from '../../../worker/src/delivery.ts'
import { vectorEnv, vectors } from '../../../worker/src/fixture.test.ts'
import { WebhookVerificationError } from './errors.ts'
import { importSigningKey, signToken } from './tokens.ts'
import { verifyWebhook } from './webhooks.ts'

interface Claims {
  kid: string
  sub: string
  aud: 'v' | 't'
  iat: number
  exp: number
  vid?: string
  raw?: string
}
interface Key {
  kid: string
  public_key_pem: string
  private_key_pem?: string
}

const keys = vectors.signing_keys as Key[]
const claims = vectors.token_claims as Record<string, Claims>
const stored = vectors.tokens as Record<string, string>

async function sdkToken(c: Claims): Promise<string> {
  // A kid with no key in the vectors (kid_nobody) is signed with app a's.
  const k = keys.find((x) => x.kid === c.kid && x.private_key_pem) ?? keys.find((x) => x.kid === 'kid_app_a')
  assert.ok(k?.private_key_pem)
  return signToken(await importSigningKey(k.private_key_pem), c.kid, c)
}

const signed = new Map<string, string>()
for (const [name, c] of Object.entries(claims)) {
  if (c.raw) continue // claim spellings the SDK never writes, e.g. "SUB"
  signed.set(name, await sdkToken(c))
}

test('SDK tokens have the Go signer header and claims, and verify', async () => {
  for (const [name, tok] of signed) {
    const [h, p, sig] = tok.split('.')
    const [wantH, wantP] = (stored[name] as string).split('.')
    assert.equal(h, wantH, `${name} header`)
    assert.equal(p, wantP, `${name} claims`)
    const k = keys.find((x) => x.kid === claims[name]?.kid)
    if (!k) continue
    const der = Buffer.from(k.public_key_pem.replace(/-----[A-Z ]+-----|\s/g, ''), 'base64')
    const pub = await crypto.subtle.importKey('spki', der, { name: 'ECDSA', namedCurve: 'P-256' }, false, [
      'verify',
    ])
    const ok = await crypto.subtle.verify(
      { name: 'ECDSA', hash: 'SHA-256' },
      pub,
      Buffer.from(sig as string, 'base64url'),
      new TextEncoder().encode(`${h}.${p}`),
    )
    assert.ok(ok, `${name} signature`)
  }
})

test('the signing key reads as PEM, PEM with \\n escapes, and base64 DER', async () => {
  const pem = keys[0]?.private_key_pem as string
  const der = pem.replace(/-----[A-Z ]+-----|\s/g, '')
  for (const text of [pem, pem.replace(/\n/g, '\\n'), der]) {
    const tok = await signToken(await importSigningKey(text), 'kid_x', { sub: 'p', aud: 'v', iat: 1, exp: 2 })
    assert.equal(tok.split('.').length, 3)
  }
  await assert.rejects(importSigningKey('not a key'), /CLIPMUX_SIGNING_KEY/)
})

// Every delivery case, with each token the SDK can write swapped for the
// SDK's own signature: the Worker must answer exactly as the vectors say.
const worker = createWorker({ now: () => vectors.now, cache: null })
const { env } = vectorEnv()
for (const c of vectors.cases as {
  name: string
  path: string
  method?: string
  range?: string
  status: number
}[]) {
  test(`delivery: ${c.name}`, async () => {
    let path = c.path
    for (const [name, tok] of Object.entries(stored))
      path = path.replaceAll(`{${name}}`, signed.get(name) ?? tok)
    const res = await worker.fetch(
      new Request(`https://media.example${path}`, {
        method: c.method ?? 'GET',
        headers: c.range ? { Range: c.range } : {},
      }),
      env,
    )
    await res.arrayBuffer()
    assert.equal(res.status, c.status)
  })
}

const hooks = JSON.parse(
  readFileSync(new URL('../../../internal/webhooks/testdata/vectors.json', import.meta.url), 'utf8'),
) as {
  tolerance_seconds: number
  cases: {
    name: string
    secret: string
    now: number
    body: string
    headers: Record<string, string>
    valid: boolean
    reason?: string
  }[]
}
for (const c of hooks.cases) {
  test(`webhook: ${c.name}`, async () => {
    const run = () =>
      verifyWebhook(c.secret, c.body, c.headers, { now: c.now, tolerance: hooks.tolerance_seconds })
    if (c.valid) {
      assert.deepEqual(await run(), JSON.parse(c.body))
      // Headers arrive as a Fetch Headers object too, and the body as bytes.
      const h = new Headers(c.headers)
      await verifyWebhook(c.secret, new TextEncoder().encode(c.body), h, { now: c.now })
    } else {
      await assert.rejects(
        run,
        (e: unknown) => e instanceof WebhookVerificationError && e.reason === c.reason,
      )
    }
  })
}
