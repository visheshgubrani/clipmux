// Writes src/schema.gen.ts from the OpenAPI spec (internal/openapi/openapi.json):
// one exported type per components.schemas entry, plus the webhook event
// types. It handles the subset the spec uses (objects, arrays, enums, $ref,
// oneOf, allOf, nullable via oneOf with null) and fails on anything else,
// so the spec can't drift into something the types silently drop.
//
//   node scripts/generate.ts           write the file
//   node scripts/generate.ts --check   fail if it is out of date

import { readFileSync, writeFileSync } from 'node:fs'

type Schema = {
  $ref?: string
  type?: string
  enum?: string[]
  properties?: Record<string, Schema>
  required?: string[]
  items?: Schema
  oneOf?: Schema[]
  allOf?: Schema[]
  additionalProperties?: Schema | boolean
  description?: string
  [k: string]: unknown
}

const specURL = new URL('../../../internal/openapi/openapi.json', import.meta.url)
const outURL = new URL('../src/schema.gen.ts', import.meta.url)
const spec = JSON.parse(readFileSync(specURL, 'utf8'))

const scalar: Record<string, string> = {
  string: 'string',
  integer: 'number',
  number: 'number',
  boolean: 'boolean',
  null: 'null',
}

function ts(s: Schema, indent: string): string {
  if (s.$ref) {
    const name = s.$ref.replace('#/components/schemas/', '')
    if (name === s.$ref) throw new Error(`unsupported $ref ${s.$ref}`)
    return name
  }
  if (s.oneOf) return s.oneOf.map((x) => ts(x, indent)).join(' | ')
  if (s.allOf) return s.allOf.map((x) => ts(x, indent)).join(' & ')
  if (s.enum) return s.enum.map((v) => `'${v}'`).join(' | ')
  if (s.type === 'array') {
    if (!s.items) throw new Error('array without items')
    const inner = ts(s.items, indent)
    return /[|&]/.test(inner) ? `(${inner})[]` : `${inner}[]`
  }
  if (s.type === 'object' || s.properties) {
    const req = new Set(s.required ?? [])
    const lines: string[] = []
    for (const [name, prop] of Object.entries(s.properties ?? {})) {
      if (prop.description) lines.push(`${indent}  /** ${prop.description} */`)
      lines.push(`${indent}  ${name}${req.has(name) ? '' : '?'}: ${ts(prop, `${indent}  `)}`)
    }
    if (s.additionalProperties && typeof s.additionalProperties === 'object') {
      lines.push(`${indent}  [detail: string]: ${ts(s.additionalProperties, `${indent}  `)} | undefined`)
    }
    return lines.length ? `{\n${lines.join('\n')}\n${indent}}` : 'Record<string, never>'
  }
  if (s.type && scalar[s.type]) return scalar[s.type] as string
  throw new Error(`unsupported schema ${JSON.stringify(s).slice(0, 120)}`)
}

const out: string[] = [
  '// Generated from internal/openapi/openapi.json by scripts/generate.ts. Do not edit.',
  '// Regenerate with: pnpm --filter @clipmux/node run generate',
  '',
]
for (const [name, schema] of Object.entries(spec.components.schemas as Record<string, Schema>)) {
  if (schema.description) out.push(`/** ${schema.description} */`)
  out.push(`export type ${name} = ${ts(schema, '')}`, '')
}
const events = Object.keys(spec.webhooks).map((e) => `'${e}'`)
out.push(
  '/** Every webhook event type the spec lists. */',
  `export type WebhookEventType = ${events.join(' | ')}`,
  '',
)
const text = out.join('\n')

if (process.argv.includes('--check')) {
  if (readFileSync(outURL, 'utf8') !== text) {
    console.error(
      'src/schema.gen.ts is out of date with the spec; run: pnpm --filter @clipmux/node run generate',
    )
    process.exit(1)
  }
} else {
  writeFileSync(outURL, text)
}
