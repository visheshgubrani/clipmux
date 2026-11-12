import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import * as React from 'react'
import { Link } from 'react-router'
import {
  type Cloudflare,
  type CloudflareInspection,
  del,
  describe,
  post,
  type Step,
  type StepResult,
} from '@/api'
import { CreateTokenLink, useInspection } from '@/components/cloudflare-token'
import { useConfirm } from '@/components/confirm'
import { PageBody, PageHeader, Section } from '@/components/page'
import { QueryState } from '@/components/query-state'
import { Steps } from '@/components/steps'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { ago, fullDate } from '@/lib/format'
import { notify } from '@/lib/notify'
import { storageName } from '@/pages/storage'
import { q } from '@/queries'

// Delivery (docs/architecture.md, "Optional upgrades"): how videos reach
// viewers, built in or through Cloudflare. Where files live is Storage.

const DEFAULT_BUCKET = 'clipmux-media'

export function Delivery() {
  return (
    <PageBody>
      <PageHeader title="Delivery" description="How videos reach viewers." />
      <DeliverySection />
    </PageBody>
  )
}

function DeliverySection() {
  const cf = useQuery(q.cloudflare())
  const storage = useQuery(q.storage())
  const [connecting, setConnecting] = React.useState(false)
  const def = storage.data?.data.find((s) => s.default)
  return (
    <Section title="Delivery">
      <QueryState query={cf} what="delivery" skeleton={<Skeleton className="h-40 w-full" />}>
        {(s) => (
          <>
            {s.connected && !s.storage_ok && (
              <Alert variant="destructive">
                <AlertTitle>R2 refuses the stored credentials</AlertTitle>
                <AlertDescription>
                  The token was probably revoked or rolled. Connect Cloudflare again with a new token.
                </AlertDescription>
              </Alert>
            )}
            {s.connected && s.storage_ok && s.needs_redeploy && (
              <Alert>
                <AlertTitle>A newer Worker is available</AlertTitle>
                <AlertDescription>
                  This version of ClipMux ships Worker {s.current_version}; {s.worker_version} is deployed.
                  Paste a token to redeploy it.
                </AlertDescription>
              </Alert>
            )}
            <div className="grid gap-4 md:grid-cols-2">
              <Card size="sm" className={s.connected ? undefined : 'ring-2 ring-primary'}>
                <CardHeader>
                  <CardTitle className="flex items-center gap-2">
                    Built in {!s.connected && <Badge variant="success">In use</Badge>}
                  </CardTitle>
                  <CardDescription>
                    This server streams every video. Simple, and fine for a few hundred viewers at once.
                  </CardDescription>
                </CardHeader>
              </Card>
              <Card size="sm" className={s.connected ? 'ring-2 ring-primary' : undefined}>
                <CardHeader>
                  <CardTitle className="flex items-center gap-2">
                    Cloudflare {s.connected && <Badge variant="success">In use</Badge>}
                  </CardTitle>
                  <CardDescription>
                    {s.connected
                      ? `The Worker at ${s.hostname} serves playback from the R2 bucket ${s.bucket}. Your apps, tokens and playback IDs stay the same.`
                      : "Serve from Cloudflare's edge with a Worker. ClipMux creates the R2 bucket, deploys the Worker and points your media hostname at it. Needs a zone on Workers Paid."}
                  </CardDescription>
                </CardHeader>
                {s.connected && def && (
                  <CardContent className="text-sm text-muted-foreground">
                    New uploads go to {storageName(def)}.{' '}
                    <Link to="/storage" className="text-link underline-offset-4 hover:underline">
                      Change it under Storage
                    </Link>
                    .
                  </CardContent>
                )}
                <CardFooter className="gap-2">
                  <Button
                    size="sm"
                    variant={s.connected ? 'outline' : 'default'}
                    onClick={() => setConnecting(true)}
                  >
                    {s.connected ? 'Connect again' : 'Connect Cloudflare'}
                  </Button>
                  {s.connected && <DisconnectButton />}
                </CardFooter>
              </Card>
            </div>
            <ConnectCloudflare
              open={connecting}
              onOpenChange={setConnecting}
              status={s}
              r2IsDefault={!!def?.cloudflare_delivery}
            />
            <EdgeAnalyticsCard cloudflare={s.connected} tokenURL={s.analytics_token_url} />
          </>
        )}
      </QueryState>
    </Section>
  )
}

/**
 * Edge analytics (P7-T05): the Worker counts playback into Analytics Engine
 * in the owner's Cloudflare account; a read-only token lets this server pull
 * those counts into Analytics.
 */
