import { type FormEvent, useEffect, useState } from 'react'
import { api, post } from '../api.ts'
import { Button } from '../components/ui/button.tsx'
import { Card, CardTitle, Muted } from '../components/ui/card.tsx'
import { Input, Label } from '../components/ui/input.tsx'

// The owner's second factor (D071): a code from any authenticator app, shown
// as text to type in (no QR library), and ten single-use recovery codes.
export function Account() {
  const [enabled, setEnabled] = useState<boolean | null>(null)
  const [secret, setSecret] = useState<{ secret: string; otpauth_url: string } | null>(null)
  const [codes, setCodes] = useState<string[]>([])
  const [error, setError] = useState('')
  useEffect(() => {
    void api<{ totp_enabled: boolean }>('/v1/session').then((s) => setEnabled(s.totp_enabled))
  }, [])
  const attempt = (fn: (f: FormData) => Promise<void>) => async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    setError('')
    try {
      await fn(new FormData(e.currentTarget))
    } catch (err) {
      setError((err as Error).message)
    }
  }
  const start = attempt(async (f) => {
    setSecret(await post('/v1/session/totp', { password: f.get('password') }))
  })
  const enable = attempt(async (f) => {
    const res = await post<{ recovery_codes: string[] }>('/v1/session/totp/enable', { code: f.get('code') })
    setCodes(res.recovery_codes)
    setSecret(null)
    setEnabled(true)
  })
  const disable = attempt(async (f) => {
    await post('/v1/session/totp/disable', { password: f.get('password') })
    setEnabled(false)
    setCodes([])
  })
  if (enabled === null) return <Muted>Loading…</Muted>
  return (
    <Card className="grid max-w-xl gap-4">
      <CardTitle>Two-factor sign-in</CardTitle>
      {codes.length > 0 && (
        <div className="grid gap-2">
          <p className="text-sm">
            Two-factor sign-in is on. Keep these recovery codes somewhere safe: each signs you in once if you
            lose your authenticator. They are shown only now.
          </p>
          <section aria-label="Recovery codes">
            <pre className="rounded bg-zinc-100 p-3 font-mono text-sm">{codes.join('\n')}</pre>
          </section>
        </div>
      )}
      {enabled && codes.length === 0 && (
        <form className="grid gap-3" onSubmit={disable}>
          <Muted>Two-factor sign-in is on: signing in needs a code from your authenticator app.</Muted>
          <Label>
            Password
            <Input name="password" type="password" autoComplete="current-password" required />
          </Label>
          <Button type="submit" variant="outline">
            Turn off two-factor sign-in
          </Button>
        </form>
      )}
      {!enabled && !secret && (
        <form className="grid gap-3" onSubmit={start}>
          <Muted>
            Protect the owner account with a code from an authenticator app as well as the password.
          </Muted>
          <Label>
            Password
            <Input name="password" type="password" autoComplete="current-password" required />
          </Label>
          <Button type="submit">Set up two-factor sign-in</Button>
        </form>
      )}
      {secret && (
        <form className="grid gap-3" onSubmit={enable}>
          <p className="text-sm">Add this key to your authenticator app, then enter the code it shows.</p>
          <section aria-label="Authenticator key">
            <code className="break-all rounded bg-zinc-100 p-3 font-mono text-sm">{secret.secret}</code>
          </section>
          <a className="text-sm text-blue-700 underline" href={secret.otpauth_url}>
            Open in an authenticator app on this device
          </a>
          <Label>
            Code
            <Input
              name="code"
              inputMode="numeric"
              autoComplete="one-time-code"
              pattern="[0-9 ]{6,7}"
              required
            />
          </Label>
          <Button type="submit">Turn on</Button>
        </form>
      )}
      {error && <p className="text-sm text-red-600">{error}</p>}
    </Card>
  )
}
