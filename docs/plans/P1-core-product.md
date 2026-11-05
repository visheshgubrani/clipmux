# Phase 1 — Core product

Source: [architecture.md](../architecture.md), "Build order". Phase 1 ships a
complete app integration with no Cloudflare: Asset, Job, Attempt and Output with
a leased Postgres queue; multipart upload; filesystem storage; first-run setup;
sandboxed encode and encrypt; signed delivery; webhooks.

**Exit test:** an app plays a signed, encrypted video in Video.js, hls.js, native Safari
and ExoPlayer (with a separate audio rendition), and killing a worker mid-job
recovers the job.

## Milestone M1 — walking skeleton (D004)

Minimal T01–T08 reach a scripted end-to-end test:

1. An API key creates an upload; the test PUTs the parts and completes it.
2. The built-in worker (child process over loopback, worker API only) encodes the
   ladder and packages AES-128 CMAF HLS.
3. Commit checks the output and moves `current_output_id`; `GET /v1/assets/{id}`
   reports `ready`.
4. Playwright (headless Chromium, hls.js) plays the signed URL; a request with no
   token is refused and a `t` token cannot fetch a segment or the key.

Deferred until after M1 (still required for the exit test): upload sweepers,
Idempotency-Key, cancel/list-parts, full sandbox hardening (M1 uses an empty
environment plus the demuxer allowlist), webhooks, first boot, dashboard.

## Tasks

Each task is a vertical slice with its own tests. Port behaviour and tests from
the sources listed; do not copy code paths that assume the old data model.
`clipmux-lms` paths are under `~/clipmux-lms`, `vod` paths under `~/vod`
(archived), and the old plans under tag `archive/open-source-p17` in clipmux-lms.

| Task | Scope | Port from |
| --- | --- | --- |
| T01 | Skeleton, config, CI, dev Postgres, toolchain pins | **done** (bootstrap) |
| T02 | **done** — Schema + goose migrations + sqlc: project (default, hidden), app, api_key (hashed, scopes), signing_key, asset (5 states, `version`, `passthrough`), output, job (partial unique index: one active mutating job per asset), attempt, playback_id, upload, outbox (webhook + route_sync). Real-Postgres tests. | — |
| T03 | **done** — Queue: `POST /worker/v1/claim` (long-poll, `FOR UPDATE SKIP LOCKED`, creates attempt + lease + fresh output), heartbeat (15 s / 90 s lease, can say stop), liveness rule (latest attempt of a non-cancelled job), lost-lease sweep, retry budgets 5 lost / 3 failed, `fail` transient vs permanent. Test: kill a worker → job recovers on a new attempt and output. | risk tables in `archive/open-source-p17:docs/plans/P18-*.md`, `P20-*.md`, `P34-*.md` |
| T04 | **done** — `Storage` fs adapter: multipart with parts written at their offset, app-signed URLs served by `serve`, `List`/`Head`/`DeletePrefix`, path safety. | invariants in clipmux-lms `server/src/utils/upload-sessions.ts` |
| T05 | **done** — Integration API uploads: `POST /v1/uploads` (size, policy, passthrough), `upload_token`, `POST …/parts` signing only within declared size (32 MiB parts), `POST …/complete` with Head size check → `processing` + job. Later: `GET …/parts`, `cancel`, Idempotency-Key, 24 h inactivity sweeper. `GET /v1/assets?passthrough=` for LMS recovery. | clipmux-lms `329fe42` (`correlation_ref` lookup); open-source branch `server/src/contracts/uploads.ts` + `upload-flows.spec.ts` characterization cases |
| T06 | **done** — Built-in worker: `serve` starts a child `clipmux worker` over loopback with only an attempt token; downloads source; ffmpeg ladder (1080/720/480/360 capped at source, 6 s aligned GOPs, libx264 default) → Shaka AES-128 CMAF with a separate audio rendition; output path grammar on sign and in the worker filter; sandbox (no network, empty env, rlimits, demuxer allowlist excluding HLS/concat). Golden tests on CPU fixtures. | clipmux-lms `transcoding/video/transcode.py`, `transcoding/config.py` (ladder, GOP, level by height, HDR tone map, loudnorm); `transcoding/hls/shaka.py`, `manifests.py`, `mp4.py` (#1616); `transcoding/tests/test_packaging.py` (decrypt with openssl, A/V sync, missing refs) |
| T07 | **done** — Commit: manifest vs `List`, parse playlists (relative URIs only, every media playlist `METHOD=AES-128` with the expected key URI), one transaction moves the pointer + retires previous + writes route_sync rows; publisher confirms routes; second transaction → `ready` + `video.asset.ready` outbox row. Late completion only for the live attempt. | — |
| T08 | **done** — Built-in delivery: `/v/{token}/{playback_id}/…` and `/p/{playback_id}/…`; route lookup (default deny); ES256 tokens (`sub`, `aud` v/t, `iat`, `exp`, project max lifetime, `kid` per app); generated `master.m3u8` with `{output_id}/` prefixes; key endpoint (HMAC-SHA256(content secret, asset_id‖output_id)[:16], `no-store`); extension allowlist, `nosniff`, Content-Type from extension, ranges; 401 `token_expired` / 403 `denied` / 404. Emit JSON test vectors for the Phase 2 Worker. | clipmux-lms `delivery/src/resource-tokens.ts` + `delivery/test/{playback,security}.spec.ts` case tables; vod `delivery/src/index.ts` (`resolveRange`) |
| **M1** | **Walking skeleton e2e (above) — done:** `TestM1SignedEncryptedPlaybackOverHTTP` (ffmpeg as the HLS client, in CI) and a manual hls.js check in headless Chromium across origins with a Node-signed token | |
| T09 | Webhooks: outbox publisher, HMAC-SHA256 over `${timestamp}.${body}` (`X-Webhook-Signature: sha256=…`, `X-Webhook-Timestamp`, the scheme the LMS already verifies), at-least-once with `event.id`, backoff for 24 h; events `video.asset.{ready,updated,errored,deleted,reencode_failed}` carrying `version`. | clipmux-lms `server/src/utils/webhook-delivery.ts` |
| T10 | First boot + packaging: setup token → owner account (registration then closes), public URL + media hostname step (no API key until set), content key secret generation, secrets encrypted with an external key, sample clip through the pipeline; minimal dashboard (upload, library, playback, apps/keys); Dockerfile + compose (app + Postgres + Caddy). | vod `docs-site` structure for later docs; vod `setup/` wizard pattern (reference) |
| T11 | `@clipmux/uploader` (32 MiB parts, resume via list-parts, ETag) and `@clipmux/player` on Video.js (D014): a token-refresh plugin that, on a 401, calls `getToken()` and swaps `/v/{old}/` for `/v/{new}/` in later requests, resuming at the same position. Check Video.js 10's status first and pick v8 or v10. | clipmux-lms `sdk/src/uploader.ts`; vod `player/src/tokenRefresh*.ts`, clipmux-lms `player/src/renewal.ts` |
| T12 | Exit test: Video.js (the shipped player), hls.js, Safari, ExoPlayer with encrypted segments and separate audio; kill-a-worker recovery; setup test (valid `v` plays + fetches key, `t` cannot, no token refused, stored segment is not plain MP4). | — |
