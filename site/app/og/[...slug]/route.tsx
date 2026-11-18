import { readFile } from 'node:fs/promises'
import { join } from 'node:path'
import { notFound } from 'next/navigation'
import { ImageResponse } from 'next/og'
import { source } from '@/lib/source'

// One branded template for every docs page: the mark, the title, the
// description, and a black poster band (D098's one loud element).
export const dynamic = 'force-static'

// Read at build time from the package, not bundled. The build runs in site/.
const fonts = join(process.cwd(), 'node_modules', '@fontsource', 'archivo', 'files')
const font = (file: string) => readFile(join(fonts, file))

type Ctx = { params: Promise<{ slug: string[] }> }

export async function GET(_req: Request, { params }: Ctx) {
  const { slug } = await params
  if (slug.at(-1) !== 'image.png') notFound()
  const page = source.getPage(slug.slice(0, -1))
  if (!page) notFound()
  const [regular, bold] = await Promise.all([
    font('archivo-latin-400-normal.woff'),
    font('archivo-latin-700-normal.woff'),
  ])
  return new ImageResponse(
    <div
      style={{
        width: '100%',
        height: '100%',
        display: 'flex',
        flexDirection: 'column',
        background: '#f5f5f7',
        fontFamily: 'Archivo',
      }}
    >
      <div style={{ display: 'flex', flexDirection: 'column', padding: '64px 72px 0', flex: 1 }}>
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 16,
            fontSize: 30,
            fontWeight: 700,
            color: '#18171e',
          }}
        >
          <svg width="44" height="44" viewBox="0 0 32 32" role="img" aria-label="ClipMux">
            <rect width="32" height="32" rx="7" fill="#6d28d9" />
            <rect x="7" y="9.5" width="2.5" height="13" rx="1.25" fill="#fff" fillOpacity=".6" />
            <path d="M12.5 9.5v13l10.5-6.5z" fill="#fff" />
          </svg>
          ClipMux
        </div>
        <div
          style={{
            marginTop: 56,
            fontSize: 72,
            fontWeight: 700,
            lineHeight: 1.05,
            letterSpacing: -1.5,
            color: '#18171e',
            maxWidth: 1000,
          }}
        >
          {page.data.title}
        </div>
        {page.data.description ? (
          <div style={{ marginTop: 24, fontSize: 30, lineHeight: 1.35, color: '#6b6a75', maxWidth: 980 }}>
            {page.data.description}
          </div>
        ) : null}
      </div>
      <div
        style={{
          display: 'flex',
          height: 64,
          background: '#000000',
          alignItems: 'center',
          padding: '0 72px',
        }}
      >
        <div style={{ display: 'flex', flex: 1, height: 4, background: '#2b2a35' }}>
          <div style={{ width: '38%', height: 4, background: '#a78bfa' }} />
        </div>
      </div>
    </div>,
    {
      width: 1200,
      height: 630,
      fonts: [
        { name: 'Archivo', data: regular, weight: 400, style: 'normal' },
        { name: 'Archivo', data: bold, weight: 700, style: 'normal' },
      ],
    },
  )
}

export function generateStaticParams(): { slug: string[] }[] {
  return source.getPages().map((p) => ({ slug: [...p.slugs, 'image.png'] }))
}
