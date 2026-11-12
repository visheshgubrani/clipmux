import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { MoreHorizontalIcon, PlusIcon } from 'lucide-react'
import * as React from 'react'
import { type Asset, del, describe, post, type ShareLink } from '@/api'
import { useConfirm } from '@/components/confirm'
import { CopyButton, CopyField } from '@/components/copy'
import { Section } from '@/components/page'
import { QueryState } from '@/components/query-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Item, ItemActions, ItemContent, ItemDescription, ItemTitle } from '@/components/ui/item'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { count, fullDate } from '@/lib/format'
import { notify } from '@/lib/notify'
import { q } from '@/queries'

// Share links (D104): a page anyone with the link can watch on, without
// the app's tokens. One link per person or group, so one can be turned off
// without the others.

const expiries = [
  { value: '', label: 'Never' },
  { value: '1', label: 'In 1 day' },
  { value: '7', label: 'In 7 days' },
  { value: '30', label: 'In 30 days' },
  { value: '90', label: 'In 90 days' },
]

const statusBadge: Record<ShareLink['status'], { label: string; variant: 'success' | 'secondary' }> = {
  active: { label: 'Active', variant: 'success' },
  expired: { label: 'Expired', variant: 'secondary' },
  revoked: { label: 'Revoked', variant: 'secondary' },
}

export function ShareLinks({ asset: a, app }: { asset: Asset; app?: string | undefined }) {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const links = useQuery(q.shares(a.id, app))
  const [creating, setCreating] = React.useState(false)
  const [streaming, setStreaming] = React.useState<ShareLink | null>(null)
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['shares', a.id] })
    void qc.invalidateQueries({ queryKey: ['asset', a.id] })
  }
  const revoke = useMutation({
    mutationFn: (l: ShareLink) => del<ShareLink>(`/v1/shares/${l.id}`, app),
    onSuccess: () => {
      notify.done('Share link revoked')
      refresh()
    },
    onError: (err) => notify.failed("Couldn't revoke the share link", err),
  })
  return (
    <Section
      title="Share links"
      description="Anyone with a link can watch this video, without your app. Make one per person or group, so you can turn one off without the others."
      actions={
        <Button variant="outline" size="sm" onClick={() => setCreating(true)}>
          <PlusIcon data-icon="inline-start" />
          New share link
        </Button>
      }
    >
      <QueryState query={links} what="share links" skeleton={<Skeleton className="h-16 w-full" />}>
        {(data) =>
          data.data.length === 0 ? (
            <p className="text-sm text-muted-foreground">No share links yet.</p>
          ) : (
            <ul className="flex flex-col divide-y rounded-lg border bg-card" aria-label="Share links">
              {data.data.map((l) => {
                const badge = statusBadge[l.status]
                return (
                  <li key={l.id} data-share={l.status}>
                    <Item className="rounded-none">
                      <ItemContent className="min-w-0">
                        <ItemTitle className="min-w-0">
                          <span className="truncate">{l.name || 'Untitled link'}</span>
                          <Badge variant={badge.variant}>{badge.label}</Badge>
                        </ItemTitle>
                        <ItemDescription>
                          {l.expires_at
                            ? `${l.status === 'expired' ? 'Expired' : 'Expires'} ${fullDate(l.expires_at)}`
                            : 'No expiry'}
                          , {count(l.views_30d ?? 0)} {(l.views_30d ?? 0) === 1 ? 'view' : 'views'} in 30 days
                        </ItemDescription>
                      </ItemContent>
                      {l.status === 'active' && (
                        <ItemActions>
                          <CopyButton value={l.url} label="Copy link" className="text-foreground" />
                          <DropdownMenu>
                            <DropdownMenuTrigger
                              render={
                                <Button
                                  variant="ghost"
                                  size="icon-sm"
                                  aria-label={`More for ${l.name || 'this link'}`}
                                />
                              }
                            >
                              <MoreHorizontalIcon />
                            </DropdownMenuTrigger>
                            <DropdownMenuContent align="end">
                              <DropdownMenuGroup>
                                <DropdownMenuItem onClick={() => setStreaming(l)}>
                                  Stream URL for other players
                                </DropdownMenuItem>
                              </DropdownMenuGroup>
                              <DropdownMenuSeparator />
                              <DropdownMenuGroup>
                                <DropdownMenuItem
                                  variant="destructive"
                                  onClick={async () => {
                                    const ok = await confirm({
                                      title: `Revoke ${l.name ? `“${l.name}”` : 'this share link'}?`,
                                      description:
                                        'Anyone with this link or its stream URL stops being able to watch, within a minute. This can’t be undone; you can make a new link.',
                                      action: 'Revoke link',
                                    })
                                    if (ok) revoke.mutate(l)
                                  }}
                                >
                                  Revoke link
                                </DropdownMenuItem>
                              </DropdownMenuGroup>
                            </DropdownMenuContent>
                          </DropdownMenu>
                        </ItemActions>
                      )}
                    </Item>
                  </li>
                )
              })}
            </ul>
          )
        }
      </QueryState>
      <CreateShare open={creating} onOpenChange={setCreating} asset={a} app={app} onDone={refresh} />
      <StreamURL link={streaming} app={app} onClose={() => setStreaming(null)} />
    </Section>
  )
}

