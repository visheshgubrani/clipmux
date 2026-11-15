# Integrating ClipMux into an app

How an app's backend and pages use ClipMux to upload, process and play video,
with the released SDKs: `@clipmux/node` (server), `@clipmux/uploader` and
`@clipmux/player` (browser). Every route and field is in [api.md](api.md); this
page is the path through them.

Edural (edural.com, an LMS) is the first live integration and the worked example
throughout, in the "Edural" notes and at the end. Edural keeps its own records,
access rules and routes, so it calls the client from its own handlers instead of
mounting `clipmuxHandler`. If your app has no such rules yet, the four steps in
`packages/node/README.md` are shorter.

```mermaid
sequenceDiagram
  participant B as Browser
  participant A as Your backend
  participant C as ClipMux (app.example.com)
  participant S as Storage (R2)
  participant W as Media (media.example.com)
  B->>A: "I want to upload this file" (size, name)
  A->>C: uploads.create {size, signed, passthrough: your record id, generate}
  C-->>A: upload (upload_token, endpoint), asset_id
  A-->>B: upload + endpoint (no API key)
  B->>C: sign parts (upload token)
  B->>S: PUT each part
  B->>C: complete
  C->>A: webhook video.asset.* (HMAC)
  A->>C: assets.get (re-fetch; the body is only a trigger)
  B->>A: "I want to watch" (your access check, every time)
  A-->>B: playback session, signed locally (ES256)
  B->>W: HLS, keys, VTT, poster with the token
```

## 1. Credentials

Make one ClipMux app for each environment of your backend: production, and one
per developer or staging, so a dev upload never shows up in production lookups.
Assets, events and `passthrough` lookups are scoped to the app.

On the ClipMux host, append the bundle straight to the backend's env file, so
nothing is printed:

```bash
clipmux admin create-integration --app "My App" --webhook-url https://api.example.com/hooks/clipmux >> .env
```

Or approve the app in the dashboard (`npx @clipmux/node init --server https://app.example.com`).
Either way you get the five variables `@clipmux/node` reads:

| Variable | What it is | Who sees it |
| --- | --- | --- |
| `CLIPMUX_URL` | the instance's public URL | the browser too (the upload's `endpoint`) |
| `CLIPMUX_API_KEY` | `cmx_…`, scopes `uploads:write assets:read assets:write` | backend only |
| `CLIPMUX_SIGNING_KID` | `kid_…`, which key signed a token | backend only |
| `CLIPMUX_SIGNING_KEY` | the PKCS #8 PEM; ClipMux keeps only its public half | backend only |
| `CLIPMUX_WEBHOOK_SECRET` | `whsec_…`, signs webhook deliveries | backend only |

`create-integration` writes the PEM on one line with `\n` escapes, in double
quotes. Node's `--env-file`, dotenv and Docker Compose read that as is, and
`importSigningKey` accepts both forms.

Check the setup, and get the Content-Security-Policy origins your pages need:

```bash
npx @clipmux/node doctor --env-file .env
```

```
✓ CLIPMUX_API_KEY works for app "Edural dev" (app_…)
✓ media URL https://edural.clipmux.com; tokens may live up to 168 h
✓ CLIPMUX_SIGNING_KEY matches kid_…
Pages with the uploader or player need, in their Content-Security-Policy:
  connect-src https://app.clipmux.com https://<account>.r2.cloudflarestorage.com https://edural.clipmux.com
  media-src blob: https://edural.clipmux.com
  img-src https://edural.clipmux.com
  style-src 'unsafe-inline'
```

Rotation:
- **API key:** `create-key` a second key, deploy it, then `revoke-key` the old one.
- **Signing key:** `rotate-signing-key`. The old kid keeps verifying until it is retired.
- **Webhook secret:** `create-webhook` for the same URL.

> **pnpm 10+ and fresh releases.** pnpm refuses a package published more recently
> than its `minimumReleaseAge`. `pnpm add` writes a `minimumReleaseAgeExclude`
> entry for each just-released `@clipmux/*` version into `pnpm-workspace.yaml`.
> Commit that entry, or the next `pnpm install --frozen-lockfile` in CI fails.

## 2. Your record and the passthrough

