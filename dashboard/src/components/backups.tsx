import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { TriangleAlertIcon } from 'lucide-react'
import type * as React from 'react'
import { type Backups, post, put } from '@/api'
import { ErrorState } from '@/components/query-state'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Item, ItemActions, ItemContent, ItemDescription, ItemTitle } from '@/components/ui/item'
import { Spinner } from '@/components/ui/spinner'
import { ago, bytes, fullDate } from '@/lib/format'
import { cn } from '@/lib/utils'
import { q } from '@/queries'

const docs = 'https://clipmux.com/docs/self-host/operate/backups'

/** A check on the card: passed, failed, or not yet knowable, with what it found. */
interface Check {
  name: string
  ok: boolean | null
  detail: React.ReactNode
  action?: React.ReactNode
}

/** The checks the card lists, in the order an owner would fix them. */
export function backupChecks(b: Backups, now = Date.now()): Check[] {
  const checks: Check[] = []
  const stale = b.last ? now - Date.parse(b.last.created_at) > 2 * b.interval_seconds * 1000 : true
  checks.push({
    name: 'A recent backup exists',
    ok: !!b.last && !stale,
    detail: b.last ? `${ago(b.last.created_at)}, ${bytes(b.last.size_bytes)}` : 'None has been written yet.',
  })
  const v = b.verified
  checks.push({
    name: 'A backup has been proved restorable',
    ok: v ? !v.error : null,
    detail: !v
      ? 'The first check runs within a minute of the first backup.'
      : v.error
        ? `The check ${ago(v.at)} failed: ${v.error}`
        : v.mode === 'restore'
          ? `Restored into a scratch database ${ago(v.at)}, and the secrets key opened it.`
          : `Checked ${ago(v.at)}, as a file only: the database role can't create databases, so it wasn't restored.`,
  })
  checks.push({
    name: 'Stored off this server',
    ok: b.off_site,
    detail: b.off_site
      ? (b.target ?? '')
      : "The backups are on this server's own disk, so losing the server loses them. Set CLIPMUX_BACKUP_URL to a bucket.",
  })
  checks.push({
    name: "This server can't delete them",
    ok: b.immutable === null ? null : b.immutable,
    detail:
      b.immutable === null
        ? 'Known after the next backup.'
        : b.immutable
          ? "The target's key can write and list but not delete, so ransomware on this server can't wipe the backups."
          : "The target's key can delete. A key that can't, with object lock and lifecycle rules on the bucket, is safer.",
  })
  checks.push({
    name: 'Sources copied',
    ok: b.sources_pending === 0,
    detail:
      b.sources_pending === 0
        ? 'Every uploaded source has a copy on the target.'
        : `${b.sources_pending} waiting to be copied, ${b.sources_pending === 1 ? 'a few at a time' : 'twenty at a time'}.`,
  })
  return checks
}

