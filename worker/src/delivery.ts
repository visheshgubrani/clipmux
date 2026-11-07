// The ClipMux Cloudflare Worker (docs/architecture.md, "Delivery"). It makes
// the same decisions as the built-in Go handler (internal/delivery), from
// what the route publisher writes into its bucket instead of from Postgres:
//
//   _routes/{playback_id}.json   the route record (internal/routes)
//   _keys.json                   the signing keys delivery accepts
//
// Both pass internal/delivery/testdata/vectors.json; change them together.

import { type PublicJWK, parse, verify } from './token.ts'

export interface Env {
  MEDIA: R2Bucket
  // JSON {"<version>": "<64 hex digits>"}: the content key secret ring.
  CONTENT_KEYS: string
  // The built-in handler, for routes whose backend is "origin".
  ORIGIN_URL?: string
}

interface Route {
  v: number
  app_id: string
  asset_id: string
  output_id: string
  policy: 'signed' | 'public'
  backend: 'r2' | 'origin'
  outputs: Record<string, number>
  denied?: boolean
  // The tracks tracks/{id}.vtt may name (internal/routes.Track).
  tracks?: Record<string, Track>
}

interface Track {
  kind: 'subtitles' | 'chapters'
  language: string
  name: string
  duration_ms: number
  key_version: number
  listed: boolean
}

interface Keys {
  v: number
  max_token_lifetime_seconds: number
  keys: { kid: string; app_id: string | null; jwk: PublicJWK; not_after: number | null }[]
}

export interface Options {
  // Unix seconds; tests pin it.
  now?: () => number
  // The edge cache; null turns it off (Node tests have none).
  cache?: Cache | null
  // How long a route or _keys.json is reused (architecture: up to 60 s).
  ttlMs?: number
  // How long "no such route" is reused, so a new ID plays soon.
  missTtlMs?: number
  fetchOrigin?: (req: Request) => Promise<Response>
}

const CONTENT_TYPES: Record<string, string> = {
  '.m3u8': 'application/vnd.apple.mpegurl',
  '.mp4': 'video/mp4',
  '.m4s': 'video/iso.segment',
  '.jpg': 'image/jpeg',
}

// A track's files: its playlist or its VTT (delivery.trackPath).
const TRACK_PATH = /^(trk_[0-9a-z_]{1,64})\.(m3u8|vtt)$/

// The output path grammar (internal/media.ValidOutputPath).
const OUTPUT_PATH =
  /^(manifest\.json|master\.m3u8|poster\.jpg|([1-9][0-9]{1,3}p|audio)\/(index\.m3u8|init\.mp4|seg_[0-9]{5,6}\.m4s))$/

const CLOCK_SKEW = 60
const IMMUTABLE = 'max-age=31536000, immutable'

function common(h: Headers): Headers {
  h.set('Access-Control-Allow-Origin', '*')
  h.set('Access-Control-Expose-Headers', 'Content-Length, Content-Range')
  h.set('X-Content-Type-Options', 'nosniff')
  return h
}

function error(status: number, code: string, message: string): Response {
  const h = common(new Headers({ 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }))
  return new Response(`${JSON.stringify({ error: { code, message } })}\n`, { status, headers: h })
}
const notFound = () => error(404, 'not_found', 'not found')
const denied = () => error(403, 'denied', 'denied')
const expired = () => error(401, 'token_expired', 'fetch a new token')

function ext(path: string): string {
  const i = path.lastIndexOf('.')
  return i < 0 || path.lastIndexOf('/') > i ? '' : path.slice(i)
}

interface Cached<T> {
  value: T
  until: number
}

