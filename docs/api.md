# ClipMux HTTP API

Every route is under one origin, the **public URL**. Playback is served from the
**media URL** (`base_url`), which may be another origin. Requests and responses
are JSON (bodies up to 64 KiB). Errors are
`{"error": {"code": "snake_case", "message": "…", …details}}`. Every `POST`
accepts an `Idempotency-Key` (≤ 255 bytes): a retry with the same key and the
same request returns the first response; the same key with a different request
is `422 idempotency_key_reused`.

## Authentication

| Caller | Credential | Reaches |
| --- | --- | --- |
| An app's backend | `Authorization: Bearer cmx_…` (API key with scopes) | the integration API, as its own app |
| A browser uploading | `Authorization: Bearer <upload_token>` | one upload's parts, list, complete, cancel |
| The dashboard | the owner's session cookie | the management API, and the integration API as one app (`ClipMux-App: app_…`, default the Dashboard app) |
| A player | the token in the playback URL | delivery |

Scopes: `uploads:write`, `assets:read`, `assets:write`. No API key can reach the
management API. A cookie request that changes state from another site is
refused (`403 cross_origin`).

## Limits

A refused request is `429 rate_limited` with `Retry-After` (seconds) and
`retry_after` in the error details; wait that long and retry (D067). Limits
are per serve process.

