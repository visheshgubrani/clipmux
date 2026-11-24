// Builds the generated Reference pages from their sources, before every
// build, dev server and typecheck (P6-T15.3). The output is git-ignored:
//
//   internal/openapi/openapi.json -> reference/api/*.mdx and reference/webhooks.mdx
//   site/data/errors.json + spec  -> reference/errors.mdx
//   CHANGELOG.md                  -> reference/changelog.mdx
//
// Pages are plain Markdown tables and code blocks, so their Markdown twins
// read as well as the HTML. It fails if the spec has an error code that
// site/data/errors.json doesn't explain.
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

const site = new URL('..', import.meta.url).pathname
const root = join(site, '..')
const out = join(site, 'content/docs/reference')
const spec = JSON.parse(readFileSync(join(root, 'internal/openapi/openapi.json'), 'utf8'))
const guide = JSON.parse(readFileSync(join(site, 'data/errors.json'), 'utf8'))

const STATUS = {
  200: 'OK',
  201: 'Created',
  202: 'Accepted',
  400: 'Bad Request',
  401: 'Unauthorized',
  403: 'Forbidden',
  404: 'Not Found',
  409: 'Conflict',
  410: 'Gone',
  413: 'Content Too Large',
  422: 'Unprocessable Content',
  429: 'Too Many Requests',
}

export const slug = (s) =>
  s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '')

const firstSentence = (s) => {
  const m = /^(.+?[.!?])(\s|$)/.exec(s)
  return m ? m[1] : s
}
const rest = (s) => s.slice(firstSentence(s).length).trim()
// Prose from the spec is Markdown, but MDX reads { } and < > outside code as
// JSX, so those are escaped everywhere except inside backticks.
export const mdx = (s) =>
  String(s ?? '')
    .split(/(`[^`]*`)/)
    .map((part, i) => (i % 2 ? part : part.replace(/[{}<>]/g, (c) => `\\${c}`)))
    .join('')
const cell = (s) => mdx(s).replace(/\|/g, '\\|').replace(/\n+/g, ' ')
const yaml = (s) => JSON.stringify(s)
const ref = (s) => (s?.$ref ? spec.components.schemas[s.$ref.split('/').pop()] : s)
const refName = (s) => s?.$ref?.split('/').pop()

// `nav` is the shorter sidebar name when the title is longer (source.config.ts).
function frontmatter(title, description, source, icon, nav) {
  const iconLine = icon ? `icon: ${icon}\n` : ''
  const navLine = nav ? `nav: ${yaml(nav)}\n` : ''
  return `---\ntitle: ${yaml(title)}\ndescription: ${yaml(description)}\nsource: ${yaml(source)}\n${iconLine}${navLine}---\n\n`
}

export function typeOf(s) {
  if (!s) return ''
  if (s.$ref) return refName(s)
  if (s.oneOf) return s.oneOf.map(typeOf).join(' or ')
  if (s.allOf) return s.allOf.map(typeOf).join(' and ')
  if (s.enum) return s.enum.map((v) => `"${v}"`).join(', ')
  if (s.type === 'array') return `array of ${typeOf(s.items)}`
  return s.format ? `${s.type} (${s.format})` : (s.type ?? 'object')
}

// The fields of an object schema, following one $ref and merging allOf.
function fields(schema) {
  const s = ref(schema)
  if (!s) return []
  if (s.allOf) return s.allOf.flatMap(fields)
  const required = new Set(s.required ?? [])
  return Object.entries(s.properties ?? {}).map(([name, p]) => ({
    name,
    type: typeOf(p),
    required: required.has(name),
    description: p.description ?? ref(p)?.description ?? '',
  }))
}

function fieldTable(schema) {
  const s = ref(schema)
  if (s?.oneOf) {
    return s.oneOf
      .map((branch) => `One of these, **${branch.title ?? typeOf(branch)}**:\n\n${fieldTable(branch)}`)
      .join('\n\n')
  }
  const rows = fields(schema)
  if (!rows.length) return ''
  return [
    '| Field | Type | Required | Description |',
    '| --- | --- | --- | --- |',
    ...rows.map(
      (f) => `| \`${f.name}\` | ${cell(f.type)} | ${f.required ? 'yes' : 'no'} | ${cell(f.description)} |`,
    ),
  ].join('\n')
}

