import { notFound } from 'next/navigation'
import { fullText, pageMarkdown, TABS } from '@/lib/llms'
import { source } from '@/lib/source'

// Plain-text files built from the docs: every page's Markdown twin
// (/raw/<slugs>.md) and each tab's llms-full.txt (/raw/<tab>/llms-full.txt).
// scripts/postbuild.mjs moves them under /docs.
export const dynamic = 'force-static'

type Ctx = { params: Promise<{ slug: string[] }> }

export async function GET(_req: Request, { params }: Ctx) {
  const { slug } = await params
  const last = slug.at(-1) ?? ''
  if (slug.length === 2 && last === 'llms-full.txt' && TABS.some((t) => t.slug === slug[0])) {
    return new Response(await fullText(slug[0] ?? ''), {
      headers: { 'Content-Type': 'text/plain; charset=utf-8' },
    })
  }
  if (!last.endsWith('.md')) notFound()
  const slugs = [...slug.slice(0, -1), last.slice(0, -3)]
  const page = source.getPage(slugs.length === 1 && slugs[0] === 'index' ? [] : slugs)
  if (!page) notFound()
  return new Response(await pageMarkdown(page), {
    headers: { 'Content-Type': 'text/markdown; charset=utf-8' },
  })
}

export function generateStaticParams(): { slug: string[] }[] {
  const pages = source.getPages().map((p) => {
    const s = p.slugs.length ? [...p.slugs] : ['index']
    const last = s.pop() ?? 'index'
    return { slug: [...s, `${last}.md`] }
  })
  return [...pages, ...TABS.map((t) => ({ slug: [t.slug, 'llms-full.txt'] }))]
}
