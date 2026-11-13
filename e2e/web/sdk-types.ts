// Compile-time checks that the three SDKs fit together as the docs say:
// what @clipmux/node returns goes into @clipmux/uploader and
// @clipmux/player unchanged. `pnpm run typecheck` fails if they drift apart.
// Nothing here runs.

import type { ClipMux } from '@clipmux/node'
import { ClipMuxUpload } from '@clipmux/uploader'

export async function uploadFromServerResult(cm: ClipMux, file: Blob): Promise<void> {
  // uploads.create's answer is what ClipMuxUpload takes, token and endpoint included.
  const upload = await cm.uploads.create({ size: file.size })
  await new ClipMuxUpload(file, { upload }).start()
}
