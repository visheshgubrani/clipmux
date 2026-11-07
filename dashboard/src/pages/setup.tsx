import { type FormEvent, useEffect, useRef, useState } from 'react'
import { Link } from 'react-router'
import { type Asset, api, post, type Setup, type SetupTest, type URLs } from '../api.ts'
import { Button } from '../components/ui/button.tsx'
import { Card, CardTitle, Muted } from '../components/ui/card.tsx'
import { Input, Label } from '../components/ui/input.tsx'
import { Preview } from '../player.tsx'

// First boot (docs/architecture.md): the owner registers with the setup
// token from the server log, sets the public and media URLs, and the setup
// test puts a sample through the whole path.
export function SetupWizard({
  setup,
  signedIn,
  onChange,
}: {
  setup: Setup
  signedIn: boolean
  onChange: () => void
}) {
  if (!setup.owner_exists) return <Register onDone={onChange} />
  if (!signedIn) return <Muted>Sign in to continue setup.</Muted>
  return (
    <div className="grid gap-6">
      <URLStep setup={setup} onDone={onChange} />
      <SetupTestStep onDone={onChange} />
    </div>
  )
}

function Register({ onDone }: { onDone: () => void }) {
  const [error, setError] = useState('')
  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    const f = new FormData(e.currentTarget)
    try {
      await post('/v1/setup/owner', {
        setup_token: f.get('token'),
        email: f.get('email'),
        password: f.get('password'),
      })
      onDone()
    } catch (err) {
      setError((err as Error).message)
    }
  }
  return (
    <Card className="mx-auto mt-10 w-full max-w-md">
      <form className="grid gap-4" onSubmit={submit}>
        <CardTitle>Welcome to ClipMux</CardTitle>
        <Muted>Create the owner account. The setup token is printed in the server log (setup_token=…).</Muted>
        <Label>
          Setup token
          <Input name="token" required autoComplete="off" />
        </Label>
        <Label>
          Email
          <Input name="email" type="email" required autoComplete="username" />
        </Label>
        <Label>
          Password (12 characters or more)
          <Input name="password" type="password" minLength={12} required autoComplete="new-password" />
        </Label>
        {error && <p className="text-sm text-red-600">{error}</p>}
        <Button type="submit">Create owner</Button>
      </form>
    </Card>
  )
}

function URLStep({ setup, onDone }: { setup: Setup; onDone: () => void }) {
  const [error, setError] = useState('')
  const pinned = setup.urls.pinned
  const submit = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    const f = new FormData(e.currentTarget)
    try {
      const { urls: saved } = await api<{ urls: URLs }>('/v1/settings/urls', {
        method: 'PUT',
        body: JSON.stringify({ public_url: f.get('public'), media_url: f.get('media') }),
      })
      setError('')
      // The page's Content-Security-Policy names the media URL it was served
      // with; a new media origin is reachable only after a reload.
      if (saved.media_url !== location.origin && saved.media_url !== setup.urls.media_url) {
        location.reload()
        return
      }
      onDone()
    } catch (err) {
      setError((err as Error).message)
    }
  }
  const publicDefault = setup.urls.confirmed ? setup.urls.public_url : location.origin
  return (
    <Card>
      <form className="grid gap-4" onSubmit={submit}>
        <CardTitle>Public URL and media URL</CardTitle>
        <Muted>
          Browsers and workers reach ClipMux at the public URL; playback is served from the media URL (ideally
          another domain). Caddy obtains certificates for both. API keys can be created once the public URL
          isn't localhost.
        </Muted>
        <Label>
          Public URL
          <Input name="public" defaultValue={publicDefault} disabled={pinned} required />
        </Label>
        <Label>
          Media URL (blank: the public URL)
          <Input
            name="media"
            defaultValue={setup.urls.confirmed ? setup.urls.media_url : ''}
            disabled={pinned}
          />
        </Label>
        {pinned && <Muted>Set by CLIPMUX_PUBLIC_URL on the server.</Muted>}
        {!setup.keys_allowed && <Muted>The public URL is loopback: API keys stay disabled.</Muted>}
        {error && <p className="text-sm text-red-600">{error}</p>}
        {!pinned && <Button type="submit">Save and test</Button>}
      </form>
    </Card>
  )
}

function SetupTestStep({ onDone }: { onDone: () => void }) {
  const [test, setTest] = useState<SetupTest | null>(null)
  const [sample, setSample] = useState<Asset | null>(null)
  const reported = useRef('')
  useEffect(() => {
    let stop = false
    const poll = async () => {
      const t = await api<SetupTest>('/v1/setup-test/latest').catch(() => null)
      if (stop) return
      setTest(t)
      if (t?.status === 'passed' && t.asset_id) {
        // Report each passing test once; the app's refresh re-renders us.
        if (reported.current !== t.id) {
          reported.current = t.id
          setSample(await api<Asset>(`/v1/assets/${t.asset_id}`))
          onDone()
        }
        return
      }
      setTimeout(poll, 2000)
    }
    void poll()
    return () => {
      stop = true
    }
  }, [onDone])
  return (
    <Card className="grid gap-4">
      <CardTitle>Setup test</CardTitle>
      <Muted>
        A sample clip is uploaded, encoded, encrypted and played through your URLs, exactly as an app's would
        be.
      </Muted>
      {!test && <Muted>No test yet.</Muted>}
      {test && (
        <ul className="grid gap-1 text-sm">
          <li className="font-medium">
            {test.status} ({test.trigger})
          </li>
          {test.checks.map((c) => (
            <li key={c.name} className={c.ok ? 'text-emerald-700' : 'text-red-600'}>
              {c.ok ? '✓' : '✗'} {c.name} {c.detail}
            </li>
          ))}
        </ul>
      )}
      <div className="flex gap-2">
        <Button variant="outline" onClick={async () => setTest(await post<SetupTest>('/v1/setup-test'))}>
          Run again
        </Button>
        {test?.status === 'passed' && (
          <Button asChild>
            <Link to="/library">Go to the library</Link>
          </Button>
        )}
      </div>
      {sample && <Preview assetId={sample.id} baseUrl={sample.base_url} />}
    </Card>
  )
}
