import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { PlusIcon } from 'lucide-react'
import * as React from 'react'
import { Link } from 'react-router'
import { del, describe, post, put, type Step, type StepResult, type StorageConfig } from '@/api'
import { CreateTokenLink, useInspection } from '@/components/cloudflare-token'
import { useConfirm } from '@/components/confirm'
import { PageBody, PageHeader, Section } from '@/components/page'
import { QueryState } from '@/components/query-state'
import { Steps } from '@/components/steps'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Item, ItemActions, ItemContent, ItemDescription, ItemTitle } from '@/components/ui/item'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Progress } from '@/components/ui/progress'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { notify } from '@/lib/notify'
import { q } from '@/queries'

// Storage (D099, D110): where uploads and their encoded files are kept. The
// owner picks the store new uploads go to (this server's disk, an R2 bucket
// or any S3 bucket) and moves the library there.

export const kindNames: Record<StorageConfig['kind'], string> = {
  fs: "This server's disk",
  r2: 'Cloudflare R2',
  s3: 'S3',
}

/** A store's name as the owner knows it: its kind, and its bucket if it has one. */
export function storageName(s: StorageConfig) {
  return s.bucket ? `${kindNames[s.kind]} (${s.bucket})` : kindNames[s.kind]
}

const DEFAULT_BUCKET = 'clipmux-media'

export function Storage() {
  const storage = useQuery(q.storage())
  const def = storage.data?.data.find((s) => s.default)
  return (
    <PageBody>
      <PageHeader
        title="Storage"
        description={
          def
            ? `New uploads go to ${storageName(def)}. Existing videos stay where they are until you move them.`
            : 'Where uploaded videos and their encoded files are kept.'
        }
      />
      <StorageSection />
    </PageBody>
  )
}

function StorageSection() {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const storage = useQuery(q.storage())
  const [adding, setAdding] = React.useState<'r2' | 's3' | null>(null)
  const [rotating, setRotating] = React.useState<StorageConfig | null>(null)
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['storage'] })
    void qc.invalidateQueries({ queryKey: ['moves'] })
  }
  const makeDefault = useMutation({
    mutationFn: (s: StorageConfig) => post(`/v1/storage/${s.id}/default`),
    onSuccess: (_, s) => {
      notify.done(`New uploads go to ${storageName(s)}`, 'Existing videos stay where they are until moved.')
      refresh()
    },
    onError: (err) => notify.failed("Couldn't change where uploads go", err),
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
      title="Where uploads are stored"
      description="New uploads go to the default. A storage can be removed once no video uses it."
      actions={
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => setAdding('r2')}>
            <PlusIcon data-icon="inline-start" />
            Add R2 bucket
          </Button>
          <Button variant="outline" size="sm" onClick={() => setAdding('s3')}>
            <PlusIcon data-icon="inline-start" />
            Add S3 storage
          </Button>
        </div>
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
                      {s.cloudflare_delivery ? ', read by the Cloudflare Worker' : ''}
                    </ItemDescription>
                  </ItemContent>
                  <ItemActions className="flex-wrap">
                    {!s.default && (
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={makeDefault.isPending}
                        onClick={() => makeDefault.mutate(s)}
                      >
                        Use for new uploads
                      </Button>
                    )}
                    {s.kind === 's3' && (
                      <Button variant="outline" size="sm" onClick={() => setRotating(s)}>
                        Rotate keys
                      </Button>
                    )}
                    {s.kind !== 'fs' && !s.default && !s.cloudflare_delivery && (
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
                </Item>
              </li>
            ))}
          </ul>
        )}
      </QueryState>
      <MoveProgress />
      <p className="text-sm text-muted-foreground">
        Serving from Cloudflare's edge is set up under{' '}
        <Link to="/delivery" className="text-link underline-offset-4 hover:underline">
          Delivery
        </Link>
        ; it reads its own R2 bucket and plays everything else through this server.
      </p>
      <AddR2 open={adding === 'r2'} onOpenChange={(o) => setAdding(o ? 'r2' : null)} onDone={refresh} />
      <AddS3 open={adding === 's3'} onOpenChange={(o) => setAdding(o ? 's3' : null)} onDone={refresh} />
      <RotateKeys storage={rotating} onClose={() => setRotating(null)} />
    </Section>
  )
}

/** The library move: videos not on the default yet, and moving them there. */
function MoveProgress() {
  const qc = useQueryClient()
  const moves = useQuery(q.moves())
  const storage = useQuery(q.storage())
  const move = useMutation({
    mutationFn: () => post('/v1/storage/moves'),
    onSuccess: () => {
      notify.done('Library move started', 'Progress also shows on Jobs.')
      void qc.invalidateQueries({ queryKey: ['moves'] })
    },
    onError: (err) => notify.failed("Couldn't start the move", err),
  })
  const m = moves.data
  const def = storage.data?.data.find((s) => s.default)
  if (!m || !def || (m.not_on_default === 0 && m.active === 0 && m.moved === 0)) return null
  const total = m.not_on_default + m.moved
  const name = storageName(def)
  return (
    <section className="flex flex-col gap-2 rounded-lg border bg-card p-4 text-sm" aria-label="Library move">
      <div className="flex justify-between gap-4">
        <span className="text-muted-foreground">Library on {name}</span>
        <span className="type-timecode">
          {m.moved} of {total}
          {m.failed > 0 && <span className="text-destructive">, {m.failed} failed</span>}
        </span>
      </div>
      <Progress value={total ? (m.moved / total) * 100 : 0} aria-label={`Library moved to ${name}`} />
      {m.not_on_default > m.active && (
        <div>
          <Button size="sm" variant="outline" onClick={() => move.mutate()} disabled={move.isPending}>
            Move {m.not_on_default - m.active} videos to {name}
          </Button>
        </div>
      )}
    </section>
  )
}

