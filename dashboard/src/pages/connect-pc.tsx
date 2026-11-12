import { useMutation, useQuery } from '@tanstack/react-query'
import * as React from 'react'
import { useSearchParams } from 'react-router'
import { ApiError, api, describe, type Pairing, post } from '@/api'
import { PageBody, PageHeader } from '@/components/page'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

// The PC agent left the dashboard (D116); `clipmux worker --server URL`
// still opens this page to approve a PC, until the agent backend goes.

// ConnectPC is where `clipmux worker --server URL` sends the owner: the
// machine shows a code, the page shows the same code and what the machine
// said about itself, and one click connects it.
export function ConnectPC() {
  const [params] = useSearchParams()
  const [code, setCode] = React.useState(params.get('code') ?? '')
  const [looked, setLooked] = React.useState(params.get('code') ?? '')
  const pairing = useQuery({
    queryKey: ['pairing', looked],
    queryFn: () => api<Pairing>(`/v1/agents/pairings/${encodeURIComponent(looked)}`),
    enabled: !!looked,
    retry: false,
  })
  const decide = useMutation({
    mutationFn: (approve: boolean) =>
      post<Pairing>(`/v1/agents/pairings/${encodeURIComponent(looked)}/${approve ? 'approve' : 'deny'}`),
    onSuccess: (p) => pairing.refetch().then(() => p),
  })
  const p = decide.data ?? pairing.data
  const notFound = pairing.error instanceof ApiError && pairing.error.status === 404
  return (
    <PageBody className="max-w-lg">
      {!p ? (
        <div className="flex flex-col gap-4">
          <PageHeader title="Connect a PC" description="Enter the code your PC shows." />
          <form
            className="flex gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              setLooked(code.trim())
            }}
          >
            <Input
              aria-label="Pairing code"
              value={code}
              onChange={(e) => setCode(e.target.value)}
              placeholder="ABCD-EFGH"
              required
              className="font-mono uppercase"
            />
            <Button type="submit" disabled={pairing.isFetching}>
              Continue
            </Button>
          </form>
          {pairing.isError && (
            <p className="text-sm text-destructive">
              {notFound ? 'No PC is waiting with that code.' : describe(pairing.error)}
            </p>
          )}
        </div>
      ) : (
        <PairingDecision
          p={p}
          code={looked}
          busy={decide.isPending}
          onDecide={(ok) => decide.mutate(ok)}
          error={decide.error}
        />
      )}
    </PageBody>
  )
}

function PairingDecision({
  p,
  code,
  busy,
  onDecide,
  error,
}: {
  p: Pairing
  code: string
  busy: boolean
  onDecide: (approve: boolean) => void
  error: unknown
}) {
  const waiting = p.status === 'pending' && p.agent_started
  const connected = p.status === 'approved' || p.status === 'consumed'
  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={
          waiting
            ? 'Connect this PC?'
            : connected
              ? 'Connected'
              : p.status === 'denied'
                ? 'Not connected'
                : 'This code expired'
        }
      />
      <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
        <dt className="text-muted-foreground">Code</dt>
        <dd className="font-mono">{code.toUpperCase()}</dd>
        <dt className="text-muted-foreground">Name</dt>
        <dd>{p.name}</dd>
        {p.hostname && (
          <>
            <dt className="text-muted-foreground">Machine</dt>
            <dd>
              {p.hostname} ({p.platform})
            </dd>
          </>
        )}
        {p.ip && (
          <>
            <dt className="text-muted-foreground">Address</dt>
            <dd>{p.ip}</dd>
          </>
        )}
      </dl>
      {waiting ? (
        <>
          <Alert>
            <AlertDescription>
              Check the code matches the one in your terminal. Once connected, this PC can download sources
              and upload encoded videos for this server.
            </AlertDescription>
          </Alert>
          <div className="flex gap-2">
            <Button disabled={busy} onClick={() => onDecide(true)}>
              Connect
            </Button>
            <Button variant="outline" disabled={busy} onClick={() => onDecide(false)}>
              Deny
            </Button>
          </div>
        </>
      ) : connected ? (
        <p className="text-sm text-muted-foreground">
          The PC picks this up within a couple of seconds. You can close this tab.
        </p>
      ) : (
        <p className="text-sm text-muted-foreground">
          Run <code>clipmux worker</code> on the PC again for a new code.
        </p>
      )}
      {error != null && <p className="text-sm text-destructive">{describe(error)}</p>}
    </div>
  )
}
