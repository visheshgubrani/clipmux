import type { AssetStatus, Job, Track } from '@/api'
import { Badge } from '@/components/ui/badge'

// Status is never colour alone: each badge carries its word.

const assetWords: Record<AssetStatus, [string, 'success' | 'tally' | 'destructive' | 'secondary']> = {
  ready: ['Ready', 'success'],
  processing: ['Encoding', 'tally'],
  waiting_upload: ['Uploading', 'tally'],
  errored: ['Failed', 'destructive'],
  deleted: ['Deleted', 'secondary'],
}

export function AssetStatusBadge({
  status,
  errorCode,
}: {
  status: AssetStatus
  errorCode?: string | undefined
}) {
  // A cancelled upload is someone's choice, not a failure (D135).
  const [word, variant] =
    status === 'errored' && errorCode === 'upload_cancelled'
      ? (['Cancelled', 'secondary'] as const)
      : (assetWords[status] ?? [status, 'secondary'])
  return <Badge variant={variant}>{word}</Badge>
}

const jobWords: Record<Job['status'], [string, 'success' | 'tally' | 'destructive' | 'secondary']> = {
  queued: ['Queued', 'secondary'],
  running: ['Running', 'tally'],
  succeeded: ['Done', 'success'],
  failed: ['Failed', 'destructive'],
  cancelled: ['Cancelled', 'secondary'],
}

export function JobStatusBadge({ status }: { status: Job['status'] }) {
  const [word, variant] = jobWords[status] ?? [status, 'secondary']
  return <Badge variant={variant}>{word}</Badge>
}

const trackWords: Record<Track['status'], [string, 'success' | 'tally' | 'destructive']> = {
  ready: ['Ready', 'success'],
  preparing: ['Preparing', 'tally'],
  errored: ['Failed', 'destructive'],
}

export function TrackStatusBadge({ status }: { status: Track['status'] }) {
  const [word, variant] = trackWords[status]
  return <Badge variant={variant}>{word}</Badge>
}

/** A small round status dot with its word, for machines and connections. */
export function Presence({ on, label }: { on: boolean; label: string }) {
  return (
    <span className="inline-flex items-center gap-1.5 text-sm">
      <span
        aria-hidden="true"
        className={on ? 'size-2 rounded-full bg-success' : 'size-2 rounded-full bg-muted-foreground/40'}
      />
      {label}
    </span>
  )
}

/**
 * What a failed video's error code means to its owner, and whether a
 * re-encode can help: not when there is no source, or ClipMux can't read
 * it (bug hunt F18).
 */
export function failedVideo(code: string | undefined): { say: string; reencode: boolean } {
  switch (code) {
    case 'upload_cancelled':
      return { say: 'The upload was cancelled.', reencode: false }
    case 'upload_expired':
      return { say: 'The upload stopped before it finished, and expired after a day.', reencode: false }
    case 'import_failed':
    case 'import_too_large':
    case 'import_url_refused':
      return { say: `The import failed (${code}), so there is no source to encode.`, reencode: false }
    case 'source_missing':
      return { say: "The source file is gone, so the video can't be encoded again.", reencode: false }
    case 'unsupported_source':
      return { say: "ClipMux can't read this file as a video.", reencode: false }
    default:
      return { say: `The encode failed${code ? ` (${code})` : ''}. Re-encode to try again.`, reencode: true }
  }
}
