import { DocsBody, DocsDescription, DocsPage, DocsTitle } from 'fumadocs-ui/layouts/docs/page'
import type { Metadata } from 'next'
import { notFound } from 'next/navigation'
import { PageActions } from '@/components/page-actions'
import { editUrl, rawPath, twinPath } from '@/lib/llms'
import { ogImage } from '@/lib/og'
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
    <DocsPage toc={page.data.toc}>
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
    title: page.data.title,
    description: page.data.description,
    alternates: { types: { 'text/markdown': twinPath(page) } },
    openGraph: { images: ogImage(page.slugs) },
  }
}
