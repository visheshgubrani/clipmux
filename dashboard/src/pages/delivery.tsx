import { type FormEvent, useEffect, useState } from 'react'
import { ApiError, api, post } from '../api.ts'
import { Button } from '../components/ui/button.tsx'
import { Card, CardTitle, Muted } from '../components/ui/card.tsx'
import { Input } from '../components/ui/input.tsx'

interface Status {
  connected: boolean
  hostname?: string
  bucket?: string
  account_id?: string
  worker_version?: string
  current_version: string
  needs_redeploy: boolean
  storage_ok: boolean
}

interface Moves {
  default_kind: string
  not_on_default: number
  active: number
  moved: number
  failed: number
}

interface Step {
  name: string
  ok: boolean
  detail?: string
}

const stepNames: Record<string, string> = {
  token: 'Token',
  account: 'Account',
  zone: 'Zone',
  workers_paid: 'Workers Paid',
  bucket: 'R2 bucket',
  storage: 'Storage credentials',
  worker: 'Deploy the Worker',
  routes: 'Publish routes',
  dns: 'Clear the hostname',
  domain: 'Custom domain',
  media_url: 'Media URL',
  saved: 'Saved',
  setup_test: 'Setup test',
}

// Delivery is Connect Cloudflare (docs/architecture.md, "Optional upgrades"):
// one token and a media hostname move playback to the edge.
export function Delivery() {
  const [status, setStatus] = useState<Status | null>(null)
  const [steps, setSteps] = useState<Step[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [moves, setMoves] = useState<Moves | null>(null)
  const load = async () => {
    setStatus(await api<Status>('/v1/cloudflare'))
    setMoves(await api<Moves>('/v1/storage/moves'))
  }
  // Refresh progress while moves run.
  useEffect(() => {
    if (!moves?.active) return
    const t = setTimeout(() => void api<Moves>('/v1/storage/moves').then(setMoves), 5000)
    return () => clearTimeout(t)
  }, [moves])
  const moveLibrary = async () => {
    await post('/v1/storage/moves')
    setMoves(await api<Moves>('/v1/storage/moves'))
  }
  // biome-ignore lint/correctness/useExhaustiveDependencies: load once
  useEffect(() => {
    void load()
  }, [])

  const connect = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    const el = e.currentTarget
    const form = new FormData(el)
    setBusy(true)
    setError('')
    setSteps([])
    try {
      const res = await post<{ connected: boolean; steps: Step[] }>('/v1/cloudflare/connect', {
        token: form.get('token'),
        hostname: form.get('hostname'),
        bucket: form.get('bucket') || undefined,
        account_id: form.get('account_id') || undefined,
      })
      setSteps(res.steps)
      if (res.connected) el.reset()
      await load()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  const reconnect = status?.connected && !status.storage_ok
  const redeploy = status?.connected && status.storage_ok && status.needs_redeploy
  return (
    <div className="grid gap-6">
      <Card className="grid gap-3">
        <CardTitle>Delivery</CardTitle>
        {status?.connected ? (
          <Muted>
            Playback is served by the Cloudflare Worker at <b>{status.hostname}</b> from the R2 bucket{' '}
            <b>{status.bucket}</b>. New uploads are stored in R2; older videos play through this server until
            they are moved.
          </Muted>
        ) : (
          <Muted>
            Playback is served by this server. Connect Cloudflare to serve from R2 at the edge: your apps,
            tokens and playback IDs stay the same.
          </Muted>
        )}
        {reconnect && (
          <p className="rounded bg-red-50 p-3 text-sm text-red-800">
            R2 refuses the stored credentials: the token was probably revoked or rolled. Reconnect Cloudflare
            with a new token.
          </p>
        )}
        {redeploy && (
          <p className="rounded bg-amber-50 p-3 text-sm text-amber-900">
            This version of ClipMux has a newer Worker ({status.current_version}; deployed{' '}
            {status.worker_version}). Paste a token to redeploy it.
          </p>
        )}
      </Card>

      {status?.connected && moves && (moves.not_on_default > 0 || moves.active > 0 || moves.moved > 0) && (
        <Card className="grid gap-3">
          <CardTitle>Move the library to R2</CardTitle>
          <Muted>
            {moves.not_on_default} videos are still stored on this server and play through it. Moving them
            copies each source and output to R2, switches its playback to the edge, then removes the old copy.
          </Muted>
          <p className="text-sm">
            {moves.active} moving · {moves.moved} moved{moves.failed > 0 ? ` · ${moves.failed} failed` : ''}
          </p>
          {moves.not_on_default > moves.active && (
            <div>
              <Button onClick={() => void moveLibrary()}>
                Move {moves.not_on_default - moves.active} videos
              </Button>
            </div>
          )}
        </Card>
      )}

      <Card className="grid gap-4">
        <CardTitle>{status?.connected ? 'Reconnect or redeploy Cloudflare' : 'Connect Cloudflare'}</CardTitle>
        <Muted>
          Create an API token with Workers Scripts, Workers Routes, R2 and DNS edit permissions for the
          account and the media domain's zone. The account needs Workers Paid. ClipMux uses the token during
          this request and keeps only its ID: R2's credentials are derived from it, so revoking or rolling the
          token cuts off storage. Its Workers and DNS permissions stay live in Cloudflare until you narrow or
          revoke it there.
        </Muted>
        <form className="grid gap-3" onSubmit={connect}>
          <Input
            name="token"
            type="password"
            placeholder="Cloudflare API token"
            required
            autoComplete="off"
          />
          <Input
            name="hostname"
            placeholder="Media hostname, e.g. media.example.com"
            required
            defaultValue={status?.hostname ?? ''}
          />
          <Input
            name="bucket"
            placeholder="R2 bucket (default clipmux-media)"
            defaultValue={status?.bucket ?? ''}
          />
          <Input name="account_id" placeholder="Account ID (only if the token reaches several)" />
          <div>
            <Button type="submit" disabled={busy}>
              {busy ? 'Connecting…' : status?.connected ? 'Connect again' : 'Connect Cloudflare'}
            </Button>
          </div>
        </form>
        {error && <p className="text-sm text-red-700">{error}</p>}
        {steps.length > 0 && (
          <ol className="grid gap-1 text-sm">
            {steps.map((s) => (
              <li key={s.name} className={s.ok ? 'text-zinc-700' : 'text-red-700'}>
                {s.ok ? '✓' : '✗'} {stepNames[s.name] ?? s.name}
                {s.detail ? <span className="text-zinc-500"> — {s.detail}</span> : null}
              </li>
            ))}
          </ol>
        )}
        <Muted>
          Prefer not to paste a token? Deploy the Worker yourself with wrangler: see docs/cloudflare.md.
        </Muted>
      </Card>
    </div>
  )
}
