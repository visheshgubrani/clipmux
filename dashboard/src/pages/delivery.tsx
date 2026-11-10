import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { PlusIcon } from 'lucide-react'
import * as React from 'react'
import { type Cloudflare, del, describe, post, put, type Step, type StorageConfig } from '@/api'
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
import { Item, ItemActions, ItemContent, ItemDescription, ItemTitle } from '@/components/ui/item'
import { Progress } from '@/components/ui/progress'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { notify } from '@/lib/notify'
import { q } from '@/queries'

// Delivery (docs/architecture.md, "Optional upgrades"): how videos reach
// viewers, built in or through Cloudflare, and where their files live.

export function Delivery() {
  return (
    <PageBody>
      <PageHeader
        title="Delivery & storage"
        description="How videos reach viewers, and where their files live."
      />
      <DeliverySection />
      <StorageSection />
    </PageBody>
  )
}

function DeliverySection() {
  const cf = useQuery(q.cloudflare())
  const [connecting, setConnecting] = React.useState(false)
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
                      : "Serve from Cloudflare's edge with a Worker, and store files on R2. Needs a zone on Workers Paid and one API token."}
                  </CardDescription>
                </CardHeader>
                {s.connected && <MoveProgress />}
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
            <ConnectCloudflare open={connecting} onOpenChange={setConnecting} status={s} />
          </>
        )}
      </QueryState>
    </Section>
  )
}

function MoveProgress() {
  const qc = useQueryClient()
  const moves = useQuery(q.moves())
  const move = useMutation({
    mutationFn: () => post('/v1/storage/moves'),
    onSuccess: () => {
      notify.done('Library move started', 'Progress also shows on Jobs.')
      void qc.invalidateQueries({ queryKey: ['moves'] })
    },
    onError: (err) => notify.failed("Couldn't start the move", err),
  })
  const m = moves.data
  if (!m || (m.not_on_default === 0 && m.active === 0 && m.moved === 0)) return null
  const total = m.not_on_default + m.moved
  return (
    <CardContent className="flex flex-col gap-2 text-sm">
      <div className="flex justify-between">
        <span className="text-muted-foreground">Library on R2</span>
        <span className="type-timecode">
          {m.moved} of {total}
          {m.failed > 0 && <span className="text-destructive">, {m.failed} failed</span>}
        </span>
      </div>
      <Progress value={total ? (m.moved / total) * 100 : 0} aria-label="Library moved to R2" />
      {m.not_on_default > m.active && (
        <div>
          <Button size="sm" variant="outline" onClick={() => move.mutate()} disabled={move.isPending}>
            Move {m.not_on_default - m.active} videos to R2
          </Button>
        </div>
      )}
      <span className="text-muted-foreground">Videos not moved yet play through this server.</span>
    </CardContent>
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
  const [steps, setSteps] = React.useState<Step[]>([])
  const connect = useMutation({
    mutationFn: (body: object) => post<{ connected: boolean; steps: Step[] }>('/v1/cloudflare/connect', body),
    onMutate: () => setSteps([]),
    onSuccess: (res) => {
      setSteps(res.steps)
      void qc.invalidateQueries({ queryKey: ['cloudflare'] })
      void qc.invalidateQueries({ queryKey: ['moves'] })
      void qc.invalidateQueries({ queryKey: ['storage'] })
      if (res.connected) notify.done('Cloudflare connected')
    },
  })
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) setSteps([])
        onOpenChange(o)
      }}
    >
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{status.connected ? 'Connect Cloudflare again' : 'Connect Cloudflare'}</DialogTitle>
          <DialogDescription>
            ClipMux creates the bucket, deploys the Worker and moves your media hostname to it. It uses the
            token for this request and keeps only its ID; the token's Workers and DNS permissions stay live in
            Cloudflare.
          </DialogDescription>
        </DialogHeader>
        <form
          className="flex max-h-[60vh] flex-col overflow-y-auto"
          onSubmit={(e) => {
            e.preventDefault()
            const f = new FormData(e.currentTarget)
            connect.mutate({
              token: f.get('token'),
              hostname: f.get('hostname'),
              bucket: f.get('bucket') || undefined,
              account_id: f.get('account_id') || undefined,
            })
          }}
        >
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="cf-token">API token</FieldLabel>
              <Input id="cf-token" name="token" type="password" autoComplete="off" required />
              <FieldDescription>
                With Workers Scripts, Workers Routes, R2 and DNS edit for the account and the media domain's
                zone, plus Zone Read and Billing Read.
              </FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="cf-hostname">Media hostname</FieldLabel>
              <Input
                id="cf-hostname"
                name="hostname"
                placeholder="media.example.com"
                required
                defaultValue={status.hostname ?? ''}
              />
            </Field>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field>
                <FieldLabel htmlFor="cf-bucket">R2 bucket</FieldLabel>
                <Input
                  id="cf-bucket"
                  name="bucket"
                  placeholder="clipmux-media"
                  defaultValue={status.bucket ?? ''}
                />
              </Field>
              <Field>
                <FieldLabel htmlFor="cf-account">Account ID</FieldLabel>
                <Input id="cf-account" name="account_id" placeholder="Only if the token reaches several" />
              </Field>
            </div>
            {connect.isError && <FieldError>{describe(connect.error)}</FieldError>}
            <Steps steps={steps} />
            <div className="flex flex-wrap items-center gap-3">
              <Button type="submit" disabled={connect.isPending}>
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

