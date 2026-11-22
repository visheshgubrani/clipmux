import { useQuery } from '@tanstack/react-query'
import { ChartLineIcon } from 'lucide-react'
import * as React from 'react'
import { Link } from 'react-router'
import type { Analytics, Range } from '@/api'
import { QueryState } from '@/components/query-state'
import { Alert, AlertAction, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { Skeleton } from '@/components/ui/skeleton'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { bytes } from '@/lib/format'
import { q } from '@/queries'

const AnalyticsView = React.lazy(() => import('@/components/analytics-view'))

export const ranges: { value: Range; label: string }[] = [
  { value: '24h', label: '24 h' },
  { value: '7d', label: '7 days' },
  { value: '30d', label: '30 days' },
  { value: '90d', label: '90 days' },
]

export function RangePicker({ value, onChange }: { value: Range; onChange: (r: Range) => void }) {
  return (
    <ToggleGroup
      aria-label="Range"
      variant="outline"
      size="sm"
      value={[value]}
      onValueChange={(v: string[]) => v[0] && onChange(v[0] as Range)}
    >
      {ranges.map((r) => (
        <ToggleGroupItem key={r.value} value={r.value}>
          {r.label}
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
  )
}

/** A server older than the analytics API (D100) says this: never sample data. */
export function NotCollecting() {
  return (
    <Empty className="border border-dashed bg-card">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <ChartLineIcon />
        </EmptyMedia>
        <EmptyTitle>Analytics isn't collecting yet</EmptyTitle>
        <EmptyDescription>
          Once it's on, ClipMux counts views, watch time and bandwidth from the playback it serves. Nothing
          goes to a third party. Unique viewers appear when your app puts a viewer ID in its tokens.
        </EmptyDescription>
      </EmptyHeader>
    </Empty>
  )
}

/**
 * With Cloudflare delivery the Worker counts playback, but nothing pulls its
 * counts in until edge analytics is connected: say so instead of showing
 * quiet zeros.
 */
export function EdgeNotCounted() {
  return (
    <Alert>
      <AlertTitle>Playback through Cloudflare isn’t counted yet</AlertTitle>
      <AlertDescription>
        Your videos stream from Cloudflare’s edge, so these numbers miss most views. Connect edge analytics
        with a read-only token to bring them in.
      </AlertDescription>
      <AlertAction>
        <Button size="sm" variant="outline" render={<Link to="/delivery#edge-analytics" />}>
          Connect edge analytics
        </Button>
      </AlertAction>
    </Alert>
  )
}

export function EdgePullFailed({ error }: { error: string }) {
  return (
    <Alert variant="destructive">
      <AlertTitle>Cloudflare edge analytics pull failed</AlertTitle>
      <AlertDescription>
        Counts from Cloudflare’s edge are stale ({error}). Check the edge analytics token on Delivery.
      </AlertDescription>
      <AlertAction>
        <Button size="sm" variant="outline" render={<Link to="/delivery#edge-analytics" />}>
          Check Delivery
        </Button>
      </AlertAction>
    </Alert>
  )
}

export function EdgeRedeployNeeded() {
  return (
    <Alert>
      <AlertTitle>Update Cloudflare Worker for accurate edge counts</AlertTitle>
      <AlertDescription>
        A newer Worker fixes edge view sampling and isolates this install’s counts. Redeploy Cloudflare
        delivery to update it.
      </AlertDescription>
      <AlertAction>
        <Button size="sm" variant="outline" render={<Link to="/delivery" />}>
          Update Worker
        </Button>
      </AlertAction>
    </Alert>
  )
}

const chartSkeleton = <Skeleton className="h-[22rem] w-full rounded-xl" />

/** The chart panel for a range, an app or one video, with every state. */
export function AnalyticsPanel({
  range,
  app,
  asset,
  children,
}: {
  range: Range
  app?: string
  asset?: string
  children?: (data: Analytics) => React.ReactNode
}) {
  const query = useQuery(q.analytics(range, app, asset))
  return (
    <QueryState query={query} what="analytics" skeleton={chartSkeleton}>
      {(data) =>
        data === null ? (
          <NotCollecting />
        ) : (
          <div className="flex flex-col gap-8">
            {data.collecting?.edge === 'not_connected' && <EdgeNotCounted />}
            {data.collecting?.edge === 'connected' && data.collecting.edge_error && (
              <EdgePullFailed error={data.collecting.edge_error} />
            )}
            {data.collecting?.edge !== 'not_used' && data.collecting?.edge_needs_redeploy && (
              <EdgeRedeployNeeded />
            )}
            {data.storage && (
              <div className="flex flex-wrap items-center justify-between gap-4 rounded-xl border bg-card px-4 py-3">
                <div className="flex flex-col gap-0.5">
                  <span className="text-xs text-muted-foreground">Storage taken</span>
                  <span className="type-timecode text-lg font-semibold">
                    {bytes(data.storage.total_bytes)}
                  </span>
                </div>
                <div className="flex flex-wrap items-center gap-x-6 gap-y-1 text-sm text-muted-foreground">
                  <span>Originals: {bytes(data.storage.source_bytes)}</span>
                  <span>Encoded &amp; tracks: {bytes(data.storage.media_bytes)}</span>
                  {!asset && (
                    <Button size="sm" variant="outline" render={<Link to="/storage" />}>
                      Manage storage
                    </Button>
                  )}
                </div>
              </div>
            )}
            <React.Suspense fallback={chartSkeleton}>
              <AnalyticsView data={data} range={range} />
            </React.Suspense>
            {children?.(data)}
          </div>
        )
      }
    </QueryState>
  )
}
