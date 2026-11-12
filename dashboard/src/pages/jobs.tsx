import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { ChevronRightIcon } from 'lucide-react'
import * as React from 'react'
import { Link, useSearchParams } from 'react-router'
import type { Attempt, Job } from '@/api'
import { PageBody, PageHeader } from '@/components/page'
import { QueryState } from '@/components/query-state'
import { JobStatusBadge } from '@/components/status'
import { Button } from '@/components/ui/button'
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '@/components/ui/empty'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { ago, videoPath } from '@/lib/format'
import { cn } from '@/lib/utils'
import { q } from '@/queries'

/** What each executor is called on the page. */
export const executorNames: Record<string, string> = {
  workers: 'this server and your PCs',
  agents: 'your PCs',
  builtin: 'this server',
  agent: 'a PC',
  runpod: 'RunPod',
  modal: 'Modal',
  speech_api: 'the speech API',
}

const kindNames: Record<string, string> = {
  import: 'Import',
  transcode: 'Encode',
  reencode: 'Re-encode',
  upload_source: 'Upload from a PC',
  move: 'Move',
  delete: 'Delete',
  transcribe: 'Subtitles',
  chapters: 'Chapters',
}

// Why a job is routed where it is (D053, D055).
const reasons: Record<string, string> = {
  primary: 'the primary',
  pinned: 'its PC, which holds the file',
  primary_offline: 'the fallback: the primary was offline',
  queue_age: 'the fallback: it waited too long for the primary',
  no_fallback: 'waiting: the primary is offline and there is no fallback',
  fallback_refused: 'waiting: an earlier attempt on this video failed, so it stays off the fallback',
  fallback_offline: 'waiting: the primary and the fallback are both offline',
  fallback_limit: 'waiting: the fallback is at its machine or spend limit',
  primary_limit: 'waiting: the primary is at its machine or spend limit',
  setup_test: 'a setup test through this executor',
}

const where = (a: Attempt) =>
  a.executor === 'agent' ? (a.agent_name ?? 'a PC') : (executorNames[a.executor] ?? a.executor)

function summary(j: Job): string {
  if (j.status === 'running') {
    const a = j.attempts.at(-1)
    const pct = j.progress != null ? `${Math.round(j.progress * 100)}%` : ''
    return [a ? `On ${where(a)}` : null, j.phase, pct].filter(Boolean).join(', ')
  }
  if (j.status === 'queued') {
    if (j.pinned_agent_id) return 'Waiting for its PC'
    if (j.routed_to)
      return `Routed to ${executorNames[j.routed_to] ?? j.routed_to}: ${reasons[j.route_reason ?? ''] ?? j.route_reason}`
    return 'Waiting for a worker'
  }
  if (j.status === 'failed') return j.error_code ?? 'Failed'
  const a = j.attempts.at(-1)
  return a ? `On ${where(a)}` : 'Done by the server, no worker needed'
}

const statuses = [
  { value: 'all', label: 'All' },
  { value: 'running', label: 'Running' },
  { value: 'queued', label: 'Queued' },
  { value: 'failed', label: 'Failed' },
  { value: 'succeeded', label: 'Done' },
]

