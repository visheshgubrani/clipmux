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
| `POST /v1/session` | address, and overall | 5 failed sign-ins per address and 20 overall in 15 minutes (`too_many_attempts`) |
| `POST /v1/setup/owner` | address, and overall | 5 wrong setup tokens per address and 20 overall in 15 minutes |
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
| `POST /v1/uploads` | `uploads:write` | `{size, playback_policy: "signed"\|"public", passthrough, title?, generate?: {subtitles?, chapters?}}` → `201 {upload_id, asset_id, part_size, part_count, upload_token, playback_ids}` |
| `POST /v1/uploads/{id}/parts` | upload token | `{part_numbers: [1…]}` (≤ 100) → `{parts: [{part_number, url}], expires_at}`; PUT each part to its URL and keep its `ETag` |
| `GET /v1/uploads/{id}/parts` | upload token | `{parts: [{part_number, etag, size}]}` — what storage holds, for resuming |
| `POST /v1/uploads/{id}/complete` | upload token | `{parts: [{part_number, etag}]}` → the asset, `processing` |
| `POST /v1/uploads/{id}/cancel` | upload token or `uploads:write` | the asset becomes `errored` / `upload_cancelled` |
| `POST /v1/assets` | `assets:write` | `{agent_source_id, playback_policy?, passthrough?, title?, generate?}` → `201` the asset, `processing`, on a file a PC agent registered for `clipmux push --local`; its jobs run only on that agent. `404 agent_source_not_found` once attached; `409 source_unavailable` if the agent was revoked. Or `{url, playback_policy?, passthrough?, title?}` → `201` the asset, `processing`; serve fetches the public http(s) URL and transcodes it. `400 import_url_refused` for a non-public or credentialed URL; a fetch that fails ends `errored` with `import_url_refused`, `import_failed` or `import_too_large` |
| `GET /v1/app` | any scope | the key's own app: `{id, name, scopes, public_url, base_url, max_token_lifetime_seconds, signing_keys: [{kid, public_key_pem, created_at, retired_at?}]}`. `signing_keys` are the app's keys that still verify tokens (not revoked, not past `retired_at`), newest first; `base_url` is the media origin playback URLs start with, and a token's `exp − iat` may not exceed `max_token_lifetime_seconds`. For the SDK's `doctor` |
| `GET /v1/assets/{id}` | `assets:read` | the asset (below) |
| `PATCH /v1/assets/{id}` | `assets:write` | `{title}` → the asset; `""` or `null` clears it. A change bumps `version` and sends `video.asset.updated`; the same title changes nothing. `409 asset_deleted` |
| `GET /v1/events?since=&after=&wait=` | `assets:read` | the app's webhook events created at or after `since` (default now), oldest first, ≤ 100: `{data: [{id, created_at, body}]}`, `body` being exactly what an endpoint receives. Long-polls up to `wait` seconds (≤ 25) until one is newer than `after`. For `clipmux listen` |
| `GET /v1/assets?passthrough=` | `assets:read` | the app's assets with that reference, newest first |
| `GET /v1/assets?updated_since=&cursor=&limit=` | `assets:read` | every change since an RFC 3339 time, oldest first, deleted included, ≤ 100 per page, `next_cursor` |
| `DELETE /v1/assets/{id}` | `assets:write` | playback stops at once; `200` once files are gone, `202` while the delete job runs |
| `POST /v1/assets/{id}/reencode` | `assets:write` | `{supersede?}` → `202 {id, kind, status}`; `409 job_active` (`job_id`), `source_missing`, `asset_not_ready`, `asset_deleted` |
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

