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

// The agent skills (skills/, T17) link to docs pages and their anchors, so a
// renamed heading must not break them: each anchor a skill uses is pinned on
// its heading with `[#id]`, and resolves in the build.

/** The clipmux.com docs links in a skill's Markdown, with their anchors. */
export function skillLinks(text) {
  return [...text.matchAll(/https:\/\/clipmux\.com(\/docs[^\s)>`#,]*)(?:#([a-z0-9_-]+))?/g)].map((m) => ({
    path: m[1].replace(/[.:]$/, ''),
    anchor: m[2] ?? '',
  }))
}

/**
 * A skill's frontmatter has its directory's name and a description that YAML
 * reads as one string: unquoted, a `: ` inside it is a parse error, and
 * `npx skills add` then skips the skill.
 */
export function skillFrontmatterProblems(text, dirName) {
  const fm = /^---\n([\s\S]*?)\n---\n/.exec(text)?.[1]
  if (fm === undefined) return ['no frontmatter']
  const problems = []
  const name = /^name: (.*)$/m.exec(fm)?.[1]
  if (name !== dirName) problems.push(`name is ${name ?? 'missing'}, not ${dirName}`)
  const description = /^description: (.*)$/m.exec(fm)?.[1]
  if (!description) problems.push('no description')
  else if (!/^".*"$/.test(description) && description.includes(': '))
    problems.push('the description has ": " and is not quoted')
  return problems
}

/** The page's MDX writes the anchor out, so renaming the heading keeps it. */
export function pinnedAnchorProblems(mdx, anchor) {
  return mdx.includes(`[#${anchor}]`)
    ? []
    : [`anchor #${anchor} is not pinned: write the heading as \`## … [#${anchor}]\``]
}

/** Every error code the spec returns, and every video and track error_code. */
export function errorCodeProblems(text, spec, guide) {
  const codes = new Set([...Object.keys(guide.asset ?? {}), ...Object.keys(guide.track ?? {})])
  const walk = (v) => {
    if (Array.isArray(v)) v.forEach(walk)
    else if (v && typeof v === 'object') {
      if (v.code?.enum) for (const c of v.code.enum) codes.add(c)
      Object.values(v).forEach(walk)
    }
  }
  for (const ops of Object.values(spec.paths ?? {}))
    for (const op of Object.values(ops)) for (const r of Object.values(op?.responses ?? {})) walk(r.content)
  return [...codes].filter((c) => !text.includes(`\`${c}\``)).map((c) => `no row for ${c}`)
}

// ---------------------------------------------------------------------------

function walk(dir) {
  return readdirSync(dir).flatMap((name) => {
    const p = join(dir, name)
    return statSync(p).isDirectory() ? walk(p) : [p]
  })
}

// `--content` checks the MDX only (frontmatter, IDs, routes, settings), with
// no build: quick while writing. The full run also checks the static build.
function main(contentOnly = false) {
  const site = new URL('..', import.meta.url).pathname
  const root = join(site, '..')
  const out = join(site, 'out')
  const content = join(site, 'content', 'docs')
  const problems = []

  const known = specRoutes(JSON.parse(readFileSync(join(root, 'internal/openapi/openapi.json'), 'utf8')))
  // The owner's management routes are on the management API page, which
  // internal/manage/docs_test.go keeps equal to the routes serve registers;
  // the worker routes have no page, and docs/api.md lists them.
  for (const f of ['site/content/docs/reference/management-api.mdx', 'docs/api.md'])
    for (const r of routesIn(readFileSync(join(root, f), 'utf8'))) known.add(r)
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

  // The agent skills: pinned anchors, and every error code in the errors list.
  const skillsDir = join(root, 'skills')
  const skillFiles = walk(skillsDir).filter(
    (f) => f.endsWith('.md') && !f.includes(`${join('skills', 'evals')}`),
  )
  for (const f of skillFiles.filter((f) => f.endsWith('SKILL.md'))) {
    const dirName = relative(skillsDir, f).split('/')[0]
    for (const p of skillFrontmatterProblems(readFileSync(f, 'utf8'), dirName))
      problems.push(`${relative(root, f)}: ${p}`)
  }
  const links = skillFiles.flatMap((f) =>
    skillLinks(readFileSync(f, 'utf8')).map((l) => ({ ...l, from: relative(root, f) })),
  )
  for (const l of links.filter((l) => l.anchor)) {
    const rel = l.path.replace(/^\/docs\/?/, '')
    const page = [join(content, `${rel}.mdx`), join(content, rel, 'index.mdx')].find((f) => existsSync(f))
    if (page)
      for (const p of pinnedAnchorProblems(readFileSync(page, 'utf8'), l.anchor))
        problems.push(`${relative(site, page)}: ${p} (${l.from})`)
  }
  const guide = JSON.parse(readFileSync(join(site, 'data', 'errors.json'), 'utf8'))
  const spec = JSON.parse(readFileSync(join(root, 'internal/openapi/openapi.json'), 'utf8'))
  for (const p of errorCodeProblems(
    readFileSync(join(skillsDir, 'clipmux-integrate', 'references', 'errors.md'), 'utf8'),
    spec,
    guide,
  ))
    problems.push(`skills/clipmux-integrate/references/errors.md: ${p}`)

  if (contentOnly) {
    if (problems.length) {
      console.error(problems.map((p) => `site lint: ${p}`).join('\n'))
      process.exit(1)
    }
    console.log('site lint: content checked, no problems')
    return
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

  // Each docs link in the skills resolves in the build, with its anchor.
  for (const l of links) {
    const target = resolve(l.path)
    if (!target) problems.push(`${l.from}: broken link ${SITE_URL}${l.path}`)
    else if (l.anchor && !target.html?.includes(`id="${l.anchor}"`))
      problems.push(`${l.from}: missing anchor ${SITE_URL}${l.path}#${l.anchor}`)
  }
  const anchors = links.filter((l) => l.anchor).length

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
  console.log(
    `site lint: ${htmlFiles.length} pages checked, ${links.length} skill links (${anchors} anchors) resolve, no problems`,
  )
}

if (import.meta.url === `file://${process.argv[1]}`) main(process.argv.includes('--content'))
