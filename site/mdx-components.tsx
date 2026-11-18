import { File, Files, Folder } from 'fumadocs-ui/components/files'
import { Step, Steps } from 'fumadocs-ui/components/steps'
import { TypeTable } from 'fumadocs-ui/components/type-table'
import defaultMdxComponents from 'fumadocs-ui/mdx'
import type { MDXComponents } from 'mdx/types'
import { LeavesYourServers, Note, Warning } from '@/components/callouts'
import { Tab, Tabs } from '@/components/tabs'

// Components every MDX page can use without an import. Steps only for real
// sequences; the three callouts and no others.
export function getMDXComponents(components?: MDXComponents): MDXComponents {
  return {
    ...defaultMdxComponents,
    File,
    Files,
    Folder,
    LeavesYourServers,
    Note,
    Step,
    Steps,
    Tab,
    Tabs,
    TypeTable,
    Warning,
    ...components,
  }
}
