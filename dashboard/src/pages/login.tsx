import { type FormEvent, useState } from 'react'
import { ApiError, post } from '../api.ts'
import { Button } from '../components/ui/button.tsx'
import { Card, CardTitle } from '../components/ui/card.tsx'
import { Input, Label } from '../components/ui/input.tsx'

export function Login({ onSignIn }: { onSignIn: () => void }) {
  const [error, setError] = useState('')
  // The second factor's field appears once the server asks for it.
  const [needsCode, setNeedsCode] = useState(false)
  const [recovery, setRecovery] = useState(false)
  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    const f = new FormData(e.currentTarget)
    const second = needsCode ? { [recovery ? 'recovery_code' : 'code']: f.get('code') } : {}
    try {
      await post('/v1/session', { email: f.get('email'), password: f.get('password'), ...second })
      onSignIn()
    } catch (err) {
      if (err instanceof ApiError && err.code === 'totp_required') {
        setNeedsCode(true)
        setError('')
        return
      }
      setError((err as Error).message)
    }
  }
  return (
    <Card className="mx-auto mt-16 w-full max-w-sm">
      <form className="grid gap-4" onSubmit={submit}>
        <CardTitle>Sign in to ClipMux</CardTitle>
        <Label>
          Email
          <Input name="email" type="email" autoComplete="username" required />
        </Label>
        <Label>
          Password
          <Input name="password" type="password" autoComplete="current-password" required />
        </Label>
        {needsCode && (
          <Label>
            {recovery ? 'Recovery code' : 'Code from your authenticator app'}
            <Input name="code" autoComplete="one-time-code" required autoFocus />
          </Label>
        )}
        {needsCode && (
          <button
            type="button"
            className="text-left text-sm text-zinc-600 underline"
            onClick={() => setRecovery(!recovery)}
          >
            {recovery ? 'Use a code from the app' : 'Use a recovery code'}
          </button>
        )}
        {error && <p className="text-sm text-red-600">{error}</p>}
        <Button type="submit">Sign in</Button>
      </form>
    </Card>
  )
}
