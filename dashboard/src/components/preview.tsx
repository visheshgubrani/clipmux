import * as React from 'react'
import { Skeleton } from '@/components/ui/skeleton'

// The player (hls.js and Video.js) loads only on the pages that play.
const Player = React.lazy(() => import('@/player').then((m) => ({ default: m.Preview })))

export function Preview(props: React.ComponentProps<typeof Player>) {
  return (
    <React.Suspense fallback={<Skeleton className="aspect-video w-full rounded-md" />}>
      <Player {...props} />
    </React.Suspense>
  )
}