Tie each asset to one record of yours and keep that link on your side. Make
the record before you call ClipMux, and send its ID as `passthrough`. It comes
back on the asset and on every webhook, so a webhook finds its record without
trusting anything else in the body.

Store on the record:
- `asset_id`, known as soon as the upload is created;
- the upload ID, to cancel it;
- the signed playback ID and the subtitles and chapters track IDs, from the re-fetched asset;
- the highest `version` you have stored.

> **Edural** keeps one `video_upload_attempts` row per upload, and its ID is the
> passthrough. A lesson points at its current attempt (what students watch) and
> its latest attempt (the only one allowed to replace it), so an older upload
> that finishes late can never take over the lesson.

## 3. Uploads

On your backend, after your own permission check:

```ts
import { ClipMux } from '@clipmux/node'
const clipmux = new ClipMux() // or pass url, apiKey, … explicitly

const upload = await clipmux.uploads.create(
  { size: file.size, playback_policy: 'signed', passthrough: record.id, title: file.name, generate: { subtitles: true, chapters: true } },
  { idempotencyKey: record.id }, // a retry can't make a second upload
)
// Store upload.asset_id and upload.upload_id on the record, then answer the page with:
return { upload: { upload_id, upload_token, part_size, part_count }, endpoint: upload.endpoint }
```

In the page:

```ts
import { ClipMuxUpload } from '@clipmux/uploader'

const controller = new AbortController()
await new ClipMuxUpload(file, {
  upload,
  endpoint,
  signal: controller.signal,
  onProgress: (p) => setProgress(p.bytesUploaded / p.bytesTotal),
}).start()
// Tell your backend it finished, so it can re-fetch the asset at once instead of waiting for the webhook.
```

- The page never sees an API key. `upload_token` reaches only this upload.
- Parts go straight to storage: `connect-src` needs the bucket's origin, and the bucket must expose `ETag`. ClipMux's Cloudflare Connect configures both.
- To cancel, abort in the page and call `clipmux.uploads.cancel(uploadId)` on the backend; the asset becomes `errored / upload_cancelled`. Treat 404 and 409 as already done.
- An idle upload stays open for 24 hours. An asset still `waiting_upload` after that will never finish, so delete it.
- `<clipmux-uploader create-url>` does all of this if your backend answers its `{size, name, type}` POST with the created upload.

> **Edural** runs uploads from a queue that survives page changes (three at a
> time). A Cancel or a failure cancels at ClipMux too, so nothing sits half-uploaded.

## 4. Webhooks and state

Mount one route for the raw body:

```ts
const raw = await req.text() // the exact bytes; never re-serialize
let event
try {
  event = await clipmux.webhooks.verify(raw, req.headers) // ±300 s, HMAC over `${timestamp}.${raw}`
} catch (err) {
  return new Response(null, { status: 401 }) // WebhookVerificationError
}
```

Then:
1. **Dedupe** on `event.id` (delivery is at least once). Commit the receipt
   in the same transaction as the state change, so a replay changes nothing.
2. **Find the record** by `event.data.passthrough`. Acknowledge (200) and
   ignore events whose passthrough isn't yours; the dashboard's test event has none.
3. **Re-fetch** with `clipmux.assets.get(event.data.id)`. The body says that
   something changed, not what it is now.
4. **Ignore stale snapshots.** Apply the asset only if its `version` is at
   least the one stored.
5. Answer non-2xx when your own store fails, and ClipMux retries.

Status mapping that works:

| Asset | Your record |
| --- | --- |
| `waiting_upload` | uploading |
| `processing` | processing |
| `ready` with an active signed playback ID | ready (never moves back) |
| `errored`, `deleted` | failed |

| Track | Stage |
| --- | --- |
| `preparing` | pending |
| `ready` | ready |
| `errored` | failed; an `errored` chapters track with `llm_not_connected` means no LLM is connected yet, so show nothing rather than an error |

Tracks are made after the video is ready and never hold it up. Expect
`video.asset.ready` first, then `video.asset.track.ready` or `video.asset.track.errored`.

**A missed webhook** (your backend was down for longer than the retries) is
caught by a sweep: re-fetch each record still in flight, or walk
`clipmux.assets.changes({ since })`.

