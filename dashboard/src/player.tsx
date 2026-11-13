import '@clipmux/player/element'
import type { ClipMuxPlayerElement } from '@clipmux/player/element'
import type {} from '@clipmux/player/react'
import { useEffect, useRef, useState } from 'react'
import { post } from './api.ts'
import { Button } from './components/ui/button.tsx'

/** Plays an asset with a preview token signed by the server's internal key. */
export function Preview({
  assetId,
  baseUrl,
  autoplay,
  chaptersTrack,
}: {
  assetId: string
  baseUrl: string
  autoplay?: boolean
  /** A ready chapters track, marked on the time slider. */
  chaptersTrack?: string | undefined
}) {
  const ref = useRef<ClipMuxPlayerElement>(null)
  const [blocked, setBlocked] = useState(false)
  useEffect(() => {
    // A tab opened before the media URL moved (Cloudflare connected or
    // disconnected) keeps the old Content-Security-Policy, which blocks the
    // new media origin; the player can only say "something went wrong".
    let origin: string
    try {
      origin = new URL(baseUrl).origin
    } catch {
      return
    }
    const onViolation = (e: SecurityPolicyViolationEvent) => {
      if (e.blockedURI.startsWith(origin)) setBlocked(true)
    }
    document.addEventListener('securitypolicyviolation', onViolation)
    return () => document.removeEventListener('securitypolicyviolation', onViolation)
  }, [baseUrl])
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const preview = () => post<{ token: string; playback_id: string }>(`/v1/assets/${assetId}/preview-token`)
    void preview().then((first) => {
      el.token = first.token
      el.getToken = async () => (await preview()).token
      el.setAttribute('base-url', baseUrl)
      el.setAttribute('playback-id', first.playback_id)
    })
  }, [assetId, baseUrl])
  useEffect(() => {
    const el = ref.current
    if (!el) return
    if (chaptersTrack) el.setAttribute('chapters-track', chaptersTrack)
    else el.removeAttribute('chapters-track')
  }, [chaptersTrack])
  if (blocked) {
    return (
      <div className="flex aspect-video w-full flex-col items-center justify-center gap-3 rounded-lg bg-black p-6 text-center text-sm text-white/80">
        <p>This page was loaded before the media hostname changed, so the browser blocks the video.</p>
        <Button size="sm" variant="secondary" onClick={() => location.reload()}>
          Reload to play
        </Button>
      </div>
    )
  }
  return (
    <clipmux-player
      ref={ref}
      // Browsers only autoplay muted; a player the viewer starts has sound.
      muted={autoplay || undefined}
      autoplay={autoplay}
      className="block overflow-hidden rounded-lg bg-black"
    />
  )
}
