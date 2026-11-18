import { clipmuxHandler } from '@clipmux/node/handlers'

export const POST = clipmuxHandler({
  // Someone wants to upload: say what the upload is, or return null to refuse.
  async upload(req, { file }) {
    await requireInstructor(req)
    const lesson = new URL(req.url).searchParams.get('lesson')
    return { passthrough: `lesson_${lesson}`, title: file.name, generate: { subtitles: true } }
  },
  // Someone wants to watch: say who they are, or return null to refuse.
  async playback(req, { playbackId }) {
    const user = await getUser(req)
    return (await canWatch(user, playbackId)) ? { viewerId: user.id } : null
  },
  // A verified webhook, with the video re-fetched so it's current.
  async asset(asset) {
    await db.lesson.update({
      where: { ref: asset.passthrough },
      data: { status: asset.status, playbackId: asset.playback_ids[0]?.id },
    })
  },
})
