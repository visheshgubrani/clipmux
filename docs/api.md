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
| Playback (`/v/`, `/p/`) | address | only refusals count: after 300 answers of 401, 403 or 404 in 5 minutes, every playback request from that address gets 429 until they age out; successful requests are never limited |

An address is the client's as `CLIPMUX_TRUSTED_PROXIES` (Caddy) reports it;
IPv6 addresses are grouped by their /64.

## Integration API

| Route | Scope | Notes |
| --- | --- | --- |
| `POST /v1/uploads` | `uploads:write` | `{size, playback_policy: "signed"\|"public", passthrough}` → `201 {upload_id, asset_id, part_size, part_count, upload_token, playback_ids}` |
| `POST /v1/uploads/{id}/parts` | upload token | `{part_numbers: [1…]}` (≤ 100) → `{parts: [{part_number, url}], expires_at}`; PUT each part to its URL and keep its `ETag` |
| `GET /v1/uploads/{id}/parts` | upload token | `{parts: [{part_number, etag, size}]}` — what storage holds, for resuming |
| `POST /v1/uploads/{id}/complete` | upload token | `{parts: [{part_number, etag}]}` → the asset, `processing` |
| `POST /v1/uploads/{id}/cancel` | upload token or `uploads:write` | the asset becomes `errored` / `upload_cancelled` |
| `POST /v1/assets` | `assets:write` | `{agent_source_id, playback_policy?, passthrough?}` → `201` the asset, `processing`, on a file a PC agent registered for `clipmux push --local`; its jobs run only on that agent. `404 agent_source_not_found` once attached; `409 source_unavailable` if the agent was revoked. Or `{url, playback_policy?, passthrough?}` → `201` the asset, `processing`; serve fetches the public http(s) URL and transcodes it. `400 import_url_refused` for a non-public or credentialed URL; a fetch that fails ends `errored` with `import_url_refused`, `import_failed` or `import_too_large` |
| `GET /v1/assets/{id}` | `assets:read` | the asset (below) |
| `GET /v1/events?since=&after=&wait=` | `assets:read` | the app's webhook events created at or after `since` (default now), oldest first, ≤ 100: `{data: [{id, created_at, body}]}`, `body` being exactly what an endpoint receives. Long-polls up to `wait` seconds (≤ 25) until one is newer than `after`. For `clipmux listen` |
| `GET /v1/assets?passthrough=` | `assets:read` | the app's assets with that reference, newest first |
| `GET /v1/assets?updated_since=&cursor=&limit=` | `assets:read` | every change since an RFC 3339 time, oldest first, deleted included, ≤ 100 per page, `next_cursor` |
| `DELETE /v1/assets/{id}` | `assets:write` | playback stops at once; `200` once files are gone, `202` while the delete job runs |
| `POST /v1/assets/{id}/reencode` | `assets:write` | `{supersede?}` → `202 {id, kind, status}`; `409 job_active` (`job_id`), `source_missing`, `asset_not_ready`, `asset_deleted` |
| `POST /v1/assets/{id}/playback-ids` | `assets:write` | `{policy}` → `201 {id, policy, playable, status}` |
| `DELETE /v1/assets/{id}/playback-ids/{pid}` | `assets:write` | `200 revoked` once unpublished, `202 revoking` before |

An asset:

```json
{"id": "ast_…", "app_id": "app_…", "status": "ready", "version": 4, "passthrough": "lesson-42",
 "error_code": "…", "base_url": "https://media.example.net",
 "playback_ids": [{"id": "pid_…", "policy": "signed", "playable": true, "status": "active"}],
 "created_at": "…", "updated_at": "…"}
```

`status` is `waiting_upload`, `processing`, `ready`, `errored` or `deleted`.
`version` rises on every change; keep the highest you have seen.

## Webhooks

