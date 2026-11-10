import type { UseQueryResult } from '@tanstack/react-query'
import { RotateCwIcon, TriangleAlertIcon } from 'lucide-react'
import type * as React from 'react'
import { ApiError } from '@/api'
import { Button } from '@/components/ui/button'
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from '@/components/ui/empty'
import { Spinner } from '@/components/ui/spinner'

/** What failed, the API's status and code, and Try again. Never an endless "Loading…". */
export function ErrorState({
  what,
  error,
  onRetry,
  retrying,
}: {
  what: string
  error: unknown
  onRetry: () => void
  retrying?: boolean
}) {
  const detail =
    error instanceof ApiError
      ? `The server answered ${error.status} (${error.code}): ${error.message}`
      : error instanceof TypeError
        ? 'The server could not be reached.'
        : String(error)
  return (
    <Empty className="border border-border bg-card">
      <EmptyHeader>
        <EmptyMedia variant="icon" className="text-destructive">
          <TriangleAlertIcon />
        </EmptyMedia>
        <EmptyTitle>Couldn't load {what}</EmptyTitle>
        <EmptyDescription>{detail}</EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <Button variant="outline" onClick={onRetry} disabled={retrying}>
          {retrying ? <Spinner data-icon="inline-start" /> : <RotateCwIcon data-icon="inline-start" />}
          Try again
        </Button>
      </EmptyContent>
    </Empty>
  )
}

/**
 * Renders a query's three states: the skeleton while it first loads, the
 * error with Try again, then the content.
 */
export function QueryState<T>({
  query,
  what,
  skeleton,
  children,
}: {
  query: Pick<UseQueryResult<T>, 'data' | 'error' | 'isPending' | 'isError' | 'refetch' | 'isFetching'>
  what: string
  skeleton: React.ReactNode
  children: (data: T) => React.ReactNode
}) {
  if (query.isPending) return <>{skeleton}</>
  if (query.isError)
    return (
      <ErrorState
        what={what}
        error={query.error}
        onRetry={() => void query.refetch()}
        retrying={query.isFetching}
      />
    )
  return <>{children(query.data as T)}</>
}
