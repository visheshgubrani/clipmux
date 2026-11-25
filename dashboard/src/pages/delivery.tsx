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
import { RestoreFenceAlert } from '@/components/backups'
import { CreateTokenLink, useInspection } from '@/components/cloudflare-token'
import { useConfirm } from '@/components/confirm'
import { PageBody, PageHeader, Section } from '@/components/page'
import { QueryState } from '@/components/query-state'
import { Steps } from '@/components/steps'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { ago, fullDate } from '@/lib/format'
import { notify } from '@/lib/notify'
import { LibraryMove, storageName } from '@/pages/storage'
import { q } from '@/queries'

// Delivery (docs/architecture.md, "Optional upgrades", D117): how videos
// reach viewers, and so where their encoded files are kept: with the
// uploads when this server streams them, in R2 when Cloudflare does. Where
// uploads are kept is Storage.

const DEFAULT_BUCKET = 'clipmux-media'
const NEW_BUCKET = '__new__'
const BUCKET_PATTERN = '^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$'

export function Delivery() {
  return (
    <PageBody>
      <PageHeader
        title="Delivery"
        description="How videos reach viewers, and where their encoded files are kept."
      />
      <DeliverySection />
    </PageBody>
  )
}

function DeliverySection() {
  const cf = useQuery(q.cloudflare())
  const storage = useQuery(q.storage())
  const backups = useQuery(q.backups())
  const [connecting, setConnecting] = React.useState(false)
  const def = storage.data?.data.find((s) => s.default)
  return (
    <Section title="Delivery">
      <QueryState query={cf} what="delivery" skeleton={<Skeleton className="h-40 w-full" />}>
        {(s) => (
          <>
            {backups.data?.fence && <RestoreFenceAlert fence={backups.data.fence} />}
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
                    {def &&
                      !s.connected &&
                      ` Encoded files are kept with your uploads, on ${storageName(def)}.`}
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
                      ? `The Worker at ${s.hostname} serves playback from the R2 bucket ${s.bucket}, where new videos are encoded. Your apps, tokens and playback IDs stay the same.`
                      : "Serve from Cloudflare's edge with a Worker. ClipMux creates an R2 bucket for encoded videos, deploys the Worker and points your media hostname at it. Needs a zone on Workers Paid."}
                  </CardDescription>
                </CardHeader>
                {def && (
                  <CardContent className="text-sm text-muted-foreground">
                    Uploads stay on {storageName(def)}
                    {s.connected ? '' : ' either way'}.{' '}
                    <Link to="/storage" className="text-link underline-offset-4 hover:underline">
                      Storage
                    </Link>{' '}
                    chooses where.
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
            {s.connected && <LibraryMove part="media" />}
            <ConnectCloudflare open={connecting} onOpenChange={setConnecting} status={s} />
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
      // The page's Content-Security-Policy names the media URL it was served
      // with, and disconnecting moves it back to this server: reload, unless a
      // failed step needs reading first.
      if (res.disconnected && res.steps.every((s) => s.ok)) {
        location.reload()
        return
      }
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
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  status: Cloudflare
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
    // Connect moved the media URL to the Worker's hostname, which the page's
    // Content-Security-Policy doesn't name yet: until a reload the browser
    // blocks every request to it and no video plays.
    if (connect.data?.connected) {
      location.reload()
      return
    }
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
            </div>
          </FieldGroup>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/** The media hostname and the bucket, from what the token reaches. */
function Choices({ found, status }: { found: CloudflareInspection; status: Cloudflare }) {
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
  const [customBucket, setCustomBucket] = React.useState('')
  const creating = listBuckets && bucket === NEW_BUCKET
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
          <>
            <NativeSelect
              id="cf-bucket"
              name={creating ? undefined : 'bucket'}
              value={bucket}
              onChange={(e) => setBucket(e.target.value)}
            >
              {bucketOptions.map((b) => (
                <NativeSelectOption key={b} value={b}>
                  {buckets.includes(b) ? b : `${b} (new)`}
                </NativeSelectOption>
              ))}
              <NativeSelectOption value={NEW_BUCKET}>Create a new bucket…</NativeSelectOption>
            </NativeSelect>
            {creating && (
              <Input
                id="cf-bucket-new"
                name="bucket"
                aria-label="New R2 bucket name"
                placeholder="clipmux-media-staging"
                pattern={BUCKET_PATTERN}
                title="3–63 lowercase letters, numbers, and hyphens"
                required
                value={customBucket}
                onChange={(e) => setCustomBucket(e.target.value.toLowerCase())}
              />
            )}
          </>
        ) : (
          <Input
            id="cf-bucket"
            name="bucket"
            placeholder={DEFAULT_BUCKET}
            value={bucket}
            onChange={(e) => setBucket(e.target.value)}
          />
        )}
        <FieldDescription>
          New videos are encoded into it and the Worker serves them from it; uploads stay where Storage keeps
          them. Use a separate bucket and media hostname per ClipMux install—choose “Create a new bucket…” to
          make a fresh one.
        </FieldDescription>
      </Field>
    </>
  )
}
