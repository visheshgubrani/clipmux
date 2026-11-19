// The one route file: POST /api/clipmux/upload, /playback and /webhook.
// clipmuxHandler reads CLIPMUX_* from the environment (.env.local, written
// by `npx @clipmux/node init`), and every route is default-deny: a callback
// that returns null answers 403.
import { ClipMux } from '@clipmux/node'
import { clipmuxHandler } from '@clipmux/node/handlers'
import { getLesson, getLessonByPlaybackId, passthroughFor, updateFromAsset } from '@/lib/lessons'
import { canTeach, canWatch, userFromRequest } from '@/lib/users'

const clipmux = new ClipMux()

export const POST = clipmuxHandler({
  clipmux,
  // <clipmux-uploader create-url="/api/clipmux/upload?lesson=1">: only the
  // instructor uploads, and only to a lesson that exists.
  async upload(req, { file }) {
    if (!canTeach(userFromRequest(req))) return null
    const lesson = await getLesson(new URL(req.url).searchParams.get('lesson') ?? '')
    if (!lesson) return null
    // Add renditions: ['1080p', '480p'] to encode only those, or
    // generate: { subtitles: true } for captions.
    return { passthrough: passthroughFor(lesson), title: lesson.title || file.name }
  },

  // <clipmux-player session-url="/api/clipmux/playback">: enrolled viewers
  // get a signed session for a lesson's video; anyone else is refused, and
  // the player fires clipmux-denied.
  async playback(req, { playbackId }) {
    const user = userFromRequest(req)
    const lesson = await getLessonByPlaybackId(playbackId)
    if (!user || !canWatch(user) || !lesson?.assetId) return null
    return {
      viewerId: user.id,
      // With the asset, the session carries its captions and duration.
      asset: await clipmux.assets.get(lesson.assetId),
      expiresIn: process.env.LMS_TOKEN_TTL ?? '15m',
    }
  },

  // A verified webhook (or `npx @clipmux/node listen`), with the asset
  // re-fetched so it's current. Throwing makes ClipMux deliver it again.
  async asset(asset) {
    await updateFromAsset(asset.passthrough, {
      status: asset.status,
      assetId: asset.id,
      playbackId: asset.playback_ids.find((p) => p.status === 'active')?.id,
    })
  },
})
