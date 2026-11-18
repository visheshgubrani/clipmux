import { DocsLayout } from 'fumadocs-ui/layouts/notebook'
import { RootProvider } from 'fumadocs-ui/provider/next'
import type { ReactNode } from 'react'
import { baseOptions } from '@/lib/layout.shared'
import { source } from '@/lib/source'

// Fumadocs' provider (search, theme switch) only on docs pages, so the landing
// page doesn't load it. Search runs on a static index.
//
// The notebook layout (D134): a full-width top bar with the tabs (Build,
// Self-host, Reference, Resources) under it, and the sidebar flush against
// the left edge (--fd-layout-width in global.css).
export default function Layout({ children }: { children: ReactNode }) {
  const base = baseOptions()
  return (
    <RootProvider
      theme={{ defaultTheme: 'system', storageKey: 'clipmux-theme' }}
      search={{ options: { type: 'static' } }}
    >
      <DocsLayout tree={source.getPageTree()} {...base} nav={{ ...base.nav, mode: 'top' }} tabMode="navbar">
        {children}
      </DocsLayout>
    </RootProvider>
  )
}
