import { useCallback, useEffect, useState } from 'react'
import { Link, useSearchParams } from 'react-router'
import { api } from '../api.ts'
import { Card, CardTitle, Muted } from '../components/ui/card.tsx'
import { Table, Td, Th } from '../components/ui/table.tsx'
import { executorNames } from './routing.tsx'

interface Attempt {
  id: string
  number: number
  executor: string
  route_reason?: string
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
  routed_to?: string
  route_reason?: string
  created_at: string
  attempts: Attempt[]
}

const where = (a: Attempt) =>
  a.executor === 'agent' ? (a.agent_name ?? 'PC agent') : (executorNames[a.executor] ?? a.executor)

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

function AttemptLine({ a }: { a: Attempt }) {
  const enc = a.encoder
  return (
    <div className="text-xs">
      #{a.number} on <span className="font-medium">{where(a)}</span>
      {a.route_reason && a.route_reason !== 'primary' && (
        <span className="text-zinc-500"> ({reasons[a.route_reason] ?? a.route_reason})</span>
      )}{' '}
      — {a.status}
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
  // Follow running jobs closely, and pick up new ones (an upload, a setup
  // test) even while nothing runs.
  useEffect(() => {
    const active = jobs.some((j) => j.status === 'queued' || j.status === 'running')
    const t = setTimeout(() => void load(), active ? 3000 : 10000)
    return () => clearTimeout(t)
  }, [jobs, load])
  return (
    <Card className="grid gap-4">
      <CardTitle>Jobs</CardTitle>
      <Muted>
        Every encode, re-encode, upload, move and delete. A job that lost its worker (a PC asleep, a crash)
        runs again on a new attempt, on the fallback if the primary is still away; a job pinned to a PC waits
        for it.
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
                  {j.status === 'queued' && j.routed_to && (
                    <div className="text-xs text-zinc-500" data-route={j.route_reason}>
                      Routed to {executorNames[j.routed_to] ?? j.routed_to}:{' '}
                      {reasons[j.route_reason ?? ''] ?? j.route_reason}
                    </div>
                  )}
                  {(j.lost_count > 0 || j.failed_count > 0) && (
                    <div className="text-xs text-zinc-500">
                      {j.lost_count} lost, {j.failed_count} failed
                    </div>
                  )}
                </Td>
                <Td>
                  {j.attempts.length === 0 ? (
                    <span className="text-xs text-zinc-500">
                      {j.status === 'queued' || j.status === 'running'
                        ? 'not claimed yet'
                        : j.status === 'succeeded'
                          ? 'done by the server, no worker needed'
                          : 'never ran on a worker'}
                    </span>
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
