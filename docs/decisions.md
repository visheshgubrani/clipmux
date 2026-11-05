# Decisions

Decisions made after [architecture.md](architecture.md) (OSS-V2, revised
2026-11-02). Where a decision here differs from the architecture doc, this file wins.

## D001 Rebuild from scratch in a new repository — accepted (2026-11-05)
Context: the TypeScript ClipMux (now `clipmux-lms`) runs the LMS in production; its `open-source` branch only partly moved toward OSS-V2 and still used TypeScript, Graphile Worker and HS256 without the Asset/Output model.
Decision: build ClipMux greenfield here with fresh history. `clipmux-lms` enters maintenance mode (its D025) and retires once the LMS migrates. The public `visheshgubrani/vod` repo is archived.
Consequence: nothing is imported wholesale; behaviour and tests are ported task by task (see the port map in plans/P1-core-product.md).

## D002 Go for the core; TypeScript only at the edges — accepted (2026-11-05)
Decision: server, worker and CLI are one Go binary, as OSS-V2 specifies. The dashboard, `@clipmux/player`, `@clipmux/uploader` and the Cloudflare Worker are TypeScript.
Why: the worker ships to other people's machines (PC agent, `push`, `--once` on serverless) as one cross-compiled binary; ffmpeg/Shaka supervision and sandboxing; small self-host footprint.
Consequence: delivery checks exist twice (Go handler, TS Worker), held together by shared JSON test vectors.

## D003 Library defaults — accepted (2026-11-05)
Decision: stdlib `net/http` routing (Go 1.22+ patterns), `pgx/v5`, `sqlc` for queries, `goose` for migrations, `log/slog`. Linting is `staticcheck -checks all` plus `govulncheck` (amended in D009, replacing the planned golangci-lint). The dashboard is a Vite + React SPA embedded with `go:embed` from `internal/web`.

## D004 Walking-skeleton milestone in Phase 1 — accepted (2026-11-05)
Decision: Phase 1 first reaches M1 — API upload → built-in worker → AES-128 package → commit → signed playback in hls.js — before sweepers, webhooks, first boot and the dashboard. Those are completed before the Phase 1 exit test, not dropped.

## D005 Private until the Phase 1 exit test; licence decided before going public — accepted (2026-11-05)
Decision: the repository stays private, with no LICENSE file, until Phase 1 passes. The licence (including how a bundled GPL FFmpeg build is handled) is decided as part of plans/launch.md. Prior options on record: Apache-2.0 (the archived `vod` repo) and AGPL-3.0 core + MIT SDK/player (clipmux-lms D021, proposed).

## D006 Names and registries — accepted (2026-11-05)
Decision: the product is ClipMux. Container image `ghcr.io/visheshgubrani/clipmux` (single image). Never publish to `ghcr.io/visheshgubrani/clipmux/server` or `/web` — production `clipmux-lms` pulls those. npm packages keep the `@clipmux` scope and start above 0.2.0 (0.1.x were published from the archived repo; 0.2.0 is vendored in the LMS). `clipmux.com`, `api.` and `media.` stay on clipmux-lms until it retires.

