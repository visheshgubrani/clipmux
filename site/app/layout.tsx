import type { Metadata, Viewport } from 'next'
import Script from 'next/script'
import type { ReactNode } from 'react'
import { DESCRIPTION, SITE_URL } from '@/lib/site'
import './global.css'

export const metadata: Metadata = {
  metadataBase: new URL(SITE_URL),
  title: { default: 'ClipMux', template: '%s | ClipMux' },
  description: DESCRIPTION,
  icons: { icon: '/favicon.svg' },
  openGraph: { siteName: 'ClipMux', images: '/og/image.png' },
}

export const viewport: Viewport = {
  themeColor: [
    { media: '(prefers-color-scheme: light)', color: '#f5f5f7' },
    { media: '(prefers-color-scheme: dark)', color: '#121118' },
  ],
}

// Light by default, dark by system preference (D133), before first paint.
// The docs' theme switch (next-themes, app/docs/layout.tsx) writes the same
// storage key, so a choice made there holds on the landing page too.
const THEME = `try{var t=localStorage.getItem('clipmux-theme');document.documentElement.classList.toggle('dark',t==='dark'||((!t||t==='system')&&matchMedia('(prefers-color-scheme: dark)').matches))}catch(e){}`

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <body className="flex min-h-screen flex-col bg-canvas text-ink">
        <Script id="theme" strategy="beforeInteractive">
          {THEME}
        </Script>
        {children}
      </body>
    </html>
  )
}
