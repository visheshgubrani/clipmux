// The docs lints behind `make site-check` (P6-T15-site.md): run after
// `next build`, over site/content/docs and the static build in site/out.
// Each check is a pure function, tested in lint.test.mjs.
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'

export const SITE_URL = 'https://clipmux.com'

const METHOD = '(?:GET|POST|PUT|PATCH|DELETE)'

/** Every page has a title and a one-sentence description. */
export function frontmatterProblems(text) {
  const m = /^---\n([\s\S]*?)\n---/.exec(text)
  if (!m) return ['no frontmatter']
  const field = (name) =>
    new RegExp(`^${name}:\\s*(.+)$`, 'm')
      .exec(m[1])?.[1]
      .trim()
      .replace(/^['"]|['"]$/g, '')
  const problems = []
  if (!field('title')) problems.push('no title')
  const description = field('description')
  if (!description) problems.push('no description')
  else if ((description.match(/[.!?](\s|$)/g) ?? []).length !== 1 || !/[.!?]$/.test(description))
    problems.push('description is not one sentence')
  return problems
}

/** Public pages carry no decision IDs, task IDs or pre-launch wording. */
export function internalIdProblems(text) {
  const found = []
  for (const re of [/\bD\d{3}\b/g, /\bP\d+-T\d+(?:\.\d+)?\w*/g, /until launch/gi])
    found.push(...(text.match(re) ?? []))
  return found.map((s) => `internal reference "${s}"`)
}

/** `/v1/assets/{asset_id}` and `/v1/assets/{id}` are the same route. */
export function normalizeRoute(path) {
  return path
    .split('?')[0]
    .replace(/\{[^}]*\}/g, '{}')
    .replace(/\/$/, '')
}

/** Routes in a Markdown file: backticked `METHOD /path`, or METHOD and path in a table row. */
export function routesIn(text) {
  const re = new RegExp(`\`(${METHOD}) (/[^\`\\s]*)\`|\\b(${METHOD})\`?\\s*\\|\\s*\`(/[^\`\\s|]*)\``, 'g')
  const out = []
  for (const m of text.matchAll(re)) out.push(`${m[1] ?? m[3]} ${normalizeRoute(m[2] ?? m[4])}`)
  return out
}

/** Every backticked `METHOD /path` in a page is a route that exists. */
export function routeProblems(text, known) {
  const re = new RegExp(`\`(${METHOD}) (/[^\`\\s]*)\``, 'g')
  const problems = []
  for (const m of text.matchAll(re)) {
    if (!known.has(`${m[1]} ${normalizeRoute(m[2])}`)) problems.push(`unknown route "${m[1]} ${m[2]}"`)
  }
  return problems
}

/** Routes from the OpenAPI spec. */
export function specRoutes(spec) {
  const out = new Set()
  for (const [path, ops] of Object.entries(spec.paths ?? {}))
    for (const method of Object.keys(ops)) out.add(`${method.toUpperCase()} ${normalizeRoute(path)}`)
  return out
}

/** Every CLIPMUX_* name in a page is a documented setting. */
export function envProblems(text, known) {
  const names = new Set(text.match(/\bCLIPMUX_[A-Z0-9_]*[A-Z0-9]\b/g) ?? [])
  return [...names].filter((n) => !known.has(n)).map((n) => `unknown setting ${n}`)
}

/** llms.txt follows llmstxt.org and links only to files the build has. */
export function llmsProblems(text, exists) {
  const problems = []
  const lines = text.split('\n')
  if (!lines[0]?.startsWith('# ')) problems.push('llms.txt does not start with an H1')
  if (!lines.some((l) => l.startsWith('> '))) problems.push('llms.txt has no blockquote summary')
  if (!lines.some((l) => l.startsWith('## '))) problems.push('llms.txt has no sections')
  for (const m of text.matchAll(/\]\(([^)]+)\)/g)) {
    const url = m[1]
    if (!url.startsWith(`${SITE_URL}/`)) problems.push(`llms.txt link is not absolute: ${url}`)
    else if (!exists(url.slice(SITE_URL.length))) problems.push(`llms.txt links to a missing file: ${url}`)
  }
  return problems
}

