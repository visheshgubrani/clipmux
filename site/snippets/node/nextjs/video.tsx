'use client'
import type {} from '@clipmux/player/react' // JSX types only
import type {} from '@clipmux/uploader/react'
import { useEffect } from 'react'

export function LessonVideo({ lessonId, playbackId }: { lessonId: string; playbackId?: string }) {
  // The elements need the DOM, so load them in the browser only.
  useEffect(() => {
    import('@clipmux/uploader/element')
    import('@clipmux/player/element')
  }, [])
  return (
    <>
      <clipmux-uploader create-url={`/api/clipmux/upload?lesson=${lessonId}`} />
      {playbackId && <clipmux-player playback-id={playbackId} session-url="/api/clipmux/playback" />}
    </>
  )
}
