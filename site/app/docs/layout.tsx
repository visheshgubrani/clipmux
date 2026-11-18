import { DocsLayout } from 'fumadocs-ui/layouts/docs'
import { RootProvider } from 'fumadocs-ui/provider/next'
import type { ReactNode } from 'react'
import { baseOptions } from '@/lib/layout.shared'
import { source } from '@/lib/source'

// Fumadocs' provider (search, theme switch) only on docs pages, so the landing
// page doesn't load it. Search runs on a static index.
export default function Layout({ children }: { children: ReactNode }) {
  return (
    <RootProvider
      theme={{ defaultTheme: 'system', storageKey: 'clipmux-theme' }}
      search={{ options: { type: 'static' } }}
    >
      <DocsLayout tree={source.getPageTree()} {...baseOptions()}>
        {children}
      </DocsLayout>
    </RootProvider>
  )
}
