import { useEffect, useState } from 'react'
import { Link } from 'react-router'
import { type App, type Asset, api } from '../api.ts'
import { Button } from '../components/ui/button.tsx'
import { Card, CardTitle, Muted } from '../components/ui/card.tsx'
import { Table, Td, Th } from '../components/ui/table.tsx'

export function Library() {
  const [assets, setAssets] = useState<Asset[]>([])
  const [apps, setApps] = useState<Record<string, string>>({})
  const [next, setNext] = useState<string | undefined>()
  const load = async (cursor?: string) => {
    const page = await api<{ data: Asset[]; next_cursor?: string }>(
      `/v1/library?limit=50${cursor ? `&cursor=${cursor}` : ''}`,
    )
    setAssets((a) => (cursor ? [...a, ...page.data] : page.data))
    setNext(page.next_cursor)
  }
  // biome-ignore lint/correctness/useExhaustiveDependencies: load the first page once
  useEffect(() => {
    void load()
    void api<{ data: App[] }>('/v1/apps').then((r) =>
      setApps(Object.fromEntries(r.data.map((a) => [a.id, a.name]))),
    )
  }, [])
  return (
    <Card className="grid gap-4">
      <div className="flex items-center justify-between">
        <CardTitle>Library</CardTitle>
        <Button asChild>
          <Link to="/upload">Upload</Link>
        </Button>
      </div>
      {assets.length === 0 ? (
        <Muted>No videos yet.</Muted>
      ) : (
        <Table>
          <thead>
            <tr>
              <Th>Asset</Th>
              <Th>App</Th>
              <Th>Status</Th>
              <Th>Reference</Th>
              <Th>Created</Th>
            </tr>
          </thead>
          <tbody>
            {assets.map((a) => (
              <tr key={a.id}>
                <Td>
                  <Link className="font-mono text-xs underline" to={`/assets/${a.id}?app=${a.app_id}`}>
                    {a.id}
                  </Link>
                </Td>
                <Td>{apps[a.app_id] ?? a.app_id}</Td>
                <Td>{a.status}</Td>
                <Td className="text-zinc-500">{a.passthrough}</Td>
                <Td className="text-zinc-500">{new Date(a.created_at).toLocaleString()}</Td>
              </tr>
            ))}
          </tbody>
        </Table>
      )}
      {next && (
        <Button variant="outline" onClick={() => load(next)}>
          More
        </Button>
      )}
    </Card>
  )
}