// Jobs is where each job ran, each attempt, and how it ended
// (docs/architecture.md, "Routing and fallback").
export function Jobs() {
  const [params, setParams] = useSearchParams()
  const status = params.get('status') ?? ''
  const assetId = params.get('asset_id') ?? ''
  const agentId = params.get('agent_id') ?? ''
  const kind = params.get('kind') ?? ''
  const agents = useQuery(q.agents())
  const jobs = useInfiniteQuery(
    q.jobs({
      ...(status ? { status } : {}),
      ...(assetId ? { asset_id: assetId } : {}),
      ...(agentId ? { agent_id: agentId } : {}),
    }),
  )
  const set = (k: string, v: string) => {
    const next = new URLSearchParams(params)
    if (v) next.set(k, v)
    else next.delete(k)
    setParams(next, { replace: true })
  }
  const live = jobs.data?.pages.some((p) =>
    p.data.some((j) => j.status === 'running' || j.status === 'queued'),
  )
  return (
    <PageBody>
      <PageHeader
        title="Jobs"
        description="Every encode, subtitle, chapter, move and delete: where it ran and how it ended."
        actions={
          <span className="flex items-center gap-2 text-sm text-muted-foreground">
            <span
              aria-hidden="true"
              className={cn('size-2 rounded-full', live ? 'bg-success' : 'bg-muted-foreground/40')}
            />
            {live ? 'Live, every 3 seconds' : 'Checking every 10 seconds'}
          </span>
        }
      />
      <div className="flex flex-col gap-4">
        <div className="flex flex-wrap items-center gap-2">
          <ToggleGroup
            aria-label="Status"
            variant="outline"
            size="sm"
            value={[status || 'all']}
            onValueChange={(v: string[]) => v[0] && set('status', v[0] === 'all' ? '' : v[0])}
          >
            {statuses.map((s) => (
              <ToggleGroupItem key={s.value} value={s.value}>
                {s.label}
              </ToggleGroupItem>
            ))}
          </ToggleGroup>
          <NativeSelect
            size="sm"
            aria-label="Kind"
            value={kind}
            onChange={(e) => set('kind', e.target.value)}
          >
            <NativeSelectOption value="">All kinds</NativeSelectOption>
            {Object.entries(kindNames).map(([k, label]) => (
              <NativeSelectOption key={k} value={k}>
                {label}
              </NativeSelectOption>
            ))}
          </NativeSelect>
          <NativeSelect
            size="sm"
            aria-label="Worker"
            value={agentId}
            onChange={(e) => set('agent_id', e.target.value)}
          >
            <NativeSelectOption value="">Any PC</NativeSelectOption>
            {(agents.data?.data ?? []).map((a) => (
              <NativeSelectOption key={a.id} value={a.id}>
                {a.name}
              </NativeSelectOption>
            ))}
          </NativeSelect>
          <Input
            aria-label="Video ID"
            placeholder="Video ID"
            defaultValue={assetId}
            className="h-7 w-44"
            onKeyDown={(e) => {
              if (e.key === 'Enter') set('asset_id', e.currentTarget.value.trim())
            }}
            onBlur={(e) => set('asset_id', e.currentTarget.value.trim())}
          />
        </div>
        <QueryState
          query={jobs}
          what="jobs"
          skeleton={
            <div className="flex flex-col gap-2">
              {Array.from({ length: 6 }, (_, i) => (
                // biome-ignore lint/suspicious/noArrayIndexKey: placeholders have no identity
                <Skeleton key={i} className="h-14 w-full" />
              ))}
            </div>
          }
        >
          {(data) => {
            const all = data.pages.flatMap((p) => p.data).filter((j) => !kind || j.kind === kind)
            if (all.length === 0)
              return (
                <Empty className="border border-dashed">
                  <EmptyHeader>
                    <EmptyTitle>
                      {status || kind || assetId || agentId ? 'No jobs match' : 'No jobs yet'}
                    </EmptyTitle>
                    <EmptyDescription>
                      {status || kind || assetId || agentId
                        ? 'Try another filter.'
                        : 'Upload a video and its encode shows up here.'}
                    </EmptyDescription>
                  </EmptyHeader>
                </Empty>
              )
            return (
              <ul
                className="flex flex-col divide-y overflow-hidden rounded-lg border bg-card"
                aria-label="Jobs"
              >
                {all.map((j) => (
                  <JobRow key={j.id} job={j} />
                ))}
              </ul>
            )
          }}
        </QueryState>
        {jobs.hasNextPage && (
          <div className="flex justify-center">
            <Button
              variant="outline"
              onClick={() => void jobs.fetchNextPage()}
              disabled={jobs.isFetchingNextPage}
            >
              {jobs.isFetchingNextPage && <Spinner data-icon="inline-start" />}
              Load more
            </Button>
          </div>
        )}
      </div>
    </PageBody>
  )
}

function JobRow({ job: j }: { job: Job }) {
  const [open, setOpen] = React.useState(j.status === 'failed')
  return (
    <li>
      <button
        type="button"
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        className="flex w-full flex-wrap items-center gap-x-4 gap-y-1.5 px-4 py-3 text-left hover:bg-muted/40"
      >
        <span className="w-20 shrink-0">
          <JobStatusBadge status={j.status} />
        </span>
        <span className="flex min-w-0 flex-[1_1_16rem] flex-col gap-0.5">
          <span className="truncate text-sm font-medium">{kindNames[j.kind] ?? j.kind}</span>
          <span
            className="truncate text-xs text-muted-foreground"
            data-route={j.status === 'queued' ? j.route_reason : undefined}
          >
            {summary(j)}
          </span>
        </span>
        <span className="text-xs text-muted-foreground">{ago(j.created_at)}</span>
        <ChevronRightIcon
          aria-hidden="true"
          className={cn('size-4 text-muted-foreground transition-transform', open && 'rotate-90')}
        />
      </button>
      {open && (
        <div className="flex flex-col gap-1.5 border-t bg-muted/30 px-4 py-3 text-sm sm:pl-28">
          <Link
            to={videoPath(j.asset_id, j.app_id)}
            className="w-fit text-link underline-offset-4 hover:underline"
          >
            Open video
          </Link>
          {(j.lost_count > 0 || j.failed_count > 0) && (
            <span className="text-muted-foreground">
              {j.lost_count} lost, {j.failed_count} failed
            </span>
          )}
          {j.attempts.length === 0 ? (
            <span className="text-muted-foreground">
              {j.status === 'queued' || j.status === 'running'
                ? 'Not claimed yet.'
                : j.status === 'succeeded'
                  ? 'Done by the server, no worker needed.'
                  : 'Never ran on a worker.'}
            </span>
          ) : (
            j.attempts.map((a) => <AttemptLine key={a.id ?? a.number} a={a} />)
          )}
        </div>
      )}
    </li>
  )
}

function AttemptLine({ a }: { a: Attempt }) {
  const enc = a.encoder
  return (
    <span>
      Attempt {a.number} on <span className="font-medium">{where(a)}</span>
      {a.route_reason && a.route_reason !== 'primary' && (
        <span className="text-muted-foreground"> ({reasons[a.route_reason] ?? a.route_reason})</span>
      )}
      , {a.status}
      {a.status === 'running' &&
        a.phase &&
        ` (${a.phase}${a.progress != null ? ` ${Math.round(a.progress * 100)}%` : ''})`}
      {a.error_code && <span className="text-destructive"> {a.error_code}</span>}
      {enc && (
        <span className="text-muted-foreground">
          {', '}
          {enc.stage}
          {enc.fallbacks?.length ? ` after ${enc.fallbacks.join(', ')} failed` : ''}
        </span>
      )}
    </span>
  )
}
