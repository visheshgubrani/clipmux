// Webhook signatures (D018): X-Webhook-Signature is "sha256=" and the hex
// HMAC-SHA256 of "{X-Webhook-Timestamp}.{raw body}", keyed with the
// endpoint secret's bytes, whsec_ prefix included. The contract is
// internal/webhooks/testdata/vectors.json.

import { WebhookVerificationError } from './errors.ts'
import type { WebhookEvent as RawEvent } from './schema.gen.ts'

type TrackType = Extract<RawEvent['type'], `video.asset.track.${string}`>

/** An event about the asset itself: ready, updated, errored, re-encode failed, deleted. */
export type AssetEvent = Omit<RawEvent, 'type' | 'track'> & { type: Exclude<RawEvent['type'], TrackType> }

/** An event about one of the asset's tracks; it always names the track. */
export type TrackEvent = Omit<RawEvent, 'type' | 'track'> & {
  type: TrackType
  track: { id: string; kind: 'subtitles' | 'chapters'; error_code?: string }
}

/**
 * A webhook event, discriminated on `type`: narrow on it and a track event
 * has its `track`. `data` is the asset as GET /v1/assets/{id} returns it.
 */
export type WebhookEvent = AssetEvent | TrackEvent

/** Headers as a Fetch Headers object, or a Node/Express headers record. */
export type HeadersLike = Headers | Record<string, string | string[] | undefined>

export type RawBody = string | Uint8Array | ArrayBuffer

const enc = new TextEncoder()

function header(h: HeadersLike, name: string): string | undefined {
  if (typeof (h as Headers).get === 'function') return (h as Headers).get(name) ?? undefined
  const rec = h as Record<string, string | string[] | undefined>
  const want = name.toLowerCase()
  for (const [k, v] of Object.entries(rec)) {
    if (k.toLowerCase() === want) return Array.isArray(v) ? v[0] : v
  }
  return undefined
}

function bytes(body: RawBody): Uint8Array {
  if (typeof body === 'string') return enc.encode(body)
  return body instanceof Uint8Array ? body : new Uint8Array(body)
}

async function hmacHex(secret: string, timestamp: string, body: Uint8Array): Promise<string> {
  const key = await crypto.subtle.importKey(
    'raw',
    enc.encode(secret),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign'],
  )
  const prefix = enc.encode(`${timestamp}.`)
  const msg = new Uint8Array(prefix.length + body.length)
  msg.set(prefix)
  msg.set(body, prefix.length)
  const mac = new Uint8Array(await crypto.subtle.sign('HMAC', key, msg))
  return Array.from(mac, (b) => b.toString(16).padStart(2, '0')).join('')
}

function equal(a: string, b: string): boolean {
  if (a.length !== b.length) return false
  let diff = 0
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i)
  return diff === 0
}

/** The X-Webhook-Signature value ClipMux sends for a body at a timestamp. */
export async function signWebhook(secret: string, timestamp: number, body: RawBody): Promise<string> {
  return `sha256=${await hmacHex(secret, String(timestamp), bytes(body))}`
}

export interface VerifyOptions {
  /** Seconds a timestamp may be off the clock; default 300. */
  tolerance?: number
  /** The clock, in Unix seconds; tests replace it. */
  now?: number
}

/**
 * Checks a webhook request and returns its event. Pass the raw body as it
 * arrived: a body parsed and serialized again no longer matches. Throws a
 * WebhookVerificationError (answer 400) for anything not from ClipMux.
 */
export async function verifyWebhook(
  secret: string,
  rawBody: RawBody,
  headers: HeadersLike,
  opts: VerifyOptions = {},
): Promise<WebhookEvent> {
  const ts = header(headers, 'X-Webhook-Timestamp')
  const sig = header(headers, 'X-Webhook-Signature')
  if (!ts || !sig) {
    throw new WebhookVerificationError(
      'missing_header',
      'X-Webhook-Timestamp and X-Webhook-Signature are required.',
    )
  }
  const now = opts.now ?? Math.floor(Date.now() / 1000)
  const tolerance = opts.tolerance ?? 300
  if (!/^\d+$/.test(ts) || Math.abs(now - Number(ts)) > tolerance) {
    throw new WebhookVerificationError('stale', `the timestamp is more than ${tolerance} s from this clock.`)
  }
  const body = bytes(rawBody)
  const want = `sha256=${await hmacHex(secret, ts, body)}`
  if (!equal(sig, want)) {
    throw new WebhookVerificationError(
      'bad_signature',
      'the signature does not match: check CLIPMUX_WEBHOOK_SECRET and that the body is the raw request body.',
    )
  }
  // Signature only (the vectors are the contract): the body is trusted as
  // ClipMux's event once it verifies.
  try {
    return JSON.parse(new TextDecoder().decode(body)) as WebhookEvent
  } catch {
    throw new WebhookVerificationError('invalid_body', 'the body is not JSON.')
  }
}
