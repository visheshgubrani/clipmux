import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronRightIcon, PlusIcon } from 'lucide-react'
import * as React from 'react'
import { Link, useParams } from 'react-router'
import { type App, type Credentials, del, describe, post, type WebhookEndpoint } from '@/api'
import { useConfirm } from '@/components/confirm'
import { CopyBlock, CopyField } from '@/components/copy'
import { PageBody, PageHeader, Section } from '@/components/page'
import { QueryState } from '@/components/query-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '@/components/ui/empty'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Item, ItemActions, ItemContent, ItemDescription, ItemTitle } from '@/components/ui/item'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { ago } from '@/lib/format'
import { notify } from '@/lib/notify'
import { q } from '@/queries'

// Apps & API (D025): each site or service that uses ClipMux gets an app with
// its own API keys, signing keys and webhooks, and sees only its own videos.

interface SecretShown {
  title: string
  items: { label: string; value: string; multiline?: boolean }[]
}

/** Secrets appear once: ClipMux keeps only a hash. */
function SecretsDialog({ shown, onClose }: { shown: SecretShown | null; onClose: () => void }) {
  return (
    <Dialog open={shown !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{shown?.title ?? 'Save this now'}</DialogTitle>
          <DialogDescription>
            It's shown once. ClipMux keeps only a hash, so it can't show it again.
          </DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-4">
          {shown?.items.map((s) => (
            <div key={s.label} className="flex flex-col gap-1.5">
              <span className="text-sm font-medium">{s.label}</span>
              {s.multiline ? (
                <CopyBlock label={s.label} value={s.value} />
              ) : (
                <CopyField label={s.label} value={s.value} />
              )}
            </div>
          ))}
        </div>
        <DialogFooter>
          <Button onClick={onClose}>I've saved it</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

/** The one command that connects an app's backend: it pairs through /connect. */
function initCommand(server: string) {
  return `npx @clipmux/node init --server ${server.replace(/\/+$/, '')}`
}

/**
 * Connect your app (P6-T23): the init one-liner, which pairs the backend and
 * writes its env file without anyone copying a secret, and for an app's page
 * the same credentials as a .env block shown once.
 */
function ConnectYourApp({
  server,
  app,
  keysAllowed,
  onSecret,
}: {
  server: string
  app?: App
  keysAllowed: boolean
  onSecret?: (s: SecretShown) => void
}) {
  const qc = useQueryClient()
  const [creating, setCreating] = React.useState(false)
  const [local, setLocal] = React.useState(false)
  const create = useMutation({
    mutationFn: (webhook: string) =>
      post<Credentials>(`/v1/apps/${app?.id}/credentials`, {
        ...(webhook ? { webhook_url: webhook } : {}),
        ...(keysAllowed ? {} : { local: true }),
      }),
    onSuccess: (c) => {
      // Its keys and endpoint show in the tabs below.
      void qc.invalidateQueries({ queryKey: ['apps', c.app_id] })
      setCreating(false)
      onSecret?.({
        title: `Credentials for ${c.app_name}`,
        items: [{ label: '.env', value: c.env, multiline: true }],
      })
    },
  })
  return (
    <Section
      title="Connect your app"
      description="Run this in your app's project. It shows a code, you approve it here, and it writes the CLIPMUX_* variables to .env.local without printing a secret."
      actions={
        app && (
          <Button
            size="sm"
            variant="outline"
            onClick={() => {
              setLocal(false)
              create.reset()
              setCreating(true)
            }}
          >
            Create credentials
          </Button>
        )
      }
    >
      <CopyField label="Connect command" value={initCommand(server)} />
      {app && (
        <Dialog open={creating} onOpenChange={setCreating}>
          <DialogContent>
            <DialogHeader>
              <DialogTitle>Create credentials</DialogTitle>
              <DialogDescription>
                A new API key and signing key for {app.name}, as a .env block for your backend. It's shown
                once.
              </DialogDescription>
            </DialogHeader>
            <form
              onSubmit={(e) => {
                e.preventDefault()
                create.mutate(String(new FormData(e.currentTarget).get('webhook_url') ?? '').trim())
              }}
            >
              <FieldGroup>
                <Field data-invalid={create.isError || undefined}>
                  <FieldLabel htmlFor="credentials-webhook">Webhook URL (optional)</FieldLabel>
                  <Input
                    id="credentials-webhook"
                    name="webhook_url"
                    type="url"
                    placeholder="https://example.com/api/clipmux/webhook"
                  />
                  <FieldDescription>Adds an endpoint and its secret to the block.</FieldDescription>
                  {create.isError && <FieldError>{describe(create.error)}</FieldError>}
                </Field>
                {!keysAllowed && (
                  <Field orientation="horizontal">
                    <Checkbox
                      id="credentials-local"
                      checked={local}
                      onCheckedChange={(v) => setLocal(v === true)}
                    />
                    <FieldLabel htmlFor="credentials-local" className="font-normal">
                      These are for apps on this computer (the public URL is localhost)
                    </FieldLabel>
                  </Field>
                )}
                <DialogFooter>
                  <Button type="submit" disabled={create.isPending || (!keysAllowed && !local)}>
                    {create.isPending && <Spinner data-icon="inline-start" />}
                    Create credentials
                  </Button>
                </DialogFooter>
              </FieldGroup>
            </form>
          </DialogContent>
        </Dialog>
      )}
    </Section>
  )
}

export function Apps({ server, keysAllowed }: { server: string; keysAllowed: boolean }) {
  const qc = useQueryClient()
  const apps = useQuery(q.apps())
  const [creating, setCreating] = React.useState(false)
  const create = useMutation({
    mutationFn: (name: string) => post<App>('/v1/apps', { name }),
    onSuccess: (app) => {
      void qc.invalidateQueries({ queryKey: ['apps'] })
      setCreating(false)
      notify.done(
        `${app.name} created`,
        'Connect it with the command below, or create its credentials on its page.',
      )
    },
  })
  return (
    <PageBody>
      <PageHeader
        title="Apps & API"
        description="Each site or service that uses ClipMux gets an app with its own keys and webhooks, and sees only its own videos."
        actions={
          <Button onClick={() => setCreating(true)}>
            <PlusIcon data-icon="inline-start" />
            Create app
          </Button>
        }
      />
      <QueryState query={apps} what="your apps" skeleton={<Skeleton className="h-32 w-full" />}>
        {(data) => (
          <ul className="flex flex-col divide-y rounded-lg border bg-card" aria-label="Apps">
            {data.data.map((a) => (
              <li key={a.id}>
                <Item className="rounded-none" render={<Link to={`/apps/${a.id}`} />}>
                  <ItemContent>
                    <ItemTitle>
                      {a.name}
                      {a.builtin && <Badge variant="secondary">Built in</Badge>}
                      {a.disabled && <Badge variant="destructive">Disabled</Badge>}
                    </ItemTitle>
                    <ItemDescription>
                      {a.builtin ? 'Your own uploads from this dashboard.' : `Created ${ago(a.created_at)}`}
                    </ItemDescription>
                  </ItemContent>
                  <ItemActions>
                    <ChevronRightIcon className="size-4 text-muted-foreground" />
                  </ItemActions>
                </Item>
              </li>
            ))}
          </ul>
        )}
      </QueryState>
      <ConnectYourApp server={server} keysAllowed={keysAllowed} />
      <Dialog open={creating} onOpenChange={setCreating}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create app</DialogTitle>
            <DialogDescription>Name it after the site or service that will use it.</DialogDescription>
          </DialogHeader>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              create.mutate(String(new FormData(e.currentTarget).get('name')).trim())
            }}
          >
            <FieldGroup>
              <Field data-invalid={create.isError || undefined}>
                <FieldLabel htmlFor="app-name">Name</FieldLabel>
                <Input
                  id="app-name"
                  name="name"
                  required
                  maxLength={100}
                  placeholder="Course site"
                  autoFocus
                />
                {create.isError && <FieldError>{describe(create.error)}</FieldError>}
              </Field>
              <DialogFooter>
                <Button type="submit" disabled={create.isPending}>
                  {create.isPending && <Spinner data-icon="inline-start" />}
                  Create app
                </Button>
              </DialogFooter>
            </FieldGroup>
          </form>
        </DialogContent>
      </Dialog>
    </PageBody>
  )
}

export function AppDetail({ server, keysAllowed }: { server: string; keysAllowed: boolean }) {
  const { id = '' } = useParams()
  const apps = useQuery(q.apps())
  const app = apps.data?.data.find((a) => a.id === id)
  const [secret, setSecret] = React.useState<SecretShown | null>(null)
  return (
    <PageBody className="gap-6">
      <Breadcrumb>
        <BreadcrumbList>
          <BreadcrumbItem>
            <BreadcrumbLink render={<Link to="/apps" />}>Apps & API</BreadcrumbLink>
          </BreadcrumbItem>
          <BreadcrumbSeparator />
          <BreadcrumbItem>
            <BreadcrumbPage>{app?.name ?? id}</BreadcrumbPage>
          </BreadcrumbItem>
        </BreadcrumbList>
      </Breadcrumb>
      <QueryState query={apps} what="this app" skeleton={<Skeleton className="h-40 w-full" />}>
        {() =>
          !app ? (
            <Empty>
              <EmptyHeader>
                <EmptyTitle>No such app</EmptyTitle>
                <EmptyDescription>It may have been removed.</EmptyDescription>
              </EmptyHeader>
            </Empty>
          ) : (
            <div className="flex flex-col gap-6">
              <PageHeader
                title={app.name}
                description={<code className="text-xs">{app.id}</code>}
                actions={
                  app.disabled ? (
                    <Badge variant="destructive">Disabled</Badge>
                  ) : (
                    <Badge variant="success">Enabled</Badge>
                  )
                }
              />
              {!app.builtin && (
                <ConnectYourApp server={server} app={app} keysAllowed={keysAllowed} onSecret={setSecret} />
              )}
              <Tabs defaultValue="keys">
                <TabsList
                  variant="line"
                  className="no-scrollbar w-full justify-start overflow-x-auto border-b *:flex-none! *:px-3!"
                >
                  <TabsTrigger value="keys">API keys</TabsTrigger>
                  <TabsTrigger value="signing">Signing keys</TabsTrigger>
                  <TabsTrigger value="webhooks">Webhooks</TabsTrigger>
                  {!app.builtin && <TabsTrigger value="danger">Danger zone</TabsTrigger>}
                </TabsList>
                <TabsContent value="keys" className="pt-6">
                  <ApiKeys app={app} keysAllowed={keysAllowed} onSecret={setSecret} />
                </TabsContent>
                <TabsContent value="signing" className="pt-6">
                  <SigningKeys app={app} onSecret={setSecret} />
                </TabsContent>
                <TabsContent value="webhooks" className="pt-6">
                  <Webhooks app={app} onSecret={setSecret} />
                </TabsContent>
                {!app.builtin && (
                  <TabsContent value="danger" className="pt-6">
                    <DangerZone app={app} />
                  </TabsContent>
                )}
              </Tabs>
            </div>
          )
        }
      </QueryState>
      <SecretsDialog shown={secret} onClose={() => setSecret(null)} />
    </PageBody>
  )
}

function ApiKeys({
  app,
  keysAllowed,
  onSecret,
}: {
  app: App
  keysAllowed: boolean
  onSecret: (s: SecretShown) => void
}) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const keys = useQuery(q.apiKeys(app.id))
  const [naming, setNaming] = React.useState(false)
  const [local, setLocal] = React.useState(false)
  const create = useMutation({
    mutationFn: (name: string) =>
      post<{ key: string; name: string }>(
        `/v1/apps/${app.id}/api-keys`,
        keysAllowed ? { name } : { name, local: true },
      ),
    onSuccess: (k) => {
      void qc.invalidateQueries({ queryKey: ['apps', app.id, 'keys'] })
      setNaming(false)
      onSecret({ title: `API key "${k.name}"`, items: [{ label: 'API key', value: k.key }] })
    },
  })
  const revoke = useMutation({
    mutationFn: (id: string) => del(`/v1/api-keys/${id}`),
    onSuccess: () => {
      notify.done('Key revoked')
      void qc.invalidateQueries({ queryKey: ['apps', app.id, 'keys'] })
    },
    onError: (err) => notify.failed("Couldn't revoke the key", err),
  })
  return (
    <Section
      title="API keys"
      description="A key can upload, read and change this app's videos, never settings."
      actions={
        <Button
          size="sm"
          onClick={() => {
            setLocal(false)
            create.reset()
            setNaming(true)
          }}
        >
          Create API key
        </Button>
      }
    >
      <QueryState query={keys} what="the API keys" skeleton={<Skeleton className="h-20 w-full" />}>
        {(data) =>
          data.data.length === 0 ? (
            <p className="text-sm text-muted-foreground">No keys yet.</p>
          ) : (
            <div className="overflow-x-auto rounded-lg border bg-card">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Name</TableHead>
                    <TableHead>Scopes</TableHead>
                    <TableHead>Last used</TableHead>
                    <TableHead>Created</TableHead>
                    <TableHead>
                      <span className="sr-only">Actions</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {data.data.map((k) => (
                    <TableRow key={k.id}>
                      <TableCell className="font-medium">{k.name}</TableCell>
                      <TableCell className="text-muted-foreground">{k.scopes.join(', ')}</TableCell>
                      <TableCell className="text-muted-foreground">
                        {k.revoked_at ? 'Revoked' : k.last_used_at ? ago(k.last_used_at) : 'Never'}
                      </TableCell>
                      <TableCell className="text-muted-foreground">{ago(k.created_at)}</TableCell>
                      <TableCell className="text-right">
                        {!k.revoked_at && (
                          <Button
                            variant="ghost"
                            size="sm"
                            className="text-destructive"
                            onClick={async () => {
                              const ok = await confirm({
                                title: `Revoke "${k.name}"?`,
                                description:
                                  "Anything using this key stops working at once. You can create a new one, but this one can't come back.",
                                action: 'Revoke key',
                              })
                              if (ok) revoke.mutate(k.id)
                            }}
                          >
                            Revoke
                          </Button>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )
        }
      </QueryState>
      <Dialog open={naming} onOpenChange={setNaming}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Create API key</DialogTitle>
            <DialogDescription>
              Name it after where it will live, so you know what to revoke.
            </DialogDescription>
          </DialogHeader>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              create.mutate(String(new FormData(e.currentTarget).get('name')).trim())
            }}
          >
            <FieldGroup>
              <Field data-invalid={create.isError || undefined}>
                <FieldLabel htmlFor="key-name">Name</FieldLabel>
                <Input id="key-name" name="name" maxLength={100} placeholder="Production backend" autoFocus />
                {create.isError && <FieldError>{describe(create.error)}</FieldError>}
              </Field>
              {!keysAllowed && (
                <Alert>
                  <AlertDescription className="flex flex-col gap-3">
                    <span>
                      The public URL is localhost, so upload links this key gets point at this computer. Apps
                      elsewhere can't use them until you{' '}
                      <Link to="/settings" className="text-link underline-offset-4 hover:underline">
                        set a public URL in Settings
                      </Link>
                      ; the key keeps working after that.
                    </span>
                    <Field orientation="horizontal">
                      <Checkbox
                        id="key-local"
                        checked={local}
                        onCheckedChange={(v) => setLocal(v === true)}
                      />
                      <FieldLabel htmlFor="key-local" className="font-normal">
                        This key is for apps on this computer
                      </FieldLabel>
                    </Field>
                  </AlertDescription>
                </Alert>
              )}
              <DialogFooter>
                <Button type="submit" disabled={create.isPending || (!keysAllowed && !local)}>
                  {create.isPending && <Spinner data-icon="inline-start" />}
                  Create key
                </Button>
              </DialogFooter>
            </FieldGroup>
          </form>
        </DialogContent>
      </Dialog>
    </Section>
  )
}

function SigningKeys({ app, onSecret }: { app: App; onSecret: (s: SecretShown) => void }) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const keys = useQuery(q.signingKeys(app.id))
  const create = useMutation({
    mutationFn: () => post<{ kid: string; private_key_pem: string }>(`/v1/apps/${app.id}/signing-keys`),
    onSuccess: (k) => {
      void qc.invalidateQueries({ queryKey: ['apps', app.id, 'signing'] })
      onSecret({
        title: `Signing key ${k.kid}`,
        items: [{ label: 'Private key (PEM)', value: k.private_key_pem, multiline: true }],
      })
    },
    onError: (err) => notify.failed("Couldn't create the signing key", err),
  })
  const revoke = useMutation({
    mutationFn: (kid: string) => del(`/v1/signing-keys/${kid}`),
    onSuccess: () => {
      notify.done('Signing key revoked')
      void qc.invalidateQueries({ queryKey: ['apps', app.id, 'signing'] })
    },
    onError: (err) => notify.failed("Couldn't revoke the signing key", err),
  })
  return (
    <Section
      title="Signing keys"
      description="Your backend signs playback tokens (ES256) with the private key; ClipMux keeps only the public half. Keep two during a rotation."
      actions={
        <Button size="sm" onClick={() => create.mutate()} disabled={create.isPending}>
          Create signing key
        </Button>
      }
    >
      <QueryState query={keys} what="the signing keys" skeleton={<Skeleton className="h-20 w-full" />}>
        {(data) =>
          data.data.length === 0 ? (
            <p className="text-sm text-muted-foreground">No signing keys yet.</p>
          ) : (
            <ul className="flex flex-col divide-y rounded-lg border bg-card">
              {data.data.map((k) => (
                <li key={k.kid}>
                  <Item className="rounded-none">
                    <ItemContent>
                      <ItemTitle className="font-mono text-[13px]">{k.kid}</ItemTitle>
                      <ItemDescription>
                        {k.revoked_at
                          ? 'Revoked'
                          : k.retired_at
                            ? `Retired ${ago(k.retired_at)}`
                            : `Created ${ago(k.created_at)}`}
                      </ItemDescription>
                    </ItemContent>
                    {!k.revoked_at && (
                      <ItemActions>
                        <Button
                          variant="ghost"
                          size="sm"
                          className="text-destructive"
                          onClick={async () => {
                            const ok = await confirm({
                              title: 'Revoke this signing key?',
                              description: 'Tokens it signed stop playing at once.',
                              action: 'Revoke key',
                            })
                            if (ok) revoke.mutate(k.kid)
                          }}
                        >
                          Revoke
                        </Button>
                      </ItemActions>
                    )}
                  </Item>
                </li>
              ))}
            </ul>
          )
        }
      </QueryState>
    </Section>
  )
}

function Webhooks({ app, onSecret }: { app: App; onSecret: (s: SecretShown) => void }) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const hooks = useQuery(q.webhooks(app.id))
  const [adding, setAdding] = React.useState(false)
  const [log, setLog] = React.useState<WebhookEndpoint | null>(null)
  const refresh = () => qc.invalidateQueries({ queryKey: ['apps', app.id, 'webhooks'] })
  const add = useMutation({
    mutationFn: (url: string) => post<{ secret: string }>(`/v1/apps/${app.id}/webhook-endpoints`, { url }),
    onSuccess: (h) => {
      void refresh()
      setAdding(false)
      onSecret({ title: 'Webhook signing secret', items: [{ label: 'Webhook secret', value: h.secret }] })
    },
  })
  const remove = useMutation({
    mutationFn: (id: string) => del(`/v1/webhook-endpoints/${id}`),
    onSuccess: () => {
      notify.done('Endpoint removed')
      void refresh()
    },
    onError: (err) => notify.failed("Couldn't remove the endpoint", err),
  })
  const newSecret = useMutation({
    mutationFn: (id: string) => post<{ secret: string }>(`/v1/webhook-endpoints/${id}/secret`),
    onSuccess: (s) => {
      void refresh()
      onSecret({
        title: 'New webhook secret',
        items: [{ label: 'New webhook secret', value: s.secret }],
      })
    },
    onError: (err) => notify.failed("Couldn't create a new secret", err),
  })
  const activate = useMutation({
    mutationFn: (id: string) => post(`/v1/webhook-endpoints/${id}/secret/activate`),
    onSuccess: () => {
      notify.done('New secret active', 'Deliveries are signed with it from the next try.')
      void refresh()
    },
    onError: (err) => notify.failed("Couldn't activate the secret", err),
  })
  return (
    <Section
      title="Webhooks"
      description="ClipMux tells your app when a video is ready, changes or is deleted, signed with each endpoint's secret."
      actions={
        <Button size="sm" onClick={() => setAdding(true)}>
          Add endpoint
        </Button>
      }
    >
      <QueryState query={hooks} what="the webhooks" skeleton={<Skeleton className="h-20 w-full" />}>
        {(data) =>
          data.data.filter((h) => !h.disabled_at).length === 0 ? (
            <Empty className="border border-dashed">
              <EmptyHeader>
                <EmptyTitle>No webhooks yet</EmptyTitle>
                <EmptyDescription>
                  Add an endpoint and ClipMux will tell your app when a video is ready.
                </EmptyDescription>
              </EmptyHeader>
            </Empty>
          ) : (
            <ul className="flex flex-col divide-y rounded-lg border bg-card">
              {data.data
                .filter((h) => !h.disabled_at)
                .map((h) => {
                  const skipped = h.skipped_until && Date.parse(h.skipped_until) > Date.now()
                  return (
                    <li key={h.id} className="flex flex-col gap-3 p-4" data-endpoint={h.id}>
                      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
                        <code className="min-w-0 flex-[1_1_16rem] truncate text-[13px]">{h.url}</code>
                        {skipped ? (
                          <Badge variant="destructive">Paused after {h.consecutive_failures} failures</Badge>
                        ) : h.consecutive_failures > 0 ? (
                          <Badge variant="warning">{h.consecutive_failures} failures in a row</Badge>
                        ) : (
                          <Badge variant="success">Healthy</Badge>
                        )}
                      </div>
                      {h.pending_secret_since && (
                        <Alert>
                          <AlertDescription>
                            A new secret is waiting. Deliveries stay signed with the old one until you
                            activate it, once your app accepts both.
                          </AlertDescription>
                        </Alert>
                      )}
                      <div className="flex flex-wrap gap-2">
                        <Button variant="outline" size="sm" onClick={() => setLog(h)}>
                          Deliveries
                        </Button>
                        {h.pending_secret_since ? (
                          <Button
                            size="sm"
                            onClick={() => activate.mutate(h.id)}
                            disabled={activate.isPending}
                          >
                            Activate new secret
                          </Button>
                        ) : (
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={async () => {
                              const ok = await confirm({
                                title: 'Create a new secret?',
                                description:
                                  "You'll see it once. Deliveries stay signed with the current secret until you activate the new one.",
                                action: 'Create secret',
                                destructive: false,
                              })
                              if (ok) newSecret.mutate(h.id)
                            }}
                          >
                            Rotate secret
                          </Button>
                        )}
                        <Button
                          variant="ghost"
                          size="sm"
                          className="text-destructive"
                          onClick={async () => {
                            const ok = await confirm({
                              title: 'Remove this endpoint?',
                              description: `ClipMux stops sending events to ${h.url}. Deliveries in flight are dropped.`,
                              action: 'Remove endpoint',
                            })
                            if (ok) remove.mutate(h.id)
                          }}
                        >
                          Remove
                        </Button>
                      </div>
                    </li>
                  )
                })}
            </ul>
          )
        }
      </QueryState>
      <Dialog open={adding} onOpenChange={setAdding}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Add endpoint</DialogTitle>
            <DialogDescription>An https URL on your app that accepts POSTed events.</DialogDescription>
          </DialogHeader>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              add.mutate(String(new FormData(e.currentTarget).get('url')).trim())
            }}
          >
            <FieldGroup>
              <Field data-invalid={add.isError || undefined}>
                <FieldLabel htmlFor="hook-url">URL</FieldLabel>
                <Input
                  id="hook-url"
                  name="url"
                  type="url"
                  required
                  placeholder="https://your-app.example.com/webhooks/clipmux"
                  autoFocus
                />
                <FieldDescription>Each delivery carries an HMAC of its timestamp and body.</FieldDescription>
                {add.isError && <FieldError>{describe(add.error)}</FieldError>}
              </Field>
              <DialogFooter>
                <Button type="submit" disabled={add.isPending}>
                  {add.isPending && <Spinner data-icon="inline-start" />}
                  Add endpoint
                </Button>
              </DialogFooter>
            </FieldGroup>
          </form>
        </DialogContent>
      </Dialog>
      <DeliveriesSheet endpoint={log} onClose={() => setLog(null)} />
    </Section>
  )
}

