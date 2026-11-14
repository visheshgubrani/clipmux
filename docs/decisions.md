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
- `clipmux admin create-key --app NAME` issues keys from the host; the dashboard issues them too (T10, D108). Keys are `cmx_`-prefixed and stored as SHA-256.

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
- No API key while the public URL is loopback (localhost, `*.localhost`, 127.0.0.0/8, ::1, 0.0.0.0): signed upload URLs and `base_url` would point at the owner's machine. LAN addresses count as reachable. `clipmux admin create-key --allow-loopback` is the development escape hatch, and the dashboard asks the owner to confirm a key is for this machine (amended in D108).
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
- API keys are refused with 409 `public_url_required` while the public URL is loopback (D023), unless the request says `local: true` (amended in D108). A key created in the dashboard defaults to `uploads:write`, `assets:read` and `assets:write`.
- Preview tokens are signed with the internal key, last one hour, and pick the asset's first active signed playback ID (a public one otherwise).
- Asset views gain `app_id`, so the library can say which app owns each asset.

## D026 Setup test — accepted (2026-11-05)
Decisions within the architecture's rules, made in P1-T10:
- The sample is embedded in the binary (`internal/setuptest/sample.mp4`, 8 s, 854×480 so the ladder makes two rungs plus audio, about 480 KB), produced by `toolchain/make_sample.sh`, so the control plane never runs ffmpeg.
- A run creates the upload in-process as the Dashboard app's backend, then does what a browser and a player do, over HTTP to the configured URLs: sign parts and PUT them on the public URL, complete, wait for `ready`, then play through the media URL with internal-key tokens. Ten checks, in order: `upload`, `encode`, `play_master`, `play_key` (16 bytes), `play_segment_decrypts` (AES-128-CBC with the delivered key and the playlist's IV gives an fMP4 fragment), `thumbnail_token_refused_segment`, `thumbnail_token_refused_key`, `thumbnail_token_poster`, `no_token_refused`, `stored_segment_encrypted` (the stored bytes don't parse as a fragment). A failed check skips the ones that depend on it. Details never carry a URL or a token.
- One test at a time per project (a partial unique index); asking again returns the queued one. Triggers: first boot (when the built-in worker is on, the URLs are confirmed, and no test ever ran; an unconfirmed install would only test the placeholder default, so saving the URLs runs its first test), saving the URLs, `POST /v1/setup-test`, and `clipmux admin setup-test --wait`, which exits non-zero unless every check passes. A new run deletes earlier samples, so the latest stays playable in the dashboard.
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
- `POST /v1/cloudflare/connect` (owner session) takes `{token, hostname, bucket?, account_id?}` and runs, in order: verify the token; pick the account (the only one, or the one named); find the active zone holding the hostname; Workers Paid; ensure the bucket (the default is `clipmux-media`, and a created bucket carries the warning that an unscoped token reaches every bucket); derive R2 credentials (token ID, and hex SHA-256 of the value), check them against the bucket and apply CORS, then store or renew them sealed; upload the embedded Worker as `clipmux-media` with `MEDIA`, `ORIGIN_URL` (the public URL) and the `CONTENT_KEYS` secret; set Worker delivery on that bucket, make it the default store (on a first connect, or as `default_storage` says; amended in D110) and reconcile every route into it; delete the DNS records at the hostname; attach the Worker as its custom domain; save `https://{hostname}` as the media URL; record the connection (token ID only); queue the setup test. It stops at the first failure, and every step is idempotent, so the same call reconnects with a new token and redeploys an upgraded Worker.
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

## D066 Metrics on a listener of their own — accepted (2026-11-07)
Decisions within D063, made in P5-T03:
- **A small exposition writer, not client_golang.** `internal/metrics` keeps counter, gauge and histogram families and scrape-time families, and writes the Prometheus text format 0.0.4 sorted by name and label values, with label values and help escaped. The format is small and stable; the client library would add `common`, `procfs` and a global registry. `promtool check metrics` passes on a live serve's output.
- **Its own listener.** `CLIPMUX_METRICS_ADDR` (host:port, unset by default) serves `/metrics`; `CLIPMUX_PPROF=true` adds Go's `/debug/pprof/` there, and is refused without the address. Neither is ever on the public listener, where `/metrics` and `/debug/` are JSON 404s rather than the dashboard. The metrics port needs no auth, so it must stay off the internet; compose keeps it on the internal network (P5-T10).
- **What is counted where it happens:** `clipmux_http_requests_total{route,code}` (the matched pattern, `unmatched` otherwise), `clipmux_http_request_duration_seconds{route}` (long-polls left out, their duration is their wait), `clipmux_http_in_flight`, `clipmux_delivery_bytes_total{policy}` (signed or public), and per background loop `clipmux_sweeper_pass_seconds`, `_errors_total`, `_panics_total` and `_last_success_timestamp_seconds`, recorded only by the replica that runs the loops.
- **What is read from Postgres when scraped**, cached for 10 s with a 2 s timeout, so every replica reports the same: active jobs by kind, status, `routed_to` and `route_reason`; the oldest due queued job by kind; finished attempts by executor, result and error code; pending outbox rows and the oldest one's age by kind; webhook deliveries by status; dispatches by provider and status; PC agents and the built-in worker seen in the last minute. Finished attempts are a counter read as a count, since attempts are kept as history.
- **Bounded labels.** Error codes outside the set ClipMux defines become `other`, because a worker may send any snake_case code and each would become a series. Routes are patterns, never paths.
- Also: the pgx pool's acquired, idle and maximum connections, acquire wait and empty acquires; `clipmux_leader`; `clipmux_build_info{version}`; goroutines, heap bytes and the process start time.
- **`/readyz`** answers 503 while the database ping fails or serve drains (D065), and only once serve is listening. OpenTelemetry stays out (P5 design note e): request IDs and job and attempt IDs in the logs cover one node.

## D067 Rate limits in one process — accepted (2026-11-07)
Decisions within D063, made in P5-T04 (P5 open question 5 stays open until the LMS's import rate is known):
- **`internal/ratelimit`** holds GCRA token buckets, sliding failure windows (the sign-in limiter, generalised; its 5-per-address, 20-overall, 15-minute literals are unchanged) and concurrency slots, each keyed by a string and capped at 100,000 keys (fully recovered keys are dropped first; past that a limiter forgets everything rather than grow). The client address moved there from `accounts` with its rule: the rightmost `X-Forwarded-For` hop a trusted proxy added. Addresses are keyed by themselves for IPv4 and by /64 for IPv6.
- **Limits are applied where the caller is already known**, not from a route table:
  - integration API, per API key or per owner session acting as an app, in `apikeys.Authenticator.Require`, shared by every API: 20 requests a second with bursts of 200, and writes (anything but GET, HEAD, OPTIONS) also 2 a second with bursts of 60;
  - `GET /v1/events`: 4 long-polls at once per key;
  - upload-token routes: 10 a second with bursts of 50 per upload, answered with CORS and `Retry-After` exposed so the browser can back off;
  - `POST /v1/setup/owner`: wrong setup tokens limited like sign-ins;
  - `POST /worker/v1/pair`: 10 wrong codes per address and 100 overall in 15 minutes, bounding guesses at a 39-bit code;
  - `POST /worker/v1/pairings`: 20 an hour per address, beside the existing cap of 10 waiting;
  - `POST /worker/v1/pairings/poll`: 2 a second per address, bursts of 4;
  - playback (`/v/`, `/p/`): only refusals count. An address with 300 answers of 401, 403 or 404 in 5 minutes gets 429 for every playback request until they age out; successes never count, so a classroom behind one NAT is never limited.
  - Signed storage URLs, attempt calls and claims are not limited: each is signed or scoped to one attempt.
- **The refusal** is `429 rate_limited` with `Retry-After` and a `retry_after` detail. `too_many_attempts` and `too_many_pairings` keep their codes. No `RateLimit-*` headers until that draft is an RFC. Rejections show as `clipmux_http_requests_total{code="429"}`; there is no counter per limit.
- Limits live in one process, so a second replica doubles every budget (known gap).

## D068 Database sessions, boot and migrations — accepted (2026-11-07)
Decisions within D063, made in P5-T05:
- **Session settings.** `db.Open` gives every connection `statement_timeout=60s` and `idle_in_transaction_session_timeout=5min` unless `DATABASE_URL` sets them (as a parameter or in `options`). There is no global `lock_timeout`, because route sync holds outbox rows across storage calls. Pool size and the rest of pgxpool's settings stay URL parameters (`pool_max_conns=…`).
- **Boot waits for Postgres.** `db.Open` retries the ping from 1 s doubling to 10 s for up to a minute, so serve outlives a database that is still starting. A URL that doesn't parse is reported without echoing it, since it carries the password.
- **Migrations hold a lock.** Goose's Postgres session locker takes an advisory lock keyed `"cmx2"` and the schema's FNV-1a hash (`db.MigrationLockID`), so serve replicas, and serve and `clipmux admin`, never migrate the same schema at once, while installs in separate schemas of one database don't wait for each other (D035's rule). Two concurrent runs both succeed; one applies.
- **A newer schema is refused.** Before migrating, serve and admin compare the database's version with the newest migration the binary has, and stop with `schema_newer` naming both, rather than run an old model against new tables.
- **`clipmux admin migrate status | up | down-to VERSION --yes`.** `status` lists every migration with when it was applied, `down-to` needs `--yes`. Other admin commands keep migrating at start, now under the lock (P5 open question 12: refusing while migrations are pending would change what operators run today, for no safety the lock doesn't already give).
- `CLIPMUX_TOOL_MEMORY_MB` must be a positive number; serve and the worker stop at boot otherwise, where it used to be ignored.

## D069 Row retention, stuck publications and webhook health — accepted (2026-11-07)
Decisions within D063, made in P5-T06:
- **`internal/retention` prunes rows past their windows**, an hourly leader loop (10-minute pass deadline). Each rule deletes at most 1,000 rows per statement and repeats until a pass comes up short, so no pass holds a long lock. The windows are constants because each follows from another rule:
  - idempotency keys after 24 h (a client's retry window);
  - dashboard sessions once expired or idle 7 days (the idle limit, D024);
  - pairings a day after they expire;
  - finished webhook deliveries after 30 days, then their events once no delivery points at them, which bounds `GET /v1/events` and `clipmux listen --since` to 30 days;
  - other finished outbox rows after 7 days;
  - all but the newest 20 finished setup tests;
  - a dispatch's sealed one-time key once the dispatch ends (`key_sealed` became nullable).
  Jobs, attempts and dispatches are kept as history and spend.
- **`clipmux admin prune [--dry-run]`** runs the same rules now; the dry run applies them in a transaction it rolls back, so its counts are exact.
- **No dead letter for route and key publications, wakes or stops.** Giving up would strand a revocation, an asset's readiness or a paid machine, so they keep their capped backoff. One pending for over an hour is "stuck": the retention pass logs an error, `clipmux_outbox_oldest_pending_seconds` shows it (and the shipped alert fires), and `clipmux admin outbox [--retry ID]` lists them and makes one due at once.
- **A breaker per webhook endpoint.** After 5 failed tries in a row (`webhooks.BreakerAfter`) the endpoint is skipped for a minute, doubling with each further failure to at most an hour. A skipped delivery is neither claimed nor charged a try, and its 24-hour window doesn't move; a success closes the breaker. A delivery whose last try falls in an open window is tried once the breaker closes, and then fails if its window has passed. Endpoints list `consecutive_failures` and `skipped_until`.
- **Two-step secret rotation** keeps D018's single signature header: `POST /v1/webhook-endpoints/{id}/secret` returns a new secret once, sealed apart (`webhook_endpoints_pending`), while deliveries stay signed with the old one; `…/secret/activate` switches signing to it from the next try.
- **Redelivery:** `GET /v1/webhook-endpoints/{id}/deliveries` lists the latest 100, and `POST /v1/webhook-deliveries/{id}/redeliver` sends a delivery that gave up (`failed`) again with a fresh 24 hours; any other is `409 not_redeliverable`.

## D070 File retention and output-less attempts — accepted (2026-11-07)
Decisions within D063 and the architecture's "Storage layout", made in P5-T07:
- **`outputs.purged_at`**: an output whose files were deleted. Delivery answers 404 for it, route records leave it out of `outputs`, and the move job doesn't copy it; the row stays as history.
- **Retired outputs** go once retired longer than the larger of 7 days and the project's maximum token lifetime, plus an hour, so a viewer mid-session on the old output finishes. The output is marked purged and its asset's routes refreshed in one transaction, and only then are its files deleted, so nothing serves a file that is going.
- **The files of a failed, lost or superseded attempt** go 30 minutes after a newer attempt of its job started or the job ended. Only a live attempt can sign, and its URLs last 15 minutes, so nothing can write there after.
- **Orphaned output prefixes** (`media/{asset}/out_*/` with no unpurged row) are deleted by a sweep of 500 assets per hourly pass, resuming from a cursor in `retention_cursors`. A claim creates its output's row before the worker gets a URL, so a prefix without a row has no writer and needs no age check. Only `out_` prefixes are touched, so an asset's other files (tracks, P5 track B) are safe.
- **Filesystem leftovers:** `tmp/` files older than 24 hours, and multipart directories nothing has written to for 7 days (an upload idle for a day is aborted, so such a directory has no upload). This is `storage.Filesystem.SweepStale`, not part of `Storage`.
- **An upload_source attempt has no output.** `attempts.output_id` became nullable; the claim creates no output and carries no content key for it, `sign` refuses it with `409 no_output`, and the migration marks the empty outputs earlier claims made as purged.
- The file rules run hourly with a 30-minute pass deadline, and on demand with `clipmux admin prune --files`.

## D071 Key rotation, the owner's second factor and the setup token — accepted (2026-11-07)
Decisions within D063, made in P5-T08 (answering P5 open questions 2 to 4):
- **The secrets key rotates.** `CLIPMUX_SECRETS_KEY_PREVIOUS` lets the box open values sealed with the old key, by the version every sealed value carries (D017); new values are always sealed with the current key. `clipmux admin reseal-secrets` reseals every column in `secrets.Columns`, one column per transaction, and is safe to repeat. At boot serve checks that every sealed value opens with the key or the previous one and refuses otherwise, naming the column. A test compares `secrets.Columns` with every bytea column named `*_encrypted`, `*_sealed` or `secrets_check` in the schema, so a new one can't be forgotten.
- **The content key secret rotates without a restart.** `contentkey.Ring` is now shared state that every copy sees, and `Replace` swaps it. Versions carry `usable_at`, and new outputs use the newest usable one. `POST /v1/content-keys/rotate` adds a pending version and reloads the ring. With Worker delivery it then runs Connect Cloudflare with the given token (D037), which deploys `CONTENT_KEYS` with the whole ring, and makes the version usable only after that succeeds. A failed deploy answers 502 with the steps and leaves the version unused. Existing outputs keep their versions.
- **Signing keys rotate gracefully.** `clipmux admin rotate-signing-key --app` creates a key and sets the app's other live keys' `retired_at` to now plus the maximum token lifetime, so tokens they signed keep working until they would expire anyway; `_keys.json` carries `not_after`. `clipmux admin list-keys --app` and `clipmux admin revoke-key ID` (an API key or a signing key) cover the rest.
- **TOTP for the owner**, on the standard library (RFC 6238: HMAC-SHA1, six digits, 30 s steps, one step of drift either way). The secret is sealed under `owners/{id}`. The last accepted step is stored, so a code works once.
  - Enrolling takes the password, then a first code. It returns ten single-use recovery codes once (stored as SHA-256; they are random, so a slow hash adds nothing) and ends every other session.
  - Once on, signing in needs `code` or `recovery_code`: none is `401 totp_required` and costs no failure; a wrong or replayed one is `401 invalid_code` and counts as a failed sign-in.
  - The secret is shown as text and an `otpauth://` link, with no QR library (open question 2).
  - `clipmux admin reset-totp` turns it off for a locked-out owner. The dashboard's account page enrols and turns it off.
- **The setup token lasts a day.** Past that, the next attempt is refused and a fresh token is logged, so a token copied from an old log can't claim an install left unregistered. It is still printed to the log (open question 3), and the setup route has a failure limit (D067).
- **No audit log this phase** (open question 4): ownership changes and key operations log at info with the request ID; a durable audit trail is a known gap.

## D072 A route cache in built-in delivery, and listing in two queries — accepted (2026-11-07)
Decisions within D063, made in P5-T09:
- **Delivery caches** each playback ID's route, each signing key and each output it reads, so a segment request usually costs no query. Entries live 30 s (`delivery.RouteTTL`), and an unknown ID 5 s, against the Worker's 60 s and 5 s (D036).
- **Invalidation comes from route publication.** `outputs.Committer` calls `OnPublished(pids…)` after the transaction that confirms route_sync rows commits, and `OnKeys()` after `_keys.json` is confirmed; serve wires them to the handler. Every change the Worker must see already goes through those rows, so on one node a revocation, a deletion, a disabled app or a new output is seen at the next request. Outputs are all dropped on any route invalidation, because a publication may follow a commit, a retirement or a purge.
- **A generation counter** refuses to store a value whose load began before an invalidation, so a slow read can't put back what was just dropped.
- The cache holds at most 100,000 entries of each kind and starts over past that. Changes that publish nothing, such as the project's maximum token lifetime, take up to 30 s; so do another replica's (known gap).
- **`assets.LoadMany`** builds a page of asset views with one media-URL read per project and one playback-ID query for the whole page (`ListPlaybackIDsForAssets`), where listing and the library made one query per asset. A test counts 2 queries for 100 assets.

## D073 Operator docs, a configuration reference and compose — accepted (2026-11-07)
Made in P5-T10:
- **`docs/configuration.md`** lists every variable by command (serve, worker, push and listen, admin, tests), with its default and what it does. `internal/config/doc_test.go` walks the Go source and fails when a variable is read that the page doesn't name, or that a deployment reads with no example in `.env.example`; variables only tests read live in their own section.
- **`docs/operations.md`** covers install, upgrade (migrations under the lock, `schema_newer`, the drain), backups (Postgres, the secrets key and the media together, and why all three), monitoring, the retention windows, how to rotate each key, a security checklist and troubleshooting by symptom.
- **`deploy/prometheus/alerts.yml`**: ten alerts, from serve down and no leader to stuck outbox rows, a queue backlog, no worker online, webhooks giving up and a full pool. `promtool check rules` passes, and a test checks that every metric they name is one serve exports.
- **Compose:**
  - JSON logs with json-file rotation (5 × 50 MB) for the app and Caddy;
  - `stop_grace_period: 40s`, which the drain (D065) fits;
  - healthchecks for the app (`clipmux healthcheck`) and Caddy (its admin API), and Caddy starts only once the app is healthy;
  - metrics on `:9090`, exposed to the compose network and never published.
- **CI's image job** checks that the metrics port isn't reachable from the host or through Caddy but answers on the compose network, and that stopping the app exits 0 within the grace period.

## D074 Track A of Phase 5 is complete — accepted (2026-11-07)
P5-T01 to T10 are done (D064–D073). What their tests prove:
- Each task's tests pass on real Postgres and SeaweedFS with the media tools: logs and redaction, deadlines, drain and panics, metrics, limits, database timeouts and the migration lock, row and file retention, key rotation and TOTP, the route cache, and the configuration reference.
- Every real-binary test in `internal/e2e` now runs serve with JSON logs at debug, and fails if any line isn't a JSON object with `time`, `level` and `msg`, or holds something shaped like an API, agent or dispatch key, a webhook secret, a JWT or a URL signature (`checkLogs`). This is exit check 1 of the plan, run by every phase's exit tests, not a separate test.
- By hand on the real binary: a SIGTERM mid-encode drained cleanly, and `/metrics` passed `promtool check metrics` on a live serve.
Left for the phase exit (P5-T20), as planned: the soak run, the Playwright suites under the enforced CSP and TOTP in a browser, a backup-and-restore drill and rotation on the real binary (including the content key through the fake Cloudflare), and CI's compose checks on a real runner. T00's real-account runs still wait for their secrets.

## D075 Online speech beside local whisper — accepted (2026-11-07)
Amends D063, whose "no remote speech API is added this phase" the owner reversed while planning track B: ClipMux is local-first, but an owner without a GPU, or who prefers it, can send transcription to a speech API. The design is in [plans/P5-production.md](plans/P5-production.md) ("Speech API") and lands as P5-T14b.
Decision:
- **Three speech modes per project** (`projects.speech_mode`): `local`, the default, transcribes with whisper.cpp on the project's executors exactly as planned; `api` sends every transcription to the connected speech API; `local_then_api` keeps the primary and uses the API as the transcriptions' fallback (replacing the media fallback for them), when no worker that can transcribe is online or after the queue-age setting. `api` and `local_then_api` need a verified connection; disconnecting returns the project to `local`.
- **One adapter:** any OpenAI-compatible `POST /audio/transcriptions` with `verbose_json` segment and word timestamps, on the standard library. That covers OpenAI, Groq, Together, Fireworks and DeepInfra, and self-hosted Speaches, LocalAI or vLLM, so "local" also includes a speech server on the owner's own LAN.
- **It runs in serve,** as an executor class `speech_api` that the router already knows how to route to. serve claims, leases and fails attempts through the same queue calls workers use, with no HTTP hop, so the key never reaches a worker; it reads the committed output's audio rendition decrypted, in chunks of whole segments under 24 MiB, with no ffmpeg in serve.
- **Off until connected.** Audio leaves the owner's infrastructure only when the owner connects an API and picks a mode that uses it; the Connect card and the docs say so. The connection's key is sealed in `ai_connections`, which also holds the chapters LLM's.
Consequence: the router gains a class and transcriptions a fallback that isn't a machine; chunk boundaries can split a word, the API has no spend cap this phase, and whether a provider accepts fragmented MP4 is proven at Connect (known gaps).

## D076 The canonical transcript and its WebVTT — accepted (2026-11-07)
Decisions within D063, made in P5-T12 (`internal/transcripts`, pure Go):
- **The canonical transcript, v1:** `version`, `language` (ISO 639-1, Whisper's three-letter `haw` and `yue`, or `und`), `duration_ms`, `model` (`{name}@sha256:{hex}` for a pinned local model, `{model}@{host}` for a speech API), `output_id` (set by serve from the attempt, never trusted from a worker) and `segments`, each with `id` `s{n}` (the recogniser's index, so a dropped segment leaves a gap), `text`, `start_ms`, `end_ms` and `words` `s{n}w{m}` whose text keeps the recogniser's leading space. Every time is rounded once, floor(x·1000+0.5).
- **Converters always produce a valid transcript:** text is sanitised (invalid UTF-8 dropped, control characters and NUL removed, line breaks and tabs made spaces), times are clipped to the duration and moved forward where the recogniser went backwards, and empty, zero-length or `[BLANK_AUDIO]` segments are dropped. `Validate` checks the same rules on whatever a worker sends, plus limits (100,000 segments, 1,000,000 words, 4 KiB per segment, 256 bytes per word); a failure is `transcript_invalid`.
- **whisper.cpp:** words are built from `-ojf` tokens, keeping their raw bytes until a word is complete, because whisper.cpp splits Devanagari and CJK characters across tokens. A token starting with a space starts a word; in scripts written without spaces (Han, kana, Thai, Lao, Khmer, Myanmar, Tibetan) each token is a word; `[_…]` control tokens are skipped.
- **OpenAI-compatible `verbose_json`** (D075): each chunk's times are clamped to its span and moved by its start; words go to the segment holding their midpoint and take the segment's own spelling, with punctuation, when its text splits into as many words, otherwise the segment is one cue. The language is the first chunk's, mapped from Whisper's names through Whisper's own table.
- **WebVTT keeps clipmux-lms's cue rules** (84 characters, 4 s, an 800 ms pause; ordered, at least 1 ms, clipped, numbered, one line, `&`, `<` and `>` escaped, so a `-->` in speech can't start a cue). Every file starts `WEBVTT` then `X-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000`, the default RFC 8216 assumes anyway, made explicit for HLS clients; browsers ignore it. So the empty transcript is exactly that header and a blank line, where the plan said `WEBVTT\n\n`: one file serves both the HLS playlist and the sidecar URL.
- clipmux-lms's audio-offset trim is not ported (alignment by construction, P5 "Transcription on workers").

## D077 whisper.cpp, its models and their measurements — accepted (2026-11-07)
Decisions within D063 and D075, made in P5-T11 (answering P5 open question 6 in part):
- **whisper.cpp v1.9.5** (commit `d1be6fd`, the codeload tarball pinned by sha256 in `toolchain/versions.env`), built by `toolchain/build_whisper.sh` with every x86-64 CPU variant ggml ships (chosen at run time from what the CPU supports) and the Vulkan backend, as loadable modules in one directory, `libexec/clipmux-whisper/`, beside `whisper-cli` with `$ORIGIN` rpaths: ggml loads backends from the executable's directory, and the sandbox lets a tool read its own. No libcurl, SDL or OpenMP. The image, the agent tarball (`libexec/whisper/`, with `CLIPMUX_WHISPER` in the launcher) and CI use the same build; CI caches it with the models.
- **One patch** (`toolchain/whisper-vad-token-times.patch`): with `--vad`, whisper-cli's JSON mapped segment times back to the audio but left token times on the speech-only timeline VAD builds, so every word after a removed silence came early (3 s of leading silence put "Americans" at 1.2 s instead of 4.8 s). The library already maps token times (`whisper_full_get_token_t0/t1`); the patch makes the CLI's JSON use them. It is hashed into the build's recipe and goes once upstream's CLI does the same.
- **The pipeline** (`media.Transcribe`): ffmpeg makes 16 kHz mono PCM from the output's audio rendition, then `whisper-cli -l auto --vad -vm … -sns -ojf -np` (non-speech tokens suppressed, as clipmux-lms's faster-whisper did by default), `-ng` on the CPU. Both run under `clipmux sandbox-exec`; whisper on Vulkan gets the GPU policy with every DRM render node and NVIDIA's device nodes, and the sandbox now passes `VK_ICD_FILENAMES`, `VK_DRIVER_FILES`, `MESA_VK_DEVICE_SELECT` and `GGML_VK_VISIBLE_DEVICES`. The image asks NVIDIA's runtime for `graphics`, which carries the Vulkan ICD.
- **Models are fetched, by digest, never baked in:** large-v3-turbo GGML and Silero VAD v6.2.0 from Hugging Face URLs pinned to a repository commit, with sha256 and size in `versions.env`. Weights in the image would slow every serverless cold start.
- **The quantization is q8_0**, measured on 120 s of speech with the same 220-word transcript from each: real-time factors on this machine's 6-core (12-thread) CPU at 6 threads q5_0 0.51, **q8_0 0.40**, f16 0.42; on its RX 6600 XT over Vulkan q5_0 0.09, **q8_0 0.05** (0.11 on a process's first run, which builds its pipelines), f16 0.18. 12 threads were slower than 6 (0.55 for q8_0), so the CPU speech slot gets half the logical CPUs, as planned. An 11 s clip costs about 22 s on the CPU: whisper encodes whole 30 s windows, so short clips pay a fixed cost.
- **Still to measure:** NVIDIA on Modal's L4 (Vulkan with `graphics` against CUDA) waits for T00's Modal secrets, so CUDA isn't built; an NVIDIA worker uses Vulkan or the CPU until then. Detection on English, Hindi and Hinglish waits for the owner's recordings (`internal/media/testdata/speech/README.md`); `TestDetectsTheOwnersLanguages` skips, naming the files, until they exist.
- **Tests:** a golden transcription of a public-domain clip (JFK, from whisper.cpp's samples) under the full sandbox finds the words, `en` and their times; 3 s of leading silence moves every word by 3 s ± 0.3 under VAD; whisper on the CPU makes no network socket call (strace: only glibc's nscd lookup over AF_UNIX, which the sandbox refuses too); the same transcription on Vulkan.

