// The Worker's request logic (src/delivery.ts) behind a Node HTTP server,
// with its MEDIA binding reading an S3-compatible bucket: the Phase 2 exit
// test (internal/e2e) puts it in front of serve. workerd's local R2 can't
// share a bucket with serve's S3 writes, so this stands in for the R2
// binding; the bundle itself runs in workerd in src/workerd.test.ts.
//
//   HARNESS_PORT S3_ENDPOINT S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY
//   CONTENT_KEYS ORIGIN_URL     node worker/e2e/harness.ts

import { createServer } from 'node:http'
import { createWorker, type Env } from '../src/delivery.ts'

const env = process.env
const endpoint = new URL(env.S3_ENDPOINT ?? '')
const bucket = env.S3_BUCKET ?? ''
const enc = new TextEncoder()

const hex = (b: ArrayBuffer) => Buffer.from(b).toString('hex')
const sha256 = async (s: string) => hex(await crypto.subtle.digest('SHA-256', enc.encode(s)))
async function hmac(key: ArrayBuffer | Uint8Array, msg: string): Promise<ArrayBuffer> {
  const k = await crypto.subtle.importKey('raw', key, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign'])
  return crypto.subtle.sign('HMAC', k, enc.encode(msg))
}

// s3 sends a SigV4-signed GET or HEAD for one object, path-style.
async function s3(method: string, key: string, range?: string): Promise<Response> {
  const url = new URL(`/${bucket}/${key}`, endpoint)
  const now = new Date().toISOString().replace(/[-:]|\.\d{3}/g, '')
  const day = now.slice(0, 8)
  const payload = 'UNSIGNED-PAYLOAD'
  const headers: Record<string, string> = {
    host: url.host,
    'x-amz-content-sha256': payload,
    'x-amz-date': now,
  }
  const signed = Object.keys(headers).sort()
  const canonical = [
    method,
    url.pathname,
    '',
    ...signed.map((h) => `${h}:${headers[h]}`),
    '',
    signed.join(';'),
    payload,
  ].join('\n')
  const scope = `${day}/us-east-1/s3/aws4_request`
  const toSign = ['AWS4-HMAC-SHA256', now, scope, await sha256(canonical)].join('\n')
  let k = await hmac(enc.encode(`AWS4${env.S3_SECRET_KEY}`), day)
  for (const part of ['us-east-1', 's3', 'aws4_request']) k = await hmac(k, part)
  const sig = hex(await hmac(k, toSign))
  const auth = `AWS4-HMAC-SHA256 Credential=${env.S3_ACCESS_KEY}/${scope}, SignedHeaders=${signed.join(';')}, Signature=${sig}`
  const h = new Headers({ ...headers, authorization: auth })
  h.delete('host')
  if (range) h.set('range', range)
  return fetch(url, { method, headers: h })
}

const media = {
  async get(key: string, opts?: { range?: { offset: number; length: number } }) {
    const r = opts?.range
    const res = await s3('GET', key, r ? `bytes=${r.offset}-${r.offset + r.length - 1}` : undefined)
    if (res.status === 404) return null
    if (!res.ok) throw new Error(`s3 get ${key}: HTTP ${res.status}`)
    const buf = new Uint8Array(await res.arrayBuffer())
    return {
      size: buf.length,
      body: new Response(buf).body,
      text: async () => new TextDecoder().decode(buf),
      json: async () => JSON.parse(new TextDecoder().decode(buf)),
    }
  },
  async head(key: string) {
    const res = await s3('HEAD', key)
    if (res.status === 404) return null
    if (!res.ok) throw new Error(`s3 head ${key}: HTTP ${res.status}`)
    return { size: Number(res.headers.get('content-length')) }
  },
}

const workerEnv = {
  MEDIA: media as unknown as R2Bucket,
  CONTENT_KEYS: env.CONTENT_KEYS ?? '{}',
  ORIGIN_URL: env.ORIGIN_URL,
} as Env
const worker = createWorker({ cache: null })

const server = createServer(async (req, res) => {
  const headers = new Headers()
  for (const [k, v] of Object.entries(req.headers)) if (typeof v === 'string') headers.set(k, v)
  const out = await worker.fetch(
    new Request(`http://${req.headers.host}${req.url}`, { method: req.method ?? 'GET', headers }),
    workerEnv,
  )
  res.writeHead(out.status, Object.fromEntries(out.headers))
  res.end(Buffer.from(await out.arrayBuffer()))
})
server.listen(Number(env.HARNESS_PORT), '127.0.0.1', () => console.log(`listening ${env.HARNESS_PORT}`))
