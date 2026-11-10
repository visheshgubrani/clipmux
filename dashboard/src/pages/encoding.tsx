import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import * as React from 'react'
import { Link, useSearchParams } from 'react-router'
import {
  type Agent,
  ApiError,
  api,
  del,
  describe,
  type Invitation,
  type Pairing,
  type Provider,
  post,
  put,
  type Routing,
  type Step,
} from '@/api'
import { useConfirm } from '@/components/confirm'
import { CopyBlock } from '@/components/copy'
import { PageBody, PageHeader, Section } from '@/components/page'
import { QueryState } from '@/components/query-state'
import { Presence } from '@/components/status'
import { Steps } from '@/components/steps'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Item, ItemActions, ItemContent, ItemDescription, ItemTitle } from '@/components/ui/item'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Progress } from '@/components/ui/progress'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { ago, dollars } from '@/lib/format'
import { notify } from '@/lib/notify'
import { executorNames } from '@/pages/jobs'
import { q } from '@/queries'

// Encoding (docs/architecture.md, "Routing and fallback"; docs/executors.md;
// D053–D058): where videos get processed. This server always works; a PC or a
// cloud GPU makes it faster.

const providerNames: Record<string, string> = { runpod: 'RunPod', modal: 'Modal' }

export function Encoding() {
  return (
    <PageBody>
      <PageHeader
        title="Encoding"
        description="Where your videos get processed. This server always works; add a PC or a cloud GPU to go faster."
      />
      <RoutingSection />
      <ComputersSection />
      <CloudSection />
    </PageBody>
  )
}

function RoutingSection() {
  const routing = useQuery(q.routing())
  return (
    <Section
      title="How videos are processed"
      description="Jobs go to the primary. When none of its workers has been seen for a minute, they go to the fallback, unless an earlier attempt on that video failed in a way that may be the file's fault."
    >
      <QueryState query={routing} what="routing" skeleton={<Skeleton className="h-28 w-full" />}>
        {(r) => <RoutingForm r={r} />}
      </QueryState>
    </Section>
  )
}

