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

## D025 Management API and the owner acting as an app — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T10:
- The dashboard's routes (`internal/manage`, listed in docs/api.md) are public `/v1` routes that need the owner's session; an API key gets 401 on every one. They cover the URL step, apps (create, disable, enable; the Dashboard app can't be disabled), API keys, signing keys and webhook endpoints (each secret only in its create response), the library across apps, and preview tokens.
- The integration routes take an `apikeys.Authenticator`: a bearer API key, or, without one, the owner's session acting as the app named by `ClipMux-App` (the Dashboard app by default) with every scope. So a dashboard upload is an ordinary `POST /v1/uploads`, with no private path.
- API keys are refused with 409 `public_url_required` while the public URL is loopback (D023). A key created in the dashboard defaults to `uploads:write`, `assets:read` and `assets:write`.
- Preview tokens are signed with the internal key, last one hour, and pick the asset's first active signed playback ID (a public one otherwise).
- Asset views gain `app_id`, so the library can say which app owns each asset.

## D026 Setup test — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T10:
- The sample is embedded in the binary (`internal/setuptest/sample.mp4`, 8 s, 854×480 so the ladder makes two rungs plus audio, about 480 KB), produced by `toolchain/make_sample.sh`, so the control plane never runs ffmpeg.
- A run creates the upload in-process as the Dashboard app's backend, then does what a browser and a player do, over HTTP to the configured URLs: sign parts and PUT them on the public URL, complete, wait for `ready`, then play through the media URL with internal-key tokens. Ten checks, in order: `upload`, `encode`, `play_master`, `play_key` (16 bytes), `play_segment_decrypts` (AES-128-CBC with the delivered key and the playlist's IV gives an fMP4 fragment), `thumbnail_token_refused_segment`, `thumbnail_token_refused_key`, `thumbnail_token_poster`, `no_token_refused`, `stored_segment_encrypted` (the stored bytes don't parse as a fragment). A failed check skips the ones that depend on it. Details never carry a URL or a token.
- One test at a time per project (a partial unique index); asking again returns the queued one. Triggers: first boot (when the built-in worker is on and no test ever ran), saving the URLs, `POST /v1/setup-test`, and `clipmux admin setup-test --wait`, which exits non-zero unless every check passes. A new run deletes earlier samples, so the latest stays playable in the dashboard.
- `CLIPMUX_SELF_DIAL=host:port` makes every check connect there (Caddy, in compose) while keeping the URL's hostname for TLS and routing, because a container often can't reach its own public hostname.

## D027 Media tool sandbox — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T10:
- Every ffmpeg, ffprobe and packager run goes through a hidden `clipmux sandbox-exec`, which confines itself on a locked thread and then execs the tool, so the tool starts confined: address space (8 GiB, `CLIPMUX_TOOL_MEMORY_MB`), CPU (6 h), file size (64 GiB), 1024 files and no core dumps; nice 19 and `SCHED_IDLE`; `no_new_privs`; a seccomp filter where `socket()` fails with EACCES (no network) and ptrace, bpf, io_uring, userfaultfd, perf, mount and keyring calls with EPERM, killing foreign-architecture and x32 calls; and Landlock where the kernel has it: system paths and the tool's own directory read-only, its own `/proc` entry only (other processes' environments can hold secrets), one writable directory (the attempt's), no TCP on ABI 4+, and no signals or abstract sockets outside on ABI 6+. The tool's environment is only `TMPDIR`, pointing at that directory. Standard library only (`syscall`).
- Network namespaces and cgroups aren't available to an unprivileged process in Docker's default runtime, so the sandbox doesn't use them: seccomp stands in for "no network", and `SCHED_IDLE` is the CPU cap (compose `cpus:` is the hard one).
- Limits, `no_new_privs` and seccomp are required. `serve` probes (`sandbox-exec --probe`) before starting the built-in worker and leaves it off with one clear error where they don't hold, unless `CLIPMUX_UNSANDBOXED_WORKER=true` says the host isolates it another way. Landlock is reported, not required. The worker reports what holds in its claim (`worker_info.sandbox`).

## D028 Packaging — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T10:
- One image (`Dockerfile`): a toolchain stage builds FFmpeg from source with `toolchain/build_ffmpeg.sh` (BuildKit cache) and installs the pinned packager; Go builds `CGO_ENABLED=0 -trimpath`; the runtime is Ubuntu 24.04 with the runtime package list, `verify_toolchain.sh` run on the final image, uid 10001, tini, and a `clipmux healthcheck` command instead of curl. It is not published (D005, D006).
- `deploy/compose.yml` runs the app (`read_only`, tmpfs `/tmp`, `cap_drop: ALL`, `no-new-privileges`, port 8080 on 127.0.0.1 only), `postgres:18-alpine` and `caddy:2`. The Caddyfile names no host: on-demand TLS asks `/_tls/ask`, which is blocked from outside; plain HTTP serves first boot. The app trusts Docker's private ranges as proxies and self-dials `caddy` for the setup test.
- CI's image job builds with the GHA cache, starts the stack, requires the sandbox probe and a refused socket inside the container, passes `clipmux admin setup-test --wait` through Caddy, and checks health through Caddy. Locally all of that holds under Docker's default seccomp profile, including Landlock.

## D029 Video.js 10 with a refresh loader — accepted (2026-11-05)
Amends D014. Made in P1-T11/T12:
- Video.js 10 went stable on 1 October 2026, so `@clipmux/player` uses it: `@videojs/html` 10.2.0 (`<video-player>`, `<video-skin>`) around `<hlsjs-video>` (`@videojs/hlsjs-video` 10.2.0, hls.js 1.6.19), preferring MSE. Versions are pinned exactly and are at least two weeks old when taken.
- Refresh is an hls.js `loader` wrapping hls.js's own: hls.js loads playlists, segments and AES-128 keys through it, and never retries a 4xx. Every request carries the session's current token (`/v/{token}/{playback_id}/` on the media origin only); a 401 calls `getToken()` once for every waiting request and retries through a fresh inner loader, so hls.js never sees the error and the position holds. A second 401 on one request, a 403, or `null` from `getToken` stops playback (`clipmux-denied`). There are no timers: VOD fetches ahead, so the first 401 arrives while seconds are still buffered.
- `<clipmux-player base-url playback-id>` fetches its first token through `getToken` when none is set, and reloads only when what plays changes.
- The spike passed in Chromium (the exit suites below: AES-128, separate audio, an 8 s token refreshed mid-playback with one continuous played range). iOS 17.1+ plays through hls.js on ManagedMediaSource and refreshes; native HLS (older iOS, AirPlay, Chromecast) can't and relies on the token lifetime. Safari's run is the macOS CI job; if it fails there, the fallback recorded in docs/plans/P1-T09-T12.md (Video.js 8 + VHS) applies.

## D030 TypeScript workspace — accepted (2026-11-05)
- A pnpm workspace at the root (pnpm 12.11.2, Node 24): `packages/uploader`, `packages/player`, `e2e/web`, and the dashboard. TypeScript 7 builds the packages with `tsc` (ESM and declarations); tests run on Node's built-in runner with type stripping; Biome lints and formats. Packages are `0.3.0` and private until launch (D006).

## D031 Uploader resume and retries — accepted (2026-11-05)
- Storage is the source of truth: the uploader keeps only `upload_id`, `upload_token`, `part_size` and `part_count` (`rememberUpload(localStorage)`), and each start lists stored parts and sends the rest; a part URL is signed in windows of 100 and again within 5 minutes of expiry or after a storage 403. Network errors, 5xx, 408 and 429 retry `min(30 s, 1 s·2ⁿ)` plus up to 1 s of jitter, 6 tries, honouring Retry-After and waiting while offline. A fatal error stops the other parts and never cancels the server upload, which stays resumable for 24 h. `pause()` stops between parts, `cancel()` ends the upload.

## D032 Dashboard — accepted (2026-11-05)
Made in P1-T10:
- `dashboard/` is a Vite + React 19 SPA with React Router, Tailwind 4 and a few shadcn-style components (button, input, card, table, a Radix dialog). It uses `@clipmux/uploader` and `@clipmux/player`, and calls only the public API with the owner's session (D025): first-boot wizard (setup token → owner → URLs → setup test → the sample plays), sign-in, library, upload, asset detail with a preview, and apps with their API keys, signing keys and webhook endpoints (each secret shown once).
- `make web` builds it into `internal/web/dist`, which is gitignored and embedded with `go:embed`; a binary built without it serves a placeholder page. The image builds it in its own stage. The SPA handler serves real files (hashed `assets/*` immutable for a year), sends every other path `index.html` uncached so deep links work, and answers a JSON 404 under the server's own prefixes (`/v1/`, `/v/`, `/p/`, `/_storage/`, `/worker/`, `/_tls/`, `/healthz`, `/readyz`).

## D033 Exit-test harness — accepted (2026-11-05)
Made in P1-T12:
- `internal/e2e` (`CLIPMUX_E2E=1`) builds the real binary and boots `serve` with its sandboxed built-in worker on a fresh schema. Go exit tests: a signed, encrypted upload plays (ffmpeg as the HLS client); SIGKILLing the worker mid-encode recovers on a new attempt and output under the real 90 s lease (about two minutes; no lease knob, so the shipped defaults are what's proven); a fresh install passes its setup test.
- `TestPlayerFixture` serves the browser suites: an encoded three-rung video on one serve, a fresh unpinned install on another, and pages, tokens and uploads on a third origin. Playwright runs them in Chromium (hls.js, `<clipmux-player>`, the uploader, the dashboard's first boot). WebKit on Linux isn't Safari and isn't run.
- Safari (`exit-safari.yml`, macOS runner, safaridriver driven by a dependency-free WebDriver script) checks native HLS with its separate audio and `<clipmux-player>` refresh. ExoPlayer (`exit.yml`, an API 35 emulator) runs a Media3 instrumented test that reaches the fixture through `adb reverse`. Both run on main, weekly, by hand, and on PRs touching playback; macOS runs unsandboxed because the sandbox is Linux only.

## D034 S3-compatible storage and per-asset stores — accepted (2026-11-05)
Decisions within the architecture's rules, made in P2-T01:
- `storage.S3` is the one adapter for R2, AWS S3 and S3-compatible stores, on `aws-sdk-go-v2`, path-style. Serve's requests go to `endpoint`; presigned URLs name `public_endpoint` (default `endpoint`), which browsers and workers must reach. The SDK's default request checksums are off except where S3 requires them, because R2 and other stores refuse them on some calls. Connecting is bounded at 10 s and response headers at 60 s.
- Parts are always 32 MiB and the declared size is ignored (D010). Complete applies the filesystem's rules to what storage holds: numbered 1..n, ETags matching with or without quotes, every part but the last exactly 32 MiB.
- Create-once PUTs sign `If-None-Match: *` into the URL. Every worker PUT now sends that header, and the filesystem store ignores it. A 412 still means "already written" (D016).
- `List` cursors are the last key returned (`StartAfter`), on both adapters, so a listing resumes across adapters and restarts.
- `internal/stores` resolves `storage_id` to an adapter: `fs` rows share the install's filesystem store, while `r2` and `s3` rows hold their config in `config` and their credentials sealed in `secrets_encrypted` (D017, associated data `storage_configs`/`{id}`). Adapters are opened once and kept. Adding a config first proves list, write, read and delete in the bucket and applies CORS (any origin, GET/HEAD/PUT, exposing `ETag`), then stores it. Setting the default changes only where new uploads go.
- Uploads, the worker API, commit, delivery, delete, reencode and the setup test all use the asset's own store. Delivery reads `storage_id` in its route query, so a segment costs no extra query.
- `Put` streams through the SDK's transfer manager (`feature/s3/transfermanager`, 32 MiB parts, 4 at a time), so a move or an import of any size needs no spooling. The filesystem's server-side `Put` takes up to the upload limit (32 MiB × 10,000); presigned PUTs keep their 5 GiB cap. AWS modules are pinned to releases at least two weeks old (the 2026-10-21 set).
- The local S3 store for tests is SeaweedFS 4.48, pinned by digest in `deploy/compose.dev.yml`. Of the P21 candidates it needs only an S3 identity file (no admin CLI), and it passes the whole contract suite including create-once and CORS. `internal/storage/s3test` gives each test its own bucket, and CI sets `CLIPMUX_REQUIRE_S3=1` so the S3 half never skips.
- `Capabilities()` is still deferred: nothing reads it until the dashboard offers storage choices (T04), per D009.

## D035 Route records, _keys.json and the reconciler — accepted (2026-11-05)
Decisions within the architecture's rules, made in P2-T02:
- `projects.delivery` is `builtin` or `worker`; `worker` names `delivery_storage_id`, the bucket the Worker reads through its R2 binding. `internal/routes` holds the `Delivery` interface: `Builtin` only confirms (it reads Postgres), and `Worker` writes into that bucket. Assets on that bucket get `backend: "r2"`; every other store is `origin`.
- A record exists exactly when the built-in handler wouldn't answer 404: active, asset not deleted, playable. A disabled app's ID keeps its record with `"denied": true`, so the Worker answers 403 like the handler (D016). It is `{"v":1,"app_id","asset_id","output_id","policy","backend","outputs","denied"?}`, where `outputs` maps each committed or retired output to its key version. Pinned paths and keys naming anything else get 404, which is how the Worker keeps the handler's "never a writing output" and key-version checks without Postgres.
- Records are rebuilt from current state when a `route_sync` row is confirmed, never taken from the payload, so out-of-order rows can't publish something stale. A failed write keeps the row pending with `last_error = publish_failed` and backs off (15 s doubling to 10 min). A revoking ID becomes `revoked` only after its delete lands, which keeps D021's 200/202 contract.
- `_keys.json` is `{"v":1,"max_token_lifetime_seconds","keys":[{"kid","app_id"|null,"jwk":{EC P-256},"not_after"|null}]}`. It lists keys not revoked and not past `retired_at`, and `not_after` lets the Worker stop a retiring key on time between rewrites. A new outbox kind, `keys_sync`, is written in the transaction that creates or revokes a key. Pending rows are coalesced into one write per project.
- Disabling or enabling an app queues a `refresh` `route_sync` for each of its live IDs, which rewrites them (denied or not) without asset events.
- `Storage` gains `Delete` (one object; a missing one is fine) for unpublishing.
- The reconciler runs every 15 minutes. It lists `_routes/`, deletes records for IDs that shouldn't be served, rewrites any whose MD5 ETag differs from the bytes Postgres implies, writes missing ones, and rewrites `_keys.json`. A clean pass writes nothing.
- One serve replica runs every background loop, holding the session advisory lock `(0x636d7831, hashtext(current_schema()))` on its own connection, re-checked every 10 s. The schema is part of the key because advisory locks are per database: installs in separate schemas of one database each elect their own leader (the Chromium suites' two installs found this). Serve waits up to 2 s for the election at boot. Every loop was already safe to run twice, so a handover costs duplicate work only.

## D036 The Cloudflare Worker — accepted (2026-11-05)
Decisions within the architecture's rules, made in P2-T03:
- `worker/` is a pnpm workspace package with no runtime dependencies. esbuild bundles it into one ES module, `dist/worker.js`. `src/index.ts` only exports the handler, because workerd refuses any other export from the entry module; the logic is in `src/delivery.ts`, and tokens are in `src/token.ts` on WebCrypto.
- Bindings: `MEDIA` (the R2 bucket of `projects.delivery_storage_id`), `CONTENT_KEYS` (a secret, JSON `{"<version>": "<hex>"}`: the whole ring, D022) and `ORIGIN_URL` (a var, the built-in handler for `backend: "origin"`).
- The checks and responses follow `internal/delivery` in the same order, with the same 401/403/404 JSON bodies, CORS, `nosniff`, Content-Type from the extension and Cache-Control. The `outputs` map stands in for the output query, and `_keys.json` for the key query. A key whose `not_after` has passed is refused, like a retired key.
- Tokens are read as Go reads them: strict base64url (one spelling), exact claim names, the last duplicate winning, `iat` and `exp` as integer literals (`null` reads as 0), and both ECDSA signature forms.
- Routes and `_keys.json` are reused for 60 s per isolate, so revocation reaches the edge within a minute. An unknown ID is remembered for only 5 s, so a newly published ID plays soon.
- Pinned objects (immutable, `max-age=31536000`) go through the Cache API, keyed by storage key, so tokens and playback IDs don't fragment it. The stored copy is public. What a viewer receives says `private` for signed IDs. A ranged request on a cache miss reads that range from R2 and isn't cached. Several ranges get the whole object (the Go handler answers multipart). Masters and keys are never cached.
- The `origin` backend forwards the viewer's path (token included) and Range to `ORIGIN_URL` only after the Worker's own checks pass. Keys are always derived at the edge.
- Tests: `vectors.test.ts` runs every case of `vectors.json` against an in-memory bucket filled from `testdata/worker-bucket.json`. That golden file is what the Go route publisher writes for the same vectors, checked by `TestVectors` (`-update-bucket` regenerates it), so a pass covers database → projection → response. `workerd.test.ts` runs the bundle in workerd through Miniflare 4.20260730.0, with a real R2 binding and the Cache API. All of it runs in CI's `ts` job.

## D037 Connect Cloudflare — accepted (2026-11-05)
Decisions within the architecture's rules and the P2 open questions, made in P2-T04:
- CI runs the Worker on local workerd on every PR. A real-account job, on main, weekly and by hand, comes with the T07 exit test and runs only once the repository has a test zone, a scoped token and Workers Paid as secrets (open question 1).
- Workers Paid is required. Connect reads the account's subscriptions and refuses before changing anything in Cloudflare unless one is a Workers Paid plan (open question 2). The plan is matched by name, `workers` and `paid`, which isn't confirmed against a real account yet (known gap).
- Rotating the content key secret will need a pasted token. That is the same Connect call: it redeploys the Worker with the whole ring, and the new version may be used only after that deploy (open question 3). The rotation command itself is still a known gap.
- Questions 4 to 6: segments are not served straight from an R2 custom domain in Phase 2. The built-in handler's route cache stays a known gap. One advisory-lock leader is D035.
- `POST /v1/cloudflare/connect` (owner session) takes `{token, hostname, bucket?, account_id?}` and runs, in order: verify the token; pick the account (the only one, or the one named); find the active zone holding the hostname; Workers Paid; ensure the bucket (the default is `clipmux-media`, and a created bucket carries the warning that an unscoped token reaches every bucket); derive R2 credentials (token ID, and hex SHA-256 of the value), check them against the bucket and apply CORS, then store or renew them sealed; upload the embedded Worker as `clipmux-media` with `MEDIA`, `ORIGIN_URL` (the public URL) and the `CONTENT_KEYS` secret; set Worker delivery on that bucket, make it the default store and reconcile every route into it; delete the DNS records at the hostname; attach the Worker as its custom domain; save `https://{hostname}` as the media URL; record the connection (token ID only); queue the setup test. It stops at the first failure, and every step is idempotent, so the same call reconnects with a new token and redeploys an upgraded Worker.
- Routes are written before the hostname moves, so the Worker serves from its first request. Existing filesystem assets play through `origin` until a move job (T05).
- The Worker bundle is committed at `internal/cloudflare/worker.js` and embedded. `make worker` rebuilds it, and CI fails when it differs from `worker/`. Its version is the first 6 bytes of its SHA-256; the dashboard asks for a token when the deployed version differs.
- `GET /v1/cloudflare` reports the connection, `needs_redeploy`, and `storage_ok` (a listing of `_routes/` with the stored credentials), which drives "Reconnect Cloudflare". `docs/cloudflare.md` documents the token permissions and the manual wrangler path.
- Tests drive a fake Cloudflare API that checks the bearer token on every call. The R2 side runs on SeaweedFS, under a second dev identity that holds the credentials the fake token derives to. The dev SeaweedFS pins `-ip=127.0.0.1`, so its raft state survives a container restart.

## D038 The move job — accepted (2026-11-05)
Decisions within the architecture's rules, made in P2-T05:
- `move` is a control-plane job like `delete` (D008, D019), claimed by the same loop, and it records `source_storage_id` and `target_storage_id`. The one-active-job index makes it exclusive with uploads' transcode, reencode and delete; a delete cancels it.
- One pass copies `uploads/{id}/` and the prefix of every committed or retired output (never a writing or lost attempt's files), skipping objects already there at the same size, so a crashed pass resumes. Objects over 5 GiB go up through the target's multipart upload in 32 MiB parts. Every size is verified by listing the target. One transaction then switches `storage_id` (only if it is still the source), queues a `refresh` of every live route (its `backend` changes) and `video.asset.updated` with `job: {kind: "move"}`.
- Cleanup is a second phase, at least 2 minutes later (past the Worker's 60 s route cache), once the asset's route rows are confirmed and PUT URLs signed for earlier jobs have expired (`LateWritesUntil`; such a URL can only add a file no manifest names, so it can't affect the copy). It deletes the old prefixes and succeeds the job.
- `POST /v1/storage/moves` (owner) queues a move to the default store for every ready or errored asset elsewhere with no active job. `GET /v1/storage/moves` reports how many assets are still off the default store and the move jobs by state. The dashboard's Delivery page offers the move after Connect Cloudflare and polls progress.

## D039 Import from URL — accepted (2026-11-05)
Decisions within the architecture's rules, made in P2-T06:
- `POST /v1/assets` (`assets:write`, `Idempotency-Key`) takes `{url, playback_policy, passthrough}` and creates a `processing` asset on the default store, its playback ID and an `import` job holding the URL (`jobs.import_url`), in one transaction. It answers `201` with the asset view.
- serve fetches the URL itself in its own loop (`imports`, up to 4 per pass, leased for 6 h 10 min, each fetch bounded at 6 h), not a worker: the control plane already holds storage credentials, and workers never fetch arbitrary URLs. Workers no longer claim `import` jobs. The body streams into `uploads/{id}/source` through `Storage.Put`, capped at the upload limit. One transaction then records the source, succeeds the import and queues the `transcode` job, which runs as it does for an upload.
- SSRF (`internal/publicnet`): only `http` and `https`, with no credentials, a host, and at most 2048 bytes; a literal non-public address is refused at the request with `400 import_url_refused`. The dialer's `Control` hook checks the address actually being connected (after DNS, on every connection and redirect, so DNS rebinding gains nothing) against public unicast. It refuses loopback, private, link-local, multicast, unspecified, CGNAT, reserved and documentation ranges, NAT64, 6to4, Teredo, site-local and ULA, with IPv4-mapped addresses judged as IPv4. There is no proxy, at most 5 redirects (each checked again), a 10 s connect and a 30 s response-header timeout.
- Failures: a refused address is permanent (`import_url_refused`). Another 4xx, an empty body, or more than the limit (`import_too_large`) is permanent. A 5xx, 408, 429, network error or short body is retried after 30 s, then 60 s, and after 3 tries becomes `import_failed`. Permanent failures error the asset with `video.asset.errored` (`job: {kind: "import", error_code}`).
- Agent sources (`agent_source_id`) wait for Phase 3.

## D040 Phase 1 exit — accepted (2026-11-05)
The Phase 1 exit test passed in CI at `14586f5` (visheshgubrani/clipmux#2): CI run 42072842722 (Go, TypeScript, lint, sqlc, toolchain, and the image under compose with the setup test through Caddy), Exit test run 42072842708 (the real binary: signed encrypted playback, a SIGKILLed worker recovering on a new attempt and output, the setup test; the Chromium suites; ExoPlayer on an Android emulator) and Exit test (Safari) run 42072842730 (native HLS with separate audio, and `<clipmux-player>` refresh). Getting there took four CI fixes: building the packages before type-checking, freeing disk for the emulator, the kill-worker test waiting out the first-boot setup test, and the packager in its own image layer, plus bounded install steps. Phase 2 started on its branch before this exit, but nothing of it merged before it. `docs/plans/launch.md` is now unblocked (D005); the owner put Phase 2 first.

## D041 Phase 2 exit test — accepted (2026-11-05)
Made in P2-T07:
- `TestPhase2ExitAppsPlayUnchangedThroughTheWorker` (`internal/e2e`, CI's Linux exit job with SeaweedFS required) runs the real binary. An app (API key, signing key, a playing filesystem asset) is set up on built-in delivery, then Connect Cloudflare runs against a fake Cloudflare API, with R2 played by the test S3 store under the identity the fake token derives. Then:
  - The same token for the same playback ID plays at the edge through `origin`.
  - A new upload with the same key lands in R2 and plays from it (ffmpeg as the HLS client).
  - A segment read straight from the bucket is ciphertext.
  - A revoked ID returns 404 at the edge within 65 s; 60 s was measured, the shipped TTL, with no test knob.
  - After the library move, the old asset's route says `r2`, and with serve stopped it plays from R2 in a fresh isolate.
- The edge is `worker/e2e/harness.ts`: the Worker's own `createWorker` behind a Node HTTP server, its `MEDIA` binding reading the same bucket through a minimal SigV4 client. workerd's local R2 can't share a bucket with serve's S3 writes; the bundle's behaviour in workerd is covered by `worker/src/workerd.test.ts`.
- `CLIPMUX_CLOUDFLARE_API` (the API base) and `CLIPMUX_R2_ENDPOINT` (with `{account_id}`, e.g. the EU jurisdiction's endpoint) are configuration. When the environment pins the URLs, Connect's media URL step passes only if `CLIPMUX_MEDIA_URL` already names the Worker's hostname, and otherwise says to set it.
- The move job's late-write wait moved from the copy to the cleanup (D038). The exit test found it held every move for 16 minutes after the asset's last job, though a late PUT can't affect the copy.
- Not covered here, and required before Phase 2 exits against Cloudflare: the setup test through the Worker (its media URL must resolve to the edge), the Chromium suites against the Worker hostname, and the same run on a real Cloudflare account (D037). These need a test zone, a scoped token and Workers Paid as repository secrets.

## D042 Phase 3 scope — accepted (2026-11-06)
Context: Phase 2 is merged (visheshgubrani/clipmux#3) with its exit proven on the harness only (D041). The owner chose the next phase and its scope; the plan is [plans/P3-pc-agent.md](plans/P3-pc-agent.md).
Decision:
- **Next phase:** Phase 3 (PC agent) follows the architecture's build order. The LMS migration and going public wait.
- **Phase 2 real-account exit:** it becomes Phase 3's task T00, a parallel track. Phase 3 starts now, because it doesn't change delivery and the open run needs Cloudflare secrets, not code. The Phase 2 exit is recorded when T00 passes.
- **Platforms:** agents run natively on Linux (amd64, arm64) or as the existing image in Docker. Windows and macOS use WSL2, Docker Desktop or a VM. Native Windows and macOS agents are out of this phase.
- **Hardware encoding:**
  - **Backends:** `nvenc`, `vaapi` (AMD and Intel on Linux), `cuda-decode` (NVIDIA cards without NVENC) and `cpu`.
  - **Selection:** each backend is proved by a real probe encode.
  - **Fallback:** an encoder failure falls back down that list to the CPU within the same attempt, recorded in the manifest.
  - **Left out:** AMF, Quick Sync and VideoToolbox, which need native Windows or macOS or add nothing over VAAPI on Linux.
- **Pairing:** a single-use pairing code in the Connect my PC command, exchanged for the agent's key in a 0600 config file. This amends the architecture's `--key KEY`, because a key never goes in argv (D012).
- **CLI:** plain `clipmux push` and `clipmux listen --forward-to` land in this phase beside `push --local`.
- **Proof on real hardware:**
  - a self-hosted runner on the owner's Ubuntu machine with an AMD RX 6600 XT (VAAPI and CPU);
  - Modal for NVIDIA (L4 for `nvenc`, A100 for `cuda-decode`);
  - an M4 Mac running the Docker agent for arm64 and a real lid-close suspend.
  - Intel hardware is unavailable, so its real run is a known gap.
Consequence: `--sandbox` becomes the agent default. Unsandboxed agents take pinned and owner-uploaded jobs only, which needs the `agents` check constraint relaxed. No `Executor` interface is added before Phase 4 gives `Wake` a second implementation.

## D043 Pairing PC agents — accepted (2026-11-06)
Decisions within the architecture's rules and D042, made in P3-T01 and T02:
- **Two pairing directions, both approved by the owner before a key exists.**
  - **Dashboard first:** `POST /v1/agents/pairings` mints a code the owner has thereby approved. The PC runs `clipmux worker --server URL --pair CODE` (or the Docker variant the card shows), which exchanges it at `POST /worker/v1/pair`.
  - **Agent first, for the PC the owner is sitting at:** `clipmux worker --server URL` asks `POST /worker/v1/pairings` for a code and a poll token, prints the code and `{public_url}/workers/connect?code=…`, and opens that link in the desktop's browser. The page shows what the machine said about itself (name, hostname, platform, address), and one click on Connect approves it. The agent polls every 2 s and receives its key on the first poll after approval. A signed-out owner is sent through sign-in and back to the same page.
  - So agents arrive `approved`. The `pending` status stays in the schema but nothing writes it, which amends "appears as pending until the owner approves it": the owner approved by minting or clicking.
- **Codes:** 8 characters from an alphabet without look-alikes, shown as `XXXX-XXXX` and accepted in any case with or without the dash. They are stored as SHA-256 and work once, for 15 minutes. An agent-started code can be redeemed only with its poll token, so reading it off the agent's screen doesn't yield the key. At most 10 agent-started pairings may wait at once, because anyone who can reach the server can start one.
- **Keys:** `cmxa_` plus `rand.Text()`, stored as SHA-256 and shown once. The agent writes server, agent ID, name and key to `agent.json` (0600, written atomically) under `CLIPMUX_AGENT_DIR` or the user's config directory, and later runs need no flags. `CLIPMUX_WORKER_KEY` still wins, for the built-in worker.
- **Authentication:** `workerapi.Auths` tries the built-in key, then agent keys. An unknown, revoked or pending agent key gets the same 401. A worker that receives 401 on claim stops (`ErrKeyRefused`), and an agent says it was disconnected.
- **Last seen:** claims and heartbeats update `last_seen_at` and `last_ip` (the client address through trusted proxies), coalesced to one write per 10 s. An agent counts as online when it was seen in the last 60 s.
- **Revocation** (`DELETE /v1/agents/{id}`) fails the agent's attempts that could still write with `agent_revoked`. Their next call is told to stop, and their jobs return to the queue at once without spending a retry budget. The response counts the current outputs the agent produced, for re-encoding.
- **Trust rule:** an agent's claim says whether rlimits, `no_new_privs` and seccomp hold. One that doesn't is untrusted and claims only jobs pinned to it and assets of the built-in Dashboard app (the owner's uploads). The schema's `CHECK (sandboxed OR scope = 'local')` is dropped (migration 14). Agents probe the sandbox by default; `--unsandboxed` opts out, and the error on a host without the sandbox says so.
- **Disk:** claims report `free_disk_bytes` for the scratch directory. A worker that reports it takes only uploads whose declared size, three times over, fits (twice for a source already on the agent). Imports and workers that don't report pass. Workers clear their scratch directory when they start.
- **Backoff:** network errors back off from 1 s to 30 s, down from a minute, so a PC waking from sleep is back within half a minute.

## D044 clipmux push — accepted (2026-11-06)
Decisions within the architecture's rules, made in P3-T04:
- `clipmux push --server URL [--policy signed|public] [--passthrough REF] [--wait] FILE|DIR…` is a client of the public upload API (`internal/push`), with the API key from `CLIPMUX_API_KEY`, never argv. Directories are walked for video extensions, skipping hidden directories. Each file prints `asset_id, playback_id, status, path` on stdout; progress goes to stderr.
- **One push, one identity:** server, absolute path, size, modification time, policy and passthrough. The `Idempotency-Key` is derived from it, so pushing a finished file again replays the create (no token) and returns the same asset without sending a byte.
- **Resume:** the upload's ID and token are kept in a 0600 file under `CLIPMUX_PUSH_STATE_DIR` (default the user's cache directory) until complete. Every run lists stored parts and sends only the missing ones (storage is the source of truth, D031). An upload that expired or was cancelled starts over under a fresh key suffix. A list that answers 409 means the sweeper finished it; completing again returns the asset.
- **Retries:** parts go four at a time. Network errors, 5xx, 408 and 429 retry `min(30 s, 1 s·2ⁿ)` plus up to 1 s of jitter, 6 tries, honouring `Retry-After`. A 403 from storage re-signs the part once. Part URLs are signed in batches of 100 just before use, and again within 5 minutes of expiry. Errors never carry a signed URL.

## D045 push --local and pinned sources — accepted (2026-11-06)
Decisions within the architecture's rules, made in P3-T05:
- **The agent's control socket.** A running agent listens on `agent.sock` (0600) beside `agent.json`. `clipmux push --local FILE|DIR…` hands it absolute paths. The agent resolves symlinks and accepts only an existing, non-empty regular file, inside `roots` from `agent.json` when any are set. Without roots, any file the agent's user can read is allowed, because only that user can open the socket. In Docker, the CLI on the host needs the agent directory bind-mounted and the files at the same paths.
- **Register, then attach.** The agent registers the file with the server by size alone (`POST /worker/v1/sources` → `src_…`) and records path, size and modification time in `sources.json` before any asset exists, so the pinned job it is about to get always finds the file. The asset then comes from:
  - with an API key, `POST /v1/assets {agent_source_id}`, owned by the key's app;
  - without one, the agent's `POST /worker/v1/sources/{id}/attach`, owned by the Dashboard app as the owner's upload. So the owner's own PC needs no key.
  - A source attaches to one asset (a unique index). The asset records `agent://{agent_id}/{source_id}`.
- **Pinned jobs.** Transcodes and re-encodes of such an asset are pinned to the agent. The claim payload carries `{agent_source_id, size}`. The worker reads the file where it lies, and Landlock adds read access to that one file. It checks size and modification time before encoding and again after, so an edited file can't commit a mixed output. A changed or missing file is a permanent `source_changed` or `source_missing`: only the owner, at that PC, can fix it. A ready asset keeps playing.
- **Waiting.** A pinned job waits while its agent is away and spends no budget. A sweeper ends it with `agent_unavailable` once the agent is revoked or unseen for 7 days (`queue.PinnedWait`, the retire window). Re-encoding a revoked agent's source is `409 source_unavailable`.
- **Forgetting.** The agent asks `POST /worker/v1/sources/sync` at start and hourly which sources it may forget: their assets were deleted, they're unknown, or they were never attached a day after registering.
- `--upload-source` isn't built yet (known gap): a local asset re-encodes only on its own PC.

## D046 Hardware encoding with CPU fallback — accepted (2026-11-06)
Decisions within the architecture's rules and D042, made in P3-T03:
- **Backends** (`internal/media/encoders.go`), shared by agents and the built-in worker:
  - `nvenc`: CUDA decode, `scale_cuda`, `h264_nvenc`.
  - `vaapi`: VAAPI decode, `scale_vaapi`, `h264_vaapi`; AMD radeonsi and Intel iHD.
  - `cuda-decode`: NVIDIA without NVENC. `-hwaccel cuda` with frames back in system memory and libx264. ffmpeg decodes on the CPU itself if the GPU can't.
  - `cpu`: libx264.
- **Detection:** a backend is offered only after a real two-frame encode when the worker starts, under the same sandbox jobs use. `ffmpeg -encoders` proves nothing: it lists `h264_nvenc` on this AMD machine, where an encode fails. VAAPI tries each `/dev/dri/renderD*` unless `CLIPMUX_VAAPI_DEVICE` names one. `--encoder` / `CLIPMUX_ENCODER` (`auto`, `nvenc`, `vaapi`, `cuda-decode`, `cpu`) names the first backend to try, and a failed test encode logs a warning. Claims report the proven encoders and the hardware.
- **The chain:** each GPU backend decodes on the GPU when the source is a common codec (H.264, HEVC, VP9, AV1, MPEG-2) in 8-bit 4:2:0. Otherwise it decodes on the CPU and encodes on the GPU (a `-hybrid` stage). The CPU comes last.
  - A stage that fails hands the whole ladder to the next stage in the same attempt, so renditions never mix encoders and the attempt spends no retry budget.
  - The CPU stage's failure is final (`transcode_failed`, or `unsupported_source` from the probe), and a cancelled context stops the chain.
  - Every GPU failure falls through rather than being classified from stderr. A corrupt source that got past the probe therefore costs extra encodes, but nothing is mislabelled.
  - `manifest.json` records `encoder: {stage, fallbacks}`.
- **Parity:** every backend gets the ladder's bitrate, maxrate and buffer, high profile, CFR at the source rate, `-g` of 6 s, `-force_key_frames` every 6 s and closed GOPs. Scene-cut keyframes are off: `-sc_threshold 0` for libx264, `-no-scenecut 1 -forced-idr 1 -strict_gop 1` for NVENC.
- **Sandbox (D027):** a GPU stage adds `--gpu` and `--device` for that backend's nodes only (`/dev/dri/renderD*` for VAAPI, `/dev/nvidia*` for CUDA). Landlock then allows read, write and ioctl on those nodes and read on `/sys` and `/proc/driver`. The memory limit moves from `RLIMIT_AS` to `RLIMIT_DATA`, because drivers reserve far more address space than they use. Seccomp still refuses `socket()`. Only `LIBVA_DRIVER_NAME`, `LIBVA_DRIVERS_PATH`, `CUDA_VISIBLE_DEVICES` and `CUDA_DEVICE_ORDER` reach a tool's environment.
- **Proof:**
  - On the owner's RX 6600 XT, under the full sandbox, `vaapi` encodes the 1080p ladder with every rendition's segments identical (6.006 s at 29.97 fps). A 10-bit 4:2:2 source takes `vaapi-hybrid`. The golden 480p segment has an SSIM of 0.994 against the CPU encode.
  - `.github/workflows/gpu.yml` reruns that on a `[self-hosted, linux, gpu, vaapi]` runner whose user is in the `render` group.
  - NVENC and `cuda-decode` are covered by argument and fallback tests only, until a Modal run on an L4 and an A100 (known gap).
- The image gains `intel-media-va-driver`. `mesa-va-drivers` and the toolchain check for `h264_nvenc`, `h264_vaapi` and `scale_cuda`'s `format` option were already in place.

## D047 Workers and Jobs pages — accepted (2026-11-06)
Decisions within the architecture's rules, made in P3-T06:
- **Workers page:** a "Connect a PC" card first. It shows the one command for the PC the owner is using (`clipmux worker --server {origin}`, approved on `/workers/connect`), and "Create a command" for any other machine, with Linux and Docker variants and a live "PC connected" once the code is redeemed. Then every agent, showing online (seen in the last 60 s) or when it was last seen, hostname, platform, address, proven encoders, sandbox status and scope. A sandboxed agent can take "any job" or "its local files only"; an unsandboxed one, "your uploads" or "its local files only". The page refreshes every 10 s.
- **Revoking** reports how many videos currently play an output the agent encoded. A revoked agent offers "Re-encode its videos" (`POST /v1/agents/{id}/reencode`), which re-encodes those assets as their own apps. Assets whose source stayed on that PC, or that have a job running, are skipped and counted.
- **Jobs page:** `GET /v1/jobs` keyset-pages jobs across the library and loads every listed job's attempts in one query. Each attempt shows where it ran (built-in, or the agent's name), how it ended, and the encoder stage and fallbacks from its output's manifest. Jobs pinned to a PC say they're waiting for it. The page follows running jobs every 3 s. This closes "no jobs or workers pages".

## D048 clipmux listen — accepted (2026-11-06)
Decisions within the architecture's rules, made in P3-T07:
- **The CLI reads the outbox through the API.** `GET /v1/events` returns the API key's app's webhook events, the same outbox rows the publisher fans out (D018), as `{id, created_at, body}` with `body` byte for byte what an endpoint receives. It needs `assets:read`, not a new scope, because an event's body is the asset view that scope already reads. No endpoint, delivery row or relay state is created on the server, and integration keys still can't manage endpoints.
- **Polling:** it long-polls (25 s) until an event newer than `after` exists. It returns everything from `since`, so the CLI re-reads a 30 s overlap and catches events committed out of order, then deduplicates by event ID. A full page (100) advances without overlap so it can't loop, and an idle window doesn't move.
- **Forwarding:** `clipmux listen --server URL --forward-to localhost:3000/webhooks [--events …] [--since 10m]` POSTs each event with D018's headers (`X-Webhook-Event`, `-Id`, `-Timestamp`, `-Signature`) and `User-Agent: ClipMux-Listen/1.0`. It signs with a secret the developer sets as their app's webhook secret: `CLIPMUX_LISTEN_SECRET`, or a `whsec_` secret generated once and kept (0600) beside the agent config, so it stays the same between sessions. A local 5xx or refused connection is retried three times (1, 2, 4 s). A 4xx from the API ends the relay; anything else backs off up to 30 s.

## D049 Running an agent — accepted (2026-11-06)
Decisions within the architecture's rules, made in P3-T08:
- **The Docker agent:** the existing image is the agent (`clipmux worker`), and no second image is built. The command the Workers page shows does three things:
  - It runs as the host user with `--no-healthcheck`, because the healthcheck is serve's.
  - It bind-mounts `~/.config/clipmux` as the agent directory, so a host `clipmux push --local` finds the container's socket and the connection survives restarts.
  - It keeps `--pair CODE`. A run whose agent directory already holds a connection to the same server ignores the spent code, so `--restart unless-stopped` can't loop on it.
  The image creates `/var/lib/clipmux-agent` (0700). GPUs need `--gpus all` (NVIDIA) or `--device /dev/dri` with the render group (AMD, Intel); the card and `docs/agents.md` say so.
- **`clipmux worker doctor`** checks, in order: the saved connection, the server's `/readyz` (and clock skew from its `Date`), the key (an empty `sources/sync`, which changes nothing), the tool versions, the sandbox probe, a test encode per GPU path, and scratch space. A ✗ exits 1. A missing sandbox, no GPU or less than 10 GiB free is a note, not a failure.
- **Native Linux** is the `clipmux` binary with an FFmpeg that has the encoders and the pinned packager (`toolchain/install_packager.sh`); `doctor` verifies them. A self-contained tarball with FFmpeg's shared libraries and VAAPI drivers waits for launch, when releases are published (known gap).

## D050 Phase 3 exit test — accepted (2026-11-06)
Made in P3-T09. `internal/e2e/agents_db_test.go` runs the real binary, with serve's built-in worker off and agents paired by `clipmux admin create-pairing` (new; also for servers without a browser). Each agent runs in its own process group with `CLIPMUX_ENCODER=cpu`, and the shipped 90 s lease and 15 s sweep apply, with no knob. It passed locally at the first try (233 s):
- **A sleeping PC recovers its job.** SIGSTOP of the agent's group mid-encode, until the sweeper marks the attempt lost, then SIGCONT. The same attempt commits with `lost_count` 1, and the video plays.
- **A sleeping PC's job moves.** A second paired agent takes the job on a new attempt and output, and the asset becomes ready. The woken laptop is told to stop, and its attempt never commits.
- **A local push uploads only outputs.** `clipmux push --local` without an API key leaves no `uploads/{asset}/` object and no upload row, the video plays, and a re-encode is pinned to and runs on the same PC.
- CI runs these in the Linux exit job (timeouts raised to 45 and 55 minutes).
Still open before Phase 3 is closed on hardware: the closed-lid suspend on the M4 Mac with the Docker agent (by hand), the Modal run of NVENC and `cuda-decode`, and registering the self-hosted GPU runner. The Workers and Jobs pages have no Playwright suite yet.

## D051 NVIDIA proven on Modal; CUDA's IPC sockets — accepted (2026-11-06)
Amends D027 and D046, from the Modal run of P3-T03:
- **NVIDIA failed inside the sandbox at first.** The first run on an L4 and an A100 found no GPU path inside the sandbox, while the same probes passed unconfined. `strace` showed why: `cuInit` opens `socket(AF_UNIX, SOCK_SEQPACKET)` for the driver's own IPC, and seccomp refused every `socket()`, so CUDA reported `CUDA_ERROR_OPERATING_SYSTEM`.
- **The narrowed rule:** a GPU stage's seccomp filter (`--gpu`) now allows exactly `socket(AF_UNIX, SOCK_SEQPACKET)`, matching the type without its flags.
  - Every other family stays refused (INET, INET6, NETLINK…), and so do stream and datagram Unix sockets, the types local services such as Docker and D-Bus listen on.
  - CPU tools still can't open any socket.
  - `TestGPUToolsGetCUDAsIPCSocketsAndNoOthers` holds all three rules. AMD's VAAPI needed no socket and still passes.
- **Proof** (`deploy/modal/gpu_test.py`, run with `uvx --from modal modal run deploy/modal/gpu_test.py`):
  - It builds the pinned toolchain on Modal and runs the media test binary.
  - NVENC accepts any of an L4, A10G or T4, whichever has capacity (the L4 queue stalled). On an A10 it was detected (`nvenc, cpu`), encoded the golden ladder sandboxed with keyframes aligned and segments decrypting, and scored an SSIM of 0.996 against the CPU encode.
  - On an A100, NVENC correctly failed its probe and the card was offered as `cuda-decode`, at an SSIM of 0.998.
  - The harness also prints each probe unconfined and sandboxed, plus the refused calls CUDA makes, for the next driver that needs something.
- **Modal's sandbox:** it runs gVisor without Landlock (ABI 0, reported, not required). rlimits, `no_new_privs` and seccomp hold there.

## D052 Phase 4 scope — accepted (2026-11-06)
Context: Phase 3 is merged (visheshgubrani/clipmux#4) with its exit proven on the real binary (D050) and NVIDIA proven on Modal (D051). The owner chose the next phase and its scope; the plan is [plans/P4-serverless.md](plans/P4-serverless.md).
Decision:
- **Next phase:** Phase 4 (Serverless) follows the architecture's build order. The LMS migration and going public wait.
- **Providers:**
  - RunPod uses on-demand pods through its REST API. It comes first, because its fake drives the CI exit test.
  - Modal uses Sandboxes through its Go SDK.
  - Both are proven on a real account in `exit-providers.yml`.
- **The worker image:**
  - CI pushes `ghcr.io/visheshgubrani/clipmux:sha-{commit}` on main as a private package, and providers pull it with a credential sealed by Connect. This amends D005, D006 and D028, which kept the image unpushed.
  - The package goes public at launch.
- **`Executor`:**
  - Lands with `Wake`, `Stop`, `Running` and `Check`. This amends the architecture's `Available`, `Wake` and `Capabilities`: availability is router logic, and `Stop` and `Running` keep a crash from leaving a paid machine running.
  - Wakes run from the outbox, and the one-time key is minted in the routing transaction.
  - One-time keys are the third `WorkerAuth`.
- **Fallback rule:** fallback is for missing capacity, never for a failing file. Only infrastructure failures and lost leases on an asset leave it eligible, so a corrupt source can't reach a paid machine, even on re-encode.
- **Carried from Phase 3:**
  - the Playwright suite for the Workers and Jobs pages;
  - `push --local --upload-source`;
  - the self-contained agent tarball. This amends D049: built in CI now, published at launch.
- **Parallel track (T00):** Phase 2's real-account run, and Phase 3's by-hand checks (the M4 Mac's lid-close, registering the GPU runner). Each exit is recorded when it passes.
Consequence: `jobs.routed_to` changes meaning from "who claimed it" to "where it is routed while queued", and attempts record who ran them and why. The default primary, `workers`, keeps today's behaviour on upgrade.

## D053 Routing: a primary, a fallback, and no fallback for a failing file — accepted (2026-11-06)
Decisions within the architecture's rules and D052, made in P4-T01:
- **Settings** (`projects`, `GET`/`PUT /v1/settings/routing`, owner session):
  - `primary_executor` is `workers` (the built-in worker and PCs compete, as before; the default), `agents` (PCs only, so the built-in worker idles), or `builtin`.
  - `fallback_executor` is null or `builtin`. A fallback can't be part of the primary (the schema refuses `builtin` under `workers`).
  - `queue_age_fallback_minutes` is null (off, the default) or 1 to 1440, and needs a fallback.
  - `modal` and `runpod` are in the schema but refused with `409 executor_not_connected` until P4-T04 and T05 can connect them.
  - "My PC, else the server" is `agents` with fallback `builtin`. This answers P3 open question 2; `CLIPMUX_BUILTIN_WORKER=false` still removes the built-in worker entirely.
- **`jobs.routed_to` names where a queued media job is routed**, no longer who claimed it.
  - `CreateJob` stamps it, reading the primary in the same statement: `agents`/`pinned` for a pinned job, otherwise the primary with `primary`. Import, move and delete jobs carry no route (`jobs_media_routed` holds this). Migration 17 backfilled existing jobs to `workers`/`primary` or `agents`/`pinned`.
  - `StartJob` no longer writes it. `attempts.route_reason` copies the reason at claim, and `attempts.executor` says who ran it.
  - A worker claims only jobs routed to its class: the built-in worker `workers` or `builtin`, an agent `workers` or `agents`; pinned jobs are decided by the pin alone.
- **The router** (`queue.Route`, a leader loop every 5 s) re-evaluates queued, unpinned media jobs whose `available_at` has passed, each in its own transaction under its row lock, and writes only a change. In order:
  - the primary has an online worker that could take the job: `primary`, unless queue age is on, `now() − available_at` has passed it and the fallback could take the job: `queue_age`;
  - otherwise the fallback could take it: `primary_offline`;
  - otherwise it waits on the primary with `no_fallback`, `fallback_refused` or `fallback_offline`.
  
  A job on the fallback that nobody has claimed returns to the primary as soon as the primary is back. Lost leases, transient failures and revocations requeue the job, so each retry is routed afresh.
- **Online means a worker that could claim this job**, seen in the last 60 s (`queue.OnlineWithin`, which the Workers page shares). The claim's per-worker rules (pin, local scope, the unsandboxed trust rule, free disk) moved into a SQL function, `job_claimable_by`, which both the claim and the router call. An unsandboxed PC therefore isn't a primary for a public upload. The built-in worker's claims and heartbeats now record `projects.builtin_seen_at` and its free disk, coalesced like agents.
- **No fallback for a failing file.** A job may go to a fallback only while every failed attempt on its **asset** failed with a capacity code: `agent_revoked`, `source_download_failed`, `upload_failed` or `scratch_full` (and `wake_*` once providers exist). Lost leases don't count against it. The bar is per asset, so a re-encode of an asset whose file failed stays off the fallback too.
  - The worker now reports `scratch_full` when its work directory can't be made, and `output_invalid` when commit refuses the output (it was `commit_failed`).
  - `transcode_failed`, `output_invalid`, `job_timeout` (P4 open question 3: an encode that runs six hours is suspicious), `commit_failed` and `processing_failed` all bar it. A barred job whose primary never returns waits without a bound, visible as `fallback_refused` (known gap).
- **The corrupt fixture.** `internal/media/testdata/corrupt.mp4` (`toolchain/make_corrupt.sh`, 52 KB) has intact MP4 boxes and AES-CTR noise for every sample byte: ffprobe reports a 320×240 video, and ffmpeg exits 69 without a frame on FFmpeg 6.1 (CI's apt build), 8.0 and 9.0.1 (the image). `TestCorruptFixturePassesTheProbeButFailsTheEncode` holds it with the real tools.
- Tests: the routing table as a pure function, one case per rule; on real Postgres, the router with real liveness, a lost lease moving to the fallback, queue age, the trust rule, the per-asset bar for each code, pinned jobs and free disk; the routing API's refusals; the built-in worker's liveness through the worker API.

## D054 Dispatches and one-time keys — accepted (2026-11-06)
Decisions within the architecture's rules and D052, made in P4-T02:
- **Providers are rows.** `executor_configs` holds one row per connected provider (`modal`, `runpod`) like `storage_configs`: `config` (image, GPU, region), credentials sealed under `executor_configs/{id}`, `max_concurrent`, `monthly_limit_cents`, `rate_cents_per_hour`, the machines' `disk_bytes` and `claim_deadline_seconds` (default 600). Connect writes it (T04, T05). `verified_at` is set when a setup test through that provider passes, and the router sends a provider work only after that.
- **Routing to a provider creates a dispatch in the router's transaction** (`queue.Queue.Dispatch`, set to `dispatch.Service.Route`). A provider counts as up for a job when it is verified, its disk fits the job (`job_claimable_by`, D053), and serve can wake it. The dispatch copies the provider's rate and gets a claim deadline; a `wake` outbox row asks for the machine.
  - **The one-time key is minted there**, `cmxd_` + `rand.Text()`, and stored twice: SHA-256 for authentication, and sealed (`dispatches/{id}`, D017) so a retried wake hands the same key to the same machine. Minting it in the publisher would have left the hash invisible until commit, and a crash would have given the retry a new key the first machine never got.
  - At most one dispatch per job is live (`dispatches_one_live_per_job`). A job with a live dispatch is left alone by the router: a woken machine keeps its job until it claims or the dispatch ends.
- **The publisher** (`dispatch.Service.Publish`, a leader loop every 2 s) locks pending `wake` and `stop` rows and, like the route publisher, calls the provider while holding the row and rebuilds from current state. A wake whose job is no longer queued and routed to that provider ends the dispatch `job_gone` without waking anything. Otherwise it unseals the key and calls `Wake` with the machine's whole environment, `CLIPMUX_SERVER` (the public URL) and `CLIPMUX_WORKER_KEY`, never argv. A failure keeps the row pending as `wake_failed` or `stop_failed`, backing off from 5 s to 5 minutes. `dispatch.End` ends a dispatch and queues a `stop` only if its machine was started.
- **`Executor`** has only what this task calls: `Wake(ctx, Wake) (instanceID, error)`, idempotent on the name `clipmux-{dispatch_id}`, and `Stop(ctx, instanceID)`. `Running` arrives with the reconciler (T03) and `Check` with Connect (T04), per "no speculative interfaces". `dispatch.Service.Resolve` opens the adapter for a config.
- **Claiming with a key.** `dispatch.Service` is the third `WorkerAuth`. A key authenticates only while its dispatch is `waking` and before its claim deadline, and yields a worker whose executor is the provider and who may claim only that dispatch's job (`LockDispatchedJob`, then `LockWakingDispatch`: job, then dispatch). The claim marks the dispatch `claimed` with its attempt, so the key is spent; the attempt records `dispatch_id`. `Release` (an undelivered payload, D016) puts the dispatch back to `waking`.
- **Serverless claims need the sandbox:** `409 sandbox_required` unless rlimits, `no_new_privs` and seccomp hold. `clipmux worker` with a `cmxd_` key confines its tools always, refuses `--unsandboxed`, runs one job, and keeps asking through errors (1 s doubling to 30 s) and empty polls until it has run that job or its key is refused (`Worker.RunDispatched`). So a machine started while serve restarts still claims.
- **The setup test through a provider.** `setup_tests.executor` names where a test's sample must run. `CreateJob` routes the sample there with reason `setup_test`, and the router keeps it there whether or not the provider is verified (if it is configured). `QueueThrough` returns `setuptest.ErrBusy` while a test going another way is active. The encode check fails as soon as the sample's latest dispatch ends unclaimed. A pass sets the provider's `verified_at`. A new trigger, `executor_connected`, is for Connect.
- Tests (real Postgres, a fake provider): a routed job wakes one machine with the right name, environment and config, and its key claims that job once and nothing else; the key isn't stored in the clear; a second router pass makes no second dispatch; a key past its deadline is refused; a retried wake reuses name and key; a cancelled job is never woken and queues no stop; a released claim leaves the dispatch claimable; ending a woken dispatch stops its machine and lets the router wake another; an unverified provider takes only its setup test, which nobody else may take. The worker API refuses an unsandboxed serverless claim, and the worker retries 503 and 204 until 401.

## D055 Fallback limits, the reaper and the machine reconciler — accepted (2026-11-06)
Decisions within the architecture's rules and D052 (answering P4 open question 1), made in P4-T03:
- **Room.** A provider takes a job only while it has room (`provider_has_room`, a SQL function the router's facts and the dispatch share):
  - it isn't backing off after failed wakes;
  - fewer of its machines than `max_concurrent` may still be running (a machine counts until it is stopped, not just until its dispatch ends);
  - this month's spend plus the job's reservation is within `monthly_limit_cents`.
  
  `dispatch.Route` re-checks room under a row lock on the provider's config before creating a dispatch, so parallel routers can't take the same last slot or the last of the budget. Without the lock the test saw 5 machines against a limit of 2.
- **Spend** (`provider_month_cents`) is an estimate, as the architecture says: each dispatch created this calendar month (UTC) costs its copied hourly rate from wake until its machine was stopped, or until now while it may still run. A dispatch that hasn't ended counts at least its **reservation**: rate × `job_estimated_seconds`, an hour per 4 GiB of source, at least 15 minutes, at most `MaxLifetime` (6 h 15 min); a source of unknown size counts as an hour. The reservation stops counting when the dispatch ends and the real run counts instead. Running machines are never stopped at the limit; the worst overshoot is a machine running past its reservation.
- **Reasons.** The routing table gains `fallback_limit` (the primary is offline and the fallback provider has no room: the job waits for the primary) and `primary_limit` (a provider primary has no room: the job waits there, or goes to the fallback by queue age).
- **Failed wakes end the dispatch** (`wake_failed`) instead of retrying it, which amends D054. The job is routable again at once with no budget spent. `executor_configs.wake_failures` counts failures in a row, and the third sets `backoff_until` five minutes ahead and resets the count; a successful wake resets it. A crash while waking still replays the same row with the same name and key (D054). Stops keep retrying until they land.
- **The reaper** (`dispatch.Service.Reap`, a leader loop every 30 s) ends a live dispatch and queues its machine's stop when:
  - its attempt is no longer running (committed, failed, or lost: a silent serverless machine is presumed gone): `attempt_ended`;
  - its job is no longer queued before the claim: `job_gone`;
  - its claim deadline passed: `wake_timeout`, with no budget spent;
  - it is older than `MaxLifetime`: `max_lifetime`.
  
  It decides again under the dispatch's lock, so a claim that wins the race keeps its machine. Ending a dispatch makes its job routable again.
- **The reconciler** (`dispatch.Service.Reconcile`, at boot and every 15 minutes) asks each provider for its running ClipMux machines (`Executor.Running`, new) and stops any that no unstopped dispatch accounts for. A dispatch accounts for a machine by instance ID, or by name (`clipmux-{dispatch_id}`) while its wake may be in flight. That covers a crash between starting a machine and recording it, an ambiguous failed wake that started one anyway, and a stop that was recorded but didn't take.
- Tests on real Postgres with the fake provider: each reaper reason ends the dispatch and stops its machine, once; a working machine is left alone; a wake timeout spends no budget and wakes another; three failed wakes back the provider off and jobs wait with `fallback_limit`; a replayed wake keeps name and key; the reconciler stops a crash's machine and a lost stop's machine but neither a live nor an in-flight one; parallel routers hold `max_concurrent`; the monthly limit counts reservations, real runs and only this month.

## D056 RunPod — accepted (2026-11-06)
Decisions within the architecture's rules and D052, made in P4-T04 (answering P4 open question 4):
- **Pods through the REST API** (`internal/runpod`, base `https://rest.runpod.io/v1`, checked against RunPod's API reference): `POST /pods`, `GET /pods?name=`, `DELETE /pods/{id}` (a 404 is "already gone"), `POST`/`DELETE /containerregistryauth`. A bearer key; errors carry the operation and status only, because a response may echo a key or a password.
- **A pod per dispatch.** `Wake` creates a GPU pod running the image's entrypoint (`clipmux`) with `dockerStartCmd: ["worker", "--once"]`, the dispatch's environment plus `NVIDIA_DRIVER_CAPABILITIES=compute,video,utility`, the configured container disk, no volume, no ports, the stored registry auth, on-demand rather than spot. It first looks the pod up by name, so a replayed wake finds the pod it started. RunPod restarts an exited container, so the reaper's stop (D055) is what ends a pod.
- **Pods are scoped to the install:** named `clipmux-{tag}-{dispatch}`, where `tag` is 8 random characters chosen at the first Connect and kept on reconnect. `Running` lists only pods with this tag, so the reconciler never stops another install's pods or the owner's own.
- **Secure Cloud only** (open question 4). A worker sees the plaintext source and that output's key, and Community Cloud hosts are strangers' machines, so `cloud_type` is fixed to `SECURE` and not offered as a choice.
- **Default GPUs** are cards with NVENC (L4, RTX A4000, RTX A5000, RTX 4090), whichever has capacity; data-centre cards without NVENC would encode on the CPU and cost more.
- **Connect RunPod** (`POST /v1/executors/runpod/connect`, owner session) runs `credentials` (an authenticated pod listing), `public_url`, `registry` (RunPod stores the ghcr credentials; the password never reaches Postgres), `image`, `saved` and `setup_test`, stopping at the first failure. A monthly limit is required and the rate is the owner's: ClipMux keeps no price table, because RunPod's prices change and the owner sees them on the GPU they pick. Connecting again keeps the row and its tag, rotates the sealed key, replaces the registry credentials (removing the old ones), and clears `verified_at`.
- **Provider-agnostic parts live in `dispatch`** (`Save`, `Statuses`, `Disconnect`, `CheckPublicURL`), because Modal needs the same (T05).
  - `CheckPublicURL` resolves the public URL's host and requires every address to pass `publicnet.Public`. It is separate from `settings.KeysAllowed`, which keeps accepting LAN installs. `CLIPMUX_EXECUTORS_ALLOW_PRIVATE_URL=true` skips it for development and the exit test, whose "pods" run on the same machine.
  - `Disconnect` refuses while routing names the provider (`409 executor_in_use`) or while a dispatch is live or a machine unstopped (`409 machines_running`), because nothing could stop that machine afterwards. It clears the credentials and keeps the row, whose dispatches are spend history; the reconciler skips disconnected providers.
- **Routing** may name a provider only once it is connected and verified (`providers_ready` in `GET /v1/settings/routing`).
- **Configuration:** `CLIPMUX_RUNPOD_API` (the fake's base in tests) and `CLIPMUX_WORKER_IMAGE` (until builds carry their image reference, T06).
- **`runpodtest`** is a fake REST API: pods and registry auths in memory, the bearer key checked on every call, every request recorded. With `Start` set it runs each pod, which the exit test (T10) uses to drive serve's real adapter with local workers.
- Tests: the exact pod request; a replayed wake reusing the pod; stop idempotent; `Running` ignoring other installs' and the owner's pods; no key in errors; Connect's step order, sealed key and password, a working stored key, rotation, refusals of a bad key, a private URL and a missing limit; disconnect's refusals; routing accepting RunPod only once verified. The cold start (open question 2) needs a real account and is measured in T10.

## D057 Modal — accepted (2026-11-06)
Decisions within the architecture's rules and D052, made in P4-T05:
- **The SDK is `github.com/modal-labs/modal-client/go` v0.11.1** (2026-10-16), the newest release at least two weeks old. The plan named `libmodal`'s `modal-go`, but its last release (v0.7.3) marks the module deprecated in favour of this one. It brings gRPC and protobuf as dependencies; their versions are the ones the SDK's release requires.
- **A Sandbox per dispatch**, in this install's Modal app `clipmux-{tag}` (created on first use; `tag` is 8 random characters kept across reconnects). It runs `/usr/local/bin/clipmux worker --once` from the registry image, because Modal runs a sandbox's command as given rather than the image's entrypoint. It gets the dispatch's environment plus `NVIDIA_DRIVER_CAPABILITIES=compute,video,utility`, the configured GPU (default L4), CPU (4) and memory (16 GiB), a timeout of `MaxLifetime`, and the tags `clipmux={tag}` and `clipmux_dispatch={id}`. `Wake` first looks the sandbox up by those tags, so a replayed wake finds it. `Running` lists the app's sandboxes with the install tag, and `Stop` terminates by ID, with "not found" counting as stopped.
- **Registry credentials** are sealed in `executor_configs` with the token. The SDK can only create ephemeral secrets (`Secrets.FromMap`) or reference named ones, so each wake passes them as an ephemeral secret (`REGISTRY_USERNAME`, `REGISTRY_PASSWORD`). No named secret is left in the owner's Modal workspace.
- **One client per stored config.** Each SDK client holds a gRPC connection, so `modal.Connector.Resolve` keeps one per config row and replaces (and closes) it when the row's `updated_at` changes. Disconnect closes them all. The token overrides any `~/.modal.toml` profile of the user serve runs as.
- **Connect Modal** (`POST /v1/executors/modal/connect`) runs `credentials` (opening the app with the token), `public_url`, `image`, `saved` and `setup_test`. Errors are redacted of the token secret in case Modal echoes it. Disk is declared (default 50 GB) for the free-disk rule, like RunPod's.
- **Shared with RunPod:** Connect's step runner moved to `dispatch` (`Step`, `Result`, `Steps`), and `manage` serves both connects through one generic handler, per "duplicate once before abstracting".
- **The SDK is tested through a seam.** It speaks gRPC to Modal and has no local fake, so the executor talks to a three-method `api` (create, find, terminate). Tests replace that with a recorder; the real SDK runs only in the real-account workflow (T10). The tests cover the exact sandbox request, a replayed wake, install-scoped listing and stop, Connect's steps, sealed secrets, one client per row and its replacement on reconnect, a bad token without its echo, and disconnect closing clients.
- To confirm on the real account (T10): that Modal ignores the image's entrypoint as assumed, and the sandbox's cold start against the claim deadline.

## D058 The worker image in a private registry — accepted (2026-11-06)
Decisions within D052, made in P4-T06:
- **CI pushes what passed.** The image job builds with `VERSION={commit}` and `WORKER_IMAGE=ghcr.io/visheshgubrani/clipmux:sha-{commit}`, runs the compose stack, the sandbox checks and the setup test, checks that the binary names its own image, and only then, on a push to main, logs in with the job's `GITHUB_TOKEN` (`packages: write`, that job only) and pushes `sha-{commit}`. Pull requests build the same way and never push. The package inherits the private repository's visibility until launch. This amends D005, D006 and D028's "never pushed"; `/server` and `/web` stay unused (D006).
- **The binary carries its image.** `-X main.workerImage=…` sets the default for `CLIPMUX_WORKER_IMAGE`, so Connect's default image is the one built from the server's own commit, and provider machines speak its worker API. The environment variable still overrides it.
- **Connect pins the digest** (`internal/registry`, `dispatch.PinImage`). It resolves the image through the OCI distribution API with the registry credentials the machines will pull with: an anonymous request, then the registry's Bearer challenge answered at its token realm with basic auth (refusing a realm on another host over plain http), or Basic auth. It accepts image indexes first, so a multi-platform tag pins to its index as docker does, and takes the digest from `Docker-Content-Digest` or else from the body. The stored image is `host/repo@sha256:…`, so every machine runs exactly what was connected, even if the tag is pushed again. A pinned reference is still fetched, because the step's other job is proving the credentials can pull.
  - For RunPod, `image` now runs before `registry`, so credentials that can't pull fail before anything is stored in the RunPod account.
  - Whether RunPod and Modal accept a digest-only reference is confirmed by the real-account run (T10).
- **`NVIDIA_DRIVER_CAPABILITIES=compute,video,utility`** is set in the image, not only by the adapters, so `docker run --gpus` agents get NVENC too.
- **`registrytest`** is a fake private registry over TLS with ghcr's token flow and one set of credentials. Connect's tests in both providers, and the exit test, use it.
- **A flaky toolchain check fixed.** `verify_toolchain.sh` piped each captured listing into `grep -q` under `pipefail`. `grep -q` exits at its first match, and if `printf` was still writing it got `SIGPIPE`, so the check reported a present filter as missing. That depended on timing: building the image on a loaded machine failed 10 checks at once, including `scale_cuda` reported missing beside a passing `scale_cuda` option check. Listings now reach grep through here-strings. Five runs in a row pass in the existing image.
- **A limit race fixed (amends D055).** Under load, parallel routers started 5 machines against a limit of 2, in 4 runs out of 10. The room check sat in the same statement as the row lock (`SELECT …, provider_has_room(…) … FOR UPDATE`). Under READ COMMITTED that statement's snapshot is taken before it waits for the lock, so the check missed the dispatches the previous holder had just committed. `dispatch.Route` now locks the provider's row, then checks room in a second statement, whose snapshot follows the lock. The concurrency test now runs 10 jobs through 8 parallel routers and passed 15 runs out of 15; D055's mutation check had missed this because the race needed load to show.
- Tests: references parsed as docker parses them; a private image pinned only with the right credentials, a wrong password, no credentials and an unknown tag all denied; a pinned reference checked again; each provider's Connect storing the pinned image, and RunPod storing no registry auth when the credentials can't pull. The image built locally with the new build argument names its image and carries the NVIDIA setting.

## D059 push --local --upload-source — accepted (2026-11-06)
Decisions within the architecture's "Local sources" and D045, made in P4-T07 (a Phase 3 carry-over, D052):
- **Encode first, upload after.** The flag rides through the agent's socket to `POST /worker/v1/sources {upload_source}`, stored on `agent_sources`. The encode runs on the PC as before, so the video plays without waiting for an upload. In the commit of that encode (`queue.CommitAttempt`, the same transaction), an `upload_source` job is queued for any asset whose agent source asked for it. It is pinned to the agent and routed `agents`/`pinned` (a check constraint holds both), and it shares the one-active-job rule, so a re-encode asked for meanwhile gets `409 job_active`.
- **The upload is an attempt like any other:** leased, heartbeated, fenced. `POST /worker/v1/attempts/{id}/source-parts` starts the attempt's multipart upload of `uploads/{asset}/source` once, under the attempt's lock, keeping its ID and part size on the attempt. It then signs at most 100 parts per call, within the file's registered size. The worker PUTs each part from the file where it lies (no copy, retried three times), checks size and modification time again, and finishes with `complete {source_parts}`. Part URLs aren't create-once: completing names the parts, and a part written twice is the same bytes.
- **Commit** completes the multipart upload and checks the stored size against the registered one (`409 source_invalid`). In the attempt's commit it sets `source_uri` to the storage key, clears `agent_source_id` and marks the source kept. A retry after storage finished but the commit didn't finds the object already there. From then on re-encodes are unpinned and route normally, fallback included, and the agent forgets the source at its next sync.
- **Failure changes nothing visible.** A file that changed or vanished fails the job permanently (`source_changed`, `source_missing`), and a spent budget fails it too. `endJob` leaves an `upload_source` job's asset untouched and sends no webhook: the video plays on, and its source simply stays on the PC.
- Tests: the commit queues the upload only when asked, and only the holding PC claims it; a failed upload sends no event and doesn't error the asset; a 70 MiB file uploads through the worker API into filesystem and S3 storage (three parts on S3) and matches byte for byte, with the asset unpinned; an edited file is refused and nothing is stored. On the real binary, `clipmux push --local --upload-source` through the agent's socket uploads the file and leaves the re-encode unpinned.

## D060 Dashboard: routing, cloud GPUs and route reasons — accepted (2026-11-06)
Made in P4-T08 (with Phase 3's missing Playwright suite, D052):
- **The Workers page** gains two cards (`dashboard/src/pages/routing.tsx`):
  - **Routing:** the primary, the fallback and the opt-in queue age, with whether this server's worker is online. A provider is offered only once `providers_ready` lists it, so the page can't propose what the API would refuse.
  - **Cloud GPUs:** each connected provider's state (ready, or waiting for its setup test; backing off), its running machines against its limit, the month's estimated spend against the limit, the rate and the pinned image. Under them are a Connect RunPod and a Connect Modal form, each showing every step's result like Connect Cloudflare, and Disconnect. The forms take dollars; the API takes cents.
  - The controls carry accessible names (`Primary executor`, `Fallback executor`, `Queue-age fallback`, `Connect RunPod`).
- **The Jobs page** shows where a queued job is routed and why, in words, for each route reason (D053, D055), and each attempt's reason when it wasn't the primary. Executors are named "this server", "a PC", "RunPod" or "Modal".
  - It used to refresh only while a job it showed was queued or running, so a job created after the page loaded (an upload, a setup test) never appeared. The browser suite found this. It now refreshes every 10 s when idle, and every 3 s while something runs.
  - An attempt's GPU and estimated cost aren't shown (known gap).
- **The browser fixture** (`TestPlayerFixture`) boots its main install against a fake RunPod API and a fake private registry, which serve trusts through `SSL_CERT_FILE`. It creates the owner and a connected PC, and publishes the owner's credentials in `/config`.
- **`e2e/web/tests/workers.spec.ts`** runs after the other suites and puts routing back. It signs in, sees the PC online with its encoders and sandbox, rescopes it, saves "PCs, else this server, after 5 minutes" and finds it after a reload. It connects RunPod through the form, with every step passing, the image pinned by digest and the key never on the page. It sees on the Jobs page the setup test's sample routed to RunPod and not yet claimed, and revokes the PC after the confirm dialog.

## D061 The self-contained Linux agent tarball — accepted (2026-11-06)
Amends D049, made in P4-T09 (a Phase 3 carry-over, D052):
- **Built from the image's own files.** A Dockerfile stage, `agent-pack`, holds exactly the runtime image's packages, toolchain and binary, plus patchelf, and runs `toolchain/pack_agent.sh`. `docker build --target agent --output type=local,dest=dist .` writes `clipmux-agent-linux-{arch}.tar.gz` and its `.sha256`. Both stages come before the runtime stage, so a plain build still makes the image.
- **What's inside:** `clipmux`, `ffmpeg` and `ffprobe` with every shared library they load except glibc's (libc, libm, the loader and the like stay the host's, matching its kernel), the VAAPI drivers (radeonsi everywhere, iHD on amd64) with their libraries, the packager, a launcher, `licenses/` and `SHA256SUMS`. That's 113 MB on amd64, mostly Mesa's driver and its LLVM. The licences are FFmpeg's GPL notices and build manifest, plus the copyright file of the Ubuntu package behind each bundled library.
- **No environment tricks for libraries.** Everything sits in `libexec/` and resolves through `$ORIGIN` DT_RPATHs (`--force-rpath`, so a library's own dependencies resolve the same way). The media sandbox already lets a tool read its own directory (D027), so under Landlock the bundled libraries and drivers load with no sandbox change. The launcher only sets `CLIPMUX_FFMPEG`, `CLIPMUX_FFPROBE`, `CLIPMUX_PACKAGER` and `LIBVA_DRIVERS_PATH`, each unless already set, and execs the real binary.
- **Intel's driver is amd64-only.** `intel-media-va-driver` moved to its own list, `CLIPMUX_RUNTIME_PACKAGES_AMD64`, installed only on amd64, because Ubuntu has no arm64 build. Before this, an arm64 image build would have failed at that install; CI had only built amd64.
- **`clipmux worker doctor --offline`** skips the connection, server and key checks, to check an install before pairing. `doctor` now also passes the driver variables (`LIBVA_DRIVERS_PATH` and the rest of `sandbox.EnvAllowed`) to its sandboxed test encodes, as the worker always did. Without that, a tarball's drivers were invisible to `doctor`.
- **The check** (`toolchain/check_agent.sh`) unpacks the tarball in a clean `ubuntu:24.04` with no FFmpeg. It verifies the checksums and that every binary and driver resolves its libraries, runs `clipmux version` and the toolchain's full capability check against the bundled FFmpeg (including a real encode), then `doctor --offline`. Locally on amd64 it passed. With `/dev/dri` passed in, `doctor` proved VAAPI on the AMD RX 6600 XT under the full sandbox, using the bundled radeonsi driver, which shows Landlock lets the bundled files load.
- **CI** (`agent.yml`: main, by hand, and pull requests touching the Dockerfile or the toolchain) builds natively on `ubuntu-24.04` and `ubuntu-24.04-arm`, runs the check, and uploads each tarball as an artifact for 30 days. It is published at launch (D005, D006).

## D062 Phase 4 exit: CI half passed; real accounts pending — accepted (2026-11-06)
Made in P4-T10, the exit test of [plans/P4-serverless.md](plans/P4-serverless.md):
- **The harness.** `internal/e2e/serverless_db_test.go` runs serve's real router, dispatcher, one-time keys, reaper, reconciler and RunPod adapter against the fake RunPod (`runpodtest`). Each pod runs as a local `clipmux worker --once` with the pod's own start command and environment, on CPU encoders, in its own process group. Like RunPod, the fake restarts a container that exits, until the pod is stopped. Connect RunPod runs through the owner API with the fake registry, and its setup test must pass on a pod before routing is set to "my PCs, else RunPod". The shipped 60 s offline rule, 90 s lease, 5 s router and 30 s reaper apply, with no knob, so the Phase 4 tests run in parallel with each other.
- **Checks 1–8 pass on the real binary**, each on its own and together with Phases 1–3 (`exit.yml`):
  1. With the PC online, a job runs there for `primary` and RunPod creates no pod.
  2. With the PC asleep and shown offline, the next job wakes a pod for `primary_offline`, commits and plays. Its dispatch ends `attempt_ended`, the pod is stopped, and the spent key gets 401.
  3. A lease lost while the PC sleeps moves the retry to a pod, on a new attempt and output; the woken PC is told to stop.
  4. With the PC online but busy (its tools frozen, its heartbeats going), a waiting job stays on the PC while queue age is off. Set to one minute through the owner API, it wakes a pod for `queue_age`.
  5. With a limit of one machine, the second job waits with `fallback_limit` and two pods never run at once. At $360 an hour against a $90.01 limit, the setup test's own few seconds leave no room for a 15-minute reservation: the next job waits with `fallback_limit` and runs on a PC once one connects.
  6. An unprobeable source errors `unsupported_source`. `corrupt.mp4` fails three times on the PC (`transcode_failed` each), is routed `fallback_refused` while the PC is offline between retries, errors `processing_failed`, and its re-encode is `fallback_refused` too. RunPod creates no pod beyond the setup test's.
  7. A `push --local` re-encode whose PC is offline stays `pinned`, with no dispatch, and runs when the PC returns.
  8. serve killed (with its process group) while RunPod creates a pod leaves exactly one pod, which the restarted serve finds by name and records. A restart between a wake and its claim keeps the same pod, whose worker retries until serve is back. At boot, the reconciler stops a pod named for an ended dispatch and leaves the owner's own pod alone.
- **The rig** can now restart serve or crash it with its process group (the built-in worker is serve's child, as in a container), and caps each serve's pool at 8 connections: the eight parallel instances otherwise exhausted Postgres's default 100 on a 12-core machine. The whole real-binary suite, Phases 1–4, takes about 12.5 minutes.
- **Found and fixed:** an attempt copied its job's route reason verbatim, so a job that waited with `fallback_limit` and then ran on its PC recorded `fallback_limit`, and the Jobs page would have said it ran there because the fallback was full. A reason that only explains waiting on the primary (`no_fallback`, `fallback_refused`, `fallback_offline`, `fallback_limit`, `primary_limit`) is now recorded as `primary` once the primary claims.
- **Check 9, real accounts, is written but not yet run.** `exit-providers.yml` (after CI on main, weekly and by hand) runs `TestExitRealProviders`: serve behind a pinned Cloudflare quick tunnel (cloudflared 2026.10.3, by digest), Connect RunPod and then Connect Modal through the tunnel with main's image and a $20 limit each, the setup test through each provider within 25 minutes, `nvenc` in the sample's manifest, every machine stopped by the database and by the provider's own list, then disconnect. It logs each cold start. It needs the secrets `RUNPOD_API_KEY`, `MODAL_TOKEN_ID`, `MODAL_TOKEN_SECRET` and `GHCR_PULL_TOKEN` (read:packages), and does nothing without them.
- **Phase 4 exits** when that run passes; until then the open items stay in known-gaps (the Modal SDK, RunPod's real API, cold starts against the 10-minute claim deadline, and digest-only images). As with Phase 2 (D041), the next phase may be planned meanwhile.

## D063 Phase 5 scope — accepted (2026-11-07)
Context: Phase 4 exited in CI with its real-account run pending (D062), which allows the next phase to be planned. An audit at HEAD found ClipMux not yet ready to run in production (text-only logs without request IDs, rate limits on sign-in only, no metrics, no retention, no drain, no key rotation), and the LMS can't migrate without subtitles, transcripts and chapters (plans/lms-migration.md). The owner chose the scope; the plan is [plans/P5-production.md](plans/P5-production.md).
Decision:
- **Next phase:** Phase 5, "Production ready, with subtitles and chapters", added to the architecture's build order after Phase 4. Its exit: a full suite run leaks no secret into its logs and drains cleanly, retention deletes only what nothing plays, and subtitles and chapters reach LMS parity.
- **Two tracks, hardening first:** logs, deadlines and drain, metrics, rate limits, database timeouts and a migration lock, retention, key rotation and TOTP, a route cache, operator docs; then subtitles, transcripts and chapters, which build on them. Track B's toolchain and transcript tasks touch no shared code and may start alongside.
- **Single node:** one serve replica is the supported production shape. Limiters, the route cache and the drain flag are per process; HA stays a known gap.
- **Speech-to-text runs on workers with whisper.cpp**, a pinned media tool run sandboxed, as a routed `transcribe` job that any executor can take. No audio leaves the owner's infrastructure, and no remote speech API is added this phase.
- **Chapters come from an LLM the owner connects**, off until connected: any OpenAI-compatible API on the standard library, or Claude through the official Go SDK (`anthropic-sdk-go`) with structured outputs.
- **LMS parity, not cutover:** the phase proves every LMS need with a contract test; moving the LMS happens after it.
- **Carried (T00):** Phase 4's real-account run, Phase 2's real Cloudflare run and Phase 3's by-hand checks. Each exit is recorded when it passes.
Consequence: "one mutating job per asset" is refined into two job families (outputs and tracks), with move and delete in both, settled in P5-T13; subtitles join the generated master without a new output, as the architecture foresaw.

## D064 Logs: request IDs, an access line, redaction in one place — accepted (2026-11-07)
Decisions within D063, made in P5-T01 (answering P5 open question 1):
- **`internal/logging`** builds every command's logger from `CLIPMUX_LOG_FORMAT` (`text`, the default, or `json`) and `CLIPMUX_LOG_LEVEL` (`debug`, `info`, the default, `warn`, `error`). An invalid value stops serve and the worker at boot. serve passes both to its built-in worker, so the child logs the same way into the same stream. Compose switching to JSON is P5-T10.
- **Context attributes:** `logging.With(ctx, key, value…)` adds attributes that every `…Context` log call on that context carries; the request ID rides this way. The worker gives each attempt's lines `job`, `attempt` and `kind`.
- **Redaction lives in one package.** `RedactURL`, `RedactPath` and `RedactError` replace the worker's and push's own copies (their second and third uses). A URL keeps its scheme, host and path; its query becomes `REDACTED`, its user info and fragment are dropped, and a `/v/{token}/` path loses the token. Import failures now log the URL's host and a redacted error, where they logged the full `*url.Error` with its query.
- **A last line of redaction on every string a logger writes** (`ReplaceAttr`, messages included): `cmx_`, `cmxa_`, `cmxd_` and `whsec_` values, JWTs, `/v/{token}/` paths, URL query strings and `X-Amz-*`, `sig`, `exp`, `token` and `code` parameters. A route pattern such as `/v/{token}/` is left as it is. The setup token isn't matched; it is logged on purpose until P5-T08 decides otherwise.
- **Request IDs.** `X-Request-Id` is believed only from `CLIPMUX_TRUSTED_PROXIES` and only when it is at most 128 characters of `[A-Za-z0-9._:-]`; otherwise serve makes `req_` + `rand.Text()`. It is always echoed. Handler error logs carry it: those with the request use `ErrorContext`, the shared helpers read it from the response header.
- **One access line per request** (`http request`): method, `route` (the matched pattern), `path` rebuilt from the pattern with the `{token}` and `{code}` wildcards as `REDACTED` (or, unmatched, the path with any playback token hidden, at most 256 bytes), status, bytes, `duration_ms`, client address, request ID. Never the query, `Referer` or `Authorization`. Successful playback, `/_storage/`, worker API and health checks log at debug, so the default level isn't flooded; 4xx at info, 5xx at warn.
- **The recorder unwraps** (`http.ResponseController` reaches the connection) and keeps the connection's `ReadFrom`, so filesystem transfers still use sendfile.
- `http.Server.ErrorLog` goes through slog at warn. Handlers log the route pattern instead of `r.URL.Path`, which can carry a live pairing code.

## D065 Deadlines, panics and an ordered drain — accepted (2026-11-07)
Decisions within D063, made in P5-T02:
- **Server settings:** `ReadHeaderTimeout` 10 s (as before), `IdleTimeout` 120 s, `MaxHeaderBytes` 64 KiB, and still no global `ReadTimeout` or `WriteTimeout`, which would cut long-polls and large transfers.
- **Deadlines per route class**, set through `http.ResponseController` before routing (`internal/server/deadlines.go`). Every request sets its own, because with no `WriteTimeout` net/http doesn't reset a keep-alive connection's write deadline between requests.
  - API: read 30 s, write 60 s.
  - Long-polls (`POST /worker/v1/claim`, `GET /v1/events`): write 45 s, past their 30 s and 25 s waits.
  - Slow inline work (Connect, `POST /v1/setup-test`, `DELETE /v1/assets/{id}`, worker `complete`): 5 minutes each way.
  - Streams (`/_storage/`, `/v/`, `/p/`): a rolling 60 s idle bound, moved forward by each write and each body read (at most once a second). A transfer takes as long as it needs while bytes flow; a client that stops reading is cut a minute later. Streams copy through `Write` rather than sendfile, because a sendfile can't move its deadline mid-transfer.
- **Panics.** The request middleware recovers a handler's panic, logs it with its stack and answers 500 if nothing was written; a panic after the response started, or `http.ErrAbortHandler`, drops the connection. `sweeper.Every` recovers too: a panicking pass is a failed pass, logged with its stack, and the loop runs on. Webhook delivery goroutines recover; the delivery's lease lapses and it is tried again.
- **Pass deadlines.** Every background loop has a per-pass deadline beside its interval (`passTimeouts` in `internal/server/server.go`: 1 minute for lost leases and routing, 5 minutes for provider calls, route sync, uploads and webhooks, 30 minutes for the reconciler and setup tests). Control jobs and imports have none, because their own leases bound a move or a 6-hour fetch.
- **Shutting down runs in order.** At SIGTERM or SIGINT:
  1. The drain starts: `/readyz` answers 503 `draining` (`/healthz` stays 200), waiting claims and event long-polls answer at once, and claims hand out nothing.
  2. The built-in worker gets SIGINT and hands its attempt back with `fail {error_code: worker_stopped}`, through the server, which is still listening. serve waits up to 20 s for it.
  3. `srv.Shutdown` gives requests in flight 15 s; their context no longer comes from the signal, so a segment or a part upload in flight finishes rather than being cancelled. Past the deadline, requests are cancelled and the server closed.
  4. The background loops stop, and serve waits for them before the pool closes, as before. They now outlive the drain, so a commit landing during it still publishes its routes.
  The 20 s and 15 s fit compose's 40 s stop grace period (P5-T10).
- **`worker_stopped` is a lost lease, not a failure.** `queue.Fail` marks the attempt `lost` with that code, spends the lost-lease budget (5) and requeues the job with no backoff, so another worker takes it at once instead of after the 90 s lease. A lost attempt doesn't bar the fallback (D053). Any worker sends it when its context ends mid-attempt (Ctrl-C on a PC agent, a stopping container), on a context of its own with 10 s. Spending the lost budget means a worker that keeps stopping mid-job can't hold a job forever.
- **The sandbox's CPU limit counts every thread.** `RLIMIT_CPU` is per process, so the flat 6 hours killed an 8-thread encode after 45 minutes of wall time. Tools now get 6 hours per CPU (`sandbox.CPUBudget(runtime.NumCPU())`); the wall clock stays bounded by the attempt's 6-hour timeout.
- **Headers.** Every response carries `X-Content-Type-Options: nosniff`. Dashboard pages add `Referrer-Policy: no-referrer`, `Cross-Origin-Opener-Policy: same-origin`, a `Permissions-Policy` refusing camera, microphone, geolocation, payment and USB, and an enforced Content-Security-Policy (P5 open question 2): scripts only from the server, styles from the server plus inline (the dialog and player components inject `<style>`), and images, media and connections also to the media URL and the public endpoint of every S3 or R2 store (`stores.Set.Origins`), which dashboard uploads PUT to; `blob:` for hls.js's worker and Media Source; `object-src`, `base-uri` and `frame-ancestors` none. The Playwright suites under the CSP are part of the Phase 5 exit (P5-T20).
- **https.** Once the public URL is https, a request for the public or media hostname that a trusted proxy says arrived over plain http gets a 308 to the same URL on https, and one that arrived over https gets `Strict-Transport-Security: max-age=31536000` (no `includeSubDomains` or `preload`, which would reach hosts this server doesn't run). The scheme comes only from a trusted proxy's `X-Forwarded-Proto`, so a direct request, Caddy's `/_tls/ask` and the built-in worker over loopback are left alone, and a redirect never names another host.

## D007 Shaka Packager 3.9.3 — accepted (2026-11-05)
Decision: pin v3.9.3 (`toolchain/versions.env`), the version clipmux-lms runs with AES-128 in production, which needs the init-segment workaround for Shaka issue 1616. Move to a release that fixes 1616 when one exists, re-running the Phase 1 player tests.
