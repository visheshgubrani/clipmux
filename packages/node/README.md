# @clipmux/node

The docs: the [quickstart](https://clipmux.com/docs/build/quickstart) and the [reference](https://clipmux.com/docs/reference/sdks/node).

The server SDK for [ClipMux](https://github.com/visheshgubrani/clipmux), the
self-hosted alternative to Mux. Your backend uses it to create uploads, read
assets, sign playback tokens and verify webhooks. It also ships drop-in route
handlers and a CLI that sets everything up.

- Zero dependencies at runtime, other than `@clipmux/uploader` for `upload`.
  It uses Web APIs only, so it runs on Node ≥ 20.19, Bun, Deno, Cloudflare
  Workers and Vercel Edge.
- ESM only. A CommonJS backend on Node ≥ 20.19 can `require()` it.
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
    // Add renditions: ['1080p', '480p'] to encode only those; left out,
    // every size up to the video's own (up to 4K).
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

`doctor` checks every credential and prints the Content-Security-Policy
sources your pages need (below). `listen` signs what it forwards with your
`CLIPMUX_WEBHOOK_SECRET`, so your handler verifies it unchanged; `--since 10m`
replays recent events. A backend in another language can use the server's
`clipmux listen`, which does the same with a secret of its own.

## Other frameworks

`clipmuxHandler` is a Fetch handler (`Request → Response`), so it works in:
- Hono, Bun and Deno;
- SvelteKit and Remix, as it is;
- Cloudflare Workers, given the client, since Workers have no `process.env`;
- Express, Fastify and plain `node:http`, through the adapter below.

```ts
import { clipmuxHandler } from '@clipmux/node/handlers'
import { toNodeListener } from '@clipmux/node/express'
app.post('/api/clipmux/:action', toNodeListener(clipmuxHandler({ … }))) // before express.json()

// Cloudflare Workers: the settings come from the binding.
export default {
  fetch: (req, env) => clipmuxHandler({ clipmux: new ClipMux({ env }), … })(req),
}
```

Every route is default-deny:
- a callback you don't pass is a 404;
- a callback that returns `null` is a 403;
- the browser routes accept only JSON.

When ClipMux refuses a request, the browser gets what it can act on:
- 400, 409, 413 and 422 pass through with ClipMux's `code` (for example
  `too_large`), and the elements don't retry them;
- 429 passes through with `Retry-After`;
- a wrong key or app is 500 `clipmux_misconfigured`, logged on your server;
- ClipMux unreachable or failing is 502 `clipmux_unavailable`, retried.

## The client

```ts
import { ClipMux } from '@clipmux/node'

const clipmux = new ClipMux() // reads CLIPMUX_URL, _API_KEY, _SIGNING_KID, _SIGNING_KEY, _WEBHOOK_SECRET

const upload = await clipmux.uploads.create({ size, passthrough: 'lesson_42' }) // hand to ClipMuxUpload as is
const asset = await clipmux.assets.get('ast_…')
const [mine] = await clipmux.assets.findByPassthrough('lesson_42')
for await (const changed of clipmux.assets.changes({ since: lastSync })) { … } // missed-webhook catch-up
const token = await clipmux.playback.token('pid_…', { viewerId: user.id, expiresIn: '15m' })
const { hls } = clipmux.playback.urls(asset, token)
const session = await clipmux.playback.session(asset, { viewerId: user.id })
const event = await clipmux.webhooks.verify(rawBody, request.headers)
if (event.type === 'video.asset.track.ready') console.log(event.track.kind) // narrowed: track is there
const transcript = await clipmux.tracks.transcript(asset.id, trackId)
```

**Retries.** Every POST carries an `Idempotency-Key`, and retries reuse it. The
client retries 408, 429 (after `Retry-After`), 5xx and network errors.

**Errors.**
- A failed call throws a `ClipMuxError` with `status`, `code` and `details`.
  Its message says what to do next.
- A missing setting throws a `ClipMuxConfigError` that names the variable. It
  is thrown only when a call needs that setting.

**Native HLS.** iOS before 17.1 and AirPlay play HLS themselves and can't
refresh a token, so a native token lasts the larger of twice the video's
duration and 4 hours (12 hours without a duration), within the server's
maximum. `<clipmux-player>` says when it plays natively:
- with `session-url`, it sends `native`, and `playback.session` signs for it;
- with `getToken`, it calls `getToken({ playbackId, native })`; sign with
  `playback.token(playbackId, { native, durationSeconds, viewerId })`.

**Posters.** `playback.session`'s `poster_url` carries its own token, which
fetches only the poster, so it is safe in an `og:image`. For a poster
alone, sign `playback.token(id, { audience: 'poster' })`.

## Content-Security-Policy

A page with `<clipmux-uploader>` or `<clipmux-player>` must allow, besides
its own origin:

| Directive     | Sources                                                        | Why                                                                                                                                 |
| ------------- | -------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| `connect-src` | the ClipMux URL, the storage bucket's origin, the media origin | the uploader signs and completes parts on the server and PUTs them to storage; the player fetches playlists, segments, keys and VTT |
| `media-src`   | `blob:` and the media origin                                   | Media Source playback, and native HLS                                                                                               |
| `img-src`     | the media origin                                               | the poster                                                                                                                          |
| `style-src`   | `'unsafe-inline'`                                              | both elements inject their styles                                                                                                   |

`npx @clipmux/node doctor` prints the exact origins, from `GET /v1/app`
(`public_url`, `upload_origins`, `base_url`). hls.js runs on the main thread,
so no `worker-src` is needed.

## Licence

MIT. The ClipMux server is AGPL-3.0; this package is MIT so apps embed it
freely.