| Surface | Counted by | Limit |
| --- | --- | --- |
| Integration API (API key, or the owner's session acting as an app) | key | 20 requests a second, bursts of 200; of those, writes (`POST`, `DELETE`) 2 a second, bursts of 60 |
| `GET /v1/events` | key | 4 long-polls at once |
| `/v1/uploads/{id}/*` with an upload token | upload | 10 requests a second, bursts of 50 |
| `POST /v1/session` | address, and overall | 5 failed sign-ins per address and 20 overall in 15 minutes (`too_many_attempts`). A browser holding the owner's known-device cookie (`__Host-clipmux_device`) skips the overall limit (D121) |
| `POST /v1/setup/owner` | address | 5 wrong setup tokens per address in 15 minutes (D121) |
| `POST /worker/v1/pair` | address, and overall | 10 wrong codes per address and 100 overall in 15 minutes |
| `POST /worker/v1/pairings` | address | 20 an hour (and at most 10 waiting at once, `too_many_pairings`) |
| `POST /worker/v1/pairings/poll` | address | 2 a second, bursts of 4 |
| `POST /v1/app-pairings` | address | 20 an hour (and at most 10 waiting at once, `too_many_pairings`) |
| `POST /v1/app-pairings/poll` | address | 2 a second, bursts of 4 |
| Playback (`/v/`, `/p/`) | address | only refusals count: after 300 answers of 401, 403 or 404 in 5 minutes, every playback request from that address gets 429 until they age out; successful requests are never limited |

An address is the client's as `CLIPMUX_TRUSTED_PROXIES` (Caddy) reports it;
IPv6 addresses are grouped by their /64.

## Integration API

The routes below, the delivery URLs and the webhooks are also described as
OpenAPI 3.1 at `GET /openapi.json` (no credential; `internal/openapi`,
D112). `TestOpenAPICoversTheDocumentedIntegrationRoutes` keeps this section,
App pairing, Analytics and Share links in step with it, and
`TestOpenAPIOperationsAreRouted` proves every operation reaches a handler.

| Route | Scope | Notes |
| --- | --- | --- |
| `POST /v1/uploads` | `uploads:write` | `{size, playback_policy: "signed"\|"public", passthrough, title?, generate?: {subtitles?, chapters?}, renditions?: ["1080p", …]}` → `201 {upload_id, asset_id, part_size, part_count, upload_token, playback_ids}` |
| `POST /v1/uploads/{id}/parts` | upload token | `{part_numbers: [1…]}` (≤ 100) → `{parts: [{part_number, url}], expires_at}`; PUT each part to its URL and keep its `ETag` |
| `GET /v1/uploads/{id}/parts` | upload token | `{parts: [{part_number, etag, size}]}` — what storage holds, for resuming |
| `POST /v1/uploads/{id}/complete` | upload token | `{parts: [{part_number, etag}]}` → the asset, `processing` |
| `POST /v1/uploads/{id}/cancel` | upload token or `uploads:write` | the asset becomes `errored` / `upload_cancelled` |
| `POST /v1/assets` | `assets:write` | `{agent_source_id, playback_policy?, passthrough?, title?, generate?, renditions?}` → `201` the asset, `processing`, on a file a PC agent registered for `clipmux push --local`; its jobs run only on that agent. `404 agent_source_not_found` once attached; `409 source_unavailable` if the agent was revoked. Or `{url, playback_policy?, passthrough?, title?, generate?, renditions?}` → `201` the asset, `processing`; serve fetches the public http(s) URL and transcodes it. `400 import_url_refused` for a non-public or credentialed URL; a fetch that fails ends `errored` with `import_url_refused`, `import_failed` or `import_too_large` |
| `GET /v1/app` | any scope | the key's own app: `{id, name, scopes, public_url, base_url, max_token_lifetime_seconds, upload_origins, signing_keys: [{kid, public_key_pem, created_at, retired_at?}]}`. `upload_origins` are where a browser sends upload parts (this server, and each bucket's public endpoint); with `base_url`, they are what an app's Content-Security-Policy must allow. `signing_keys` are the app's keys that still verify tokens (not revoked, not past `retired_at`), newest first; `base_url` is the media origin playback URLs start with, and a token's `exp − iat` may not exceed `max_token_lifetime_seconds`. For the SDK's `doctor` |
| `GET /v1/assets/{id}` | `assets:read` | the asset (below) |
| `PATCH /v1/assets/{id}` | `assets:write` | `{title}` → the asset; `""` or `null` clears it. A change bumps `version` and sends `video.asset.updated`; the same title changes nothing. `409 asset_deleted` |
| `GET /v1/events?since=&after=&wait=` | `assets:read` | the app's webhook events created at or after `since` (default now), oldest first, ≤ 100: `{data: [{id, created_at, body}]}`, `body` being exactly what an endpoint receives. Long-polls up to `wait` seconds (≤ 25) until one is newer than `after`, the `created_at` (RFC 3339) of the newest event the caller has. For `listen` |
| `GET /v1/assets?passthrough=` | `assets:read` | the app's assets with that reference, newest first |
| `GET /v1/assets?updated_since=&cursor=&limit=` | `assets:read` | every change since an RFC 3339 time, oldest first, deleted included, ≤ 100 per page, `next_cursor` |
| `DELETE /v1/assets/{id}` | `assets:write` | playback stops at once; `200` once files are gone, `202` while the delete job runs |
| `POST /v1/assets/{id}/reencode` | `assets:write` | `{supersede?, renditions?}` → `202 {id, kind, status}`; `409 job_active` (`job_id`), `source_missing`, `asset_not_ready`, `asset_deleted` |
| `GET /v1/assets/{id}/tracks/{track_id}` | `assets:read` | a track: `{id, kind, source, status, language?, name?, error_code?, duration_ms?, chapters?: [{start_ms, end_ms, title}]}`; chapters once a chapters track is ready |
| `GET /v1/assets/{id}/tracks/{track_id}/transcript?words=` | `assets:read` | a subtitles track's transcript, the moment it reads `ready`: `{track_id, language, duration_ms, text, segments: [{id, start_ms, end_ms, text, words?: [{id, start_ms, end_ms, text}]}]}` (words with `words=true`); `409 track_not_ready` before |
| `POST /v1/assets/{id}/tracks` | `assets:write` | `{kind: "subtitles"\|"chapters", supersede?}` → `202 {track, job}`; a new track is made and the old one keeps playing until it is ready. `409 asset_not_ready`, `no_audio`, `subtitles_not_ready` (chapters), `chapters_not_connected`, `job_active` (`job_id`; `supersede` cancels a transcription or chapters job), `asset_deleted`; idempotent with `Idempotency-Key` |
| `DELETE /v1/assets/{id}/tracks/{track_id}` | `assets:write` | the asset; `200` once no route lists the track, `202` while it is unpublished; its file and transcript go at once |
| `POST /v1/assets/{id}/playback-ids` | `assets:write` | `{policy}` → `201 {id, policy, playable, status}` |
| `DELETE /v1/assets/{id}/playback-ids/{pid}` | `assets:write` | `200 revoked` once unpublished, `202 revoking` before |

