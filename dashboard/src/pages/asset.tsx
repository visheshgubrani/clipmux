import { useCallback, useEffect, useState } from 'react'
import { useParams, useSearchParams } from 'react-router'
import { ApiError, type Asset, api, type Track } from '../api.ts'
import { Button } from '../components/ui/button.tsx'
import { Card, CardTitle, Muted } from '../components/ui/card.tsx'
import { Preview } from '../player.tsx'

interface Transcript {
  track_id: string
  language: string
  duration_ms: number
  text: string
  segments: { id: string; start_ms: number; end_ms: number; text: string }[]
}

interface TrackDetail extends Track {
  chapters?: { start_ms: number; end_ms: number; title: string }[]
}

// The asset's job in progress, as the Jobs page shows it.
interface ActiveJob {
  kind: string
  status: string
  phase?: string
  progress?: number
}

const jobLine = (j: ActiveJob) =>
  j.status === 'queued'
    ? `${j.kind} waiting for a worker`
    : `${j.phase ?? j.kind}${j.progress != null ? ` ${Math.round(j.progress * 100)}%` : ''}`

// Subtitles become part of the stream's master playlist; a player opened
// before they were ready needs a reload to list them.
const subtitlesKey = (a: Asset) =>
  a.tracks
    .filter((t) => t.kind === 'subtitles' && t.status === 'ready')
    .map((t) => t.id)
    .join(',')

const busy = (a: Asset) =>
  a.status === 'waiting_upload' || a.status === 'processing' || a.tracks.some((t) => t.status === 'preparing')

export function AssetPage() {
  const { id = '' } = useParams()
  const [params] = useSearchParams()
  const app = params.get('app') ?? undefined
  const [asset, setAsset] = useState<Asset | null>(null)
  const [job, setJob] = useState<ActiveJob | null>(null)
  // The subtitles the player was opened with (null: not opened yet).
  const [playerSubtitles, setPlayerSubtitles] = useState<string | null>(null)
  // A change made here polls again until its tracks settle.
  const [tick, setTick] = useState(0)
  const load = useCallback(async () => {
    const a = await api<Asset>(`/v1/assets/${id}`, app ? { app } : {})
    setAsset(a)
    if (a.status === 'processing' || a.tracks.some((t) => t.status === 'preparing')) {
      const jobs = await api<{ data: ActiveJob[] }>(
        `/v1/jobs?asset_id=${encodeURIComponent(id)}&limit=5`,
      ).catch(() => ({ data: [] }))
      setJob(jobs.data.find((j) => j.status === 'running' || j.status === 'queued') ?? null)
    } else {
      setJob(null)
    }
    return a
  }, [id, app])
  // biome-ignore lint/correctness/useExhaustiveDependencies: tick restarts the poll after a change
  useEffect(() => {
    let stop = false
    const poll = async () => {
      const a = await load()
      if (!stop && busy(a)) setTimeout(poll, 3000)
    }
    void poll()
    return () => {
      stop = true
    }
  }, [load, tick])
  if (!asset) return <Muted>Loading…</Muted>
  const chapters = asset.tracks.find((t) => t.kind === 'chapters' && t.status === 'ready')
  const subtitles = subtitlesKey(asset)
  if (asset.status === 'ready' && playerSubtitles === null) setPlayerSubtitles(subtitles)
  return (
    <>
      <Card className="grid gap-4">
        <CardTitle className="font-mono text-base">{asset.id}</CardTitle>
        <dl className="grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
          <dt className="text-zinc-500">Status</dt>
          <dd>
            {asset.status}
            {asset.error_code ? ` (${asset.error_code})` : ''}
            {job && asset.status === 'processing' && (
              <span className="text-zinc-500" data-job-progress>
                {' '}
                — {jobLine(job)}
              </span>
            )}
          </dd>
          <dt className="text-zinc-500">Version</dt>
          <dd>{asset.version}</dd>
          {asset.duration_seconds !== null && (
            <>
              <dt className="text-zinc-500">Duration</dt>
              <dd>{asset.duration_seconds.toFixed(1)} s</dd>
            </>
          )}
          <dt className="text-zinc-500">Playback IDs</dt>
          <dd className="font-mono text-xs">
            {asset.playback_ids.map((p) => `${p.id} (${p.policy}, ${p.status})`).join(', ')}
          </dd>
          <dt className="text-zinc-500">Base URL</dt>
          <dd className="font-mono text-xs">{asset.base_url}</dd>
        </dl>
        {asset.status === 'ready' && (
          <Preview
            key={playerSubtitles ?? ''}
            assetId={asset.id}
            baseUrl={asset.base_url}
            chaptersTrack={chapters?.id}
          />
        )}
        {asset.status === 'ready' && playerSubtitles !== null && subtitles !== playerSubtitles && (
          <div className="flex items-center gap-3 text-sm">
            <span>New subtitles are ready.</span>
            <Button variant="outline" size="sm" onClick={() => setPlayerSubtitles(subtitles)}>
              Reload the player
            </Button>
          </div>
        )}
      </Card>
      {asset.status !== 'deleted' && (
        <TracksCard asset={asset} app={app} job={job} onChange={() => setTick((t) => t + 1)} />
      )}
    </>
  )
}

const statusClass: Record<string, string> = {
  ready: 'text-green-700',
  preparing: 'text-amber-700',
  errored: 'text-red-700',
}

