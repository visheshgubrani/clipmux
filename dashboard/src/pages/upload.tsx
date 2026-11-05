import { ClipMuxUpload, type CreatedUpload } from '@clipmux/uploader'
import { useState } from 'react'
import { useNavigate } from 'react-router'
import { post } from '../api.ts'
import { Card, CardTitle, Muted } from '../components/ui/card.tsx'

// A dashboard upload is an ordinary POST /v1/uploads, made as the built-in
// Dashboard app, and the same part uploads an app's browser does.
export function Upload() {
  const navigate = useNavigate()
  const [progress, setProgress] = useState<number | null>(null)
  const [error, setError] = useState('')
  const start = async (file: File) => {
    setError('')
    try {
      const created = await post<CreatedUpload & { asset_id: string }>('/v1/uploads', {
        size: file.size,
        playback_policy: 'signed',
      })
      setProgress(0)
      await new ClipMuxUpload(file, {
        endpoint: location.origin,
        upload: created,
        onProgress: (p) => setProgress(p.bytesUploaded / p.bytesTotal),
      }).start()
      navigate(`/assets/${created.asset_id}`)
    } catch (err) {
      setError((err as Error).message)
      setProgress(null)
    }
  }
  return (
    <Card className="grid gap-4">
      <CardTitle>Upload a video</CardTitle>
      <Muted>It goes to the Dashboard app with a signed playback ID.</Muted>
      <input
        type="file"
        accept="video/*"
        disabled={progress !== null}
        onChange={(e) => {
          const f = e.currentTarget.files?.[0]
          if (f) void start(f)
        }}
      />
      {progress !== null && (
        <div className="h-2 overflow-hidden rounded bg-zinc-200">
          <div
            className="h-full bg-zinc-900 transition-all"
            style={{ width: `${Math.round(progress * 100)}%` }}
          />
        </div>
      )}
      {error && <p className="text-sm text-red-600">{error}</p>}
    </Card>
  )
}
