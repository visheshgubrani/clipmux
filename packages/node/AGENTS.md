# Integrating ClipMux with @clipmux/node (for coding agents)

This guide ships with the package, so it matches the version installed. Follow
it in order. The whole integration is one command, one route file, two
elements and three callbacks.

## Rules

- **Never put a secret in browser code.** That covers `CLIPMUX_API_KEY`,
  `CLIPMUX_SIGNING_KEY` and `CLIPMUX_WEBHOOK_SECRET`. The browser talks only to
  the app's own `/api/clipmux/*` routes.
- **Never print or log those values,** and never commit the env file.
- **Don't hand-roll HMAC or JWT code.** Use `clipmuxHandler`,
  `clipmux.webhooks.verify` and `clipmux.playback.token`.
- **Decide access in the callbacks,** from the app's own session. Return
  `null` to refuse.
- **Store videos by `passthrough`,** your own ID such as `lesson_42`. Store
  `asset.id`, `asset.status` and `asset.playback_ids[0].id` from the `asset`
  callback.

## Steps

1. **Install** with `npm i @clipmux/node @clipmux/uploader @clipmux/player`.
2. **Get credentials.** Run `npx @clipmux/node init --server <ClipMux URL> --no-open`.
   - It prints a code and an approval URL. Ask the user to open the URL and
     approve. It then writes `CLIPMUX_*` to `.env.local`, or to `.env` when
     there is a `.env` and no `.env.local`.
   - Without a webhook endpoint, it writes a local `CLIPMUX_WEBHOOK_SECRET` for `listen`.
3. **Add one route** that serves `POST /api/clipmux/upload`, `/playback` and
   `/webhook`:
   - Next.js App Router: `app/api/clipmux/[action]/route.ts` with
     `export const POST = clipmuxHandler({ upload, playback, asset })` from
     `@clipmux/node/handlers`.
   - Express: `app.post('/api/clipmux/:action', toNodeListener(clipmuxHandler({…})))`,
     with `clipmuxHandler` from `@clipmux/node/handlers` and `toNodeListener`
     from `@clipmux/node/express`. Mount it **before** `express.json()`,
     because the webhook needs the raw body.
   - Hono, Bun, SvelteKit, Remix: pass the `Request` to the handler and
     return its `Response`.
   - Cloudflare Workers: there is no `process.env`, so pass the client:
     `clipmuxHandler({ clipmux: new ClipMux({ env }), … })`.
4. **Write the callbacks.**
   - `upload(req, { file })` returns `{ passthrough, title?, playback_policies?, renditions?, generate?: { subtitles?, chapters? } }`
     or `null`. The body is already read: take IDs from the URL, for example
     `create-url="/api/clipmux/upload?lesson=42"`.
   - `playback(req, { playbackId, native })` returns `{ viewerId?, asset? }` or
     `null`. Pass `asset` to include its tracks and duration.
   - `asset(asset, event)` is called once per verified webhook, with the
     current asset. Upsert by `asset.passthrough`. If it throws, ClipMux
     retries the delivery for 24 hours.
5. **Add the elements:**
   - `import '@clipmux/uploader/element'`, then
     `<clipmux-uploader create-url="/api/clipmux/upload?…">`. It fires
     `clipmux-uploaded` with `{asset}`.
   - `import '@clipmux/player/element'`, then
     `<clipmux-player playback-id="…" session-url="/api/clipmux/playback">`. It
     fires `clipmux-denied` when `playback` returns `null`.
   - In React 19 they are plain JSX. The types come with the packages.
   - Your own token route instead of `session-url`: set
     `player.getToken = async ({ playbackId, native }) => …` and sign with
     `clipmux.playback.token(playbackId, { native, viewerId })`. Pass
     `native` through, or Safari before 17.1 stops after 15 minutes.
   - The page's Content-Security-Policy must allow the origins `doctor`
     prints (`connect-src`, `media-src blob:`, `img-src`, `style-src
     'unsafe-inline'`).
6. **Verify.**
   - Run `npx @clipmux/node doctor`, which must end with `✓ Ready`.
   - For local webhooks, run `npx @clipmux/node listen --forward-to localhost:<port>/api/clipmux/webhook`.
     It signs with `CLIPMUX_WEBHOOK_SECRET`, so the handler verifies it
     unchanged; `--since 10m` replays recent events.
   - Test with a file using `npx @clipmux/node upload sample.mp4 --passthrough lesson_42 --wait`.

## When something fails

Every error has a stable `code`.

| Code                                    | Meaning and fix                                                                                             |
| --------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `ClipMuxConfigError` (names a variable) | Run `init`, or set that variable on the server side                                                         |
| `unauthorized`                          | Wrong or revoked `CLIPMUX_API_KEY`; run `doctor`                                                            |
| `insufficient_scope`                    | Create a key with the default scopes (`uploads:write`, `assets:read`, `assets:write`)                       |
| 403 `denied` from the player            | Your `playback` callback returned `null`, or the token's playback ID or app is wrong                        |
| 401 `token_expired`                     | Normal; the player refreshes by itself                                                                      |
| `bad_signature` on the webhook          | Wrong `CLIPMUX_WEBHOOK_SECRET`, or a parsed and re-serialized body (Express: mount before `express.json()`) |
| `stale` on the webhook                  | The server clock is off by more than 5 minutes                                                              |
| `asset_not_ready`                       | Wait for the `asset` callback with `status: 'ready'`                                                        |
| `job_active`                            | Pass `supersede: true`, or wait                                                                             |
| `upload_not_active`                     | The idempotency key's upload has finished; create the upload with a new key                                 |
| 500 `clipmux_misconfigured` (browser)   | The handler's key or app is wrong; run `doctor` on the server                                               |
| 502 `clipmux_unavailable` (browser)     | ClipMux was unreachable or failing; the elements retry                                                      |

The full API, as OpenAPI 3.1, is at `GET <CLIPMUX_URL>/openapi.json`.
