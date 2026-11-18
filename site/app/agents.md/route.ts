import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { SUMMARY } from '@/lib/llms'
import { SITE_URL } from '@/lib/site'

// /agents.md: the prompt the landing page hands to a coding agent. Its body
// is the SDK's own AGENTS.md, which also ships inside @clipmux/node, so the
// two never differ; the header points at the rest of the docs.
export const dynamic = 'force-static'

export function GET() {
  const guide = readFileSync(join(process.cwd(), '..', 'packages', 'node', 'AGENTS.md'), 'utf8')
  const header = [
    '# ClipMux for coding agents',
    '',
    `> ${SUMMARY}`,
    '',
    'Follow the guide below to add ClipMux to an app. For anything it does not cover:',
    '',
    `- [Docs index](${SITE_URL}/llms.txt): every page, as Markdown`,
    `- [Build docs, in full](${SITE_URL}/docs/build/llms-full.txt): integrating an app`,
    `- [Troubleshooting](${SITE_URL}/docs/build/going-live/troubleshooting.md): each error and what to do`,
    `- [OpenAPI spec](${SITE_URL}/openapi.json): the integration API, OpenAPI 3.1`,
    '',
    '---',
    '',
  ].join('\n')
  return new Response(`${header}${guide}`, { headers: { 'Content-Type': 'text/markdown; charset=utf-8' } })
}
