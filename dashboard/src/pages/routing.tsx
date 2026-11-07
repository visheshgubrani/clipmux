import { type FormEvent, useCallback, useEffect, useState } from 'react'
import { ApiError, api, post } from '../api.ts'
import { Button } from '../components/ui/button.tsx'
import { Card, CardTitle, Muted } from '../components/ui/card.tsx'
import { Input, Label } from '../components/ui/input.tsx'

// Routing and serverless providers (docs/architecture.md, "Routing and
// fallback"; docs/executors.md; D053–D058): where jobs run, which provider
// takes them when the primary is away, and what that may cost.

interface Routing {
  primary_executor: string
  fallback_executor: string | null
  queue_age_fallback_minutes: number | null
  builtin_worker: { last_seen_at?: string; online: boolean }
  providers_ready: string[]
}

interface Provider {
  kind: string
  connected: boolean
  verified: boolean
  config: { image?: string; gpu?: string; gpu_type_ids?: string[] }
  max_concurrent: number
  monthly_limit_cents: number
  rate_cents_per_hour: number
  month_cents: number
  machines: number
  backoff_until?: string
}

interface Step {
  name: string
  ok: boolean
  detail?: string
}

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

const providerNames: Record<string, string> = { runpod: 'RunPod', modal: 'Modal' }

const dollars = (cents: number) => `$${(cents / 100).toFixed(2)}`

const selectClass = 'h-9 w-full rounded-md border border-zinc-300 bg-white px-2 text-sm'

// RoutingCard chooses the primary executor and the fallback.
export function RoutingCard() {
  const [r, setR] = useState<Routing | null>(null)
  const [error, setError] = useState('')
  const [saved, setSaved] = useState(false)
  const load = useCallback(async () => setR(await api<Routing>('/v1/settings/routing')), [])
  useEffect(() => {
    void load()
  }, [load])
  if (!r) return null

  const save = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    const form = new FormData(e.currentTarget)
    const fallback = (form.get('fallback') as string) || null
    const queueAge = form.get('queue_age_on') ? Number(form.get('queue_age_minutes')) : null
    setError('')
    setSaved(false)
    try {
      setR(
        await api<Routing>('/v1/settings/routing', {
          method: 'PUT',
          body: JSON.stringify({
            primary_executor: form.get('primary'),
            fallback_executor: fallback,
            queue_age_fallback_minutes: queueAge,
          }),
        }),
      )
      setSaved(true)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err))
    }
  }

  const primaries = ['workers', 'agents', 'builtin', ...r.providers_ready]
  const fallbacks = ['builtin', ...r.providers_ready]
  return (
    <Card className="grid gap-4">
      <CardTitle>Routing</CardTitle>
      <Muted>
        Jobs go to the primary. When none of its workers that could take a job has been seen for a minute, the
        job goes to the fallback, unless an earlier attempt on that video failed in a way that may be the
        file's fault. A cloud provider appears here once it is connected and its setup test has passed.
      </Muted>
      <p className="text-sm">
        This server's worker is{' '}
        {r.builtin_worker.online ? (
          <span className="text-green-700">online</span>
        ) : (
          <span className="text-zinc-500">offline (off, or not seen for a minute)</span>
        )}
        .
      </p>
      <form className="grid gap-3 sm:grid-cols-2" onSubmit={save} key={JSON.stringify(r)}>
        <Label>
          Primary
          <select
            name="primary"
            aria-label="Primary executor"
            className={selectClass}
            defaultValue={r.primary_executor}
          >
            {primaries.map((p) => (
              <option key={p} value={p}>
                {executorNames[p] ?? p}
              </option>
            ))}
          </select>
        </Label>
        <Label>
          Fallback
          <select
            name="fallback"
            aria-label="Fallback executor"
            className={selectClass}
            defaultValue={r.fallback_executor ?? ''}
          >
            <option value="">none: wait for the primary</option>
            {fallbacks.map((p) => (
              <option key={p} value={p}>
                {executorNames[p] ?? p}
              </option>
            ))}
          </select>
        </Label>
        <label className="flex items-center gap-2 text-sm sm:col-span-2">
          <input
            type="checkbox"
            name="queue_age_on"
            aria-label="Queue-age fallback"
            defaultChecked={r.queue_age_fallback_minutes != null}
          />
          Also send a job to the fallback when it has waited this many minutes for a busy primary:
          <Input
            name="queue_age_minutes"
            type="number"
            min={1}
            max={1440}
            aria-label="Queue-age minutes"
            className="w-20"
            defaultValue={r.queue_age_fallback_minutes ?? 10}
          />
        </label>
        <div className="sm:col-span-2">
          <Button type="submit">Save routing</Button>
          {saved && <span className="ml-3 text-sm text-green-700">Saved.</span>}
        </div>
      </form>
      {error && <p className="text-sm text-red-700">{error}</p>}
    </Card>
  )
}

