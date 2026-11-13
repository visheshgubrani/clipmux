import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import * as React from 'react'
import { del, type Provider, put, type Routing } from '@/api'
import { useConfirm } from '@/components/confirm'
import { ConnectProviderDialog } from '@/components/connect-provider'
import { PageBody, PageHeader, Section } from '@/components/page'
import { QueryState } from '@/components/query-state'
import { Presence } from '@/components/status'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Field, FieldLabel } from '@/components/ui/field'
import { Label } from '@/components/ui/label'
import { Progress } from '@/components/ui/progress'
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'
import { Skeleton } from '@/components/ui/skeleton'
import { dollars } from '@/lib/format'
import { notify } from '@/lib/notify'
import { q } from '@/queries'

// Encoding (D116): one place videos are encoded. This server always can,
// on its GPU when it sees one; a cloud GPU takes over once it is connected
// and a sample has encoded on it, with this server behind it.

type Cloud = 'modal' | 'runpod'
const clouds: Cloud[] = ['modal', 'runpod']
const names: Record<Cloud, string> = { modal: 'Modal', runpod: 'RunPod' }
const blurbs: Record<Cloud, string> = {
  modal: 'GPU machines that start for each video and stop after. You pay only while one encodes.',
  runpod: 'GPU pods on RunPod’s Secure Cloud, started for each video and stopped after.',
}

const isCloud = (s: string | null | undefined): s is Cloud => s === 'modal' || s === 'runpod'

export function Encoding() {
  return (
    <PageBody>
      <PageHeader
        title="Encoding"
        description="Where your videos are encoded. This server always works; a cloud GPU is faster."
      />
      <WhereSection />
    </PageBody>
  )
}

function WhereSection() {
  // Until a connected provider has passed its check, look again every few
  // seconds, so the page shows it taking over without a reload.
  const executors = useQuery({
    ...q.executors(),
    refetchInterval: (query) =>
      query.state.data?.data.some((p) => p.connected && !p.verified) ? 5000 : false,
  })
  const checking = !!executors.data?.data.some((p) => p.connected && !p.verified)
  const routing = useQuery({ ...q.routing(), refetchInterval: checking ? 5000 : false })
  return (
    <Section title="Where videos are encoded">
      <QueryState query={routing} what="encoding" skeleton={<Skeleton className="h-64 w-full" />}>
        {(r) => (
          <QueryState query={executors} what="cloud GPUs" skeleton={<Skeleton className="h-48 w-full" />}>
            {(ex) => <Choices routing={r} providers={ex.data} checking={checking} />}
          </QueryState>
        )}
      </QueryState>
    </Section>
  )
}

