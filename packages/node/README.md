# @clipmux/node

The server SDK for [ClipMux](https://github.com/visheshgubrani/clipmux), the
self-hosted alternative to Mux. Your backend uses it to create uploads, read
assets, sign playback tokens and verify webhooks. It also ships drop-in route
handlers and a CLI that sets everything up.

- Zero dependencies at runtime, other than `@clipmux/uploader` for `upload`.
  It uses Web APIs only, so it runs on Node ≥ 20, Bun, Deno, Cloudflare
  Workers and Vercel Edge.
- Types are generated from the server's OpenAPI spec (`GET /openapi.json` on
  your instance).

## Four steps

**1. Connect the app.** The owner approves it in the ClipMux dashboard. The
credentials go to `.env.local` and are never printed.

```bash
npm i @clipmux/node @clipmux/uploader @clipmux/player
npx @clipmux/node init --server https://video.example.com
```

**2. Add one route file.** In Next.js it goes at
`app/api/clipmux/[action]/route.ts`:

```ts
import { clipmuxHandler } from '@clipmux/node/handlers'

export const POST = clipmuxHandler({
  // An instructor uploads: say what the upload is, or return null to refuse.
  async upload(req, { file }) {
    const user = await requireInstructor(req)
    const lesson = new URL(req.url).searchParams.get('lesson')
    return { passthrough: `lesson_${lesson}`, title: file.name, generate: { subtitles: true } }
  },
  // A student asks to watch: return who they are, or null to refuse.
  async playback(req, { playbackId }) {
    const user = await getUser(req)
    return (await canWatch(user, playbackId)) ? { viewerId: user.id } : null
  },
  // A verified webhook, with the asset re-fetched so it is current.
  async asset(asset) {
    await db.lesson.update({
      where: { ref: asset.passthrough },
      data: { status: asset.status, playbackId: asset.playback_ids[0]?.id },
    })
  },
})
```

**3. Add two elements** to your pages:

```html
<script type="module">
  import '@clipmux/uploader/element'
  import '@clipmux/player/element'
</script>
<clipmux-uploader create-url="/api/clipmux/upload?lesson=42"></clipmux-uploader>
<clipmux-player playback-id="pid_…" session-url="/api/clipmux/playback"></clipmux-player>
```

The browser never sees the ClipMux URL, an API key or a signing key.

**4. Check the setup**, and forward webhooks to your laptop while you develop:

```bash
npx @clipmux/node doctor
npx @clipmux/node listen --forward-to localhost:3000/api/clipmux/webhook
```

## Other frameworks

`clipmuxHandler` is a Fetch handler (`Request → Response`), so it works in:
- Hono, Bun, Deno and Cloudflare Workers;
- SvelteKit and Remix, as it is;
- Express, Fastify and plain `node:http`, through the adapter below.

```ts
import { toNodeListener } from '@clipmux/node/express'
app.post('/api/clipmux/:action', toNodeListener(clipmuxHandler({ … }))) // before express.json()
```

Every route is default-deny:
- a callback you don't pass is a 404;
- a callback that returns `null` is a 403;
- the browser routes accept only JSON.

## The client

```ts
import { ClipMux } from '@clipmux/node'

const clipmux = new ClipMux() // reads CLIPMUX_URL, _API_KEY, _SIGNING_KID, _SIGNING_KEY, _WEBHOOK_SECRET

const upload = await clipmux.uploads.create({ size, passthrough: 'lesson_42' })
const asset = await clipmux.assets.get('ast_…')
const [mine] = await clipmux.assets.findByPassthrough('lesson_42')
for await (const changed of clipmux.assets.changes({ since: lastSync })) { … } // missed-webhook catch-up
const token = await clipmux.playback.token('pid_…', { viewerId: user.id, expiresIn: '15m' })
const { hls } = clipmux.playback.urls(asset, token)
const session = await clipmux.playback.session(asset, { viewerId: user.id })
const event = await clipmux.webhooks.verify(rawBody, request.headers)
const transcript = await clipmux.tracks.transcript(asset.id, trackId)
```

**Retries.** Every POST carries an `Idempotency-Key`, and retries reuse it. The
client retries 408, 429 (after `Retry-After`), 5xx and network errors.

**Errors.**
- A failed call throws a `ClipMuxError` with `status`, `code` and `details`.
  Its message says what to do next.
- A missing setting throws a `ClipMuxConfigError` that names the variable. It
  is thrown only when a call needs that setting.

**Native HLS.** `playback.session` with `native: true` signs for the larger of
twice the video's duration and 4 hours, because iOS before 17.1 and AirPlay
can't refresh a token. `<clipmux-player>` sends `native` itself.

## Licence

MIT. The ClipMux server is AGPL-3.0; this package is MIT so apps embed it
freely.
