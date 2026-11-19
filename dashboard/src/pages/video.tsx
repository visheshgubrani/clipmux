import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { MoreHorizontalIcon, PencilIcon, PlusIcon, RotateCwIcon } from 'lucide-react'
import * as React from 'react'
import { Link, useParams, useSearchParams } from 'react-router'
import {
  type Asset,
  api,
  del,
  describe,
  type Job,
  LADDER,
  type PlaybackID,
  patch,
  post,
  type Range,
  type Rung,
  type Track,
  type TrackDetail,
  type Transcript,
} from '@/api'
import { AnalyticsPanel, RangePicker } from '@/components/analytics-state'
import { useConfirm } from '@/components/confirm'
import { CopyBlock, CopyButton, CopyID } from '@/components/copy'
import { PageBody, Section } from '@/components/page'
import { isPublic } from '@/components/poster'
import { Preview } from '@/components/preview'
import { QueryState } from '@/components/query-state'
import { RenditionPicker, renditionsNote } from '@/components/renditions'
import { ShareLinks } from '@/components/share-links'
import { AssetStatusBadge, TrackStatusBadge } from '@/components/status'
import { Alert, AlertAction, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { clock, fullDate, timecode, videoName } from '@/lib/format'
import { notify } from '@/lib/notify'
import { cn } from '@/lib/utils'
import { q } from '@/queries'

const jobLine = (j: Job) =>
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

export function VideoPage() {
  const { id = '' } = useParams()
  const [params, setParams] = useSearchParams()
  const app = params.get('app') ?? undefined
  const tab = params.get('tab') ?? 'overview'
  const asset = useQuery(q.asset(id, app))
  return (
    <PageBody className="gap-6">
      <Breadcrumb>
        <BreadcrumbList>
          <BreadcrumbItem>
            <BreadcrumbLink render={<Link to="/videos" />}>Videos</BreadcrumbLink>
          </BreadcrumbItem>
          <BreadcrumbSeparator />
          <BreadcrumbItem>
            <BreadcrumbPage className="max-w-[40ch] truncate">
              {asset.data ? videoName(asset.data) : id}
            </BreadcrumbPage>
          </BreadcrumbItem>
        </BreadcrumbList>
      </Breadcrumb>
      <QueryState
        query={asset}
        what="this video"
        skeleton={
          <div className="flex flex-col gap-6">
            <Skeleton className="h-9 w-2/3" />
            <Skeleton className="aspect-video w-full max-w-4xl rounded-md" />
          </div>
        }
      >
        {(a) => (
          <Video
            asset={a}
            app={app}
            tab={tab}
            onTab={(t) => {
              const next = new URLSearchParams(params)
              if (t === 'overview') next.delete('tab')
              else next.set('tab', t)
              setParams(next, { replace: true })
            }}
          />
        )}
      </QueryState>
    </PageBody>
  )
}

function Video({
  asset: a,
  app,
  tab,
  onTab,
}: {
  asset: Asset
  app?: string | undefined
  tab: string
  onTab: (t: string) => void
}) {
  const busy = a.status === 'processing' || a.tracks.some((t) => t.status === 'preparing')
  const jobs = useQuery(q.assetJobs(a.id, busy))
  const job = jobs.data?.data.find((j) => j.status === 'running' || j.status === 'queued') ?? null
  // The subtitles the player was opened with (null: not opened yet).
  const [playerSubtitles, setPlayerSubtitles] = React.useState<string | null>(null)
  const subtitles = subtitlesKey(a)
  React.useEffect(() => {
    if (a.status === 'ready' && playerSubtitles === null) setPlayerSubtitles(subtitles)
  }, [a.status, playerSubtitles, subtitles])
  const chapters = a.tracks.find((t) => t.kind === 'chapters' && t.status === 'ready')
  const deleted = a.status === 'deleted'

  return (
    <div className="flex flex-col gap-6">
      <Header asset={a} app={app} job={job} />
      {a.status === 'ready' && playerSubtitles !== null && subtitles !== playerSubtitles && (
        <Alert>
          <AlertDescription>New subtitles are ready.</AlertDescription>
          <AlertAction>
            <Button variant="outline" size="sm" onClick={() => setPlayerSubtitles(subtitles)}>
              Reload the player
            </Button>
          </AlertAction>
        </Alert>
      )}
      <Tabs value={tab} onValueChange={(v) => onTab(String(v))}>
        <TabsList
          variant="line"
          className="no-scrollbar w-full justify-start overflow-x-auto border-b *:flex-none! *:px-3!"
        >
          <TabsTrigger value="overview">Overview</TabsTrigger>
          {!deleted && <TabsTrigger value="captions">Captions & chapters</TabsTrigger>}
          {!deleted && <TabsTrigger value="sharing">Sharing</TabsTrigger>}
          <TabsTrigger value="analytics">Analytics</TabsTrigger>
        </TabsList>
        {/* The player stays mounted across tabs, so it keeps playing. */}
        <TabsContent value="overview" keepMounted className="pt-6 data-[hidden]:hidden">
          <div className="flex flex-col gap-8">
            {a.status === 'ready' ? (
              <div className="flex max-w-4xl flex-col gap-0">
                <PlayerWithChapters
                  key={playerSubtitles ?? ''}
                  asset={a}
                  app={app}
                  chaptersTrack={chapters}
                />
              </div>
            ) : (
              <div className="flex aspect-video max-w-4xl items-center justify-center rounded-md bg-poster text-sm text-white/70">
                {a.status === 'errored' && a.error_code === 'upload_cancelled'
                  ? 'The upload was cancelled.'
                  : a.status === 'errored'
                    ? `The encode failed${a.error_code ? ` (${a.error_code})` : ''}. Re-encode to try again.`
                    : deleted
                      ? 'This video was deleted.'
                      : 'The player appears once encoding finishes.'}
              </div>
            )}
            <Details asset={a} />
          </div>
        </TabsContent>
        {!deleted && (
          <TabsContent value="captions" className="pt-6">
            <TracksSection asset={a} app={app} job={job} />
          </TabsContent>
        )}
        {!deleted && (
          <TabsContent value="sharing" className="pt-6">
            <Sharing asset={a} app={app} />
          </TabsContent>
        )}
        <TabsContent value="analytics" className="pt-6">
          <VideoAnalytics id={a.id} />
        </TabsContent>
      </Tabs>
    </div>
  )
}

function Header({ asset: a, app, job }: { asset: Asset; app?: string | undefined; job: Job | null }) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const [renaming, setRenaming] = React.useState(false)
  const [reencoding, setReencoding] = React.useState(false)
  const refresh = () => qc.invalidateQueries({ queryKey: ['asset', a.id] })
  const remove = useMutation({
    mutationFn: () => del(`/v1/assets/${a.id}`, app),
    onSuccess: () => {
      notify.done('Video deleted')
      void refresh()
      void qc.invalidateQueries({ queryKey: ['library'] })
      void qc.invalidateQueries({ queryKey: ['library-counts'] })
    },
    onError: (err) => notify.failed("Couldn't delete the video", err),
  })
  const deleted = a.status === 'deleted'
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <h1 className="type-page-title min-w-0 break-words">{videoName(a)}</h1>
        {!deleted && (
          <Button variant="ghost" size="icon-sm" aria-label="Rename" onClick={() => setRenaming(true)}>
            <PencilIcon />
          </Button>
        )}
        <div className="ml-auto flex items-center gap-2">
          {!deleted && (
            <Button variant="outline" onClick={() => setReencoding(true)} disabled={a.status !== 'ready'}>
              <RotateCwIcon data-icon="inline-start" />
              Re-encode
            </Button>
          )}
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button variant="outline" size="icon" aria-label="More actions" />}>
              <MoreHorizontalIcon />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuGroup>
                <DropdownMenuItem onClick={() => void navigator.clipboard.writeText(a.id)}>
                  Copy video ID
                </DropdownMenuItem>
                <DropdownMenuItem render={<Link to={`/jobs?asset_id=${a.id}`} />}>
                  See its jobs
                </DropdownMenuItem>
              </DropdownMenuGroup>
              {!deleted && (
                <>
                  <DropdownMenuSeparator />
                  <DropdownMenuGroup>
                    <DropdownMenuItem
                      variant="destructive"
                      onClick={async () => {
                        const ok = await confirm({
                          title: `Delete "${videoName(a)}"?`,
                          description:
                            'Playback stops everywhere, its files are removed and its subtitles and chapters go with it. This can’t be undone.',
                          action: 'Delete video',
                        })
                        if (ok) remove.mutate()
                      }}
                    >
                      Delete video
                    </DropdownMenuItem>
                  </DropdownMenuGroup>
                </>
              )}
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-sm text-muted-foreground">
        <AssetStatusBadge status={a.status} errorCode={a.error_code} />
        {!deleted && <span>{isPublic(a) ? 'Public' : 'Private'}</span>}
        {job && a.status === 'processing' && <span data-job-progress>{jobLine(job)}</span>}
      </div>
      <RenameDialog asset={a} app={app} open={renaming} onOpenChange={setRenaming} />
      <ReencodeDialog
        asset={a}
        app={app}
        open={reencoding}
        onOpenChange={setReencoding}
        onQueued={() => void refresh()}
      />
    </div>
  )
}