function Choices({
  routing: r,
  providers,
  checking,
}: {
  routing: Routing
  providers: Provider[]
  checking: boolean
}) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const [connecting, setConnecting] = React.useState<Cloud | null>(null)
  const byKind = Object.fromEntries(providers.map((p) => [p.kind, p])) as Partial<Record<string, Provider>>
  // The latest setup test says whether a provider's check failed.
  const latest = useQuery({ ...q.setupTest(), refetchInterval: checking ? 5000 : false }).data
  const chosen = isCloud(r.primary_executor) ? r.primary_executor : 'server'
  const fallback = r.fallback_executor === 'builtin'
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['routing'] })
    void qc.invalidateQueries({ queryKey: ['executors'] })
  }
  const save = useMutation({
    mutationFn: (body: { primary_executor: string; fallback_executor: string | null }) =>
      put<Routing>('/v1/settings/routing', { ...body, queue_age_fallback_minutes: null }),
    onSuccess: (next) => {
      qc.setQueryData(['routing'], next)
      notify.done(
        isCloud(next.primary_executor)
          ? `New videos encode on ${names[next.primary_executor]}`
          : 'New videos encode on this server',
      )
    },
    onError: (err) => notify.failed("Couldn't change where videos encode", err),
  })
  const disconnect = useMutation({
    mutationFn: (kind: Cloud) => del(`/v1/executors/${kind}`),
    onSuccess: (_, kind) => {
      notify.done(
        `${names[kind]} disconnected`,
        chosen === kind ? 'New videos encode on this server.' : undefined,
      )
      refresh()
    },
    onError: (err) => notify.failed("Couldn't disconnect", err),
  })
  const choose = (v: string) => {
    if (v === chosen) return
    if (isCloud(v)) save.mutate({ primary_executor: v, fallback_executor: 'builtin' })
    else save.mutate({ primary_executor: 'builtin', fallback_executor: null })
  }
  return (
    <div className="flex flex-col gap-4">
      <RadioGroup
        value={chosen}
        onValueChange={(v) => choose(String(v))}
        aria-label="Where videos are encoded"
      >
        <Option
          value="server"
          title="This server"
          body="Uses this machine’s GPU when ClipMux can see one (NVIDIA, AMD or Intel), otherwise its CPU. Nothing to set up."
          status={
            <Presence
              on={r.builtin_worker.online}
              label={r.builtin_worker.online ? 'Online' : 'Offline, or not seen for a minute'}
            />
          }
          disabled={save.isPending}
        />
        {clouds.map((kind) => {
          const p = byKind[kind]?.connected ? byKind[kind] : undefined
          const failed =
            p && !p.verified && latest?.executor === kind && latest.status === 'failed'
              ? latest.checks.find((c) => !c.ok)
              : undefined
          return (
            <Option
              key={kind}
              value={kind}
              provider={kind}
              title={names[kind]}
              body={blurbs[kind]}
              disabled={!p?.verified || save.isPending}
              status={
                !p ? null : p.verified ? (
                  <Badge variant="success">Ready</Badge>
                ) : failed ? (
                  <Badge variant="destructive">Check failed</Badge>
                ) : (
                  <Badge variant="warning">Checking</Badge>
                )
              }
            >
              {p && !p.verified && (
                <p className={failed ? 'text-sm text-destructive' : 'text-sm text-muted-foreground'}>
                  {failed
                    ? `A sample didn't encode on ${names[kind]} (${failed.name}${failed.detail ? `: ${failed.detail}` : ''}). Connect again to retry; until then videos encode on this server.`
                    : `A sample video is encoding on ${names[kind]}. It takes over new videos once that works; until then they encode on this server.`}
                </p>
              )}
              {p && <Spend p={p} />}
              <div className="flex flex-wrap gap-2">
                {p ? (
                  <>
                    <Button variant="outline" size="sm" onClick={() => setConnecting(kind)}>
                      Connect again
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      className="text-destructive"
                      disabled={disconnect.isPending}
                      onClick={async () => {
                        const ok = await confirm({
                          title: `Disconnect ${names[kind]}?`,
                          description:
                            chosen === kind
                              ? `ClipMux forgets its credentials, and new videos encode on this server.`
                              : 'ClipMux forgets its credentials.',
                          action: 'Disconnect',
                        })
                        if (ok) disconnect.mutate(kind)
                      }}
                    >
                      Disconnect
                    </Button>
                  </>
                ) : (
                  <Button size="sm" onClick={() => setConnecting(kind)}>
                    Connect {names[kind]}
                  </Button>
                )}
              </div>
            </Option>
          )
        })}
      </RadioGroup>
      {isCloud(chosen) && (
        <Field orientation="horizontal">
          <Checkbox
            id="fallback"
            checked={fallback}
            disabled={save.isPending}
            onCheckedChange={(v) =>
              save.mutate({ primary_executor: chosen, fallback_executor: v === true ? 'builtin' : null })
            }
          />
          <FieldLabel htmlFor="fallback" className="font-normal">
            If {names[chosen]} can’t start a machine, encode on this server instead
          </FieldLabel>
        </Field>
      )}
      <ConnectProviderDialog kind={connecting} onClose={() => setConnecting(null)} />
    </div>
  )
}

/** One choice: a radio, its title and blurb, a status on the right, and whatever else it needs below. */
function Option({
  value,
  title,
  body,
  status,
  disabled,
  provider,
  children,
}: {
  value: string
  title: string
  body: string
  status?: React.ReactNode
  disabled?: boolean
  provider?: Cloud
  children?: React.ReactNode
}) {
  const id = `encode-${value}`
  return (
    <div
      data-provider={provider}
      className="flex flex-col gap-3 rounded-lg border bg-card p-4 has-data-checked:border-primary has-data-checked:bg-primary/5"
    >
      <div className="flex flex-wrap items-start gap-x-3 gap-y-2">
        <RadioGroupItem value={value} id={id} disabled={disabled} className="mt-0.5" />
        <div className="flex min-w-0 flex-[1_1_16rem] flex-col gap-1">
          <Label htmlFor={id} className="font-medium">
            {title}
          </Label>
          <p className="text-sm text-muted-foreground">{body}</p>
        </div>
        {status}
      </div>
      {children && <div className="flex flex-col gap-3 pl-7">{children}</div>}
    </div>
  )
}

/** This month's spend against the limit, and how many machines run. */
function Spend({ p }: { p: Provider }) {
  return (
    <div className="flex flex-col gap-2 text-sm">
      <div className="flex justify-between gap-4">
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
        {p.machines} of at most {p.max_concurrent} running, {dollars(p.rate_cents_per_hour)} per machine-hour
      </span>
      {p.backoff_until && (
        <span className="text-destructive">
          Backing off after failed starts until {new Date(p.backoff_until).toLocaleTimeString()}
        </span>
      )}
    </div>
  )
}
