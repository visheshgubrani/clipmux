import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import * as React from 'react'
import { useSearchParams } from 'react-router'
import { ApiError, describe, post, put, type Setup, type Step } from '@/api'
import { CopyBlock } from '@/components/copy'
import { PageBody, PageHeader, Section } from '@/components/page'
import { SetupTestPanel, UrlsForm } from '@/components/setup-parts'
import { Steps } from '@/components/steps'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Item, ItemActions, ItemContent, ItemDescription, ItemTitle } from '@/components/ui/item'
import { Separator } from '@/components/ui/separator'
import { Spinner } from '@/components/ui/spinner'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { notify } from '@/lib/notify'
import { q } from '@/queries'

export function Settings({ setup }: { setup: Setup }) {
  const [params, setParams] = useSearchParams()
  const tab = params.get('tab') ?? 'general'
  return (
    <PageBody className="max-w-[880px]">
      <PageHeader title="Settings" />
      <Tabs
        value={tab}
        onValueChange={(v) => {
          const next = new URLSearchParams(params)
          if (v === 'general') next.delete('tab')
          else next.set('tab', String(v))
          setParams(next, { replace: true })
        }}
      >
        <TabsList
          variant="line"
          className="no-scrollbar w-full justify-start overflow-x-auto border-b *:flex-none! *:px-3!"
        >
          <TabsTrigger value="general">General</TabsTrigger>
          <TabsTrigger value="account">Account</TabsTrigger>
          <TabsTrigger value="security">Security</TabsTrigger>
          <TabsTrigger value="about">About</TabsTrigger>
        </TabsList>
        <TabsContent value="general" className="flex flex-col gap-10 pt-8">
          <Section title="Addresses" description="Saving runs the setup test again.">
            <UrlsForm setup={setup} />
          </Section>
          <Section
            title="Setup test"
            description="A sample through upload, encoding, encryption and playback."
          >
            <SetupTestPanel />
          </Section>
        </TabsContent>
        <TabsContent value="account" className="flex flex-col gap-10 pt-8">
          <PasswordSection />
          <Separator />
          <TwoFactorSection />
        </TabsContent>
        <TabsContent value="security" className="flex flex-col gap-10 pt-8">
          <ContentKeySection />
          <Section title="Backups">
            <p className="text-sm text-muted-foreground">
              Back up Postgres and the secrets key together. Losing either makes every video unplayable. See
              docs/operations.md for the backup and restore steps.
            </p>
          </Section>
        </TabsContent>
        <TabsContent value="about" className="pt-8">
          <Section title="ClipMux">
            <div className="flex flex-col gap-2 text-sm text-muted-foreground">
              <p>
                Open source under AGPL-3.0. <code>@clipmux/player</code>, <code>@clipmux/uploader</code> and
                the Cloudflare Worker are MIT. FFmpeg runs as a separate GPL program.
              </p>
              <p>
                Run <code>clipmux version</code> on the server to see the release.
              </p>
            </div>
          </Section>
        </TabsContent>
      </Tabs>
    </PageBody>
  )
}

