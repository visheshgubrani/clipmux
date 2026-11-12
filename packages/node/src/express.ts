// toNodeListener: runs a Fetch handler (clipmuxHandler) on Node's
// http.IncomingMessage / ServerResponse, so it mounts in Express, Fastify
// (via its raw request) or a bare http.createServer:
//
//   app.post('/api/clipmux/:action', toNodeListener(clipmuxHandler({ … })))
//
// The webhook needs the body exactly as it arrived. Mount the route before
// express.json(), or give it express.raw({ type: '*/*' }); a body another
// middleware already parsed into an object is refused with a clear error.

import type { IncomingMessage, ServerResponse } from 'node:http'

type NodeRequest = IncomingMessage & { body?: unknown; originalUrl?: string; protocol?: string }

async function rawBody(req: NodeRequest): Promise<Uint8Array> {
  if (req.body !== undefined && req.body !== null) {
    if (typeof req.body === 'string') return new TextEncoder().encode(req.body)
    if (req.body instanceof Uint8Array) return req.body
    if (typeof req.body === 'object' && Object.keys(req.body).length > 0) {
      throw new TypeError(
        '@clipmux/node: the request body was already parsed (express.json()?); mount the ClipMux route before it, or use express.raw({ type: "*/*" }) for it',
      )
    }
  }
  if (req.readableEnded) return new Uint8Array()
  const chunks: Uint8Array[] = []
  for await (const c of req)
    chunks.push(typeof c === 'string' ? new TextEncoder().encode(c) : (c as Uint8Array))
  return Buffer.concat(chunks)
}

/** Adapts a Fetch handler to Node's (req, res, next?) listener shape. */
export function toNodeListener(handler: (req: Request) => Promise<Response>) {
  return (req: NodeRequest, res: ServerResponse, next?: (err?: unknown) => void): void => {
    void (async () => {
      const headers = new Headers()
      for (const [k, v] of Object.entries(req.headers)) {
        if (Array.isArray(v)) for (const x of v) headers.append(k, x)
        else if (v !== undefined) headers.set(k, v)
      }
      const proto = req.protocol ?? ((req.socket as { encrypted?: boolean }).encrypted ? 'https' : 'http')
      const url = new URL(req.originalUrl ?? req.url ?? '/', `${proto}://${req.headers.host ?? 'localhost'}`)
      const method = req.method ?? 'GET'
      const body = method === 'GET' || method === 'HEAD' ? undefined : await rawBody(req)
      const response = await handler(
        new Request(url, { method, headers, ...(body ? { body: body as Uint8Array<ArrayBuffer> } : {}) }),
      )
      res.statusCode = response.status
      response.headers.forEach((v, k) => {
        res.setHeader(k, v)
      })
      res.end(Buffer.from(await response.arrayBuffer()))
    })().catch((err: unknown) => {
      if (next) next(err)
      else {
        res.statusCode = 500
        res.end()
      }
    })
  }
}