const json = (v) => `\`\`\`json\n${JSON.stringify(v, null, 2)}\n\`\`\``

function errorCodes(response) {
  const codes = []
  const walk = (v) => {
    if (Array.isArray(v)) v.forEach(walk)
    else if (v && typeof v === 'object') {
      if (v.code?.enum) codes.push(...v.code.enum)
      Object.values(v).forEach(walk)
    }
  }
  for (const m of Object.values(response.content ?? {})) walk(m.schema)
  return codes
}

function auth(op) {
  const sec = op.security ?? spec.security ?? []
  if (!sec.length) return 'None.'
  return sec
    .map((req) =>
      Object.entries(req)
        .map(([scheme, scopes]) =>
          scheme === 'apiKey'
            ? `An API key${scopes.length ? ` with ${scopes.map((s) => `\`${s}\``).join(' or ')}` : ''}`
            : scheme === 'uploadToken'
              ? "The upload's `upload_token`"
              : scheme,
        )
        .join(' and '),
    )
    .join(', or ')
    .concat('.')
}

function operations() {
  const ops = []
  for (const [path, item] of Object.entries(spec.paths)) {
    for (const [method, op] of Object.entries(item)) {
      if (method === 'parameters') continue
      ops.push({
        path,
        method: method.toUpperCase(),
        op,
        params: [...(item.parameters ?? []), ...(op.parameters ?? [])],
      })
    }
  }
  return ops
}

function operationSection({ path, method, op, params }) {
  const parts = [`## ${mdx(op.summary)}`, '', `\`${method} ${path}\``, '', mdx(op.description)]
  parts.push('', `**Authentication:** ${auth(op)}`)
  if (op.servers?.length) parts.push('', 'Served from the media URL (`base_url`), not the public URL.')
  const ps = params.map((p) => (p.$ref ? spec.components.parameters[p.$ref.split('/').pop()] : p))
  if (ps.length) {
    parts.push(
      '',
      '### Parameters',
      '',
      '| Name | In | Type | Required | Description |',
      '| --- | --- | --- | --- | --- |',
      ...ps.map(
        (p) =>
          `| \`${p.name}\` | ${p.in} | ${cell(typeOf(p.schema))} | ${p.required ? 'yes' : 'no'} | ${cell(p.description)} |`,
      ),
    )
  }
  const body = op.requestBody?.content?.['application/json']
  if (body) {
    const table = fieldTable(body.schema)
    parts.push('', '### Request body', '')
    if (table) parts.push(table, '')
    if (body.example !== undefined) parts.push(json(body.example))
  }
  for (const [code, r] of Object.entries(op.responses).filter(([c]) => c.startsWith('2'))) {
    parts.push('', `### Response ${code}`, '', mdx(r.description ?? STATUS[code] ?? ''))
    for (const [type, m] of Object.entries(r.content ?? {})) {
      if (m.example === undefined) continue
      parts.push(
        '',
        type === 'application/json' ? json(m.example) : `\`\`\`text\n${m.example.trimEnd()}\n\`\`\``,
      )
    }
  }
  const errors = Object.entries(op.responses)
    .filter(([c]) => !c.startsWith('2'))
    .flatMap(([c, r]) => errorCodes(r).map((code) => [c, code]))
  if (errors.length) {
    parts.push(
      '',
      '### Errors',
      '',
      '| Status | Code |',
      '| --- | --- |',
      ...errors.map(([c, code]) => `| ${c} | [\`${code}\`](/docs/reference/errors#${code}) |`),
    )
  }
  return parts.join('\n')
}

