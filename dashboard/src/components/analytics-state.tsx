import { useQuery } from '@tanstack/react-query'
import { ChartLineIcon } from 'lucide-react'
import * as React from 'react'
import type { Analytics, Range } from '@/api'
import { QueryState } from '@/components/query-state'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { Skeleton } from '@/components/ui/skeleton'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
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

/** Until the analytics API lands (D100) this is what Analytics says: never sample data. */
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