An asset:

```json
{"id": "ast_…", "app_id": "app_…", "status": "ready", "version": 4, "passthrough": "lesson-42", "title": "Lesson 42",
 "error_code": "…", "base_url": "https://media.example.net",
 "playback_ids": [{"id": "pid_…", "policy": "signed", "playable": true, "status": "active"}],
 "duration_seconds": 1830.4,
 "tracks": [{"id": "trk_…", "kind": "subtitles", "source": "generated", "status": "ready", "language": "hi", "name": "Hindi"}],
 "created_at": "…", "updated_at": "…"}
```

`generate` on an upload, an import or an agent's attach asks for subtitles
and chapters; chapters imply subtitles. Tracks are made after the video is
ready and never hold it up. A track's `status` is `preparing`, `ready` or
`errored` (`error_code`: `no_audio`, `transcribe_failed`, `transcript_invalid`,
`transcribe_unavailable`, `speech_api_rejected`, and for chapters
`subtitles_failed`, `llm_not_connected`, `llm_unavailable` (the model kept
failing for now through three tries), `model_rejected`, `model_refused`,
`invalid_model_output` (still invalid after one corrective re-prompt) and
`transcript_too_large` (a prompt over 512 KB, never truncated)); retired and deleted tracks aren't listed. A ready
subtitles track plays in the master as an HLS subtitles rendition, and its
VTT is at `{base_url}/v/{token}/{playback_id}/tracks/{track_id}.vtt` (or
`/p/…`), the sidecar URL; a chapters track's VTT is at the same path.
`duration_seconds` is null until the first output.

`title` (optional, trimmed, one line, ≤ 200 characters) is for people: not
unique and never part of a URL; `passthrough` and `title` are left out when
unset. `status` is `waiting_upload`, `processing`, `ready`, `errored` or `deleted`.
`version` rises on every change; keep the highest you have seen.

## App pairing

`npx @clipmux/node init --server URL` gets an app's credentials without
anyone copying a secret (P6-T23, D113). It mirrors agent pairing: the CLI
starts a pairing, shows the code and opens `verify_url`, and polls; the owner
checks the code matches on `/connect`, picks or creates the app and approves.
None of these routes needs a key.

| Route | Request → response |
| --- | --- |
| `POST /v1/app-pairings` | `{name?, hostname?, client?}` (≤ 100, 253 and 100 characters; `name` defaults to `hostname`) → `201 {code, poll_token, verify_url, expires_at, interval_seconds}`. The code (`KXFT-9M2Q`, any case, the dash optional) works for 10 minutes; `verify_url` is `{public_url}/connect?code=…`. `429 too_many_pairings` while 10 wait for the owner |
| `POST /v1/app-pairings/poll` | `{poll_token}` → `{status: pending \| approved \| denied \| expired, credentials?}`. Poll every `interval_seconds`. The first poll after approval carries `credentials` (below) and is the only one that ever does: they are sealed with the secrets key until then and deleted as they are returned, so a later poll is `404 pairing_not_found`. A pairing approved but not collected before it expires (approval leaves it at least a minute) drops its credentials and reads `expired` |

`credentials` here and from `POST /v1/apps/{id}/credentials` are
`{url, app_id, app_name, api_key_id, api_key, signing_kid, signing_key, webhook_endpoint_id?, webhook_secret?, env}`:
a new API key with the default scopes (named after the pairing's `name`, or
`integration`), a new signing key (`signing_key` is its PKCS #8 PEM, kept by
nobody but you), and with a webhook URL a new endpoint and its secret. `env`
is the same as a `.env` block:

