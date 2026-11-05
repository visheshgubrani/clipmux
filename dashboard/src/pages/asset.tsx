import { useEffect, useState } from 'react'
import { useParams, useSearchParams } from 'react-router'
import { type Asset, api } from '../api.ts'
import { Card, CardTitle, Muted } from '../components/ui/card.tsx'
import { Preview } from '../player.tsx'

export function AssetPage() {
  const { id = '' } = useParams()
  const [params] = useSearchParams()
  const app = params.get('app') ?? undefined
  const [asset, setAsset] = useState<Asset | null>(null)
  useEffect(() => {
    let stop = false
    const poll = async () => {
      const a = await api<Asset>(`/v1/assets/${id}`, app ? { app } : {})
      if (stop) return
      setAsset(a)
      if (a.status === 'waiting_upload' || a.status === 'processing') setTimeout(poll, 3000)
    }
    void poll()
    return () => {
      stop = true
    }
  }, [id, app])
  if (!asset) return <Muted>Loading…</Muted>
  return (
    <Card className="grid gap-4">
      <CardTitle className="font-mono text-base">{asset.id}</CardTitle>
      <dl className="grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
        <dt className="text-zinc-500">Status</dt>
        <dd>
          {asset.status}
          {asset.error_code ? ` (${asset.error_code})` : ''}
        </dd>
        <dt className="text-zinc-500">Version</dt>
        <dd>{asset.version}</dd>
        <dt className="text-zinc-500">Playback IDs</dt>
        <dd className="font-mono text-xs">
          {asset.playback_ids.map((p) => `${p.id} (${p.policy}, ${p.status})`).join(', ')}
        </dd>
        <dt className="text-zinc-500">Base URL</dt>
        <dd className="font-mono text-xs">{asset.base_url}</dd>
      </dl>
      {asset.status === 'ready' && <Preview assetId={asset.id} baseUrl={asset.base_url} />}
    </Card>
  )
}
