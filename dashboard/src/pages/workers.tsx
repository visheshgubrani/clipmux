import { useCallback, useEffect, useState } from 'react'
import { Link, useSearchParams } from 'react-router'
import { ApiError, api, post } from '../api.ts'
import { Button } from '../components/ui/button.tsx'
import { Card, CardTitle, Muted } from '../components/ui/card.tsx'
import { Input } from '../components/ui/input.tsx'
import { Table, Td, Th } from '../components/ui/table.tsx'
import { ProvidersCard, RoutingCard } from './routing.tsx'

interface Agent {
  id: string
  name: string
  status: string
  scope: string
  online: boolean
  sandboxed: boolean
  hostname: string
  platform: string
  last_ip?: string
  last_seen_at: string | null
  capabilities: {
    ffmpeg_version?: string
    packager_version?: string
    encoders?: string[]
    speech?: { backends?: string[]; model?: string }
  }
}

interface Invitation {
  code: string
  expires_at: string
  command: string
  docker_command: string
}

interface Pairing {
  status: 'pending' | 'approved' | 'denied' | 'consumed' | 'expired'
  agent_started: boolean
  name: string
  hostname: string
  platform: string
  ip?: string
  agent_id?: string
  expires_at: string
}

const ago = (iso: string | null) => {
  if (!iso) return 'never'
  const s = Math.max(0, Math.round((Date.now() - Date.parse(iso)) / 1000))
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.round(s / 60)} min ago`
  return new Date(iso).toLocaleString()
}

function Copyable({ value }: { value: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="flex items-start gap-2">
      <pre className="flex-1 overflow-x-auto whitespace-pre-wrap break-all rounded bg-zinc-900 p-3 font-mono text-xs text-zinc-100">
        {value}
      </pre>
      <Button
        variant="outline"
        size="sm"
        type="button"
        onClick={async () => {
          await navigator.clipboard.writeText(value)
          setCopied(true)
          setTimeout(() => setCopied(false), 1500)
        }}
      >
        {copied ? 'Copied' : 'Copy'}
      </Button>
    </div>
  )
}

// ConnectCard is "Connect my PC" (docs/architecture.md, "Optional upgrades"):
// one command for the PC, which shows up here the moment it runs.
function ConnectCard({ onConnected }: { onConnected: () => void }) {
  const [inv, setInv] = useState<Invitation | null>(null)
  const [docker, setDocker] = useState(false)
  const [pairing, setPairing] = useState<Pairing | null>(null)
  const server = window.location.origin

  // Watch the code until the PC redeems it.
  useEffect(() => {
    if (!inv || pairing?.status === 'consumed' || pairing?.status === 'expired') return
    const t = setTimeout(async () => {
      const p = await api<Pairing>(`/v1/agents/pairings/${inv.code}`).catch(() => null)
      setPairing(p)
      if (p?.status === 'consumed') onConnected()
    }, 2000)
    return () => clearTimeout(t)
  }, [inv, pairing, onConnected])

  if (!inv) {
    return (
      <Card className="grid gap-3">
        <CardTitle>Connect a PC</CardTitle>
        <Muted>
          Let a computer you own encode for this server, with its GPU if it has one. On the PC you're using
          right now, run this and approve it in the browser tab it opens:
        </Muted>
        <Copyable value={`clipmux worker --server ${server}`} />
        <Muted>For another machine, or one without a browser, create a one-time command instead.</Muted>
        <Button
          className="justify-self-start"
          type="button"
          variant="outline"
          onClick={async () => setInv(await post<Invitation>('/v1/agents/pairings'))}
        >
          Create a command
        </Button>
      </Card>
    )
  }
  const done = pairing?.status === 'consumed'
  return (
    <Card className="grid gap-3">
      <CardTitle>{done ? 'PC connected' : 'Run this on the PC'}</CardTitle>
      {done ? (
        <Muted>It's listed below and starts taking jobs now.</Muted>
      ) : (
        <>
          <div className="flex gap-4 text-sm">
            <button
              type="button"
              className={docker ? 'text-zinc-500' : 'font-semibold'}
              onClick={() => setDocker(false)}
            >
              Linux
            </button>
            <button
              type="button"
              className={docker ? 'font-semibold' : 'text-zinc-500'}
              onClick={() => setDocker(true)}
            >
              Docker (Windows, macOS)
            </button>
          </div>
          <Copyable value={docker ? inv.docker_command : inv.command} />
          {docker && (
            <Muted>
              With a GPU, add <code>--gpus all</code> after <code>docker run</code> for NVIDIA (with the
              NVIDIA Container Toolkit), or{' '}
              <code>--device /dev/dri --group-add "$(getent group render | cut -d: -f3)"</code> for AMD or
              Intel on Linux. Docker Desktop on a Mac encodes on the CPU.
            </Muted>
          )}
          <Muted>
            {pairing?.status === 'expired'
              ? 'This code expired. Create a new command.'
              : `The code works once, until ${new Date(inv.expires_at).toLocaleTimeString()}. Waiting for the PC…`}
          </Muted>
        </>
      )}
      <Button
        className="justify-self-start"
        variant="outline"
        size="sm"
        type="button"
        onClick={() => {
          setInv(null)
          setPairing(null)
        }}
      >
        {done ? 'Connect another' : 'Cancel'}
      </Button>
    </Card>
  )
}

export function Workers() {
  const [agents, setAgents] = useState<Agent[]>([])
  const load = useCallback(async () => setAgents((await api<{ data: Agent[] }>('/v1/agents')).data), [])
  useEffect(() => {
    void load()
    const t = setInterval(() => void load(), 10000)
    return () => clearInterval(t)
  }, [load])
  const [notice, setNotice] = useState('')
  const revoke = async (a: Agent) => {
    if (!confirm(`Revoke ${a.name}? It stops at once, and its running job moves to another worker.`)) return
    const res = await api<{ fenced_attempts: number; current_outputs: number }>(`/v1/agents/${a.id}`, {
      method: 'DELETE',
    })
    setNotice(
      res.current_outputs > 0
        ? `${a.name} is revoked. ${res.current_outputs} video(s) currently play an output it encoded; re-encode them if you no longer trust it.`
        : `${a.name} is revoked.`,
    )
    void load()
  }
  const reencode = async (a: Agent) => {
    if (!confirm(`Re-encode every video whose current version ${a.name} encoded?`)) return
    const res = await post<{ queued: string[]; skipped: Record<string, string> }>(
      `/v1/agents/${a.id}/reencode`,
    )
    const skipped = Object.keys(res.skipped).length
    setNotice(
      `Queued ${res.queued.length} re-encode(s).` +
        (skipped
          ? ` ${skipped} couldn't be (their source stayed on that PC, or another job is running).`
          : ''),
    )
  }
  const setScope = async (a: Agent, scope: string) => {
    await api(`/v1/agents/${a.id}`, { method: 'PATCH', body: JSON.stringify({ scope }) })
    void load()
  }
  return (
    <>
      <RoutingCard />
      <ConnectCard onConnected={load} />
      <Card className="grid gap-4">
        <CardTitle>Workers</CardTitle>
        <Muted>
          Connected PCs take jobs alongside this server's built-in worker, if it's on. See where each job ran
          on the{' '}
          <Link to="/jobs" className="underline">
            Jobs
          </Link>{' '}
          page.
        </Muted>
        {notice && <p className="rounded bg-amber-50 p-2 text-sm">{notice}</p>}
        {agents.length === 0 ? (
          <Muted>No PCs connected yet.</Muted>
        ) : (
          <Table>
            <thead>
              <tr>
                <Th>PC</Th>
                <Th>Status</Th>
                <Th>Encoders</Th>
                <Th>Speech</Th>
                <Th>Sandbox</Th>
                <Th>Takes</Th>
                <Th />
              </tr>
            </thead>
            <tbody>
              {agents.map((a) => (
                <tr key={a.id}>
                  <Td>
                    <div className="font-medium">{a.name}</div>
                    <div className="text-xs text-zinc-500">
                      {[a.hostname, a.platform, a.last_ip].filter(Boolean).join(' · ')}
                    </div>
                  </Td>
                  <Td>
                    {a.status === 'revoked' ? (
                      <span className="text-zinc-500">revoked</span>
                    ) : (
                      <span className={a.online ? 'text-green-700' : 'text-zinc-500'}>
                        {a.online ? 'online' : `offline, seen ${ago(a.last_seen_at)}`}
                      </span>
                    )}
                  </Td>
                  <Td className="text-xs">{a.capabilities.encoders?.join(', ') ?? '—'}</Td>
                  <Td className="text-xs" title={a.capabilities.speech?.model ?? ''}>
                    {a.capabilities.speech?.backends?.length
                      ? a.capabilities.speech.backends.join(', ')
                      : '—'}
                  </Td>
                  <Td className="text-xs">{a.sandboxed ? 'sandboxed' : 'not sandboxed'}</Td>
                  <Td>
                    {a.status !== 'revoked' && (
                      <select
                        className="rounded border border-zinc-300 px-1 py-0.5 text-xs"
                        value={a.scope}
                        onChange={(e) => void setScope(a, e.target.value)}
                        title={
                          a.sandboxed ? '' : 'Without a sandbox it only takes your own uploads and local jobs'
                        }
                      >
                        <option value="all">{a.sandboxed ? 'any job' : 'your uploads'}</option>
                        <option value="local">its local files only</option>
                      </select>
                    )}
                  </Td>
                  <Td>
                    {a.status !== 'revoked' ? (
                      <Button variant="ghost" size="sm" type="button" onClick={() => void revoke(a)}>
                        Revoke
                      </Button>
                    ) : (
                      <Button variant="ghost" size="sm" type="button" onClick={() => void reencode(a)}>
                        Re-encode its videos
                      </Button>
                    )}
                  </Td>
                </tr>
              ))}
            </tbody>
          </Table>
        )}
      </Card>
      <ProvidersCard />
    </>
  )
}