function CreateShare({
  open,
  onOpenChange,
  asset: a,
  app,
  onDone,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  asset: Asset
  app?: string | undefined
  onDone: () => void
}) {
  const create = useMutation({
    mutationFn: (body: { name: string; expires_at?: string }) =>
      post<ShareLink>(`/v1/assets/${a.id}/shares`, body, app),
    onSuccess: () => {
      notify.done('Share link created')
      onDone()
    },
  })
  const made = create.data
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) create.reset()
        onOpenChange(o)
      }}
    >
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{made ? 'Share link ready' : 'New share link'}</DialogTitle>
          <DialogDescription>
            {made
              ? 'Send this link to the people who should watch. You can copy it again from the list.'
              : 'Anyone with the link can watch until it expires or you revoke it.'}
          </DialogDescription>
        </DialogHeader>
        {made ? (
          <>
            <CopyField value={made.url} label="share link" />
            <DialogFooter>
              <Button onClick={() => onOpenChange(false)}>Done</Button>
            </DialogFooter>
          </>
        ) : (
          <form
            onSubmit={(e) => {
              e.preventDefault()
              const f = new FormData(e.currentTarget)
              const days = Number(f.get('expires') || 0)
              create.mutate({
                name: String(f.get('name') ?? ''),
                ...(days ? { expires_at: new Date(Date.now() + days * 86_400_000).toISOString() } : {}),
              })
            }}
          >
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="share-name">Name</FieldLabel>
                <Input
                  id="share-name"
                  name="name"
                  maxLength={100}
                  placeholder="For the board"
                  autoComplete="off"
                />
                <FieldDescription>Only you see it, to tell your links apart.</FieldDescription>
              </Field>
              <Field>
                <FieldLabel htmlFor="share-expires">Expires</FieldLabel>
                <NativeSelect id="share-expires" name="expires" defaultValue="7">
                  {expiries.map((x) => (
                    <NativeSelectOption key={x.value} value={x.value}>
                      {x.label}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
                <FieldDescription>
                  An expiry can’t be changed later; make a new link instead.
                </FieldDescription>
              </Field>
              {create.isError && <FieldError>{describe(create.error)}</FieldError>}
              <DialogFooter>
                <Button type="submit" disabled={create.isPending}>
                  {create.isPending && <Spinner data-icon="inline-start" />}
                  Create link
                </Button>
              </DialogFooter>
            </FieldGroup>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}

/** The link's plain HLS URL, signed once: for players that can't open the page. */
function StreamURL({
  link,
  app,
  onClose,
}: {
  link: ShareLink | null
  app?: string | undefined
  onClose: () => void
}) {
  const sign = useMutation({
    mutationFn: (l: ShareLink) =>
      post<{ url: string; expires_at: string }>(`/v1/shares/${l.id}/stream-url`, {}, app),
  })
  const { mutate, reset } = sign
  React.useEffect(() => {
    if (link) mutate(link)
    else reset()
  }, [link, mutate, reset])
  return (
    <Dialog open={link !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Stream URL</DialogTitle>
          <DialogDescription>
            An HLS URL for players that can’t open the share page, such as VLC or a TV app. It can’t be
            refreshed, and it stops when the link is revoked.
          </DialogDescription>
        </DialogHeader>
        {sign.isPending && <Skeleton className="h-9 w-full" />}
        {sign.isError && <FieldError>{describe(sign.error)}</FieldError>}
        {sign.data && (
          <div className="flex flex-col gap-2">
            <CopyField value={sign.data.url} label="stream URL" />
            <p className="text-sm text-muted-foreground">Plays until {fullDate(sign.data.expires_at)}.</p>
          </div>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
