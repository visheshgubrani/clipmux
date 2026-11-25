# Changelog

What changed in each release, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
form. The server is versioned by its `v*` tag; each npm package by its own
`package.json` version (D092). The npm 0.1.x releases of `@clipmux/player` and
`@clipmux/uploader` came from the archived predecessor and share nothing with
these.

## Unreleased

## v1.0.0-rc.2 - 2026-11-25

### Added

- Encrypted database backups and disaster recovery (D143, D144, D145,
  D146, D147, D148): scheduled age-encrypted PostgreSQL backups to off-host
  storage, a backup before every migration, scratch restores that verify
  each backup, source mirroring, and `clipmux admin restore` behind a restore fence and store
  epoch, so a restored database can't delete newer media. Backup status is
  on the dashboard and in the metrics. Restores were rehearsed against
  Backblaze B2, including Object Lock; a cold restore reached playback in
  72 s.
- Analytics: how much storage the videos take (sources, encoded files and
  tracks), with deleted videos left out.
- Agent skills (`npx skills add visheshgubrani/clipmux`):
  `clipmux-integrate` for the backend and `clipmux-player` for the
  frontend.
- Example apps: `examples/nextjs-lms` and `examples/express`.

### Changed

- One shape for every way a video gets in (D150). `POST /v1/uploads`,
  `POST /v1/assets` (a URL or an agent source) and agent attach take the
  same settings, checked in one place: `playback_policies`, `passthrough`,
  `title`, `generate` and `renditions`. **Breaking:** `playback_policy:
  "public"` is now `playback_policies: ["public"]`, and
  `["signed", "public"]` makes one ID of each in the same call. The old
  field is ignored, so a request still sending it gets one signed ID.
  `@clipmux/node`'s types and its handler's `UploadDecision` follow.
- Imports from a URL are capped by `CLIPMUX_MAX_UPLOAD_BYTES`, like uploads.
- The image keeps its layers the same across releases (D142): a host that
  ran the previous release downloads only what changed, about 17 MB per
  architecture when only ClipMux changed, instead of 1.7 GB.
- Cloud GPUs: Modal and RunPod default to 10 machines at once (D141).
- Cloud GPUs wake in parallel (D149): one slow provider call no longer holds
  up the others, and a machine woken late still gets its whole claim
  window. Modal waits up to 90 s for placement.

### Added

- Dashboard: **Upload** imports a video from a URL as well as files, with
  the same settings.
- `clipmux push` and `npx @clipmux/node upload` take an `http(s)` URL in
  place of a file and import it. `clipmux push --policy signed,public`
  makes both playback IDs.

### Fixed

- RunPod: ClipMux now uses RunPod's REST API v2. RunPod retired v1 on
  2026-11-15, so in rc.1 Connect RunPod failed with `lookup rest.runpod.io
  … no such host`, and no pod could start. RunPod now places a pod on one
  GPU type at a time, so the chosen GPUs are tried in order until one has
  capacity; with none free, the wake waits and tries again (D139).
- The README's quick start installs from `clipmux.com/install.sh`.
  `releases/latest/download/install.sh` is a 404 while every release is a
  prerelease.
- Dashboard: the jobs page no longer calls a running delete, move or
  chapters job "Not claimed yet". The server runs those itself, and a
  delete or move waits until the video's upload links expire, up to 16
  minutes after its last job. An encode cancelled before it ran no longer
  reads "Done by the server".
- Analytics: views and watch time are no longer undercounted by Analytics
  Engine sampling, aborted streams count only the bytes sent, the edge pull
  starts from when analytics was connected, and built-in delivery counts
  range and `HEAD` requests correctly.
- Backups to a bucket with default Object Lock send the checksum it
  requires, and a key scoped to one bucket no longer reads a 403 as a
  missing object.
- Dashboard: chapter navigation uses the shared player's seek bar. Public
  embed snippets include the ready chapter track when there is one.

### @clipmux/node 0.2.0

- **Breaking:** `uploads.create`, `assets.importUrl` and the handler's
  `UploadDecision` take `playback_policies: ['signed' | 'public', …]`
  instead of `playback_policy` (D150). `UploadDecision` is now the shared
  `AssetSettings` type.
- `npx @clipmux/node upload URL` imports a public `http(s)` URL.

### @clipmux/player 0.6.1

- Chapters load when signed playback starts and update in place when a
  track is replaced, removed, or supplied by a renewed session.

## v1.0.0-rc.1 - 2026-11-19

The first release of the server: the image, the agent tarballs, the
`clipmux` binaries and the one-line installers, on a GitHub Release.

### Install and upgrade

- `curl -fsSL https://clipmux.com/install.sh | sudo sh` installs ClipMux,
  Postgres and Caddy in `/opt/clipmux` from
  `ghcr.io/visheshgubrani/clipmux:v1.0.0-rc.1` (amd64 and arm64). Run it
  again to upgrade; `--uninstall` keeps the volumes unless `--purge`.
