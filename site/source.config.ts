import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { metaSchema, pageSchema } from 'fumadocs-core/source/schema'
import { defineConfig, defineDocs } from 'fumadocs-mdx/config'
import { z } from 'zod'
import { darkTheme, lightTheme } from './lib/code-theme'

const git = (...args: string[]) => execFileSync('git', args, { encoding: 'utf8' }).trim()
const ROOT = git('rev-parse', '--show-toplevel')

// A page was last modified when its MDX or the file it is generated from
// (`source:`, e.g. internal/openapi/openapi.json) last changed in git. A shallow
// clone would date every page to its one commit, so it dates none.
const SHALLOW = git('rev-parse', '--is-shallow-repository') === 'true'
async function lastModified(file: string): Promise<Date | undefined> {
  if (SHALLOW) return undefined
  const frontmatter = /^---\n([\s\S]*?)\n---/.exec(readFileSync(file, 'utf8'))?.[1] ?? ''
  const from = /^source:\s*"?([^"\n]+)"?$/m.exec(frontmatter)?.[1]
  const dates = [file, from && join(ROOT, from)]
    .filter((f): f is string => Boolean(f))
    .map((f) => git('log', '-1', '--format=%cI', '--', f))
    .filter(Boolean)
    .map((d) => new Date(d))
  return dates.length ? new Date(Math.max(...dates.map(Number))) : undefined
}

// Every docs page is MDX under content/docs. The processed Markdown is kept so
// each page can also be served as plain Markdown (T15.2: `.md` twins, llms.txt).
export const docs = defineDocs({
  dir: 'content/docs',
  docs: {
    // `source` names the file a generated page is built from, for Edit on GitHub.
    // `nav` is a shorter name for the sidebar ("Overview" for a tab's index).
    schema: pageSchema.extend({ source: z.string().optional(), nav: z.string().optional() }),
    postprocess: { includeProcessedMarkdown: true },
    lastModified,
  },
  meta: { schema: metaSchema },
})

export default defineConfig({
  mdxOptions: {
    rehypeCodeOptions: { themes: { light: lightTheme, dark: darkTheme } },
  },
})
