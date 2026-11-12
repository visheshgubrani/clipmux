import { useQuery } from '@tanstack/react-query'
import * as React from 'react'
import { Link } from 'react-router'
import type { Analytics, Range } from '@/api'
import { AnalyticsPanel, RangePicker } from '@/components/analytics-state'
import { PageBody, PageHeader, Section } from '@/components/page'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { bytes, count, hours, timecode } from '@/lib/format'
import { q } from '@/queries'

export function AnalyticsPage() {
  const [range, setRange] = React.useState<Range>('7d')
  const [app, setApp] = React.useState('')
  const apps = useQuery(q.apps())
  return (
    <PageBody>
      <PageHeader
        title="Analytics"
        description="Counted from the playback ClipMux delivers: by this server, and at the edge when Cloudflare serves it."
        actions={
          <>
            {(apps.data?.data.length ?? 0) > 1 && (
              <NativeSelect size="sm" aria-label="App" value={app} onChange={(e) => setApp(e.target.value)}>
                <NativeSelectOption value="">All apps</NativeSelectOption>
                {(apps.data?.data ?? []).map((a) => (
                  <NativeSelectOption key={a.id} value={a.id}>
                    {a.name}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            )}
            <RangePicker value={range} onChange={setRange} />
          </>
        }
      />
      <AnalyticsPanel range={range} {...(app ? { app } : {})}>
        {(data) => <TopVideos data={data} />}
      </AnalyticsPanel>
    </PageBody>
  )
}

function TopVideos({ data }: { data: Analytics }) {
  const rows = data.top_assets ?? []
  if (rows.length === 0) return null
  return (
    <Section title="Top videos">
      <div className="overflow-x-auto rounded-lg border bg-card">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Video</TableHead>
              <TableHead className="text-right">Views</TableHead>
              <TableHead className="text-right">Watch time</TableHead>
              <TableHead className="text-right">Average view</TableHead>
              <TableHead className="text-right">Bandwidth</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={r.asset_id}>
                <TableCell className="max-w-[28rem] truncate font-medium">
                  <Link to={`/videos/${r.asset_id}`}>{r.title || r.asset_id}</Link>
                </TableCell>
                <TableCell className="type-timecode text-right">{count(r.views)}</TableCell>
                <TableCell className="type-timecode text-right">{hours(r.watch_seconds)}</TableCell>
                <TableCell className="type-timecode text-right">
                  {r.views ? timecode(r.watch_seconds / r.views) : '—'}
                </TableCell>
                <TableCell className="type-timecode text-right">{bytes(r.bytes_delivered)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </Section>
  )
}