function DeliveriesSheet({ endpoint, onClose }: { endpoint: WebhookEndpoint | null; onClose: () => void }) {
  const qc = useQueryClient()
  const deliveries = useQuery({ ...q.deliveries(endpoint?.id ?? ''), enabled: !!endpoint })
  const redeliver = useMutation({
    mutationFn: (id: string) => post(`/v1/webhook-deliveries/${id}/redeliver`),
    onSuccess: () => {
      notify.done('Redelivery queued')
      void qc.invalidateQueries({ queryKey: ['deliveries', endpoint?.id] })
    },
    onError: (err) => notify.failed("Couldn't redeliver", err),
  })
  return (
    <Sheet open={endpoint !== null} onOpenChange={(o) => !o && onClose()}>
      <SheetContent className="w-full sm:max-w-lg">
        <SheetHeader>
          <SheetTitle>Deliveries</SheetTitle>
          <SheetDescription className="truncate">{endpoint?.url}</SheetDescription>
        </SheetHeader>
        <div className="flex-1 overflow-y-auto px-4 pb-4">
          <QueryState
            query={deliveries}
            what="the deliveries"
            skeleton={<Skeleton className="h-40 w-full" />}
          >
            {(data) =>
              data.data.length === 0 ? (
                <p className="text-sm text-muted-foreground">Nothing sent yet.</p>
              ) : (
                <ul className="flex flex-col divide-y text-sm" aria-label="Deliveries">
                  {data.data.map((d) => (
                    <li key={d.id} className="flex flex-col gap-1 py-3">
                      <span className="flex items-center gap-2">
                        <Badge
                          variant={
                            d.status === 'delivered'
                              ? 'success'
                              : d.status === 'failed'
                                ? 'destructive'
                                : 'secondary'
                          }
                        >
                          {d.last_status ?? d.status}
                        </Badge>
                        <span className="min-w-0 truncate font-medium">{d.event_type}</span>
                        <span className="ml-auto shrink-0 text-xs text-muted-foreground">
                          {ago(d.created_at)}
                        </span>
                      </span>
                      <span className="text-xs text-muted-foreground">
                        {d.attempts} {d.attempts === 1 ? 'try' : 'tries'}
                        {d.next_attempt_at && d.status !== 'delivered' && d.status !== 'failed'
                          ? `, next ${ago(d.next_attempt_at)}`
                          : ''}
                        {d.last_error ? `: ${d.last_error}` : ''}
                      </span>
                      {d.status === 'failed' && (
                        <div>
                          <Button
                            variant="outline"
                            size="xs"
                            onClick={() => redeliver.mutate(d.id)}
                            disabled={redeliver.isPending}
                          >
                            Redeliver
                          </Button>
                        </div>
                      )}
                    </li>
                  ))}
                </ul>
              )
            }
          </QueryState>
        </div>
      </SheetContent>
    </Sheet>
  )
}