const kindNames: Record<StorageConfig['kind'], string> = {
  fs: "This server's disk",
  r2: 'Cloudflare R2',
  s3: 'S3',
}

function StorageSection() {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const storage = useQuery(q.storage())
  const [adding, setAdding] = React.useState(false)
  const [rotating, setRotating] = React.useState<StorageConfig | null>(null)
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['storage'] })
    void qc.invalidateQueries({ queryKey: ['moves'] })
  }
  const makeDefault = useMutation({
    mutationFn: (s: StorageConfig) => post(`/v1/storage/${s.id}/default`),
    onSuccess: () => {
      notify.done('Default storage changed', 'New uploads go there; existing videos stay until moved.')
      refresh()
    },
    onError: (err) => notify.failed("Couldn't change the default", err),
  })
  const remove = useMutation({
    mutationFn: (s: StorageConfig) => del(`/v1/storage/${s.id}`),
    onSuccess: () => {
      notify.done('Storage removed')
      refresh()
    },
    onError: (err) => notify.failed("Couldn't remove the storage", err),
  })
  return (
    <Section
      title="Storage"
      description="New uploads go to the default. A storage can be removed once no video uses it."
      actions={
        <Button variant="outline" size="sm" onClick={() => setAdding(true)}>
          <PlusIcon data-icon="inline-start" />
          Add S3 storage
        </Button>
      }
    >
      <QueryState query={storage} what="storage" skeleton={<Skeleton className="h-24 w-full" />}>
        {(data) => (
          <ul className="flex flex-col divide-y rounded-lg border bg-card" aria-label="Storage">
            {data.data.map((s) => (
              <li key={s.id} data-storage={s.kind}>
                <Item className="rounded-none">
                  <ItemContent>
                    <ItemTitle>
                      {kindNames[s.kind]}
                      {s.bucket && <span className="font-normal text-muted-foreground">{s.bucket}</span>}
                      {s.default && <Badge variant="tally">Default</Badge>}
                    </ItemTitle>
                    <ItemDescription>
                      {s.asset_count} video{s.asset_count === 1 ? '' : 's'}
                      {s.endpoint ? `, ${s.endpoint}` : ''}
                      {s.cloudflare_delivery ? ', managed by Connect Cloudflare' : ''}
                    </ItemDescription>
                  </ItemContent>
                  {s.kind !== 'fs' && (
                    <ItemActions className="flex-wrap">
                      {!s.default && (
                        <Button variant="outline" size="sm" onClick={() => makeDefault.mutate(s)}>
                          Make default
                        </Button>
                      )}
                      {s.kind === 's3' && (
                        <Button variant="outline" size="sm" onClick={() => setRotating(s)}>
                          Rotate keys
                        </Button>
                      )}
                      {!s.default && !s.cloudflare_delivery && (
                        <Button
                          variant="ghost"
                          size="sm"
                          className="text-destructive"
                          onClick={async () => {
                            const ok = await confirm({
                              title: `Remove ${s.bucket ?? 'this storage'}?`,
                              description:
                                'ClipMux forgets this storage and its credentials. Files in the bucket stay where they are.',
                              action: 'Remove storage',
                            })
                            if (ok) remove.mutate(s)
                          }}
                        >
                          Remove
                        </Button>
                      )}
                    </ItemActions>
                  )}
                </Item>
              </li>
            ))}
          </ul>
        )}
      </QueryState>
      <AddStorage open={adding} onOpenChange={setAdding} onDone={refresh} />
      <RotateKeys storage={rotating} onClose={() => setRotating(null)} />
    </Section>
  )
}