```bash
# ClipMux credentials for Course site (app_…)
CLIPMUX_URL=https://video.example.com
CLIPMUX_API_KEY=cmx_…
CLIPMUX_SIGNING_KID=kid_…
CLIPMUX_SIGNING_KEY="-----BEGIN PRIVATE KEY-----\nMIG…\n-----END PRIVATE KEY-----"
CLIPMUX_WEBHOOK_SECRET=whsec_…
```

`CLIPMUX_WEBHOOK_SECRET` is there only with a webhook URL. `clipmux admin
create-integration --app NAME [--webhook-url URL] [--allow-loopback]` prints
the same block from the host.

## Webhooks

Each app's endpoints receive its assets' events: `video.asset.ready`,
`video.asset.updated`, `video.asset.errored`, `video.asset.reencode_failed`,
`video.asset.deleted`, and for tracks `video.asset.track.ready`,
`video.asset.track.errored` and `video.asset.track.deleted`, which carry
`track: {id, kind, error_code?}` beside the asset. Delivery is at least once and unordered; dedupe on
`X-Webhook-Id`. Retries back off for 24 hours; after that, catch up with
`updated_since`.

```http
POST /your/endpoint
Content-Type: application/json
X-Webhook-Event: video.asset.ready
X-Webhook-Id: evt_…
X-Webhook-Timestamp: 1700000000
X-Webhook-Signature: sha256=<hex HMAC-SHA256(secret, "1700000000." + raw body)>

{"id": "evt_…", "type": "video.asset.ready", "created_at": "…", "data": {…the asset…},
 "job": {"id": "job_…", "kind": "reencode", "error_code": "…"}}
```

Verify the signature over the raw body with the endpoint's `whsec_…` secret and
reject a timestamp more than 5 minutes off. Any 2xx is success; redirects are
not followed. A receiver in any language can check itself against
`internal/webhooks/testdata/vectors.json` (D112): the same secret, clock,
body and headers must give each case's `valid` and `reason`.

## Delivery

```text
Signed: {base_url}/v/{token}/{playback_id}/master.m3u8
Public: {base_url}/p/{playback_id}/master.m3u8
Poster: {base_url}/v/{t_token}/{playback_id}/poster.jpg
```

Tokens are ES256 JWTs signed with the app's signing key: header `kid`, claims
`sub` (playback ID), `aud` (`v` plays everything including the content key, `t`
fetches only the poster), `iat`, `exp` (at most the project maximum after `iat`,
7 days by default). `401 token_expired` means fetch a new token; `403 denied`
means stop; `404` means the ID is unknown or revoked.

## Management API (owner session)

The owner's routes, which the dashboard calls with the session cookie, are
documented at https://clipmux.com/docs/reference/management-api (the source
is `site/content/docs/reference/management-api.mdx`).
`internal/manage/docs_test.go` keeps that page complete: it fails when serve
registers an owner route the page doesn't list, or the page lists one serve
doesn't register.

## Pairing a PC agent (worker API, no key yet)

| Route | Notes |
| --- | --- |
| `POST /worker/v1/pair` | `{code, name?, hostname?, platform?}` → `201 {agent_id, key, name}` for a code from `POST /v1/agents/pairings`; `404 pairing_not_found` once used or expired |
| `POST /worker/v1/pairings` | `{name?, hostname?, platform?}` → `201 {pairing_id, code, poll_token, verify_url, expires_at, interval_seconds}`; `429 too_many_pairings` while 10 wait for the owner |
| `POST /worker/v1/pairings/poll` | `{poll_token}` → `{status: pending \| approved \| denied \| expired, credentials?}`; the key is in the first `approved` answer only |

