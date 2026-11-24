// Schema.org structured data (JSON-LD) for the landing page and the docs.
// Only facts the site states elsewhere: no ratings, prices or versions.
import { DESCRIPTION, REPO_URL, SITE_URL } from '@/lib/site'
import { source } from '@/lib/source'

const ORG = `${SITE_URL}/#organization`
const WEBSITE = `${SITE_URL}/#website`
const SOFTWARE = `${SITE_URL}/#software`
const LICENSE = 'https://www.gnu.org/licenses/agpl-3.0.html'

export function homeSchema() {
  return {
    '@context': 'https://schema.org',
    '@graph': [
      { '@type': 'Organization', '@id': ORG, name: 'ClipMux', url: `${SITE_URL}/`, sameAs: [REPO_URL] },
      {
        '@type': 'WebSite',
        '@id': WEBSITE,
        name: 'ClipMux',
        url: `${SITE_URL}/`,
        description: DESCRIPTION,
        inLanguage: 'en',
        publisher: { '@id': ORG },
      },
      {
        '@type': 'SoftwareApplication',
        '@id': SOFTWARE,
        name: 'ClipMux',
        description: DESCRIPTION,
        url: `${SITE_URL}/`,
        applicationCategory: 'DeveloperApplication',
        applicationSubCategory: 'Video on demand platform',
        // docs/self-host/requirements: one Linux server (amd64 or arm64) with Docker.
        operatingSystem: 'Linux',
        downloadUrl: `${SITE_URL}/docs/self-host/install`,
        license: LICENSE,
        isAccessibleForFree: true,
        publisher: { '@id': ORG },
      },
      {
        '@type': 'SoftwareSourceCode',
        '@id': `${SITE_URL}/#source`,
        name: 'ClipMux',
        codeRepository: REPO_URL,
        programmingLanguage: ['Go', 'TypeScript'],
        license: LICENSE,
        targetProduct: { '@id': SOFTWARE },
      },
    ],
  }
}

type DocPage = {
  url: string
  slugs: string[]
  data: { title: string; description?: string; lastModified?: Date }
}

/** A docs page as a TechArticle, with breadcrumbs through the pages above it that exist. */
export function docSchema(page: DocPage) {
  const url = `${SITE_URL}${page.url}`
  const article = {
    '@type': 'TechArticle',
    '@id': `${url}#article`,
    headline: page.data.title,
    ...(page.data.description && { description: page.data.description }),
    url,
    mainEntityOfPage: url,
    inLanguage: 'en',
    isPartOf: { '@id': WEBSITE },
    publisher: { '@id': ORG },
    ...(page.data.lastModified && { dateModified: page.data.lastModified.toISOString() }),
  }
  if (page.slugs.length === 0) return { '@context': 'https://schema.org', ...article }

  const crumbs = [{ name: 'Docs', url: `${SITE_URL}/docs` }]
  for (let i = 1; i < page.slugs.length; i++) {
    const parent = source.getPage(page.slugs.slice(0, i))
    if (parent) crumbs.push({ name: parent.data.title, url: `${SITE_URL}${parent.url}` })
  }
  crumbs.push({ name: page.data.title, url })
  return {
    '@context': 'https://schema.org',
    '@graph': [
      article,
      {
        '@type': 'BreadcrumbList',
        itemListElement: crumbs.map((c, i) => ({
          '@type': 'ListItem',
          position: i + 1,
          name: c.name,
          item: c.url,
        })),
      },
    ],
  }
}