function PasswordSection() {
  const session = useQuery(q.session())
  const change = useMutation({
    mutationFn: (body: { current_password: string; new_password: string }) =>
      put('/v1/session/password', body),
    onSuccess: () => notify.done('Password changed', 'Every other browser is signed out.'),
  })
  const formRef = React.useRef<HTMLFormElement>(null)
  return (
    <Section
      title="Change password"
      description={
        <>
          Signed in as <span className="text-foreground">{session.data?.email}</span>. Changing it signs out
          every other browser.
        </>
      }
    >
      <form
        ref={formRef}
        className="max-w-md"
        onSubmit={(e) => {
          e.preventDefault()
          const f = new FormData(e.currentTarget)
          change.mutate(
            { current_password: String(f.get('current')), new_password: String(f.get('new')) },
            { onSuccess: () => formRef.current?.reset() },
          )
        }}
      >
        <FieldGroup>
          <Field data-invalid={change.isError || undefined}>
            <FieldLabel htmlFor="current-password">Current password</FieldLabel>
            <Input
              id="current-password"
              name="current"
              type="password"
              autoComplete="current-password"
              required
            />
          </Field>
          <Field data-invalid={change.isError || undefined}>
            <FieldLabel htmlFor="new-password">New password</FieldLabel>
            <Input
              id="new-password"
              name="new"
              type="password"
              autoComplete="new-password"
              minLength={12}
              maxLength={1024}
              required
            />
            <FieldDescription>At least 12 characters.</FieldDescription>
            {change.isError && (
              <FieldError>
                {change.error instanceof ApiError && change.error.code === 'invalid_credentials'
                  ? "The current password isn't right."
                  : describe(change.error)}
              </FieldError>
            )}
          </Field>
          <div>
            <Button type="submit" disabled={change.isPending}>
              {change.isPending && <Spinner data-icon="inline-start" />}
              Change password
            </Button>
          </div>
        </FieldGroup>
      </form>
    </Section>
  )
}

// The owner's second factor (D071): a code from any authenticator app, shown
// as text to type in (no QR library), and ten single-use recovery codes.
function TwoFactorSection() {
  const qc = useQueryClient()
  const session = useQuery(q.session())
  const enabled = !!session.data?.totp_enabled
  const [secret, setSecret] = React.useState<{ secret: string; otpauth_url: string } | null>(null)
  const [codes, setCodes] = React.useState<string[]>([])
  const refresh = () => qc.invalidateQueries({ queryKey: ['session'] })
  const start = useMutation({
    mutationFn: (password: string) =>
      post<{ secret: string; otpauth_url: string }>('/v1/session/totp', { password }),
    onSuccess: setSecret,
  })
  const enable = useMutation({
    mutationFn: (code: string) => post<{ recovery_codes: string[] }>('/v1/session/totp/enable', { code }),
    onSuccess: (res) => {
      setCodes(res.recovery_codes)
      setSecret(null)
      void refresh()
    },
  })
  const disable = useMutation({
    mutationFn: (password: string) => post('/v1/session/totp/disable', { password }),
    onSuccess: () => {
      setCodes([])
      notify.done('Two-factor sign-in is off')
      void refresh()
    },
  })
  const error = start.error ?? enable.error ?? disable.error
  return (
    <Section
      title="Two-factor sign-in"
      description="A code from an authenticator app as well as the password."
      actions={<Badge variant={enabled ? 'success' : 'secondary'}>{enabled ? 'On' : 'Off'}</Badge>}
    >
      <div className="flex max-w-md flex-col gap-4">
        {codes.length > 0 && (
          <Alert>
            <AlertDescription className="flex flex-col gap-3">
              <span>
                Two-factor sign-in is on. Keep these recovery codes somewhere safe: each signs you in once if
                you lose your authenticator. They're shown only now.
              </span>
              <section aria-label="Recovery codes">
                <CopyBlock label="recovery codes" value={codes.join('\n')} />
              </section>
            </AlertDescription>
          </Alert>
        )}
        {enabled && codes.length === 0 && (
          <form
            onSubmit={(e) => {
              e.preventDefault()
              disable.mutate(String(new FormData(e.currentTarget).get('password')))
            }}
          >
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="tf-off-password">Password</FieldLabel>
                <Input
                  id="tf-off-password"
                  name="password"
                  type="password"
                  autoComplete="current-password"
                  required
                />
              </Field>
              <div>
                <Button type="submit" variant="outline" disabled={disable.isPending}>
                  Turn off two-factor sign-in
                </Button>
              </div>
            </FieldGroup>
          </form>
        )}
        {!enabled && !secret && (
          <form
            onSubmit={(e) => {
              e.preventDefault()
              start.mutate(String(new FormData(e.currentTarget).get('password')))
            }}
          >
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="tf-password">Password</FieldLabel>
                <Input
                  id="tf-password"
                  name="password"
                  type="password"
                  autoComplete="current-password"
                  required
                />
              </Field>
              <div>
                <Button type="submit" disabled={start.isPending}>
                  Set up two-factor sign-in
                </Button>
              </div>
            </FieldGroup>
          </form>
        )}
        {secret && (
          <form
            onSubmit={(e) => {
              e.preventDefault()
              enable.mutate(String(new FormData(e.currentTarget).get('code')))
            }}
          >
            <FieldGroup>
              <p className="text-sm">Add this key to your authenticator app, then enter the code it shows.</p>
              <section aria-label="Authenticator key">
                <CopyBlock label="authenticator key" value={secret.secret} />
              </section>
              <a className="text-sm text-link underline-offset-4 hover:underline" href={secret.otpauth_url}>
                Open in an authenticator app on this device
              </a>
              <Field>
                <FieldLabel htmlFor="tf-code">Code</FieldLabel>
                <Input
                  id="tf-code"
                  name="code"
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  pattern="[0-9 ]{6,7}"
                  required
                  className="w-40 tracking-[0.2em]"
                />
              </Field>
              <div>
                <Button type="submit" disabled={enable.isPending}>
                  Turn on
                </Button>
              </div>
            </FieldGroup>
          </form>
        )}
        {error != null && <p className="text-sm text-destructive">{describe(error)}</p>}
      </div>
    </Section>
  )
}