## D078 Tracks and two job families — accepted (2026-11-07)
Decisions within D063, made in P5-T13 (migration 00024, `internal/tracks`):
- **`tracks`**, per asset: `kind` (`subtitles`, `chapters`), `source` (`generated`; `uploaded` reserved), `status` (`preparing` → `publishing` → `ready` → `retired`, `preparing` → `errored`, any → `deleted`), `language`, `name`, `error_code`, `key_version`, `duration_ms`, `chapters`, the output it was made from, and a `version`. Partial unique indexes allow one ready and one being made per asset and kind, so regenerating never overwrites. **`transcripts`** holds the canonical transcript apart, keyed by track. **`ai_connections`** (the speech API's and the chapters LLM's, D075) is created here with its sealed key in `secrets.Columns`.
- **One mutating job per asset becomes one per family:** `jobs_one_active_output` (import, transcode, reencode, upload_source, move, delete) and `jobs_one_active_track` (transcribe, chapters, move, delete). Move and delete are in both, so they still exclude everything; a transcription waiting for a worker no longer blocks a re-encode. `lockForChange` locks the family it changes (a re-encode its outputs), delete locks and cancels both, and a cancelled transcription's worker is told to stop at its next call. The Down drops the track jobs before restoring the single index.
- **A transcription is a routed worker job**, never pinned. Claims name their `kinds` (absent means the media kinds, all an older worker asks for) and carry `speech: {backends, model}` when the worker can transcribe; a worker runs its media and speech loops apart, so a long transcription never delays an encode. `job_claimable_by` takes the kind and the worker's speech, with a disk rule of 1 GiB plus 64 KiB a second of the asset's duration for a transcription. The router counts the built-in worker's speech (`projects.builtin_speech`, from its last claim), an agent's (`capabilities.speech`), and every verified provider as able.
- **A transcription attempt writes no output.** It reads the asset's current output at claim (`attempts.input_output_id`) and keeps reading it if a re-encode commits meanwhile; the claim carries no source.
- **The fallback bar is scoped to the family** (D053): only failed attempts of the job's own family count, so a failed transcription doesn't bar the re-encode from a fallback, nor the reverse. `model_unavailable` joins the capacity codes.
- **Video never waits for, or fails with, its captions.** An output's commit queues a transcription for each subtitles track still being made, reading that output; with no audio the track errors `no_audio` and no job is made. A track job that fails for good errors only its track (`transcript_invalid`, `transcribe_unavailable`, `speech_api_rejected` and `no_audio` kept, anything else `transcribe_failed`), and chapters still being made error `subtitles_failed`; the asset's status and events don't change. A transcription no worker claims within 24 hours (`queue.SpeechWait`, the `speech_wait` loop) errors `transcribe_unavailable`.
- The control loop now runs only the kinds it knows and reports any other, where an unknown kind used to fall through to delete.
- Track webhooks and tracks in the asset view come with the integration API (P5-T16); the new error codes are metric labels already.

## D079 Transcription from a worker to its commit — accepted (2026-11-07)
Decisions within D063, made in P5-T14 (answering P5 open questions 7 and 9 for now):
- **`GET /worker/v1/attempts/{id}/audio`** streams a live transcription attempt's input: its output's audio rendition read from storage, every segment decrypted (`media.DecryptSegment`, CBC with PKCS#7, now also the setup test's), joined after the clear init segment into one fragmented MP4. The worker needs no content key. It is a stream for the deadlines (D065): a rolling minute idle. A stopped attempt gets 409 `attempt_stopped`.
- **Two claim loops.** A worker's media loop claims as before; once speech is proven, a second loop claims `kinds: ["transcribe"]`, each in its own scratch directory (`{work}/media`, `{work}/speech`), since a loop clears its own at start. Both report `speech` in every claim. Speech is proven off the start path, so a first start that downloads the models doesn't hold up encodes; a `--once` machine proves it first and then claims any kind it can run.
- **`--speech auto|off|cpu|vulkan`** (`CLIPMUX_SPEECH`, which serve passes to its built-in worker). `auto` takes a GPU whose Vulkan probe passes, else a CPU with at least 4 cores and 6 GiB (P5 open question 7). `cuda` is refused with the reason (D077). The probe runs the real model on 2 s of a tone under the job sandbox, without VAD, which would skip it.
- **Models are fetched by the worker, never the sandboxed tool,** from the URLs pinned in `versions.env`, into `CLIPMUX_MODELS_DIR` (the built-in worker's under serve's data directory, an agent's under its agent directory, the image's `/var/lib/clipmux-agent/models`), checked against size and sha256 on every start, and replaced if they don't match. `internal/media/models.go` is a copy of the pins that a test regenerates and checks, so `versions.env` stays the one place a digest is written. A stalled download is cut after a minute without bytes.
- **The commit.** `complete {transcript}` runs `tracks.Publisher.CommitTranscript` under the job's liveness lock, so a stale attempt commits and writes nothing. serve sets the transcript's output and duration from the attempt's input output, validates it (`transcript_invalid`, 409, which the worker reports as permanent: its converter only makes valid ones), renders WebVTT, seals it with AES-128-GCM under a key derived per track from the content key secret (`contentkey.DeriveTrack`; `0x01 | version | nonce | ciphertext`, the storage key as associated data; vectors from Python's `cryptography`), writes `media/{asset}/tracks/{track}.vtt`, stores the transcript, and moves the track to `publishing` with its language, display name and duration. Publishing its routes and becoming ready is P5-T15.
- **Failure codes:** `transcribe_failed` (whisper failed: may be the file, bars the fallback), `model_unavailable` (capacity), `source_download_failed` for the audio (capacity), `transcript_invalid` (permanent).
- **`worker doctor`** checks speech: a note when whisper or the models are missing, a ✓ naming the backend once a probe runs, a ✗ only when every backend's probe fails.
- **Real-binary tests run with `CLIPMUX_SPEECH=off`** for serve, agents and pods unless a test turns it on, because every instance has a new data directory and the models are 0.9 GB. Two wakes per captioned video on a serverless primary stay accepted (open question 9); the speech API avoids the second (D075).

## D080 The speech API in serve — accepted (2026-11-07)
Decisions within D075, made in P5-T14b (migration 00025, `internal/speechapi`):
- **Routing:** `projects.speech_mode` (`local`, `api`, `local_then_api`) and the class `speech_api`, which only a transcription can be routed to and which is up while a verified connection exists. `decideRoute` is unchanged; for a transcription the router feeds it the mode's primary and fallback (the API replaces the media fallback under `local_then_api`, and queue age applies). A transcription in `api` mode starts routed `speech_api`. `attempts.executor` gains `speech_api`.
- **The runner** is a leader loop (`speech_api`, every 5 s, a 2-hour pass deadline) that claims one transcription at a time through the queue, renews its lease every 15 s, and commits through the same `tracks.Publisher.CommitTranscript` as a worker's `complete`. The key is opened in serve for each job and reaches nothing else. At the drain it hands its attempt back `worker_stopped`; a stop verdict (the job cancelled) ends it quietly.
- **Audio** is the input output's rendition decrypted segment by segment, joined behind the clear init segment into chunks of at most 24 MiB (about 25 minutes at 128 kb/s), each built as segments are read so memory stays near one chunk, and cut at segment boundaries. Silence packs small (4 s of it is about 1 KB), so a chunk can hold more time than speech would suggest.
- **Requests** are multipart `POST {base}/audio/transcriptions`: `model`, `response_format=verbose_json`, both timestamp granularities, `temperature=0`, `file=chunk.m4a` as `audio/mp4`; later chunks carry the first chunk's detected language and the previous chunk's last 200 characters as `prompt`. 408, 409, 429, 5xx and network errors are retried 4 times (2, 4, 8 s, or `Retry-After` up to a minute), then `speech_api_unavailable`, a capacity code that retries with the job's backoff. Any other refusal is `speech_api_rejected`, permanent, and shown on the track. A request has 10 minutes. The transcript names the model `{model}@{host}`.
- **Connect** (`POST /v1/settings/speech/connect`, owner session): `credentials` lists the API's models (a server without the route passes), `transcription` sends an embedded 11 s public-domain clip made by the real pipeline exactly as chunks are sent, so a provider that won't read fragmented MP4 fails at Connect, and `saved` stores the connection verified with its key sealed for its row. A failed step saves nothing. The key is optional for a keyless server on the owner's network; the base URL must be http or https with no user info or query, and isn't filtered for private addresses (D018). `PUT /v1/settings/speech` refuses `api` and `local_then_api` with `409 speech_not_connected` until one is connected; disconnecting returns the project to `local` in the same transaction, and the router moves queued transcriptions back.
- **Metrics:** `clipmux_speech_api_audio_seconds_total`, and the two codes as error labels.
- **Known gaps:** one transcription through the API at a time per serve, no spend cap or estimate (D075), and a word across a chunk boundary can be split.