function DangerZone({ app }: { app: App }) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const toggle = useMutation({
    mutationFn: () => post(`/v1/apps/${app.id}/${app.disabled ? 'enable' : 'disable'}`),
    onSuccess: () => {
      notify.done(app.disabled ? 'App enabled' : 'App disabled')
      void qc.invalidateQueries({ queryKey: ['apps'] })
    },
    onError: (err) => notify.failed("Couldn't change the app", err),
  })
  return (
    <Item variant="outline" className="border-destructive/40 bg-card">
      <ItemContent>
        <ItemTitle>{app.disabled ? 'Enable this app' : 'Disable this app'}</ItemTitle>
        <ItemDescription>
          {app.disabled
            ? 'Its keys, uploads and playback work again.'
            : 'Its keys stop working and its videos stop playing until you enable it again.'}
        </ItemDescription>
      </ItemContent>
      <ItemActions>
        <Button
          variant={app.disabled ? 'default' : 'destructive'}
          disabled={toggle.isPending}
          onClick={async () => {
            if (app.disabled) return toggle.mutate()
            const ok = await confirm({
              title: `Disable ${app.name}?`,
              description:
                'Its API keys, upload tokens and playback stop at once. You can enable it again later.',
              action: 'Disable app',
            })
            if (ok) toggle.mutate()
          }}
        >
          {app.disabled ? 'Enable' : 'Disable'}
        </Button>
      </ItemActions>
    </Item>
  )
}
