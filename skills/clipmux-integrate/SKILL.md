---
name: clipmux-integrate
description: "Add ClipMux (self-hosted video: upload, encode, signed HLS playback) to an app's backend with the @clipmux/node SDK. Use whenever a user wants video uploads, video hosting, a ClipMux webhook, signed or private playback tokens, captions, subtitles or chapters in a Node, Next.js, Express, Hono, Bun, Deno, SvelteKit, Remix or Cloudflare Workers app, mentions ClipMux, CLIPMUX_* variables or `npx @clipmux/node`, or hits a ClipMux error code. For the browser elements, use clipmux-player as well."
---

# Integrate ClipMux in an app's backend

ClipMux is a self-hosted video server. An app integrates with one command, one
route file and three callbacks; the browser side is two elements (the
`clipmux-player` skill). Docs: https://clipmux.com/docs (every page is also
Markdown at its URL plus `.md`, which is quicker to read).

## Rules that keep the integration safe

- **No secret in browser code.** `CLIPMUX_API_KEY`, `CLIPMUX_SIGNING_KEY` and
  `CLIPMUX_WEBHOOK_SECRET` stay on the server: no `NEXT_PUBLIC_`/`VITE_`
  prefix, no import of `@clipmux/node` from a client component. The browser
  only talks to the app's own `/api/clipmux/*` routes, which is what makes
  access control possible at all.
- **Never print, log or commit the credentials.** `init` writes them without
  printing them; keep it that way, and check the env file is git-ignored.
- **Don't hand-roll HMAC or JWT code.** `clipmuxHandler`,
  `clipmux.webhooks.verify` and `clipmux.playback.token` get the details right
  (raw body, ES256, a longer token for native HLS) that hand-written code gets
  subtly wrong.
- **Decide access in the callbacks, from the app's own session.** Every route
  is default-deny: return `null` to refuse.

## Steps

