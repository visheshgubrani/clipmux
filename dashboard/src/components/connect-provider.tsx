import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronDownIcon, ExternalLinkIcon } from 'lucide-react'
import * as React from 'react'
import { type Approval, api, describe, type GPU, post, type Step, type StepResult } from '@/api'
import { Steps } from '@/components/steps'
import { Badge } from '@/components/ui/badge'
import { Button, buttonVariants } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Spinner } from '@/components/ui/spinner'
import { dollars } from '@/lib/format'
import { notify } from '@/lib/notify'

// Connect RunPod or Modal (D056, D057, D109, D111). The owner picks GPUs
// from the provider's own list with prices; the limits default to one
// machine and $50 a month, and the image and registry wait under Advanced.

type Kind = 'runpod' | 'modal'
const names: Record<Kind, string> = { runpod: 'RunPod', modal: 'Modal' }

/** Where each provider issues a key by hand. */
const keyPages: Record<Kind, string> = {
  runpod: 'https://console.runpod.io/user/settings',
  modal: 'https://modal.com/settings/tokens',
}

/** Modal shows `modal token set --token-id ak-… --token-secret as-…`; pasting it fills both fields. */
function modalCommand(text: string) {
  const id = /--token-id[=\s]+(\S+)/.exec(text)?.[1]
  const secret = /--token-secret[=\s]+(\S+)/.exec(text)?.[1]
  return id && secret ? { id, secret } : null
}

/** A link styled as a button, opening the provider in a new tab. */
function ProviderLink({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className={buttonVariants({ variant: 'outline', size: 'sm', className: 'w-fit' })}
    >
      <ExternalLinkIcon data-icon="inline-start" />
      {children}
    </a>
  )
}

/** The GPUs to rent, in the order ClipMux tries them, each with its price per hour. */
function GPUPicker({
  gpus,
  chosen,
  onChange,
  note,
}: {
  gpus: GPU[]
  chosen: string[]
  onChange: (ids: string[]) => void
  note: string
}) {
  return (
    <Field>
      <FieldLabel>GPUs</FieldLabel>
      <ul className="flex flex-col divide-y rounded-lg border" aria-label="GPUs">
        {gpus.map((g) => {
          const id = `gpu-${g.id.replace(/\W+/g, '-')}`
          return (
            <li key={g.id} className="flex items-center gap-3 px-3 py-2">
              <Checkbox
                id={id}
                aria-label={g.name}
                checked={chosen.includes(g.id)}
                onCheckedChange={(v) =>
                  onChange(
                    v === true
                      ? gpus.filter((x) => x.id === g.id || chosen.includes(x.id)).map((x) => x.id)
                      : chosen.filter((c) => c !== g.id),
                  )
                }
              />
              <label htmlFor={id} className="flex min-w-0 flex-1 flex-wrap items-center gap-x-2 text-sm">
                <span className="font-medium">{g.name}</span>
                <span className="text-muted-foreground">{g.memory_gb} GB</span>
                {g.recommended && <Badge variant="success">Recommended</Badge>}
              </label>
              <span className="type-timecode text-sm">{dollars(g.cents_per_hour)}/hr</span>
            </li>
          )
        })}
      </ul>
      <FieldDescription>{note}</FieldDescription>
    </Field>
  )
}

/** The first choices: the recommended GPU and the two cheapest after it, as fallbacks for capacity. */
function firstChoices(gpus: GPU[]) {
  return gpus.slice(0, 3).map((g) => g.id)
}

interface Limits {
  machines: number
  monthly: number
}

