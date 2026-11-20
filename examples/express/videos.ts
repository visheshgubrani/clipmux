// The app's videos, in memory: swap this for your database. The ClipMux side
// needs only a status and a playback ID, kept by the passthrough you chose.
import type { Asset } from '@clipmux/node'

export interface Video {
  passthrough: string
  title: string
  status: Asset['status']
  playbackId?: string | undefined
}

const videos = new Map<string, Video>()

export function addVideo(title: string): Video {
  const video: Video = { passthrough: `video_${videos.size + 1}`, title, status: 'waiting_upload' }
  videos.set(video.passthrough, video)
  return video
}

export const allVideos = () => [...videos.values()]
export const videoByPlaybackId = (id: string) => allVideos().find((v) => v.playbackId === id)

export function updateVideo(asset: Asset): void {
  const video = asset.passthrough ? videos.get(asset.passthrough) : undefined
  if (!video) return
  video.status = asset.status
  video.playbackId = asset.playback_ids.find((p) => p.status === 'active')?.id
}