## D081 Tracks in delivery and the Worker — accepted (2026-11-07)
Decisions within D063, made in P5-T15 (vectors first: 23 track cases in `internal/delivery/testdata/vectors.json`, passed by the Go handler and the Worker):
- **The route record gains `tracks`**: every track that can be served (stored and publishing, ready, or retired within the window) by ID, with `kind`, `language`, `name`, `duration_ms`, `key_version` and `listed` (a subtitles track publishing or ready). Omitted when empty, so existing records and their ETags don't change and `v` stays 1. Both builders (one ID's and the reconciler's whole project) fill it alike.
- **Publication:** a stored track writes a `route_sync` row with `op: "track"` per live playback ID. Once every row of the track is confirmed, and only once its asset is ready, the track is ready; with no playback ID it is ready at once. An asset becoming ready finishes the tracks that waited for it, so video is always ready first. A track becoming ready retires its kind's previous ready track first (one ready per kind), which stays in records unlisted, and queues a refresh so the master stops listing it.
- **Paths, under the playback ID:** `tracks/{id}.m3u8` (subtitles only) is generated, `no-store`: one VOD segment for the whole duration, times from integer milliseconds so the Worker writes the same bytes. `tracks/{id}.vtt` is decrypted and served `text/vtt; charset=utf-8`, private or public with the ID's policy, immutable; it is also the sidecar URL. Chapters are served at the same VTT path and never listed; they have no playlist. A thumbnail token gets 403 there. The branch comes before the output lookup, which would otherwise read `tracks` as an output ID; anything else under `tracks/`, any `.vtt` elsewhere, and unknown, deleted, errored, preparing or other assets' tracks are 404.
- **The master** lists each listed subtitles track, in ID order, as `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME=…,LANGUAGE=…,AUTOSELECT=YES,DEFAULT=NO,URI="tracks/{id}.m3u8"` before the first variant, and adds `SUBTITLES="subs"` to every variant.
- **The Worker** caches the sealed bytes at the edge and decrypts after the cache with WebCrypto AES-GCM, so no plaintext is cached; an `origin`-backend asset's VTT comes decrypted from the built-in handler and isn't cached. The vectors carry each track's sealed bytes, made once by `-update`, so the Worker decrypts exactly what Go sealed; the workerd test runs it in Cloudflare's runtime. Its version changes, so `GET /v1/cloudflare` reports `needs_redeploy`.
- **Delivery's cache** (D072) holds each asset's servable tracks and drops them on every route invalidation, so a track made ready or retired shows at the next request on one node.
- **Moves and retention:** the move job copies `media/{asset}/tracks/` with the outputs (its cleanup deleted everything under `media/{asset}/` before); retired tracks go with the output window: marked deleted with a route refresh, then their file is deleted.
- **Still to see:** whether Safari and ExoPlayer accept a single-segment WebVTT playlist as long as the video (P5 open question 8) is shown only by `exit-safari.yml` and `exit.yml` (P5-T20); the fallback is 6 s segments sliced at delivery.

## D082 Tracks in the integration API and webhooks — accepted (2026-11-07)
Decisions within D063, made in P5-T16:
- **`generate: {subtitles?, chapters?}`** on `POST /v1/uploads`, `POST /v1/assets` (import and agent source) and an agent's attach creates preparing tracks in the asset's own transaction; chapters imply subtitles. It is omitted from the idempotency body when empty, so earlier keys replay unchanged.
- **The asset view** gains `duration_seconds` (the current output's, now set as each output commits, which also gives a transcription's disk rule its duration; null before) and `tracks`, oldest first, retired and deleted hidden, `publishing` shown as `preparing`. Listing builds a page in three queries (D072 said two).
- **Routes:** `GET /v1/assets/{id}/tracks/{track}` (chapters once ready), `GET …/transcript` (`409 track_not_ready` before ready, words with `?words=true`, `text` the segments joined with whitespace collapsed), `POST /v1/assets/{id}/tracks {kind, supersede?}` (a new track, or one still waiting for its job; `409 asset_not_ready`, `no_audio`, `subtitles_not_ready`, `chapters_not_connected`, `job_active`; `supersede` cancels only a track job, never a move or delete, and deletes the preparing track it was making; idempotent), and `DELETE …/tracks/{track}` (cancels its job, deletes the file and transcript at once, refreshes the routes, chapters waiting on deleted subtitles error `subtitles_failed`; `200` or `202`).
- **Webhooks:** `video.asset.track.ready`, `.errored` (with the job when one caused it, and one for each chapters track the cascade errored) and `.deleted`, carrying the asset view and `track: {id, kind, error_code?}`, written after the asset's version moves.
- A chapters regeneration queues a `chapters` job that serve runs from P5-T17.

## D083 Chapters in serve — accepted (2026-11-07)
Decisions within D063, made in P5-T17 (`internal/chapters`; answering P5 open question 11):
- **The model picks, serve derives.** The model answers `{"chapters": [{"start_segment_id", "title"}]}` and nothing else; every time comes from the transcript: the first chapter starts at 0, each ends where the next starts, the last at the duration. Local checks (structured outputs can't express them): 1–10 chapters, only segment IDs the prompt showed (segments with text), the first at the first of them, in order with rising start times, no duplicates, titles 1–100 characters after collapsing whitespace, any script.
- **The transcript is untrusted data.** It goes in the user turn between `<transcript>` and `</transcript>`, one `id: text` line per segment, with `<` and `>` in speech replaced by `‹` and `›` so speech can't close the block; the rules are only in the system prompt. A prompt over 512 KB is `transcript_too_large`, never truncated. A transcript with no speech gets `[]` without a call.
- **Two adapters behind a consumer-side `Model`** (`Complete(ctx, Prompt) (json.RawMessage, error)`, errors carrying `Code` and `Transient`). **OpenAI-compatible** on the standard library: `POST {base}/chat/completions` with a strict `json_schema` response format, or `json_object` for a server without one (Connect finds out and stores it in `ai_connections.response_format`); a Markdown fence around the JSON is tolerated; a `refusal` is `model_refused`. **Claude** on `github.com/anthropics/anthropic-sdk-go/v2` (v2.7.0): `Messages.New` with the owner's model (default `claude-opus-5-5`), a system prompt, `output_config` effort `low` and a `json_schema` format, no thinking, sampling or prefill. The client is built with `option.WithoutEnvironmentDefaults()`, so serve's own `ANTHROPIC_*` variables never reach it, `WithMaxRetries(0)` and a 120 s timeout. `stop_reason: refusal` is a permanent `model_refused` with no fallback model: the owner's choice of model is the one that runs (open question 11). Both: 408, 409, 429, 5xx (529 included) and the network are transient (`llm_unavailable`); any other answer is `model_rejected`. Credentials are checked with `GET {base}/models` (a 404 passes) or `Models.Get`.
- **One corrective re-prompt.** An invalid answer is sent back once with the reason appended to the same user turn; still invalid is `invalid_model_output`.
- **The job runs in its own leader loop** (`chapters`, every 5 s, a 15-minute pass), not the control loop the plan named: a model can take minutes and a delete shouldn't wait behind it. `ClaimControlJob` takes the kinds each loop runs. The job leases itself for 10 minutes. Transient failures count against `failed_count` and retry after 30 s and 2 minutes; the third ends the track `llm_unavailable`. The drain hands a job in flight back claimable with no try spent. The commit locks the job (a job cancelled meanwhile commits nothing), then asset and track, seals the chapters VTT (one cue per chapter, the same renderer as subtitles) under the track's key, writes it, stores `chapters` and publishes like a transcript, so chapters are ready only once their routes are.
- **Track jobs chain.** `tracks.QueueNext` starts the asset's next track job once none is active: chapters for a preparing chapters track whose subtitles are ready, else a transcription that waited. It runs when a track becomes ready, when a transcription or chapters job ends, and after a speech-API commit, so an upload asking for chapters goes upload → video ready → subtitles ready → chapters ready with no other trigger. Chapters with no ready subtitles end `subtitles_failed`; with no connection `llm_not_connected`.
- **Connect** (`GET /v1/settings/chapters`, `POST`/`DELETE /v1/settings/chapters/connect`, owner session): `credentials`, `structured_output` (chapters of a fixed three-segment transcript) and `saved`, the key sealed in `ai_connections` beside the speech API's. A failed step saves nothing. The base URL isn't filtered for private addresses (Ollama on a LAN, D018). Disconnecting leaves ready chapters playing.
- The six codes are metric labels. Tests: the grounding and prompt rules ported from `chapter.spec.ts`; both adapters' requests and error classes against httptest fakes (the Claude one through `option.WithBaseURL`, with serve's environment set to other values); on Postgres, the chain from a transcript commit to ready chapters with ciphertext in storage, retries to `llm_unavailable`, a Claude refusal, JSON-mode fallback at Connect, the drain, a cancelled job, and the key in no row of any table and no log line.

## D084 The player, the dashboard and the setup test for tracks — accepted (2026-11-07)
Decisions within D063, made in P5-T18 and T19:
- **`@clipmux/player` 0.4.0.** Subtitles need nothing new: hls.js lists the master's subtitles rendition, the Video.js skin's captions button offers it, and hls.js loads its playlist and VTT through `config.loader`, the refresh loader (D029). Chapters are never in the master, so the app names the track: `chapters-track="trk_…"` fetches the chapters VTT with the session's token (a 401 refreshes it once, like the loader), parses it to seconds (`player.chapters`, the `clipmux-chapters` event), and adds its cues to a hidden chapters text track of the inner `<video>` (`addTextTrack`), which the skin's time slider draws as segments. Two traps found in Chrome: a light-DOM `<track>` is copied into `<hlsjs-video>`'s video without its cues, and a `<track>` with no `src` empties its cues when its load fails; and hls.js clears every text track's cues as its manifest loads, so the cues go in after `loadedmetadata`. `trackUrl`, `fetchTrack` and `parseChapters` are exported for other players.
- **The dashboard** shows an asset's tracks (status, language, error code), its transcript and chapters, regenerates (the old track plays until the new one is ready) and deletes them, and polls again after a change. Upload has subtitles and chapters toggles. A **Captions** page holds the speech mode, Connect a speech API and Connect an LLM (Claude or OpenAI-compatible), each saying what leaves the owner's infrastructure. The Workers page shows each PC's speech backends. All through the public API with the owner's session.
- **The setup test's sample has speech**: 11 s of JFK's public-domain inaugural address (the clip the speech tests use) over the test pattern, 655 KB (`toolchain/make_sample.sh`). "Americans" is heard at 1.22–1.98 s.
- **Four checks** follow the playback checks: `subtitles_ready` (within the encode timeout), `play_subtitles` (the master lists the rendition, its playlist names the VTT, and a cue holding "americans" covers 1.6 s ± 0.5 s, so whisper.cpp's word cues and a speech API's sentence cues both pass), `thumbnail_token_refused_track` (403) and `stored_track_encrypted`. They run only when something can transcribe as the test starts (the built-in worker or an online PC that reported speech, or a speech API in a mode that uses it, `ProjectCanTranscribe`) and otherwise pass as skipped, saying why. A first-boot test usually skips them: the built-in worker proves speech after it starts.
- **Not required in a provider's setup test**, where the plan wanted it: the Phase 4 harness's pods run with speech off, and requiring it there would bar every provider that hasn't fetched the 0.9 GB models. Verified providers still count as able to transcribe for routing (D078); `exit-providers.yml` now transcribes 11 s of English on each real provider and logs the time, which is the proof that waits for T00's secrets.
- Tests: the player's parsing, URLs and refresh in Node; on the real binary in Chrome (the browser fixture now connects a fake speech API and a fake LLM and uploads with chapters), the cue at its time, chapters on the time slider, the VTT after a refreshed token, the dashboard's track pages, Connect an LLM, the upload toggles and the speech column; the four checks on a hand-built asset (pass, out of time, plain storage, an errored transcription); and the whole setup test with real whisper.cpp on this machine's CPU, every check passing.

## D085 Phase 5 exit: the CI half passed; real accounts, devices and drills pending — accepted (2026-11-07)
Made in P5-T20, the exit test of [plans/P5-production.md](plans/P5-production.md). `internal/e2e/phase5_db_test.go` runs on the real binary with a fake OpenAI-compatible speech API and chat API on loopback (`tracks_db_test.go`); `exit.yml` runs it with Phases 1–4, installs promtool (`prom/prometheus:v3.5.0`) and soaks for 30 minutes on its weekly run.
- **Passed here, on the real binary:**
  1. No secret in any log: every real-binary test checks its JSON logs (D074); the Phase 5 tests also find neither AI key in any row of any table or any log line.
  2. SIGTERM with a speech request held at the API, a 720p encode under way and a `/v1/events` long-poll open: `/readyz` 503 within 1 s, the long-poll answers within 2 s, both attempts end `lost` with `worker_stopped` (`lost_count` 1, `failed_count` 0, the jobs queued), serve exits 0 well within 30 s, and after a restart the encode commits and the subtitles become ready. Not covered: a throttled segment GET and a 32 MiB part PUT finishing their bytes.
  4. The 6th bad setup token and the 11th wrong pairing code from one address (each with a different spoofed `X-Forwarded-For`, ignored) and the 61st of 61 concurrent `POST /v1/uploads` on one key get 429 with `Retry-After`, while a second key still creates; 40 viewers on one address play a signed video with no refusal.
  5. The 60 s soak: 50 HLS clients on 3 assets with uploads and listing served 723,384 playback requests with no 5xx and a route-cache hit ratio of 0.9999 (the new `clipmux_delivery_cache_lookups_total{table,result}`), and goroutines and heap returned to their baseline.
  6. `/metrics` on `CLIPMUX_METRICS_ADDR` shows the committed attempt and is 404 on the public listener; promtool checks it in CI.
  7. (half) A schema newer than the binary refuses to boot with `schema_newer`.
  9. (half) Every dashboard suite in Chromium records CSP violations from the page and the console and has none.
  10. With the speech API holding the transcription, the asset is ready and a second upload becomes ready; `video.asset.ready` is written before `track.ready` for the subtitles, then the chapters.
  11. A silent video gets `no_audio` with no track job, chapters `subtitles_failed`; a transcription the API refuses errors only its track (`speech_api_rejected`, the API path's code for it; whisper's is `transcribe_failed`) and the chapters (`subtitles_failed`), the asset staying ready.
  13. A re-encode committing while the transcription is held doesn't disturb it: one attempt, committed, reading the output it started on; a second re-encode keeps the same track listed in the new master with byte-identical cues.
  14. (hls.js) In Chromium the cue the speech API heard at 1–3 s is active within ±0.5 s of its time, and the captions button lists it; chapters show as two time-slider segments (D084).
  15. (half) A thumbnail token gets 403 on `tracks/` (setup test, vectors); after a mid-play token refresh the subtitles and chapters VTTs load with the new token; storage holds ciphertext.
  16. (delete) Deleting leaves no live track, no transcript, no file under the data directory, and a 404 for the VTT. Track rows stay, marked `deleted`, as history, as assets do.
  19. The LMS parity contract (`TestPhase5ExitLMSParityContract`), against the shapes in `LMS/api/src/test/harness.ts`: track states map to `pending`, `completed` and `failed`; the transcript is served the moment its track reads ready, as `{language, duration_ms, text, segments: [{startMs, endMs, text}]}` with literal values; the sidecar VTT loads as `text/vtt; charset=utf-8`; chapters convert to `[{startTime, endTime, title}]` in seconds; a signed webhook arrives for each stage (ready, both tracks ready, both errored on a failure, deleted); delete leaves nothing.
  20. (api mode) A transcription in `api` mode is routed `speech_api` from the start, and a SIGTERM mid-request hands it back `worker_stopped` (check 2). `local_then_api`'s routing is proven on Postgres (D080).
  18. Both chapters adapters run against httptest fakes in CI (D083).
- **A deadlock found and fixed by check 4:** `uploads.Create` looked up its store through the pool while holding its transaction, so a burst of creates larger than the pool on a cold store cache waited on each other forever (61 at once against the rig's 8 connections hung serve). The two track commits and the upload sweeper did the same. `stores.Set` gains `GetIn` and `ForAssetIn`, which read through the caller's transaction, and those four callers use them.
- **Pending, each recorded when it passes:**
  - Real accounts: `exit-llm.yml` (checks 18 and 20 on Claude, OpenAI's chat API and OpenAI's speech API; secrets `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`) and `exit-providers.yml`'s speech run on RunPod and Modal (T00, D084).
  - Devices: Safari's native HLS (`exit-safari.yml`) and ExoPlayer (`exit.yml`'s Android job) showing the subtitles track (check 14 and open question 8, the single-segment WebVTT playlist), which need their suites extended and a run on those runners.
  - Drills not yet automated: retention on the real binary (check 3; its rules pass on Postgres and SeaweedFS, D069 and D070), backup and restore (7), secrets, content-key and signing-key rotation on the real binary (8; on Postgres, D071), TOTP in a browser (9), a revoked ID's track at the Cloudflare edge within 65 s and a move to R2 that plays its subtitles (15, 16; the Worker passes the shared vectors in workerd, D081), and a transcription routed to RunPod with `primary_offline` on the real binary (17; on Postgres, D078), which needs pods with speech.
  - Detection on the owner's English, Hindi and Hinglish recordings (12), which aren't recorded yet.
Consequence: as with Phase 4 (D062), the phase exits in CI with these carried as T00-style tracks; the LMS cutover (plans/lms-migration.md) can be planned, and should not switch production until the device and real-account runs above pass.

## D086 Licence: AGPL-3.0 core, MIT for what apps embed — accepted (2026-11-07)
Context: D005 kept the repository private, with no LICENSE file, until the licence was decided as part of the launch. The options on record were Apache-2.0 (the archived `vod`) and AGPL-3.0 core with an MIT SDK and player (clipmux-lms D021, proposed). The owner chose the second while planning Phase 6 ([plans/P6-launch.md](plans/P6-launch.md)).
Decision:
- **AGPL-3.0** for the repository: the server, the worker and the CLI, one binary.
- **MIT** for `packages/player`, `packages/uploader` and `worker/`, each with its own `LICENSE` and `"license"` field, so apps and their deployments embed them under no copyleft.
- **FFmpeg** stays a GPLv2+ build (`--enable-gpl`, x264 and x265, no `--enable-nonfree`), run as a separate process and never linked. The image ships the agent tarball's `licenses/` directory (D061) and a source offer naming the pinned source digests in `toolchain/versions.env` and the scripts that build them.
Consequence: P6-T06 adds the files. Contributions need a DCO or a CLA (P6 open question 1), and a CLA is what would keep dual licensing possible.