function generateApi() {
  const dir = join(out, 'api')
  rmSync(dir, { recursive: true, force: true })
  mkdirSync(dir, { recursive: true })
  const ops = operations()
  const groups = spec['x-tagGroups']
  const tagPages = []
  for (const group of groups) {
    for (const name of group.tags) {
      const tag = spec.tags.find((t) => t.name === name)
      const mine = ops.filter((o) => o.op.tags?.[0] === name)
      const page = slug(name)
      tagPages.push({ page, name, group: group.name, tag, mine })
      writeFileSync(
        join(dir, `${page}.mdx`),
        // "Videos API" says what the page is in a search result; the sidebar keeps "Videos".
        frontmatter(
          `${name} API`,
          firstSentence(tag.description),
          'internal/openapi/openapi.json',
          undefined,
          name,
        ) +
          [mdx(rest(tag.description)), ...mine.map(operationSection)].filter(Boolean).join('\n\n') +
          '\n',
      )
    }
  }
  const security = Object.entries(spec.components.securitySchemes)
    .map(
      ([name, s]) =>
        `- **${name === 'apiKey' ? 'API key' : 'Upload token'}** (\`Authorization: Bearer …\`): ${mdx(s.description)}`,
    )
    .join('\n')
  const index =
    frontmatter(
      'API reference',
      'Every route of the integration API, with its parameters, an example and its errors.',
      'internal/openapi/openapi.json',
    ) +
    [
      mdx(spec.info.description),
      `Every instance serves this description as OpenAPI 3.1 at \`${spec.servers[0].variables.public_url.default}/openapi.json\`, with no credential, so a code generator or an agent reads the version the instance runs. The copy on this site is at [/openapi.json](/openapi.json).`,
      '## Authentication',
      security,
      '## Idempotency',
      mdx(spec.components.parameters.IdempotencyKey.description),
      '## Errors',
      'Every error is JSON: `{"error": {"code": "snake_case", "message": "…", …details}}`. The [errors reference](/docs/reference/errors) lists every code and what to do about it.',
      ...groups.map(
        (g) =>
          `## ${g.name}\n\n${tagPages
            .filter((p) => p.group === g.name)
            .map((p) => `- [${p.name}](/docs/reference/api/${p.page}): ${firstSentence(p.tag.description)}`)
            .join('\n')}`,
      ),
    ].join('\n\n') +
    '\n'
  writeFileSync(join(dir, 'index.mdx'), index)
  writeFileSync(
    join(dir, 'meta.json'),
    `${JSON.stringify({ title: 'API reference', icon: 'Braces', pages: ['index', ...tagPages.map((p) => p.page)] }, null, 2)}\n`,
  )
  return ops
}

function generateWebhooks() {
  const headers = ['WebhookEvent', 'WebhookId', 'WebhookTimestamp', 'WebhookSignature'].map(
    (k) => spec.components.parameters[k],
  )
  const parts = [
    frontmatter(
      'Webhook events',
      "Every event your app's webhook endpoints receive, with its headers and an example body.",
      'internal/openapi/openapi.json',
      'Webhook',
    ),
    "Each app's endpoints receive its videos' events as a `POST` with a JSON body. Any 2xx is success; redirects are not followed. Delivery is at least once and unordered, and retries back off for 24 hours; after that, catch up with `GET /v1/assets?updated_since=`.",
    '## Headers',
    [
      '| Header | Description |',
      '| --- | --- |',
      ...headers.map((h) => `| \`${h.name}\` | ${cell(h.description ?? typeOf(h.schema))} |`),
    ].join('\n'),
    '## Verify a delivery',
    [
      '1. Read the raw body before any JSON parsing.',
      '2. Compute HMAC-SHA256 of `{X-Webhook-Timestamp}.{raw body}` with the endpoint secret (`whsec_…`, prefix included) as the key, and compare its hex with `X-Webhook-Signature` after `sha256=`, in constant time.',
      '3. Reject a timestamp more than 300 seconds from your clock.',
      '4. Dedupe on `X-Webhook-Id`, then re-fetch the asset before acting on it.',
    ].join('\n'),
    'With the Node SDK, `clipmux.webhooks.verify` does all four. A receiver in any language can test itself against the [webhook test vectors](https://github.com/visheshgubrani/clipmux/blob/main/internal/webhooks/testdata/vectors.json): the same secret, clock, body and headers must give each case its `valid` and `reason`.',
    '## The event body',
    fieldTable({ $ref: '#/components/schemas/WebhookEvent' }),
  ]
  for (const [name, item] of Object.entries(spec.webhooks)) {
    const post = item.post
    parts.push(
      `## \`${name}\``,
      mdx(post.description),
      json(post.requestBody.content['application/json'].example),
    )
  }
  writeFileSync(join(out, 'webhooks.mdx'), `${parts.join('\n\n')}\n`)
}

