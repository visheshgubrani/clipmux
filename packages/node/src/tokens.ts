// ES256 playback tokens (D015), signed offline with the app's signing key
// through Web Crypto, so they work on Node, Bun, Deno and edge runtimes.
// The header and claims are written in the same order as the Go signer, so
// a token for the same claims has the same first two segments.

import { ClipMuxConfigError } from './errors.ts'

/** What a token lets its holder fetch: everything, or only the poster. */
export type Audience = 'video' | 'poster'

export interface TokenClaims {
  /** The playback ID. */
  sub: string
  aud: 'v' | 't'
  iat: number
  exp: number
  /** A stable viewer ID for unique-viewer analytics (D103). */
  vid?: string | undefined
}

const enc = new TextEncoder()

export function b64url(bytes: Uint8Array): string {
  let bin = ''
  for (const b of bytes) bin += String.fromCharCode(b)
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function b64decode(s: string): Uint8Array {
  const bin = atob(s)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

/**
 * Reads CLIPMUX_SIGNING_KEY as the dashboard or `init` gives it: a PKCS #8
 * PEM, the same PEM with its newlines written as `\n` (common in .env files
 * and CI secrets), or the base64 of its DER.
 */
export async function importSigningKey(text: string, variable = 'CLIPMUX_SIGNING_KEY'): Promise<CryptoKey> {
  const pem = text.trim().replace(/\\n/g, '\n')
  if (pem.includes('BEGIN EC PRIVATE KEY')) {
    throw new ClipMuxConfigError(
      variable,
      `${variable} is a SEC 1 key; ClipMux signing keys are PKCS #8 (convert with \`openssl pkcs8 -topk8 -nocrypt\`).`,
    )
  }
  const body = pem.replace(/-----(BEGIN|END) PRIVATE KEY-----/g, '').replace(/\s+/g, '')
  let der: Uint8Array
  try {
    der = b64decode(body)
  } catch {
    throw new ClipMuxConfigError(variable, `${variable} is not a PEM or base64 private key.`)
  }
  try {
    return await crypto.subtle.importKey(
      'pkcs8',
      der as Uint8Array<ArrayBuffer>,
      { name: 'ECDSA', namedCurve: 'P-256' },
      false,
      ['sign'],
    )
  } catch {
    throw new ClipMuxConfigError(variable, `${variable} is not a P-256 private key.`)
  }
}

/** Signs claims as a compact JWT with header {alg, kid, typ}. */
export async function signToken(key: CryptoKey, kid: string, c: TokenClaims): Promise<string> {
  const header = b64url(enc.encode(JSON.stringify({ alg: 'ES256', kid, typ: 'JWT' })))
  const claims: TokenClaims = { sub: c.sub, aud: c.aud, iat: c.iat, exp: c.exp }
  if (c.vid) claims.vid = c.vid
  const payload = b64url(enc.encode(JSON.stringify(claims)))
  const input = `${header}.${payload}`
  // Web Crypto's ECDSA signature is already JWS's r||s form.
  const sig = await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, key, enc.encode(input))
  return `${input}.${b64url(new Uint8Array(sig))}`
}

/**
 * Seconds from a number of seconds or a string such as `90s`, `10m`, `4h`
 * or `7d`.
 */
export function seconds(v: number | string): number {
  if (typeof v === 'number') {
    if (!Number.isFinite(v) || v <= 0) throw new RangeError(`expiresIn must be positive, got ${v}`)
    return Math.floor(v)
  }
  const m = /^(\d+)\s*(s|m|h|d)$/.exec(v.trim())
  if (!m)
    throw new RangeError(`expiresIn must be seconds or like "10m", "4h", "7d"; got ${JSON.stringify(v)}`)
  const n = Number(m[1])
  const unit = { s: 1, m: 60, h: 3600, d: 86400 }[m[2] as 's' | 'm' | 'h' | 'd']
  return n * unit
}
