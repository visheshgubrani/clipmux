import { useQuery } from '@tanstack/react-query'
import { CircleCheckIcon, CircleIcon, UploadIcon, XIcon } from 'lucide-react'
import * as React from 'react'
import { Link } from 'react-router'
import type { LibraryAsset } from '@/api'
import { PageBody, PageHeader, Section } from '@/components/page'
import { Poster } from '@/components/poster'
import { QueryState } from '@/components/query-state'
import { Button } from '@/components/ui/button'
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyTitle } from '@/components/ui/empty'
import {
  Item,
  ItemActions,
  ItemContent,
  ItemDescription,
  ItemGroup,
  ItemMedia,
  ItemTitle,
} from '@/components/ui/item'
import { Skeleton } from '@/components/ui/skeleton'
import { useUpload } from '@/components/upload'
import { ago, greeting, videoName, videoPath } from '@/lib/format'
import { q } from '@/queries'

// Home answers "is everything fine?" (docs/design.md): what's left to set
// up, what needs attention, and the newest videos. No number tiles; the
// numbers live on Analytics.

const HIDE_KEY = 'clipmux-hide-setup'

export function Home() {
  const upload = useUpload()
  const recent = useQuery({ ...q.latest({}, 8), select: (d) => d.data })
  const attention = useQuery({ ...q.latest({ status: 'errored' }, 5), select: (d) => d.data })
  const encoding = useQuery({ ...q.latest({ status: 'processing' }, 5), select: (d) => d.data })
  const needs = [...(attention.data ?? []), ...(encoding.data ?? [])]

  return (
    <PageBody>
      <PageHeader
        title={greeting()}
        actions={
          <Button className="max-md:hidden" onClick={() => upload.open()}>
            <UploadIcon data-icon="inline-start" />
            Upload videos
          </Button>
        }
      />
      <Checklist />
      {needs.length > 0 && (
        <Section title="Needs attention" aria-label="Needs attention">
          <ItemGroup className="gap-1">
            {needs.map((a) => (
              <Item key={a.id} variant="outline" size="sm" render={<Link to={videoPath(a.id, a.app_id)} />}>
                <ItemMedia className="w-24">
                  <Poster asset={a} />
                </ItemMedia>
                <ItemContent>
                  <ItemTitle>{videoName(a)}</ItemTitle>
                  <ItemDescription>
                    {a.status === 'errored'
                      ? `Failed: ${a.error_code ?? 'the encode stopped'}`
                      : `Encoding, added ${ago(a.created_at)}`}
                  </ItemDescription>
                </ItemContent>
              </Item>
            ))}
          </ItemGroup>
        </Section>
      )}
      <Section
        title="Recent videos"
        actions={
          <Button variant="link" size="sm" render={<Link to="/videos" />}>
            View all
          </Button>
        }
      >
        <QueryState query={recent} what="your videos" skeleton={<PosterGridSkeleton count={8} />}>
          {(videos) =>
            videos.length === 0 ? (
              <Empty className="border border-dashed">
                <EmptyHeader>
                  <EmptyTitle>No videos yet</EmptyTitle>
                  <EmptyDescription>Upload one and it's ready to play in a few minutes.</EmptyDescription>
                </EmptyHeader>
                <EmptyContent>
                  <Button onClick={() => upload.open()}>
                    <UploadIcon data-icon="inline-start" />
                    Upload videos
                  </Button>
                </EmptyContent>
              </Empty>
            ) : (
              <PosterGrid videos={videos} />
            )
          }
        </QueryState>
      </Section>
      <StatusLine />
    </PageBody>
  )
}

export function PosterGrid({ videos }: { videos: LibraryAsset[] }) {
  return (
    <ul className="grid grid-cols-[repeat(auto-fill,minmax(200px,1fr))] gap-x-4 gap-y-6">
      {videos.map((a) => (
        <li key={a.id}>
          <Link
            to={videoPath(a.id, a.app_id)}
            className="group flex flex-col gap-2 rounded-md outline-offset-4"
          >
            <Poster asset={a} className="transition-opacity group-hover:opacity-90" />
            <span className="flex min-w-0 flex-col gap-0.5">
              <span className="truncate text-sm font-medium">{videoName(a)}</span>
              <span className="text-xs text-muted-foreground">{ago(a.created_at)}</span>
            </span>
          </Link>
        </li>
      ))}
    </ul>
  )
}

export function PosterGridSkeleton({ count }: { count: number }) {
  return (
    <div className="grid grid-cols-[repeat(auto-fill,minmax(200px,1fr))] gap-x-4 gap-y-6" aria-busy="true">
      {Array.from({ length: count }, (_, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: placeholders have no identity
        <div key={i} className="flex flex-col gap-2">
          <Skeleton className="aspect-video w-full rounded-md" />
          <Skeleton className="h-4 w-3/4" />
          <Skeleton className="h-3 w-1/3" />
        </div>
      ))}
    </div>
  )
}

