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
