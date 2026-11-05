import { type FormEvent, useState } from 'react'
import { post } from '../api.ts'
import { Button } from '../components/ui/button.tsx'
import { Card, CardTitle } from '../components/ui/card.tsx'
import { Input, Label } from '../components/ui/input.tsx'

export function Login({ onSignIn }: { onSignIn: () => void }) {
  const [error, setError] = useState('')
  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    const f = new FormData(e.currentTarget)
    try {
      await post('/v1/session', { email: f.get('email'), password: f.get('password') })
      onSignIn()
    } catch (err) {
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
        {error && <p className="text-sm text-red-600">{error}</p>}
        <Button type="submit">Sign in</Button>
      </form>
    </Card>
  )
}