function ContentKeySection() {
  const cf = useQuery(q.cloudflare())
  const [open, setOpen] = React.useState(false)
  const [steps, setSteps] = React.useState<Step[]>([])
  const rotate = useMutation({
    mutationFn: (token?: string) =>
      post<{ version: number; steps?: Step[] }>(
        '/v1/content-keys/rotate',
        token ? { cloudflare_token: token } : {},
      ),
    onMutate: () => setSteps([]),
    onSuccess: (res) => {
      setSteps(res.steps ?? [])
      notify.done(
        'Content key rotated',
        `New encodes use version ${res.version}. Existing videos keep theirs.`,
      )
      setOpen(false)
    },
  })
  const needsToken = !!cf.data?.connected
  return (
    <Section title="Content encryption key">
      <Item variant="outline" className="bg-card">
        <ItemContent>
          <ItemTitle>Rotate the content key</ItemTitle>
          <ItemDescription>
            New encodes use a new key. Existing videos keep theirs and keep playing.
          </ItemDescription>
        </ItemContent>
        <ItemActions>
          <Button variant="outline" size="sm" onClick={() => setOpen(true)}>
            Rotate key
          </Button>
        </ItemActions>
      </Item>
      <Dialog
        open={open}
        onOpenChange={(o) => {
          if (!o) rotate.reset()
          setOpen(o)
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Rotate the content key?</DialogTitle>
            <DialogDescription>
              {needsToken
                ? 'The Cloudflare Worker is redeployed with the new key before any video uses it, so ClipMux needs a token for this.'
                : 'New encodes use the new key at once. Nothing is re-encoded.'}
            </DialogDescription>
          </DialogHeader>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              const token = needsToken ? String(new FormData(e.currentTarget).get('token')) : undefined
              rotate.mutate(token)
            }}
          >
            <FieldGroup>
              {needsToken && (
                <Field>
                  <FieldLabel htmlFor="rot-token">Cloudflare API token</FieldLabel>
                  <Input id="rot-token" name="token" type="password" autoComplete="off" required />
                </Field>
              )}
              {rotate.isError && <FieldError>{describe(rotate.error)}</FieldError>}
              <Steps steps={steps} />
              <div className="flex justify-end gap-2">
                <Button type="button" variant="outline" onClick={() => setOpen(false)}>
                  Cancel
                </Button>
                <Button type="submit" disabled={rotate.isPending}>
                  {rotate.isPending && <Spinner data-icon="inline-start" />}
                  Rotate key
                </Button>
              </div>
            </FieldGroup>
          </form>
        </DialogContent>
      </Dialog>
    </Section>
  )
}