const stepNames: Record<string, string> = {
  credentials: 'Credentials',
  public_url: 'Public URL reachable',
  image: 'Worker image',
  registry: 'Registry access',
  saved: 'Saved',
  setup_test: 'Setup test',
  transcription: 'Sample transcription',
  structured_output: 'Sample chapters',
}

export function Steps({ steps }: { steps: Step[] }) {
  if (steps.length === 0) return null
  return (
    <ol className="grid gap-1 text-sm">
      {steps.map((s) => (
        <li key={s.name} className={s.ok ? 'text-zinc-700' : 'text-red-700'}>
          {s.ok ? '✓' : '✗'} {stepNames[s.name] ?? s.name}
          {s.detail ? <span className="text-zinc-500"> — {s.detail}</span> : null}
        </li>
      ))}
    </ol>
  )
}

// The fields every provider asks for: registry access, limits and rate.
function CommonFields() {
  return (
    <>
      <Input name="image" placeholder="Worker image (default: the one this server was built from)" />
      <div className="grid gap-3 sm:grid-cols-2">
        <Input name="registry_username" placeholder="Registry username (private image)" autoComplete="off" />
        <Input
          name="registry_password"
          type="password"
          placeholder="Registry token with read:packages"
          autoComplete="off"
        />
      </div>
      <div className="grid gap-3 sm:grid-cols-3">
        <Label>
          At most this many machines
          <Input name="max_concurrent" type="number" min={1} max={100} defaultValue={1} required />
        </Label>
        <Label>
          Monthly limit ($)
          <Input name="monthly_limit" type="number" min={1} step="0.01" required />
        </Label>
        <Label>
          Rate ($ per machine-hour)
          <Input name="rate" type="number" min={0} step="0.01" required />
        </Label>
      </div>
    </>
  )
}

const common = (form: FormData) => ({
  image: form.get('image') || undefined,
  registry_username: form.get('registry_username') || undefined,
  registry_password: form.get('registry_password') || undefined,
  max_concurrent: Number(form.get('max_concurrent')),
  monthly_limit_cents: Math.round(Number(form.get('monthly_limit')) * 100),
  rate_cents_per_hour: Math.round(Number(form.get('rate')) * 100),
})