// ConnectPC is where `clipmux worker --server URL` sends the owner: the
// machine shows a code, the page shows the same code and what the machine
// said about itself, and one click connects it.
export function ConnectPC() {
  const [params] = useSearchParams()
  const [code, setCode] = useState(params.get('code') ?? '')
  const [pairing, setPairing] = useState<Pairing | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const lookup = useCallback(async (c: string) => {
    setError('')
    try {
      setPairing(await api<Pairing>(`/v1/agents/pairings/${encodeURIComponent(c)}`))
    } catch (e) {
      setPairing(null)
      setError(
        e instanceof ApiError && e.status === 404 ? 'No PC is waiting with that code.' : (e as Error).message,
      )
    }
  }, [])
  useEffect(() => {
    if (params.get('code')) void lookup(params.get('code') ?? '')
  }, [params, lookup])

  const decide = async (approve: boolean) => {
    setBusy(true)
    try {
      setPairing(
        await post<Pairing>(
          `/v1/agents/pairings/${encodeURIComponent(code)}/${approve ? 'approve' : 'deny'}`,
        ),
      )
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (!pairing) {
    return (
      <Card className="mx-auto grid w-full max-w-md gap-3">
        <CardTitle>Connect a PC</CardTitle>
        <Muted>Enter the code your PC shows.</Muted>
        <form
          className="flex gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            void lookup(code)
          }}
        >
          <Input value={code} onChange={(e) => setCode(e.target.value)} placeholder="ABCD-EFGH" required />
          <Button type="submit">Continue</Button>
        </form>
        {error && <p className="text-sm text-red-600">{error}</p>}
      </Card>
    )
  }
  const waiting = pairing.status === 'pending' && pairing.agent_started
  return (
    <Card className="mx-auto grid w-full max-w-md gap-4">
      <CardTitle>
        {waiting
          ? 'Connect this PC?'
          : pairing.status === 'approved' || pairing.status === 'consumed'
            ? 'Connected'
            : pairing.status === 'denied'
              ? 'Not connected'
              : 'This code expired'}
      </CardTitle>
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-zinc-500">Code</dt>
        <dd className="font-mono">{code.toUpperCase()}</dd>
        <dt className="text-zinc-500">Name</dt>
        <dd>{pairing.name}</dd>
        {pairing.hostname && (
          <>
            <dt className="text-zinc-500">Machine</dt>
            <dd>
              {pairing.hostname} ({pairing.platform})
            </dd>
          </>
        )}
        {pairing.ip && (
          <>
            <dt className="text-zinc-500">Address</dt>
            <dd>{pairing.ip}</dd>
          </>
        )}
      </dl>
      {waiting ? (
        <>
          <Muted>
            Check the code matches the one in your terminal. Once connected, this PC can download sources and
            upload encoded videos for this server.
          </Muted>
          <div className="flex gap-2">
            <Button type="button" disabled={busy} onClick={() => void decide(true)}>
              Connect
            </Button>
            <Button type="button" variant="outline" disabled={busy} onClick={() => void decide(false)}>
              Deny
            </Button>
          </div>
        </>
      ) : pairing.status === 'approved' || pairing.status === 'consumed' ? (
        <Muted>
          The PC picks this up within a couple of seconds and starts taking jobs. You can close this tab, or
          see it on the{' '}
          <Link to="/workers" className="underline">
            Workers
          </Link>{' '}
          page.
        </Muted>
      ) : (
        <Muted>
          Run <code>clipmux worker</code> on the PC again for a new code.
        </Muted>
      )}
      {error && <p className="text-sm text-red-600">{error}</p>}
    </Card>
  )
}