function ReencodeDialog({
  asset: a,
  app,
  open,
  onOpenChange,
  onQueued,
}: {
  asset: Asset
  app?: string | undefined
  open: boolean
  onOpenChange: (o: boolean) => void
  onQueued: () => void
}) {
  const current: readonly Rung[] = a.renditions ?? LADDER
  const [renditions, setRenditions] = React.useState<Rung[]>([...current])
  React.useEffect(() => {
    if (open) setRenditions([...(a.renditions ?? LADDER)])
  }, [open, a.renditions])
  const same = renditions.length === current.length && renditions.every((r) => current.includes(r))
  const reencode = useMutation({
    // An unchanged choice is left out, so the asset keeps the one it has.
    mutationFn: () => post(`/v1/assets/${a.id}/reencode`, same ? {} : { renditions }, app),
    onSuccess: () => {
      notify.done('Re-encode queued', 'The current version keeps playing until the new one is ready.')
      onOpenChange(false)
      onQueued()
    },
  })
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Re-encode video</DialogTitle>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            reencode.mutate()
          }}
        >
          <FieldGroup>
            <Field data-invalid={reencode.isError || undefined}>
              <FieldLabel id="reencode-renditions">Resolutions</FieldLabel>
              <RenditionPicker labelledBy="reencode-renditions" value={renditions} onChange={setRenditions} />
              <FieldDescription>
                {renditionsNote(renditions)} The current version keeps playing until the new one is ready.
              </FieldDescription>
              {reencode.isError && <FieldError>{describe(reencode.error)}</FieldError>}
            </Field>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={reencode.isPending}>
                {reencode.isPending && <Spinner data-icon="inline-start" />}
                Re-encode
              </Button>
            </DialogFooter>
          </FieldGroup>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function RenameDialog({
  asset: a,
  app,
  open,
  onOpenChange,
}: {
  asset: Asset
  app?: string | undefined
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const save = useMutation({
    mutationFn: (title: string) => patch<Asset>(`/v1/assets/${a.id}`, { title }, app),
    onSuccess: (next) => {
      qc.setQueryData(['asset', a.id, app ?? ''], next)
      void qc.invalidateQueries({ queryKey: ['library'] })
      notify.done('Title saved')
      onOpenChange(false)
    },
  })
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Rename video</DialogTitle>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            save.mutate(String(new FormData(e.currentTarget).get('title') ?? '').trim())
          }}
        >
          <FieldGroup>
            <Field data-invalid={save.isError || undefined}>
              <FieldLabel htmlFor="title">Title</FieldLabel>
              <Input id="title" name="title" defaultValue={a.title ?? ''} maxLength={200} autoFocus />
              <FieldDescription>
                For people: it shows here and in your library. Leave it blank to clear it.
              </FieldDescription>
              {save.isError && <FieldError>{describe(save.error)}</FieldError>}
            </Field>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
                Cancel
              </Button>
              <Button type="submit" disabled={save.isPending}>
                {save.isPending && <Spinner data-icon="inline-start" />}
                Save title
              </Button>
            </DialogFooter>
          </FieldGroup>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/** The player on black, with the chapters drawn as a labelled strip that seeks. */
