import { GlobeIcon } from 'lucide-react'
import * as React from 'react'
import type { Asset, LibraryAsset } from '@/api'
import { timecode } from '@/lib/format'
import { cn } from '@/lib/utils'

export const isPublic = (a: Pick<Asset, 'playback_ids'>) =>
  a.playback_ids.some((p) => p.policy === 'public' && p.status === 'active')

const encoding = (a: Pick<Asset, 'status'>) => a.status === 'processing' || a.status === 'waiting_upload'

/**
 * The poster, the design's one memorable element (docs/design.md): a frame
 * on black with burned-in timecode, a tally lamp only when the video isn't
 * ready, a globe only when it's public, and a playhead moving along the
 * bottom while it encodes.
 */
export function Poster({ asset, className }: { asset: LibraryAsset | Asset; className?: string }) {
  const [failed, setFailed] = React.useState(false)
  const src = 'poster_url' in asset && asset.poster_url && !failed ? asset.poster_url : null
  const tc = timecode(asset.duration_seconds)
  // A cancelled upload is someone's choice, not a failure (D135): a grey lamp.
  const lamp =
    asset.status === 'errored'
      ? asset.error_code === 'upload_cancelled'
        ? 'cancelled'
        : 'failed'
      : encoding(asset)
        ? 'encoding'
        : null
  return (
    <div className={cn('relative aspect-video overflow-hidden rounded-md bg-poster', className)}>
      {src && (
        <img
          src={src}
          alt=""
          loading="lazy"
          decoding="async"
          onError={() => setFailed(true)}
          className="absolute inset-0 size-full object-cover"
        />
      )}
      {lamp && (
        <span className="absolute top-2 left-2 flex items-center gap-1.5 rounded-sm bg-black/70 px-1.5 py-0.5 text-[11px] font-medium text-white">
          <span
            aria-hidden="true"
            className={cn(
              'size-1.5 rounded-[1px]',
              lamp === 'failed' ? 'bg-[#f87171]' : lamp === 'cancelled' ? 'bg-white/50' : 'bg-[#a78bfa]',
            )}
          />
          {lamp === 'failed'
            ? 'Failed'
            : lamp === 'cancelled'
              ? 'Cancelled'
              : asset.status === 'waiting_upload'
                ? 'Uploading'
                : 'Encoding'}
        </span>
      )}
      {isPublic(asset) && (
        <span title="Public" className="absolute top-2 right-2 rounded-sm bg-black/70 p-1 text-white">
          <GlobeIcon aria-label="Public" className="size-3" />
        </span>
      )}
      {tc && (
        <span className="type-timecode absolute bottom-2 left-2 rounded-[3px] bg-black/72 px-1.5 py-px text-[11px] font-medium text-white">
          {tc}
        </span>
      )}
      {lamp === 'encoding' && (
        <span aria-hidden="true" className="absolute inset-x-0 bottom-0 h-0.5 overflow-hidden bg-white/10">
          <span className="animate-playhead block h-full w-1/4 bg-[#a78bfa]" />
        </span>
      )}
    </div>
  )
}