/** Internal links and their anchors resolve within the build. */
export function linkProblems(html, fromFile, resolve, skip = new Set()) {
  const problems = []
  for (const m of html.matchAll(/\shref="(\/[^"]*)"/g)) {
    const href = m[1].replace(/&amp;/g, '&')
    if (href.startsWith('//') || href.startsWith('/_next/')) continue
    const [pathAndQuery, hash] = href.split('#')
    const path = pathAndQuery.split('?')[0] || '/'
    if (skip.has(path)) continue
    const target = resolve(path)
    if (!target) problems.push(`${fromFile}: broken link ${href}`)
    else if (hash && target.html !== undefined && !target.html.includes(`id="${hash}"`))
      problems.push(`${fromFile}: missing anchor ${href}`)
  }
  return problems
}

// ---------------------------------------------------------------------------

function walk(dir) {
  return readdirSync(dir).flatMap((name) => {
    const p = join(dir, name)
    return statSync(p).isDirectory() ? walk(p) : [p]
  })
}

function main() {
  const site = new URL('..', import.meta.url).pathname
  const root = join(site, '..')
  const out = join(site, 'out')
  const content = join(site, 'content', 'docs')
  const problems = []

  const known = specRoutes(JSON.parse(readFileSync(join(root, 'internal/openapi/openapi.json'), 'utf8')))
  // The management and worker routes have no spec yet; docs/api.md lists them.
  for (const r of routesIn(readFileSync(join(root, 'docs/api.md'), 'utf8'))) known.add(r)
  const settings = new Set(
    ['site/content/docs/reference/configuration.mdx', 'docs/configuration.md'].flatMap(
      (f) => readFileSync(join(root, f), 'utf8').match(/\bCLIPMUX_[A-Z0-9_]+\b/g) ?? [],
    ),
  )

  for (const file of walk(content).filter((f) => f.endsWith('.mdx'))) {
    const text = readFileSync(file, 'utf8')
    const name = relative(site, file)
    for (const p of [
      ...frontmatterProblems(text),
      ...internalIdProblems(text),
      ...routeProblems(text, known),
      ...envProblems(text, settings),
    ])
      problems.push(`${name}: ${p}`)
  }

  const exists = (path) => existsSync(join(out, path)) && statSync(join(out, path)).isFile()
  const resolve = (path) => {
    for (const c of path === '/' ? ['index.html'] : [path, `${path}.html`, `${path}/index.html`]) {
      if (exists(c)) return { html: c.endsWith('.html') ? readFileSync(join(out, c), 'utf8') : undefined }
    }
    return null
  }
  const redirects = new Set(
    readFileSync(join(out, '_redirects'), 'utf8')
      .split('\n')
      .filter((l) => l.startsWith('/'))
      .map((l) => l.split(/\s+/)[0]),
  )
  const htmlFiles = walk(out).filter((f) => f.endsWith('.html'))
  for (const file of htmlFiles)
    problems.push(...linkProblems(readFileSync(file, 'utf8'), relative(out, file), resolve, redirects))

  problems.push(...llmsProblems(readFileSync(join(out, 'llms.txt'), 'utf8'), exists))

  // Every docs page has its Markdown twin.
  for (const file of htmlFiles) {
    const rel = relative(out, file)
    if ((rel === 'docs.html' || rel.startsWith('docs/')) && !exists(rel.replace(/\.html$/, '.md')))
      problems.push(`${rel}: no Markdown twin`)
  }

  if (problems.length) {
    console.error(problems.map((p) => `site lint: ${p}`).join('\n'))
    process.exit(1)
  }
  console.log(`site lint: ${htmlFiles.length} pages checked, no problems`)
}

if (import.meta.url === `file://${process.argv[1]}`) main()