/** "Finish setting up": each optional upgrade as a row, until done or hidden. */
function Checklist() {
  const [hidden, setHidden] = React.useState(() => {
    try {
      return localStorage.getItem(HIDE_KEY) === '1'
    } catch {
      return false
    }
  })
  const cloudflare = useQuery(q.cloudflare())
  const storage = useQuery(q.storage())
  const executors = useQuery(q.executors())
  const chapters = useQuery(q.chapters())
  const apps = useQuery(q.apps())
  const steps = [
    {
      done: (storage.data?.data ?? []).some((s) => s.default && s.kind !== 'fs'),
      title: 'Store uploads in a bucket',
      detail: "Keep videos on Cloudflare R2 or S3 instead of this server's disk.",
      to: '/storage',
    },
    {
      done: !!cloudflare.data?.connected,
      title: 'Connect Cloudflare',
      detail: 'Serve videos from the edge with a Worker.',
      to: '/delivery',
    },
    {
      done: (executors.data?.data ?? []).some((p) => p.connected),
      title: 'Add a cloud GPU',
      detail: 'RunPod or Modal, with a monthly spend limit.',
      to: '/encoding',
    },
    {
      done: !!chapters.data?.connection,
      title: 'Turn on chapters',
      detail: 'Connect an LLM to title chapters from subtitles.',
      to: '/captions',
    },
    {
      done: (apps.data?.data ?? []).some((a) => !a.builtin),
      title: 'Create an app',
      detail: 'Get an API key to upload and play from your own site.',
      to: '/apps',
    },
  ]
  const left = steps.filter((s) => !s.done).length
  if (hidden || left === 0) return null
  return (
    <Section
      title="Finish setting up"
      description={`${left} optional ${left === 1 ? 'step' : 'steps'} left. Everything already works without them.`}
      actions={
        <Button
          variant="ghost"
          size="sm"
          onClick={() => {
            try {
              localStorage.setItem(HIDE_KEY, '1')
            } catch {
              // Hidden for this visit only.
            }
            setHidden(true)
          }}
        >
          <XIcon data-icon="inline-start" />
          Hide
        </Button>
      }
    >
      <ItemGroup className="gap-0 rounded-lg border bg-card">
        {steps.map((s, i) => (
          <Item
            key={s.title}
            size="sm"
            className={i > 0 ? 'rounded-none border-t' : 'rounded-none'}
            render={<Link to={s.to} />}
          >
            <ItemMedia>
              {s.done ? (
                <CircleCheckIcon className="size-4 text-success" aria-label="Done" />
              ) : (
                <CircleIcon className="size-4 text-muted-foreground" aria-label="Not done" />
              )}
            </ItemMedia>
            <ItemContent>
              <ItemTitle className={s.done ? 'text-muted-foreground line-through decoration-1' : ''}>
                {s.title}
              </ItemTitle>
              {!s.done && <ItemDescription>{s.detail}</ItemDescription>}
            </ItemContent>
            {!s.done && <ItemActions className="text-sm text-link">Set up</ItemActions>}
          </Item>
        ))}
      </ItemGroup>
    </Section>
  )
}

const cloudNames: Record<string, string> = { modal: 'Modal', runpod: 'RunPod' }

/** One quiet line at the bottom: where things run and play from. */
function StatusLine() {
  const routing = useQuery(q.routing())
  const cloudflare = useQuery(q.cloudflare())
  const storage = useQuery(q.storage())
  const speech = useQuery(q.speech())
  const chapters = useQuery(q.chapters())
  const store = storage.data?.data.find((s) => s.default)
  const r = routing.data
  const items = [
    {
      to: '/encoding',
      label: 'Encoding',
      value: !r
        ? '…'
        : r.primary_executor in cloudNames
          ? `${cloudNames[r.primary_executor]}${r.fallback_executor === 'builtin' ? ', else this server' : ''}`
          : r.builtin_worker.online
            ? 'this server'
            : 'this server (offline)',
    },
    { to: '/delivery', label: 'Delivery', value: cloudflare.data?.connected ? 'Cloudflare' : 'built in' },
    {
      to: '/storage',
      label: 'Storage',
      value: !store
        ? '…'
        : store.kind === 'fs'
          ? "this server's disk"
          : `${store.kind === 'r2' ? 'R2' : 'S3'}, ${store.bucket ?? ''}`,
    },
    {
      to: '/captions',
      label: 'Captions',
      value: `${speech.data?.mode === 'api' ? 'speech API' : 'on your machines'}, chapters ${chapters.data?.connection ? 'on' : 'off'}`,
    },
  ]
  return (
    <footer className="flex flex-wrap gap-x-6 gap-y-2 border-t pt-4 text-sm text-muted-foreground">
      {items.map((i) => (
        <Link key={i.label} to={i.to} className="hover:text-foreground">
          {i.label}: <span className="text-foreground">{i.value}</span>
        </Link>
      ))}
    </footer>
  )
}