function PlayerWithChapters({
  asset: a,
  app,
  chaptersTrack,
}: {
  asset: Asset
  app?: string | undefined
  chaptersTrack?: Track | undefined
}) {
  const wrap = React.useRef<HTMLDivElement>(null)
  const detail = useQuery({
    queryKey: ['track', a.id, chaptersTrack?.id ?? ''],
    queryFn: () => api<TrackDetail>(`/v1/assets/${a.id}/tracks/${chaptersTrack?.id}`, { app }),
    enabled: !!chaptersTrack,
  })
  const chapters = detail.data?.chapters ?? []
  const total = (a.duration_seconds ?? 0) * 1000
  const seek = (ms: number) => {
    const el = wrap.current?.querySelector('clipmux-player') as
      | (HTMLElement & { media?: HTMLVideoElement | null })
      | null
    const media = el?.media
    if (media) {
      media.currentTime = ms / 1000
      void media.play?.()
    }
  }
  return (
    <div ref={wrap} className="flex flex-col gap-3">
      <Preview assetId={a.id} baseUrl={a.base_url} chaptersTrack={chaptersTrack?.id} />
      {chapters.length > 0 && total > 0 && (
        <nav aria-label="Chapters" className="flex gap-0.5">
          {chapters.map((c) => (
            <button
              key={c.start_ms}
              type="button"
              onClick={() => seek(c.start_ms)}
              style={{ flexGrow: Math.max(1, c.end_ms - c.start_ms) }}
              className="group flex min-w-0 basis-0 flex-col gap-1.5 text-left"
            >
              <span className="h-1 rounded-full bg-muted transition-colors group-hover:bg-primary" />
              <span className="flex min-w-0 gap-1.5 text-xs">
                <span className="type-timecode text-muted-foreground">{clock(c.start_ms)}</span>
                <span className="truncate">{c.title}</span>
              </span>
            </button>
          ))}
        </nav>
      )}
    </div>
  )
}