function RoutingForm({ r }: { r: Routing }) {
  const qc = useQueryClient()
  const [queueAge, setQueueAge] = React.useState(r.queue_age_fallback_minutes != null)
  const [saved, setSaved] = React.useState(false)
  const save = useMutation({
    mutationFn: (body: object) => put<Routing>('/v1/settings/routing', body),
    onSuccess: (next) => {
      qc.setQueryData(['routing'], next)
      setSaved(true)
    },
  })
  const primaries = ['workers', 'agents', 'builtin', ...r.providers_ready]
  const fallbacks = ['builtin', ...r.providers_ready]
  return (
    <form
      className="flex flex-col gap-4 rounded-lg border bg-card p-4"
      onSubmit={(e) => {
        e.preventDefault()
        const f = new FormData(e.currentTarget)
        setSaved(false)
        save.mutate({
          primary_executor: f.get('primary'),
          fallback_executor: (f.get('fallback') as string) || null,
          queue_age_fallback_minutes: queueAge ? Number(f.get('queue_age_minutes')) : null,
        })
      }}
    >
      <FieldGroup className="grid gap-4 sm:grid-cols-2">
        <Field>
          <FieldLabel htmlFor="primary">Send new jobs to</FieldLabel>
          <NativeSelect
            id="primary"
            name="primary"
            aria-label="Primary executor"
            defaultValue={r.primary_executor}
          >
            {primaries.map((p) => (
              <NativeSelectOption key={p} value={p}>
                {executorNames[p] ?? p}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </Field>
        <Field>
          <FieldLabel htmlFor="fallback">When it's away, use</FieldLabel>
          <NativeSelect
            id="fallback"
            name="fallback"
            aria-label="Fallback executor"
            defaultValue={r.fallback_executor ?? ''}
          >
            <NativeSelectOption value="">Nothing: wait for the primary</NativeSelectOption>
            {fallbacks.map((p) => (
              <NativeSelectOption key={p} value={p}>
                {executorNames[p] ?? p}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </Field>
        <Field orientation="horizontal" className="sm:col-span-2">
          <Checkbox
            id="queue-age"
            aria-label="Queue-age fallback"
            checked={queueAge}
            onCheckedChange={(v) => setQueueAge(v === true)}
          />
          <FieldLabel htmlFor="queue-age" className="font-normal">
            Also use the fallback when a job has waited
            <Input
              name="queue_age_minutes"
              type="number"
              min={1}
              max={1440}
              aria-label="Queue-age minutes"
              className="mx-1 inline-flex h-7 w-16"
              defaultValue={r.queue_age_fallback_minutes ?? 10}
            />
            minutes for a busy primary
          </FieldLabel>
        </Field>
      </FieldGroup>
      <p className="text-sm text-muted-foreground">
        A cloud provider appears in these lists once it's connected and its setup test has passed.
      </p>
      {save.isError && <p className="text-sm text-destructive">{describe(save.error)}</p>}
      <div className="flex items-center gap-3">
        <Button type="submit" disabled={save.isPending}>
          {save.isPending && <Spinner data-icon="inline-start" />}
          Save routing
        </Button>
        {saved && <span className="text-sm text-success">Saved.</span>}
      </div>
    </form>
  )
}

function ComputersSection() {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const routing = useQuery(q.routing())
  const agents = useQuery(q.agents())
  const refresh = () => qc.invalidateQueries({ queryKey: ['agents'] })
  const revoke = useMutation({
    mutationFn: (a: Agent) => del<{ fenced_attempts: number; current_outputs: number }>(`/v1/agents/${a.id}`),
    onSuccess: (res, a) => {
      notify.done(
        `${a.name} removed`,
        res.current_outputs > 0
          ? `${res.current_outputs} video(s) currently play an output it encoded; re-encode them if you no longer trust it.`
          : undefined,
      )
      void refresh()
    },
    onError: (err) => notify.failed("Couldn't remove the PC", err),
  })
  const reencode = useMutation({
    mutationFn: (a: Agent) =>
      post<{ queued: string[]; skipped: Record<string, string> }>(`/v1/agents/${a.id}/reencode`),
    onSuccess: (res) => {
      const skipped = Object.keys(res.skipped).length
      notify.done(
        `Queued ${res.queued.length} re-encode${res.queued.length === 1 ? '' : 's'}`,
        skipped
          ? `${skipped} couldn't be: their source stayed on that PC, or another job is running.`
          : undefined,
      )
    },
    onError: (err) => notify.failed("Couldn't queue the re-encodes", err),
  })
  const scope = useMutation({
    mutationFn: ({ a, scope }: { a: Agent; scope: string }) =>
      api(`/v1/agents/${a.id}`, { method: 'PATCH', body: JSON.stringify({ scope }) }),
    onSuccess: () => void refresh(),
    onError: (err) => notify.failed("Couldn't change what it takes", err),
  })
  return (
    <>
      <Section title="This server">
        <Item variant="outline" className="bg-card">
          <ItemContent>
            <ItemTitle>Built-in worker</ItemTitle>
            <ItemDescription>
              Encodes on this server's CPU, and transcribes with whisper.cpp when it can.
            </ItemDescription>
          </ItemContent>
          <ItemActions>
            {routing.data ? (
              <Presence
                on={routing.data.builtin_worker.online}
                label={routing.data.builtin_worker.online ? 'Online' : 'Offline, or not seen for a minute'}
              />
            ) : (
              <Skeleton className="h-4 w-16" />
            )}
          </ItemActions>
        </Item>
      </Section>
      <Section
        title="Your computers"
        description="A PC you own encodes for this server, with its GPU if it has one."
      >
        <ConnectCard onConnected={() => void refresh()} />
        <QueryState query={agents} what="your PCs" skeleton={<Skeleton className="h-16 w-full" />}>
          {(data) =>
            data.data.length === 0 ? null : (
              <ul className="flex flex-col divide-y rounded-lg border bg-card" aria-label="PCs">
                {data.data.map((a) => (
                  <li
                    key={a.id}
                    aria-label={a.name}
                    className="flex flex-wrap items-center gap-x-4 gap-y-2 p-4"
                  >
                    <span className="flex min-w-0 flex-[1_1_16rem] flex-col gap-1">
                      <span className="flex items-center gap-2">
                        <span className="font-medium">{a.name}</span>
                        {a.status === 'revoked' ? (
                          <Badge variant="secondary">Removed</Badge>
                        ) : (
                          <Presence
                            on={a.online}
                            label={a.online ? 'Online' : `Offline, seen ${ago(a.last_seen_at)}`}
                          />
                        )}
                      </span>
                      <span className="text-xs text-muted-foreground">
                        {[a.hostname, a.platform, a.last_ip].filter(Boolean).join(', ')}
                      </span>
                      <span className="text-xs text-muted-foreground">
                        Encoders: {a.capabilities.encoders?.join(', ') ?? '—'}. Speech:{' '}
                        <span title={a.capabilities.speech?.model ?? ''}>
                          {a.capabilities.speech?.backends?.length
                            ? a.capabilities.speech.backends.join(', ')
                            : '—'}
                        </span>
                        . {a.sandboxed ? 'Sandboxed.' : 'Not sandboxed.'}
                      </span>
                    </span>
                    {a.status !== 'revoked' && (
                      <NativeSelect
                        size="sm"
                        aria-label={`What ${a.name} takes`}
                        value={a.scope}
                        onChange={(e) => scope.mutate({ a, scope: e.target.value })}
                        title={
                          a.sandboxed ? '' : 'Without a sandbox it only takes your own uploads and local jobs'
                        }
                      >
                        <NativeSelectOption value="all">
                          {a.sandboxed ? 'Any job' : 'Your uploads'}
                        </NativeSelectOption>
                        <NativeSelectOption value="local">Its own files only</NativeSelectOption>
                      </NativeSelect>
                    )}
                    {a.status !== 'revoked' ? (
                      <Button
                        variant="ghost"
                        size="sm"
                        className="text-destructive"
                        onClick={async () => {
                          const ok = await confirm({
                            title: `Remove ${a.name}?`,
                            description:
                              'Its key stops working at once, and its running job moves to another worker.',
                            action: 'Remove PC',
                          })
                          if (ok) revoke.mutate(a)
                        }}
                      >
                        Revoke
                      </Button>
                    ) : (
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={async () => {
                          const ok = await confirm({
                            title: `Re-encode what ${a.name} encoded?`,
                            description:
                              'Every video whose current version this PC encoded is encoded again elsewhere.',
                            action: 'Re-encode videos',
                            destructive: false,
                          })
                          if (ok) reencode.mutate(a)
                        }}
                      >
                        Re-encode its videos
                      </Button>
                    )}
                  </li>
                ))}
              </ul>
            )
          }
        </QueryState>
      </Section>
    </>
  )
}

// ConnectCard is "Connect my PC" (docs/architecture.md, "Optional upgrades"):
// one command for the PC, which shows up here the moment it runs.
function ConnectCard({ onConnected }: { onConnected: () => void }) {
  const [inv, setInv] = React.useState<Invitation | null>(null)
  const [os, setOs] = React.useState<'linux' | 'docker'>('linux')
  const create = useMutation({ mutationFn: () => post<Invitation>('/v1/agents/pairings'), onSuccess: setInv })
  const pairing = useQuery({
    queryKey: ['pairing', inv?.code ?? ''],
    queryFn: () => api<Pairing>(`/v1/agents/pairings/${inv?.code}`),
    enabled: !!inv,
    refetchInterval: (query) => {
      const s = query.state.data?.status
      return s === 'consumed' || s === 'expired' ? false : 2000
    },
  })
  const done = pairing.data?.status === 'consumed'
  React.useEffect(() => {
    if (done) onConnected()
  }, [done, onConnected])
  const server = window.location.origin

  if (!inv)
    return (
      <div className="flex flex-col gap-3 rounded-lg border bg-card p-4">
        <span className="text-sm">
          On the PC you're using right now, run this and approve it in the browser tab it opens:
        </span>
        <CopyBlock label="command" value={`clipmux worker --server ${server}`} />
        <div className="flex flex-wrap items-center gap-3 text-sm text-muted-foreground">
          For another machine, or one without a browser:
          <Button variant="outline" size="sm" onClick={() => create.mutate()} disabled={create.isPending}>
            Create a one-time command
          </Button>
        </div>
        {create.isError && <p className="text-sm text-destructive">{describe(create.error)}</p>}
      </div>
    )
  return (
    <div className="flex flex-col gap-3 rounded-lg border bg-card p-4">
      <span className="font-medium">{done ? 'PC connected' : 'Run this on the PC'}</span>
      {done ? (
        <p className="text-sm text-muted-foreground">It's listed below and starts taking jobs now.</p>
      ) : (
        <>
          <Tabs value={os} onValueChange={(v) => setOs(v as 'linux' | 'docker')}>
            <TabsList>
              <TabsTrigger value="linux">Linux</TabsTrigger>
              <TabsTrigger value="docker">Docker (Windows, macOS)</TabsTrigger>
            </TabsList>
          </Tabs>
          <CopyBlock label="command" value={os === 'docker' ? inv.docker_command : inv.install_command} />
          <p className="text-sm text-muted-foreground">
            {os === 'linux' ? (
              <>
                Installs the agent with a systemd service that starts at boot (glibc 2.39 or newer, such as
                Ubuntu 24.04). With <code>sudo sh</code> it runs as a system service, otherwise as your user.
                If ClipMux is already installed, run <code>{inv.command}</code> instead.
              </>
            ) : (
              <>
                With a GPU, add <code>--gpus all</code> after <code>docker run</code> for NVIDIA (with the
                NVIDIA Container Toolkit), or <code>--device /dev/dri</code> for AMD or Intel on Linux. Docker
                Desktop on a Mac encodes on the CPU.
              </>
            )}
          </p>
          <p className="text-sm text-muted-foreground">
            {pairing.data?.status === 'expired'
              ? 'This code expired. Create a new command.'
              : `The code works once, until ${new Date(inv.expires_at).toLocaleTimeString()}. Waiting for the PC…`}
          </p>
        </>
      )}
      <div>
        <Button variant="outline" size="sm" onClick={() => setInv(null)}>
          {done ? 'Connect another' : 'Cancel'}
        </Button>
      </div>
    </div>
  )
}

function CloudSection() {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const providers = useQuery(q.executors())
  const [connecting, setConnecting] = React.useState<'runpod' | 'modal' | null>(null)
  const disconnect = useMutation({
    mutationFn: (kind: string) => del(`/v1/executors/${kind}`),
    onSuccess: (_, kind) => {
      notify.done(`${providerNames[kind]} disconnected`)
      void qc.invalidateQueries({ queryKey: ['executors'] })
      void qc.invalidateQueries({ queryKey: ['routing'] })
    },
    onError: (err) => notify.failed("Couldn't disconnect", err),
  })
  return (
    <Section
      title="Cloud GPUs"
      description="A provider rents a GPU machine for one job at a time. Spend is estimated from the rate you enter and each machine's running time, not the provider's bill."
    >
      <QueryState query={providers} what="cloud GPUs" skeleton={<Skeleton className="h-40 w-full" />}>
        {(data) => {
          const byKind = Object.fromEntries(data.data.map((p) => [p.kind, p]))
          return (
            <div className="grid gap-4 md:grid-cols-2">
              {(['runpod', 'modal'] as const).map((kind) => {
                const p = byKind[kind] as Provider | undefined
                return (
                  <ProviderCard
                    key={kind}
                    kind={kind}
                    provider={p?.connected ? p : undefined}
                    onConnect={() => setConnecting(kind)}
                    onDisconnect={async () => {
                      const ok = await confirm({
                        title: `Disconnect ${providerNames[kind]}?`,
                        description: 'Its credentials are forgotten. Routing must not name it first.',
                        action: 'Disconnect',
                      })
                      if (ok) disconnect.mutate(kind)
                    }}
                  />
                )
              })}
            </div>
          )
        }}
      </QueryState>
      <ConnectProviderDialog kind={connecting} onClose={() => setConnecting(null)} />
    </Section>
  )
}

function ProviderCard({
  kind,
  provider: p,
  onConnect,
  onDisconnect,
}: {
  kind: 'runpod' | 'modal'
  provider?: Provider | undefined
  onConnect: () => void
  onDisconnect: () => void
}) {
  const blurb =
    kind === 'runpod'
      ? 'GPU pods on Secure Cloud, started for each job.'
      : 'Serverless GPU sandboxes that start for each job and stop after.'
  return (
    <Card size="sm" data-provider={kind}>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          {providerNames[kind]}
          {p &&
            (p.verified ? (
              <Badge variant="success">Ready</Badge>
            ) : (
              <Badge variant="warning">Setup test pending</Badge>
            ))}
        </CardTitle>
        <CardDescription>{blurb}</CardDescription>
      </CardHeader>
      {p && (
        <CardContent className="flex flex-col gap-2 text-sm">
          <div className="flex justify-between">
            <span className="text-muted-foreground">This month</span>
            <span className="type-timecode">
              {dollars(p.month_cents)} of {dollars(p.monthly_limit_cents)}
            </span>
          </div>
          <Progress
            value={p.monthly_limit_cents ? Math.min(100, (p.month_cents / p.monthly_limit_cents) * 100) : 0}
            aria-label="Spend this month"
          />
          <span className="text-muted-foreground">
            {p.machines} of at most {p.max_concurrent} running, {dollars(p.rate_cents_per_hour)} per
            machine-hour
          </span>
          {p.backoff_until && (
            <span className="text-destructive">
              Backing off after failed starts until {new Date(p.backoff_until).toLocaleTimeString()}
            </span>
          )}
          {p.config.image && <code className="truncate text-xs text-muted-foreground">{p.config.image}</code>}
        </CardContent>
      )}
      <CardFooter className="gap-2">
        {p ? (
          <>
            <Button variant="outline" size="sm" onClick={onConnect}>
              Connect again
            </Button>
            <Button variant="ghost" size="sm" className="text-destructive" onClick={onDisconnect}>
              Disconnect
            </Button>
          </>
        ) : (
          <Button size="sm" onClick={onConnect}>
            Connect {providerNames[kind]}
          </Button>
        )}
      </CardFooter>
    </Card>
  )
}

function ConnectProviderDialog({ kind, onClose }: { kind: 'runpod' | 'modal' | null; onClose: () => void }) {
  const qc = useQueryClient()
  const [steps, setSteps] = React.useState<Step[]>([])
  const connect = useMutation({
    mutationFn: (body: object) =>
      post<{ connected: boolean; steps: Step[] }>(`/v1/executors/${kind}/connect`, body),
    onMutate: () => setSteps([]),
    onSuccess: (res) => {
      setSteps(res.steps)
      void qc.invalidateQueries({ queryKey: ['executors'] })
      void qc.invalidateQueries({ queryKey: ['routing'] })
      if (res.connected) notify.done(`${providerNames[kind ?? '']} connected`)
    },
  })
  const name = kind ? providerNames[kind] : ''
  return (
    <Dialog
      open={kind !== null}
      onOpenChange={(o) => {
        if (!o) {
          setSteps([])
          connect.reset()
          onClose()
        }
      }}
    >
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Connect {name}</DialogTitle>
          <DialogDescription>
            ClipMux checks the credentials, that {name} can reach this server, and runs a setup test before
            saving.
          </DialogDescription>
        </DialogHeader>
        <form
          aria-label={`Connect ${name}`}
          className="flex max-h-[60vh] flex-col gap-4 overflow-y-auto"
          onSubmit={(e) => {
            e.preventDefault()
            const f = new FormData(e.currentTarget)
            const common = {
              image: f.get('image') || undefined,
              registry_username: f.get('registry_username') || undefined,
              registry_password: f.get('registry_password') || undefined,
              max_concurrent: Number(f.get('max_concurrent')),
              monthly_limit_cents: Math.round(Number(f.get('monthly_limit')) * 100),
              rate_cents_per_hour: Math.round(Number(f.get('rate')) * 100),
            }
            connect.mutate(
              kind === 'runpod'
                ? { api_key: f.get('api_key'), ...common }
                : { token_id: f.get('token_id'), token_secret: f.get('token_secret'), ...common },
            )
          }}
        >
          <FieldGroup>
            {kind === 'runpod' ? (
              <Field>
                <FieldLabel htmlFor="api_key">RunPod API key</FieldLabel>
                <Input id="api_key" name="api_key" type="password" autoComplete="off" required />
              </Field>
            ) : (
              <div className="grid gap-4 sm:grid-cols-2">
                <Field>
                  <FieldLabel htmlFor="token_id">Modal token ID</FieldLabel>
                  <Input id="token_id" name="token_id" autoComplete="off" required />
                </Field>
                <Field>
                  <FieldLabel htmlFor="token_secret">Modal token secret</FieldLabel>
                  <Input id="token_secret" name="token_secret" type="password" autoComplete="off" required />
                </Field>
              </div>
            )}
            <div className="grid gap-4 sm:grid-cols-3">
              <Field>
                <FieldLabel htmlFor="max_concurrent">At most this many machines</FieldLabel>
                <Input
                  id="max_concurrent"
                  name="max_concurrent"
                  type="number"
                  min={1}
                  max={100}
                  defaultValue={1}
                  required
                />
              </Field>
              <Field>
                <FieldLabel htmlFor="monthly_limit">Monthly limit ($)</FieldLabel>
                <Input id="monthly_limit" name="monthly_limit" type="number" min={1} step="0.01" required />
              </Field>
              <Field>
                <FieldLabel htmlFor="rate">Rate ($ per machine-hour)</FieldLabel>
                <Input id="rate" name="rate" type="number" min={0} step="0.01" required />
              </Field>
            </div>
            <Field>
              <FieldLabel htmlFor="image">Worker image</FieldLabel>
              <Input id="image" name="image" placeholder="Default: the one this server was built from" />
            </Field>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field>
                <FieldLabel htmlFor="registry_username">Registry username</FieldLabel>
                <Input
                  id="registry_username"
                  name="registry_username"
                  placeholder="Registry username (private image)"
                  autoComplete="off"
                />
              </Field>
              <Field>
                <FieldLabel htmlFor="registry_password">Registry token</FieldLabel>
                <Input
                  id="registry_password"
                  name="registry_password"
                  type="password"
                  placeholder="Registry token with read:packages"
                  autoComplete="off"
                />
                <FieldDescription>Only for a private image.</FieldDescription>
              </Field>
            </div>
            {connect.isError && <FieldError>{describe(connect.error)}</FieldError>}
            <Steps steps={steps} />
            <div>
              <Button type="submit" disabled={connect.isPending}>
                {connect.isPending && <Spinner data-icon="inline-start" />}
                {connect.isPending ? 'Connecting…' : `Connect ${name}`}
              </Button>
            </div>
          </FieldGroup>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// ConnectPC is where `clipmux worker --server URL` sends the owner: the
// machine shows a code, the page shows the same code and what the machine
// said about itself, and one click connects it.
export function ConnectPC() {
  const [params] = useSearchParams()
  const [code, setCode] = React.useState(params.get('code') ?? '')
  const [looked, setLooked] = React.useState(params.get('code') ?? '')
  const pairing = useQuery({
    queryKey: ['pairing', looked],
    queryFn: () => api<Pairing>(`/v1/agents/pairings/${encodeURIComponent(looked)}`),
    enabled: !!looked,
    retry: false,
  })
  const decide = useMutation({
    mutationFn: (approve: boolean) =>
      post<Pairing>(`/v1/agents/pairings/${encodeURIComponent(looked)}/${approve ? 'approve' : 'deny'}`),
    onSuccess: (p) => pairing.refetch().then(() => p),
  })
  const p = decide.data ?? pairing.data
  const notFound = pairing.error instanceof ApiError && pairing.error.status === 404
  return (
    <PageBody className="max-w-lg">
      {!p ? (
        <div className="flex flex-col gap-4">
          <PageHeader title="Connect a PC" description="Enter the code your PC shows." />
          <form
            className="flex gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              setLooked(code.trim())
            }}
          >
            <Input
              aria-label="Pairing code"
              value={code}
              onChange={(e) => setCode(e.target.value)}
              placeholder="ABCD-EFGH"
              required
              className="font-mono uppercase"
            />
            <Button type="submit" disabled={pairing.isFetching}>
              Continue
            </Button>
          </form>
          {pairing.isError && (
            <p className="text-sm text-destructive">
              {notFound ? 'No PC is waiting with that code.' : describe(pairing.error)}
            </p>
          )}
        </div>
      ) : (
        <PairingDecision
          p={p}
          code={looked}
          busy={decide.isPending}
          onDecide={(ok) => decide.mutate(ok)}
          error={decide.error}
        />
      )}
    </PageBody>
  )
}

function PairingDecision({
  p,
  code,
  busy,
  onDecide,
  error,
}: {
  p: Pairing
  code: string
  busy: boolean
  onDecide: (approve: boolean) => void
  error: unknown
}) {
  const waiting = p.status === 'pending' && p.agent_started
  const connected = p.status === 'approved' || p.status === 'consumed'
  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={
          waiting
            ? 'Connect this PC?'
            : connected
              ? 'Connected'
              : p.status === 'denied'
                ? 'Not connected'
                : 'This code expired'
        }
      />
      <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
        <dt className="text-muted-foreground">Code</dt>
        <dd className="font-mono">{code.toUpperCase()}</dd>
        <dt className="text-muted-foreground">Name</dt>
        <dd>{p.name}</dd>
        {p.hostname && (
          <>
            <dt className="text-muted-foreground">Machine</dt>
            <dd>
              {p.hostname} ({p.platform})
            </dd>
          </>
        )}
        {p.ip && (
          <>
            <dt className="text-muted-foreground">Address</dt>
            <dd>{p.ip}</dd>
          </>
        )}
      </dl>
      {waiting ? (
        <>
          <Alert>
            <AlertDescription>
              Check the code matches the one in your terminal. Once connected, this PC can download sources
              and upload encoded videos for this server.
            </AlertDescription>
          </Alert>
          <div className="flex gap-2">
            <Button disabled={busy} onClick={() => onDecide(true)}>
              Connect
            </Button>
            <Button variant="outline" disabled={busy} onClick={() => onDecide(false)}>
              Deny
            </Button>
          </div>
        </>
      ) : connected ? (
        <p className="text-sm text-muted-foreground">
          The PC picks this up within a couple of seconds and starts taking jobs. You can close this tab, or
          see it on{' '}
          <Link to="/encoding" className="text-link underline-offset-4 hover:underline">
            Encoding
          </Link>
          .
        </p>
      ) : (
        <p className="text-sm text-muted-foreground">
          Run <code>clipmux worker</code> on the PC again for a new code.
        </p>
      )}
      {error != null && <p className="text-sm text-destructive">{describe(error)}</p>}
    </div>
  )
}
