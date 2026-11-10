import type { Step } from '@/api'
import { cn } from '@/lib/utils'

const stepNames: Record<string, string> = {
  credentials: 'Credentials',
  public_url: 'Public URL reachable',
  image: 'Worker image',
  registry: 'Registry access',
  saved: 'Saved',
  setup_test: 'Setup test',
  transcription: 'Sample transcription',
  structured_output: 'Sample chapters',
  token: 'Token',
  account: 'Account',
  zone: 'Zone',
  workers_paid: 'Workers Paid',
  bucket: 'R2 bucket',
  storage: 'Storage credentials',
  worker: 'Deploy the Worker',
  routes: 'Publish routes',
  dns: 'Clear the hostname',
  domain: 'Custom domain',
  media_url: 'Media URL',
  delivery: 'Built-in delivery',
  forgotten: 'Forget the token',
}

/** The steps a Connect ran, each passed or failed with the server's detail. */
export function Steps({
  steps,
  raw,
}: {
  steps: { name: string; ok: boolean; detail?: string }[]
  raw?: boolean
}) {
  if (steps.length === 0) return null
  return (
    <ol className="flex flex-col gap-1 text-sm">
      {steps.map((s) => (
        <li key={s.name} className="flex flex-wrap gap-x-2">
          <span className={cn('font-medium', s.ok ? 'text-success' : 'text-destructive')}>
            {s.ok ? '✓' : '✗'} {raw ? s.name : (stepNames[s.name] ?? s.name)}
          </span>
          {s.detail && <span className="min-w-0 text-muted-foreground">{s.detail}</span>}
        </li>
      ))}
    </ol>
  )
}

export type { Step }
