import { type FormEvent, useEffect, useState } from 'react'
import { Link, useParams } from 'react-router'
import { type App, api, post } from '../api.ts'
import { Button } from '../components/ui/button.tsx'
import { Card, CardTitle, Muted, Secret } from '../components/ui/card.tsx'
import { Dialog } from '../components/ui/dialog.tsx'
import { Input } from '../components/ui/input.tsx'
import { Table, Td, Th } from '../components/ui/table.tsx'

export function Apps() {
  const [apps, setApps] = useState<App[]>([])
  const load = async () => setApps((await api<{ data: App[] }>('/v1/apps')).data)
  // biome-ignore lint/correctness/useExhaustiveDependencies: load once
  useEffect(() => {
    void load()
  }, [])
  const create = async (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    const form = e.currentTarget
    await post('/v1/apps', { name: new FormData(form).get('name') })
    form.reset()
    void load()
  }
  return (
    <Card className="grid gap-4">
      <CardTitle>Apps</CardTitle>
      <Muted>
        Each app has its own API keys, signing keys and webhook endpoints, and sees only its own videos.
      </Muted>
      <Table>
        <tbody>
          {apps.map((a) => (
            <tr key={a.id}>
              <Td>
                <Link className="underline" to={`/apps/${a.id}`}>
                  {a.name}
                </Link>
              </Td>
              <Td className="font-mono text-xs text-zinc-500">{a.id}</Td>
              <Td>{a.disabled ? 'disabled' : a.builtin ? 'built-in' : ''}</Td>
            </tr>
          ))}
        </tbody>
      </Table>
      <form className="flex gap-2" onSubmit={create}>
        <Input name="name" placeholder="New app name" required maxLength={100} />
        <Button type="submit">Create app</Button>
      </form>
    </Card>
  )
}

interface Row {
  id?: string
  kid?: string
  name?: string
  url?: string
  scopes?: string[]
  revoked_at?: string
  disabled_at?: string
  last_used_at?: string
}

export function AppDetail({ keysAllowed }: { keysAllowed: boolean }) {
  const { id = '' } = useParams()
  const [app, setApp] = useState<App | null>(null)
  const [keys, setKeys] = useState<Row[]>([])
  const [signing, setSigning] = useState<Row[]>([])
  const [hooks, setHooks] = useState<Row[]>([])
  const [secret, setSecret] = useState<{ label: string; value: string } | null>(null)
  const [error, setError] = useState('')
  const load = async () => {
    const all = (await api<{ data: App[] }>('/v1/apps')).data
    setApp(all.find((a) => a.id === id) ?? null)
    setKeys((await api<{ data: Row[] }>(`/v1/apps/${id}/api-keys`)).data)
    setSigning((await api<{ data: Row[] }>(`/v1/apps/${id}/signing-keys`)).data)
    setHooks((await api<{ data: Row[] }>(`/v1/apps/${id}/webhook-endpoints`)).data)
  }
  // biome-ignore lint/correctness/useExhaustiveDependencies: reload when the route's app changes
  useEffect(() => {
    void load()
  }, [id])
  const act = async (fn: () => Promise<void>) => {
    setError('')
    try {
      await fn()
      await load()
    } catch (err) {
      setError((err as Error).message)
    }
  }
  if (!app) return <Muted>Loading…</Muted>
  return (
    <div className="grid gap-6">
      <Card className="flex items-center justify-between">
        <div>
          <CardTitle>{app.name}</CardTitle>
          <Muted className="font-mono">{app.id}</Muted>
        </div>
        {!app.builtin && (
          <Button
            variant={app.disabled ? 'default' : 'destructive'}
            onClick={() =>
              act(async () => void (await post(`/v1/apps/${id}/${app.disabled ? 'enable' : 'disable'}`)))
            }
          >
            {app.disabled ? 'Enable' : 'Disable'}
          </Button>
        )}
      </Card>
      {error && <p className="text-sm text-red-600">{error}</p>}
      <Card className="grid gap-3">
        <CardTitle>API keys</CardTitle>
        {!keysAllowed && <Muted>Set a public URL that isn't localhost before creating API keys.</Muted>}
        <Rows
          rows={keys}
          label={(k) => `${k.name} · ${k.scopes?.join(', ')}`}
          onRevoke={(k) => act(async () => api(`/v1/api-keys/${k.id}`, { method: 'DELETE' }))}
        />
        <Button
          disabled={!keysAllowed}
          onClick={() =>
            act(async () => {
              const k = await post<{ key: string }>(`/v1/apps/${id}/api-keys`, { name: 'backend' })
              setSecret({ label: 'API key', value: k.key })
            })
          }
        >
          Create API key
        </Button>
      </Card>
      <Card className="grid gap-3">
        <CardTitle>Signing keys</CardTitle>
        <Muted>
          Your backend signs playback tokens (ES256) with the private key; ClipMux keeps only the public half.
        </Muted>
        <Rows
          rows={signing}
          label={(k) => k.kid ?? ''}
          onRevoke={(k) => act(async () => api(`/v1/signing-keys/${k.kid}`, { method: 'DELETE' }))}
        />
        <Button
          onClick={() =>
            act(async () => {
              const k = await post<{ kid: string; private_key_pem: string }>(`/v1/apps/${id}/signing-keys`)
              setSecret({ label: `Private key ${k.kid}`, value: k.private_key_pem })
            })
          }
        >
          Create signing key
        </Button>
      </Card>
      <Card className="grid gap-3">
        <CardTitle>Webhook endpoints</CardTitle>
        <Rows
          rows={hooks}
          label={(h) => h.url ?? ''}
          onRevoke={(h) => act(async () => api(`/v1/webhook-endpoints/${h.id}`, { method: 'DELETE' }))}
        />
        <form
          className="flex gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            const form = e.currentTarget
            const url = new FormData(form).get('url')
            void act(async () => {
              const h = await post<{ secret: string }>(`/v1/apps/${id}/webhook-endpoints`, { url })
              setSecret({ label: 'Webhook signing secret', value: h.secret })
              form.reset()
            })
          }}
        >
          <Input name="url" type="url" placeholder="https://your-app.example.com/webhooks/clipmux" required />
          <Button type="submit">Add</Button>
        </form>
      </Card>
      <Dialog open={!!secret} onOpenChange={(o) => !o && setSecret(null)} title="Store this secret">
        {secret && <Secret label={secret.label} value={secret.value} />}
        <Button onClick={() => setSecret(null)}>Done</Button>
      </Dialog>
    </div>
  )
}

function Rows({
  rows,
  label,
  onRevoke,
}: {
  rows: Row[]
  label: (r: Row) => string
  onRevoke: (r: Row) => void
}) {
  if (rows.length === 0) return <Muted>None yet.</Muted>
  return (
    <Table>
      <thead>
        <tr>
          <Th>Name</Th>
          <Th>State</Th>
          <Th />
        </tr>
      </thead>
      <tbody>
        {rows.map((r) => {
          const ended = r.revoked_at ?? r.disabled_at
          return (
            <tr key={r.id ?? r.kid}>
              <Td className="font-mono text-xs">{label(r)}</Td>
              <Td>
                {ended
                  ? 'revoked'
                  : r.last_used_at
                    ? `used ${new Date(r.last_used_at).toLocaleDateString()}`
                    : 'active'}
              </Td>
              <Td className="text-right">
                {!ended && (
                  <Button size="sm" variant="outline" onClick={() => onRevoke(r)}>
                    Revoke
                  </Button>
                )}
              </Td>
            </tr>
          )
        })}
      </tbody>
    </Table>
  )
}
