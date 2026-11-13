import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronDownIcon, RotateCwIcon } from 'lucide-react'
import * as React from 'react'
import { type Asset, api, describe, post, put, type Setup, type SetupTest, type URLs } from '@/api'
import { Preview } from '@/components/preview'
import { Steps } from '@/components/steps'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { q } from '@/queries'

/** The public and media URLs. */
export function UrlsForm({ setup, onSaved }: { setup: Setup; onSaved?: () => void }) {
  const qc = useQueryClient()
  const pinned = setup.urls.pinned
  const save = useMutation({
    mutationFn: (body: { public_url: string; media_url: string }) =>
      put<{ urls: URLs }>('/v1/settings/urls', body),
    onSuccess: async ({ urls }) => {
      // The page's Content-Security-Policy names the media URL it was served
      // with; a new media origin is reachable only after a reload.
      if (urls.media_url !== location.origin && urls.media_url !== setup.urls.media_url) {
        location.reload()
        return
      }
      await qc.invalidateQueries({ queryKey: ['setup'] })
      onSaved?.()
    },
  })
  const publicDefault = setup.urls.confirmed ? setup.urls.public_url : location.origin
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        const f = new FormData(e.currentTarget)
        save.mutate({ public_url: String(f.get('public')), media_url: String(f.get('media') ?? '') })
      }}
    >
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="public-url">Public URL</FieldLabel>
          <Input id="public-url" name="public" defaultValue={publicDefault} disabled={pinned} required />
          <FieldDescription>Where browsers and apps reach the dashboard and the API.</FieldDescription>
        </Field>
        <Field>
          <FieldLabel htmlFor="media-url">Media URL</FieldLabel>
          <Input
            id="media-url"
            name="media"
            defaultValue={setup.urls.confirmed ? setup.urls.media_url : ''}
            placeholder="Blank: the public URL"
            disabled={pinned}
          />
          <FieldDescription>
            Where videos play from. A separate domain keeps stored files away from your session.
          </FieldDescription>
        </Field>
        {pinned && (
          <Alert>
            <AlertDescription>
              Set by CLIPMUX_PUBLIC_URL on the server, so it can't be changed here.
            </AlertDescription>
          </Alert>
        )}
        {!setup.keys_allowed && (
          <FieldDescription>
            While the public URL is localhost you can try everything, but apps can't get API keys.
          </FieldDescription>
        )}
        {save.isError && <FieldError>{describe(save.error)}</FieldError>}
        {!pinned && (
          <div>
            <Button type="submit" disabled={save.isPending}>
              {save.isPending && <Spinner data-icon="inline-start" />}
              Save
            </Button>
          </div>
        )}
      </FieldGroup>
    </form>
  )
}

/**
 * The setup test: a sample through upload, encoding, encryption and
 * playback on this server, with the sample playing once it passes.
 */
export function SetupTestPanel({ after }: { after?: (test: SetupTest) => React.ReactNode }) {
  const qc = useQueryClient()
  const test = useQuery(q.setupTest())
  const t = test.data
  const sample = useQuery({
    queryKey: ['asset', t?.asset_id ?? '', 'sample'],
    queryFn: () => api<Asset>(`/v1/assets/${t?.asset_id}`),
    enabled: t?.status === 'passed' && !!t.asset_id,
  })
  const run = useMutation({
    mutationFn: () => post<SetupTest>('/v1/setup-test'),
    onSuccess: (next) => qc.setQueryData(['setup-test'], next),
  })
  // Settle the app's setup state when a test passes, once per test.
  const reported = React.useRef('')
  React.useEffect(() => {
    if (t?.status === 'passed' && reported.current !== t.id) {
      reported.current = t.id
      void qc.invalidateQueries({ queryKey: ['setup'] })
    }
  }, [t, qc])

  if (test.isPending) return <Skeleton className="h-24 w-full" />
  if (test.isError)
    return (
      <Alert variant="destructive">
        <AlertDescription>Couldn't load the setup test: {describe(test.error)}</AlertDescription>
      </Alert>
    )
  if (!t)
    return (
      <div className="flex flex-wrap items-center gap-3 text-sm text-muted-foreground">
        No setup test has run yet.
        <Button variant="outline" size="sm" onClick={() => run.mutate()} disabled={run.isPending}>
          Run the setup test
        </Button>
      </div>
    )

  const passed = t.checks.filter((c) => c.ok).length
  const running = t.status !== 'passed' && t.status !== 'failed'
  return (
    <div className="flex flex-col gap-4">
      <Collapsible defaultOpen={t.status !== 'passed'} key={`${t.id}-${t.status}`}>
        <div
          className={
            t.status === 'passed'
              ? 'flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg bg-success/10 px-3 py-2.5 text-sm text-success'
              : t.status === 'failed'
                ? 'flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg bg-destructive/10 px-3 py-2.5 text-sm text-destructive'
                : 'flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg bg-muted px-3 py-2.5 text-sm'
          }
        >
          {running && <Spinner />}
          <span className="font-medium">
            {t.status === 'passed'
              ? `All ${t.checks.length} checks passed`
              : t.status === 'failed'
                ? `${t.checks.length - passed} of ${t.checks.length} checks failed`
                : `Testing… ${passed} checks passed so far`}
          </span>
          <span className="text-muted-foreground">({t.trigger.replaceAll('_', ' ')})</span>
          <CollapsibleTrigger render={<Button variant="ghost" size="xs" className="ml-auto text-current" />}>
            Details
            <ChevronDownIcon data-icon="inline-end" />
          </CollapsibleTrigger>
        </div>
        <CollapsibleContent className="pt-3">
          <Steps steps={t.checks} raw />
        </CollapsibleContent>
      </Collapsible>
      {t.status === 'passed' && sample.data && (
        <Preview assetId={sample.data.id} baseUrl={sample.data.base_url} />
      )}
      <div className="flex flex-wrap gap-2">
        <Button variant="outline" onClick={() => run.mutate()} disabled={run.isPending || running}>
          <RotateCwIcon data-icon="inline-start" />
          Run again
        </Button>
        {after?.(t)}
      </div>
      {run.isError && <p className="text-sm text-destructive">{describe(run.error)}</p>}
    </div>
  )
}