function AddStorage({
  open,
  onOpenChange,
  onDone,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  onDone: () => void
}) {
  const add = useMutation({
    mutationFn: (body: object) => post<StorageConfig>('/v1/storage', body),
    onSuccess: () => {
      notify.done('Storage added', 'Make it the default to send new uploads there.')
      onDone()
      onOpenChange(false)
    },
  })
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) add.reset()
        onOpenChange(o)
      }}
    >
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add S3 storage</DialogTitle>
          <DialogDescription>
            Any S3-compatible bucket. ClipMux writes, reads, lists and deletes a test object and sets CORS
            before saving.
          </DialogDescription>
        </DialogHeader>
        <form
          aria-label="Add S3 storage"
          onSubmit={(e) => {
            e.preventDefault()
            const f = new FormData(e.currentTarget)
            add.mutate({
              endpoint: f.get('endpoint'),
              bucket: f.get('bucket'),
              region: f.get('region') || undefined,
              public_endpoint: f.get('public_endpoint') || undefined,
              access_key_id: f.get('access_key_id'),
              secret_access_key: f.get('secret_access_key'),
            })
          }}
        >
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="s3-endpoint">Endpoint</FieldLabel>
              <Input
                id="s3-endpoint"
                name="endpoint"
                type="url"
                placeholder="https://s3.eu-central-1.amazonaws.com"
                required
              />
            </Field>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field>
                <FieldLabel htmlFor="s3-bucket">Bucket</FieldLabel>
                <Input id="s3-bucket" name="bucket" required />
              </Field>
              <Field>
                <FieldLabel htmlFor="s3-region">Region</FieldLabel>
                <Input id="s3-region" name="region" placeholder="Optional" />
              </Field>
            </div>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field>
                <FieldLabel htmlFor="s3-key">Access key ID</FieldLabel>
                <Input id="s3-key" name="access_key_id" autoComplete="off" required />
              </Field>
              <Field>
                <FieldLabel htmlFor="s3-secret">Secret access key</FieldLabel>
                <Input id="s3-secret" name="secret_access_key" type="password" autoComplete="off" required />
              </Field>
            </div>
            <Field>
              <FieldLabel htmlFor="s3-public">Public endpoint</FieldLabel>
              <Input id="s3-public" name="public_endpoint" type="url" placeholder="Optional" />
              <FieldDescription>
                Where browsers send upload parts, when it differs from the endpoint.
              </FieldDescription>
            </Field>
            {add.isError && <FieldError>{describe(add.error)}</FieldError>}
            <div>
              <Button type="submit" disabled={add.isPending}>
                {add.isPending && <Spinner data-icon="inline-start" />}
                Check and add
              </Button>
            </div>
          </FieldGroup>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function RotateKeys({ storage, onClose }: { storage: StorageConfig | null; onClose: () => void }) {
  const rotate = useMutation({
    mutationFn: (body: object) => put(`/v1/storage/${storage?.id}/credentials`, body),
    onSuccess: () => {
      notify.done('Keys rotated', 'The old keys can be deleted at the provider now.')
      onClose()
    },
  })
  return (
    <Dialog
      open={storage !== null}
      onOpenChange={(o) => {
        if (!o) {
          rotate.reset()
          onClose()
        }
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Rotate keys for {storage?.bucket}</DialogTitle>
          <DialogDescription>
            The old keys keep working until the new ones pass the same checks.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            const f = new FormData(e.currentTarget)
            rotate.mutate({
              access_key_id: f.get('access_key_id'),
              secret_access_key: f.get('secret_access_key'),
            })
          }}
        >
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="rot-key">New access key ID</FieldLabel>
              <Input id="rot-key" name="access_key_id" autoComplete="off" required />
            </Field>
            <Field>
              <FieldLabel htmlFor="rot-secret">New secret access key</FieldLabel>
              <Input id="rot-secret" name="secret_access_key" type="password" autoComplete="off" required />
            </Field>
            {rotate.isError && <FieldError>{describe(rotate.error)}</FieldError>}
            <div>
              <Button type="submit" disabled={rotate.isPending}>
                {rotate.isPending && <Spinner data-icon="inline-start" />}
                Check and rotate
              </Button>
            </div>
          </FieldGroup>
        </form>
      </DialogContent>
    </Dialog>
  )
}