1. **Install:** `npm i @clipmux/node @clipmux/uploader @clipmux/player`.
2. **Credentials:** run `npx @clipmux/node init --server <ClipMux URL> --no-open`
   (background it: it waits). It prints a code and an `Approve:` URL; give the
   URL to the user, who approves as the server's owner, and add the app's
   webhook URL there (`https://<app>/api/clipmux/webhook`; tick "runs on this
   computer" for a localhost app). It then writes the five `CLIPMUX_*`
   It then writes the five `CLIPMUX_*` variables to `.env.local` (or `.env`
   if only that exists); pass `--env FILE` to write a different env file.
   [Quickstart](https://clipmux.com/docs/build/quickstart#connect-the-app)
3. **One route file** answering `POST /api/clipmux/upload`, `/playback` and
   `/webhook`, from `clipmuxHandler({ upload, playback, asset })` in
   `@clipmux/node/handlers`:
   - Next.js App Router: `app/api/clipmux/[action]/route.ts`,
     `export const POST = clipmuxHandler({…})`.
     [Next.js](https://clipmux.com/docs/build/frameworks/nextjs#the-route-file)
   - Express: `app.post('/api/clipmux/:action', toNodeListener(clipmuxHandler({…})))`
     with `toNodeListener` from `@clipmux/node/express`, mounted **before**
     `express.json()` so the webhook sees the raw body.
     [Express](https://clipmux.com/docs/build/frameworks/express#mount-the-route)
   - Hono, Bun, Deno, SvelteKit, Remix: pass the `Request`, return the
     `Response`. Cloudflare Workers have no `process.env`: pass
     `clipmux: new ClipMux({ env })`.
     [Fetch runtimes](https://clipmux.com/docs/build/frameworks/fetch-runtimes#cloudflare-workers)
4. **The callbacks** (full signatures:
   [route handlers](https://clipmux.com/docs/reference/sdks/node#route-handlers)):
   - `upload(req, { file })` → `{ passthrough, title?, generate? }` or `null`.
     The body is already read, so take your record's ID from the URL the
     uploader posts to (`create-url="/api/clipmux/upload?lesson=42"`) and check
     the session (only instructors upload, say).
   - `playback(req, { playbackId, native })` → `{ viewerId?, asset? }` or
     `null` (SDK releases after 0.1.0 also take `expiresIn`, default 15 min). Look the record up by its stored playback ID, check the viewer
     may watch, and pass `asset: await clipmux.assets.get(assetId)` so the
     session carries tracks and duration.
     [Signed playback](https://clipmux.com/docs/build/play/sign-tokens#sign-a-token)
   - `asset(asset, event)` runs once per verified webhook with the asset
     re-fetched, so it is current. Upsert by `asset.passthrough`; store
     `asset.id`, `asset.status` and the active `asset.playback_ids[].id`.
     Delivery is at least once and may be out of order, so write idempotently.
     If it throws, ClipMux retries for 24 hours.
     [Webhooks](https://clipmux.com/docs/build/webhooks#handle-them-with-clipmuxhandler),
     [writing it safely](https://clipmux.com/docs/build/webhooks#write-the-handler-to-be-safe)
5. **Show the video** with the `clipmux-player` skill: the page gets only the
   playback ID, never a token signed at render time.
6. **Verify:** `npx @clipmux/node doctor` must end with `✓ Ready.`; it also
   prints the Content-Security-Policy sources the pages need. Locally, without a
   reachable webhook URL, `npx @clipmux/node listen --forward-to localhost:<port>/api/clipmux/webhook`
   delivers events signed with the same secret.
   [listen](https://clipmux.com/docs/build/webhooks/listen#with-clipmuxnode).
   A test upload: `npx @clipmux/node upload sample.mp4 --passthrough lesson_42 --wait`.

## Passthrough, captions and chapters

- **`passthrough`** is your own ID (`lesson_42`, up to 255 bytes). It comes
  back on every webhook; `clipmux.assets.findByPassthrough('lesson_42')`
  finds the videos later, newest first.
  [Passthrough](https://clipmux.com/docs/build/upload/passthrough#find-videos-by-it)
- **Subtitles:** return `generate: { subtitles: true }` from `upload`, or
  `clipmux.tracks.create(assetId, { kind: 'subtitles' })` for an existing
  video. They arrive after `video.asset.ready`, on `video.asset.track.ready`.
  [Subtitles](https://clipmux.com/docs/build/captions/subtitles#ask-for-subtitles)
- **Chapters:** `generate: { chapters: true }` (implies subtitles; needs an LLM
  connected on the server, else the track errors `llm_not_connected`).
  [Chapters](https://clipmux.com/docs/build/captions/chapters#ask-for-chapters)
- Missed webhooks: `clipmux.assets.changes({ since })` catches up.
  [Catch up](https://clipmux.com/docs/build/webhooks#catch-up-on-missed-events)

## When something fails

Every error carries a stable `code`; act on it, not the message.
[references/errors.md](references/errors.md) lists every code with what to do.
The ones an integration usually meets:

| Code                                         | Do this                                                                                                         |
| -------------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `ClipMuxConfigError` (names a variable)      | Run `init`, or set that variable on the server side                                                             |
| `unauthorized`                               | Wrong or revoked `CLIPMUX_API_KEY`: run `doctor`                                                                |
| `bad_signature` (webhook 400)                | Wrong `CLIPMUX_WEBHOOK_SECRET`, or the body was parsed and re-serialized: Express route before `express.json()` |
| `stale` (webhook 400)                        | The server clock is off by more than 5 minutes                                                                  |
| `denied` / `clipmux-denied`                  | Your `playback` callback returned `null`, or the playback ID or app is wrong                                    |
| `clipmux_misconfigured` (500 to the browser) | The handler's key or app is wrong: run `doctor` on the server                                                   |
| `clipmux_unavailable` (502 to the browser)   | ClipMux unreachable or failing; the elements retry                                                              |
| `asset_not_ready`                            | Wait for the `asset` callback with `status: 'ready'`                                                            |
| `job_active`                                 | Wait, or pass `supersede: true`                                                                                 |

More: [troubleshooting](https://clipmux.com/docs/build/going-live/troubleshooting),
[errors reference](https://clipmux.com/docs/reference/errors), and before
shipping the [production checklist](https://clipmux.com/docs/build/going-live/production-checklist).
The server's own spec is at `GET <CLIPMUX_URL>/openapi.json`.