function generateErrors(ops) {
  const seen = new Map() // code -> { statuses, routes }
  for (const { path, method, op } of ops) {
    for (const [status, r] of Object.entries(op.responses)) {
      for (const code of errorCodes(r)) {
        const e = seen.get(code) ?? { statuses: new Set(), routes: new Set() }
        e.statuses.add(status)
        e.routes.add(`${method} ${path}`)
        seen.set(code, e)
      }
    }
  }
  const missing = [...seen.keys()].filter((c) => !guide.http[c])
  if (missing.length) throw new Error(`site/data/errors.json doesn't explain: ${missing.join(', ')}`)
  const byStatus = new Map()
  for (const [code, e] of seen) {
    const first = [...e.statuses].sort()[0]
    byStatus.set(first, [...(byStatus.get(first) ?? []), code])
  }
  const parts = [
    frontmatter(
      'Errors',
      'Every error code the API returns, what it means and what to do.',
      'site/data/errors.json',
      'TriangleAlert',
    ),
    'Every error is JSON, `{"error": {"code": "snake_case", "message": "…", …details}}`. Act on `code`; `message` is for people and may change. Retry only what says to: a 4xx other than 409 `idempotency_in_progress` and 429 fails the same way again.',
  ]
  for (const status of [...byStatus.keys()].sort()) {
    parts.push(`## ${status} ${STATUS[status] ?? ''}`.trim())
    for (const code of byStatus.get(status).sort()) {
      const e = seen.get(code)
      const also = [...e.statuses].filter((s) => s !== status)
      parts.push(
        `### \`${code}\``,
        `${mdx(guide.http[code])}${also.length ? ` Also returned as ${also.join(', ')}.` : ''}`,
        `Returned by: ${[...e.routes].map((r) => `\`${r}\``).join(', ')}.`,
      )
    }
  }
  const table = (entries) =>
    [
      '| Code | What happened |',
      '| --- | --- |',
      ...Object.entries(entries).map(([c, d]) => `| \`${c}\` | ${cell(d)} |`),
    ].join('\n')
  parts.push(
    "## A video's error_code",
    "When a video's `status` is `errored`, its `error_code` says why. A failed re-encode leaves the video playing and puts the code on the `video.asset.reencode_failed` event's `job`.",
    table(guide.asset),
    "## A track's error_code",
    "When a subtitles or chapters track's `status` is `errored`, its `error_code` says why.",
    table(guide.track),
  )
  writeFileSync(join(out, 'errors.mdx'), `${parts.join('\n\n')}\n`)
}

// Decision references are for the repository; the site drops them.
export function stripInternalRefs(text) {
  return text.replace(/\s*\(((?:D\d{3}(?:,\s*)?)+)\)/g, '').replace(/\((?:D\d{3},\s*)+([^)]*)\)/g, '($1)')
}

function generateChangelog() {
  const src = readFileSync(join(root, 'CHANGELOG.md'), 'utf8')
  const body = stripInternalRefs(src.replace(/^# Changelog\n+/, '')).replace(
    '(docs/security-review.md, findings 1–6)',
    '(the security review, findings 1–6)',
  )
  writeFileSync(
    join(out, 'changelog.mdx'),
    frontmatter(
      'Changelog',
      'What changed in each release of the server and the npm packages.',
      'CHANGELOG.md',
      'History',
    ) + body,
  )
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const ops = generateApi()
  generateWebhooks()
  generateErrors(ops)
  generateChangelog()
  console.log(
    `generate-reference: ${ops.length} operations, ${Object.keys(spec.webhooks).length} webhook events`,
  )
}
