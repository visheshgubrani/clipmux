import { DocsBody, DocsDescription, DocsPage, DocsTitle } from 'fumadocs-ui/layouts/notebook/page'
import type { Metadata } from 'next'
import { notFound } from 'next/navigation'
import { JsonLd } from '@/components/json-ld'
import { PageActions } from '@/components/page-actions'
import { editUrl, rawPath, twinPath } from '@/lib/llms'
import { ogImage } from '@/lib/og'
import { docSchema } from '@/lib/schema'
import { SITE_URL } from '@/lib/site'
import { source } from '@/lib/source'
import { getMDXComponents } from '@/mdx-components'

type Props = { params: Promise<{ slug?: string[] }> }

export default async function Page({ params }: Props) {
  const { slug } = await params
  const page = source.getPage(slug)
  if (!page) notFound()
  const MDX = page.data.body
  return (
    // Notebook leaves the article at the start of its column; centre it, as the
    // docs layout did.
    <DocsPage toc={page.data.toc} full={page.data.full} className="*:mx-auto *:w-full">
      <DocsTitle className="type-title">{page.data.title}</DocsTitle>
      <DocsDescription className="mb-0">{page.data.description}</DocsDescription>
      <PageActions
        // `next dev` serves the twins under /raw; the build moves them to /docs.
        markdownUrl={process.env.NODE_ENV === 'development' ? rawPath(page) : twinPath(page)}
        absoluteMarkdownUrl={`${SITE_URL}${twinPath(page)}`}
        editUrl={editUrl(page)}
      />
      <DocsBody>
        <MDX components={getMDXComponents()} />
      </DocsBody>
      <JsonLd data={docSchema(page)} />
    </DocsPage>
  )
}

export function generateStaticParams() {
  return source.generateParams()
}

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { slug } = await params
  const page = source.getPage(slug)
  if (!page) notFound()
  return {
    // "Build with ClipMux" already names the site; don't append " | ClipMux" again.
    title: page.data.title.includes('ClipMux') ? { absolute: page.data.title } : page.data.title,
    description: page.data.description,
    alternates: { canonical: page.url, types: { 'text/markdown': twinPath(page) } },
    // A page's openGraph replaces the layout's, so it names the site again.
    openGraph: { siteName: 'ClipMux', url: page.url, images: ogImage(page.slugs) },
  }
}