Each app's endpoints receive its assets' events: `video.asset.ready`,
`video.asset.updated`, `video.asset.errored`, `video.asset.reencode_failed`,
`video.asset.deleted`. Delivery is at least once and unordered; dedupe on
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
not followed.

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
| `POST /v1/session/totp` | `{password}` → `201 {secret, otpauth_url}`: a new authenticator secret, waiting for a code |
| `POST /v1/session/totp/enable` | `{code}` → `{enabled: true, recovery_codes: [10]}`, shown once; ends every other session |
| `POST /v1/session/totp/disable` | `{password}` → `{enabled: false}`. A locked-out owner runs `clipmux admin reset-totp` on the host |
| `POST /v1/content-keys/rotate` | `{cloudflare_token?}` → `{version, usable, current, steps}`: a new content key secret version for new outputs. With Cloudflare delivery the token is required (`409 cloudflare_token_required`): the Worker is redeployed with the new version before any output uses it; if that fails, `502` with the steps and the version stays unused (D071) |
| `PUT /v1/settings/urls` | `{public_url, media_url?}`; `409 pinned` when the environment sets them |
| `GET /v1/settings/routing` | `{primary_executor, fallback_executor, queue_age_fallback_minutes, builtin_worker: {last_seen_at, online}, providers_ready}` (D053); `providers_ready` lists the providers connected and verified by a setup test |
| `PUT /v1/settings/routing` | `{primary_executor: workers \| agents \| builtin, fallback_executor?: builtin, queue_age_fallback_minutes?: 1–1440}`; a fallback can't be part of the primary, and queue age needs a fallback (`400 invalid_request`); a provider (`modal`, `runpod`) is `409 executor_not_connected` until it is connected and its setup test passed |
| `GET /v1/executors` | `{data: [{kind, connected, verified, verified_at, config, max_concurrent, monthly_limit_cents, rate_cents_per_hour, disk_bytes, month_cents, machines, backoff_until}]}`: every provider ever connected, with this month's estimated spend (D055, D056) |
| `POST /v1/executors/runpod/connect` | `{api_key, image?, registry_username?, registry_password?, gpu_type_ids?, disk_gb?, max_concurrent?, monthly_limit_cents, rate_cents_per_hour}` → `{connected, steps: [{name, ok, detail}]}`; steps `credentials`, `public_url`, `image` (resolved to its digest with the registry credentials), `registry`, `saved`, `setup_test`, stopping at the first failure; connecting again rotates the key (docs/executors.md) |
| `POST /v1/executors/modal/connect` | `{token_id, token_secret, environment?, image?, registry_username?, registry_password?, gpu?, cpu?, memory_mib?, disk_gb?, max_concurrent?, monthly_limit_cents, rate_cents_per_hour}` → `{connected, steps}`; steps `credentials`, `public_url`, `image`, `saved`, `setup_test` (D057) |
| `DELETE /v1/executors/{kind}` | `204`; `409 executor_in_use` while routing names it, `409 machines_running` while one of its machines may run |
| `GET /v1/apps` · `POST /v1/apps` | `{name}` |
| `POST /v1/apps/{id}/disable` · `…/enable` | a disabled app's keys, upload tokens and playback stop; the Dashboard app can't be disabled |
| `GET`/`POST /v1/apps/{id}/api-keys` · `DELETE /v1/api-keys/{id}` | the secret (`key`) is in the create response only; `409 public_url_required` while the public URL is loopback |
| `GET`/`POST /v1/apps/{id}/signing-keys` · `DELETE /v1/signing-keys/{kid}` | `private_key_pem` in the create response only |
| `GET`/`POST /v1/apps/{id}/webhook-endpoints` · `DELETE /v1/webhook-endpoints/{id}` | `secret` in the create response only |
| `POST /v1/webhook-endpoints/{id}/secret` | `201 {secret, active: false}`: a new secret, shown once; deliveries stay signed with the old one until it is activated (D069) |
| `POST /v1/webhook-endpoints/{id}/secret/activate` | `{active: true}`: deliveries are signed with the new secret from the next try; `409 no_pending_secret` |
| `GET /v1/webhook-endpoints/{id}/deliveries` | the latest 100: `{data: [{id, event_id, event_type, status, attempts, last_status, last_error, created_at, finished_at, next_attempt_at}]}`. Endpoints list `consecutive_failures` and `skipped_until`: after 5 failed tries in a row an endpoint is skipped for a minute, doubling to an hour, without spending its deliveries' tries |
| `POST /v1/webhook-deliveries/{id}/redeliver` | `202`: a delivery that gave up (`failed`) is retried for a fresh 24 hours; `409 not_redeliverable` otherwise |
| `GET /v1/library?app_id=&status=&cursor=&limit=` | every app's assets, newest first |
| `POST /v1/assets/{id}/preview-token` | a one-hour `v` token signed with the server's internal key: `{playback_id, token, url, expires_at}` |
| `GET /v1/cloudflare` | `{connected, hostname, bucket, account_id, worker_version, current_version, needs_redeploy, storage_ok}` |
| `POST /v1/cloudflare/connect` | `{token, hostname, bucket?, account_id?}` → `{connected, steps: [{name, ok, detail}]}`; runs Connect Cloudflare, stopping at the first failed step (each is safe to repeat). The token is used for this request only |
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
