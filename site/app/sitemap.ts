import type { MetadataRoute } from 'next'
import { SITE_URL } from '@/lib/site'
import { source } from '@/lib/source'

export const dynamic = 'force-static'

export default function sitemap(): MetadataRoute.Sitemap {
  return [{ url: `${SITE_URL}/` }, ...source.getPages().map((p) => ({ url: `${SITE_URL}${p.url}` }))]
}