function EdgeAnalyticsCard({ cloudflare, tokenURL }: { cloudflare: boolean; tokenURL: string }) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const edge = useQuery(q.edgeAnalytics())
  const [replacing, setReplacing] = React.useState(false)
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['edge-analytics'] })
    void qc.invalidateQueries({ queryKey: ['analytics'] })
  }
  const connect = useMutation({
    mutationFn: (token: string) => post('/v1/cloudflare/analytics', { token }),
    onSuccess: () => {
      notify.done('Edge analytics connected', 'Counts from the edge show up in Analytics within 5 minutes.')
      setReplacing(false)
      refresh()
    },
  })
  const disconnect = useMutation({
    mutationFn: () => del('/v1/cloudflare/analytics'),
    onSuccess: () => {
      notify.done('Edge analytics disconnected')
      refresh()
    },
    onError: (err) => notify.failed("Couldn't disconnect edge analytics", err),
  })
  const e = edge.data
  // Only Worker delivery has edge counts; a token kept after a disconnect still shows.
  if (!e || (!cloudflare && !e.connected)) return null
  const form = (
    <form
      className="flex flex-col gap-3"
      onSubmit={(ev) => {
        ev.preventDefault()
        connect.mutate(String(new FormData(ev.currentTarget).get('token') ?? ''))
      }}
    >
      <Field>
        <FieldLabel htmlFor="edge-token">Read-only API token</FieldLabel>
        <CreateTokenLink url={tokenURL} />
        <Input id="edge-token" name="token" type="password" autoComplete="off" required />
        <FieldDescription>
          Cloudflare opens with only Account Analytics: Read chosen. Create it on the account the Worker runs
          in and paste it here. ClipMux keeps it encrypted.
        </FieldDescription>
      </Field>
      {connect.isError && <FieldError>{describe(connect.error)}</FieldError>}
      <div className="flex flex-wrap gap-2">
        <Button type="submit" size="sm" disabled={connect.isPending}>
          {connect.isPending && <Spinner data-icon="inline-start" />}
          {e.connected ? 'Save token' : 'Connect edge analytics'}
        </Button>
        {replacing && (
          <Button type="button" size="sm" variant="ghost" onClick={() => setReplacing(false)}>
            Cancel
          </Button>
        )}
      </div>
    </form>
  )
  return (
    <Card size="sm" id="edge-analytics" role="region" aria-label="Edge analytics">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          Edge analytics {e.connected && <Badge variant="success">Connected</Badge>}
        </CardTitle>
        <CardDescription>
          {e.connected
            ? `Views at the edge are pulled from Cloudflare every 5 minutes. Last pull ${ago(e.last_pull_at)}${e.pulled_through ? `, counted through ${fullDate(e.pulled_through)}` : ''}.`
            : 'The Worker counts views, watch time and bandwidth in Analytics Engine in your Cloudflare account, keyed by hashed viewer IDs and never by address. Connect a read-only token so Analytics includes them.'}
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {e.last_error && (
          <Alert variant="destructive">
            <AlertTitle>The last pull failed</AlertTitle>
            <AlertDescription>{e.last_error}</AlertDescription>
          </Alert>
        )}
        {(!e.connected || replacing) && form}
      </CardContent>
      {e.connected && !replacing && (
        <CardFooter className="gap-2">
          <Button size="sm" variant="outline" onClick={() => setReplacing(true)}>
            Replace token
          </Button>
          <Button
            size="sm"
            variant="ghost"
            className="text-destructive"
            disabled={disconnect.isPending}
            onClick={async () => {
              const ok = await confirm({
                title: 'Disconnect edge analytics?',
                description:
                  'ClipMux forgets the token and stops pulling counts from the edge. Counts already pulled stay in Analytics.',
                action: 'Disconnect',
              })
              if (ok) disconnect.mutate()
            }}
          >
            Disconnect
          </Button>
        </CardFooter>
      )}
    </Card>
  )
}

function DisconnectButton() {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const [steps, setSteps] = React.useState<Step[]>([])
  const disconnect = useMutation({
    mutationFn: () => del<{ disconnected: boolean; steps: Step[] }>('/v1/cloudflare'),
    onSuccess: (res) => {
      setSteps(res.steps)
      if (res.disconnected) notify.done('Back on built-in delivery')
      void qc.invalidateQueries({ queryKey: ['cloudflare'] })
      void qc.invalidateQueries({ queryKey: ['setup'] })
      void qc.invalidateQueries({ queryKey: ['storage'] })
    },
    onError: (err) => notify.failed("Couldn't disconnect Cloudflare", err),
  })
  return (
    <>
      <Button
        size="sm"
        variant="ghost"
        className="text-destructive"
        disabled={disconnect.isPending}
        onClick={async () => {
          const ok = await confirm({
            title: 'Disconnect Cloudflare?',
            description:
              'Playback moves back to this server and the Worker stops answering for your videos. Files on R2 stay there and keep playing through this server. The Worker and its domain stay in your Cloudflare account.',
            action: 'Disconnect Cloudflare',
          })
          if (ok) disconnect.mutate()
        }}
      >
        {disconnect.isPending && <Spinner data-icon="inline-start" />}
        Disconnect
      </Button>
      {steps.some((s) => !s.ok) && <Steps steps={steps} />}
    </>
  )
}