function Details({ asset: a }: { asset: Asset }) {
  const apps = useQuery(q.apps())
  const appName = apps.data?.data.find((x) => x.id === a.app_id)?.name ?? a.app_id
  const rows: [string, React.ReactNode][] = [
    [
      'Duration',
      <span key="d" className="type-timecode">
        {timecode(a.duration_seconds) ?? '—'}
      </span>,
    ],
    ['Added', fullDate(a.created_at)],
    ['App', appName],
    ['Version', a.version],
    ['Video ID', <CopyID key="id" value={a.id} />],
    ['Reference', a.passthrough || '—'],
  ]
  return (
    <Section title="Details">
      <dl className="grid max-w-4xl grid-cols-1 gap-x-8 gap-y-4 text-sm sm:grid-cols-2 lg:grid-cols-3">
        {rows.map(([k, v]) => (
          <div key={k} className="flex min-w-0 flex-col gap-0.5">
            <dt className="text-muted-foreground">{k}</dt>
            <dd className="min-w-0">{v}</dd>
          </div>
        ))}
      </dl>
    </Section>
  )
}

// TracksSection lists a video's subtitles and chapters (docs/api.md,
// "Tracks"), with the transcript of ready subtitles, and makes new ones or
// deletes them through the integration API.
function TracksSection({ asset: a, app, job }: { asset: Asset; app?: string | undefined; job: Job | null }) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const chapters = useQuery(q.chapters())
  const [open, setOpen] = React.useState<string | null>(null)
  const refresh = () => qc.invalidateQueries({ queryKey: ['asset', a.id] })
  const make = useMutation({
    mutationFn: (kind: 'subtitles' | 'chapters') => post(`/v1/assets/${a.id}/tracks`, { kind }, app),
    onSuccess: (_, kind) => {
      notify.done(kind === 'subtitles' ? 'Subtitles queued' : 'Chapters queued')
      void refresh()
    },
    onError: (err) => notify.failed("Couldn't start that", err),
  })
  const remove = useMutation({
    mutationFn: (t: Track) => del(`/v1/assets/${a.id}/tracks/${t.id}`, app),
    onSuccess: (_, t) => {
      notify.done(t.kind === 'subtitles' ? 'Subtitles deleted' : 'Chapters deleted')
      void refresh()
    },
    onError: (err) => notify.failed("Couldn't delete the track", err),
  })
  const has = (k: Track['kind']) => a.tracks.some((t) => t.kind === k)
  return (
    <Section
      title="Subtitles and chapters"
      description="A new track keeps the old one playing until it's ready."
      aria-label="Tracks"
      actions={
        a.status === 'ready' && (
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={() => make.mutate('subtitles')}
              disabled={make.isPending}
            >
              {has('subtitles') ? 'Regenerate subtitles' : 'Generate subtitles'}
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => make.mutate('chapters')}
              disabled={make.isPending || !chapters.data?.connection}
              title={chapters.data?.connection ? undefined : 'Connect an LLM on Captions & AI first'}
            >
              {has('chapters') ? 'Regenerate chapters' : 'Generate chapters'}
            </Button>
          </>
        )
      }
    >
      {a.tracks.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          {a.status === 'ready'
            ? "None yet. Make them from this video's audio with the buttons above."
            : 'None yet. You can make them from the audio once the video is ready.'}
          {!chapters.data?.connection && (
            <>
              {' '}
              Chapters need an LLM:{' '}
              <Link to="/captions" className="text-link underline-offset-4 hover:underline">
                connect one
              </Link>
              .
            </>
          )}
        </p>
      ) : (
        <ul className="flex flex-col divide-y rounded-lg border bg-card">
          {a.tracks.map((t) => (
            <li key={t.id} className="flex flex-col gap-3 p-4" data-track={t.kind}>
              <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm">
                <span className="font-medium">{t.kind === 'subtitles' ? 'Subtitles' : 'Chapters'}</span>
                {t.name && <span>{t.name}</span>}
                <TrackStatusBadge status={t.status} />
                {t.error_code && <span className="text-destructive">{t.error_code}</span>}
                {t.status === 'preparing' && job && a.status === 'ready' && (
                  <span className="text-muted-foreground">{jobLine(job)}</span>
                )}
                <code className="text-xs text-muted-foreground">{t.id}</code>
                <span className="ml-auto flex gap-1">
                  {t.status === 'ready' && (
                    <Button variant="outline" size="sm" onClick={() => setOpen(open === t.id ? null : t.id)}>
                      {open === t.id ? 'Hide' : t.kind === 'subtitles' ? 'Transcript' : 'Chapters'}
                    </Button>
                  )}
                  <Button
                    variant="ghost"
                    size="sm"
                    className="text-destructive"
                    onClick={async () => {
                      const ok = await confirm({
                        title: t.kind === 'subtitles' ? 'Delete these subtitles?' : 'Delete these chapters?',
                        description: 'They stop playing at once. You can generate new ones later.',
                        action: t.kind === 'subtitles' ? 'Delete subtitles' : 'Delete chapters',
                      })
                      if (ok) remove.mutate(t)
                    }}
                  >
                    Delete
                  </Button>
                </span>
              </div>
              {open === t.id && <TrackBody asset={a} track={t} app={app} />}
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}

function TrackBody({ asset, track, app }: { asset: Asset; track: Track; app?: string | undefined }) {
  const transcript = useQuery({
    queryKey: ['transcript', asset.id, track.id],
    queryFn: () => api<Transcript>(`/v1/assets/${asset.id}/tracks/${track.id}/transcript`, { app }),
    enabled: track.kind === 'subtitles',
  })
  const detail = useQuery({
    queryKey: ['track', asset.id, track.id],
    queryFn: () => api<TrackDetail>(`/v1/assets/${asset.id}/tracks/${track.id}`, { app }),
    enabled: track.kind === 'chapters',
  })
  if (track.kind === 'chapters')
    return (
      <QueryState query={detail} what="the chapters" skeleton={<Skeleton className="h-16 w-full" />}>
        {(d) =>
          (d.chapters ?? []).length === 0 ? (
            <p className="text-sm text-muted-foreground">No speech, so no chapters.</p>
          ) : (
            <ol className="flex flex-col gap-1.5 text-sm" aria-label="Chapter list">
              {(d.chapters ?? []).map((c) => (
                <li key={c.start_ms} className="flex gap-3">
                  <span className="type-timecode w-12 shrink-0 text-muted-foreground">
                    {clock(c.start_ms)}
                  </span>{' '}
                  <span>{c.title}</span>
                </li>
              ))}
            </ol>
          )
        }
      </QueryState>
    )
  return (
    <QueryState query={transcript} what="the transcript" skeleton={<Skeleton className="h-24 w-full" />}>
      {(tr) => (
        <section
          className="flex max-h-80 flex-col gap-2 overflow-y-auto pr-2 text-sm"
          aria-label="Transcript"
        >
          <p className="text-muted-foreground">
            {track.name ?? tr.language}, {tr.segments.length} segments
          </p>
          {tr.segments.map((s) => (
            <p key={s.id} className="flex gap-3 leading-relaxed">
              <span className="type-timecode w-12 shrink-0 text-muted-foreground">{clock(s.start_ms)}</span>{' '}
              <span>{s.text}</span>
            </p>
          ))}
        </section>
      )}
    </QueryState>
  )
}

/** Who can watch, in plain words, with the playback IDs under it for those who need them. */
function Sharing({ asset: a, app }: { asset: Asset; app?: string | undefined }) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const active = a.playback_ids.filter((p) => p.status === 'active')
  const pub = active.filter((p) => p.policy === 'public')
  const refresh = () => qc.invalidateQueries({ queryKey: ['asset', a.id] })
  const add = useMutation({
    mutationFn: (policy: PlaybackID['policy']) =>
      post<PlaybackID>(`/v1/assets/${a.id}/playback-ids`, { policy }, app),
    onSuccess: () => void refresh(),
    onError: (err) => notify.failed("Couldn't add the playback ID", err),
  })
  const revoke = useMutation({
    mutationFn: (pid: string) => del(`/v1/assets/${a.id}/playback-ids/${pid}`, app),
    onSuccess: () => void refresh(),
    onError: (err) => notify.failed("Couldn't revoke the playback ID", err),
  })
  const setPrivacy = async (to: 'private' | 'public') => {
    if (to === 'public' && pub.length === 0) {
      await add.mutateAsync('public')
      notify.done('Video is public')
    }
    if (to === 'private' && pub.length > 0) {
      const ok = await confirm({
        title: 'Make this video private?',
        description:
          'Its public links and embeds stop playing at once. Your app can still play it with signed tokens.',
        action: 'Make private',
      })
      if (!ok) return
      // A share link's ID doesn't count: it stops with its link.
      if (!active.some((p) => p.policy === 'signed' && !p.share)) await add.mutateAsync('signed')
      for (const p of pub) await revoke.mutateAsync(p.id)
      notify.done('Video is private')
    }
  }
  const first = pub[0]
  return (
    <div className="flex max-w-3xl flex-col gap-10">
      <Section title="Who can watch">
        <RadioGroup
          value={pub.length > 0 ? 'public' : 'private'}
          onValueChange={(v) => void setPrivacy(v as 'private' | 'public')}
          aria-label="Who can watch"
          className="gap-2"
        >
          {[
            {
              v: 'private',
              title: 'Private',
              body: 'Plays only with a token your app signs. Best for paid or signed-in content.',
            },
            { v: 'public', title: 'Public', body: 'Anyone with the link or the embed can watch.' },
          ].map((o) => (
            <Label
              key={o.v}
              htmlFor={`privacy-${o.v}`}
              className="flex items-start gap-3 rounded-lg border p-4 font-normal has-data-checked:border-primary has-data-checked:bg-primary/5"
            >
              <RadioGroupItem value={o.v} id={`privacy-${o.v}`} className="mt-0.5" />
              <span className="flex flex-col gap-1">
                <span className="font-medium">{o.title}</span>
                <span className="text-muted-foreground">{o.body}</span>
              </span>
            </Label>
          ))}
        </RadioGroup>
        {first && (
          <div className="flex flex-col gap-2 pt-2">
            <span className="text-sm font-medium">Embed</span>
            <CopyBlock
              label="embed code"
              value={`<!-- npm i @clipmux/player, then in your bundle: import '@clipmux/player/element' -->\n<clipmux-player base-url="${a.base_url}" playback-id="${first.id}"></clipmux-player>`}
            />
            <span className="text-sm font-medium">Stream URL</span>
            <CopyBlock label="stream URL" value={`${a.base_url}/p/${first.id}/master.m3u8`} />
          </div>
        )}
        {!first && (
          <p className="text-sm text-muted-foreground">
            Your app signs a playback token with one of its signing keys for each viewer. See{' '}
            <Link to={`/apps/${a.app_id}`} className="text-link underline-offset-4 hover:underline">
              this video's app
            </Link>
            .
          </p>
        )}
      </Section>
      <ShareLinks asset={a} app={app} />
      <Section
        title="Playback IDs"
        description="Each ID plays the video under its own policy and can be revoked on its own."
        actions={
          <DropdownMenu>
            <DropdownMenuTrigger render={<Button variant="outline" size="sm" />}>
              <PlusIcon data-icon="inline-start" />
              Add playback ID
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuGroup>
                <DropdownMenuItem onClick={() => add.mutate('signed')}>Signed</DropdownMenuItem>
                <DropdownMenuItem onClick={() => add.mutate('public')}>Public</DropdownMenuItem>
              </DropdownMenuGroup>
            </DropdownMenuContent>
          </DropdownMenu>
        }
      >
        <div className="overflow-x-auto rounded-lg border bg-card">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>Policy</TableHead>
                <TableHead>State</TableHead>
                <TableHead>
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {a.playback_ids.map((p) => (
                <TableRow key={p.id}>
                  <TableCell>
                    <span className="flex items-center gap-1">
                      <code className="text-[13px]">{p.id}</code>
                      <CopyButton value={p.id} label="Copy playback ID" />
                    </span>
                  </TableCell>
                  <TableCell>
                    {p.share ? (
                      <span className="flex flex-col">
                        Share link
                        <span className="max-w-48 truncate text-muted-foreground">
                          {p.share.name || 'Untitled link'}
                        </span>
                      </span>
                    ) : p.policy === 'public' ? (
                      'Public'
                    ) : (
                      'Signed'
                    )}
                  </TableCell>
                  <TableCell>
                    <Badge variant={p.status === 'active' ? 'success' : 'secondary'}>
                      {p.status === 'active' ? 'Active' : p.status === 'revoking' ? 'Revoking' : 'Revoked'}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-right">
                    {/* A share link's ID is revoked with its link, above. */}
                    {p.status === 'active' && !p.share && (
                      <Button
                        variant="ghost"
                        size="sm"
                        className="text-destructive"
                        onClick={async () => {
                          const ok = await confirm({
                            title: 'Revoke this playback ID?',
                            description: `Anything playing ${p.id} stops within a minute. This can’t be undone.`,
                            action: 'Revoke ID',
                          })
                          if (ok) revoke.mutate(p.id)
                        }}
                      >
                        Revoke
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      </Section>
    </div>
  )
}

function VideoAnalytics({ id }: { id: string }) {
  const [range, setRange] = React.useState<Range>('30d')
  return (
    <div className={cn('flex flex-col gap-4')}>
      <div className="flex justify-end">
        <RangePicker value={range} onChange={setRange} />
      </div>
      <AnalyticsPanel range={range} asset={id} />
    </div>
  )
}