Agent keys start with `cmxa_` and authenticate `POST /worker/v1/claim` like the
built-in worker's key. A serverless machine's one-time key starts with `cmxd_`
(D054): it claims only the job its dispatch was created for, once, before the
dispatch's claim deadline, and its claim is `409 sandbox_required` unless the
worker reports rlimits, `no_new_privs` and seccomp. A claim reports `free_disk_bytes`; a worker takes only
uploads whose size, three times over, fits its scratch disk.
| `POST /worker/v1/sources` | agent key: `{size, upload_source?}` → `201 {id, size, upload_source}`, a file that stays on the agent (`push --local`); with `upload_source`, the agent uploads it after its first encode (D059) |
| `POST /worker/v1/sources/{id}/attach` | agent key: `{playback_policy?, passthrough?}` → `201` the asset, as the owner's upload (the Dashboard app), with a transcode job pinned to the agent |
| `POST /worker/v1/sources/sync` | agent key: `{ids}` (≤ 1000) → `{gone}`: sources the agent may forget (asset deleted, unknown, or unattached for a day) |

An `upload_source` job (D059) is claimed only by the agent holding the file. Its
attempt calls `POST /worker/v1/attempts/{id}/source-parts` with `{parts: [n…]}`
(at most 100; an empty list just starts the upload) → `{part_size, part_count,
urls: {"n": url}, expires_at}`, PUTs each part, and finishes with `complete`
carrying `{source_parts: [{number, etag}]}` instead of a manifest. The server
completes the multipart upload into `uploads/{asset}/source`, checks its size
against the file's (`409 source_invalid` otherwise), and moves the asset's
source there, unpinned from the agent.

A claim for a job on a local source carries `source: {agent_source_id, size}`
instead of a URL. A job pinned to an agent waits for it, spending no retry
budget, and ends with `agent_unavailable` once the agent is revoked or unseen
for 7 days.

## Analytics (D100, D103)

The owner's session sees every app, or the one `app_id` names. An API key
reaches these routes only with the `analytics:read` scope, which a key has
only when asked for at creation, and then sees its own app.

| Route | Notes |
| --- | --- |
| `GET /v1/analytics?range=24h\|7d\|30d\|90d&app_id=` | → `{totals, previous, series, top_assets, storage, collecting}` below. `app_id` is ignored for an API key. `400 invalid_request` for any other `range` |
| `GET /v1/analytics/assets/{id}?range=` | the same for one asset, without `top_assets`; a deleted asset keeps its history (with `storage` all zero). `404 not_found` for an asset outside the project, or outside the key's app |

```json
{"totals":   {"views": 1840, "watch_seconds": 302400, "unique_viewers": 412, "bytes_delivered": 98123456789},
 "previous": {"views": 1502, "watch_seconds": 251000, "unique_viewers": null, "bytes_delivered": 80211000000},
 "series": [{"t": "2026-11-03T00:00:00Z", "views": 260, "watch_seconds": 43100, "bytes_delivered": 14000000000}],
 "top_assets": [{"asset_id": "ast_…", "title": "Lesson 42", "views": 310, "watch_seconds": 61000, "bytes_delivered": 17000000000}],
 "storage": {"source_bytes": 524288000, "media_bytes": 2147483648, "total_bytes": 2671771648},
 "collecting": {"builtin": true, "edge": "not_used"}}
```

`series` buckets are an hour for `24h` and a UTC day otherwise, oldest
first, zero-filled, with `t` the bucket's start. The last bucket is the one
now falls in, so it is partial. `previous` is the same number of whole
buckets just before, for the change against it. Unique viewers are kept per
hour for 14 days (used for exact `24h` comparisons) and per UTC day for 180
days (used for `7d`, `30d` and `90d`). `top_assets` lists at most 10 live
assets by views (and bytes when sampled views read zero); a `title` may
be absent. Totals include deleted assets' history; `storage` reports live
sources and non-purged outputs and tracks (`deleted_at IS NULL`).

What counts (D103): a `GET` answered 200 or 206; a range not from byte 0
adds bytes only. A view is one
generated `master.m3u8`. Watch time is 6 seconds per audio segment served,
which overcounts by the player's buffer-ahead. Bytes are every counted body
actually written/streamed (both built-in and Cloudflare Worker count streamed
bytes on aborted transfers).
`unique_viewers` counts distinct `vid` claims (excluding poster fetches)
and is `null` when no view in
the range carried one; sign `vid` (any stable ID for the viewer, which
ClipMux keeps only as a keyed hash) to count viewers. Tokens signed by the
server's internal key don't count (even on public playback IDs), so
previews, library posters and setup tests don't; share links (below) are the
exception.