/** One line with the limits ClipMux sets, and Change to edit them. */
function LimitsLine({ limits, onChange }: { limits: Limits; onChange: (l: Limits) => void }) {
  const [open, setOpen] = React.useState(false)
  return (
    <div className="flex flex-col gap-3">
      <p className="text-sm text-muted-foreground">
        {limits.machines === 1 ? 'One machine at a time' : `Up to ${limits.machines} machines at once`}, at
        most {dollars(limits.monthly * 100)} a month.{' '}
        {!open && (
          <Button type="button" variant="link" size="sm" className="h-auto p-0" onClick={() => setOpen(true)}>
            Change
          </Button>
        )}
      </p>
      {open && (
        <div className="grid gap-4 sm:grid-cols-2">
          <Field>
            <FieldLabel htmlFor="max_concurrent">At most this many machines</FieldLabel>
            <Input
              id="max_concurrent"
              type="number"
              min={1}
              max={100}
              required
              value={limits.machines}
              onChange={(e) => onChange({ ...limits, machines: Number(e.target.value) })}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="monthly_limit">Monthly limit ($)</FieldLabel>
            <Input
              id="monthly_limit"
              type="number"
              min={1}
              step="1"
              required
              value={limits.monthly}
              onChange={(e) => onChange({ ...limits, monthly: Number(e.target.value) })}
            />
          </Field>
        </div>
      )}
    </div>
  )
}

/** The worker image and registry login, only for a private image. */
function Advanced() {
  return (
    <Collapsible>
      <CollapsibleTrigger className="group flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground">
        <ChevronDownIcon className="size-4 transition-transform group-data-[panel-open]:rotate-180" />
        Advanced
      </CollapsibleTrigger>
      <CollapsibleContent keepMounted className="flex flex-col gap-4 pt-3 data-[closed]:hidden">
        <Field>
          <FieldLabel htmlFor="image">Worker image</FieldLabel>
          <Input id="image" name="image" placeholder="Default: the one this server was built from" />
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field>
            <FieldLabel htmlFor="registry_username">Registry username</FieldLabel>
            <Input id="registry_username" name="registry_username" autoComplete="off" />
          </Field>
          <Field>
            <FieldLabel htmlFor="registry_password">Registry token</FieldLabel>
            <Input id="registry_password" name="registry_password" type="password" autoComplete="off" />
          </Field>
        </div>
        <FieldDescription>
          Only for a private worker image: a registry token with read:packages.
        </FieldDescription>
      </CollapsibleContent>
    </Collapsible>
  )
}

/** The shared settings: limits, and whatever Advanced holds. */
function settingsFrom(form: HTMLFormElement | null, limits: Limits) {
  const f = new FormData(form ?? undefined)
  return {
    image: f.get('image') || undefined,
    registry_username: f.get('registry_username') || undefined,
    registry_password: f.get('registry_password') || undefined,
    max_concurrent: limits.machines,
    monthly_limit_cents: Math.round(limits.monthly * 100),
  }
}

function useConnected(kind: Kind | null, setSteps: (s: Step[]) => void) {
  const qc = useQueryClient()
  return React.useCallback(
    (res: StepResult) => {
      setSteps(res.steps)
      void qc.invalidateQueries({ queryKey: ['executors'] })
      void qc.invalidateQueries({ queryKey: ['routing'] })
      if (res.connected && kind) notify.done(`${names[kind]} connected`)
    },
    [qc, kind, setSteps],
  )
}