> **Edural** runs a one-minute pg-boss sweep and also reconciles when a
> teacher's page polls (at most once per 10 s per upload). A
> `video.asset.track.ready` stores the transcript at once.

## 5. Transcripts

```ts
const t = await clipmux.tracks.transcript(asset.id, subtitlesTrackId)
// { track_id, language, duration_ms, text, segments: [{ id, start_ms, end_ms, text }] }
```

Milliseconds, snake_case. `language` is detected (`en`, `hi`, …). A 404 or 409
means there is no transcript to wait for. Strip `\u0000` before storing it in
Postgres `text`.

## 6. Playback

Every play and every renewal goes through your access check, then a token
signed locally. Signing makes no ClipMux call: only the media origin and the
maximum token lifetime are read, once, from `GET /v1/app`.

```ts
// POST /api/lessons/:id/playback  { native?: boolean }
if (!(await canWatch(viewer, lesson))) return new Response(null, { status: 403 })
const session = await clipmux.playback.session(playbackId, {
  viewerId: viewer.id, // a stable ID: unique viewers in Analytics; a random one for anonymous viewers
  native,              // the browser plays HLS itself (iOS < 17.1, AirPlay) and can't renew
  expiresIn: '15m',
})
return Response.json({ ...session, chapters_track_id: readyChaptersTrackId ?? null })
```

- A **string** target (a stored playback ID) gives a session with no tracks
  and no duration. A **native** token is then signed for 12 hours. For a
  shorter one, sign `playback.token(id, { native: true, durationSeconds, viewerId })`.
  Or pass the asset itself, which costs an `assets.get`.
- `poster_url` carries a poster-only token (`aud: t`), so it is safe in an
  `og:image`. For a poster alone (a list page, say), sign
  `playback.token(id, { audience: 'poster', expiresIn: '1h' })` and build the URL
  with `playback.urls({ baseUrl, playbackId, policy: 'signed' }, token).poster`.
- **Free previews:** keep the playback ID signed and give anyone a token,
  rather than adding a public playback ID. You can still stop it, and a shared
  URL dies with its token.
- Delivery answers 401 `token_expired` (the player renews), 403 for a token
  that is wrong or refused, and 404 for a removed playback ID.

## 7. The player in React or Next.js

```tsx
'use client'
import type {} from '@clipmux/player/react'            // JSX types, no React dependency
import type { ClipMuxPlayerElement } from '@clipmux/player/element'

export function Player({ grant, renew, onDenied }) {
  const ref = useRef<ClipMuxPlayerElement>(null)
  const [defined, setDefined] = useState(false)
  useEffect(() => { import('@clipmux/player/element').then(() => setDefined(true)) }, []) // browser only
  useEffect(() => {
    const el = ref.current
    if (!defined || !el) return
    let first: string | null = grant.token
    el.getToken = async ({ native }) => {
      const t = first && !native ? first : null
      first = null
      return t ?? renew({ native }).then((s) => s.token, () => null) // null stops playback
    }
    el.addEventListener('clipmux-denied', onDenied)
    return () => el.removeEventListener('clipmux-denied', onDenied)
  }, [defined])
  return (
    <clipmux-player ref={ref} base-url={grant.base_url} playback-id={grant.playback_id}
      poster={grant.poster_url} chapters-track={grant.chapters_track_id ?? undefined} />
  )
}
```

- Import the element in an effect: it needs the DOM and Media Source, so a server render must not load it.
- Set `getToken` rather than `token`, so the first token can be a native one when the browser needs it.
- `clipmux-denied` means your backend said no on a renewal. Show your own sign-in or enroll state, not an error. In Next.js, `router.refresh()` re-renders the page's locked state.
- `el.media` is the `<hlsjs-video>`: `currentTime`, `paused`, `ended`, `duration`, `readyState` and media events, for resume and progress reporting.
- Subtitles come inside HLS and appear in the captions menu with no attribute. Chapters need `chapters-track`.
- Theming: `--media-accent-color` on an ancestor colours the controls.
- Keep `"skipLibCheck": true`, since Video.js 10.2's declarations don't all type-check.

## 8. Deleting

`clipmux.assets.delete(id)`. A 200 (gone), 202 (deleting) and 404 (already
gone) all mean done. Delete from a sweep, not inline, so a failure is retried:
replaced uploads, cancelled ones, and records whose parent was deleted.

