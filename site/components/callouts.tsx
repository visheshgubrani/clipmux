import { Callout } from 'fumadocs-ui/components/callout'
import { CloudUpload } from 'lucide-react'
import type { ReactNode } from 'react'

// The docs have three callouts and no others (P6-T15-site.md, "Page template").

export function Note({ title, children }: { title?: ReactNode; children: ReactNode }) {
  return (
    <Callout type="info" title={title}>
      {children}
    </Callout>
  )
}

export function Warning({ title, children }: { title?: ReactNode; children: ReactNode }) {
  return (
    <Callout type="warn" title={title}>
      {children}
    </Callout>
  )
}

// Marks a step that sends data to a third party: a speech API, an LLM,
// Cloudflare or a cloud GPU (design.md: say what leaves the infrastructure).
export function LeavesYourServers({ children }: { children: ReactNode }) {
  return (
    <Callout type="warn" title="Leaves your servers" icon={<CloudUpload className="size-5 text-graphite" />}>
      {children}
    </Callout>
  )
}
