import { useMutation, useQueryClient } from '@tanstack/react-query'
import { CheckIcon } from 'lucide-react'
import { Link, Navigate } from 'react-router'
import { describe, post, type Setup } from '@/api'
import { CopyField } from '@/components/copy'
import { UrlsForm } from '@/components/setup-parts'
import { Button } from '@/components/ui/button'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Spinner } from '@/components/ui/spinner'
import { cn } from '@/lib/utils'
import { AuthLayout } from '@/pages/login'

// First run (docs/architecture.md, "First boot"): the first visitor creates
// the one owner account with the setup token from the server log and sets
// the addresses. The setup test waits on Settings for when the owner wants
// it (D115).

const steps = ['Account', 'Your addresses']

function Stepper({ at }: { at: number }) {
  return (
    <ol aria-label="Setup steps" className="flex flex-wrap gap-x-5 gap-y-2 text-sm">
      {steps.map((label, i) => {
        const done = i < at
        const current = i === at
        return (
          <li
            key={label}
            aria-current={current ? 'step' : undefined}
            className={cn('flex items-center gap-2', current ? 'font-medium' : 'text-muted-foreground')}
          >
            <span
              className={cn(
                'flex size-5 items-center justify-center rounded-full text-[11px] font-semibold',
                done || current ? 'bg-primary text-primary-foreground' : 'bg-muted text-muted-foreground',
              )}
            >
              {done ? <CheckIcon className="size-3" /> : i + 1}
            </span>
            {label}
          </li>
        )
      })}
    </ol>
  )
}

export function SetupWizard({ setup, signedIn }: { setup: Setup; signedIn: boolean }) {
  if (!setup.owner_exists) return <Register />
  if (!signedIn) return <Navigate to="/login?next=%2Fsetup" replace />
  if (setup.urls.confirmed) return <Navigate to="/" replace />
  return (
    <AuthLayout wide>
      <Stepper at={1} />
      <div className="flex flex-col gap-1.5">
        <h1 className="text-xl font-semibold">Where will people reach ClipMux?</h1>
        <p className="text-sm text-muted-foreground">
          Videos and uploads are addressed by these URLs. Keep localhost to try things out; you can change
          them later in Settings.
        </p>
      </div>
      <UrlsForm setup={setup} />
    </AuthLayout>
  )
}

function Register() {
  const qc = useQueryClient()
  const create = useMutation({
    mutationFn: (body: { setup_token: string; email: string; password: string }) =>
      post('/v1/setup/owner', body),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['setup'] })
      await qc.invalidateQueries({ queryKey: ['session'] })
    },
  })
  return (
    <AuthLayout wide>
      <Stepper at={0} />
      <div className="flex flex-col gap-1.5">
        <h1 className="text-xl font-semibold">Create your account</h1>
        <p className="text-sm text-muted-foreground">
          This instance has one owner: you. Once the account exists, sign-up closes for good.
        </p>
      </div>
      <form
        onSubmit={(e) => {
          e.preventDefault()
          const f = new FormData(e.currentTarget)
          create.mutate({
            setup_token: String(f.get('token')).trim(),
            email: String(f.get('email')),
            password: String(f.get('password')),
          })
        }}
      >
        <FieldGroup>
          <Field>
            <FieldLabel htmlFor="setup-token">Setup token</FieldLabel>
            <Input id="setup-token" name="token" required autoComplete="off" className="font-mono" />
            <FieldDescription>
              The server printed it when it started, and it lasts a day. Find it with:
            </FieldDescription>
            <CopyField value={'docker compose logs clipmux | grep setup_token'} label="command" />
          </Field>
          <Field>
            <FieldLabel htmlFor="email">Email</FieldLabel>
            <Input id="email" name="email" type="email" required autoComplete="username" />
          </Field>
          <Field data-invalid={create.isError || undefined}>
            <FieldLabel htmlFor="password">Password</FieldLabel>
            <Input
              id="password"
              name="password"
              type="password"
              minLength={12}
              maxLength={1024}
              required
              autoComplete="new-password"
              aria-invalid={create.isError || undefined}
            />
            <FieldDescription>At least 12 characters. A few random words work well.</FieldDescription>
            {create.isError && <FieldError>{describe(create.error)}</FieldError>}
          </Field>
          <Button type="submit" size="lg" disabled={create.isPending}>
            {create.isPending && <Spinner data-icon="inline-start" />}
            Create account
          </Button>
        </FieldGroup>
      </form>
      <p className="text-sm text-muted-foreground">
        Already set up?{' '}
        <Link to="/login" className="text-link underline-offset-4 hover:underline">
          Sign in
        </Link>
      </p>
    </AuthLayout>
  )
}
