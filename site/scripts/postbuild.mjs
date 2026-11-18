// After `next build`: put the Markdown twins and per-tab llms-full.txt files
// where their URLs say (out/raw/... -> out/docs/...), and copy the OpenAPI
// spec the server embeds to /openapi.json.
import { cp, rename, rm } from 'node:fs/promises'
import { join } from 'node:path'

const out = new URL('../out/', import.meta.url).pathname
const raw = join(out, 'raw')

await rename(join(raw, 'index.md'), join(out, 'docs.md'))
await cp(raw, join(out, 'docs'), { recursive: true })
await rm(raw, { recursive: true })
await cp(new URL('../../internal/openapi/openapi.json', import.meta.url).pathname, join(out, 'openapi.json'))
console.log('postbuild: Markdown twins under /docs, /openapi.json copied')
