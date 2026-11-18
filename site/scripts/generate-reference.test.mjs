import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mdx, stripInternalRefs, typeOf } from './generate-reference.mjs'

test('decision references are dropped from the changelog, other notes kept', () => {
  assert.equal(stripInternalRefs('Faster encodes (D129): a stage.'), 'Faster encodes: a stage.')
  assert.equal(
    stripInternalRefs('No URL imports (D123, migration 00035).'),
    'No URL imports (migration 00035).',
  )
  assert.equal(stripInternalRefs('Lists (D120, D121) here.'), 'Lists here.')
})

test('MDX-significant characters are escaped outside code only', () => {
  assert.equal(mdx('{public_url}/s/{secret} and `{kept}`'), '\\{public_url\\}/s/\\{secret\\} and `{kept}`')
  assert.equal(mdx('a <b> c'), 'a \\<b\\> c')
})

test('types read as people write them', () => {
  assert.equal(typeOf({ type: 'string', enum: ['signed', 'public'] }), '"signed", "public"')
  assert.equal(typeOf({ type: 'array', items: { $ref: '#/components/schemas/Track' } }), 'array of Track')
  assert.equal(typeOf({ oneOf: [{ type: 'number' }, { type: 'null' }] }), 'number or null')
  assert.equal(typeOf({ type: 'string', format: 'date-time' }), 'string (date-time)')
})