`collecting.edge` is `not_used` without Cloudflare delivery, and
`not_connected` with it until edge analytics are connected (below): the
Worker counts, but nothing pulls its counts in. When Cloudflare delivery is
connected, `collecting.edge_needs_redeploy` indicates if the deployed Worker
should be redeployed; once `connected`, `edge_error` and
`edge_pulled_through` report the latest pull state. Serve
pulls every 5 minutes, so the edge's last few minutes show up on the next
pull, and each pull recomputes the hour before it.

## Share links (D104)

A share link plays one asset for anyone holding its URL, with no session or
app token. Each link owns one signed playback ID, created with it; that ID
shows in the asset's `playback_ids` with `share: {id, name}`. Revoking the
link revokes the ID, and the link's views are the ID's views. Dashboard
previews and library posters never use a link's ID, so revoking a link
leaves them alone. The owner's session reaches these routes as the asset's
app, as it does playback IDs.

| Route | Scope | Notes |
| --- | --- | --- |
| `POST /v1/assets/{id}/shares` | `assets:write` | `{name?, expires_at?}` → `201 {id, asset_id, name, url, playback_id, status, expires_at, created_at}`. `url` is `{public_url}/s/{secret}`. `name` is one line of up to 100 characters; `expires_at` must be in the future and can't be changed later. Takes `Idempotency-Key`. Allowed while the asset is processing; `409 asset_deleted`, `409 too_many_playback_ids` |
| `GET /v1/assets/{id}/shares` | `assets:read` | `{data: [...]}`, newest first: each link as above with `views_30d`, its views (built-in and edge) over the last 30 UTC days |
| `DELETE /v1/shares/{id}` | `assets:write` | revokes the link's playback ID: `200` once unpublished, `202` while revoking. Revoking again changes nothing |
| `POST /v1/shares/{id}/stream-url` | `assets:write` | `{url, expires_at}`: a signed `master.m3u8` URL for players that can't open the page. It can't refresh; it lasts until the link's expiry or the project's longest token lifetime, whichever is first, and stops when the link is revoked. `409 share_expired`, `409 share_revoked`, `409 asset_not_ready` |

`status` is `active`, `revoked` or `expired`. Once its expiry passes a link
reads `expired`, whatever stopped it; before that, a link whose playback ID
was revoked (or whose asset was deleted) reads `revoked`. A loop revokes
expired links' playback IDs within a minute, so their routes also leave the
Worker; the tokens already stop at the expiry.

The page and its token endpoint need no session or key:

| Route | Notes |
| --- | --- |
| `GET /s/{secret}` | the share page: a player for the link's video, static HTML whose script asks the endpoint below for tokens. Its own CSP (itself and the media URL only), `frame-ancestors 'none'`, `noindex`, no referrer, `no-store`. It keeps a random viewer ID in the browser's storage, so unique viewers count; a private window sends none |
| `POST /s/{secret}/playback` | `{viewer_id?}` → `{title?, duration_seconds, base_url, playback_id, token, expires_at, poster_url, tracks}`, where `tracks` are the ready ones. `token` is a `v` token for the link's playback ID, signed with the internal key, valid until the earliest of the link's expiry, max(2 × duration, 4 h) and the project's longest token lifetime. A `viewer_id` of 16–64 `[A-Za-z0-9_-]` becomes the token's `vid`; anything else is dropped. `404 share_not_found` (unknown, revoked, the asset deleted or its app disabled), `410 share_expired`, `409 asset_not_ready`. Successes are limited to 2 a second per address, bursts of 20 (`429 rate_limited`); refusals count toward the playback refusal limit. `no-store`, `noindex`, no referrer |

Plays through a share link count in analytics though the internal key signs
them (D103).

