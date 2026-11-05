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

## Integration API

| Route | Scope | Notes |
| --- | --- | --- |
| `POST /v1/uploads` | `uploads:write` | `{size, playback_policy: "signed"\|"public", passthrough}` → `201 {upload_id, asset_id, part_size, part_count, upload_token, playback_ids}` |
| `POST /v1/uploads/{id}/parts` | upload token | `{part_numbers: [1…]}` (≤ 100) → `{parts: [{part_number, url}], expires_at}`; PUT each part to its URL and keep its `ETag` |
| `GET /v1/uploads/{id}/parts` | upload token | `{parts: [{part_number, etag, size}]}` — what storage holds, for resuming |
| `POST /v1/uploads/{id}/complete` | upload token | `{parts: [{part_number, etag}]}` → the asset, `processing` |
| `POST /v1/uploads/{id}/cancel` | upload token or `uploads:write` | the asset becomes `errored` / `upload_cancelled` |
| `GET /v1/assets/{id}` | `assets:read` | the asset (below) |
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
| `POST /v1/session` · `GET /v1/session` · `DELETE /v1/session` | sign in (`429` with `Retry-After` after repeated failures), who am I, sign out |
| `PUT /v1/settings/urls` | `{public_url, media_url?}`; `409 pinned` when the environment sets them |
| `GET /v1/apps` · `POST /v1/apps` | `{name}` |
| `POST /v1/apps/{id}/disable` · `…/enable` | a disabled app's keys, upload tokens and playback stop; the Dashboard app can't be disabled |
| `GET`/`POST /v1/apps/{id}/api-keys` · `DELETE /v1/api-keys/{id}` | the secret (`key`) is in the create response only; `409 public_url_required` while the public URL is loopback |
| `GET`/`POST /v1/apps/{id}/signing-keys` · `DELETE /v1/signing-keys/{kid}` | `private_key_pem` in the create response only |
| `GET`/`POST /v1/apps/{id}/webhook-endpoints` · `DELETE /v1/webhook-endpoints/{id}` | `secret` in the create response only |
| `GET /v1/library?app_id=&status=&cursor=&limit=` | every app's assets, newest first |
| `POST /v1/assets/{id}/preview-token` | a one-hour `v` token signed with the server's internal key: `{playback_id, token, url, expires_at}` |
| `GET /v1/cloudflare` | `{connected, hostname, bucket, account_id, worker_version, current_version, needs_redeploy, storage_ok}` |
| `POST /v1/cloudflare/connect` | `{token, hostname, bucket?, account_id?}` → `{connected, steps: [{name, ok, detail}]}`; runs Connect Cloudflare, stopping at the first failed step (each is safe to repeat). The token is used for this request only |