## D087 Phase 6 scope: launch, with the LMS on ClipMux first — accepted (2026-11-07)
Context: Phases 1–5 have exited in CI (D040, D041, D050, D062, D085). Nothing for a stranger exists yet: no public image or release, no installer, a dashboard with no design layer, a README that says "nothing usable yet", and no licence file. The owner chose the scope; the plan is [plans/P6-launch.md](plans/P6-launch.md), which absorbs the checklist that was `plans/launch.md`.
Decision:
- **Next phase:** Phase 6, "Launch", after Phase 5. Its exit is the launch checklist. A fresh VM installs in one line and plays its sample with no build, and an agent pairs from the dashboard's command. Every pending real run is recorded (D088). The LMS has served production through its soak window. Then the repository goes public.
- **The LMS moves before launch,** not after: real production traffic on ClipMux before strangers install it. Its instance is installed with the one-liner on a release candidate, which dogfoods the installer. `clipmux.com` is then free for the docs site, which settles the domain plan of D006.
- **Tracks:**
  - prove what's built (A);
  - distribution: the licence, releases, installers and npm (B);
  - the dashboard, designed first and signed off by the owner (C);
  - docs and agent skills (D);
  - then the cutover and launch day.
- **Agent skills** are `SKILL.md` files under `skills/`, installed with `npx skills add visheshgubrani/clipmux` and evaluated with skill-creator. No MCP server and no OpenAPI spec in v1.
- **Before the phase starts,** Phase 5's branch is pushed, passes GitHub CI and merges to main.
Consequence: the integration API only grows; the owner routes the dashboard lacks (password change, library search, disconnecting Cloudflare, storage configs) come in P6-T12.

## D088 The launch waits on every real run — accepted (2026-11-07)
Context: several phases exited in CI against fakes, with their real-account, device and drill runs carried as T00 tracks: Phase 2's Cloudflare zone (D041), Phase 3's by-hand checks (D050), Phase 4's providers (D062), and Phase 5's LLMs, devices and drills (D085). The owner was asked whether v1 could ship those paths as beta.
Decision: no path ships as beta. Each of the following passes, and is recorded as a decision, before the repository goes public:
- Cloudflare on a real zone;
- RunPod and Modal on real accounts, for encode and speech;
- `exit-llm.yml`;
- Safari and ExoPlayer showing subtitles;
- the retention, backup-and-restore, rotation and TOTP drills;
- a transcription on RunPod;
- the by-hand checks.

These are P6-T01 to T05.
Consequence: Track A is the critical path. Its secrets, accounts, zone and hardware are owner actions and start at once.

## D007 Shaka Packager 3.9.3 — accepted (2026-11-05)
Decision: pin v3.9.3 (`toolchain/versions.env`), the version clipmux-lms runs with AES-128 in production, which needs the init-segment workaround for Shaka issue 1616. Move to a release that fixes 1616 when one exists, re-running the Phase 1 player tests.

## D089 Real Cloudflare staging preparation — partial (2026-11-08)
The owner selected retained staging at `app-test.clipmux.com` and
`media-test.clipmux.com`, backed by a local app through a dedicated Tunnel,
with the `clipmux-staging` bucket and the currently unused `clipmux-media`
Worker. The existing `media.clipmux.com` service must remain untouched.

- The supplied credential is an active **account-owned** API token. Cloudflare
  answers 401 at `/user/tokens/verify` and accepts the account verification
  endpoint. Connect now falls back on 401/403, selecting the supplied account
  or the only accessible account. The endpoint and Connect sequence have
  regression tests, the latter against real Postgres.
- The real app's authenticated Connect request passed `token`, `account` and
  `zone`, then failed `workers_paid`. The account's subscriptions list R2 Paid
  but no Workers Paid. No cloud resources or DNS records were changed. This
  records a prerequisite failure, not a Phase 2 exit.
- `deploy/cloudflare-staging.py` retains isolated local app state, private
  credentials and a playable 70 MiB multipart fixture. The opt-in browser
  fixture targets the retained install without deleting its database; the
  live reporter suppresses raw failures that might expose signed URLs.
- All 79 Worker tests passed, including workerd, vectors, ranges and cache
  behavior. The account-token regression passed against Postgres. Browser
  type checking and build passed. The local Chromium upload/resume regression
  passed. `make lint` passed with Go 1.27.3; the installed 1.27.2 triggers
  GO-2026-6809 in govulncheck. This does not establish real-R2 playback.
- The real custom-domain, browser upload/playback, captions, move/offline,
  revocation and key-rotation acceptance checks remain open in known gaps.

## D090 Phase 2 real-account exit — passed (2026-11-08)
The owner purchased Workers Paid and approved retaining a dedicated staging
bucket, Worker and Tunnel. This closes the real-account part of D041 and
P6-T02; other launch gates remain open.

**Resources:** account `49b962322ece3363f6495da349dd9d9e`, zone
`2f39459c928f5c09fced515c1a27ce56`; app `app-test.clipmux.com`, Worker custom
domain `media-test.clipmux.com`, bucket `clipmux-staging`, script
`clipmux-media`, Tunnel `69fccb22-af9a-41ae-aa28-cf9d940f82c3`. The previous
`media.clipmux.com` domain remains on `delivery`, with its original DNS record.
Private state, resource metadata, versions and sanitized logs are retained in
ignored `data/cloudflare-staging/`; no credentials are in this record.

**Real checks that passed:**
- The dashboard connected twice, including the paid-plan check, bucket/CORS,
  script upload, route publication and custom-domain attachment. The setup
  test passed through the public Worker URL, including direct storage's
  encrypted-segment check. Five Chromium tests passed: Connect/reconnect,
  hls.js renditions/audio/keys and audience checks, player token refresh,
  denied refresh, and a valid 70 MiB upload interrupted after part 1 and
  resumed with only parts 2 and 3. The uploaded asset became ready in R2.
- Existing app credentials, signing key, playback ID and media base URL played
  the filesystem baseline through Worker origin fallback. Generated subtitles
  were delivered through the Worker. Only speech used a deterministic local
  provider; Cloudflare, storage, encoding, projections and delivery were real.
- With requests warmed first, the revoked playback ID's track returned 404
  immediately (rounded to seconds); a revoked signing key returned 403 in
  6 seconds. Both are below the shipped 60-second cache plus 5-second margin.
- Content-key rotation redeployed the ring, preserved old-output playback,
  and encoded a new R2 output using the new key version. Real R2 delivery
  passed HEAD, an exact eight-byte Range/Content-Range check, CORS, nosniff,
  and ciphertext checks, then decoded audio and video.
- The filesystem baseline was re-encoded to a fresh immutable output, moved
  with its subtitles to R2, and decoded with serve stopped, without first
  fetching that new output. The acceptance test passed in 301.48 seconds.
  With the app, Tunnel connector and databases stopped, four more Chromium
  tests passed: hls.js, player token refresh, denied refresh, and rendering
  the expected subtitle cue. Playback therefore did not depend on the local
  control plane or a cached copy of the fresh moved output.

**Fixes found by the real run:** account-owned tokens need account-scoped
verification (D089). Reconnect must inspect the hostname's Worker attachment
and preserve DNS already managed by that same Worker; Cloudflare rejects
attempts to delete it via the DNS API. Regression tests pass against real
Postgres and S3 test storage. The live browser harness waits for the Connect
response, allows asynchronous fixture loading, and accepts newer content-key
versions. All 79 Worker tests, TypeScript checks/build, and `make lint` passed;
lint used Go 1.27.3 because the host's 1.27.2 triggers GO-2026-6809.

**Operator settings:** Browser Integrity Check produced error 1010 for API
clients. With explicit owner approval, configuration rule
`bcebd8d4f11245ca9069d8554dfffaac` in ruleset
`bd76d4f27f894f37bf500df008d89151` sets `bic: false` only on the two staging
hostnames. The zone-wide setting and production hostnames were preserved.
Tunnel transport is HTTP/2: QUIC stalled despite passing connectivity checks.
Cloudflare's [current CDN terms](https://www.cloudflare.com/service-specific-terms-application-services/)
were reviewed; this run used the paid Developer Platform and R2 services.

Local processes are stopped; cloud resources, database volumes and private
credentials are retained. The move's delayed source-cleanup phase resumes
when serve next starts. Real-account CI remains an opt-in local harness;
main/weekly automation and real speech-provider/device tests are separate gaps.

## D091 Licence files, a DCO, a clean history and a security review — accepted (2026-11-08)
P6-T06, carrying out D086 and answering P6 open question 1:
- **The files.** `LICENSE` is the AGPL-3.0 text from gnu.org. `packages/player`, `packages/uploader` and `worker/` each have an MIT `LICENSE` and `"license": "MIT"`; the root, the dashboard and `e2e/web` say `AGPL-3.0-only`.
- **`toolchain/licenses.sh`** writes the licences once for both binary forms. It writes ClipMux's AGPL, FFmpeg's and whisper.cpp's notices and build manifests, `packages.txt` (every Debian package with its source package and version), a README, and `SOURCE.md`. `SOURCE.md` is the written offer (GPLv2 §3(b), three years). It names every pinned source and digest from `toolchain/versions.env` and the scripts that build them.
  - The image puts them in `/usr/share/doc/clipmux/licenses/` and carries OCI labels, with `licenses="AGPL-3.0-only AND GPL-2.0-or-later"`.
  - The agent tarball puts them in `licenses/`, beside the bundled libraries' copyright files.
  - `image.yml` and `toolchain/check_agent.sh` fail if either form is missing them.
- **A DCO, not a CLA** (open question 1). `CONTRIBUTING.md` asks for `git commit -s`. This follows the plan's recommendation: with no CLA, dual licensing later needs every contributor's consent, so the owner should confirm it (or switch to a CLA) before the first outside contribution. Enforcing the sign-off (a DCO app or check) is left for when outside contributions start.
- **`CODE_OF_CONDUCT.md`** is Contributor Covenant 2.1, from `vod`, reporting through a private GitHub report. **`SECURITY.md`** points at GitHub private vulnerability reporting, with a 3-day acknowledgement and a 90-day window.
- **The history is clean.** gitleaks v8.28.0 and trufflehog 3.90.8 scanned all 99 commits on every ref, and neither found a live secret ([security-review.md](security-review.md), "History scan"). Every hit is a test vector or the dev SeaweedFS credential; trufflehog verified none. Nothing needed rotating or scrubbing. `.gitleaks.toml` allows exactly those files. (Amended 2026-11-14: the scan was meant to run in `ci.yml` on every pull request, but the lean CI job (74634dc) dropped it; gitleaks runs locally before merging, per `.github/README.md`.)
- **The threat table was reviewed against the code** ([security-review.md](security-review.md)). Of 31 rows, 22 hold and 9 hold in part; none is missing. Six findings go beyond the table, two of them medium:
  - without Landlock, the built-in worker's tools can read serve's environment;
  - the global sign-in failure limit lets anyone lock the owner out.
  All six are in known gaps until they are fixed. None blocks a release candidate; both mediums should be fixed before v1.0.0.

## D092 npm: both packages publish from release.yml with provenance — accepted (2026-11-08)
P6-T09:
- `@clipmux/player` (0.4.0) and `@clipmux/uploader` (0.3.0) drop `"private"`. Each package now has a licence, repository, homepage and keywords. Each ships `dist`, its `src` (for the source maps, without tests), `LICENSE` and `README.md`, and has `publishConfig: {access: public, provenance: true}`.
- **Versions are the packages' own**, not the server's tag. On every `v*` tag, `release.yml`'s `npm` job builds, type-checks and tests both packages. It checks each tarball's contents (licence, README and entry point present, no tests), then publishes each version npm doesn't have yet. A prerelease version goes to the `next` dist-tag. Run by hand, the job is a dry run by default.
- It authenticates by npm trusted publishing (OIDC), or by the `NPM_TOKEN` secret until that is configured on npmjs.com. Provenance needs the job's `id-token: write`.
- The READMEs now show the backend half: signing ES256 playback tokens with `jose`, creating an upload with `passthrough`, and verifying `X-Webhook-Signature` in constant time.
- **The old releases** (`@clipmux/player` 0.1.0–0.1.2 and `@clipmux/uploader` 0.1.0–0.1.1, from the TypeScript ClipMux) are still live. The owner deprecates them after the first publish, logged in to npm: `npm deprecate '@clipmux/player@0.1.x' 'Replaced by the ClipMux rewrite; use >= 0.4'`, and likewise for the uploader (`>= 0.3`).

## D093 One-line installs for the server and a Linux agent — accepted (2026-11-08)
P6-T08:
- **`deploy/install.sh`**, POSIX sh. It checks Docker, Compose v2, the architecture, free disk (4 GiB at least, with a note under 20 GiB) and, on a first install, ports 80 and 443. It downloads `compose.yml` and `Caddyfile` from the release and checks both against `SHA256SUMS`. It writes `.env` (0600) with `CLIPMUX_VERSION`, a `POSTGRES_PASSWORD` and a `CLIPMUX_SECRETS_KEY` from `openssl rand` or `/dev/urandom`. Then it runs `docker compose up -d --wait` and prints the setup URL, the token from the logs, and the secrets key with "store this off the host".
  - Re-run, it upgrades: it keeps `.env` and moves only `CLIPMUX_VERSION`.
  - `--uninstall` keeps the volumes and `.env`; `--purge` deletes them and the directory.
  - It runs nothing it downloads. The whole script is inside `main`, so a truncated pipe runs nothing.
  - The release workflow (P6-T07) replaces `@CLIPMUX_VERSION@` with its tag; unreplaced, the script resolves `/releases/latest`.
- **`deploy/compose.release.yml`** is the operator compose, published as `compose.yml`. It is `deploy/compose.yml` with `image: ${CLIPMUX_IMAGE}:${CLIPMUX_VERSION}` in place of the build, and `check_install.sh` fails on any other drift. Both files take `CLIPMUX_HTTP_PORT`, `CLIPMUX_HTTPS_PORT` and `CLIPMUX_ADMIN_PORT`.
- **`deploy/install-agent.sh`**, for Linux with glibc 2.39 or newer. It downloads `clipmux-agent-linux-{amd64,arm64}.tar.gz`, checks it against `SHA256SUMS`, and swaps it into place whole.
  - As root, it installs to `/opt/clipmux-agent` with a system unit running as a `clipmux-agent` user in `video` and `render`.
  - Otherwise, it installs to `~/.local/share/clipmux-agent` with a user unit and asks for lingering.
  - The unit pairs on its first start with `--pair`, as the Docker command does. Once `agent.json` appears, the installer rewrites the unit without the spent code.
- **The Workers page's Linux tab** shows the one-liner. Invitations gain `install_command`, which names the server's own release (`releases/download/vX.Y.Z/…`) or, for a build that isn't a release, `releases/latest/download/…`. Docker stays the path for macOS and Windows; pinning its image tag is P6-T07.
- **Tests.** `toolchain/check_install.sh` serves two release candidates from the host and checks, in order:
  - a tampered `compose.yml` is refused;
  - the first installs and prints a token; the owner registers through Caddy, saves the URLs, and the setup test passes;
  - an agent installs and pairs from `install_command` and shows online, and the spent code leaves its unit;
  - the upgrade keeps the secrets key, the owner's session and an API key, and the setup test passes again;
  - `--uninstall` keeps the volumes, and `--purge` removes them.

  `install.yml` runs it on a clean runner, as root, with images in a registry on the runner. ci.yml shellchecks the scripts. Against a real `-rc` on ghcr it waits for P6-T07.
- **Run on this machine (2026-11-09).** `check_install.sh` passed end to end against `v0.0.0-ci.1` and `ci.2` built from this tree, pushed to a local registry, with HTTPS on 8443 because the host already serves 443. It covered the tampered file, install, the setup test through Caddy, the upgrade keeping the session, key and secrets key, uninstall and purge. Its first runs failed on the check itself: under `pipefail`, `… | grep -q` fails when grep exits early, so the check now reads into a variable.
- In a clean `ubuntu:24.04` container, `curl … | sh -s -- --server --pair` installed the agent, verified the tarball and passed the glibc check. With no systemd, it printed the worker command, which paired with the dashboard's code; the agent showed online and sandboxed. The tarball passes `check_agent.sh`, licences included, and the image ships `/usr/share/doc/clipmux/licenses/`.
- **Not run here:** the systemd units (system and user), lingering, and the agent's `--uninstall`. The Install workflow covers the system unit as root, and the rest waits for a real VM (exit check 2).

## D094 The operator drills run on the real binary — passed (2026-11-08)
P6-T04, closing P5 checks 3, 7, 8, 9 and 17 (D085). `internal/e2e/drills_db_test.go` and `e2e/web/tests/totp.spec.ts` join `exit.yml` with the other exit tests. All passed on this machine:
- **Retention** (4 s). An output retired 9 days ago, a 2-day-old idempotency key, an expired pairing and a session idle 8 days are counted by `clipmux admin prune --dry-run` without deleting. `prune --files` then deletes them: the output is marked purged and its files are gone from storage, the pruned session gets 401, and the current output still plays.
- **Backup and restore** (7 s), as docs/operations.md says. It takes a `pg_dump -Fc` of a running install (Postgres 18's tools, or the `postgres:18-alpine` image's) and a copy of the data directory. Both are restored into a new database and a new directory. With another secrets key serve refuses to boot. With the same one, the owner's session, the API key and the signing key all work, the video plays, and a new upload encodes and plays.
- **Secrets-key rotation** (6 s). With v2 alone serve refuses, naming `CLIPMUX_SECRETS_KEY_PREVIOUS`. With v1 as the previous key it runs; after `reseal-secrets`, v2 alone serves and encodes, v1 alone is refused, and every content key secret is sealed under v2.
  - **Found and fixed:** the boot check of `projects.secrets_check` ran before the per-column check and answered only "sealed with a different secrets key". It now names the version it needs and the variable, as D071 intended (`TestANewKeyAloneNamesThePreviousKey`).
- **Content-key rotation** (4 s). `POST /v1/content-keys/rotate` on built-in delivery makes version 2 current. The next output uses 2 and the existing one keeps 1, and both play before and after a restart.
- **Signing-key rotation** (8 s). After `rotate-signing-key`, the old key's tokens play until its retirement (shown by `list-keys`) and the new key's play at once. After `revoke-key` the old key's tokens get 403 within the route cache's 30 s.
- **TOTP in Chromium** (1.1 min, its own install in the fixture). The owner enrols from the Account page with a code computed in the test, and the enrolling code can't be reused. The password alone then asks for a code, the next step's code signs in, and a wrong one is refused. A recovery code signs in once and is refused the second time.
- **A transcription on RunPod for `primary_offline`** (93 s). With "my PCs, else RunPod" and no PC online, the encode and then the transcription each woke a pod of the fake RunPod. The pod transcribed with whisper.cpp on the CPU (`pods.speech`), the English track was ready with JFK's words, and every pod stopped. It skips where `CLIPMUX_SKIP_SPEECH=1`; exit.yml has whisper.

