import { CheckIcon } from 'lucide-react'
import { LADDER, type Rung } from '@/api'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { cn } from '@/lib/utils'

// Which renditions an upload or a re-encode encodes (D130). Every rung is
// on by default, which the request leaves out: ClipMux then encodes every
// rung up to the video's own resolution.

const rungLabel = (r: Rung) => (r === '2160p' ? '4K' : r)
const rungHeight = (r: Rung) => Number.parseInt(r, 10)

/** renditionsBody is what a request sends: nothing for the whole ladder. */
export function renditionsBody(value: readonly Rung[]): { renditions?: Rung[] } {
  return value.length === LADDER.length ? {} : { renditions: [...value] }
}

/** renditionsNote says what a choice encodes for a video whose short side is maxHeight. */
export function renditionsNote(value: readonly Rung[], maxHeight?: number): string {
  if (value.length === LADDER.length)
    return maxHeight
      ? `Every size up to the video's own ${maxHeight}p.`
      : "Every size up to the video's own resolution."
  const above = maxHeight ? value.filter((r) => rungHeight(r) > maxHeight) : []
  if (above.length === value.length)
    return `None of these fit a ${maxHeight}p video, so it gets its largest size that does.`
  if (above.length > 0)
    return `${above.map(rungLabel).join(' and ')} ${above.length > 1 ? 'are' : 'is'} above the video's ${maxHeight}p, so ${above.length > 1 ? 'they are' : 'it is'} skipped.`
  return 'Only these sizes, where the video has them.'
}

export function RenditionPicker({
  value,
  onChange,
  maxHeight,
  labelledBy,
}: {
  value: readonly Rung[]
  onChange: (v: Rung[]) => void
  /** The largest video's short side, when the browser could read it. */
  maxHeight?: number | undefined
  labelledBy: string
}) {
  return (
    <ToggleGroup
      multiple
      aria-labelledby={labelledBy}
      variant="outline"
      size="sm"
      value={[...value]}
      // At least one stays on: an empty choice would encode nothing.
      onValueChange={(v: string[]) => v.length > 0 && onChange(LADDER.filter((r) => v.includes(r)))}
      // A grid, not a row: six chips don't fit a phone's width.
      className="grid w-full grid-cols-3 sm:grid-cols-6"
    >
      {LADDER.map((r) => (
        <ToggleGroupItem
          key={r}
          value={r}
          aria-label={r === '2160p' ? '4K (2160p)' : r}
          // Above the video: skipped whether chosen or not.
          className={cn(
            'w-full',
            maxHeight && rungHeight(r) > maxHeight && 'text-muted-foreground line-through',
          )}
        >
          <CheckIcon aria-hidden="true" className="hidden group-aria-pressed/toggle:block" />
          {rungLabel(r)}
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
  )
}

/** videoShortSide reads a file's short side from its metadata, or undefined when the browser can't. */
export function videoShortSide(file: File): Promise<number | undefined> {
  return new Promise((resolve) => {
    const video = document.createElement('video')
    const url = URL.createObjectURL(file)
    let settled = false
    const done = (side?: number) => {
      if (settled) return
      settled = true
      URL.revokeObjectURL(url)
      video.removeAttribute('src')
      resolve(side)
    }
    video.preload = 'metadata'
    video.muted = true
    video.onloadedmetadata = () =>
      done(video.videoWidth && video.videoHeight ? Math.min(video.videoWidth, video.videoHeight) : undefined)
    video.onerror = () => done()
    setTimeout(done, 5000)
    video.src = url
  })
}
