import '@clipmux/player/element'
import { useEffect, useRef } from 'react'
import { post } from './api.ts'

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
  const ref = useRef<HTMLElement & { getToken?: () => Promise<string | null>; token?: string }>(null)
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

declare module 'react' {
  namespace JSX {
    interface IntrinsicElements {
      'clipmux-player': React.DetailedHTMLProps<React.HTMLAttributes<HTMLElement>, HTMLElement> & {
        muted?: boolean | undefined
        autoplay?: boolean | undefined
      }
    }
  }
}