## D095 exit-llm passed on real accounts — passed (2026-11-08)
Part of P6-T01. The owner chose to run the real-account suites on this machine with keys in the ignored `.env`, not as repository secrets in CI. `TestExitRealLLMAndSpeech` passed in 47 s, closing P5 checks 18 and 20 on real accounts (D085):
- OpenAI's speech API (`whisper-1`) transcribed the 11 s of JFK as English with word timings, and the subtitles were ready.
- Chapters through Claude (`claude-opus-5-5`, structured output) and OpenAI's chat API (`gpt-4.1-mini`, `json_schema`) each became ready, starting at 0.
- Neither key appeared in serve's logs.

Still pending for T01 and the Phase 4 exit (D062): `TestExitRealProviders` on RunPod and Modal. It needs a worker image the providers can pull (none is on ghcr yet) and a `GHCR_PULL_TOKEN` (read:packages), which `.env` doesn't have.

## D096 The real-provider run: RunPod half-passes, Modal never wakes — superseded by D097 (2026-11-09)
P6-T01, `TestExitRealProviders` run from this machine.
- **Setup.** Serve ran behind a Cloudflare quick tunnel that the owner started. The worker images were `ghcr.io/visheshgubrani/clipmux:t01-174b7b6-dirty` and `…-dirty.2`, built from the uncommitted tree and pushed to the private package with `GHCR_PULL_TOKEN`. Two runs. This does not record the Phase 4 exit.
- **RunPod: everything but the GPU encode passed.**
  - Connect passed every step, including the image pinned by digest (a digest-only reference is accepted, D058) and pulls with the stored registry credentials.
  - The setup test passed through a pod. Cold starts were 42 s and 25 s after the wake, well inside the 10-minute claim deadline.
  - With RunPod as the primary, 11 s of English were transcribed on the pod's GPU with Vulkan, in 28.4 s on one run and 231 s on the other.
  - Every pod stopped, by the database and by RunPod's own list.
  - **But the sample encoded on the CPU, not NVENC.** The worker's new `hardware.failed` (below) shows why: inside the media sandbox, CUDA's `cuInit(0)` fails with `CUDA_ERROR_OPERATING_SYSTEM`, for NVENC and CUDA decode alike. The pod enforced Landlock ABI 4. The sandbox's GPU allowance (`/sys`, `/proc/driver`, the `/dev/nvidia*` character devices) is missing something CUDA needs. Sandboxed NVENC was proven only on Modal (D051), so this is the first real NVIDIA host with Landlock on. The fix needs an NVIDIA machine to find the path. Until then every RunPod encode is CPU-only, at GPU prices.
- **Modal: never claimed.**
  - **Found and fixed:** Connect failed at `credentials` because the Go SDK reads `~/.modal.toml` and refuses with no `HOME`, as under the exit rig or a systemd unit. `newSDK` now points `MODAL_CONFIG_PATH` at an empty file, so serve never reads a user's profile either (`TestTheClientNeedsNoHomeAndReadsNoProfile`).
  - On the second run Connect passed. Then the setup test's wake never returned: no error and no log line for 15 minutes, until the setup test gave up (`encode`: "not ready after 15m0s"). The likely cause is Modal converting the ~850 MB registry image on first use while `Sandboxes.Create` blocks; this is unconfirmed.
  - **A sandbox was left running** after the test ended. The test's database went with it, so no reconciler would ever stop it before Modal's 6 h 15 min lifetime. It was found by listing the test's app through the SDK and terminated by hand; afterwards Modal listed 0 sandboxes and RunPod 0 pods. The exit test now stops every machine of its install through the provider at cleanup, pass or fail (`stopAll`).
- **Worker diagnostics.** `media.Hardware.Failed` carries each GPU backend's failed probe encode (FFmpeg's stderr tail), in the claim's capabilities and the worker's log. Before this, a GPU that failed its probe was silent.
- **Still to do for T01:**
  - find the sandbox rule CUDA needs, and re-run RunPod for NVENC;
  - find why Modal's wake blocks (bound the create, or pre-build the image), and re-run Modal for encode and speech, including whisper on the L4 (P5 open question 6);
  - then record the Phase 4 exit.

## D097 Phase 4 exit on real accounts — passed (2026-11-09)
P6-T01, closing D062's real-account half and the providers' speech run (D085). `TestExitRealProviders` passed on real RunPod and Modal accounts in 9 min 10 s. It ran from this machine behind the owner's Cloudflare quick tunnel, with the worker image `ghcr.io/visheshgubrani/clipmux:t01-174b7b6-dirty.6` in the private package.

**Results:**
- **RunPod:**
  - Connect passed every step, including a digest-only image reference and registry credentials.
  - The setup test passed in 1 min 23 s (cold start 39 s after the wake, inside the 10-minute claim deadline). The sample encoded on **NVENC** inside the media sandbox (seccomp, rlimits, Landlock ABI 4).
  - With RunPod as the primary, 11 s of English were transcribed on the pod's GPU (Vulkan) in 30.8 s, model fetch included.
- **Modal:**
  - Connect passed every step, including converting the image.
  - The setup test passed in 1 min 46 s (cold start 47 s) on **NVENC**.
  - 11 s were transcribed on Vulkan in 43.5 s, on the first of L4, A10G or T4 with capacity; which one isn't recorded (known gap).
- **Cleanup:** every machine stopped, by the database and by each provider's own list.
- **Diagnostics:** three short RunPod pods (an RTX 4090 twice, an L4) and two Modal sandboxes were started by hand to find the causes. All were removed, as were their registry credentials and a credential a failed run left behind. RunPod lists 0 pods and 0 credentials; Modal 0 sandboxes.

**What the real runs found, and the fixes** (each with a test):
1. **CUDA needs to write the tool's own `/proc/<pid>`.** Under Landlock, `cuInit` failed with `CUDA_ERROR_OPERATING_SYSTEM`; it passed once the tool could write `/proc/self`, and only that (`/proc/thread-self`, `/proc/driver` and `/proc/sys` didn't help). A GPU stage may now read and write existing files beneath its own `/proc/<pid>`, nothing else (`TestGPUToolsMayWriteTheirOwnProcEntries`). Modal runs under gVisor without Landlock, which is why D051 never saw this.
2. **RunPod's RTX 4090 hosts refuse NVENC** ("OpenEncodeSessionEx failed: unsupported device") even outside the sandbox, while CUDA decode works. The 4090 left `runpod.DefaultGPUTypes` (now L4, RTX A4000, RTX A5000).
3. **Modal appends the sandbox command to the image's entrypoint** (`tini -- clipmux`), as RunPod does. D062's unproven assumption was wrong: `clipmux /usr/local/bin/clipmux worker --once` exited at once, so no Modal sandbox had ever claimed. The command is now `worker --once`.
4. **The dispatch pass held every provider call in one transaction**, and the "60 s each" in its comment was never enforced. A blocked Modal wake held the pass until its 5-minute bound, was read as a shutdown and rolled back silently. Stops queued behind it waited, and a sandbox Modal created in a rolled-back pass was recorded nowhere (the leaked sandbox of D096). Now:
   - each wake or stop is its own transaction;
   - stops go before wakes;
   - every provider call has its own bound (`Service.CallTimeout`, 3 minutes).
5. **Providers may say "not yet".** A wake returning `dispatch.ErrPreparing` keeps its dispatch waking and is retried with backoff, with no failure counted. The claim deadline still bounds it.
6. **Modal converts a registry image on first use, for minutes**, and `Sandboxes.Create` waited for it with no bound. The adapter now builds with a 30 s wait and answers `ErrPreparing` while Modal converts. Connect starts the conversion as a new `image_conversion` step.
7. **Modal waits up to 21 minutes to place a GPU sandbox**, and its L4 queue stalls (as in D051). The Go SDK takes one GPU type, so the adapter tries `L4,A10G,T4` (the new default; all three have NVENC) in order, 45 s each. The SDK terminates an attempt that wasn't placed. With none free, the wake is `ErrPreparing`.
8. **Modal's SDK refused to start with no `HOME`.** It now reads an empty config, never the user's profile (D096).
9. **A failed wake isn't a broken provider.** RunPod answered "no instances available" once and woke a pod seconds later, but the setup test had already failed. It now fails at once only when a woken machine never claimed, or after three failed wakes in a row (`TestTheEncodeCheckWaitsOutOneFailedWake`).
10. **Errors that say why.** RunPod errors keep RunPod's reason, with every 20+ character token masked. Workers report each GPU backend's failed probe (`hardware.failed`).
11. **The exit test cleans up after itself.** It stops every machine of its install through the provider at cleanup, pass or fail.

Consequence: the Phase 4 exit is recorded; P6-T01 is done but for running `exit-providers.yml` in CI, which needs the repository secrets and `REAL_PROVIDERS` and is the owner's choice (they asked for local runs).