| Route | Notes |
| --- | --- |
| `GET /v1/setup` | no session needed: `{owner_exists, urls: {public_url, media_url, confirmed, pinned}, keys_allowed}` |
| `POST /v1/setup/owner` | `{setup_token, email, password}` → `201`, signed in; `409 registration_closed` once an owner exists |
| `POST /v1/session` · `GET /v1/session` · `DELETE /v1/session` | sign in (`429` with `Retry-After` after repeated failures), who am I (`{email, totp_enabled}`), sign out. With two-factor sign-in on, signing in also takes `code` (from the authenticator) or `recovery_code`; without one it is `401 totp_required`, and a wrong or reused one `401 invalid_code`, counted as a failed sign-in (D071) |
| `PUT /v1/session/password` | `{current_password, new_password}` → `204`; the new one is 12 to 1024 characters (`400 weak_password`), a wrong current one is `401 invalid_credentials` and counts as a failed sign-in (`429` after five). Every other session ends; this one stays signed in |
| `POST /v1/session/totp` | `{password}` → `201 {secret, otpauth_url}`: a new authenticator secret, waiting for a code |
| `POST /v1/session/totp/enable` | `{code}` → `{enabled: true, recovery_codes: [10]}`, shown once; ends every other session |
| `POST /v1/session/totp/disable` | `{password}` → `{enabled: false}`. A locked-out owner runs `clipmux admin reset-totp` on the host |
| `POST /v1/content-keys/rotate` | `{cloudflare_token?}` → `{version, usable, current, steps}`: a new content key secret version for new outputs. With Cloudflare delivery the token is required (`409 cloudflare_token_required`): the Worker is redeployed with the new version before any output uses it; if that fails, `502` with the steps and the version stays unused (D071) |
| `PUT /v1/settings/urls` | `{public_url, media_url?}`; `409 pinned` when the environment sets them |
| `GET /v1/settings/routing` | `{primary_executor, fallback_executor, queue_age_fallback_minutes, builtin_worker: {last_seen_at, online}, providers_ready}` (D053); `providers_ready` lists the providers connected and verified by a setup test |
| `PUT /v1/settings/routing` | `{primary_executor: workers \| agents \| builtin, fallback_executor?: builtin, queue_age_fallback_minutes?: 1–1440}`; a fallback can't be part of the primary, and queue age needs a fallback (`400 invalid_request`); a provider (`modal`, `runpod`) is `409 executor_not_connected` until it is connected and its setup test passed. A provider's passing connect test sets routing itself: that provider as primary, `builtin` as fallback when serve runs the built-in worker (D116) |
| `GET /v1/settings/speech` | `{mode: local \| api \| local_then_api, connection: {base_url, model, has_key, verified_at} \| null}`: where transcriptions run (D075). `local` is whisper.cpp on the project's executors; `api` sends every transcription to the connected speech API; `local_then_api` uses it when no worker that can transcribe is online, or after the queue-age setting. The key is never returned |
| `PUT /v1/settings/speech` | `{mode}`; `api` and `local_then_api` are `409 speech_not_connected` until a speech API is connected; queued transcriptions move to match |
| `POST /v1/settings/speech/connect` | `{base_url, model, api_key?}`, any OpenAI-compatible `/audio/transcriptions` with `verbose_json` timestamps (OpenAI `whisper-1`, Groq `whisper-large-v3-turbo`, or a server on your network such as Speaches; the key may be empty for one that needs none) → `{connected, steps}`: `credentials` (`GET {base_url}/models`), `transcription` (11 s of sample speech sent exactly as transcriptions send audio), `saved`. **Audio leaves your infrastructure** for this provider once a mode uses it |
| `DELETE /v1/settings/speech/connect` | Forgets the connection and returns the mode to `local`; `404` when none is connected |
| `GET /v1/settings/chapters` | `{connection: {provider, base_url?, model, response_format?, has_key, verified_at} \| null}`: the LLM that makes chapters. The key is never returned |
| `POST /v1/settings/chapters/connect` | `{provider: "openai_compatible" \| "anthropic", base_url?, model?, api_key?}` → `{connected, steps}`. `openai_compatible` is any `/chat/completions` API (OpenAI, Ollama, vLLM, Groq, OpenRouter) and needs `base_url` and `model`; its key may be empty for a server that needs none. `anthropic` is Claude through Anthropic's SDK and needs `api_key`; `model` defaults to `claude-opus-5-5`. Steps: `credentials` (`GET {base_url}/models`, or reading the Claude model), `structured_output` (chapters of a fixed three-segment transcript; an OpenAI-compatible server without strict JSON schemas falls back to JSON mode), `saved`. **Transcript text leaves your infrastructure** for this provider whenever chapters are made |
| `DELETE /v1/settings/chapters/connect` | Forgets the connection; chapters already queued end `llm_not_connected`, ready ones keep playing. `404` when none is connected |
| `GET /v1/executors` | `{data: [{kind, connected, verified, verified_at, config, max_concurrent, monthly_limit_cents, rate_cents_per_hour, disk_bytes, month_cents, machines, backoff_until}]}`: every provider ever connected, with this month's estimated spend (D055, D056) |
| `POST /v1/executors/runpod/connect` | `{api_key, image?, registry_username?, registry_password?, gpu_type_ids?, disk_gb?, max_concurrent?, monthly_limit_cents?, rate_cents_per_hour?}` (a limit left out is $50; a rate left out is the dearest chosen GPU's catalog price, D111) → `{connected, steps: [{name, ok, detail}]}`; steps `credentials`, `public_url`, `image` (resolved to its digest with the registry credentials), `registry`, `saved`, `setup_test`, stopping at the first failure; connecting again rotates the key (docs/executors.md) |
| `POST /v1/executors/modal/connect` | `{token_id, token_secret, environment?, image?, registry_username?, registry_password?, gpu?, cpu?, memory_mib?, disk_gb?, max_concurrent?, monthly_limit_cents?, rate_cents_per_hour?}` (`gpu` is a list such as `L4,T4`; defaults as for RunPod, with Modal's prices) → `{connected, steps}`; steps `credentials`, `public_url`, `image`, `saved`, `setup_test` (D057) |
| `POST /v1/executors/{kind}/authorize` | browser approval instead of a pasted key (D109): the `connect` body without `api_key` or `token_*` → `201 {id, url, code?, expires_at, status: "pending"}`. The settings are checked first (`400 invalid_request`). Open `url`, the provider's own page (Modal shows `code`). At most three wait per project, each for 10 minutes: `429 too_many_approvals`; `502 approval_unavailable` when the provider won't start one (paste a key instead) |
| `GET /v1/executors/{kind}/authorize/{id}` | polls the approval once (Modal's poll waits up to 10 s) → `{id, expires_at, status: pending \| approved \| denied \| expired \| shown, result?}`. `shown` means the provider put the key on its own page for you to paste instead (RunPod has done this). Once approved it runs that provider's `connect` with the credentials it issued, which never come back, and `result` is its `{connected, steps}`. One poll runs at a time, and a settled approval answers the same to every poll; `404` once expired |
| `POST /v1/executors/runpod/gpus` | `{api_key}` → `{data: [{id, name, memory_gb, cents_per_hour, recommended?}]}`: RunPod's Secure Cloud GPUs whose NVENC the worker can use, from RunPod's catalog, recommended first, then by price. The key isn't kept; `400 key_rejected` (D111) |
| `GET /v1/executors/modal/gpus` | the same shape for Modal: its NVENC GPUs at Modal's published prices, including a default sandbox's CPU and memory (D111) |
| `DELETE /v1/executors/{kind}` | `204`; routing that named it moves back to `builtin` (as primary) or no fallback (D116); `409 machines_running` while one of its machines may run |
| `GET /v1/apps` · `POST /v1/apps` | `{name}` |
| `POST /v1/apps/{id}/disable` · `…/enable` | a disabled app's keys, upload tokens and playback stop; the Dashboard app can't be disabled |
| `GET`/`POST /v1/apps/{id}/api-keys` · `DELETE /v1/api-keys/{id}` | create `{name?, scopes?, local?}` → `201 {id, name, scopes, created_at, key}`; `name` ≤ 100 characters, `default` when blank; `scopes` default to `uploads:write`, `assets:read`, `assets:write`; `analytics:read` is only given when asked for. List `{data: [{id, name, scopes, created_at, last_used_at?, revoked_at?}]}`. The secret (`key`) is in the create response only; `409 public_url_required` while the public URL is loopback, unless `local: true` confirms the key is for apps on this machine (D108) |
| `POST /v1/apps/{id}/credentials` | `{webhook_url?, local?}` → `201` the app's credentials (see App pairing), made in one transaction and shown once: an API key, a signing key, and with `webhook_url` an endpoint. `409 public_url_required` on a loopback public URL unless `local: true` (D108); `400 invalid_request` for a refused webhook URL, which creates nothing |
| `GET /v1/app-pairings/{code}` | an app pairing as `/connect` shows it: `{status: pending \| approved \| denied \| consumed \| expired, name, hostname, client, ip?, app_id?, expires_at}`; `404 pairing_not_found` |
| `POST /v1/app-pairings/{code}/approve` | `{app_id \| new_app_name, webhook_url?, local?}` → the pairing, `approved`, with its credentials sealed for the CLI's next poll. Exactly one of `app_id` and `new_app_name` (`400 invalid_request`); `404 not_found` for an unknown app, `409 name_taken`, `409 public_url_required` as above. A pairing already decided or expired is returned as it is |
| `POST /v1/app-pairings/{code}/deny` | → the pairing, `denied`; its CLI stops |
| `GET`/`POST /v1/apps/{id}/signing-keys` · `DELETE /v1/signing-keys/{kid}` | `private_key_pem` in the create response only |
| `GET`/`POST /v1/apps/{id}/webhook-endpoints` · `DELETE /v1/webhook-endpoints/{id}` | `secret` in the create response only |
| `POST /v1/webhook-endpoints/{id}/secret` | `201 {secret, active: false}`: a new secret, shown once; deliveries stay signed with the old one until it is activated (D069) |
| `POST /v1/webhook-endpoints/{id}/secret/activate` | `{active: true}`: deliveries are signed with the new secret from the next try; `409 no_pending_secret` |
| `GET /v1/webhook-endpoints/{id}/deliveries` | the latest 100: `{data: [{id, event_id, event_type, status, attempts, last_status, last_error, created_at, finished_at, next_attempt_at}]}`. Endpoints list `consecutive_failures` and `skipped_until`: after 5 failed tries in a row an endpoint is skipped for a minute, doubling to an hour, without spending its deliveries' tries |
| `POST /v1/webhook-deliveries/{id}/redeliver` | `202`: a delivery that gave up (`failed`) is retried for a fresh 24 hours; `409 not_redeliverable` otherwise |
| `GET /v1/library?app_id=&status=&q=&include_deleted=&cursor=&limit=` | every app's assets, newest first: `{data: [asset + poster_url], next_cursor?}`. Deleted assets only with `include_deleted=true` or `status=deleted`. `q` (≤ 255 bytes) is an exact asset ID, or text a title or passthrough contains, case-insensitive (`%` and `_` match themselves). `poster_url` is `{base_url}/p/{pid}/poster.jpg` for a public playback ID, else `{base_url}/v/{t_token}/{pid}/poster.jpg` with a one-hour `t` token signed by the internal key; `null` until a playback ID is playable |
| `GET /v1/library/counts?app_id=` | `{ready, processing, errored, waiting_upload}` |
| `POST /v1/assets/{id}/preview-token` | a one-hour `v` token signed with the server's internal key: `{playback_id, token, url, expires_at}` |
| `GET /v1/cloudflare` | `{connected, hostname, bucket, account_id, worker_version, current_version, needs_redeploy, storage_ok, token_url, r2_token_url, analytics_token_url}`; the `*_url`s open Cloudflare's create-token form with the permissions Connect, R2 storage and edge analytics need already chosen (D109) |
| `POST /v1/cloudflare/inspect` | `{token, account_id?}` → `{accounts: [{id, name}], account_id?, zones?, buckets?, workers_paid, unreadable?}`: what the token reaches, so nothing is typed by hand. With several accounts and none named, only `accounts` is filled. `unreadable` names what it can't list (`zones`, `buckets`, `billing`). Changes nothing; `400 token_rejected` |
| `POST /v1/cloudflare/connect` | `{token, hostname, bucket?, account_id?, default_storage?}` → `{connected, steps: [{name, ok, detail}]}`; runs Connect Cloudflare, stopping at the first failed step (each is safe to repeat). The bucket is created if missing. `default_storage` says whether new uploads go to it; left out, a first connect makes it the default and a reconnect leaves the choice alone (D110). The token is used for this request only |
| `DELETE /v1/cloudflare` | back to built-in delivery → `{disconnected, steps: [{name, ok, detail}]}`, steps `media_url` (back to the public URL), `delivery`, `routes` (the Worker's records and keys leave the bucket, so it refuses every ID), `forgotten`, `setup_test`, stopping at the first failure (each is safe to repeat). The R2 store stays a storage config, and the default if it was; the Worker and its domain stay in your Cloudflare account. `404 not_connected`; `409 pinned` while `CLIPMUX_MEDIA_URL` names the Worker (D099) |
| `GET /v1/cloudflare/analytics` | `{connected, account_id?, pulled_through?, last_pull_at?, last_error?}`: edge analytics (D103). `pulled_through` is how far serve has pulled the Worker's counts; a first connect starts 13 days back and catches up a day per pull |
| `POST /v1/cloudflare/analytics` | `{token}` → the status above. A second Cloudflare token with only **Account Analytics: Read**, checked with one query and kept sealed; a new token keeps `pulled_through`. `409 not_connected` without Cloudflare delivery, `400 token_rejected` |
| `DELETE /v1/cloudflare/analytics` | `204`; the token is forgotten and pulled counts stay. `404 not_connected` |
| `GET /v1/storage` | `{data: [{id, kind: fs \| r2 \| s3, bucket?, endpoint?, public_endpoint?, region?, default, asset_count, cloudflare_delivery, created_at}]}`; `asset_count` leaves deleted assets out. Credentials are never returned |
| `POST /v1/storage` | `{endpoint, bucket, access_key_id, secret_access_key, region?, public_endpoint?}` → `201` the config, `kind: "s3"`, not the default. Saved only after a test write, read, list and delete pass and CORS is set: `400 storage_check_failed` with the store's reason otherwise. `public_endpoint` is where browsers PUT upload parts, when it differs |
| `POST /v1/storage/r2` | `{token, bucket?, account_id?, default?}` → `{connected, steps}`, steps `token`, `account`, `bucket` (created if missing; `clipmux-media` by default), `storage` (keys derived from the token, checked like `POST /v1/storage`), and `default` when asked. Delivery doesn't change; adding the same bucket again renews its keys, and Connect Cloudflare reuses it. The token needs only Workers R2 Storage: Edit (D110) |
| `PUT /v1/storage/{id}/credentials` | `{access_key_id, secret_access_key}` → `204` once the new keys pass the same checks; the old ones stay until then (`400 storage_check_failed`). `409 builtin_storage` for the filesystem |
| `POST /v1/storage/{id}/default` | new uploads go there, this server's disk included → the `GET /v1/storage` list; existing assets stay until moved |
| `DELETE /v1/storage/{id}` | `204`; the config is no longer listed and its credentials are dropped. `409 builtin_storage` (the filesystem), `storage_default`, `cloudflare_delivery` (disconnect first), `storage_in_use` with `asset_count` and `active_moves` (an asset counts until its files are gone; move the library first) |
| `GET /v1/storage/moves` | `{default_storage_id, default_kind, not_on_default, active, moved, failed}`: assets not on the default store still play through this server |
| `POST /v1/storage/moves` | queues a `move` job to the default store for every live asset elsewhere with no active job → `202 {queued, target_storage_id}` |
| `GET /v1/jobs?status=&asset_id=&agent_id=&cursor=&limit=` | jobs across the library, newest first: `{data: [{id, asset_id, app_id, kind, status, phase, progress, pinned_agent_id, lost_count, failed_count, error_code, routed_to, route_reason, attempts: [{number, executor, route_reason, agent_id, agent_name, status, error_code, phase, progress, encoder: {stage, fallbacks}}]}], next_cursor}`. `routed_to` is where a media job is routed (`workers`, `agents`, `builtin`, `modal`, `runpod`) and `route_reason` why: `pinned`, `primary`, `primary_offline`, `queue_age`, `no_fallback`, `fallback_refused` or `fallback_offline` (D053); each attempt keeps the reason it ran where it did, which is `primary` when the primary claims a job that was waiting for it (D062) |
| `GET /v1/jobs/{id}` | one job with its attempts |
| `POST /v1/agents/{id}/reencode` | queues a re-encode of every ready asset whose current output the agent encoded → `202 {queued, skipped: {asset_id: code}}` |
| `POST /v1/agents/pairings` | `{name?}` → `201 {code, expires_at, server, command, docker_command}`: a one-time code the owner has already approved, valid for 15 minutes |
| `GET /v1/agents/pairings/{code}` | `{status, agent_started, name, hostname, platform, ip, agent_id, expires_at}`; `status` is `pending`, `approved`, `denied`, `consumed` (an agent holds its key) or `expired` |
| `POST /v1/agents/pairings/{code}/approve` · `…/deny` | decides a code that `clipmux worker --server URL` is showing on a PC |
| `GET /v1/agents` | `{data: [{id, name, status, scope, online, sandboxed, hostname, platform, last_ip, last_seen_at, capabilities}]}` |
| `PATCH /v1/agents/{id}` | `{name?, scope?: "all" \| "local"}` |
| `DELETE /v1/agents/{id}` | revokes: the key stops working and the agent's running attempts are fenced and requeued → `{agent, fenced_attempts, current_outputs}` |

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
| `GET /v1/analytics?range=24h\|7d\|30d\|90d&app_id=` | → `{totals, previous, series, top_assets, collecting}` below. `app_id` is ignored for an API key. `400 invalid_request` for any other `range` |
| `GET /v1/analytics/assets/{id}?range=` | the same for one asset, without `top_assets`; a deleted asset keeps its history. `404 not_found` for an asset outside the project, or outside the key's app |

```json
{"totals":   {"views": 1840, "watch_seconds": 302400, "unique_viewers": 412, "bytes_delivered": 98123456789},
 "previous": {"views": 1502, "watch_seconds": 251000, "unique_viewers": null, "bytes_delivered": 80211000000},
 "series": [{"t": "2026-11-03T00:00:00Z", "views": 260, "watch_seconds": 43100, "bytes_delivered": 14000000000}],
 "top_assets": [{"asset_id": "ast_…", "title": "Lesson 42", "views": 310, "watch_seconds": 61000, "bytes_delivered": 17000000000}],
 "collecting": {"builtin": true, "edge": "not_used"}}
```

`series` buckets are an hour for `24h` and a UTC day otherwise, oldest
first, zero-filled, with `t` the bucket's start. The last bucket is the one
now falls in, so it is partial. `previous` is the same number of whole
buckets just before, for the change against it. Unique viewers are kept per
UTC day, so they count the days a range touches: for `24h`, yesterday and
today. `top_assets` lists at most 10 live assets, by views; a `title` may
be absent. Totals include deleted assets' history.

What counts (D103): a `GET` answered 200 or 206; a range not from byte 0
adds bytes only. A view is one
generated `master.m3u8`. Watch time is 6 seconds per audio segment served,
which overcounts by the player's buffer-ahead. Bytes are every counted body.
`unique_viewers` counts distinct `vid` claims and is `null` when no view in
the range carried one; sign `vid` (any stable ID for the viewer, which
ClipMux keeps only as a keyed hash) to count viewers. Tokens signed by the
server's internal key don't count, so previews and setup tests don't; share
links (below) are the exception.

`collecting.edge` is `not_used` without Cloudflare delivery, and
`not_connected` with it until edge analytics are connected (below): the
Worker counts, but nothing pulls its counts in. Once `connected`, serve
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