/** The Settings → Backups card: what is backed up, whether it can be restored, and what the owner confirms. */
export function BackupsCard() {
  const qc = useQueryClient()
  const status = useQuery(q.backups())
  const run = useMutation({
    mutationFn: () => post('/v1/settings/backups/run'),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['backups'] }),
  })
  const kit = useMutation({
    mutationFn: (confirmed: boolean) => put('/v1/settings/backups/recovery-kit', { confirmed }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['backups'] }),
  })
  if (status.isError)
    return <ErrorState what="backups" error={status.error} onRetry={() => status.refetch()} />
  const b = status.data
  if (!b) return <Spinner />
  if (!b.enabled)
    return (
      <Alert variant="destructive">
        <TriangleAlertIcon />
        <AlertTitle>Backups are off</AlertTitle>
        <AlertDescription>
          CLIPMUX_BACKUP_INTERVAL is 0, so nothing is backed up. Remove it to back up every hour.{' '}
          <a href={docs} target="_blank" rel="noopener noreferrer" className="underline underline-offset-3">
            Back up and restore
          </a>
        </AlertDescription>
      </Alert>
    )
  const checks = backupChecks(b)
  return (
    <div className="flex flex-col gap-3">
      {b.fence && (
        <Alert variant="destructive">
          <TriangleAlertIcon />
          <AlertTitle>Restore fence up since {ago(b.fence.detected_at)}</AlertTitle>
          <AlertDescription>
            {b.fence.detail || b.fence.reason} Nothing is deleted from storage while it's up. On the server,
            read <code>clipmux admin restore report</code>, then run{' '}
            <code>clipmux admin restore finish --yes</code>.
          </AlertDescription>
        </Alert>
      )}
      {b.error && (
        <Alert variant="destructive">
          <TriangleAlertIcon />
          <AlertTitle>The last backup failed {ago(b.error.at)}</AlertTitle>
          <AlertDescription className="break-words">{b.error.message}</AlertDescription>
        </Alert>
      )}
      <Item variant="outline" className="bg-card">
        <ItemContent>
          <ItemTitle>
            {b.last ? (
              <span title={fullDate(b.last.created_at)}>Last backup {ago(b.last.created_at)}</span>
            ) : (
              'No backup yet'
            )}
            {b.immutable && <Badge variant="secondary">Immutable</Badge>}
          </ItemTitle>
          <ItemDescription>
            Every {every(b.interval_seconds)} to <span className="break-all">{b.target}</span>.
          </ItemDescription>
        </ItemContent>
        <ItemActions>
          <Button
            variant="outline"
            size="sm"
            disabled={b.requested || run.isPending}
            onClick={() => run.mutate()}
          >
            {(b.requested || run.isPending) && <Spinner data-icon="inline-start" />}
            {b.requested ? 'Waiting for the server' : 'Back up now'}
          </Button>
        </ItemActions>
      </Item>
      <ul className="flex flex-col gap-2 text-sm">
        {checks.map((c) => (
          <CheckRow key={c.name} check={c} />
        ))}
        <CheckRow
          check={{
            name: 'Recovery kit stored off this server',
            ok: b.recovery_kit_confirmed_at ? true : null,
            detail: b.recovery_kit_confirmed_at
              ? `Confirmed ${ago(b.recovery_kit_confirmed_at)}.`
              : 'A backup is encrypted with a key derived from the secrets key, so it opens only with that key. Keep the secrets key and DATABASE_URL (your .env) in a password manager or somewhere else off this server, then confirm.',
            action: (
              <Button
                variant="outline"
                size="sm"
                disabled={kit.isPending}
                onClick={() => kit.mutate(!b.recovery_kit_confirmed_at)}
              >
                {b.recovery_kit_confirmed_at ? 'Undo' : "I've stored it"}
              </Button>
            ),
          }}
        />
      </ul>
      <p className="text-sm text-muted-foreground">
        <a
          href={docs}
          target="_blank"
          rel="noopener noreferrer"
          className="underline underline-offset-3 hover:text-foreground"
        >
          Back up and restore
        </a>{' '}
        has the steps to restore on a new server.
      </p>
    </div>
  )
}

function CheckRow({ check }: { check: Check }) {
  const mark = check.ok === null ? '○' : check.ok ? '✓' : '✗'
  return (
    <li className="flex items-start gap-3">
      <span
        aria-hidden
        className={cn(
          'mt-0.5 w-4 text-center font-medium',
          check.ok === null ? 'text-muted-foreground' : check.ok ? 'text-success' : 'text-destructive',
        )}
      >
        {mark}
      </span>
      <div className="flex min-w-0 flex-1 flex-col">
        <span className="font-medium">
          {check.name}
          <span className="sr-only">: {check.ok === null ? 'not known yet' : check.ok ? 'yes' : 'no'}</span>
        </span>
        <span className="text-muted-foreground">{check.detail}</span>
      </div>
      {check.action}
    </li>
  )
}

function every(seconds: number): string {
  if (seconds % 86400 === 0) return seconds === 86400 ? 'day' : `${seconds / 86400} days`
  if (seconds % 3600 === 0) return seconds === 3600 ? 'hour' : `${seconds / 3600} hours`
  if (seconds % 60 === 0) return seconds === 60 ? 'minute' : `${seconds / 60} minutes`
  return `${seconds} seconds`
}
