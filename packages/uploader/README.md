# @clipmux/uploader

Uploads a file to ClipMux in parts, straight to storage, resuming from what
storage already holds. No dependencies.

```ts
import { ClipMuxUpload, rememberUpload } from '@clipmux/uploader'

// Your backend called POST /v1/uploads with its API key and returned the body.
const memory = rememberUpload(localStorage)
const upload = memory.load(file) ?? (await createUploadOnMyBackend(file.size))
memory.save(file, upload)

const asset = await new ClipMuxUpload(file, {
  endpoint: 'https://video.example.com',
  upload,
  onProgress: (p) => console.log(p.bytesUploaded / p.bytesTotal),
}).start()
memory.forget(file)
```

- Parts are the upload's `part_size` (32 MiB); up to four are in flight.
- After a reload, `start()` asks ClipMux which parts are stored and sends the rest.
- Network errors, 5xx, 408 and 429 are retried (Retry-After honoured, offline waits for `online`);
  a failure rejects with a `ClipMuxUploadError` (`code`, `status`, `retryable`) and leaves the upload
  resumable for 24 hours. `cancel()` ends it for good.
- Storage must expose the `ETag` header to the page (ClipMux's own storage does).

Uppy users can map its `@uppy/aws-s3` callbacks to the same endpoints instead (docs/api.md).

## Your backend: creating the upload and hearing when it's ready

The browser never holds an API key. Your backend creates the upload with one
(scope `uploads:write`) and hands the body to the page; `passthrough` is your
own reference, which comes back on the asset and its webhooks.

```ts
// POST /api/uploads, after your own access check
export async function createUpload(size: number, lessonId: string) {
  const res = await fetch(`${process.env.CLIPMUX_URL}/v1/uploads`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${process.env.CLIPMUX_API_KEY}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ size, playback_policy: 'signed', passthrough: lessonId }),
  })
  if (!res.ok) throw new Error(`create upload: ${res.status}`)
  return res.json() // {upload_id, asset_id, part_size, part_count, upload_token, playback_ids}
}
```

When the video is ready, ClipMux sends `video.asset.ready` to your webhook
endpoint. Verify the signature over the raw body before trusting it, then
re-fetch the asset (`GET /v1/assets/{id}`) rather than relying on the body:

```ts
import { createHmac, timingSafeEqual } from 'node:crypto'

export function verifyWebhook(raw: string, headers: Headers, secret: string): boolean {
  const ts = headers.get('X-Webhook-Timestamp') ?? ''
  const sig = headers.get('X-Webhook-Signature') ?? ''
  if (Math.abs(Date.now() / 1000 - Number(ts)) > 300) return false
  const want = `sha256=${createHmac('sha256', secret).update(`${ts}.${raw}`).digest('hex')}`
  return sig.length === want.length && timingSafeEqual(Buffer.from(sig), Buffer.from(want))
}
```

Delivery is at least once: dedupe on `X-Webhook-Id`. Every route and field is
in [docs/api.md](https://github.com/visheshgubrani/clipmux/blob/main/docs/api.md).

## Licence

MIT. The ClipMux server is AGPL-3.0; this package is MIT so apps embed it freely.
