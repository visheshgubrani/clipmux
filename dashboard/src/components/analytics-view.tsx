import * as React from 'react'
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from 'recharts'
import type { Analytics, AnalyticsTotals, Range } from '@/api'
import { type ChartConfig, ChartContainer, ChartTooltip, ChartTooltipContent } from '@/components/ui/chart'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { bytes, count, hours } from '@/lib/format'
import { cn } from '@/lib/utils'

// The chart half of Analytics (docs/design.md): one panel whose header picks
// the metric and shows each total with its change. Loaded only with the
// pages that chart, so recharts stays out of the main bundle.

type Metric = 'views' | 'watch_seconds' | 'unique_viewers' | 'bytes_delivered'

const metrics: { key: Metric; label: string; format: (n: number) => string }[] = [
  { key: 'views', label: 'Views', format: count },
  { key: 'watch_seconds', label: 'Watch time', format: hours },
  { key: 'unique_viewers', label: 'Unique viewers', format: count },
  { key: 'bytes_delivered', label: 'Bandwidth', format: bytes },
]

function change(now: number | null, before: number | null): { text: string; up: boolean } | null {
  if (now == null || before == null || before === 0) return null
  const pct = Math.round(((now - before) / before) * 100)
  return { text: `${pct >= 0 ? '+' : ''}${pct}%`, up: pct >= 0 }
}

const tick = (range: Range) =>
  new Intl.DateTimeFormat(
    undefined,
    range === '24h' ? { hour: 'numeric' } : { month: 'short', day: 'numeric' },
  )

export default function AnalyticsView({
  data,
  range,
  className,
}: {
  data: Analytics
  range: Range
  className?: string
}) {
  const [metric, setMetric] = React.useState<Metric>('views')
  const m = metrics.find((x) => x.key === metric) ?? (metrics[0] as (typeof metrics)[number])
  // The series has no unique viewers per bucket; that metric shows its total only.
  const seriesKey = metric === 'unique_viewers' ? 'views' : metric
  const config = { [seriesKey]: { label: m.label, color: 'var(--chart-1)' } } satisfies ChartConfig
  const fmt = tick(range)
  return (
    <section aria-label="Analytics" className={cn('flex flex-col rounded-xl border bg-card', className)}>
      <ToggleGroup
        aria-label="Metric"
        value={[metric]}
        onValueChange={(v: string[]) => v[0] && setMetric(v[0] as Metric)}
        spacing={0}
        className="grid w-full grid-cols-2 border-b md:grid-cols-4"
      >
        {metrics.map((x) => {
          const total = data.totals[x.key as keyof AnalyticsTotals]
          const c = change(total, data.previous[x.key as keyof AnalyticsTotals])
          return (
            <ToggleGroupItem
              key={x.key}
              value={x.key}
              className="h-auto flex-col items-start gap-1 rounded-none border-0 px-4 py-3 text-left data-[pressed]:bg-muted/60 [&:not(:first-child)]:border-l"
            >
              <span className="text-xs font-normal text-muted-foreground">{x.label}</span>
              <span className="type-timecode text-xl font-semibold">
                {total == null ? '—' : x.format(total)}
              </span>
              <span
                className={cn(
                  'text-xs font-normal',
                  c ? (c.up ? 'text-success' : 'text-muted-foreground') : 'text-muted-foreground',
                )}
              >
                {total == null
                  ? 'Needs a viewer ID in tokens'
                  : c
                    ? `${c.text} vs previous`
                    : 'No earlier data'}
              </span>
            </ToggleGroupItem>
          )
        })}
      </ToggleGroup>
      <div className="p-4">
        <ChartContainer config={config} className="aspect-auto h-64 w-full">
          <AreaChart data={data.series} margin={{ left: 4, right: 8, top: 8 }}>
            <CartesianGrid vertical={false} />
            <XAxis
              dataKey="t"
              tickLine={false}
              axisLine={false}
              minTickGap={32}
              tickFormatter={(t: string) => fmt.format(new Date(t))}
            />
            <YAxis
              tickLine={false}
              axisLine={false}
              width={56}
              tickFormatter={(v: number) => (seriesKey === 'views' ? count(v) : m.format(v))}
            />
            <ChartTooltip
              content={
                <ChartTooltipContent
                  labelFormatter={(_, p) => {
                    const t = (p?.[0]?.payload as { t?: string } | undefined)?.t
                    return t ? fmt.format(new Date(t)) : ''
                  }}
                />
              }
            />
            <Area
              dataKey={seriesKey}
              type="monotone"
              stroke={`var(--color-${seriesKey})`}
              fill={`var(--color-${seriesKey})`}
              fillOpacity={0.12}
              strokeWidth={2}
            />
          </AreaChart>
        </ChartContainer>
      </div>
    </section>
  )
}
