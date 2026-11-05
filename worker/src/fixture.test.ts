// The vectors' bucket for Worker tests: route records and _keys.json as the
// Go publisher writes them (testdata/worker-bucket.json), plus every file.

import { readFileSync } from 'node:fs'
import type { Env } from './delivery.ts'

const testdata = new URL('../../internal/delivery/testdata/', import.meta.url)
export const vectors = JSON.parse(readFileSync(new URL('vectors.json', testdata), 'utf8'))
const bucket = JSON.parse(readFileSync(new URL('worker-bucket.json', testdata), 'utf8'))

// memoryBucket is the slice of R2Bucket the Worker uses.
export function memoryBucket(objects: Map<string, Uint8Array>) {
  const body = (b: Uint8Array) =>
    new ReadableStream({
      start(c) {
        c.enqueue(b)
        c.close()
      },
    })
  const obj = (b: Uint8Array) => ({
    size: b.length,
    body: body(b),
    text: async () => new TextDecoder().decode(b),
    json: async () => JSON.parse(new TextDecoder().decode(b)),
  })
  return {
    gets: [] as string[],
    async get(key: string, opts?: { range?: { offset: number; length: number } }) {
      this.gets.push(key)
      const b = objects.get(key)
      if (!b) return null
      const r = opts?.range
      return obj(r ? b.slice(r.offset, r.offset + r.length) : b)
    },
    async head(key: string) {
      const b = objects.get(key)
      return b ? { size: b.length } : null
    },
  }
}

export function vectorEnv(): { env: Env; objects: Map<string, Uint8Array> } {
  const enc = new TextEncoder()
  const objects = new Map<string, Uint8Array>()
  for (const [pid, r] of Object.entries(bucket.routes))
    objects.set(`_routes/${pid}.json`, enc.encode(JSON.stringify(r)))
  objects.set('_keys.json', enc.encode(JSON.stringify(bucket.keys)))
  for (const o of vectors.outputs) {
    for (const [name, body] of Object.entries(vectors.files)) {
      objects.set(`media/${o.asset_id}/${o.id}/${name}`, enc.encode(body as string))
    }
  }
  const env = {
    MEDIA: memoryBucket(objects) as unknown as R2Bucket,
    CONTENT_KEYS: JSON.stringify({ [vectors.content_key_version]: vectors.content_key_secret_hex }),
  }
  return { env, objects }
}
