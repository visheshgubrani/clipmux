import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { GridIcon, ListIcon, MoreHorizontalIcon, SearchIcon, UploadIcon } from 'lucide-react'
import * as React from 'react'
import { Link, useSearchParams } from 'react-router'
import { del, type LibraryAsset } from '@/api'
import { useConfirm } from '@/components/confirm'
import { PageBody, PageHeader } from '@/components/page'
import { isPublic, Poster } from '@/components/poster'
import { QueryState } from '@/components/query-state'
import { AssetStatusBadge } from '@/components/status'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyTitle } from '@/components/ui/empty'
import { InputGroup, InputGroupAddon, InputGroupInput } from '@/components/ui/input-group'
import { Label } from '@/components/ui/label'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Spinner } from '@/components/ui/spinner'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { useUpload } from '@/components/upload'
import { ago, timecode, videoName, videoPath } from '@/lib/format'
import { notify } from '@/lib/notify'
import { PosterGrid, PosterGridSkeleton } from '@/pages/home'
import { q } from '@/queries'

const VIEW_KEY = 'clipmux-videos-view'

function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = React.useState(value)
  React.useEffect(() => {
    const t = setTimeout(() => setV(value), ms)
    return () => clearTimeout(t)
  }, [value, ms])
  return v
}