export function ConnectProviderDialog({ kind, onClose }: { kind: Kind | null; onClose: () => void }) {
  return (
    <Dialog open={kind !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-lg">
        {kind === 'modal' && <ConnectModal />}
        {kind === 'runpod' && <ConnectRunPod />}
      </DialogContent>
    </Dialog>
  )
}

const DEFAULT_LIMITS: Limits = { machines: 1, monthly: 50 }

/**
 * Modal: pick GPUs, then approve ClipMux on the page Modal's CLI uses; the
 * token goes straight to this server. Pasting one stays a click away.
 */
function ConnectModal() {
  const gpus = useQuery({
    queryKey: ['gpus', 'modal'],
    queryFn: () => api<{ data: GPU[] }>('/v1/executors/modal/gpus'),
  })
  const [chosen, setChosen] = React.useState<string[] | null>(null)
  const picked = chosen ?? firstChoices(gpus.data?.data ?? [])
  const [limits, setLimits] = React.useState(DEFAULT_LIMITS)
  const [steps, setSteps] = React.useState<Step[]>([])
  const [paste, setPaste] = React.useState(false)
  const [tokenID, setTokenID] = React.useState('')
  const [tokenSecret, setTokenSecret] = React.useState('')
  const [approval, setApproval] = React.useState<Approval | null>(null)
  const [settled, setSettled] = React.useState<Approval | null>(null)
  const [pollError, setPollError] = React.useState<unknown>(null)
  const [blocked, setBlocked] = React.useState(false)
  const form = React.useRef<HTMLFormElement>(null)
  const connected = useConnected('modal', setSteps)
  const body = () => ({ ...settingsFrom(form.current, limits), gpu: picked.join(',') })
  const connect = useMutation({
    mutationFn: (b: object) => post<StepResult>('/v1/executors/modal/connect', b),
    onMutate: () => setSteps([]),
    onSuccess: connected,
  })
  const begin = useMutation({
    mutationFn: (b: object) => post<Approval>('/v1/executors/modal/authorize', b),
    onMutate: () => {
      setSteps([])
      setApproval(null)
      setSettled(null)
      setPollError(null)
    },
  })
  // Poll until the approval settles; each answer can take seconds while
  // the server waits on Modal.
  React.useEffect(() => {
    if (!approval) return
    let stopped = false
    void (async () => {
      while (!stopped) {
        try {
          const a = await api<Approval>(`/v1/executors/modal/authorize/${approval.id}`)
          if (stopped) return
          if (a.status !== 'pending') {
            setSettled(a)
            if (a.result) connected(a.result)
            return
          }
        } catch (err) {
          if (!stopped) setPollError(err)
          return
        }
        await new Promise((r) => setTimeout(r, 2000))
      }
    })()
    return () => {
      stopped = true
    }
  }, [approval, connected])
  const approve = () => {
    if (form.current && !form.current.reportValidity()) return
    // Opened while the click counts, so it isn't blocked, and with no way back here.
    const tab = window.open('', '_blank')
    if (tab) tab.opener = null
    begin.mutate(body(), {
      onSuccess: (a) => {
        setApproval(a)
        if (tab && a.url) tab.location.href = a.url
        setBlocked(!tab)
      },
      onError: () => tab?.close(),
    })
  }
  const waiting = approval !== null && !settled && !pollError
  return (
    <>
      <DialogHeader>
        <DialogTitle>Connect Modal</DialogTitle>
        <DialogDescription>
          Pick the GPUs, then approve ClipMux on Modal's page. ClipMux checks that Modal can reach this server
          and runs a setup test before using it.
        </DialogDescription>
      </DialogHeader>
      <form
        ref={form}
        aria-label="Connect Modal"
        className="flex max-h-[65vh] flex-col overflow-y-auto"
        onSubmit={(e) => {
          e.preventDefault()
          connect.mutate({ ...body(), token_id: tokenID.trim(), token_secret: tokenSecret.trim() })
        }}
      >
        <FieldGroup>
          {gpus.isPending && <Spinner />}
          {gpus.isError && <FieldError>{describe(gpus.error)}</FieldError>}
          {gpus.data && (
            <GPUPicker
              gpus={gpus.data.data}
              chosen={picked}
              onChange={setChosen}
              note="Modal's prices, including the CPU and memory each machine gets. ClipMux tries them in this order when one has no capacity, and only pays while a video encodes."
            />
          )}
          <LimitsLine limits={limits} onChange={setLimits} />
          <Advanced />
          {!paste ? (
            <div className="flex flex-col gap-3">
              <div className="flex flex-wrap items-center gap-3">
                <Button
                  type="button"
                  onClick={approve}
                  disabled={begin.isPending || waiting || picked.length === 0}
                >
                  {(begin.isPending || waiting) && <Spinner data-icon="inline-start" />}
                  {waiting ? 'Waiting for Modal…' : 'Connect with Modal'}
                </Button>
                <Button type="button" variant="link" size="sm" onClick={() => setPaste(true)}>
                  Paste a token instead
                </Button>
              </div>
              {approval?.url && waiting && (
                <p className="text-sm text-muted-foreground" aria-live="polite">
                  {blocked
                    ? 'Your browser kept the page from opening: '
                    : "Approve ClipMux on Modal's page. "}
                  <a
                    href={approval.url}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-link underline-offset-4 hover:underline"
                  >
                    {blocked ? 'open Modal' : 'Open it again'}
                  </a>
                  {approval.code && (
                    <>
                      . It shows the code <code className="font-mono text-foreground">{approval.code}</code>
                    </>
                  )}
                  .
                </p>
              )}
              {begin.isError && <FieldError>{describe(begin.error)}</FieldError>}
              {pollError !== null && <FieldError>{describe(pollError)}</FieldError>}
              {settled?.status === 'denied' && (
                <FieldError>The approval was refused on Modal's page.</FieldError>
              )}
              {settled?.status === 'expired' && (
                <FieldError>The approval expired. Start it again.</FieldError>
              )}
            </div>
          ) : (
            <div className="flex flex-col gap-4">
              <ProviderLink href={keyPages.modal}>Create a token on Modal</ProviderLink>
              <div className="grid gap-4 sm:grid-cols-2">
                <Field>
                  <FieldLabel htmlFor="token_id">Modal token ID</FieldLabel>
                  <Input
                    id="token_id"
                    autoComplete="off"
                    required
                    value={tokenID}
                    onChange={(e) => {
                      const both = modalCommand(e.target.value)
                      setTokenID(both ? both.id : e.target.value)
                      if (both) setTokenSecret(both.secret)
                    }}
                  />
                </Field>
                <Field>
                  <FieldLabel htmlFor="token_secret">Modal token secret</FieldLabel>
                  <Input
                    id="token_secret"
                    type="password"
                    autoComplete="off"
                    required
                    value={tokenSecret}
                    onChange={(e) => setTokenSecret(e.target.value)}
                  />
                </Field>
              </div>
              <FieldDescription>
                Pasting the whole <code>modal token set …</code> command Modal shows fills both fields.
              </FieldDescription>
              {connect.isError && <FieldError>{describe(connect.error)}</FieldError>}
              <div className="flex flex-wrap items-center gap-3">
                <Button type="submit" disabled={connect.isPending || picked.length === 0}>
                  {connect.isPending && <Spinner data-icon="inline-start" />}
                  {connect.isPending ? 'Connecting…' : 'Connect Modal'}
                </Button>
                <Button type="button" variant="link" size="sm" onClick={() => setPaste(false)}>
                  Approve in the browser instead
                </Button>
              </div>
            </div>
          )}
          <Steps steps={steps} />
        </FieldGroup>
      </form>
    </>
  )
}

/**
 * RunPod: a key, like Cloudflare's token. "Get a key from RunPod" opens the
 * page where RunPod creates one for ClipMux and shows it; once pasted, the
 * GPUs come from RunPod's catalog with their prices.
 */
function ConnectRunPod() {
  const [key, setKey] = React.useState('')
  const [chosen, setChosen] = React.useState<string[] | null>(null)
  const [limits, setLimits] = React.useState(DEFAULT_LIMITS)
  const [steps, setSteps] = React.useState<Step[]>([])
  const form = React.useRef<HTMLFormElement>(null)
  const connected = useConnected('runpod', setSteps)
  const gpus = useMutation({
    mutationFn: (apiKey: string) => post<{ data: GPU[] }>('/v1/executors/runpod/gpus', { api_key: apiKey }),
  })
  const { mutate: listGPUs, reset: resetGPUs } = gpus
  // Read RunPod's GPUs a moment after the key stops changing.
  React.useEffect(() => {
    const k = key.trim()
    if (!k) {
      resetGPUs()
      return
    }
    const t = setTimeout(() => listGPUs(k), 400)
    return () => clearTimeout(t)
  }, [key, listGPUs, resetGPUs])
  const list = gpus.data?.data ?? []
  const picked = chosen ?? firstChoices(list)
  const getKey = useMutation({
    mutationFn: () => post<Approval>('/v1/executors/runpod/authorize', settingsFrom(form.current, limits)),
  })
  const connect = useMutation({
    mutationFn: (b: object) => post<StepResult>('/v1/executors/runpod/connect', b),
    onMutate: () => setSteps([]),
    onSuccess: connected,
  })
  return (
    <>
      <DialogHeader>
        <DialogTitle>Connect RunPod</DialogTitle>
        <DialogDescription>
          Paste a RunPod API key and pick the GPUs. ClipMux checks that RunPod can reach this server and runs
          a setup test before using it.
        </DialogDescription>
      </DialogHeader>
      <form
        ref={form}
        aria-label="Connect RunPod"
        className="flex max-h-[65vh] flex-col overflow-y-auto"
        onSubmit={(e) => {
          e.preventDefault()
          connect.mutate({ ...settingsFrom(form.current, limits), api_key: key.trim(), gpu_type_ids: picked })
        }}
      >
        <FieldGroup>
          <Field>
            <FieldLabel htmlFor="api_key">RunPod API key</FieldLabel>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="w-fit"
              disabled={getKey.isPending}
              onClick={() => {
                // Opened while the click counts, so it isn't blocked, and with no way back here.
                const tab = window.open('', '_blank')
                if (tab) tab.opener = null
                getKey.mutate(undefined, {
                  onSuccess: (a) => {
                    if (tab) tab.location.href = a.url ?? keyPages.runpod
                  },
                  // RunPod's approval page unavailable: its settings page makes keys too.
                  onError: () => {
                    if (tab) tab.location.href = keyPages.runpod
                  },
                })
              }}
            >
              {getKey.isPending ? (
                <Spinner data-icon="inline-start" />
              ) : (
                <ExternalLinkIcon data-icon="inline-start" />
              )}
              Get a key from RunPod
            </Button>
            <Input
              id="api_key"
              name="api_key"
              type="password"
              autoComplete="off"
              required
              value={key}
              onChange={(e) => setKey(e.target.value)}
            />
            <FieldDescription>
              RunPod opens a page that creates a key for ClipMux (it names RunPod's Flash CLI). Approve it,
              copy the key it shows and paste it here. You can restrict or delete it in RunPod's settings.
            </FieldDescription>
            {gpus.isPending && (
              <FieldDescription className="flex items-center gap-2">
                <Spinner /> Reading RunPod's GPUs…
              </FieldDescription>
            )}
            {gpus.isError && <FieldError>{describe(gpus.error)}</FieldError>}
          </Field>
          {gpus.data &&
            (list.length > 0 ? (
              <GPUPicker
                gpus={list}
                chosen={picked}
                onChange={setChosen}
                note="RunPod's Secure Cloud prices per hour, GPUs that encode with NVENC only. ClipMux tries them in this order when one has no capacity, and only pays while a video encodes."
              />
            ) : (
              <FieldError>RunPod has none of the GPUs ClipMux encodes on for rent right now.</FieldError>
            ))}
          {gpus.data && <LimitsLine limits={limits} onChange={setLimits} />}
          <Advanced />
          {connect.isError && <FieldError>{describe(connect.error)}</FieldError>}
          <Steps steps={steps} />
          <div>
            <Button type="submit" disabled={connect.isPending || !gpus.data || picked.length === 0}>
              {connect.isPending && <Spinner data-icon="inline-start" />}
              {connect.isPending ? 'Connecting…' : 'Connect RunPod'}
            </Button>
          </div>
        </FieldGroup>
      </form>
    </>
  )
}
