import { docs } from 'fumadocs-mdx:collections/server'
import { type LoaderPlugin, loader } from 'fumadocs-core/source'
import { lucideIconsPlugin } from 'fumadocs-core/source/plugins/lucide-icons'

// A page's frontmatter `nav:` names it in the sidebar when its title is too
// long there, as a tab's index ("Overview") is.
const navTitles: LoaderPlugin = {
  name: 'clipmux:nav-title',
  transformPageTree: {
    file(node, path) {
      const file = path ? this.storage.read(path) : undefined
      const nav = file?.format === 'page' ? (file.data as { nav?: string }).nav : undefined
      if (nav) node.name = nav
      return node
    },
  },
}

// Folders and pages name a Lucide icon in meta.json or frontmatter (`icon:`).
export const source = loader({
  baseUrl: '/docs',
  source: docs.toFumadocsSource(),
  plugins: [lucideIconsPlugin(), navTitles],
})