/**
 * An R2 bucket as storage (D110), without moving delivery: a token that can
 * only manage R2, and a bucket, existing or created now.
 */
function AddR2({
  open,
  onOpenChange,
  onDone,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  onDone: () => void
}) {
  const cloudflare = useQuery(q.cloudflare())
  const [token, setToken] = React.useState('')
  const [accountID, setAccountID] = React.useState('')
  const [bucket, setBucket] = React.useState(DEFAULT_BUCKET)
  const [useIt, setUseIt] = React.useState(true)
  const [steps, setSteps] = React.useState<Step[]>([])
  const inspect = useInspection(open ? token : '', accountID)
  const add = useMutation({
    mutationFn: (body: object) => post<StepResult>('/v1/storage/r2', body),
    onMutate: () => setSteps([]),
    onSuccess: (res) => {
      setSteps(res.steps)
      if (res.connected) {
        notify.done(
          'R2 bucket added',
          useIt ? `New uploads go to ${bucket}.` : 'Use it for new uploads any time.',
        )
        onDone()
        close()
      }
    },
  })
  const close = () => {
    setToken('')
    setAccountID('')
    setBucket(DEFAULT_BUCKET)
    setUseIt(true)
    setSteps([])
    add.reset()
    onOpenChange(false)
  }
  const found = inspect.data
  const needAccount = !!found && found.accounts.length > 1 && !found.account_id
  const buckets = found?.buckets ?? []
  const listable = !!found?.account_id && !found.unreadable?.includes('buckets')
  return (
    <Dialog open={open} onOpenChange={(o) => (o ? onOpenChange(true) : close())}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Add R2 bucket</DialogTitle>
          <DialogDescription>
            Keep uploads in Cloudflare R2. ClipMux creates the bucket if it's new and checks it with a test
            write. Delivery doesn't change: connect Cloudflare under Delivery to serve from the edge.
          </DialogDescription>
        </DialogHeader>
        <form
          aria-label="Add R2 bucket"
          className="flex max-h-[60vh] flex-col overflow-y-auto"
          onSubmit={(e) => {
            e.preventDefault()
            add.mutate({ token: token.trim(), bucket, account_id: found?.account_id, default: useIt })
          }}
        >
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="r2-token">API token</FieldLabel>
              {cloudflare.data && <CreateTokenLink url={cloudflare.data.r2_token_url} />}
              <Input
                id="r2-token"
                name="token"
                type="password"
                autoComplete="off"
                required
                value={token}
                onChange={(e) => setToken(e.target.value)}
              />
              <FieldDescription>
                Cloudflare opens with R2 Storage: Edit already chosen. Create the token and paste it here.
                ClipMux keeps only its ID; R2's keys derive from it, so keep the token in Cloudflare.
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
                <FieldLabel htmlFor="r2-account">Account</FieldLabel>
                <NativeSelect
                  id="r2-account"
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
            {found && !needAccount && (
              <Field>
                <FieldLabel htmlFor="r2-bucket">Bucket</FieldLabel>
                {listable ? (
                  <NativeSelect
                    id="r2-bucket"
                    name="bucket"
                    value={bucket}
                    onChange={(e) => setBucket(e.target.value)}
                  >
                    {!buckets.includes(DEFAULT_BUCKET) && (
                      <NativeSelectOption value={DEFAULT_BUCKET}>{DEFAULT_BUCKET} (new)</NativeSelectOption>
                    )}
                    {buckets.map((b) => (
                      <NativeSelectOption key={b} value={b}>
                        {b}
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                ) : (
                  <Input
                    id="r2-bucket"
                    name="bucket"
                    placeholder={DEFAULT_BUCKET}
                    value={bucket}
                    onChange={(e) => setBucket(e.target.value)}
                  />
                )}
                <FieldDescription>
                  {listable
                    ? 'An existing bucket, or a new one ClipMux creates.'
                    : 'Created if it doesn’t exist.'}
                </FieldDescription>
              </Field>
            )}
            {found && !needAccount && (
              <Field orientation="horizontal">
                <Checkbox id="r2-default" checked={useIt} onCheckedChange={(v) => setUseIt(v === true)} />
                <FieldLabel htmlFor="r2-default" className="font-normal">
                  Use it for new uploads
                </FieldLabel>
              </Field>
            )}
            {add.isError && <FieldError>{describe(add.error)}</FieldError>}
            <Steps steps={steps} />
            <div>
              <Button type="submit" disabled={add.isPending || !found || needAccount}>
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

function AddS3({
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
      notify.done('Storage added', 'Use it for new uploads to send them there.')
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
            before saving. For Cloudflare R2, use Add R2 bucket instead.
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