function ConnectForm({ kind, onDone }: { kind: 'runpod' | 'modal'; onDone: () => void }) {
  const [steps, setSteps] = useState<Step[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const connect = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    const el = e.currentTarget
    const form = new FormData(el)
    const body =
      kind === 'runpod'
        ? { api_key: form.get('api_key'), ...common(form) }
        : { token_id: form.get('token_id'), token_secret: form.get('token_secret'), ...common(form) }
    setBusy(true)
    setError('')
    setSteps([])
    try {
      const res = await post<{ connected: boolean; steps: Step[] }>(`/v1/executors/${kind}/connect`, body)
      setSteps(res.steps)
      if (res.connected) el.reset()
      onDone()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <details className="rounded-md border border-zinc-200 p-3">
      <summary className="cursor-pointer text-sm font-medium">Connect {providerNames[kind]}</summary>
      <form className="mt-3 grid gap-3" onSubmit={connect} aria-label={`Connect ${providerNames[kind]}`}>
        {kind === 'runpod' ? (
          <Input
            name="api_key"
            type="password"
            placeholder="RunPod API key"
            aria-label="RunPod API key"
            required
          />
        ) : (
          <div className="grid gap-3 sm:grid-cols-2">
            <Input name="token_id" placeholder="Modal token ID" aria-label="Modal token ID" required />
            <Input
              name="token_secret"
              type="password"
              placeholder="Modal token secret"
              aria-label="Modal token secret"
              required
            />
          </div>
        )}
        <CommonFields />
        <div>
          <Button type="submit" disabled={busy}>
            {busy ? 'Connecting…' : `Connect ${providerNames[kind]}`}
          </Button>
        </div>
      </form>
      {error && <p className="mt-2 text-sm text-red-700">{error}</p>}
      <div className="mt-2">
        <Steps steps={steps} />
      </div>
    </details>
  )
}

// ProvidersCard is Connect RunPod and Connect Modal, with each provider's
// state and this month's estimated spend.
export function ProvidersCard() {
  const [providers, setProviders] = useState<Provider[]>([])
  const [error, setError] = useState('')
  const load = useCallback(
    async () => setProviders((await api<{ data: Provider[] }>('/v1/executors')).data),
    [],
  )
  useEffect(() => {
    void load()
    const t = setInterval(() => void load(), 10000)
    return () => clearInterval(t)
  }, [load])
  const disconnect = async (kind: string) => {
    if (!confirm(`Disconnect ${providerNames[kind]}? Its credentials are forgotten.`)) return
    setError('')
    try {
      await api(`/v1/executors/${kind}`, { method: 'DELETE' })
      void load()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err))
    }
  }
  const connected = providers.filter((p) => p.connected)
  return (
    <Card className="grid gap-4">
      <CardTitle>Cloud GPUs</CardTitle>
      <Muted>
        A provider rents a GPU machine for one job at a time, as the primary or as the fallback. Spend is an
        estimate from the rate you enter and each machine's running time, not the provider's bill; a job
        reserves its estimated run before its machine starts. See docs/executors.md.
      </Muted>
      {connected.map((p) => (
        <div
          key={p.kind}
          className="grid gap-1 rounded-md border border-zinc-200 p-3 text-sm"
          data-provider={p.kind}
        >
          <div className="flex items-center justify-between">
            <span className="font-medium">{providerNames[p.kind] ?? p.kind}</span>
            <Button variant="outline" size="sm" onClick={() => void disconnect(p.kind)}>
              Disconnect
            </Button>
          </div>
          <div>
            {p.verified ? (
              <span className="text-green-700">ready</span>
            ) : (
              <span className="text-amber-700">waiting for its setup test to pass</span>
            )}
            {p.backoff_until && (
              <span className="text-red-700">
                {' '}
                · backing off after failed starts until {new Date(p.backoff_until).toLocaleTimeString()}
              </span>
            )}
          </div>
          <div className="text-zinc-600">
            {p.machines} of at most {p.max_concurrent} machines running · {dollars(p.month_cents)} of{' '}
            {dollars(p.monthly_limit_cents)} this month · {dollars(p.rate_cents_per_hour)} per machine-hour
          </div>
          {p.config.image && (
            <div className="break-all font-mono text-xs text-zinc-500">{p.config.image}</div>
          )}
        </div>
      ))}
      {error && <p className="text-sm text-red-700">{error}</p>}
      <ConnectForm kind="runpod" onDone={load} />
      <ConnectForm kind="modal" onDone={load} />
    </Card>
  )
}
