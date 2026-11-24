import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  envProblems,
  errorCodeProblems,
  frontmatterProblems,
  headProblems,
  internalIdProblems,
  linkProblems,
  llmsProblems,
  pinnedAnchorProblems,
  routeProblems,
  routesIn,
  skillFrontmatterProblems,
  skillLinks,
  specRoutes,
} from './lint.mjs'

test('frontmatter needs a title and a one-sentence description', () => {
  assert.deepEqual(
    frontmatterProblems('---\ntitle: Upload\ndescription: Upload a video from the browser.\n---\n'),
    [],
  )
  assert.deepEqual(frontmatterProblems('# no frontmatter'), ['no frontmatter'])
  assert.deepEqual(frontmatterProblems('---\ndescription: One sentence.\n---'), ['no title'])
  assert.deepEqual(frontmatterProblems('---\ntitle: T\n---'), ['no description'])
  assert.deepEqual(frontmatterProblems('---\ntitle: T\ndescription: First. Second.\n---'), [
    'description is not one sentence',
  ])
  assert.deepEqual(frontmatterProblems('---\ntitle: T\ndescription: No full stop\n---'), [
    'description is not one sentence',
  ])
})

test('internal IDs are refused', () => {
  assert.deepEqual(internalIdProblems('Keys rotate (D069), see P6-T15.2, until launch.'), [
    'internal reference "D069"',
    'internal reference "P6-T15.2"',
    'internal reference "until launch"',
  ])
  assert.deepEqual(internalIdProblems('A 1080p rendition, 4K, H.264 and AES-128.'), [])
})

test('routes are checked against the spec, whatever the parameter is called', () => {
  const known = specRoutes({
    paths: { '/v1/assets/{id}': { get: {}, delete: {} }, '/v1/uploads': { post: {} } },
  })
  assert.deepEqual(routeProblems('Call `GET /v1/assets/{asset_id}` or `POST /v1/uploads`.', known), [])
  assert.deepEqual(routeProblems('Call `PATCH /v1/assets/{id}`.', known), [
    'unknown route "PATCH /v1/assets/{id}"',
  ])
})

test('routes are read from api.md prose and table rows', () => {
  assert.deepEqual(routesIn('`GET /v1/events?after=`, and | `POST` | `/v1/session` |'), [
    'GET /v1/events',
    'POST /v1/session',
  ])
})

test('settings must be documented', () => {
  const known = new Set(['CLIPMUX_PUBLIC_URL'])
  assert.deepEqual(envProblems('Set CLIPMUX_PUBLIC_URL, not CLIPMUX_PUBLIC_ULR.', known), [
    'unknown setting CLIPMUX_PUBLIC_ULR',
  ])
})

test('llms.txt has the llmstxt.org shape and absolute links to files that exist', () => {
  const exists = (p) => p === '/docs.md'
  const good =
    '# ClipMux\n\n> Summary.\n\n## Start\n\n- [Introduction](https://clipmux.com/docs.md): Intro.\n'
  assert.deepEqual(llmsProblems(good, exists), [])
  assert.deepEqual(llmsProblems('ClipMux\n- [a](/docs.md)\n- [b](https://clipmux.com/docs/x.md)', exists), [
    'llms.txt does not start with an H1',
    'llms.txt has no blockquote summary',
    'llms.txt has no sections',
    'llms.txt link is not absolute: /docs.md',
    'llms.txt links to a missing file: https://clipmux.com/docs/x.md',
  ])
})

test('internal links and anchors resolve; redirects and assets are skipped', () => {
  const resolve = (p) => (p === '/docs/build' ? { html: '<h2 id="upload">Upload</h2>' } : null)
  const html =
    '<a href="/docs/build#upload"></a><a href="/docs/build#nope"></a><a href="/docs/gone"></a>' +
    '<a href="/install.sh"></a><a href="/_next/static/x.js"></a><a href="https://example.com/"></a>'
  assert.deepEqual(linkProblems(html, 'index.html', resolve, new Set(['/install.sh'])), [
    'index.html: missing anchor /docs/build#nope',
    'index.html: broken link /docs/gone',
  ])
})

test('skill links to the docs are read, with their anchors', () => {
  assert.deepEqual(
    skillLinks(
      'See [x](https://clipmux.com/docs/build/quickstart#connect-the-app), https://clipmux.com/docs/reference/errors#<code>, and `https://clipmux.com/docs`.',
    ),
    [
      { path: '/docs/build/quickstart', anchor: 'connect-the-app' },
      { path: '/docs/reference/errors', anchor: '' },
      { path: '/docs', anchor: '' },
    ],
  )
})

test('an anchor a skill uses is pinned on its heading', () => {
  const page = '## Connect the app [#connect-the-app]\n\n## Check it\n'
  assert.deepEqual(pinnedAnchorProblems(page, 'connect-the-app'), [])
  assert.deepEqual(pinnedAnchorProblems(page, 'check-it'), [
    'anchor #check-it is not pinned: write the heading as `## … [#check-it]`',
  ])
})

test('the skill lists every error code', () => {
  const spec = {
    paths: {
      '/v1/x': {
        post: {
          responses: {
            409: {
              content: {
                'application/json': {
                  schema: {
                    properties: {
                      error: { properties: { code: { enum: ['job_active', 'asset_not_ready'] } } },
                    },
                  },
                },
              },
            },
          },
        },
      },
    },
  }
  const guide = { asset: { transcode_failed: '' }, track: { no_audio: '' } }
  assert.deepEqual(errorCodeProblems('`job_active` `transcode_failed`, `no_audio`', spec, guide), [
    'no row for asset_not_ready',
  ])
})

test("a skill's frontmatter parses, with its directory's name", () => {
  assert.deepEqual(
    skillFrontmatterProblems(
      '---\nname: clipmux-player\ndescription: "Video: the player."\n---\n',
      'clipmux-player',
    ),
    [],
  )
  assert.deepEqual(
    skillFrontmatterProblems(
      '---\nname: clipmux-player\ndescription: Video: the player.\n---\n',
      'clipmux-player',
    ),
    ['the description has ": " and is not quoted'],
  )
  assert.deepEqual(
    skillFrontmatterProblems('---\nname: other\ndescription: Plays.\n---\n', 'clipmux-player'),
    ['name is other, not clipmux-player'],
  )
})

test('a page names its canonical URL and carries parseable JSON-LD', () => {
  const ld = '<script type="application/ld+json">{"@type":"TechArticle"}</script>'
  const canonical = '<link rel="canonical" href="https://clipmux.com/docs/build"/>'
  assert.deepEqual(headProblems(`<head>${canonical}${ld}</head>`, 'https://clipmux.com/docs/build'), [])
  assert.deepEqual(headProblems(`<head>${ld}</head>`, 'https://clipmux.com/docs/build'), [
    'no canonical link',
  ])
  assert.deepEqual(headProblems(`<head>${canonical}${ld}</head>`, 'https://clipmux.com/docs/build/upload'), [
    'canonical is https://clipmux.com/docs/build, not https://clipmux.com/docs/build/upload',
  ])
  assert.deepEqual(headProblems(`<head>${canonical}</head>`, 'https://clipmux.com/docs/build'), [
    'no JSON-LD',
  ])
  assert.deepEqual(
    headProblems(
      `<head>${canonical}<script type="application/ld+json">{"@type":</script></head>`,
      'https://clipmux.com/docs/build',
    ),
    ['JSON-LD does not parse'],
  )
})