## 9. Local development

```bash
npx @clipmux/node listen --env-file .env --forward-to localhost:4100/hooks/clipmux
```

`listen` long-polls the dev app's events and signs what it forwards with
*your* `CLIPMUX_WEBHOOK_SECRET`, so the handler verifies it unchanged. The app
needs no webhook endpoint for this; any local `whsec_…` value works. `--since 10m`
replays recent events.

## 10. Going live

- [ ] A separate production app, its bundle only in the production env file.
- [ ] The webhook URL registered (`--webhook-url`), reachable, answering 200.
- [ ] `doctor` passes with the production env.
- [ ] Uploads: parts reach storage from the production origin (CORS), and the record goes uploading → processing → ready without a reload.
- [ ] Playback: renewals keep a 15+ minute session playing; a refused renewal shows your locked state.
- [ ] Cleanup: a replaced or removed video's asset is deleted by the next sweep.
- [ ] Analytics: views and unique viewers appear (needs `vid`).
- [ ] Nothing logs tokens, signed URLs, keys or webhook signatures.

## Edural, the worked example

Edural's integration lives in its repository:
- `api/src/integrations/clipmux.ts`, the only importer of `@clipmux/node`;
- `api/src/modules/media/`, which holds state and the sweep;
- `sdk/src/react/player.tsx`, the student player;
- the portal's upload queue and preview.

| Edural route | ClipMux call |
| --- | --- |
| `POST /academies/{a}/lessons/{l}/video/uploads` | `uploads.create` (passthrough = attempt ID, idempotency key = attempt ID) |
| `POST /academies/{a}/video-uploads/{u}/complete` | `assets.get` → reconcile |
| `DELETE /academies/{a}/video-uploads/{u}` | `uploads.cancel` (best effort; the sweep deletes the asset) |
| `POST /hooks/clipmux` | `webhooks.verify` → `assets.get` → reconcile |
| `POST /academies/{a}/lessons/{l}/video/playback` (teacher preview) | `playback.session`, `vid = staff:<id>` |
| `POST /student/lessons/{l}/playback` | `playback.session`, `vid = student:<id>` or `anon:<random>` |
| minute sweep | `assets.get`, `assets.findByPassthrough`, `assets.delete`, `tracks.transcript` |

Verified live on 2026-11-15, against `app.clipmux.com` with `@clipmux/node`
0.1.0, `@clipmux/uploader` 0.5.0 and `@clipmux/player` 0.6.0 from npm:
- The browser upload made three calls (parts sign, part PUT to R2, complete), all 200.
- A 66-second clip was ready in about 80 s on Modal; `video.asset.ready` and `video.asset.track.ready` reached the backend through `listen`.
- The transcript was stored from the track event, and the AI draft started.
- The teacher preview and the student player both played from `edural.clipmux.com`: master, variant, key, segments, poster and subtitles all 200.
- A renewal was signed per viewer (15 min), and native renewals got 4 h tokens.
- A refused renewal returned `null`, and the app fell back to its enroll page.

## Troubleshooting (met during the Edural integration)

| Symptom | Cause | Fix |
| --- | --- | --- |
| `DELETE /v1/assets/{id}` → 500 `internal`; the server logs `delete tracks: … violates check constraint "tracks_check"` | servers before this fix couldn't mark an `errored` track deleted (for example chapters `llm_not_connected`) | fixed in the server (Unreleased); upgrade. Until then, retry from a sweep |
| Chrome logs `Refused to get unsafe header "Retry-After"` once per part | the uploader reads `Retry-After` from every part PUT, and the bucket's CORS doesn't expose it | harmless; fix pending in the uploader (known-gaps.md) |
| `ERR_PNPM_…` minimum release age on install | pnpm refuses packages younger than its threshold | keep the `minimumReleaseAgeExclude` entry pnpm wrote |
| Delivery answers 403 for a URL with a token | the token is malformed or signed with another app's key | check `CLIPMUX_SIGNING_KID` with `doctor`; an expired token is 401, not 403 |
| A chapters track always ends `errored` | no LLM connected for chapters | Captions settings → connect an LLM; existing assets need `tracks.create(id, { kind: 'chapters' })` |
