import type { InferPageType } from 'fumadocs-core/source'
import { REPO_URL, SITE_URL } from '@/lib/site'
import { source } from '@/lib/source'

export type Page = InferPageType<typeof source>

// The docs tabs, in sidebar order. A page belongs to the tab its first slug names.
export const TABS = [
  { slug: 'build', title: 'Build', about: 'Adding uploads, webhooks and signed playback to an app.' },
  { slug: 'self-host', title: 'Self-host', about: 'Installing and running a ClipMux server.' },
  { slug: 'reference', title: 'Reference', about: 'The API, webhooks, errors, limits, settings and CLI.' },
] as const

export const SUMMARY =
  'ClipMux is self-hosted video infrastructure, an open-source alternative to Mux and Cloudflare Stream: upload, encode and stream encrypted HLS from servers you own. It is one Go binary and Postgres; apps integrate through a REST API (OpenAPI 3.1), the @clipmux/node SDK, and the <clipmux-uploader> and <clipmux-player> elements.'

// Where a page's Markdown twin is served: /docs.md for the introduction,
// /docs/<slugs>.md for every other page. The build writes them there
// (scripts/postbuild.mjs); in `next dev` the same text is at rawPath(page).
export function twinPath(page: Page): string {
  return page.slugs.length ? `/docs/${page.slugs.join('/')}.md` : '/docs.md'
}

export function rawPath(page: Page): string {
  return `/raw/${page.slugs.length ? page.slugs.join('/') : 'index'}.md`
}

export function editUrl(page: Page): string {
  return `${REPO_URL}/blob/main/${page.data.source ?? `site/content/docs/${page.path}`}`
}

export async function pageMarkdown(page: Page): Promise<string> {
  const body = await page.data.getText('processed')
  const description = page.data.description ? `\n\n${page.data.description}` : ''
  return `# ${page.data.title}${description}\n\nSource: ${SITE_URL}${page.url}\n\n${body.trim()}\n`
}

export function pagesInTab(tab: string | null): Page[] {
  return source.getPages().filter((p) => (tab === null ? p.slugs.length === 0 : p.slugs[0] === tab))
}

function sidebarOrder(): Page[] {
  // getPages() follows the file system; the page tree follows meta.json.
  const order: string[] = []
  const walk = (nodes: ReturnType<typeof source.getPageTree>['children']) => {
    for (const n of nodes) {
      if (n.type === 'page') order.push(n.url)
      else if (n.type === 'folder') {
        if (n.index) order.push(n.index.url)
        walk(n.children)
      }
    }
  }
  walk(source.getPageTree().children)
  const rank = new Map(order.map((u, i) => [u, i]))
  return [...source.getPages()].sort((a, b) => (rank.get(a.url) ?? 1e9) - (rank.get(b.url) ?? 1e9))
}

export async function fullText(tab: string | null): Promise<string> {
  const pages = sidebarOrder().filter((p) => tab === null || p.slugs[0] === tab)
  const parts = await Promise.all(pages.map(pageMarkdown))
  return parts.join('\n---\n\n')
}

// llms.txt, in the llmstxt.org format: an H1, a blockquote summary, then
// sections of absolute links to each page's Markdown twin.
export function llmsIndex(): string {
  const pages = sidebarOrder()
  const line = (p: Page) =>
    `- [${p.data.title}](${SITE_URL}${twinPath(p)})${p.data.description ? `: ${p.data.description}` : ''}`
  const out = ['# ClipMux', '', `> ${SUMMARY}`, '']
  out.push('## Start', '', ...pages.filter((p) => p.slugs.length === 0).map(line), '')
  for (const tab of TABS) {
    const inTab = pages.filter((p) => p.slugs[0] === tab.slug)
    if (inTab.length) out.push(`## ${tab.title}`, '', ...inTab.map(line), '')
  }
  out.push(
    '## Optional',
    '',
    `- [All the docs](${SITE_URL}/llms-full.txt): every page in one file`,
    ...TABS.map((t) => `- [${t.title}, in full](${SITE_URL}/docs/${t.slug}/llms-full.txt): ${t.about}`),
    `- [OpenAPI spec](${SITE_URL}/openapi.json): the integration API, OpenAPI 3.1`,
    '',
  )
  return out.join('\n')
}
