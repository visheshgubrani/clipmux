import { createFromSource } from 'fumadocs-core/search/server'
import { source } from '@/lib/source'

// The search index, built once at build time and searched in the browser
// (Orama's static mode), so the site needs no server.
export const revalidate = false

export const { staticGET: GET } = createFromSource(source, { language: 'english' })
