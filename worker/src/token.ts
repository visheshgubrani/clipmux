// ES256 playback tokens, read exactly as the Go handler reads them (D015):
// strict base64url, exact claim names with the last duplicate winning (as
// JSON.parse does), iat and exp as integer literals, and both ECDSA forms.

export interface Claims {
  sub: string
  aud: string
  iat: number
  exp: number
  // The optional viewer ID; delivery keeps only its keyed hash (D103).
  vid: string
}

export interface Token {
  kid: string
  claims: Claims
  input: string
  sig: Uint8Array
}

const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_'
const INDEX = new Map([...ALPHABET].map((c, i) => [c, i]))

// b64urlDecode is Go's base64.RawURLEncoding.Strict(): no padding, only the
// URL alphabet, and the unused low bits of the last character must be zero,
// so a token has exactly one spelling.
export function b64urlDecode(s: string): Uint8Array | null {
  if (s.length % 4 === 1) return null
  const out = new Uint8Array(Math.floor((s.length * 6) / 8))
  let acc = 0
  let bits = 0
  let o = 0
  for (const c of s) {
    const v = INDEX.get(c)
    if (v === undefined) return null
    acc = (acc << 6) | v
    bits += 6
    if (bits >= 8) {
      bits -= 8
      out[o++] = (acc >> bits) & 0xff
    }
  }
  if (bits > 0 && (acc & ((1 << bits) - 1)) !== 0) return null
  return out
}

function object(part: string): { obj: Record<string, unknown>; raw: string } | null {
  const bytes = b64urlDecode(part)
  if (!bytes) return null
  let raw: string
  try {
    raw = new TextDecoder('utf-8', { fatal: true, ignoreBOM: false }).decode(bytes)
  } catch {
    return null
  }
  let obj: unknown
  try {
    obj = JSON.parse(raw)
  } catch {
    return null
  }
  if (typeof obj !== 'object' || obj === null || Array.isArray(obj)) return null
  return { obj: obj as Record<string, unknown>, raw }
}

const str = (o: Record<string, unknown>, name: string): string => {
  const v = o[name]
  return typeof v === 'string' ? v : ''
}

// integer accepts what Go's json.Unmarshal into int64 accepts: an integer
// literal (no fraction or exponent), or null, which reads as 0.
function integer(o: Record<string, unknown>, raw: string, name: string): number | null {
  if (!(name in o)) return null
  const v = o[name]
  if (v === null) return 0
  if (typeof v !== 'number' || !Number.isSafeInteger(v)) return null
  // The last occurrence of the name holds the value JSON.parse kept.
  const re = new RegExp(`"${name}"\\s*:\\s*(-?[0-9][0-9.eE+-]*)`, 'g')
  let literal = ''
  for (const m of raw.matchAll(re)) literal = m[1] ?? ''
  return /^-?(0|[1-9][0-9]*)$/.test(literal) ? v : null
}

export function parse(token: string): Token | null {
  const parts = token.split('.')
  if (parts.length !== 3 || token.length > 4096) return null
  const [h, p, s] = parts as [string, string, string]
  const hdr = object(h)
  if (!hdr || str(hdr.obj, 'alg') !== 'ES256' || str(hdr.obj, 'kid') === '') return null
  const payload = object(p)
  if (!payload) return null
  const iat = integer(payload.obj, payload.raw, 'iat')
  const exp = integer(payload.obj, payload.raw, 'exp')
  if (iat === null || exp === null) return null
  const sig = b64urlDecode(s)
  if (sig?.length !== 64) return null
  return {
    kid: str(hdr.obj, 'kid'),
    claims: {
      sub: str(payload.obj, 'sub'),
      aud: str(payload.obj, 'aud'),
      iat,
      exp,
      vid: str(payload.obj, 'vid'),
    },
    input: `${h}.${p}`,
    sig,
  }
}

export interface PublicJWK {
  kty: string
  crv: string
  x: string
  y: string
}

// verify checks the raw r||s signature. WebCrypto accepts both (r, s) and
// (r, n-s), as Go's ecdsa.Verify does.
export async function verify(t: Token, jwk: PublicJWK): Promise<boolean> {
  try {
    const key = await crypto.subtle.importKey(
      'jwk',
      { kty: jwk.kty, crv: jwk.crv, x: jwk.x, y: jwk.y, ext: true },
      { name: 'ECDSA', namedCurve: 'P-256' },
      false,
      ['verify'],
    )
    return await crypto.subtle.verify(
      { name: 'ECDSA', hash: 'SHA-256' },
      key,
      t.sig,
      new TextEncoder().encode(t.input),
    )
  } catch {
    return false
  }
}
