import { useQueryClient } from '@tanstack/react-query'
import * as React from 'react'
import { ApiError, describe, post } from '@/api'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Field, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Spinner } from '@/components/ui/spinner'
import { Wordmark } from '@/components/wordmark'

/** The centred column every signed-out page uses: the wordmark, then one task. */
export function AuthLayout({ children, wide }: { children: React.ReactNode; wide?: boolean }) {
  return (
    <main className="flex min-h-svh flex-col items-center px-4 pt-[12vh] pb-16">
      <div
        className={
          wide ? 'flex w-full max-w-[460px] flex-col gap-8' : 'flex w-full max-w-[360px] flex-col gap-8'
        }
      >
        <Wordmark />
        {children}
      </div>
    </main>
  )
}

export function Login() {
  const qc = useQueryClient()
  const [error, setError] = React.useState('')
  const [busy, setBusy] = React.useState(false)
  // The second factor's field appears once the server asks for it.
  const [needsCode, setNeedsCode] = React.useState(false)
  const [recovery, setRecovery] = React.useState(false)
  const [forgot, setForgot] = React.useState(false)
  const creds = React.useRef<{ email: string; password: string }>({ email: '', password: '' })

  const signIn = async (second: Record<string, FormDataEntryValue | null>) => {
    setBusy(true)
    setError('')
    try {
      await post('/v1/session', { ...creds.current, ...second })
      await qc.invalidateQueries({ queryKey: ['session'] })
    } catch (err) {
      if (err instanceof ApiError && err.code === 'totp_required') {
        setNeedsCode(true)
        return
      }
      setError(
        err instanceof ApiError && err.status === 429
          ? 'Too many tries from here. Wait a few minutes, then sign in again.'
          : describe(err),
      )
    } finally {
      setBusy(false)
    }
  }

  if (needsCode)
    return (
      <AuthLayout>
        <div className="flex flex-col gap-1.5">
          <h1 className="text-xl font-semibold">{recovery ? 'Use a recovery code' : 'Two-factor code'}</h1>
          <p className="text-sm text-muted-foreground">
            {recovery
              ? 'Each of your ten recovery codes works once.'
              : 'Enter the 6-digit code from your authenticator app.'}
          </p>
        </div>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            const code = new FormData(e.currentTarget).get('code')
            void signIn({ [recovery ? 'recovery_code' : 'code']: code })
          }}
        >
          <FieldGroup>
            <Field data-invalid={!!error || undefined}>
              <FieldLabel htmlFor="code">
                {recovery ? 'Recovery code' : 'Code from your authenticator app'}
              </FieldLabel>
              <Input
                id="code"
                name="code"
                autoComplete="one-time-code"
                inputMode={recovery ? 'text' : 'numeric'}
                required
                autoFocus
                aria-invalid={!!error || undefined}
                className="h-10 text-base tracking-[0.2em]"
              />
              {error && <FieldError>{error}</FieldError>}
            </Field>
            <Button type="submit" size="lg" disabled={busy}>
              {busy && <Spinner data-icon="inline-start" />}
              Sign in
            </Button>
          </FieldGroup>
        </form>
        <div className="flex justify-between text-sm">
          <Button variant="link" className="px-0" onClick={() => setRecovery(!recovery)}>
            {recovery ? 'Use a code from the app' : 'Use a recovery code'}
          </Button>
          <Button
            variant="link"
            className="px-0 text-muted-foreground"
            onClick={() => {
              setNeedsCode(false)
              setRecovery(false)
              setError('')
            }}
          >
            Back
          </Button>
        </div>
      </AuthLayout>
    )

  return (
    <AuthLayout>
      <h1 className="text-xl font-semibold">Sign in</h1>
      <form
        onSubmit={(e) => {
          e.preventDefault()
          const f = new FormData(e.currentTarget)
          creds.current = { email: String(f.get('email')), password: String(f.get('password')) }
          void signIn({})
        }}
      >
        <FieldGroup>
          <Field>
            <FieldLabel htmlFor="email">Email</FieldLabel>
            <Input id="email" name="email" type="email" autoComplete="username" required autoFocus />
          </Field>
          <Field data-invalid={!!error || undefined}>
            <div className="flex items-center justify-between">
              <FieldLabel htmlFor="password">Password</FieldLabel>
              <Button
                type="button"
                variant="link"
                size="xs"
                className="h-auto px-0 text-muted-foreground"
                aria-expanded={forgot}
                onClick={() => setForgot(!forgot)}
              >
                Forgot password?
              </Button>
            </div>
            <Input
              id="password"
              name="password"
              type="password"
              autoComplete="current-password"
              required
              aria-invalid={!!error || undefined}
            />
            {error && <FieldError>{error}</FieldError>}
          </Field>
          {forgot && (
            <Alert>
              <AlertDescription className="flex flex-col gap-2">
                ClipMux has one owner and no email reset. Set a new password on the server:
                <code className="block overflow-x-auto rounded-md bg-muted px-2.5 py-2 text-xs whitespace-nowrap">
                  docker compose exec clipmux clipmux admin reset-password
                </code>
              </AlertDescription>
            </Alert>
          )}
          <Button type="submit" size="lg" disabled={busy}>
            {busy && <Spinner data-icon="inline-start" />}
            Sign in
          </Button>
        </FieldGroup>
      </form>
    </AuthLayout>
  )
}