/** The zone a hostname lives under: the longest of zones it ends with. */
function zoneOf(hostname: string, zones: string[]) {
  return zones
    .filter((z) => hostname === z || hostname.endsWith(`.${z}`))
    .sort((a, b) => b.length - a.length)[0]
}

/**
 * Connect Cloudflare (D037, D109): create a token from a pre-filled link,
 * paste it, and pick the account, the media hostname under one of its zones
 * and the bucket from what the token reaches.
 */
function ConnectCloudflare({
  open,
  onOpenChange,
  status,
  r2IsDefault,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  status: Cloudflare
  r2IsDefault: boolean
}) {
  const qc = useQueryClient()
  const [token, setToken] = React.useState('')
  const [accountID, setAccountID] = React.useState('')
  const [steps, setSteps] = React.useState<Step[]>([])
  const inspect = useInspection(open ? token : '', accountID)
  const connect = useMutation({
    mutationFn: (body: object) => post<StepResult>('/v1/cloudflare/connect', body),
    onMutate: () => setSteps([]),
    onSuccess: (res) => {
      setSteps(res.steps)
      void qc.invalidateQueries({ queryKey: ['cloudflare'] })
      void qc.invalidateQueries({ queryKey: ['moves'] })
      void qc.invalidateQueries({ queryKey: ['storage'] })
      if (res.connected) notify.done('Cloudflare connected')
    },
  })
  const close = () => {
    setToken('')
    setAccountID('')
    setSteps([])
    connect.reset()
    onOpenChange(false)
  }
  const found = inspect.data
  const ready = !!found?.account_id
  return (
    <Dialog open={open} onOpenChange={(o) => (o ? onOpenChange(true) : close())}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{status.connected ? 'Connect Cloudflare again' : 'Connect Cloudflare'}</DialogTitle>
          <DialogDescription>
            ClipMux creates the R2 bucket, deploys the Worker and moves your media hostname to it. It uses the
            token for this request and keeps only its ID; the token's permissions stay live in Cloudflare.
          </DialogDescription>
        </DialogHeader>
        <form
          className="flex max-h-[60vh] flex-col overflow-y-auto"
          onSubmit={(e) => {
            e.preventDefault()
            const f = new FormData(e.currentTarget)
            const zone = String(f.get('zone') ?? '')
            const hostname = zone
              ? `${String(f.get('subdomain') ?? '').trim()}.${zone}`
              : String(f.get('hostname') ?? '').trim()
            connect.mutate({
              token: token.trim(),
              hostname,
              bucket: String(f.get('bucket') ?? '').trim() || undefined,
              account_id: found?.account_id,
              default_storage: f.get('default_storage') === 'on',
            })
          }}
        >
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="cf-token">API token</FieldLabel>
              <CreateTokenLink url={status.token_url} />
              <Input
                id="cf-token"
                name="token"
                type="password"
                autoComplete="off"
                required
                value={token}
                onChange={(e) => setToken(e.target.value)}
              />
              <FieldDescription>
                Cloudflare opens with every permission ClipMux needs already chosen: Workers, R2, the zone's
                DNS and Billing read. Pick the account, create the token and paste it here.
              </FieldDescription>
              {inspect.isPending && (
                <FieldDescription className="flex items-center gap-2">
                  <Spinner /> Checking the token…
                </FieldDescription>
              )}
              {inspect.isError && <FieldError>{describe(inspect.error)}</FieldError>}
            </Field>
            {found && found.accounts.length > 1 && (
              <Field>
                <FieldLabel htmlFor="cf-account">Account</FieldLabel>
                <NativeSelect
                  id="cf-account"
                  name="account_id"
                  value={accountID}
                  onChange={(e) => setAccountID(e.target.value)}
                >
                  <NativeSelectOption value="">Choose an account</NativeSelectOption>
                  {found.accounts.map((a) => (
                    <NativeSelectOption key={a.id} value={a.id}>
                      {a.name}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
              </Field>
            )}
            {found && ready && (
              <Choices
                // A new inspection resets the choices to what it found.
                key={`${found.account_id}:${(found.zones ?? []).join(',')}:${(found.buckets ?? []).join(',')}`}
                found={found}
                status={status}
                storeUploads={status.connected ? r2IsDefault : true}
              />
            )}
            {connect.isError && <FieldError>{describe(connect.error)}</FieldError>}
            <Steps steps={steps} />
            <div className="flex flex-wrap items-center gap-3">
              <Button
                type="submit"
                disabled={
                  connect.isPending ||
                  !ready ||
                  (found && !found.workers_paid && !found.unreadable?.includes('billing'))
                }
              >
                {connect.isPending && <Spinner data-icon="inline-start" />}
                {connect.isPending
                  ? 'Connecting…'
                  : status.connected
                    ? 'Connect again'
                    : 'Connect Cloudflare'}
              </Button>
              <span className="text-sm text-muted-foreground">Prefer wrangler? See docs/cloudflare.md.</span>
            </div>
          </FieldGroup>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/** The media hostname, bucket and upload choice, from what the token reaches. */
function Choices({
  found,
  status,
  storeUploads,
}: {
  found: CloudflareInspection
  status: Cloudflare
  storeUploads: boolean
}) {
  const zones = found.zones ?? []
  const buckets = found.buckets ?? []
  const pickZones = zones.length > 0
  const listBuckets = !found.unreadable?.includes('buckets')
  const previous = status.hostname ?? ''
  const startZone = (previous && zoneOf(previous, zones)) || zones[0] || ''
  const [zone, setZone] = React.useState(startZone)
  const [sub, setSub] = React.useState(
    previous && startZone && previous.endsWith(`.${startZone}`)
      ? previous.slice(0, -startZone.length - 1)
      : 'media',
  )
  const [bucket, setBucket] = React.useState(status.bucket || DEFAULT_BUCKET)
  const [toR2, setToR2] = React.useState(storeUploads)
  // The bucket it starts on stays a choice when it doesn't exist yet.
  const first = status.bucket || DEFAULT_BUCKET
  const bucketOptions = buckets.includes(first) ? buckets : [first, ...buckets]
  return (
    <>
      {!found.workers_paid && !found.unreadable?.includes('billing') && (
        <Alert variant="destructive">
          <AlertTitle>This account isn't on Workers Paid</AlertTitle>
          <AlertDescription>
            Video delivery exceeds the free plan's limits and terms. Subscribe to Workers Paid in Cloudflare,
            then paste the token again.
          </AlertDescription>
        </Alert>
      )}
      {pickZones ? (
        <Field>
          <FieldLabel htmlFor="cf-subdomain">Media hostname</FieldLabel>
          <div className="flex items-center gap-2">
            <Input
              id="cf-subdomain"
              name="subdomain"
              aria-label="Subdomain"
              className="min-w-0 flex-1"
              required
              value={sub}
              onChange={(e) => setSub(e.target.value)}
            />
            <span className="text-muted-foreground">.</span>
            <NativeSelect
              id="cf-zone"
              name="zone"
              aria-label="Zone"
              className="min-w-0 flex-1"
              value={zone}
              onChange={(e) => setZone(e.target.value)}
            >
              {zones.map((z) => (
                <NativeSelectOption key={z} value={z}>
                  {z}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </div>
          <FieldDescription>
            Viewers fetch video from https://{sub || '…'}.{zone}. It must differ from this server's address,
            and any DNS record already at that name is replaced.
          </FieldDescription>
        </Field>
      ) : (
        <Field>
          <FieldLabel htmlFor="cf-hostname">Media hostname</FieldLabel>
          <Input
            id="cf-hostname"
            name="hostname"
            placeholder="media.example.com"
            required
            defaultValue={previous}
          />
          <FieldDescription>
            {found.unreadable?.includes('zones')
              ? 'A name under a zone in this account.'
              : 'The token reaches no active zone in this account; add the domain to Cloudflare first.'}
          </FieldDescription>
        </Field>
      )}
      <Field>
        <FieldLabel htmlFor="cf-bucket">R2 bucket</FieldLabel>
        {listBuckets ? (
          <NativeSelect
            id="cf-bucket"
            name="bucket"
            value={bucket}
            onChange={(e) => setBucket(e.target.value)}
          >
            {bucketOptions.map((b) => (
              <NativeSelectOption key={b} value={b}>
                {buckets.includes(b) ? b : `${b} (new)`}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        ) : (
          <Input
            id="cf-bucket"
            name="bucket"
            placeholder={DEFAULT_BUCKET}
            value={bucket}
            onChange={(e) => setBucket(e.target.value)}
          />
        )}
        <FieldDescription>The Worker serves from it. A new bucket is created for you.</FieldDescription>
      </Field>
      <Field orientation="horizontal">
        <input type="hidden" name="default_storage" value={toR2 ? 'on' : 'off'} />
        <Checkbox id="cf-default" checked={toR2} onCheckedChange={(v) => setToR2(v === true)} />
        <div className="flex flex-col gap-1">
          <FieldLabel htmlFor="cf-default" className="font-normal">
            Store new uploads in R2
          </FieldLabel>
          <FieldDescription>
            Recommended: the Worker serves them from the edge. Otherwise uploads stay where they are now and
            play through this server; change it under Storage any time.
          </FieldDescription>
        </div>
      </Field>
    </>
  )
}
