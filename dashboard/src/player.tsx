import '@clipmux/player/element'
import { useEffect, useRef } from 'react'
import { post } from './api.ts'

/** Plays an asset with a preview token signed by the server's internal key. */
export function Preview({
  assetId,
  baseUrl,
  autoplay,
}: {
  assetId: string
  baseUrl: string
  autoplay?: boolean
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
  return (
    <clipmux-player
      ref={ref}
      muted
      autoplay={autoplay}
      className="block overflow-hidden rounded-lg bg-black"
    />
  )
}

declare module 'react' {
  // biome-ignore lint/style/noNamespace: JSX augmentation
  namespace JSX {
    interface IntrinsicElements {
      'clipmux-player': React.DetailedHTMLProps<React.HTMLAttributes<HTMLElement>, HTMLElement> & {
        muted?: boolean
        autoplay?: boolean | undefined
      }
    }
  }
}
