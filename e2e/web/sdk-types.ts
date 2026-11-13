// Compile-time checks that the three SDKs fit together as the docs say:
// what @clipmux/node returns goes into @clipmux/uploader and
// @clipmux/player unchanged. `pnpm run typecheck` fails if they drift apart.
// Nothing here runs.

import type { Chapter as ApiChapter, ClipMux } from '@clipmux/node'
import type { PlayerChapter, PlaybackSession as PlayerSession } from '@clipmux/player'
import { ClipMuxUpload } from '@clipmux/uploader'

export async function uploadFromServerResult(cm: ClipMux, file: Blob): Promise<void> {
  // uploads.create's answer is what ClipMuxUpload takes, token and endpoint included.
  const upload = await cm.uploads.create({ size: file.size })
  await new ClipMuxUpload(file, { upload }).start()
}

// What playback.session() answers is what the player's session URL expects.
export async function sessionForPlayer(cm: ClipMux): Promise<PlayerSession> {
  return cm.playback.session('pid_x', { native: false })
}

// Chapters from the API (milliseconds) and the player (seconds) are distinct types.
export function chapterSeconds(c: ApiChapter): PlayerChapter {
  return { startTime: c.start_ms / 1000, endTime: c.end_ms / 1000, title: c.title }
}
