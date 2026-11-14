# @clipmux/uploader

Uploads a file to ClipMux in parts, straight to storage, resuming from what
storage already holds. No dependencies.

```html
<script type="module">import '@clipmux/uploader/element'</script>
<clipmux-uploader create-url="/api/clipmux/upload" accept="video/*" max-size="5000000000"></clipmux-uploader>
```

```ts
document.querySelector('clipmux-uploader')!.addEventListener('clipmux-uploaded', (e) => {
  console.log(e.detail.asset.id) // processing; your webhook hears when it's ready
})
```

`<clipmux-uploader>` is a drop zone with a file button, progress, Cancel and
Retry. It POSTs `{"size", "name", "type"}` to `create-url` (with the page's
cookies), which is your backend's route that creates the upload with its API
key and answers with it plus `endpoint`, the ClipMux URL; the `@clipmux/node`
handler's `/upload` route does exactly that. The upload is remembered in
`localStorage` until it completes, so choosing the same file after a reload
resumes it.

- **Attributes:** `create-url`; `accept`, as on `<input type=file>`, checked
  for dropped files too; `max-size` in bytes. The host's `state` attribute is
  `idle`, `uploading`, `done` or `error`. `el.options` takes further
  `ClipMuxUpload` options (`concurrency`, `store`, …).
- **Events** (they bubble, typed): `clipmux-progress` (`Progress`),
  `clipmux-uploaded` (`{ asset }`), `clipmux-error` (`{ code, message, status, error }`;
  `file_too_large`, `file_type_rejected`, `create_denied` for a `401`/`403`
  from `create-url`, or a `ClipMuxUploadError` code).
- **Styling:** the parts `dropzone`, `label`, `button`, `progress`, `bar`,
  `status`, `cancel` and `retry` (`clipmux-uploader::part(bar) { … }`), the
  custom properties `--clipmux-uploader-accent`, `-border`, `-radius`,
  `-background`, `-color` and `-font`, and the default slot for the drop
  zone's text.
- **React 18 or 19:** `import type {} from '@clipmux/uploader/react'` types the
  element in JSX, including `onclipmux-uploaded`, with no React dependency.

The same without the element:

```ts
import { ClipMuxUpload } from '@clipmux/uploader'

const asset = await new ClipMuxUpload(file, { createUrl: '/api/clipmux/upload' }).start()
```

With your own backend route instead:

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
- Retries honour Retry-After, and offline waits for `online`. Part uploads
  retry network errors, 5xx, 408 and 429; the calls to ClipMux and to
  `create-url` retry network errors, 5xx and 429. A 4xx such as `413
  too_large` fails at once.
- A failure rejects with a `ClipMuxUploadError` (`code`, `status`,
  `retryable`) and leaves the upload resumable for 24 hours. `cancel()` ends
  it for good.
- Storage must expose the `ETag` header to the page (ClipMux's own storage does).
- With `createUrl`, a remembered upload that has expired or was cancelled is
  replaced by a new one; `store: null` turns the memory off.

Uppy users can map its `@uppy/aws-s3` callbacks to the same endpoints instead (docs/api.md).

## Your backend: creating the upload and hearing when it's ready

The browser never holds an API key. Your backend creates the upload with one
(scope `uploads:write`) and hands the body to the page; `passthrough` is your
own reference, which comes back on the asset and its webhooks.

With `@clipmux/node`, `uploads.create` returns exactly what `ClipMuxUpload`
takes, `endpoint` included:

```ts
const upload = await clipmux.uploads.create({ size, playback_policy: 'signed', passthrough: lessonId })
return Response.json(upload) // the page: new ClipMuxUpload(file, { upload }).start()
```

Or with `fetch`:

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

## Content-Security-Policy

A page with the uploader must allow, besides its own origin:
- `connect-src`: the ClipMux URL (signing and completing parts) and the
  storage bucket's origin (the part PUTs), which is the ClipMux URL itself
  when it stores files on its own disk;
- `style-src 'unsafe-inline'`: the element's styles.

`npx @clipmux/node doctor` prints the exact origins, from `GET /v1/app`
(`public_url` and `upload_origins`).

## Licence

MIT. The ClipMux server is AGPL-3.0; this package is MIT so apps embed it freely.