- The installer prints the address the internet sees the server at (from
  clipmux.com's CDN), then the server's own address, instead of the first
  local address, which behind NAT or on a cloud VM is private.
- The dashboard's Docker command for a PC agent names the server's own
  image tag (`:v1.0.0-rc.1`), so the agent speaks its worker API.

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

### @clipmux/uploader 0.6.0

- `create`, a function that creates the upload with your own client, joins
  `upload` and `createUrl`. Its upload is remembered like a `createUrl` one,
  and a remembered upload ClipMux has ended is replaced by a new one.
- `cancel()` resolves with the video ClipMux answered with (`errored`,
  `upload_cancelled`), or `null`. A cancel while the upload is still being
  created cancels it once it exists, instead of leaving it waiting.
- `rememberUpload(store).list()` returns the remembered uploads with their
  file's name, size and time, so a page can ask for them after a reload.
  `forget()` takes a `{ name, size, lastModified }` as well as a file.

### @clipmux/uploader 0.5.0

- A given `upload` without `upload_token` is refused at once instead of
  sending `Bearer undefined`.
- Installs next to `@types/react` 18.
- Content-Security-Policy guidance in the README.

### Server

- `GET /v1/app` lists `upload_origins`, for an app's Content-Security-Policy.
- The OpenAPI spec at `/openapi.json` describes every operation and webhook,
  groups its tags, and has an example for every JSON body.
- `clipmux worker --pair` names `clipmux admin create-pairing` as where its
  code comes from, not the removed Workers page.
- `clipmux admin create-key` defaults to the scopes `doctor` checks
  (`uploads:write`, `assets:read`, `assets:write`) and accepts spaces in
  `--scopes`.
- `clipmux listen` reads `CLIPMUX_URL` when `CLIPMUX_SERVER` is unset.
- The share page's `poster_url` carries a poster-only token.
- The ladder goes up to the source: 2160p and 1440p join 1080p and below
  (D130). Uploads, imports and re-encodes take `renditions` (e.g.
  `["1080p", "480p"]`); the dashboard's upload and re-encode dialogs,
  `clipmux push --renditions` and `npx @clipmux/node upload --renditions`
  offer it. The worker contract is version 2: update PC agents.
- Speech runs on the GPU it names (D131): the amd64 image carries whisper.cpp's
  CUDA backend for NVIDIA serverless machines, the speech probe fails a GPU
  backend whisper didn't use, and Vulkan whisper works under Landlock (it ran
  on the CPU before). `--speech cuda` is a mode.
- Faster encodes (D129): a hybrid GPU stage scales every rung on the GPU,
  audio encodes beside the video, uploads run 8 at a time, and each commit
  logs its phase timings, the GPU's name and why any GPU stage fell back.

### Server fixes

- An upload larger than about 29 GiB can complete. The complete request
  was capped at 64 KiB, about 936 parts, and the refusal said "body must be
  a JSON object". It now takes every part of the largest upload. Any
  request body over its route's limit answers `413 too_large` with the
  limit, instead of `400 invalid_request`.
- A failed video says why. The worker's message, such as ffmpeg's first
  error lines, is kept on each failed attempt (with URL query strings
  removed) and shown on the video page and the Jobs page; the management
  API's attempts carry it as `error_message`. Re-encode is offered on a
  failed video when there is a source to encode, and the page no longer
  suggests it for a file ClipMux can't read or an upload that never
  finished.
- Sources that failed to encode now do:
  - OBS and other MPEG-TS recordings whose keyframes are far apart: the
    poster step seeked to where nothing decodes and threw the finished
    encode away;
  - a video of a single frame;
  - a clip under 3 s whose audio is digital silence (loudness
    normalisation measured it as NaN; clips that short are no longer
    normalised);
  - a declared audio track with no packets (a recorder with a dead mic),
    which now gives a video-only output;
  - WebM recorded in the browser with MediaRecorder, whose header has no
    duration (it's read from the packets);
  - very tall or very wide frames (400×5000, 8192×700), which no rung fit:
    the one rendition is scaled until its long side fits 4096.
- A source whose frame size changes partway through (a stitched capture)
  kept only the part before the change, and still showed as ready. Every
  rendition now runs the whole length.
- An audio file with cover art is refused as having no video, instead of
  becoming a video a frame long.
- Concurrent upload completes or cancels no longer freeze the server: each
  held a database connection while waiting for a second, so a burst as large
  as the pool deadlocked it until a restart.
- A rotated phone video (portrait, or upside down) no longer comes out
  sideways or upside down on a full-GPU encode: a rotated source takes the
  hybrid stage, whose CPU decode applies the rotation.
- The dashboard can upload a file again after its earlier upload ended on
  the server (its video deleted, the upload expired or cancelled elsewhere).
  It kept the dead upload and failed with "the upload is no longer active"
  every time.
- The dashboard asks before a page with uploads running is left. After a
  reload it lists the stopped uploads with how much is stored; adding the
  file again resumes it, and Discard ends it.
- Cancelling an upload in the dashboard deletes its empty video instead of
  leaving it as Failed. A video whose upload was cancelled elsewhere shows as
  Cancelled.
- Deleting an asset, or one of its tracks, no longer fails with 500 when a
  track is `errored` (chapters with no LLM connected, say): a deleted track
  drops its `error_code` (found by the first live integration, Edural).

### Server security (docs/security-review.md, findings 1–6)

- The built-in worker refuses to run without Landlock, and serve and the
  workers are undumpable, so a hostile file can't read serve's secrets
  (D120). Set `CLIPMUX_UNSANDBOXED_WORKER` to run it anyway.
- Seccomp denies new namespaces to the media tools (D120).
- A device the owner signed in from skips the global sign-in limit, so
  strangers can't lock the owner out (D121).
- An upload part URL binds the part's length (D122).
- The Dashboard app takes no API keys, and its existing keys are revoked;
  an unsandboxed agent takes no URL imports (D123, migration 00035).
- Delivery serves only the files a committed output's manifest lists, and
  sweeps the rest (D124, migration 00036).