// TracksCard lists an asset's subtitles and chapters (docs/api.md,
// "Tracks"), with the transcript of ready subtitles, and makes new ones or
// deletes them through the integration API.
function TracksCard({
  asset,
  app,
  job,
  onChange,
}: {
  asset: Asset
  app?: string | undefined
  job: ActiveJob | null
  onChange: () => void
}) {
  const [error, setError] = useState('')
  const [open, setOpen] = useState<string | null>(null)
  const opts = app ? { app } : {}
  const act = async (fn: () => Promise<unknown>) => {
    setError('')
    try {
      await fn()
      onChange()
    } catch (err) {
      setError(err instanceof ApiError ? `${err.code}: ${err.message}` : String(err))
    }
  }
  const regenerate = (kind: 'subtitles' | 'chapters') =>
    act(() =>
      api(`/v1/assets/${asset.id}/tracks`, {
        method: 'POST',
        body: JSON.stringify({ kind }),
        ...opts,
      }),
    )
  const remove = (t: Track) => {
    if (!confirm(`Delete this ${t.kind} track? It stops playing at once.`)) return
    void act(() => api(`/v1/assets/${asset.id}/tracks/${t.id}`, { method: 'DELETE', ...opts }))
  }
  return (
    <Card className="grid gap-4" aria-label="Tracks">
      <CardTitle>Subtitles and chapters</CardTitle>
      {asset.tracks.length === 0 ? (
        <Muted>
          {asset.status === 'ready'
            ? "None yet. Make them from this video's audio:"
            : 'None yet. You can make them from the audio once the video is ready.'}
        </Muted>
      ) : (
        <ul className="grid gap-2">
          {asset.tracks.map((t) => (
            <li
              key={t.id}
              className="grid gap-2 rounded-md border border-zinc-200 p-3 text-sm"
              data-track={t.kind}
            >
              <div className="flex flex-wrap items-center gap-3">
                <span className="font-medium capitalize">{t.kind}</span>
                {t.name && <span>{t.name}</span>}
                <span className={statusClass[t.status] ?? ''}>
                  {t.status}
                  {t.error_code ? ` (${t.error_code})` : ''}
                  {t.status === 'preparing' && job && asset.status === 'ready' && (
                    <span className="text-zinc-500"> — {jobLine(job)}</span>
                  )}
                </span>
                <span className="font-mono text-xs text-zinc-500">{t.id}</span>
                <span className="ml-auto flex gap-2">
                  {t.status === 'ready' && (
                    <Button variant="outline" size="sm" onClick={() => setOpen(open === t.id ? null : t.id)}>
                      {open === t.id ? 'Hide' : t.kind === 'subtitles' ? 'Transcript' : 'Chapters'}
                    </Button>
                  )}
                  <Button variant="ghost" size="sm" onClick={() => remove(t)}>
                    Delete
                  </Button>
                </span>
              </div>
              {open === t.id && <TrackBody asset={asset} track={t} app={app} />}
            </li>
          ))}
        </ul>
      )}
      {asset.status === 'ready' && (
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => void regenerate('subtitles')}>
            {asset.tracks.some((t) => t.kind === 'subtitles') ? 'Regenerate subtitles' : 'Generate subtitles'}
          </Button>
          <Button variant="outline" size="sm" onClick={() => void regenerate('chapters')}>
            {asset.tracks.some((t) => t.kind === 'chapters') ? 'Regenerate chapters' : 'Generate chapters'}
          </Button>
        </div>
      )}
      <Muted>A new track keeps the old one playing until it is ready.</Muted>
      {error && <p className="text-sm text-red-700">{error}</p>}
    </Card>
  )
}

const clock = (ms: number) => {
  const s = Math.floor(ms / 1000)
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
}

function TrackBody({ asset, track, app }: { asset: Asset; track: Track; app?: string | undefined }) {
  const [transcript, setTranscript] = useState<Transcript | null>(null)
  const [detail, setDetail] = useState<TrackDetail | null>(null)
  useEffect(() => {
    const opts = app ? { app } : {}
    if (track.kind === 'subtitles') {
      void api<Transcript>(`/v1/assets/${asset.id}/tracks/${track.id}/transcript`, opts).then(setTranscript)
    } else {
      void api<TrackDetail>(`/v1/assets/${asset.id}/tracks/${track.id}`, opts).then(setDetail)
    }
  }, [asset.id, track, app])
  if (track.kind === 'chapters') {
    if (!detail) return <Muted>Loading…</Muted>
    return (
      <ol className="grid gap-1" aria-label="Chapter list">
        {(detail.chapters ?? []).map((c) => (
          <li key={c.start_ms}>
            <span className="font-mono text-xs text-zinc-500">{clock(c.start_ms)}</span> {c.title}
          </li>
        ))}
        {detail.chapters?.length === 0 && <Muted>No speech, so no chapters.</Muted>}
      </ol>
    )
  }
  if (!transcript) return <Muted>Loading…</Muted>
  return (
    <section className="grid max-h-80 gap-1 overflow-y-auto" aria-label="Transcript">
      <Muted>
        {track.name ?? transcript.language}, {transcript.segments.length} segments
      </Muted>
      {transcript.segments.map((s) => (
        <p key={s.id}>
          <span className="font-mono text-xs text-zinc-500">{clock(s.start_ms)}</span> {s.text}
        </p>
      ))}
    </section>
  )
}
