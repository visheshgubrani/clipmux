import { useCallback, useEffect, useState } from 'react'
import { Link, useSearchParams } from 'react-router'
import { api } from '../api.ts'
import { Card, CardTitle, Muted } from '../components/ui/card.tsx'
import { Table, Td, Th } from '../components/ui/table.tsx'

interface Attempt {
  id: string
  number: number
  executor: string
  agent_name?: string
  status: string
  error_code?: string
  phase?: string
  progress?: number
  encoder?: { stage: string; fallbacks?: string[] }
}

interface Job {
  id: string
  asset_id: string
  kind: string
  status: string
  phase?: string
  progress?: number
  pinned_agent_id?: string
  lost_count: number
  failed_count: number
  error_code?: string
  created_at: string
  attempts: Attempt[]
}

const where = (a: Attempt) => (a.executor === 'agent' ? (a.agent_name ?? 'PC agent') : a.executor)

function AttemptLine({ a }: { a: Attempt }) {
  const enc = a.encoder
  return (
    <div className="text-xs">
      #{a.number} on <span className="font-medium">{where(a)}</span> — {a.status}
      {a.status === 'running' &&
        a.phase &&
        ` (${a.phase}${a.progress != null ? ` ${Math.round(a.progress * 100)}%` : ''})`}
      {a.error_code && <span className="text-red-600"> {a.error_code}</span>}
      {enc && (
        <span className="text-zinc-500">
          {' '}
          · {enc.stage}
          {enc.fallbacks?.length ? ` after ${enc.fallbacks.join(', ')} failed` : ''}
        </span>
      )}
    </div>
  )
}

// Jobs is where each job ran, each attempt, and how it ended
// (docs/architecture.md, "Routing and fallback").
export function Jobs() {
  const [params] = useSearchParams()
  const [jobs, setJobs] = useState<Job[]>([])
  const filter = params.toString()
  const load = useCallback(async () => {
    setJobs((await api<{ data: Job[] }>(`/v1/jobs${filter ? `?${filter}` : ''}`)).data)
  }, [filter])
  useEffect(() => {
    void load()
  }, [load])
  // Follow running jobs.
  useEffect(() => {
    if (!jobs.some((j) => j.status === 'queued' || j.status === 'running')) return
    const t = setTimeout(() => void load(), 3000)
    return () => clearTimeout(t)
  }, [jobs, load])
  return (
    <Card className="grid gap-4">
      <CardTitle>Jobs</CardTitle>
      <Muted>
        Every encode, re-encode, move and delete. A job that lost its worker (a PC asleep, a crash) runs again
        on a new attempt; a job pinned to a PC waits for it.
      </Muted>
      {jobs.length === 0 ? (
        <Muted>No jobs yet.</Muted>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Job</Th>
              <Th>Status</Th>
              <Th>Attempts</Th>
            </tr>
          </thead>
          <tbody>
            {jobs.map((j) => (
              <tr key={j.id} className="align-top">
                <Td>
                  <div className="font-medium">{j.kind}</div>
                  <Link className="font-mono text-xs underline" to={`/assets/${j.asset_id}`}>
                    {j.asset_id}
                  </Link>
                  <div className="text-xs text-zinc-500">{new Date(j.created_at).toLocaleString()}</div>
                </Td>
                <Td>
                  <div
                    className={
                      j.status === 'failed'
                        ? 'text-red-600'
                        : j.status === 'succeeded'
                          ? 'text-green-700'
                          : ''
                    }
                  >
                    {j.status}
                    {j.status === 'queued' && j.pinned_agent_id ? ' (waiting for its PC)' : ''}
                  </div>
                  {j.error_code && <div className="text-xs text-red-600">{j.error_code}</div>}
                  {(j.lost_count > 0 || j.failed_count > 0) && (
                    <div className="text-xs text-zinc-500">
                      {j.lost_count} lost, {j.failed_count} failed
                    </div>
                  )}
                </Td>
                <Td>
                  {j.attempts.length === 0 ? (
                    <span className="text-xs text-zinc-500">not claimed yet</span>
                  ) : (
                    j.attempts.map((a) => <AttemptLine key={a.id} a={a} />)
                  )}
                </Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
    </Card>
  )
}