## D008 Queue and worker API details — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T03:
- Claim long-polls by retrying every second for up to 30 s (default 25 s); LISTEN/NOTIFY can replace the polling later without changing the API.
- The built-in worker authenticates with a key minted per boot and held only in memory. PC agent keys (Phase 3) and one-time serverless keys (Phase 4) plug into the same `WorkerAuth` interface.
- Executors never claim `move` or `delete` jobs; those run in the control plane with storage credentials.
- Error codes the queue assigns: `worker_lost` (lost-lease budget of 5 spent) and `processing_failed` (failure budget of 3 spent, or a worker sent a code that isn't snake_case). Permanent failures keep the worker's code.
- Transient failures back off 30 s, then 2 min.
- Claims are refused with 409 unless the worker supports contract version 1 (`unsupported_contract`) and reports Shaka Packager ≥ 3.9.1 (`packager_too_old`).
- A lost attempt whose worker heartbeats back while it is still the latest attempt returns to `running`; its lost lease still counts against the budget.
- Worker responses are `{"action": "continue" | "stop", "reason": …}` so a worker needs one code path; errors are `{"error": {"code", "message"}}`.

## D009 Lean structure, checked against go.dev guidance — accepted (2026-11-05)
Context: before T04 the repo was reviewed against go.dev/doc/modules/layout, the Code Review Comments wiki, "Package names" and Effective Go, to avoid the sprawl of the earlier repos.
Decision and changes:
- Removed seven placeholder packages (`api`, `worker`, `media`, `tokens`, `outbox`, `executor`, `delivery`) and the `auth` package: no package or interface before the code that needs it.
- sqlc now generates into `internal/db` (`db.Job`, `db.New(tx)`), not a package named after the tool.
- Secrets use `crypto/rand.Text` (Go 1.24+) instead of three hand-written generators.
- `serve` waits for its background goroutines before the pool closes, and requests inherit the server context so long-polls end at shutdown.
- The embedded dashboard moved to `internal/web`, so nothing in the module is importable from outside.
- CI gains a lint job: gofmt, vet, `staticcheck -checks all`, `govulncheck`.
- The `go` directive stays at 1.27 (a supported release line); CI installs its latest patch.
Consequence: AGENTS.md "Keeping it lean" lists the rules; T04 onward creates packages as tasks land.

## D010 Filesystem storage details — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T04:
- Every file operation goes through an `os.Root`, so the kernel refuses `..` and symlink escapes; key validation (segments of `[A-Za-z0-9._-]`, no `.`/`..`/empty segment, ≤ 512 bytes) is the first line, the output path grammar (T06) sits in front of both.
- `CreateMultipart` takes the declared size and returns the part size, so the filesystem adapter can preallocate and the API can tell the browser the part size; S3/R2 adapters ignore the size and return 32 MiB.
- Part sizes are enforced at write time: every part but the last is exactly the part size, the last is the remainder, and a part never writes past its range. A part becomes visible (ListParts) only after its bytes land. ETags are quoted MD5s, as on S3.
- Signed URLs are `{public_url}/_storage/{parts/{upload}/{n} | objects/{key}}?exp=&sig=`, with sig = HMAC-SHA256 over method, resource and expiry. The key lives in `storage/signing.key` (mode 0600) so URLs survive restarts. CORS allows any origin and exposes `ETag`, because the URL is the authorisation.
- Stored bytes are served as `application/octet-stream` with `nosniff`, `CSP: sandbox` and `no-store`.
- `List` and `DeletePrefix` take directory-like prefixes ending in `/`; every prefix the core uses has that shape.
- `Capabilities()` is left out until the dashboard needs adapter compatibility (Phase 2), per D009.
- `CLIPMUX_PUBLIC_URL` (an origin, default `http://localhost:8080`) is configuration until first boot stores it (T10).

## D011 Upload API: reliability rules and browser clients — accepted (2026-11-05)
Context: uploads must be seamless; reliability decides every trade-off here.
Decision:
- Server side is the standard library over the `Storage` interface. The S3 SDK (`aws-sdk-go-v2`) arrives only inside the R2/S3 adapter in Phase 2; the upload API does not change then.
- Browser side: the endpoints map one to one onto `@uppy/aws-s3` callbacks (createMultipartUpload via the app's backend, signPart, listParts, completeMultipartUpload, abortMultipartUpload), with `getChunkSize` returning the `part_size` from create. Uppy is supported, never bundled; a test drives the endpoints in Uppy's call order. `@clipmux/uploader` (T11, from clipmux-lms, proven in production) is the zero-dependency default.
- Create is one transaction (asset, playback ID, upload, storage multipart). `Idempotency-Key` replays it per app; a different body under the same key is 422. Upload tokens are never stored, so a replay rotates the token.
- Complete runs under the upload's row lock and is idempotent. If storage finished but the commit did not, the next complete or sweep recovers from the source object. ETags are compared without quotes.
- The sweeper (every 5 min) completes uploads whose parts all landed, records newly stored parts as activity, and expires an upload only after 24 h without activity (a signed part or a new part). It never touches cancelled uploads.
- Part URLs last 1 hour so a batch can be used sequentially on a slow link; signing is limited to 100 parts per call and to parts within the declared size.
- The upload endpoints answer CORS for any origin: the browser carries an upload token, never a cookie. Cancel accepts the upload token or the owning app's API key.
- `clipmux admin create-key --app NAME` issues keys until the dashboard exists (T10). Keys are `cmx_`-prefixed and stored as SHA-256.

## D012 Worker pipeline and content keys — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T06:
- One content key per output: `HMAC-SHA256(secret, "clipmux-content-key-v1\n" + asset_id + "\n" + output_id)[:16]`; the packager key ID is `SHA-256("clipmux-key-id-v1\n" + output_id)[:16]`. Test vectors (computed independently) live in `internal/contentkey` for the Phase 2 Worker. The secret is `{data_dir}/content.key` (`v1:{hex}`, mode 0600) until T10 moves it into Postgres, encrypted.
- Encoding: one ffmpeg run decodes once and encodes the ladder with libx264 (`veryfast`, high profile, capped VBR with clipmux-lms's production bitrates), keyframes forced every 6 s, closed GOPs, no scene cuts. Rungs follow the source's short side, so vertical video gets the same ladder; a source below 360 gets one rung at its own size. Audio is AAC 128k, 48 kHz stereo, loudness-normalised, as its own rendition.
- Packaging: Shaka 3.9.3 `--protection_scheme=aes128 --enable_raw_key_encryption --clear_lead=0`, key and key ID through a mode-0600 flag file (never argv), `--hls_key_uri=../key/{version}`, segments `seg_00001.m4s`. The Shaka #1616 init repair is ported from clipmux-lms; the golden test proves segments decode to 0 packets without it.
- Sandbox for M1: empty environment, its own working directory, wall-clock limits from the job context, input demuxer allowlist (no HLS, concat or image sequences) and `-protocol_whitelist file`. Network namespaces, rlimits and CPU caps are deferred (known gaps).
- Sign: only output-grammar paths, at most 500 per call and 20,000 per attempt (`attempts.signed_paths`), each under `media/{asset_id}/{output_id}/`; PUT URLs last 15 min, source URLs 1 h. The worker uploads manifest.json last.
- Claim payloads carry the source URL and size and the output's key. A job whose source is missing fails permanently with `source_missing` at claim.
- Worker failure codes: `unsupported_source` (permanent), `source_download_failed`, `transcode_failed`, `upload_failed`, `commit_failed` (transient). A server "stop" is never reported as a failure.
- `clipmux worker` takes its key from `CLIPMUX_WORKER_KEY`, never argv. `serve` starts it as a child once commit exists (T07), so a worker can't loop on a missing endpoint.

## D013 Commit and route publication — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T07:
- Verification runs before any lock and trusts nothing the worker checked: the manifest must equal a full listing of the output prefix (sizes included, no extra files), the master may reference only `{rendition}/index.m3u8` and no key, and every media playlist must be AES-128 from its first segment with `../key/{version}` and name only stored segments. A failed check is 409 `output_invalid`; the worker reports it as a transient failure, so the job retries on a fresh attempt and output.
- The commit transaction runs inside the liveness lock (`queue.CommitAttempt`): output committed, `current_output_id` moved (version +1), previous output retired, attempt committed, job succeeded, one `route_sync` outbox row per active playback ID.
- Route publication confirms `route_sync` rows. The built-in handler reads routes from Postgres, so v1 only confirms; the Phase 2 Worker adapter writes `_routes/{playback_id}.json` at the same point. Publication runs inline after the commit and every 15 s from a sweeper.
- When the last route of the current output is confirmed, the asset moves `processing → ready` (version +1, `video.asset.ready`) or, for a re-encode, stays ready with `video.asset.updated`. The asset row is locked before counting pending routes, so exactly one publisher finishes. An asset with no playback IDs is ready in the commit transaction.
- `serve` supervises the built-in worker as a child (`clipmux worker` over loopback) with only PATH, the tool paths and its key in its environment, restarting with backoff. Missing media tools disable it with one clear error rather than a crash loop. `CLIPMUX_BUILTIN_WORKER=false` turns it off.
- `complete` accepts manifests up to 8 MiB (a long video lists thousands of segments).

## D014 Video.js for the shipped player; hls.js for development and tests — accepted (2026-11-05)
Context: delivery works with any HLS player (tokens ride in the path and every playlist URI is relative), so the choice only shapes `@clipmux/player`, the integration docs and the exit test. clipmux-lms ships a Vidstack player.
Decision:
- `@clipmux/player` and the integration docs use Video.js: the most widely adopted open-source player, with accessible controls, captions, skins and plugins, and an HLS engine that plays fMP4, AES-128 and separate audio. Token refresh is a small plugin on its HLS request hook: on a 401 it calls `getToken()` and rewrites `/v/{old}/` to `/v/{new}/` for later requests, resuming at the same position.
- hls.js is the development and test player (the M1 browser check, debugging): it exposes every event and needs no UI.
- Before T11, check Video.js 10 (announced as a rewrite with the Vidstack and Plyr teams): use it if stable, otherwise Video.js 8.
Consequences: iOS Safari plays natively under Video.js by default, where no web player can refresh a token, so phones rely on the lifetime rule (2 × duration or 4 h), as AirPlay and Chromecast already do; the player docs must say so. The Phase 1 exit test adds Video.js. The clipmux-lms Vidstack player is reference for its renewal logic only. The LMS switches players when it migrates.

## D015 Built-in delivery — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T08:
- Tokens are ES256 JWTs on the standard library only (`internal/tokens`), interoperable with openssl and Node in both directions. Only `alg: ES256` with a `kid` is accepted. base64url decoding is strict (a lenient decoder accepted 15 spellings of one token), and claim names are matched exactly with the last duplicate winning, as `JSON.parse` does, so the Go handler and the Worker read every token alike. Both ECDSA forms `(r, s)` and `(r, n−s)` verify: JWT libraries, openssl and WebCrypto produce and accept either, so rejecting one would refuse about half of all app tokens (amended in D016).
- Checks, in order: route (unknown, revoked, deleted or not-yet-playable → 404), policy (a signed ID without a token → 403), signature with a key that belongs to the route's app or is the server's internal key (no app), `sub` = playback ID, `aud` ∈ {v, t}, `iat` no more than 60 s ahead, `exp − iat` within the project maximum (→ 403), then `exp` (→ 401 token_expired).
- The master is generated per request: every URI gets `{output_id}/` in front, so a session stays pinned to one output; served `no-store`. Pinned files are served only from this asset's committed or retired outputs, only for output-grammar names (never manifest.json), with Content-Type from the extension (`.m3u8 .mp4 .m4s .jpg`), `nosniff`, and `private` (signed) or `public` `max-age=31536000, immutable`. The key path derives the key, `no-store`, and only for the output's key version.
- A `t` token fetches only the poster, unpinned at `{playback_id}/poster.jpg` (or pinned). Public IDs need no token and also play under a token URL.
- CORS `*` on every delivery response: the URL is the credential and there are no cookies.
- `CLIPMUX_MEDIA_URL` (default `CLIPMUX_PUBLIC_URL`) is returned as `base_url` on assets and in ready/updated webhooks.
- `testdata/vectors.json` (43 cases since D016, test-only keys, Python-computed content keys) is the contract the Phase 2 Worker must also pass.
- `clipmux admin create-signing-key --app NAME` stores the public key and prints the private key once, until the dashboard exists.

## D016 Review of P1-T01 to T08 and its fixes — accepted (2026-11-05)
Context: two independent code reviews (correctness and security/spec) and a functional matrix on the real binary (36 checks: uploads, playback, token rules, isolation, cancel, bad sources, concurrency, a killed worker, a serve restart). The functional checks all passed; the reviews found the defects below, each fixed with a test that fails without the fix.
- Playlist checks parse HLS attribute lists strictly (upper-case names, no duplicates, quoted URIs, nothing trailing) and allow only the tags Shaka writes. Before, a duplicate or unquoted `URI=` passed and hls.js would follow it to another host.
- Presigned PUTs are create-once (filesystem: hard link from tmp; R2/S3: signed `If-None-Match: *`), so a worker can't overwrite an output after commit with a URL that is still valid. The worker treats 412 as already written; commit verifies every size.
- `CompleteMultipart` recovers when a crash fell between moving the data into place and cleaning up staging, instead of failing every retry and leaving the upload stuck.
- The worker bounds transfers with an idle watchdog (60 s without a byte cancels), bounds connecting and response headers, and bounds each attempt to 6 h (`job_timeout`), so a stalled connection can't hold a lease forever. A download that completes on its last try succeeds. `fail` and `complete` check the HTTP status and retry 5xx. Errors never carry a signed URL or a playback token.
- A claim whose payload can't be delivered (the worker hung up, or signing the source failed) is released: attempt and empty output deleted, job requeued, no lost lease counted.
- Delivery denies a disabled app's playback IDs, refuses retired signing keys, and requires the server's internal key to belong to the route's project. Upload tokens of a disabled app stop working.
- The unpinned poster is cached for 5 minutes (it follows the current output); a `t` token may fetch the pinned poster.
- Playback IDs carry `playable` (false until the asset has an output). A replayed create for a finished upload returns no token instead of one that can never work.
- The upload sweeper visits uploads least-recently-swept first, so one that keeps failing can't starve the rest.

## D017 Secrets at rest — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T09:
- `internal/secrets` seals every secret kept in Postgres with AES-256-GCM. A sealed value is `0x01 | key version (uint32 BE) | 12-byte nonce | ciphertext+tag`, and the associated data is `clipmux-secret-v1\n{table}\n{row_id}`, so a ciphertext copied to another row or table won't open.
- The key is `CLIPMUX_SECRETS_KEY` (`v1:` + 64 hex digits) or, unset, `{data_dir}/secrets.key` (mode 0600). Only `serve` creates the file, and only on a database that holds no sealed secrets yet; `clipmux admin` only loads it, so an admin run with the wrong data directory can't seal secrets under a key serve doesn't have.
- `projects.secrets_check` holds a value sealed on first boot; serve refuses to start with a key that can't open it. Key rotation is deferred (known gap).

## D018 Webhook delivery — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T09:
- An outbox webhook row is the event. A publisher pass (every 5 s in serve) fans it out to one `webhook_deliveries` row per enabled endpoint of the event's app, then sends due deliveries. An event for an app with no endpoints is done; apps catch up with `GET /v1/assets?updated_since=`.
- Bodies are `{"id","type","created_at","data":<the asset as GET /v1/assets/{id} returns it>,"job"?:{"id","kind","error_code"}}`, built inside the state change's transaction, so `data.version` is the version the event describes. The bytes sent are the stored jsonb as Postgres renders it, identical on every try: receivers must verify the raw body.
- Signing is the scheme the LMS already verifies: `X-Webhook-Signature: sha256=<hex HMAC-SHA256(secret, "{timestamp}.{body}")>` with `X-Webhook-Timestamp` in Unix seconds, fresh on every try, and the secret is the `whsec_…` string's bytes. Headers also carry `X-Webhook-Event`, `X-Webhook-Id` (the event ID to dedupe on) and `User-Agent: ClipMux-Webhooks/1.0`.
- Any 2xx is success. Redirects are failures and never followed; a request has 10 s; at most 4 KiB of the response is read and none is kept. Failures are stored as codes (`http_500`, `timeout`, `connection_failed`, `endpoint_disabled`, `app_disabled`), never response text.
- Retries wait 30 s, 2 min, 10 min, 30 min, 1 h, 2 h, 4 h, then 8 h, and stop 24 h after fan-out (the last try is moved to exactly 24 h): 11 tries in all. A claimed delivery is leased for 60 s and results are fenced on the attempt count, so a crashed sender only delays a resend.
- Only the owner creates endpoints (`clipmux admin create-webhook` until the dashboard), so their URLs (http or https, no credentials or fragment) are not filtered for private addresses: a self-hosted receiver often sits on the same private network.

## D019 Deleting an asset — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T09:
- `DELETE /v1/assets/{id}` (`assets:write`) commits one transaction: lock the active upload, then the active job, then the asset (the order every writer uses); cancel the upload and the job, which fences their attempts; mark the asset `deleted` (version +1, `error_code` cleared); mark its playback IDs `revoking` with an unpublish `route_sync` row each; queue a `delete` job. Playback stops at that commit, because delivery refuses deleted assets.
- The delete job runs in serve (`internal/lifecycle`), never on a worker. Each pass is safe to repeat: wait until every unpublish is confirmed, abort any multipart upload (its staging lives outside the asset's prefixes), delete `uploads/{id}/` and `media/{id}/`, then in one transaction set `deleted_at` and write `video.asset.deleted` only the first time. If a job of the asset ended less than 16 minutes ago, a second pass runs then, because PUT URLs signed before the cancel last 15 minutes.
- The request runs the job inline for up to 10 s and answers 200 when the files are gone, or 202 while the job continues; both bodies are the asset view with `status: "deleted"`. Deleting a deleted asset changes nothing. `GET /v1/assets/{id}` keeps answering for a deleted asset, so a late webhook receiver can confirm it.
- Revoked playback IDs read `revoked` only once their unpublish is confirmed; until then they read `revoking`.

## D020 Catching up on missed events — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T09:
- `GET /v1/assets?updated_since={RFC 3339}` lists every asset of the app whose `updated_at` is at or after the time, oldest first, ordered by `(updated_at, id)`, deleted assets included, at most 100 per page (`limit`), with an opaque `next_cursor` when there is more. It can't be combined with `passthrough`.
- Every change to what the view shows bumps `version` and `updated_at`. `updated_at` is the transaction's start time, so a change can commit slightly after a later one; clients should overlap their window by a few minutes and keep the highest `version` per asset.
- A failed re-encode changes nothing in the view, so it is visible only as its `video.asset.reencode_failed` webhook (known gap).

## D021 Re-encode and playback-ID endpoints — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T09b:
- `POST /v1/assets/{id}/reencode` (`assets:write`) queues a `reencode` job and answers 202 `{id, kind, status}`. Another active job gives 409 `{"error":{"code":"job_active","job_id":…}}`; `{"supersede": true}` cancels it first (its worker is told to stop). A ready asset keeps playing until the new output commits (`video.asset.updated`, or `reencode_failed`); an errored asset returns to `processing` with its error cleared. It needs a kept, stored source (409 `source_missing`), not `waiting_upload` (409 `asset_not_ready`) and not deleted (409 `asset_deleted`).
- `POST /v1/assets/{id}/playback-ids` adds a `signed` (default) or `public` ID, at most 1000 live per asset, answering 201. Its route is published from the outbox; once it is live the app gets `video.asset.updated`. `DELETE /v1/assets/{id}/playback-ids/{pid}` marks the ID `revoking` (delivery refuses it at once) and answers 200 `revoked` once the unpublish is confirmed, or 202 `revoking` before; the confirmation bumps the version and sends `video.asset.updated`. Revoking twice is harmless.
- Every POST takes `Idempotency-Key` through `internal/idempotency`. A key belongs to the app and to one request: the hash covers the route, the asset and the canonical body, so reusing a key on another endpoint or asset is 422 `idempotency_key_reused`. Upload keys stored before this change no longer replay.
- Request validation errors carry details beside code and message (`httpjson.Problem`).

## D022 Server keys live in Postgres, sealed — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T10:
- Content key secrets are rows of `content_key_secrets` (one per version), sealed with the secrets key (D017, associated data `content_key_secrets` / `{project}/v{n}`). On first boot serve imports `{data_dir}/content.key` if an older install left one, otherwise creates version 1, and removes the file once the database holds the same secret; a file that disagrees with the database stops the boot.
- Delivery and the worker API hold a ring of every version: new outputs use the newest, and an output's key is derived with the version it records, so rotating the secret never strands a viewer mid-session. Serve refuses to start if any output uses a version the ring lacks. (Rotation itself has no command yet.)
- The server's internal ES256 key (`signing_keys` with no app) is created at boot, with its private key sealed; a partial unique index allows one active per project. App keys still keep only their public half (`internal/signingkeys`).
- The filesystem storage URL-signing key stays in `{data_dir}/storage/signing.key`: it guards only the files in that same directory, so sealing it in Postgres would protect nothing a copy of the disk doesn't already expose.

## D023 Public and media URLs are project settings — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T10:
- `projects.public_url` and `projects.media_url` (an origin, renamed from `media_hostname` so dev keeps scheme and port) are the source of truth. `base_url` in asset views and webhook events is read from the project in the same transaction, so no component keeps a copy. Only filesystem storage signs with the public URL in memory; `internal/settings` updates it the moment the owner saves new URLs.
- `CLIPMUX_PUBLIC_URL` (and optionally `CLIPMUX_MEDIA_URL`) pins both URLs: they are written to the project at boot and the first-boot URL step is read-only. Unset, a fresh install stores `http://localhost:8080` unconfirmed and the owner chooses real URLs at first boot. `CLIPMUX_MEDIA_URL` alone is refused.
- No API key while the public URL is loopback (localhost, `*.localhost`, 127.0.0.0/8, ::1, 0.0.0.0): signed upload URLs and `base_url` would point at the owner's machine. LAN addresses count as reachable. `clipmux admin create-key --allow-loopback` is the development escape hatch.
- Caddy obtains certificates on demand and asks `GET /_tls/ask?domain=` first; it answers 200 only for the public and media hostnames, never for IPs or loopback names, so the Caddyfile names no hosts.
- The built-in worker runs with `--storage-via-server`: signed `/_storage/` URLs go to serve over loopback rather than back through the public hostname, which may not route from inside the host (the signature covers method, path and expiry, not the host).

## D024 Owner account and dashboard sessions — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T10:
- `POST /v1/setup/owner` takes the setup token (fresh each boot, held in memory, compared in constant time, printed only while no owner exists) with an email and a password. `owners.project_id` is unique, so registration closes atomically: a second one is 409 `registration_closed` however many race.
- Passwords are PBKDF2-HMAC-SHA256 from the standard library, 600,000 iterations, 16-byte salt, stored as `pbkdf2-sha256$iterations$salt$hash`, 12 to 1024 characters, rehashed on sign-in when below today's count. Argon2id would need `x/crypto`; one throttled account doesn't justify it.
- Sessions are a random cookie whose SHA-256 is stored, ending after 30 days or 7 days idle. The cookie is HttpOnly, SameSite=Strict, Path=/ with no Domain, and on an https public URL it is `__Host-clipmux_session` and Secure (`clipmux_session` on plain http). State-changing cookie requests from another site are refused with `http.CrossOriginProtection`.
- Sign-ins are limited before any hashing: 5 failures per address and 20 overall per 15 minutes answer 429 with `Retry-After`; an unknown email costs the same hash. The address is the connection's, or the forwarding chain's first untrusted hop when the connection comes from `CLIPMUX_TRUSTED_PROXIES`.
- `clipmux admin reset-password` reads the new password from stdin and ends every session. TOTP is deferred (known gap).

## D007 Shaka Packager 3.9.3 — accepted (2026-11-05)
Decision: pin v3.9.3 (`toolchain/versions.env`), the version clipmux-lms runs with AES-128 in production, which needs the init-segment workaround for Shaka issue 1616. Move to a release that fixes 1616 when one exists, re-running the Phase 1 player tests.
