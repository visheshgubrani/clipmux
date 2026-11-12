import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import * as React from 'react'
import { Link, useSearchParams } from 'react-router'
import { ApiError, type AppPairing, api, describe, post } from '@/api'
import { PageBody, PageHeader } from '@/components/page'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Field, FieldDescription, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Spinner } from '@/components/ui/spinner'
import { q } from '@/queries'

// /connect (P6-T23): `npx @clipmux/node init --server URL` shows a code and
// opens this page. The owner checks the code matches, picks or creates the
// app, and approves; the CLI then collects the app's credentials once and
// writes them to its env file, so no secret is copied by hand.

const NEW_APP = '__new__'

export function ConnectApp({ keysAllowed }: { keysAllowed: boolean }) {
  const [params] = useSearchParams()
  const [code, setCode] = React.useState(params.get('code') ?? '')
  const [looked, setLooked] = React.useState(params.get('code') ?? '')
  const pairing = useQuery({
    queryKey: ['app-pairing', looked],
    queryFn: () => api<AppPairing>(`/v1/app-pairings/${encodeURIComponent(looked)}`),
    enabled: !!looked,
    retry: false,
  })
  const notFound = pairing.error instanceof ApiError && pairing.error.status === 404
  return (
    <PageBody className="max-w-lg">
      {!pairing.data ? (
        <div className="flex flex-col gap-4">
          <PageHeader title="Connect an app" description="Enter the code your terminal shows." />
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
              {notFound ? 'No app is waiting with that code.' : describe(pairing.error)}
            </p>
          )}
        </div>
      ) : (
        <Decision
          p={pairing.data}
          code={looked}
          keysAllowed={keysAllowed}
          onDecided={() => pairing.refetch()}
        />
      )}
    </PageBody>
  )
}

function Decision({
  p,
  code,
  keysAllowed,
  onDecided,
}: {
  p: AppPairing
  code: string
  keysAllowed: boolean
  onDecided: () => void
}) {
  const qc = useQueryClient()
  const apps = useQuery(q.apps())
  const choices = (apps.data?.data ?? []).filter((a) => !a.builtin && !a.disabled)
  const [appID, setAppID] = React.useState('')
  const [local, setLocal] = React.useState(false)
  const picked = appID || (choices[0]?.id ?? NEW_APP)
  const decide = useMutation({
    mutationFn: (body: Record<string, unknown> | null) =>
      post<AppPairing>(
        `/v1/app-pairings/${encodeURIComponent(code)}/${body ? 'approve' : 'deny'}`,
        body ?? undefined,
      ),
    onSuccess: () => {
      // Approval may have created the app, and gave it new keys.
      void qc.invalidateQueries({ queryKey: ['apps'] })
      onDecided()
    },
  })
  const waiting = p.status === 'pending'
  const connected = p.status === 'approved' || p.status === 'consumed'
  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={
          waiting
            ? 'Connect this app?'
            : connected
              ? 'Connected'
              : p.status === 'denied'
                ? 'Not connected'
                : 'This code expired'
        }
      />
      <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
        <dt className="text-muted-foreground">Code</dt>
        <dd className="font-mono text-base font-semibold tracking-wider">{code.toUpperCase()}</dd>
        {p.name && (
          <>
            <dt className="text-muted-foreground">Name</dt>
            <dd className="break-all">{p.name}</dd>
          </>
        )}
        {p.hostname && (
          <>
            <dt className="text-muted-foreground">Machine</dt>
            <dd className="break-all">{p.hostname}</dd>
          </>
        )}
        {p.client && (
          <>
            <dt className="text-muted-foreground">Client</dt>
            <dd className="break-all">{p.client}</dd>
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
        <form
          onSubmit={(e) => {
            e.preventDefault()
            const f = new FormData(e.currentTarget)
            const webhook = String(f.get('webhook_url') ?? '').trim()
            decide.mutate({
              ...(picked === NEW_APP
                ? { new_app_name: String(f.get('new_app_name') ?? '').trim() }
                : { app_id: picked }),
              ...(webhook ? { webhook_url: webhook } : {}),
              ...(keysAllowed ? {} : { local: true }),
            })
          }}
        >
          <FieldGroup>
            <Alert>
              <AlertDescription>
                Check the code matches the one in your terminal. Connecting gives that machine an API key, a
                signing key and, if you add one, a webhook secret for the app you pick.
              </AlertDescription>
            </Alert>
            <Field>
              <FieldLabel htmlFor="connect-app">App</FieldLabel>
              <NativeSelect id="connect-app" value={picked} onChange={(e) => setAppID(e.target.value)}>
                {choices.map((a) => (
                  <NativeSelectOption key={a.id} value={a.id}>
                    {a.name}
                  </NativeSelectOption>
                ))}
                <NativeSelectOption value={NEW_APP}>New app…</NativeSelectOption>
              </NativeSelect>
            </Field>
            {picked === NEW_APP && (
              <Field>
                <FieldLabel htmlFor="connect-new-app">New app name</FieldLabel>
                <Input
                  id="connect-new-app"
                  name="new_app_name"
                  required
                  maxLength={100}
                  defaultValue={p.name}
                  placeholder="Course site"
                />
              </Field>
            )}
            <Field>
              <FieldLabel htmlFor="connect-webhook">Webhook URL (optional)</FieldLabel>
              <Input
                id="connect-webhook"
                name="webhook_url"
                type="url"
                placeholder="https://example.com/api/clipmux/webhook"
              />
              <FieldDescription>Where ClipMux tells your app a video is ready.</FieldDescription>
            </Field>
            {!keysAllowed && (
              <Alert>
                <AlertDescription className="flex flex-col gap-3">
                  <span>
                    The public URL is localhost, so upload links point at this computer. Apps elsewhere can't
                    use them until you{' '}
                    <Link to="/settings" className="text-link underline-offset-4 hover:underline">
                      set a public URL in Settings
                    </Link>
                    .
                  </span>
                  <Field orientation="horizontal">
                    <Checkbox
                      id="connect-local"
                      checked={local}
                      onCheckedChange={(v) => setLocal(v === true)}
                    />
                    <FieldLabel htmlFor="connect-local" className="font-normal">
                      This app runs on this computer
                    </FieldLabel>
                  </Field>
                </AlertDescription>
              </Alert>
            )}
            <div className="flex gap-2">
              <Button type="submit" disabled={decide.isPending || (!keysAllowed && !local)}>
                {decide.isPending && <Spinner data-icon="inline-start" />}
                Connect
              </Button>
              <Button
                type="button"
                variant="outline"
                disabled={decide.isPending}
                onClick={() => decide.mutate(null)}
              >
                Deny
              </Button>
            </div>
            {decide.isError && <p className="text-sm text-destructive">{describe(decide.error)}</p>}
          </FieldGroup>
        </form>
      ) : connected ? (
        <p className="text-sm text-muted-foreground">
          Your terminal picks up the credentials within a couple of seconds and writes them to its env file.
          You can close this tab, or see the app on{' '}
          <Link
            to={p.app_id ? `/apps/${p.app_id}` : '/apps'}
            className="text-link underline-offset-4 hover:underline"
          >
            Apps & API
          </Link>
          .
        </p>
      ) : (
        <p className="text-sm text-muted-foreground">
          Run <code>npx @clipmux/node init</code> again for a new code.
        </p>
      )}
    </div>
  )
}
