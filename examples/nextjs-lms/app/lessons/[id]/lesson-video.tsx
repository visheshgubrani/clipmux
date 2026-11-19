'use client'
import type { ClipMuxPlayerElement } from '@clipmux/player/element' // types only, nothing loaded
import type {} from '@clipmux/player/react' // JSX types for <clipmux-player>
import type { ClipMuxUploaderElement } from '@clipmux/uploader/element'
import type {} from '@clipmux/uploader/react'
import { useRouter } from 'next/navigation'
import { useCallback, useEffect, useState } from 'react'

export function LessonVideo(props: {
  lessonId: string
  playbackId: string | undefined
  canUpload: boolean
  waiting: boolean
}) {
  const router = useRouter()
  const [denied, setDenied] = useState(false)
  const [uploaded, setUploaded] = useState(false)

  // The elements need the DOM (and the player Media Source): load them in
  // the browser only.
  useEffect(() => {
    void import('@clipmux/uploader/element')
    void import('@clipmux/player/element')
  }, [])

  // Listen through refs: React 19 attaches onclipmux-* props to an element
  // it creates in the browser, but not to one it hydrates from the server's
  // HTML. A ref callback runs whenever the element mounts, and its cleanup
  // when it goes.
  const player = useCallback((el: ClipMuxPlayerElement | null) => {
    const onDenied = () => setDenied(true)
    el?.addEventListener('clipmux-denied', onDenied)
    return () => el?.removeEventListener('clipmux-denied', onDenied)
  }, [])
  const uploader = useCallback((el: ClipMuxUploaderElement | null) => {
    const onUploaded = () => setUploaded(true)
    el?.addEventListener('clipmux-uploaded', onUploaded)
    return () => el?.removeEventListener('clipmux-uploaded', onUploaded)
  }, [])

  // While ClipMux encodes, re-render every few seconds: the webhook updates
  // the lesson on the server, and the player appears when it's ready.
  const polling = props.waiting || (uploaded && !props.playbackId)
  useEffect(() => {
    if (!polling) return
    const timer = setInterval(() => router.refresh(), 3000)
    return () => clearInterval(timer)
  }, [polling, router])

  return (
    <>
      {props.playbackId &&
        (denied ? (
          <p className="notice" role="alert">
            You aren't enrolled in this course. Sign in as an enrolled student to watch.
          </p>
        ) : (
          <clipmux-player ref={player} playback-id={props.playbackId} session-url="/api/clipmux/playback" />
        ))}
      {props.canUpload && (
        <section>
          <h2>{props.playbackId ? 'Replace the video' : 'Upload the video'}</h2>
          <clipmux-uploader
            ref={uploader}
            create-url={`/api/clipmux/upload?lesson=${encodeURIComponent(props.lessonId)}`}
            accept="video/*"
          />
        </section>
      )}
    </>
  )
}