export function Videos() {
  const upload = useUpload()
  const [params, setParams] = useSearchParams()
  const [text, setText] = React.useState(params.get('q') ?? '')
  const search = useDebounced(text.trim(), 250)
  const status = params.get('status') ?? ''
  const app = params.get('app') ?? ''
  const showDeleted = params.get('deleted') === '1'
  const [view, setView] = React.useState<'grid' | 'list'>(() => {
    try {
      return localStorage.getItem(VIEW_KEY) === 'list' ? 'list' : 'grid'
    } catch {
      return 'grid'
    }
  })
  const setParam = (k: string, v: string) => {
    const next = new URLSearchParams(params)
    if (v) next.set(k, v)
    else next.delete(k)
    setParams(next, { replace: true })
  }
  // biome-ignore lint/correctness/useExhaustiveDependencies: mirror the debounced search into the URL
  React.useEffect(() => {
    if ((params.get('q') ?? '') !== search) setParam('q', search)
  }, [search])

  const apps = useQuery(q.apps())
  const counts = useQuery(q.counts(app || undefined))
  const library = useInfiniteQuery(
    q.library({
      ...(search ? { q: search } : {}),
      ...(status ? { status } : {}),
      ...(app ? { app_id: app } : {}),
      ...(showDeleted ? { include_deleted: true } : {}),
    }),
  )
  const appNames = Object.fromEntries((apps.data?.data ?? []).map((a) => [a.id, a.name]))
  const c = counts.data
  const total = c ? c.ready + c.processing + c.errored + c.waiting_upload : undefined
  const chips = [
    { value: 'all', label: 'All', n: total },
    { value: 'ready', label: 'Ready', n: c?.ready },
    { value: 'processing', label: 'Encoding', n: c?.processing },
    { value: 'errored', label: 'Failed', n: c?.errored },
  ]

  // Dropping files anywhere on the page opens Upload with them.
  const [dragging, setDragging] = React.useState(false)
  return (
    <PageBody
      onDragOver={(e) => {
        if (e.dataTransfer.types.includes('Files')) {
          e.preventDefault()
          setDragging(true)
        }
      }}
      onDragLeave={(e) => {
        if (e.currentTarget === e.target) setDragging(false)
      }}
      onDrop={(e) => {
        if (!e.dataTransfer.files.length) return
        e.preventDefault()
        setDragging(false)
        upload.open([...e.dataTransfer.files])
      }}
      className={dragging ? 'outline-2 outline-offset-[-8px] outline-dashed outline-primary' : undefined}
    >
      <PageHeader
        title="Videos"
        actions={
          <Button className="max-md:hidden" onClick={() => upload.open()}>
            <UploadIcon data-icon="inline-start" />
            Upload
          </Button>
        }
      />
      <div className="flex flex-col gap-6">
        <div className="flex flex-wrap items-center gap-2">
          <InputGroup className="max-w-sm min-w-0 flex-[1_1_240px]">
            <InputGroupAddon>
              <SearchIcon />
            </InputGroupAddon>
            <InputGroupInput
              type="search"
              aria-label="Search videos"
              placeholder="Search by title, reference or ID"
              value={text}
              onChange={(e) => setText(e.target.value)}
            />
          </InputGroup>
          <ToggleGroup
            aria-label="Status"
            variant="outline"
            size="sm"
            value={[status || 'all']}
            onValueChange={(v: string[]) => v[0] && setParam('status', v[0] === 'all' ? '' : v[0])}
          >
            {chips.map((chip) => (
              <ToggleGroupItem key={chip.value} value={chip.value}>
                {chip.label}
                {chip.n !== undefined && <span className="text-muted-foreground tabular-nums">{chip.n}</span>}
              </ToggleGroupItem>
            ))}
          </ToggleGroup>
          {(apps.data?.data.length ?? 0) > 1 && (
            <NativeSelect
              size="sm"
              aria-label="App"
              value={app}
              onChange={(e) => setParam('app', e.target.value)}
            >
              <NativeSelectOption value="">All apps</NativeSelectOption>
              {(apps.data?.data ?? []).map((a) => (
                <NativeSelectOption key={a.id} value={a.id}>
                  {a.name}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          )}
          <div className="ml-auto flex items-center gap-4">
            <Label className="text-sm font-normal text-muted-foreground">
              <Switch
                size="sm"
                checked={showDeleted}
                onCheckedChange={(v) => setParam('deleted', v ? '1' : '')}
              />
              Show deleted
            </Label>
            <ToggleGroup
              aria-label="View"
              variant="outline"
              size="sm"
              value={[view]}
              onValueChange={(v: string[]) => {
                const next = v[0] === 'list' ? 'list' : 'grid'
                setView(next)
                try {
                  localStorage.setItem(VIEW_KEY, next)
                } catch {
                  // Remembered for this visit only.
                }
              }}
            >
              <ToggleGroupItem value="grid" aria-label="Grid view">
                <GridIcon />
              </ToggleGroupItem>
              <ToggleGroupItem value="list" aria-label="List view">
                <ListIcon />
              </ToggleGroupItem>
            </ToggleGroup>
          </div>
        </div>

        <QueryState query={library} what="your videos" skeleton={<PosterGridSkeleton count={12} />}>
          {(data) => {
            const videos = data.pages.flatMap((p) => p.data)
            if (videos.length === 0)
              return search || status || app ? (
                <Empty className="border border-dashed">
                  <EmptyHeader>
                    <EmptyTitle>No videos match</EmptyTitle>
                    <EmptyDescription>Try another search, or clear the filters.</EmptyDescription>
                  </EmptyHeader>
                  <EmptyContent>
                    <Button
                      variant="outline"
                      onClick={() => {
                        setText('')
                        setParams(new URLSearchParams(), { replace: true })
                      }}
                    >
                      Clear filters
                    </Button>
                  </EmptyContent>
                </Empty>
              ) : (
                <button
                  type="button"
                  onClick={() => upload.open()}
                  className="flex flex-col items-center gap-2 rounded-xl border border-dashed bg-card px-6 py-16 text-center hover:bg-muted/40"
                >
                  <UploadIcon className="size-5 text-muted-foreground" aria-hidden="true" />
                  <span className="font-medium">Upload your first video</span>
                  <span className="text-sm text-muted-foreground">
                    Drop files here, or click to choose. MP4, MOV, MKV and WebM all work.
                  </span>
                </button>
              )
            return view === 'grid' ? (
              <PosterGrid videos={videos} />
            ) : (
              <VideoTable videos={videos} appNames={appNames} />
            )
          }}
        </QueryState>
        {library.hasNextPage && (
          <div className="flex justify-center">
            <Button
              variant="outline"
              onClick={() => void library.fetchNextPage()}
              disabled={library.isFetchingNextPage}
            >
              {library.isFetchingNextPage && <Spinner data-icon="inline-start" />}
              Load more
            </Button>
          </div>
        )}
      </div>
    </PageBody>
  )
}

function VideoTable({ videos, appNames }: { videos: LibraryAsset[]; appNames: Record<string, string> }) {
  return (
    <div className="overflow-x-auto rounded-lg border bg-card">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Video</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Privacy</TableHead>
            <TableHead>Duration</TableHead>
            <TableHead>Captions</TableHead>
            <TableHead>App</TableHead>
            <TableHead>Added</TableHead>
            <TableHead>
              <span className="sr-only">Actions</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {videos.map((a) => (
            <TableRow key={a.id}>
              <TableCell className="max-w-[22rem]">
                <Link to={videoPath(a.id, a.app_id)} className="flex items-center gap-3 font-medium">
                  <Poster asset={a} className="w-20 shrink-0 rounded-sm" />
                  <span className="flex min-w-0 flex-col">
                    <span className="truncate">{videoName(a)}</span>
                    {a.passthrough === 'clipmux-setup-test' && (
                      <span className="text-xs font-normal text-muted-foreground">Setup test</span>
                    )}
                  </span>
                </Link>
              </TableCell>
              <TableCell>
                <AssetStatusBadge status={a.status} />
              </TableCell>
              <TableCell className="text-muted-foreground">{isPublic(a) ? 'Public' : 'Private'}</TableCell>
              <TableCell className="type-timecode text-muted-foreground">
                {timecode(a.duration_seconds) ?? '—'}
              </TableCell>
              <TableCell className="text-muted-foreground">
                {a.tracks
                  .filter((t) => t.status === 'ready')
                  .map((t) => (t.kind === 'subtitles' ? 'Subtitles' : 'Chapters'))
                  .join(', ') || '—'}
              </TableCell>
              <TableCell className="text-muted-foreground">{appNames[a.app_id] ?? a.app_id}</TableCell>
              <TableCell className="text-muted-foreground">{ago(a.created_at)}</TableCell>
              <TableCell className="text-right">
                <RowMenu asset={a} />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

function RowMenu({ asset }: { asset: LibraryAsset }) {
  const confirm = useConfirm()
  const qc = useQueryClient()
  const remove = useMutation({
    mutationFn: () => del(`/v1/assets/${asset.id}`, asset.app_id),
    onSuccess: () => {
      notify.done('Video deleted')
      void qc.invalidateQueries({ queryKey: ['library'] })
      void qc.invalidateQueries({ queryKey: ['library-counts'] })
    },
    onError: (err) => notify.failed("Couldn't delete the video", err),
  })
  if (asset.status === 'deleted') return null
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={<Button variant="ghost" size="icon-sm" aria-label={`Actions for ${videoName(asset)}`} />}
      >
        <MoreHorizontalIcon />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuGroup>
          <DropdownMenuItem render={<Link to={videoPath(asset.id, asset.app_id)} />}>Open</DropdownMenuItem>
          <DropdownMenuItem onClick={() => void navigator.clipboard.writeText(asset.id)}>
            Copy ID
          </DropdownMenuItem>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuGroup>
          <DropdownMenuItem
            variant="destructive"
            onClick={async () => {
              const ok = await confirm({
                title: `Delete "${videoName(asset)}"?`,
                description:
                  'Playback stops everywhere, its files are removed and its subtitles and chapters go with it. This can’t be undone.',
                action: 'Delete video',
              })
              if (ok) remove.mutate()
            }}
          >
            Delete
          </DropdownMenuItem>
        </DropdownMenuGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