export function createWorker(opts: Options = {}) {
  const now = opts.now ?? (() => Math.floor(Date.now() / 1000))
  const ttl = opts.ttlMs ?? 60_000
  const missTtl = opts.missTtlMs ?? 5_000
  const routes = new Map<string, Cached<Route | null>>()
  let keys: Cached<Keys | null> | undefined
  let secrets: { raw: string; ring: Map<number, Promise<CryptoKey>> } | undefined

  async function route(env: Env, pid: string): Promise<Route | null> {
    const hit = routes.get(pid)
    if (hit && hit.until > Date.now()) return hit.value
    const obj = await env.MEDIA.get(`_routes/${pid}.json`)
    const value = obj ? ((await obj.json()) as Route) : null
    routes.set(pid, { value, until: Date.now() + (value ? ttl : missTtl) })
    if (routes.size > 10_000) routes.delete(routes.keys().next().value as string)
    return value
  }

  async function signingKeys(env: Env): Promise<Keys | null> {
    if (keys && keys.until > Date.now()) return keys.value
    const obj = await env.MEDIA.get('_keys.json')
    const value = obj ? ((await obj.json()) as Keys) : null
    keys = { value, until: Date.now() + ttl }
    return value
  }

  function secret(env: Env, version: number): Promise<CryptoKey> | undefined {
    if (!secrets || secrets.raw !== env.CONTENT_KEYS) {
      const ring = new Map<number, Promise<CryptoKey>>()
      for (const [v, hex] of Object.entries(JSON.parse(env.CONTENT_KEYS || '{}') as Record<string, string>)) {
        const bytes = new Uint8Array((hex.match(/../g) ?? []).map((b) => Number.parseInt(b, 16)))
        ring.set(
          Number(v),
          crypto.subtle.importKey('raw', bytes, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']),
        )
      }
      secrets = { raw: env.CONTENT_KEYS, ring }
    }
    return secrets.ring.get(version)
  }

  // authorize returns the token's audience, or a refusal (D015's order).
  async function authorize(env: Env, r: Route, pid: string, raw: string): Promise<string | Response> {
    const tok = parse(raw)
    if (!tok) return denied()
    const ks = await signingKeys(env)
    const key = ks?.keys.find((k) => k.kid === tok.kid)
    const t = now()
    if (!ks || !key || (key.not_after !== null && key.not_after <= t)) return denied()
    // An app's key signs only its own IDs; the internal key signs for any.
    if (key.app_id !== null && key.app_id !== r.app_id) return denied()
    if (!(await verify(tok, key.jwk))) return denied()
    const c = tok.claims
    if (
      c.sub !== pid ||
      (c.aud !== 'v' && c.aud !== 't') ||
      c.iat <= 0 ||
      c.iat > t + CLOCK_SKEW ||
      c.exp - c.iat > ks.max_token_lifetime_seconds
    ) {
      return denied()
    }
    if (c.exp <= t) return expired()
    return c.aud
  }

  async function contentKey(env: Env, r: Route, outputID: string, version: string): Promise<Response> {
    const v = Number(version)
    const k = /^[0-9]+$/.test(version) ? secret(env, v) : undefined
    if (!k || r.outputs[outputID] !== v) return notFound()
    const mac = await crypto.subtle.sign(
      'HMAC',
      await k,
      new TextEncoder().encode(`clipmux-content-key-v1\n${r.asset_id}\n${outputID}`),
    )
    const h = common(new Headers({ 'Content-Type': 'application/octet-stream', 'Cache-Control': 'no-store' }))
    return new Response(new Uint8Array(mac).slice(0, 16), { headers: h })
  }

  async function master(env: Env, r: Route, req: Request): Promise<Response> {
    if (r.backend === 'origin') {
      return origin(env, req, null)
    }
    const obj = await env.MEDIA.get(`media/${r.asset_id}/${r.output_id}/master.m3u8`)
    if (!obj) return error(500, 'internal', 'internal error')
    const text = await obj.text()
    const h = common(
      new Headers({ 'Content-Type': CONTENT_TYPES['.m3u8'] ?? '', 'Cache-Control': 'no-store' }),
    )
    const subs = Object.entries(r.tracks ?? {})
      .filter(([, t]) => t.listed)
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
      .map(([id, t]) => ({ id, name: t.name, language: t.language }))
    return new Response(
      req.method === 'HEAD' ? null : masterWithSubtitles(rewriteMaster(text, r.output_id), subs),
      {
        headers: h,
      },
    )
  }

  // track serves tracks/{id}.m3u8 (subtitles only) or tracks/{id}.vtt:
  // the edge caches the sealed bytes, and they are decrypted after it, so
  // no plaintext sits in the cache (delivery.track).
  async function track(env: Env, req: Request, r: Route, rel: string): Promise<Response> {
    const m = TRACK_PATH.exec(rel)
    const t = m ? r.tracks?.[m[1] as string] : undefined
    if (!m || !t) return notFound()
    const id = m[1] as string
    if (m[2] === 'm3u8') {
      if (t.kind !== 'subtitles') return notFound()
      const h = common(
        new Headers({ 'Content-Type': CONTENT_TYPES['.m3u8'] ?? '', 'Cache-Control': 'no-store' }),
      )
      return new Response(req.method === 'HEAD' ? null : subtitlePlaylist(id, t.duration_ms), { headers: h })
    }
    const scope = r.policy === 'signed' ? 'private' : 'public'
    if (r.backend === 'origin') {
      // The origin decrypts; nothing it returns goes in the edge cache.
      return finish(await origin(env, req, null), 'text/vtt; charset=utf-8', `${scope}, ${IMMUTABLE}`, req)
    }
    const key = `media/${r.asset_id}/tracks/${id}.vtt`
    const edge = cacheFor()
    let sealed: Uint8Array | undefined
    const hit = edge ? await edge.match(new Request(cacheURL(key))) : undefined
    if (hit) sealed = new Uint8Array(await hit.arrayBuffer())
    if (!sealed) {
      const obj = await env.MEDIA.get(key)
      if (!obj) return notFound()
      sealed = new Uint8Array(await new Response(obj.body).arrayBuffer())
      if (edge)
        await remember(
          key,
          new Response(sealed.slice()),
          new Headers({ 'Content-Type': 'application/octet-stream' }),
        )
    }
    const plain = await openTrack(env, r.asset_id, id, key, sealed)
    if (!plain) return error(500, 'internal', 'internal error')
    const h = common(
      new Headers({ 'Content-Type': 'text/vtt; charset=utf-8', 'Cache-Control': `${scope}, ${IMMUTABLE}` }),
    )
    h.set('Content-Length', String(plain.length))
    return new Response(req.method === 'HEAD' ? null : plain, { headers: h })
  }

  // openTrack reverses contentkey.SealTrack: 0x01 | version u32 | 12-byte
  // nonce | AES-128-GCM ciphertext and tag, the storage key as associated
  // data, under HMAC-SHA256(secret, "clipmux-track-key-v1\n" + asset +
  // "\n" + track)[:16].
  async function openTrack(
    env: Env,
    assetID: string,
    trackID: string,
    key: string,
    sealed: Uint8Array,
  ): Promise<Uint8Array | null> {
    if (sealed.length < 17 + 16 || sealed[0] !== 1) return null
    const version = new DataView(sealed.buffer, sealed.byteOffset, sealed.byteLength).getUint32(1)
    const s = secret(env, version)
    if (!s) return null
    const enc = new TextEncoder()
    const mac = await crypto.subtle.sign(
      'HMAC',
      await s,
      enc.encode(`clipmux-track-key-v1\n${assetID}\n${trackID}`),
    )
    const aes = await crypto.subtle.importKey('raw', new Uint8Array(mac).slice(0, 16), 'AES-GCM', false, [
      'decrypt',
    ])
    try {
      const plain = await crypto.subtle.decrypt(
        { name: 'AES-GCM', iv: sealed.slice(5, 17), additionalData: enc.encode(key) },
        aes,
        sealed.slice(17),
      )
      return new Uint8Array(plain)
    } catch {
      return null
    }
  }

  // origin passes a request that passed every check here to the built-in
  // handler, which checks it again and serves from the asset's storage.
  async function origin(env: Env, req: Request, cacheKey: string | null): Promise<Response> {
    if (!env.ORIGIN_URL) return error(500, 'internal', 'no origin')
    const u = new URL(req.url)
    const upstream = new Request(env.ORIGIN_URL.replace(/\/$/, '') + u.pathname, {
      method: req.method,
      headers: req.headers.has('Range') ? { Range: req.headers.get('Range') ?? '' } : {},
    })
    const res = await (opts.fetchOrigin ?? fetch)(upstream)
    const h = common(new Headers(res.headers))
    if (cacheKey && res.status === 200) await remember(cacheKey, res.clone(), h)
    return new Response(res.body, { status: res.status, headers: h })
  }

  const cacheFor = (): Cache | null =>
    opts.cache !== undefined ? opts.cache : typeof caches !== 'undefined' ? caches.default : null
  const cacheURL = (key: string) => `https://edge-cache.clipmux.invalid/${key}`

  async function remember(key: string, res: Response, h: Headers): Promise<void> {
    const cache = cacheFor()
    if (!cache) return
    // Stored public and immutable; what a viewer gets says private or not.
    const stored = new Headers(h)
    stored.set('Cache-Control', `public, ${IMMUTABLE}`)
    await cache.put(cacheURL(key), new Response(res.body, { status: 200, headers: stored }))
  }

  async function object(
    env: Env,
    req: Request,
    r: Route,
    outputID: string,
    rel: string,
    cache: string,
  ): Promise<Response> {
    const ctype = CONTENT_TYPES[ext(rel)]
    if (!ctype || rel === 'manifest.json' || !OUTPUT_PATH.test(rel)) return notFound()
    const key = `media/${r.asset_id}/${outputID}/${rel}`
    const scope = r.policy === 'signed' ? 'private' : 'public'
    const pinned = cache === IMMUTABLE
    const edge = pinned ? cacheFor() : null
    if (edge) {
      const hit = await edge.match(new Request(cacheURL(key), { headers: rangeOnly(req) }))
      if (hit) return finish(hit, ctype, `${scope}, ${cache}`, req)
    }
    if (r.backend === 'origin') {
      const res = await origin(env, req, pinned ? key : null)
      return finish(res, ctype, `${scope}, ${cache}`, req)
    }
    const range = req.headers.get('Range')
    if (range) return ranged(env, req, key, range, ctype, `${scope}, ${cache}`)
    const obj = await env.MEDIA.get(key)
    if (!obj) return notFound()
    const h = common(new Headers({ 'Content-Type': ctype, 'Cache-Control': `${scope}, ${cache}` }))
    h.set('Content-Length', String(obj.size))
    h.set('Accept-Ranges', 'bytes')
    if (edge && req.method === 'GET') {
      const [a, b] = obj.body.tee()
      await remember(key, new Response(a), h)
      return new Response(b, { headers: h })
    }
    return new Response(req.method === 'HEAD' ? null : obj.body, { headers: h })
  }

  // ranged serves one byte range, as Go's http.ServeContent does for a
  // single range; several ranges get the whole object.
  async function ranged(
    env: Env,
    req: Request,
    key: string,
    header: string,
    ctype: string,
    cache: string,
  ): Promise<Response> {
    const head = await env.MEDIA.head(key)
    if (!head) return notFound()
    const size = head.size
    const h = common(new Headers({ 'Content-Type': ctype, 'Cache-Control': cache, 'Accept-Ranges': 'bytes' }))
    const r = resolveRange(header, size)
    if (r === 'unsatisfiable') {
      h.set('Content-Range', `bytes */${size}`)
      return new Response(null, { status: 416, headers: h })
    }
    const obj = await env.MEDIA.get(key, r ? { range: { offset: r.start, length: r.end - r.start + 1 } } : {})
    if (!obj) return notFound()
    if (!r) {
      h.set('Content-Length', String(size))
      return new Response(req.method === 'HEAD' ? null : obj.body, { headers: h })
    }
    h.set('Content-Range', `bytes ${r.start}-${r.end}/${size}`)
    h.set('Content-Length', String(r.end - r.start + 1))
    return new Response(req.method === 'HEAD' ? null : obj.body, { status: 206, headers: h })
  }

  async function serve(req: Request, env: Env): Promise<Response> {
    const url = new URL(req.url)
    if (req.method === 'OPTIONS' && /^\/[vp]\//.test(url.pathname)) {
      const h = common(new Headers())
      h.set('Access-Control-Allow-Methods', 'GET, HEAD')
      h.set('Access-Control-Allow-Headers', 'Range')
      h.set('Access-Control-Max-Age', '86400')
      return new Response(null, { status: 204, headers: h })
    }
    if (req.method !== 'GET' && req.method !== 'HEAD') return notFound()
    const m = /^\/(?:v\/([^/]+)|(p))\/([^/]+)\/(.+)$/.exec(url.pathname)
    if (!m) return notFound()
    const [, token, , pid, path] = m as unknown as [
      string,
      string | undefined,
      string | undefined,
      string,
      string,
    ]
    const r = await route(env, pid)
    if (!r) return notFound()
    if (r.denied) return denied()

    let audience = 'v'
    if (r.policy === 'signed') {
      if (token === undefined) return denied()
      const a = await authorize(env, r, pid, token)
      if (a instanceof Response) return a
      audience = a
    }

    if (path === 'master.m3u8' && audience === 'v') return master(env, r, req)
    if (path === 'poster.jpg') return object(env, req, r, r.output_id, 'poster.jpg', 'max-age=300')
    // Before the output lookup, which would take "tracks" for an output. A
    // thumbnail token never reads a track.
    if (path.startsWith('tracks/')) return audience === 'v' ? track(env, req, r, path.slice(7)) : denied()
    if (audience !== 'v' && !path.endsWith('/poster.jpg')) return denied()
    const slash = path.indexOf('/')
    if (slash < 0) return notFound()
    const outputID = path.slice(0, slash)
    const rest = path.slice(slash + 1)
    // Only this asset's committed or retired outputs (never a writing one).
    if (!(outputID in r.outputs)) return notFound()
    if (rest.startsWith('key/')) return contentKey(env, r, outputID, rest.slice(4))
    if (audience !== 'v' && rest !== 'poster.jpg') return denied()
    return object(env, req, r, outputID, rest, IMMUTABLE)
  }

  return {
    async fetch(req: Request, env: Env): Promise<Response> {
      try {
        return await serve(req, env)
      } catch (err) {
        console.error('delivery', err instanceof Error ? err.message : String(err))
        return error(500, 'internal', 'internal error')
      }
    },
  }
}

// rewriteMaster prefixes every URI with outputID/ (delivery.RewriteMaster).
export function rewriteMaster(master: string, outputID: string): string {
  let out = ''
  for (const raw of master.split(/(?<=\n)/)) {
    let line = raw.replace(/[\r\n]+$/, '')
    if (line.startsWith('#')) line = line.replaceAll('URI="', `URI="${outputID}/`)
    else if (line !== '') line = `${outputID}/${line}`
    out += `${line}\n`
  }
  return out
}

// masterWithSubtitles adds one EXT-X-MEDIA per listed subtitles track (in ID
// order) before the first variant, and SUBTITLES="subs" to every variant
// (delivery.Master).
export function masterWithSubtitles(
  master: string,
  subs: { id: string; name: string; language: string }[],
): string {
  if (subs.length === 0) return master
  const attr = (s: string) => s.replace(/["\r\n]/g, '')
  const media = subs
    .map(
      (s) =>
        `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="${attr(s.name)}",LANGUAGE="${attr(s.language)}",AUTOSELECT=YES,DEFAULT=NO,URI="tracks/${s.id}.m3u8"\n`,
    )
    .join('')
  let out = ''
  let inserted = false
  for (const line of master.split(/(?<=\n)/)) {
    if (line.startsWith('#EXT-X-STREAM-INF:')) {
      if (!inserted) {
        out += media
        inserted = true
      }
      out += `${line.replace(/\n$/, '')},SUBTITLES="subs"\n`
      continue
    }
    out += line
  }
  return inserted ? out : out + media
}

// subtitlePlaylist is a subtitles track's playlist: its whole VTT as one
// segment, times from integer milliseconds (delivery.SubtitlePlaylist).
export function subtitlePlaylist(trackID: string, durationMS: number): string {
  const target = Math.max(Math.ceil(durationMS / 1000), 1)
  const extinf = `${Math.floor(durationMS / 1000)}.${String(durationMS % 1000).padStart(3, '0')}`
  return `#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:${target}\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:${extinf},\n${trackID}.vtt\n#EXT-X-ENDLIST\n`
}

function rangeOnly(req: Request): HeadersInit {
  const r = req.headers.get('Range')
  return r ? { Range: r } : {}
}

function finish(res: Response, ctype: string, cache: string, req: Request): Response {
  const h = common(new Headers(res.headers))
  h.set('Content-Type', ctype)
  h.set('Cache-Control', cache)
  return new Response(req.method === 'HEAD' ? null : res.body, { status: res.status, headers: h })
}

// resolveRange reads one "bytes=" range; null means serve it whole.
export function resolveRange(
  header: string,
  size: number,
): { start: number; end: number } | null | 'unsatisfiable' {
  const m = /^bytes=(\d*)-(\d*)$/.exec(header.trim())
  if (!m) return null
  const [, a = '', b = ''] = m
  if (a === '' && b === '') return null
  if (a === '') {
    const n = Number(b)
    if (n === 0) return 'unsatisfiable'
    return { start: Math.max(0, size - n), end: size - 1 }
  }
  const start = Number(a)
  if (start >= size) return 'unsatisfiable'
  const end = b === '' ? size - 1 : Math.min(Number(b), size - 1)
  if (end < start) return null
  return { start, end }
}
