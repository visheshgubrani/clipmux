import { metaSchema, pageSchema } from 'fumadocs-core/source/schema'
import { defineConfig, defineDocs } from 'fumadocs-mdx/config'
import { z } from 'zod'
import { darkTheme, lightTheme } from './lib/code-theme'

// Every docs page is MDX under content/docs. The processed Markdown is kept so
// each page can also be served as plain Markdown (T15.2: `.md` twins, llms.txt).
export const docs = defineDocs({
  dir: 'content/docs',
  docs: {
    // `source` names the file a generated page is built from, for Edit on GitHub.
    // `nav` is a shorter name for the sidebar ("Overview" for a tab's index).
    schema: pageSchema.extend({ source: z.string().optional(), nav: z.string().optional() }),
    postprocess: { includeProcessedMarkdown: true },
  },
  meta: { schema: metaSchema },
})

export default defineConfig({
  mdxOptions: {
    rehypeCodeOptions: { themes: { light: lightTheme, dark: darkTheme } },
  },
})
