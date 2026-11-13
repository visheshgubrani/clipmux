# Changelog

What changed in each release, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
form. The server is versioned by its `v*` tag; each npm package by its own
`package.json` version (D092). The npm 0.1.x releases of `@clipmux/player` and
`@clipmux/uploader` came from the archived predecessor and share nothing with
these.

## Unreleased

### @clipmux/node 0.1.0

The first release: the client, the drop-in handlers, webhook verification
and the `init`, `doctor`, `listen` and `upload` CLI.

Fixed before release:
- `listen` and `assets.waitUntilReady` sent an event ID as `after`, which
  the server refuses (it takes a time), so both failed on the first event;
  and past 100 events they read the same page forever. They now move their
  window as the server's `clipmux listen` does. `listen` takes `--since`.
- `uploads.create` returns `upload_token` as required, so its answer goes
  into `new ClipMuxUpload(file, { upload })` as it is. An idempotent replay
  of a finished upload throws `upload_not_active`.
- The handlers no longer turn every ClipMux 4xx into 502: 400, 409, 413 and
  422 keep their status and code, 429 keeps `Retry-After`, a wrong key or
  app is 500 `clipmux_misconfigured`, and ClipMux unreachable is 502
  `clipmux_unavailable`.
- `playback.token()` takes `native` and `durationSeconds`, and reads the
  server's maximum lifetime from `/v1/app` (waiting at most 3 s).
- `playback.session()` signs `poster_url` with a poster-only token.
- `WebhookEvent` is a union discriminated on `type`; a track event has its
  `track`.
- `doctor` prints the Content-Security-Policy sources a page needs.
- Node ≥ 20.19 (ESM only).

### @clipmux/player 0.6.0

- `getToken` receives `{ playbackId, native }`, so a token route can sign a
  session-length token for native HLS. A callback with no arguments still
  works.
- `PlaybackSession` accepts what `@clipmux/node`'s `playback.session()`
  answers (`expires_at` may be null; `title` and `duration_seconds`).
- The chapter type is `PlayerChapter` (seconds); `Chapter` is a deprecated
  alias.
- Content-Security-Policy guidance in the README.
- Installs next to `@types/react` 18 (the optional peer was `>=19`, which
  npm refused).

### @clipmux/uploader 0.5.0

- A given `upload` without `upload_token` is refused at once instead of
  sending `Bearer undefined`.
- Installs next to `@types/react` 18.
- Content-Security-Policy guidance in the README.

### Server

- `GET /v1/app` lists `upload_origins`, for an app's Content-Security-Policy.
- `clipmux admin create-key` defaults to the scopes `doctor` checks
  (`uploads:write`, `assets:read`, `assets:write`) and accepts spaces in
  `--scopes`.
- `clipmux listen` reads `CLIPMUX_URL` when `CLIPMUX_SERVER` is unset.
- The share page's `poster_url` carries a poster-only token.