## D098 The dashboard design, signed off: design v2 and its toolset — accepted (2026-11-09)
P6-T10. The owner reviewed the clickable mock ([ClipMux dashboard mock](https://claude.ai/artifact/XMu8H6RAk33jUTVdaktxbf)) and the written spec, [design.md](design.md), and signed them off after one round of changes.
- **v1 → v2.** The owner asked for a leaner Home, "no KPI cards, it's looking bloated", and a redesign using the `frontend-design` and `shadcn` skills. The v1 mock read as the generic dashboard kit:
  - Inter;
  - everything boxed in identical cards;
  - icon-in-a-tinted-square tiles;
  - "A · B · C" meta strings;
  - a split sign-in panel with a headline.
- **v2 is rooted in the editing room.**
  - **Type:** Archivo, with page titles set wide (`font-stretch: 118%`); durations as tabular timecode.
  - **Colour:** one tally violet; status colours only with a word.
  - **Layout:** rows and sections instead of cards, and a radius ladder (4, 6, 8, 12 px).
  - **The poster is the one memorable element:** on black, with burned-in timecode, a tally lamp only when the video isn't ready, a globe only when it's public, and a playhead line while it encodes.
- **Home:** what's left to set up as rows, what needs attention, the newest videos, and a status line. No number tiles; numbers live on Analytics, in one chart panel.
- **Navigation:** a left sidebar, a sheet on phones.
  - Home, Videos, Analytics, Jobs; then Connections (Encoding, Delivery, Captions & AI, Apps & API); then Settings and the account menu.
  - `/library`, `/assets/:id`, `/workers` and `/account` redirect to the new pages. `/workers/connect` stays where the CLI opens it.
- **The toolset:**
  - **Components:** shadcn 4.23 on Base UI (`base-nova`), only the components used, restyled through tokens.
  - **Server state:** TanStack Query v5.
  - **Also:** lucide, recharts through shadcn's chart, `input-otp`, self-hosted Archivo, `tw-animate-css`.
  - **Rejected:**
    - Aceternity and `motion`: their effects work against "lean", and `motion` adds about 40 KB.
    - A form library, a date library and `cmdk`: not needed.
    - shadcn's new `cn` package: clsx and tailwind-merge stay.
  - All run under the CSP (D065). The player and the chart load only on the pages that use them.
- **Accounts stay as the architecture has them.** One owner; sign-up is the first-run account with the setup token, then sign-in only; a lost password is reset on the host.

Consequence: P6-T11 and T13 build v2; [design.md](design.md) is the spec and wins over the mock.

## D099 The owner routes the redesigned dashboard needs — accepted (2026-11-09)
P6-T12. The owner chose to build the small owner routes the new pages need now, with the analytics API deferred (D100). Every route is public `/v1` with the owner's session (no private path), tested on real Postgres, and in [api.md](api.md).
- **Asset titles.** `assets.title` (migration 00026) is optional, trimmed, one line and at most 200 characters. It is for people only: not unique, never in a URL, ignored by delivery.
  - `POST /v1/uploads` and `POST /v1/assets` take `title`, and the asset view shows it.
  - `PATCH /v1/assets/{id}` `{title}` (`assets:write`) renames an asset, and `""` or `null` clears the title. It is an integration route, so an app can keep titles in sync too. A change bumps `version` and writes `video.asset.updated`, like any other change an app might miss. The same title again changes nothing. A deleted asset is `409 asset_deleted`.
- **The library searches on the server.** `GET /v1/library` leaves deleted assets out unless `include_deleted=true` (or `status=deleted`). This fixes "No videos yet." showing beside a "More" button.
  - `q` matches an exact asset ID, or a title or passthrough containing it, case-insensitively. LIKE's `%` and `_` are taken literally.
  - Each item adds `poster_url`. With a public playback ID, it is `{base_url}/p/{pid}/poster.jpg`. Otherwise it uses a one-hour `t`-audience token signed with the internal key, the preview token's signer. It is null until a playback ID is playable.
  - `GET /v1/library/counts` gives `{ready, processing, errored, waiting_upload}` for the filter chips.
- **Changing the password**, `PUT /v1/session/password` `{current_password, new_password}`, re-checks the current password under the sign-in limiter. Every other session ends and this one stays, as when TOTP is turned on.
- **Disconnecting Cloudflare**, `DELETE /v1/cloudflare`, runs steps the way Connect does. Each step is safe to repeat:
  1. the media URL goes back to the public URL;
  2. delivery goes back to the built-in handler;
  3. the Worker's `_routes/` and `_keys.json` are deleted from the bucket. A request that still reaches the Worker is then refused, rather than served from records nothing updates any more;
  4. the connection is forgotten.

  The R2 store stays a storage config, and stays the default if it was: its assets play through this server. The Worker and its domain stay in the Cloudflare account, because no token is kept to remove them. It is `409 pinned` while `CLIPMUX_MEDIA_URL` names the Worker.
- **Storage configs.**
  - `GET /v1/storage` lists the configs with their asset counts.
  - `POST /v1/storage` adds a generic S3 bucket only after `stores.AddS3`'s test write, read, list and delete and CORS pass (`400 storage_check_failed` otherwise). R2 comes from Connect Cloudflare or `POST /v1/storage/r2` (amended in D110).
  - `PUT /v1/storage/{id}/credentials` checks new keys the same way before sealing them, and the open store is replaced at once.
  - `POST /v1/storage/{id}/default` picks where new uploads go.
  - `DELETE /v1/storage/{id}` retires a config that nothing needs. It is refused (409) for the filesystem store, for the default, while any asset may have files there (a deleted asset counts until its files are gone), during a move to or from it, and while Cloudflare delivery reads it.
  - **A removed config stays as a row** (migration 00027, `removed_at`), with its credentials dropped. Assets, move jobs and the delivery history keep naming it, and a hard delete would break those references or force rewriting history. It is never listed, made the default, or matched by Connect Cloudflare again.
- **API key names** were already stored and listed. A name is now trimmed and at most 100 characters, and a blank name is still `default`.
- Adding a bucket, rotating its keys, and disconnecting Cloudflare (which clears a bucket's routes) get the long request deadline, as Connect does.

## D100 Analytics enters the roadmap: the UI now, the API next phase — accepted (2026-11-09)
The dashboard redesign (P6 Track C, D098) draws an analytics page. Its API is deferred, so the page ships with its empty state, and ClipMux doesn't yet record views.
- **The contract is fixed now** as "Planned" in [api.md](api.md), so the page is built against it:
  - `GET /v1/analytics?range=24h|7d|30d|90d&app_id=` returns `totals`, the same for the `previous` range, a `series` of buckets, and `top_assets`.
  - `GET /v1/analytics/assets/{id}?range=` returns the same for one asset.
- **`unique_viewers` may be null.** Counting viewers needs an identifier that playback tokens don't carry today. The UI shows "—" rather than a guess.
- **Where the numbers come from is the next phase's decision.** The options are built-in delivery's logs, the Worker's analytics, or a player beacon. Whichever it is, it must keep D067's limits and never log tokens.
- The route answers 404 until then. The dashboard treats a 404 as "not available on this server", not as an error.

## D101 Track C passes: the dashboard redesigned and built — passed (2026-11-09)
P6-T11, T13 and T14, on the design D098 signed off and the routes of D099.
- **Built:**
  - **Design layer:** the v2 tokens and dark mode in `dashboard/src/index.css`, Archivo, the brand mark and favicon, and a pre-paint theme script.
  - **Components:** shadcn on Base UI in `components/ui/`, and the app pieces in `components/`: `PageHeader`, `Section`, `Poster`, the status badges, `QueryState` with `ErrorState`, `useConfirm`, the copy fields, the error boundary and the Upload dialog.
  - **Data:** TanStack Query in `src/queries.ts`.
  - **Pages:** every page in `src/pages/`.
- **The P6 exit check 6, as run:**
  - **Every dashboard suite runs twice,** in desktop Chromium and at 375 × 812 (`phone`). Each project has a fixture of its own, so suites that change their install start from the same state.
  - **New suites:**
    - apps: secrets once, signing keys, webhooks with deliveries and two-step rotation, disable;
    - connect: `/workers/connect` approve and deny;
    - settings: change password, with other browsers signed out;
    - delivery: S3 storage added, rotated and removed against SeaweedFS;
    - home;
    - analytics: the not-collecting state;
    - errors: each page's API failing, then Try again recovering;
    - upload: titled public upload → poster → search → private → delete.
  - **Existing suites** were moved to the new pages: dashboard, totp, tracks and workers.
  - **Every visited page is checked** for horizontal scroll (`noHorizontalScroll`) and for CSP violations (`watchCSP`). Every destructive action is confirmed in a dialog; no native `confirm()` is left.
  - **Result on this machine:** 38 passed across both projects (34 in one full run, then the last two suites again after fixing them), with 1 skipped: the live Cloudflare captions test, which needs a live config.
- **Found and fixed by the suites:**
  - A wrong current password (401 `invalid_credentials`) signed the owner out. Only `unauthorized` now does.
  - The routing form lost its "Saved." because it remounted on new data.
  - Toast and section names collided with dialogs and fields. Sections are now named by their headings, and the tests name the dialog they mean.
- **Not covered:** Firefox and Safari for the dashboard; the live Cloudflare suite was updated to the dialog but not run here.

Consequence: P6-T10 to T14 are done. Analytics' API (D100) is the next piece of work.

## D102 Phase 7 scope: sharing and analytics, before the LMS cutover — accepted (2026-11-10)
Context: two things the architecture promised are missing. Share links for signed videos were never built (architecture "Integration flow", known gaps), so a private video plays only through the owner's preview or an app's token. The analytics API's contract was fixed in D100, and the dashboard is built against it, but nothing records views. Phase 6's remaining tasks are releases, docs, devices, by-hand checks and the LMS cutover with its soak. The owner chose the scope and the timing; the plan is [plans/P7-sharing-analytics.md](plans/P7-sharing-analytics.md).
Decision:
- **Next phase:** Phase 7, "Sharing and analytics". Its exit has three parts:
  - a share link plays a signed video in a fresh browser, and stops when revoked or expired;
  - both delivery adapters count to the same vectors;
  - `/v1/analytics` gives exact numbers for scripted playback, through built-in delivery and through the Worker on a real zone.
- **It runs now, beside Phase 6's remaining tasks.** P6-T19, the LMS cutover, waits for Phase 7's exit. The release candidate the LMS installs then carries Phase 7, so the soak tests delivery counting on real traffic, and v1.0 doesn't launch with an empty Analytics page. This is a stated exception to "each phase starts only after the previous exit test passes", because Phase 6's exit is the launch itself.
- **Scope:** share links with an expiry and a plain HLS URL for other players (D104), and analytics counted at delivery and stored in Postgres (D103). No player beacon, no password on links and no embed this phase.
Consequence: P6's order changes (T19 after the P7 exit). The integration API only grows; the output format, the path grammar and the token claims don't change.

## D103 Analytics: counted at delivery, rolled up in Postgres — accepted (2026-11-10)
Context: D100 left open where the numbers come from (built-in delivery's logs, the Worker's analytics or a player beacon) and fixed the response. The owner asked whether a separate analytics database, such as ClickHouse, was the right tool for a self-hosted product. clipmux-lms used a player beacon written to ClickHouse and Workers Analytics Engine for bandwidth; the LMS turned the beacon off, and its schema was never committed. The image is built with `CGO_ENABLED=0`.
Decision:
- **Postgres, not a second database.** The API needs hourly totals per playback ID and a per-day set of viewers per asset. A busy LMS adds about 12,000 hourly rows a day, which Postgres serves without a new service.
  - **ClickHouse** would be a second stateful container (about 1–2 GB of memory) to install, upgrade, secure and back up. It pays for itself only at billions of raw events, such as a multi-tenant cloud edition.
  - **TimescaleDB** would replace the stock Postgres image, and parts of it are under the Timescale License.
  - **DuckDB** needs cgo and a single writer.
- **Counted where delivery answers the viewer,** so every player counts, including native Safari, ExoPlayer and AirPlay. No player beacon this phase; exact watch time and drop-off curves would need one later.
- **What counts:** a `GET` on an active route answered 200 or 206. Refusals never count. A 206 that doesn't start at byte 0 adds its bytes only; views and watch time need the first byte.
  - **`views`:** one per generated `master.m3u8`.
  - **`watch_seconds`:** 6 per audio segment, or per video segment for an output with no audio. It overcounts by the player's buffer-ahead, about 30 s per view, and the docs say so.
  - **`bytes_delivered`:** the body bytes of every counted response.
  - **`unique_viewers`:** distinct viewer keys, kept per day per asset. A viewer key is HMAC-SHA256 under a per-project analytics key over the app ID and the token's `vid`, cut to 16 bytes. The count is `null` when no counted view in the range carried a `vid`.
- **What doesn't count:**
  - tokens signed by the internal key, unless the playback ID belongs to a share link, which leaves out previews, posters and the setup test;
  - fetches the Worker forwards to origin (marked `X-ClipMux-Edge: 1`), which are counted at the edge.
- **Privacy:** no IP, User-Agent, referrer, token or raw `vid` is stored anywhere. This settles the architecture's open item on where `vid` is logged: only as a keyed hash, in analytics.
- **Built-in delivery** aggregates in memory and flushes an additive upsert every 15 s, plus a last flush on shutdown, so each replica counts on its own.
- **The Worker** writes one Analytics Engine data point per counted response, with the viewer key computed under a new `ANALYTICS_KEY` secret. The dataset is `clipmux_delivery`, one per account; a point is blobs `[project, app, asset, playback ID, kind, viewer key hex]`, doubles `[views, watch seconds, bytes]` and index the asset (P7-T04). The project comes from a `PROJECT_ID` binding; route records gain `no_audio` (outputs whose video segments carry the watch time) and `share`. serve pulls the edge into the same tables with a separate read-only token (Account Analytics: Read), sealed like the AI keys, because Connect Cloudflare's token is forgotten after setup. Each pull recomputes whole hours and replaces their edge rows, so a pull repeated never counts twice.
- **Rollups:** hourly totals for the 24-hour range, daily totals for the others, and viewers both per asset and per app per day, all written in one flush (P7-T03 measured a 90-day read without them at up to 5.7 s).
- **Retention (D105):** hourly rows 14 days; daily rows and viewers 180 days, the span two 90-day ranges need. There are no lifetime totals.
- **The API** is D100's contract with two additions:
  - `collecting`, saying whether built-in and edge counting are on;
  - an opt-in `analytics:read` API-key scope for an app's own assets.
Consequence: P7-T01 to T05. The analytics key and the edge token join `secrets.Columns`. The edge token is kept in `edge_analytics` with a watermark; each 5-minute pass recomputes at most a day from an hour before it, never further back than 13 days so the hourly rows hold every hour of a day it rebuilds (P7-T05). Analytics Engine keeps three months and samples at high volume, which can drop viewer keys; both go in known gaps.

## D104 Share links: a page on the public URL, a playback ID each — accepted (2026-11-10)
Context: the architecture promises an expiring share link signed with the internal key, each with its own signed playback ID. The owner asked whether a presigned URL would do. It can't: outputs are AES-128 ciphertext whose keys are derived at delivery, so a presigned storage URL hands out segments with no key. It also can't be revoked, and it bypasses the route record.
Decision:
- **A link is a page, not a token.** `{public_url}/s/{secret}` opens a small page that fetches short-lived tokens from `POST /s/{secret}/playback`, so a link can outlive the 7-day token maximum, and expire or be revoked on its own.
- **Each link owns one signed playback ID,** created with it. Revoking the link revokes the ID through the existing unpublish, and the link's views are that ID's views. Previews, posters and "Make private" leave share IDs alone.
- **The secret** is `crypto/rand.Text()`, about 130 bits. It is looked up by its SHA-256 and sealed for the owner to copy again. Failed lookups count toward the playback refusal limiter.
- **Tokens:**
  - signed with the internal key, `aud` `v`, `sub` the link's ID;
  - `exp` is the earlier of the link's expiry and now + max(2 × duration, 4 h);
  - `vid` is a random per-browser ID from the page, so share links count unique viewers.
- **Options this phase:**
  - an optional expiry, enforced by the tokens and by a loop that revokes expired links' IDs;
  - a plain HLS URL for other players, which can't refresh and lasts at most the project maximum.
  No password, no embed and no changing an expiry.
- **The page** is a second Vite entry with its own CSP and `frame-ancestors 'none'`, `noindex` and no referrer. Its path wildcard is named `{token}`, so the access line redacts it.
- **The routes** are integration routes (`assets:write`, `assets:read`), so an app can create links too. The owner reaches them as the asset's app.
Consequence: P7-T06 to T08. The threats table gains a passed-on link, guessed secrets and a leaked HLS URL.
Built in P7-T06 (package `internal/shares`, migration 00030):
- **No `revoked_at` on a link.** Its state is read from its expiry and its playback ID: past the expiry it is `expired`, whatever stopped the ID; before it, a stopped ID is `revoked`. Only the service's clock is compared, never Postgres's.
- **Each link's ID** is created through `lifecycle.AddPlaybackIDIn`, in the link's transaction, so it keeps the 1000-ID cap and the deleted-asset check. Route records mark it `share`, so the Worker counts it; the vectors have a case for it.
- **An idempotent replay** stores only the link's ID, never its secret; the response is rebuilt from the sealed copy.
- **Tokens** are also capped by the project's longest token lifetime, which delivery enforces.
- **A disabled app's links** answer `404 share_not_found`, as its playback IDs answer 403.
- **The per-address limit** spends a request only on a link that would play; refusals (404, 410) go to delivery's refusal limiter, which now covers `/s/`.
Built in P7-T07: the page is plain TypeScript and CSS, not React, so it loads its own 2.6 KB script and the player chunk alone. Its CSP is the dashboard's without the storage buckets. A failed refresh or a refused segment makes it ask `/playback` again and show why (revoked, expired, still processing), rather than leaving a stalled player.

## D105 Analytics are kept 180 days, fixed — accepted (2026-11-10)
Context: P7 open question 2. The API's longest range is 90 days, compared with the 90 days before it. T03 added daily rollups, so hourly rows serve only the 24-hour range.
Decision: the retention loop (D069) deletes analytics older than its window. These are constants like the other windows, not settings; one can come when someone asks for longer.
- `playback_daily`, `playback_viewers` and `playback_viewer_days`: 180 days (`retention.AnalyticsWindow`).
- `playback_hourly`: 14 days (`retention.HourlyAnalyticsWindow`). The 24-hour range and the 24 hours before it need two days; the rest is room for an edge pull to recompute recent hours.
Consequence: no lifetime totals (known gaps). A backup holds at most 180 days of analytics.

## D106 Phase 7 real-zone run: the pull is exact, the edge samples — recorded (2026-11-11)
Context: P7-T10 on the D090 staging zone. The staging install's Postgres volume no longer existed, so the owner chose to re-provision it: staging now has its own Postgres on loopback port 5445 (`deploy/cloudflare-staging.py db-start`, named volume `clipmux-staging_pgdata`), a new owner and integration credentials (the dead ones moved to `data/cloudflare-staging/retired-2026-11-11/`), a new 70 MiB baseline on filesystem storage, and Connect Cloudflare run again on the same `clipmux-staging` bucket and `media-test.clipmux.com`. The test is `TestP7LiveEdge` (`CLIPMUX_E2E_LIVE_P7=1`).
Results:
- **Connect binds the analytics dataset** with the existing Connect token: all 13 steps passed, including the setup test through the Worker, and Worker `911308c9e39d` writes to `clipmux_delivery`. No extra permission is needed to write.
- **The read-only token works.** A token with only Account Analytics: Read connected and pulled every 5 minutes without an error. To skip an hour of empty catch-up passes, the test moves the watermark to the last hour; the 13-day catch-up stays covered by the fake SQL API.
- **The pull is exact.** In each of three runs the pulled views, watch seconds, bytes and viewers equalled Analytics Engine's own `_sample_interval`-weighted sums for the asset.
- **Analytics Engine samples even one viewer.** About ten points in a second on one index (the asset) came back at a sample interval of 2, with the rest dropped. One run kept the masters and segments and came out 292 bytes over the script's 2,354,553 (0.012%), with views, watch time and viewers exact; another dropped the master and every segment, losing the view and its watch time. So exit check 4 can't hold against the script, even within 1%, at this volume.
- **A revoked share link stopped at the edge** on the first poll after revoking, inside 65 seconds. The video was on filesystem storage, so the Worker forwarded the master to serve, which refused the revoked ID at once; the Worker's own 60-second route cache wasn't what was measured.
- **Not explained:** the scripted viewer's requests for the main playback ID never reached serve, though its route's backend is `origin`; the Worker still answered with this install's output. Only the share links' masters were seen at origin.
Consequence: exit check 4 is open (D107). The data point index, now the asset, is the lever to try: a per-request index may keep small bursts from being sampled, but Analytics Engine doesn't promise it. The origin bypass needs its own look before the "a forwarded fetch counts once" claim holds on the real zone. Docs say edge counts are estimates (cloudflare.md, known-gaps). The read-only token was pasted into a session transcript and should be rolled.

## D107 Phase 7 closes with open items — recorded (2026-11-11)
Context: the owner wrapped Phase 7 up at T11 rather than chase the edge items further. D102 made P6-T19, the LMS cutover, wait for P7's exit.
Exit test, item by item:
1. **A share link plays — passed, one part open.** It plays with subtitles in a fresh browser with no session, in desktop Chromium and at 375 px (`share.spec.ts`). Revoked, built-in delivery refuses it at once (Go tests and Playwright); at the edge it stopped inside 65 seconds (D106). Expired, the page says so and `/playback` answers 410. Open: its HLS URL is checked as a signed playlist that loads (`sharing.spec.ts`), not played in hls.js and ExoPlayer.
2. **Both adapters count alike — passed.** The Go handler and the Worker agree with every counting vector, including the share link's.
3. **Built-in counts are exact — passed.** Literal totals, series, previous and top assets on real Postgres; previews, posters and the setup test add nothing; a request carrying `X-ClipMux-Edge` counts nothing at origin. Open on the real zone: D106's origin bypass.
4. **Edge counts — partly passed.** The pull equals Analytics Engine's weighted sums exactly; against the script it doesn't, because of sampling (D106).
5. **Counts survive a stop — passed for SIGTERM** (serve's final flush is tested); `kill -9` losing at most one 15-second flush is by design, not measured.
6. **Nothing leaks — partly passed.** The access line and `logging.Scrub` redact `/s/{secret}` and tokens, unit-tested; no end-to-end log scan has yet run with share links in it.
7. **The dashboard passes — passed.** 52 browser tests in both projects with CSP enforced, one live-only skip.
Decision: Phase 7's features ship as built. Items 1 (the HLS URL in real players), 4 and 6, and D106's origin bypass, are open and listed in known-gaps. Before P6-T19 starts, the owner decides which of them it needs; the LMS's own playback uses signed tokens with `vid`, which items 2, 3 and 7 cover.
Consequence: the P7 plan is marked closed with these items. P6 resumes at the owner's call.

## D108 A dashboard API key on a loopback URL needs the owner's confirmation — accepted (2026-11-12)
Context: trying ClipMux on localhost, the owner could create apps but not their keys: D023 refused every key while the public URL is loopback, and only the CLI had an escape hatch (`create-key --allow-loopback`). The refusal's reason still holds: on loopback, signed upload URLs and `base_url` point at the owner's machine.
Decision:
- `POST /v1/apps/{id}/api-keys` takes `local: true`. On a loopback public URL a request without it is still `409 public_url_required`; with it, the key is created.
- The dashboard keeps "Create API key" enabled. While `keys_allowed` is false, the create dialog says why and asks the owner to tick "This key is for apps on this computer" before Create works.
- Keys are not bound to the URL: `base_url` and signed URLs come from the public URL when they're issued, so the same key works for apps elsewhere once a public URL is set.
- The CLI flag stays.
Consequence: D023's loopback rule and D025's 409 now apply only to requests without `local`. The dashboard needs no CLI step on a local install.

## D109 Getting provider credentials without hunting for them — accepted (2026-11-12)
Context: every connection asked the owner to find and copy credentials on the provider's site by hand. Cloudflare has offered third-party OAuth since June 2026, but its redirect URIs must match exactly, so a self-hosted install, with an address of its own, needs a ClipMux-owned public OAuth client and a hosted callback relay. Modal and RunPod offer no OAuth to other apps, but each CLI gets its key through a browser approval (`modal token new`, `flash login`).
Decision:
- **Cloudflare: a pre-filled token link and one paste.**
  - `GET /v1/cloudflare` returns `token_url`, `r2_token_url` and `analytics_token_url`. These are account-owned token template links (`dash.cloudflare.com/?to=/:account/api-tokens&permissionGroupKeys=…&name=…`) with the permissions each flow uses:
    - Connect: Workers Scripts edit, Workers Routes edit, Workers R2 Storage edit, Billing read, Zone read and DNS edit.
    - R2 storage: Workers R2 Storage edit.
    - Edge analytics: Account Analytics read.
  - Cloudflare names any key it can't apply on the form.
  - `POST /v1/cloudflare/inspect {token, account_id?}` reads what a pasted token reaches (accounts, active zones, R2 buckets, Workers Paid) and changes nothing. The Connect dialog offers those as choices: the account, a subdomain under one of the zones, and an existing or new bucket. It warns about Workers Paid before Connect runs. The token is used for the call only.
  - Real OAuth is deferred (known-gaps).
- **Modal and RunPod: approve in the browser, or paste.**
  - `POST /v1/executors/{kind}/authorize` takes the Connect request without credentials. It checks the settings first, so no key is issued for settings Connect would refuse, then starts the provider's approval:
    - Modal: anonymous `TokenFlowCreate`, then `TokenFlowWait`. `expires_in_seconds` stays 0, since it would set the issued token's lifetime.
    - RunPod: unauthenticated GraphQL `createFlashAuthRequest`, then `flashAuthRequestStatus`.
  - It returns ClipMux's own approval ID, the provider's page and Modal's code.
  - `GET /v1/executors/{kind}/authorize/{id}` polls the provider once; Modal's poll waits up to 10 seconds. Once approved, it runs the provider's Connect with the issued credentials and returns its steps. One poll runs at a time, so Connect runs once, and a settled approval answers the same to every poll.
  - Credentials go from the provider to this server and are sealed as pasted ones are; they never reach the browser.
  - Pending approvals live in memory, at most three per project, for 10 minutes (or RunPod's shorter expiry); a restart only means starting again.
  - The dashboard opens the provider's page in a tab that can't reach back (`opener` cleared). Pasting stays one click away, with links to each provider's key page; for Modal, the pasted `modal token set …` command fills both fields.
- **Real run (2026-11-12):** Modal returned a token to the poller. RunPod's console showed the key on its page with "paste it into the terminal" and reported the request `CONSUMED` with no key, so the poll answers `shown` and the dialog turns to pasting, with the key already on the owner's screen.
- These approval flows are the providers' own CLI flows, not documented integration APIs. RunPod's page names Flash, and its key is the key Flash would get. If a provider changes them, `authorize` answers `502 approval_unavailable` and the dialog offers paste.
Consequence: docs/api.md lists the new routes and docs/cloudflare.md the links; the risk is in known-gaps. `TestLiveModalApproval` (`CLIPMUX_LIVE_APPROVAL=1`) checks Modal's flow against the real API.

## D110 The owner chooses where uploads go, and Connect stops choosing for them — accepted (2026-11-12)
Context: the storage list sat under Delivery. This server's disk couldn't become the default again from the dashboard, an R2 bucket could only come from Connect Cloudflare, and every Connect, a reconnect included, made R2 the default (D037).
Decision:
- **A Storage page** (`/storage`, its own sidebar entry) leads with where new uploads go. Every store, this server's disk included, has "Use for new uploads", and the library move is offered there for whatever the default is.
- **R2 without delivery:** `POST /v1/storage/r2 {token, bucket?, account_id?, default?}` runs Connect's token, account, bucket and storage steps: the bucket is created if missing, and keys are derived from the token as in D037. It doesn't touch delivery, and it reports steps like Connect. Adding the same bucket again renews its keys, and Connect later reuses that config. The token needs only Workers R2 Storage edit.
- **Connect leaves the default alone unless asked:** `default_storage` on `POST /v1/cloudflare/connect`.
  - Left out, the first connect makes the bucket the default and a reconnect leaves the choice unchanged.
  - `true` or `false` is explicit; the dialog's "Store new uploads in R2" tick box sends it (checked on a first connect, and on a reconnect while R2 is the default).
  - The routes step says where new uploads go.
- Home's status line reads the default store. Its checklist gains "Store uploads in a bucket", done when the default isn't this server's disk.
Consequence: D037's "make it the default store" and D099's "R2 still comes from Connect Cloudflare" are amended. Cloudflare delivery with uploads on disk works as before: assets on other stores play through origin.

## D111 Connecting a cloud GPU asks for GPUs, not limits — accepted (2026-11-12)
Context: the Modal and RunPod dialogs asked for a machine count, a monthly limit, a rate, an image and registry credentials before the key: too much for a first connect. The rate was a number the owner had to look up.
Decision:
- **GPUs from the provider, with prices.**
  - `POST /v1/executors/runpod/gpus {api_key}` reads RunPod's catalog (`GET https://api.runpod.io/v2/catalog/gpus`, which needs the key) and lists the GPUs on Secure Cloud whose NVENC the worker can use, priced per hour. The key isn't kept.
  - `GET /v1/executors/modal/gpus` lists Modal's NVENC GPUs (L4, T4, A10G, L40S) at Modal's published per-second prices, since Modal has no price API, plus the CPU and memory each sandbox gets.
  - The L4 is marked recommended and listed first: Ada's NVENC with AV1, cheap, and proven on both. The dialog ticks the first three, which ClipMux tries in order when one has no capacity.
- **The rate fills itself in.** Left out, it is the dearest chosen GPU's price: from RunPod's catalog during Connect's `credentials` step (a chosen GPU not on Secure Cloud fails that step), or from Modal's table. A Modal GPU outside the table is `400 invalid_request`.
- **Limits default.** A monthly limit left out is $50 (`dispatch.DefaultMonthlyLimitCents`), and machines one. D055 holds: a provider is never connected without a limit. The dialog shows them as one line with Change.
- **Image and registry wait under Advanced**, collapsed; they matter only while the worker image is private.
- **RunPod is a key field**, like Cloudflare's token: "Get a key from RunPod" opens the approval page that creates a key and shows it (D109), and the pasted key lists the GPUs. Modal keeps "Connect with Modal", with pasting a token one click away.
- `CLIPMUX_RUNPOD_CATALOG` points serve at a test double, as `CLIPMUX_RUNPOD_API` does.
Consequence: a first connect is a key (or an approval) and a GPU choice. Modal's prices are a table to refresh when Modal changes them (known gaps).

## D112 A TypeScript SDK, an OpenAPI spec and a four-step integration — accepted (2026-11-12)
Context: integrating took about 120 lines of glue: raw `fetch` to `/v1/uploads`, a hand-rolled webhook HMAC, `jose` for ES256 tokens, five env vars copied from three dashboard tabs (one a multi-line PEM), a headless uploader, and a player wired with `base-url`, `token` and `getToken`. Most integrations are now written by coding agents, which get exactly these details subtly wrong: the HMAC over a re-serialized body, an API key in the browser, a token too short for native HLS. P6 open question 2 recommended tested snippets, not SDKs, and D087 said no OpenAPI spec and no MCP server in v1. The owner chose otherwise.
Decision:
- **One server SDK, `@clipmux/node`** (`packages/node`, MIT):
  - TypeScript only, with zero runtime dependencies on Web APIs (`fetch`, `crypto.subtle`), so it runs on Node ≥ 20, Bun, Deno and edge runtimes.
  - It holds the client, the resources, offline token signing, webhook verification, Web `Request`→`Response` handlers with default-deny callbacks, and a CLI (`init`, `doctor`, `listen`, `upload`).
  - Python, Go and PHP get tested snippets for token signing and webhook verification instead (this answers open question 2).
- **Shared vectors for every language:**
  - tokens from `internal/delivery/testdata/vectors.json` (D015);
  - webhooks from `internal/webhooks/testdata/vectors.json`, built from `webhooks.Sign` (16 cases, including the LMS's openssl-checked vector), regenerated with `go test ./internal/webhooks -run TestVectors -update`.
- **An OpenAPI 3.1 spec of the integration API in v1**:
  - It covers the integration routes, delivery URLs, `GET /v1/app`, the public app-pairing routes, and webhooks.
  - It is embedded in serve, served unauthenticated, and checked against the routes.
  - The SDK's types are generated from it.
  - The management API stays out of it.
  - This amends D087.
  - It is JSON (`internal/openapi/openapi.json`, `GET /openapi.json`), not YAML, so the standard library parses it for the route tests.
- **One command to credentials.** `npx @clipmux/node init` runs a device flow: the owner approves in the dashboard, and it writes the five `CLIPMUX_*` variables to an env file without printing a secret, so agent transcripts stay clean. An owner route and `clipmux admin create-integration` make the same bundle.
- **The browser needs no configuration.**
  - `<clipmux-player session-url>` and `<clipmux-uploader create-url>` talk only to the app's own handler, whose responses carry `base_url` and `endpoint`.
  - The player says when it plays native HLS, so the handler signs the longer token D014 requires.
- **An MCP server comes after launch,** built on the spec's operations (known gaps).
- The work is P6 Track E (T21–T28), before the LMS cutover (T19) and launch.
Consequence:
- P6 now adds integration routes (`GET /v1/app`, app pairings) as well as owner routes; the integration API still only grows.
- T15–T17 are rescoped around the SDK.
- The LMS adapter becomes a thin layer over `@clipmux/node`.
- AGENTS.md allows TypeScript in `examples/`.
Built (T24–T26), where it differs from the plan:
- **Types come from a 100-line generator** (`packages/node/scripts/generate.ts`, zero dependencies), not `openapi-typescript`, which needs TypeScript 5's compiler API and the workspace is on TypeScript 7. `pnpm test` fails when `schema.gen.ts` is stale.
- **The webhook route answers 500 when the app's `asset` callback throws,** so ClipMux retries the delivery (24 hours, D018) instead of the event being lost to a transient error. Verified deliveries otherwise answer 200, and forged ones 400 with the reason.
- **`init` takes no `--webhook-url`:** the owner enters it on `/connect` (D113). Without an endpoint, `init` writes a local `whsec_local_…` secret so `listen` and the handler agree while developing; `doctor` flags it.
- **The CLI's bin is `clipmux-node`,** run as `npx @clipmux/node <command>`, so it never shadows the server's `clipmux` binary.
- **`upload` in the CLI reuses `@clipmux/uploader`,** the SDK's only runtime dependency; `release.yml` pins its `workspace:^` range to the version it publishes.
- **A playback session without the asset** takes `base_url` from `GET /v1/app`, has no tracks, and a native token of 12 hours, since the duration is unknown.

## D113 How an app gets its credentials: one bundle, three ways in — accepted (2026-11-12)
Context: D112 asks for one command to credentials. P6-T23 builds the server half: `GET /v1/app` for `doctor`, a credentials bundle, and an app-pairing device flow for `init`. The plan fixed the routes; these are the choices within them.
Decision:
- **The bundle** (`apppairings.Issue`) makes, in the caller's transaction, a new API key with the default scopes (`uploads:write`, `assets:read`, `assets:write`, now `apikeys.DefaultScopes`), a new signing key, and with a webhook URL a new endpoint. A refused webhook URL or a loopback refusal creates nothing. It always makes new keys rather than reusing an app's: the private key and API key exist nowhere else to reuse. The API key is named after the pairing's `name`, or `integration` from the dashboard and the CLI, so the owner knows what to revoke.
- **D108 applies to all three ways in.** `POST /v1/apps/{id}/credentials` and `…/approve` take `local`; `create-integration` takes `--allow-loopback`. `/connect` and the dialog ask the owner to tick "this app runs on this computer" while the public URL is loopback.
- **The `.env` block** has the five variables, `CLIPMUX_WEBHOOK_SECRET` only with a webhook. The PEM is one double-quoted line with `\n` escapes, which dotenv, Next.js and `node --env-file` all expand, so the block can be appended to any env file. A comment line names the app.
- **App pairing mirrors agent pairing (D042, D067)** in a new table, `app_pairings`:
  - The code format, the SHA-256 storage of codes and poll tokens, the 10-pending cap and the start and poll limits move to `internal/pairing`, shared by both flows. Each flow keeps its own buckets, at the same numbers. App codes hash under their own prefix, so an agent code never matches an app pairing.
  - A pairing lasts 10 minutes, not agent pairing's 15: `init` runs with the developer at the screen.
  - Approval issues the bundle and seals it as JSON (`secrets.Columns`: `app_pairings.credentials_sealed`). The CLI's next poll opens it, marks the pairing consumed and clears the column in the same transaction, so credentials are handed over once; a check constraint keeps sealed credentials only on `approved` rows. Approval leaves the pairing at least a minute to be collected. Approved but uncollected past expiry, the sealed copy is dropped at the next poll or by retention, which prunes app pairings a day after expiry as it does agent pairings.
  - There is no dashboard-first code (agent pairing's `Invite`): the dashboard's "Create credentials" covers that case directly.
- **`GET /v1/app`** answers any valid key whatever its scopes (`apikeys.AnyScope`), beside the asset routes. `signing_keys` lists the keys that still verify tokens (not revoked, not past `retired_at`), with their public PEM, so `doctor` can check the private key it holds matches one of them. `scopes` is the key's own.
- **The dashboard** gets `/connect` (pick an app or "New app…", which defaults to the CLI's name, an optional webhook URL, the code shown large so it can be matched) and a "Connect your app" panel with the `init` one-liner on Apps & API and on each app's page (not the built-in Dashboard app), where "Create credentials" shows the `.env` block once.
Consequence: docs/api.md has an App pairing section and the new routes; docs/configuration.md lists the five app-side variables. An approval nobody collects leaves an API key and a signing key the owner revokes by hand (known gaps).

## D114 The LMS cutover replaces clipmux-lms outright, on a shared host — accepted (2026-11-12)
Context: P6-T19 sketched the cutover as a release candidate installed by the one-liner, a library import, a second LMS adapter and a soak with `clipmux-lms` read-only. The owner confirmed the LMS (Edural) has no users and no videos to keep, and that the instance runs on the existing VPS beside other production apps.
Decision:
- **No import, no soak, no rollback window.** `clipmux-lms` is switched off once the new instance passes its setup test. Its database is dumped once, then removed with its buckets, Worker and Modal app. P6 open questions 4 and 6 (the library's subtitles, the import's write rate) no longer apply.
- **Hostnames (open question 5):**
  - `app.clipmux.com` is the public URL (dashboard and API), DNS-only to the VPS;
  - `media.clipmux.com` is the Worker's custom domain;
  - `clipmux.com` stays free for the docs site;
  - `api.clipmux.com` retires with the legacy stack.
- **A shared host.** The VPS is arm64, with 2 vCPU and 4 GB, and its own Caddy serves every app on ports 80 and 443. ClipMux runs without its in-stack Caddy, through a proxy override (`deploy/compose.proxy.yml`), published on loopback. The built-in worker is off, and encoding and speech go to Modal with no fallback.
- **Before a release.** It runs a `sha-{commit}` image from `main`. `image.yml` builds it for amd64 (Modal) and arm64 (the VPS). The registry credentials are pasted into Connect Modal until the package is public.
- **Connect names the Worker after its hostname**, so this instance's Worker and D090's staging Worker share one Cloudflare account.
- **The LMS replaces its adapter** rather than adding a second one beside it. Its plan is Edural's P36: Edural signs ES256 tokens with `vid`, and every playback ID is signed.
Consequence: T19 doesn't wait for T07's release candidate or for T28. The launch exit's item 5 (the LMS through a soak window) is met by the LMS running in production on this instance, not by a timed soak.

## D115 Onboarding ends at the addresses; the setup test runs on request — accepted (2026-11-12)
Context: first boot, and every save of the URLs, queued the setup test (D026), and the wizard's last step waited for it (D032). On a CPU-only VPS that's minutes of watching checks, and a "clipmux-setup-test" video turns up in a library the owner hasn't used yet. The owner asked for less to sit through: people want to start, and can check the path when they choose.
Decision:
- **The wizard is two steps:** create the account, then the addresses. Saving the addresses ("Save", no longer "Save and test") lands on Home.
- **No automatic run on first boot or on saving the URLs.** `setuptest.Runner.QueueFirst`, `manage.API.URLsSaved` and the `CountSetupTests` query are gone.
- **The test stays one click away:** Settings → General → Setup test, `POST /v1/setup-test` and `clipmux admin setup-test --wait`. Connect and Disconnect Cloudflare still queue one, and connecting a cloud GPU still runs its own (D056), which is what verifies the provider.
- Home's checklist drops "Run the setup test".
Consequence: D026's first-boot and URL-save triggers and D032's test step are withdrawn. The `first_boot` and `urls_saved` trigger values stay in the schema for rows already written (Cloudflare's run still uses `urls_saved`).

## D116 Encoding is one choice: this server or a cloud GPU — accepted (2026-11-12)
Context: the Encoding page had a primary/fallback/queue-age form (D053, D060), a list of PCs with pairing commands and scopes, and the cloud GPU cards. Connecting Modal didn't put it to work: the owner then had to find the routing form and name it. Self-hosters with a GPU don't need a PC agent, since the built-in worker already probes NVENC, VAAPI and CUDA (D046); VPS owners have Modal, RunPod or the CPU. The owner asked for the PC option to go and for the rest to just work.
Decision:
- **One radio group:** This server (its GPU when it sees one, otherwise the CPU), Modal, RunPod. A provider can be chosen once connected and verified; until then its card says a sample is encoding on it, or which check failed (`GET /v1/setup-test/latest` now carries `executor`).
- **Connecting is choosing.** When a provider's connect test passes, `setuptest` verifies it (D056) and, in the same transaction, sets routing to that provider as primary, with the built-in worker as fallback when serve runs one (`Runner.ProviderFallback`) and no queue-age rule. Until it passes, jobs go where they went.
- **One tick box while a cloud GPU is chosen:** "If it can't start a machine, encode on this server instead" (`fallback_executor` `builtin` or null). The queue-age rule stays in the API only.
- **Choosing This server writes `builtin`.** The page reads `workers`, `agents` and `builtin` alike as This server.
- **Disconnect hands the work back** instead of refusing (`409 executor_in_use` is gone): routing that named the provider moves to `builtin` as primary, or to no fallback, in the same transaction. It still refuses while one of its machines may run.
- **PCs leave the dashboard:** no list, no pairing card, no "Connect a PC" on Home. `/workers/connect` stays, alone, because `clipmux worker --server` opens it, until the agent backend is removed.
- **The PC agent stays, as a command-line option.** The agent package, pairing, `push --local` and the `workers`/`agents` routing modes are kept and documented in docs/agents.md (pair with `clipmux worker --server` or `clipmux admin create-pairing`; list, revoke and route through the API). Removing them is decided after launch, from whether owners ask for PCs; deleting a tested phase now would cost more than keeping it.
Consequence: D053's "my PC, else the server", D056's "routing must name it after the test", and D060's routing card are amended.

## D117 Storage holds the uploads; Delivery holds the encoded files — accepted (2026-11-12)
Context: each asset had one store for its source, outputs and tracks (D034). Storage and Delivery both asked for an R2 token, and adding R2 under Storage left Delivery saying "Built in" while the Worker, connected under Delivery, could serve only the assets in its own bucket. Owners expect Storage to be where uploads are kept and Delivery to be where the files viewers get are kept. Raw masters are also the largest, coldest files: they needn't sit in the delivery bucket.
Decision:
- **Two stores per asset, both fixed at creation.** `assets.storage_id` holds `uploads/{asset}/…` and comes from the default store, as before. `assets.media_storage_id` (migration 00032) holds `media/{asset}/…`, outputs and tracks, and comes from `media_target(project)`: the delivery bucket while `projects.delivery = 'worker'`, otherwise the default store. The create queries set it; a trigger fills it from `storage_id` for an insert that names none.
- **Every store lookup names its part.** `stores.Set` has `SourceFor` and `MediaFor` (and their `…In` forms) instead of `ForAsset`. Uploads, imports and source uploads use the source store; outputs, audio, tracks, delivery, retention, the setup test's storage reads and the speech API use the media store; a delete purges each prefix from its own store.
- **Route records follow the encoded files:** `backend` is `r2` when `media_storage_id` is the delivery bucket.
- **A move carries each part to its own target.** `jobs` gains `source_media_storage_id` and `target_media_storage_id`; the job copies the source when its store changes and the servable outputs and tracks when theirs does, switches both columns in one update, and cleans up each prefix from the store it left. A move with no media stores (queued before this) moves the encoded files with the source. `GET /v1/storage/moves` reports `uploads_elsewhere` and `media_elsewhere`; `POST` queues moves to both targets.
- **Connect Cloudflare no longer touches where uploads go:** `default_storage` is gone from the request and the dialog. Its bucket becomes the media target through `delivery_storage_id`. Disconnect leaves the media target to fall back to the default store; encoded files already in R2 keep playing through origin, and the R2 store stays while assets use it.
- **The pages say so.** Storage is "where uploaded originals are kept" and offers moving uploads; Delivery says where encoded files are kept for the delivery chosen and, under Cloudflare, offers moving them into the bucket. Home's status line reads "Uploads" and "Delivery".
- A store is in use, and can't be removed, while any asset's source or encoded files are on it, or a move reads or writes it.
Consequence: D034's per-asset store becomes a pair, D037's "Connect makes R2 the default" and D110's `default_storage` are withdrawn, and D038's move job moves parts. Down for 00032 refuses while any asset keeps the two apart.

## D118 The speech models are baked into the image — accepted (2026-11-13)
Context: D077 and D079 kept the models out of the image so a serverless cold start wouldn't pull 0.9 GB more. But a Modal or RunPod machine is new for every job and has no volume, so each one downloaded the 874 MB model from Hugging Face before it could claim its job, and in `--once` mode it did so before any job, encodes included. The owner's first real run on Modal showed it on every dispatch.
Decision:
- **The image carries both models** at `/usr/share/clipmux/models` (`CLIPMUX_BAKED_MODELS_DIR`). A `models` build stage runs `toolchain/fetch_speech_models.sh`, which checks each against its sha256 in `toolchain/versions.env`; its layer depends only on the pins. The image job checks the baked files against the pins on a read-only container, and `models/NOTICE` carries their MIT licences.
- **A worker looks there first** (`worker.BakedModel`), trusting a file of the right size: the build hashed it and the image is pinned by digest, so re-hashing 0.9 GB at every cold start buys nothing. Without one (a PC agent) it fetches into `CLIPMUX_MODELS_DIR` as before. serve passes the variable to the built-in worker, which stops keeping a copy in the data volume.
- **A machine woken for anything but a transcription skips speech** (`CLIPMUX_SPEECH=off` in the wake's environment, `dispatch.WakeEnv`): it runs the one job it was woken for, so loading the model would only delay an encode.
Consequence: the image grows by about 875 MB. This supersedes D077's and D079's "fetched, never baked"; their digests, sandboxing and probes stand.

## D119 Providers follow the server's upgrades and report their own bill — accepted (2026-11-13)
Context: the owner's first real Modal test found three things. Connect failed with "can't be pulled with these registry credentials", which didn't say whether the token, its scope or the tag was wrong. The image a provider runs was pinned once, at Connect, so an upgraded server kept waking the old worker until someone connected again. And spend was ClipMux's estimate (D055), which on the first real month was twice Modal's bill: it prices every machine at the dearest GPU the owner allowed, and counts until the reaper records the stop.
Decision:
- **Registry refusals say which they are** (`registry.ErrBadCredentials`, `ErrNotFound`, `ErrDenied`): refused credentials, a tag the accepted credentials can't find, or an image that is private or missing to whoever asked. Connect anonymous first works with no credentials once the package is public (P6-T07), and the dashboard opens the registry fields when the image step fails.
- **An upgrade re-pins the default image.** A provider's config records `image_chosen` when the owner entered the image. Otherwise, at boot and every 15 minutes, `dispatch.Repin` resolves this build's `CLIPMUX_WORKER_IMAGE` and, for the same repository, moves the pin (`SetExecutorImage`: the config only, so the provider stays verified and keeps its routing); Modal then starts converting the new image. RunPod keeps a private image's credentials itself, so it re-pins only a public image. A config from before `image_chosen` re-pins when its image is this build's repository.
- **Modal's bill replaces the estimate where Modal has one.** Every 15 minutes `dispatch.RefreshBills` asks each executor that is a `dispatch.Biller`; Modal's reads its billing report for the `clipmux-{tag}` app (daily rows for the month's whole days and hourly rows for today's whole hours, since hourly reports span at most seven days), summing the cost in cents rounded up. `executor_configs` keeps `billed_month_cents` and `billed_through` (migration 00033). The month's spend, for the limit and the dashboard, is that bill plus the estimate only for the time since `billed_through`; without a bill this month (a plan without billing reports, RunPod, or a new month) it is the estimate, as before.
Consequence: D055's limit counts Modal's real charges, egress included and before credits, and shows "from Modal's bill up to …". D058's "pinned at Connect" now moves with upgrades for the default image.

## D120 The built-in worker requires Landlock; serve and workers are undumpable; seccomp denies namespaces — accepted (2026-11-14)
Context: the security review's findings 1 and 6. Without Landlock, a media tool exploited by a hostile upload runs as serve's user with no file confinement: it can read `/proc/<serve>/environ` (the secrets key, the database password), the image's PID 1 (`tini`, same uid, dumpable, the same environment), and `secrets.key`, `content.key` and `storage/signing.key` in the data directory. `PR_SET_DUMPABLE` alone covers only the first.
Decision:
- **serve's built-in worker refuses to start when the probe reports Landlock ABI 0**, with one error saying why and what to do. `CLIPMUX_UNSANDBOXED_WORKER=true` stays the explicit opt-out; where only Landlock is missing the opted-out worker still passes `--sandbox`, so its tools keep the limits, `no_new_privs` and seccomp.
- **serve and `clipmux worker` mark themselves undumpable** (`sandbox.Undumpable`, `PR_SET_DUMPABLE=0`) first thing; a failure is a warning. Another process of the same user then can't read their environment or ptrace them.
- **Agents and serverless providers are unchanged.** Modal's gVisor reports Landlock ABI 0 by design (D051), and a provider machine holds only one attempt's token; an agent's tools run on the owner's own PC.
- **Seccomp denies namespaces (finding 6)**, on amd64 and arm64: `unshare` and `setns` fail with EPERM, `clone` with any `CLONE_NEW*` flag (`BPF_JSET 0x7e020080` on its first argument) with EPERM, and `clone3` with ENOSYS, since its flags are in memory where seccomp can't read them; glibc then falls back to `clone`, as under Docker's default profile. A native agent's tools no longer reach user-namespace kernel surface.
Consequence: amends D027's "Landlock is reported, not required" for the built-in worker, and its seccomp list. An install whose kernel lacks Landlock either encodes on a provider or an agent, or opts out knowing its tools can read serve's files.

## D121 The owner's known device skips the global sign-in limit — accepted (2026-11-14)
Context: the security review's finding 2. The global limit of 20 failed sign-ins per 15 minutes refused every address, so 20 wrong passwords from four addresses every 15 minutes kept the owner out. The setup-token limiter worked the same way.
Decision:
- **A successful sign-in, registration or password change sets `__Host-clipmux_device`** (`clipmux_device` on plain http): HttpOnly, Secure, SameSite=Strict, Path=/, one year. Its value is the owner ID, the issue time in microseconds, and an HMAC-SHA256 over both and the project. The key is 32 random bytes made on first use and sealed in `projects.device_key_sealed` (migration 00034, `secrets.Columns` as `projects_device`), like the analytics key.
- **The cookie is worth one thing: a sign-in that skips the global limit.** The 5-per-address limit, the password and the second factor still apply. It is believed only if its MAC holds, it is under a year old, it names the project's owner, and it was issued no earlier than `owners.password_changed_at`, so a password change or `admin reset-password` retires every cookie issued before it. Only a cookie whose MAC holds costs a database read, and nothing is hashed before the limiter.
- **Re-entering the password for an account change** (`passwordOK`: password, TOTP) uses the per-address limit only: the request already carries the owner's session.
- **Wrong setup tokens are limited per address only.** The token is 130 bits, so the global limit only let others lock the installer out.
- **Pairing keeps its global 100 per 15 minutes**: its code is about 39 bits, and an owner locked out of pairing for 15 minutes can still run `clipmux admin create-pairing`.
Consequence: amends D024's "20 overall" and D067's setup-token limit. An owner on a new device during an attack still waits out the window; they can sign in from a device they used before, or reset the password on the host.

## D122 A part URL binds the part's length — accepted (2026-11-14)
Context: the security review's finding 5. On R2 and S3, a part URL didn't bind the part's length, so a holder of an upload token could PUT up to 5 GiB per part; complete then failed, but the bytes stayed until the upload was aborted.
Decision:
- **`Storage.PresignPart` takes the part's `size`.** The S3 adapter sets `ContentLength`, which the SDK's signer adds to `X-Amz-SignedHeaders`, so the store refuses a body of any other length (403). It refuses a size outside 1 byte to `PartSize`, since S3 doesn't keep the declared size. The filesystem adapter refuses a size other than the part's own and, as before, a body of any other length.
- **Callers compute it** with `storage.PartLength`: the part size, except the last part, which holds the rest. `POST /v1/uploads/{id}/parts` and the worker API's source parts both do.
- Browsers and Go's client send `Content-Length` themselves, so no client changes; CORS needs nothing, since `Content-Length` isn't an author header.
Consequence: amends D034's "the declared size is ignored" for signing: parts are still 32 MiB, and each URL now carries its length.

## D123 The Dashboard app takes no API keys; unsandboxed agents take no URL imports — accepted (2026-11-14)
Context: the security review's finding 3. An unsandboxed agent claims pinned jobs and "owner jobs", which `job_claimable_by` (00024) defined as the Dashboard app's. But an API key for the Dashboard app, or an import from any URL, could create those, so a file from anywhere could reach ffmpeg on the owner's unconfined PC.
Decision:
- **The routes that issue keys refuse the built-in app with `409 builtin_app`:** `POST /v1/apps/{id}/api-keys`, `POST /v1/apps/{id}/credentials`, and approving an app pairing for it (`apppairings.Issue`). `apikeys.Create` itself doesn't check, so tests can still act as that app.
- **Migration 00035 revokes the keys the built-in app already has**, and redefines `job_claimable_by` so an untrusted worker's owner jobs exclude any asset with an `import` job. Its Down restores the old function and leaves the keys revoked.
- So the owner's jobs are what the owner uploaded in the dashboard or pinned to the agent. The owner's session acting as an app (D025) still works.
Consequence: amends D043's "owner jobs" and D025: an integration needs its own app. The deploy checks production's built-in-app keys before migrating, since 00035 revokes them.

## D124 Only the committed manifest's files are served, and the rest are swept — accepted (2026-11-14)
Context: the security review's finding 4. Commit verifies storage against the manifest, but the worker's PUT URLs last 15 minutes, so a file could be added under an output after its verification, and delivery served any grammar-valid name under a committed output.
Decision:
- **Built-in delivery serves only the names `out.Manifest` lists**, pinned paths and the unpinned poster alike. The delivery cache keeps each output's listed files with the output (D072), so a segment still costs no query.
- **The `file_retention` loop sweeps each committed output once** (`SweepUnlisted`): 16 minutes after its commit (`CommitGrace`), when no URL can write there, it deletes every object under the output's prefix that the manifest doesn't list, `manifest.json` aside, and records `outputs.unlisted_swept_at` (migration 00036). Outputs committed earlier are swept on the first passes, 100 a pass. A manifest that lists nothing is left alone. This covers the Cloudflare Worker, which reads the bucket, without changing the shared delivery vectors.
Consequence: on the Worker a file added after commit can be served until the hourly loop sweeps it, at most about 76 minutes after the commit (known gap); giving the Worker the manifest check would change the shared delivery vectors and the route records, and is left for later.

## D125 npm publishes from the private repository with a token, without provenance — accepted (2026-11-14)
Context: Edural integrates through the packages, so they ship before launch. The repository is still private, and npm attests provenance only for public sources, so `npm publish --provenance` from it is refused.
Decision:
- **`release.yml` publishes with the `NPM_TOKEN` secret**: a granular token with read and write on the `@clipmux` scope. There is no `--provenance` and no `publishConfig.provenance` until launch (P6-T20), when the repository goes public and trusted publishing replaces the token.
- **This release is dispatched by hand** (`gh workflow run release.yml -f dry_run=false` on main), not tagged: a `v*` tag would also run the image, agent, exit and Safari workflows, and `v1.0.0` stays the launch tag. Safari runs on the owner's Mac.
- **Versions:** `@clipmux/node` 0.1.0 (its first release), `@clipmux/player` 0.6.0, `@clipmux/uploader` 0.5.0. `CHANGELOG.md` records each.
Consequence: amends D092's provenance and its trusted publishing. The `repository` links 404 for outsiders until launch.

## D126 The SDKs fit together as documented — accepted (2026-11-14)
Context: planning Edural's move found the documented hand-offs between the three packages didn't compile or didn't work: `listen` failed on its first event, a ClipMux 413 became a retried 502, `getToken` never said `native`, and the poster URL carried the playback token.
Decision:
- **The events poll is by time.** `after` is the newest event's `created_at`, as the server always required, and the window moves as `internal/listen` does.
- **`uploads.create` returns the token as required**, so its answer goes into `ClipMuxUpload` as it is; a replay without one throws `upload_not_active`.
- **The handlers keep ClipMux's refusals:** 400, 409, 413 and 422 pass through, 429 with `Retry-After`; a wrong key or app is 500 `clipmux_misconfigured`; unreachable is 502 `clipmux_unavailable`.
- **`getToken({ playbackId, native })`**, and `playback.token()` signs native tokens with the session's rule.
- **A session's `poster_url` has a poster-only token** (audience `t`), in node and on the share page, as docs/api.md said.
- **`GET /v1/app` lists `upload_origins`**, and `doctor` prints the CSP sources a page needs. The e2e suite enforces that CSP on the player and uploader pages.
- **One `listen` story:** `npx @clipmux/node listen` for an app built on the SDK (it signs with the app's own secret); `clipmux listen` for other stacks, now also reading `CLIPMUX_URL`.
- **`e2e/web/sdk-types.ts`** checks the hand-offs at compile time, and `packages/node/scripts/live-check.ts` is the release gate against a live instance.
Consequence: the player's `GetToken` and `PlaybackSession` types and node's `WebhookEvent` changed shape; pre-1.0, that is a minor version.

## D127 Connect lists the models; subtitles follow encoding, and the page says so — accepted (2026-11-14)
Context: the Connect forms for a speech API and the chapters LLM asked for the model as free text, so the owner had to know the exact ID, although both credential checks already called `GET {base}/models` and threw the answer away. And the Captions page labelled the local speech mode "On this server and your PCs" after D116 had taken PCs out of the dashboard. In fact `local` follows the encoding routing (`queue.speechRoute`), so an owner on Modal, like app.clipmux, already transcribes there with the models baked into the image (D118). The page never said so.
Decision:
- **Two owner routes list models before anything is saved:** `POST /v1/settings/speech/models {base_url, api_key?}` and `POST /v1/settings/chapters/models {provider, base_url?, api_key?}` → `{listed, data: [{id, name?}]}`. OpenAI-compatible APIs are read from `GET {base}/models`, trimmed, deduplicated and sorted, with IDs over 200 characters dropped. Claude's come from the SDK's `Models.List`, newest first with display names, deprecated and retired ones left out. A server without the route, or whose answer isn't OpenAI's list, is `listed: false`, not an error. A key refused with 401 or 403 is `400 key_rejected`; any other failure is `502 models_unavailable` with the API's own short message. The key is used for the one call and isn't kept or logged. The credentials steps now share these calls.
- **The model field is a free-text autocomplete** (Base UI's Autocomplete). It asks 400 ms after the URL or key stops changing. Likely models come first (speech: whisper, transcribe, speech, stt, asr; LLM: anything but embeddings, speech, images, moderation and rerankers), and nothing is hidden. While the owner hasn't typed a model, the usual one is filled in: `whisper-large-v3-turbo`, else `whisper-1`, else the first speech model; `claude-opus-5-5` for Claude; and an OpenAI-compatible LLM's only chat model when it lists one.
- **Subtitles are two cards, like Encoding:** "Where videos are encoded" (`local`), naming the place from the routing, such as "On Modal, with the whisper model built into ClipMux's image", and "A speech API" (`api`), which holds the connection and its form. When an API is connected, a tick box under the first, "If no machine can transcribe, use the speech API", is `local_then_api`. The three modes and their routing don't change. Home's status line names the place too.
Consequence: D084's Captions page changes its speech control from a select to radio cards. The Encoding page's option card becomes the shared `Choice` component.

## D128 A captioned video wakes one serverless machine — accepted (2026-11-14)
Context: on Modal or RunPod, a captioned upload woke two machines. The encode's machine ran with speech off (D118) and stopped at its commit; the transcription that commit queued then woke a second one. That meant a second cold start (about 47 s on Modal, 39 s on RunPod, D097), a second GPU placement (Modal's can take minutes, D097), and the subtitles waiting for both. P5 open question 9 had accepted this. The owner compared ClipMux with clipmux-lms, which ran the encode and the transcription in one Modal container, in parallel, on the source audio. They kept ClipMux's two jobs, for their separate retries and failure codes and for subtitles timed from the committed audio (D076, D078), and asked for the second machine to go. (D127 is the Connect forms' model lists, on another branch.)
Decision:
- **A hand-off, in the commit.** When an encode's output commits and the commit queues the asset's transcription, `outputs.Committer.HandOff` (`dispatch.Service.HandOff`) gives that job the same machine:
  - the encode's dispatch ends `handed_off`, marked stopped with no stop queued;
  - a new dispatch for the transcription is created already woken on the same instance, with a new one-time key and a 2-minute claim window (`HandOffWindow`);
  - `complete` answers `{"action": "done", "next_key": …}`, and the machine claims the transcription with that key.

  The machine's time is counted once: the first dispatch stops at the instant the second wakes. The reconciler knows the machine by its instance ID, and the router leaves a job with a live dispatch alone.
- **Only when it can work.** The worker must have said at its claim that it can take a transcription next (`follow_on: ["transcribe"]`). The transcription's route at commit must be the machine's own provider: the provider as primary (as D116 sets it), or a setup test naming it. The provider must still be connected, with room under its concurrency and monthly limits (`ProviderHasRoomForHandOff`, which doesn't count the moving machine twice and ignores the wake backoff, since nothing is woken). In `api` mode the transcription goes to the speech API, so nothing is handed off.
- **Speech is proven after the encode, not before.** The wake keeps `CLIPMUX_SPEECH=off`, so the encode never waits for the model (D118), and adds `CLIPMUX_FOLLOW_ON_SPEECH=auto`. A worker reads that variable only on a dispatched machine, sends `follow_on`, and proves speech (from the baked models) only when handed the transcription. A worker image from before this ignores the variable and sends no `follow_on`, so it is never handed anything.
- **Failure falls back to today's path.** If the answer carrying the key is lost or the machine dies, the new dispatch ends `wake_timeout` after the window, the machine is stopped, and the job is routed and woken for as before. If the machine can't prove speech, it claims the job and fails it `model_unavailable`, a capacity code, and the job goes on. Nothing chains further: only a transcription is handed on, and only from an encode.
- Tests:
  - On Postgres: the hand-off itself (the keys, counted once, no wake or stop, the router leaving it alone, the stop after the transcription); each condition that prevents one; and the window running out.
  - Two real-binary drills: a captioned upload on a RunPod primary creates one pod for the video, and the transcription runs on the same instance under its own dispatch. A pod that can't transcribe fails the handed job `model_unavailable`, and a second pod makes the subtitles.
  - The e2e pods now keep speech off on encode pods unless they are handed a transcription, as real machines do.
Consequence: D118's "a machine woken for anything but a transcription skips speech" now holds only until its encode commits, and P5 open question 9 is answered for a provider that is the primary. A provider reached only as the fallback still wakes a second machine (known gaps). Running the transcription in parallel with the encode on one machine is not done: it would give up the committed-audio timing or need the audio rendition transcribed before the commit.
