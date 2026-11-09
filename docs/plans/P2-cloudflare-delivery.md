# Phase 2 — Cloudflare delivery

Source: [architecture.md](../architecture.md), "Build order", "Delivery",
"Adapters" and "Optional upgrades", with [decisions.md](../decisions.md) where
they differ. Phase 2 adds an R2/S3 storage adapter and the Cloudflare Worker
behind the contracts Phase 1 proved. It also adds the Connect Cloudflare card
and a `move` job for assets that leave filesystem storage. The integration API,
tokens, URLs and `base_url` stay the same. The Worker is a CDN upgrade, not a
feature gate, so built-in delivery stays fully supported.

Starts only after the Phase 1 exit test passes, including the Safari,
ExoPlayer and image CI jobs ([known-gaps.md](../known-gaps.md)).

Next: [Phase 3 — PC agent](P3-pc-agent.md). The real-account run of this
phase's exit test is its task T00 (D042).

**Exit test:** app code is unchanged on the Worker, old filesystem assets still
play, and a revoked ID stops within a minute.

Concretely, automated in CI (T07):

1. **App code unchanged.** An app is set up on built-in delivery: API key,
   signing key, webhook endpoint and a playing asset. The test then connects
   Cloudflare and runs the Phase 1 player suites (hls.js and
   `<clipmux-player>`) again with no change to the app's config, code or
   stored playback IDs. `base_url` is unchanged, tokens are signed by the
   app's own key, and a new upload made after the switch is stored in R2 and
   plays from it.
2. **Old filesystem assets still play.** An asset uploaded before the switch
   plays through the Worker on the `origin` backend. After its `move` job, it
   plays from R2 with `serve` stopped, which proves "Playback doesn't need the
   control plane".
3. **A revoked ID stops within a minute.** After
   `DELETE /v1/assets/{id}/playback-ids/{pid}` returns 200, the Worker answers
   404 within 60 s. A signing key revoked in the dashboard stops its tokens
   within 60 s too, through `_keys.json`.
4. The setup test (D026) passes through the Worker. A segment read straight
   from R2 isn't a plain MP4 fragment.

## Design notes

Each task is planned against these points. Each is settled in that task's
decision record.

- **The Worker can't ask Postgres.** The built-in handler checks the output's
  status (committed or retired, never `writing`), whether it belongs to the
  asset, its key version, disabled apps, retired signing keys and the
  project's maximum token lifetime. Several `vectors.json` cases depend on
  these checks: "an output still being written", "a key version the output
  doesn't use", "a disabled app's playback ID is denied" and "a token signed
  by a retired key". So the projection must carry them. Proposal for T02:
  - Route record v1 gains `outputs`, a map from output ID to key version for
    the asset's committed and retired outputs that still have files. A
    pinned path or key outside that map gets 404.
  - `_keys.json` carries each active key's `kid`, `app_id` (null for the
    internal key) and public JWK, plus the project's
    `max_token_lifetime_seconds`. A retired or revoked key is simply absent.
  - Disabling an app unpublishes its routes, and enabling it republishes
    them. Both go through `route_sync`.
  - The vectors file gains whatever the Worker needs to build its fixtures
    from route records and `_keys.json`, and the Go handler keeps passing it
    unchanged. Contract changes go in `vectors.json` first (AGENTS.md).
- **Who reads what.** The built-in handler keeps reading Postgres. Route
  records and `_keys.json` are written only to the bucket the Worker reads.
  On `fs` or `s3` storage the Worker still reads `_routes/` from R2, and the
  route's `backend` says where the bytes are.
- **`origin` backend.** The Worker passes the viewer's request, token
  included, to the built-in handler at the project's public URL. Serve
  already answers `/v/` and `/p/` on any host (D032). The Worker still checks
  the request first, so the origin sees only requests that would pass, and
  the built-in handler checks them again. Responses are edge-cached under the
  storage key, the same as R2 bytes.
- **Outbox kinds.** `_keys.json` changes (create, revoke or retire a key,
  disable an app, change the maximum lifetime) need an outbox row of their
  own, such as `keys_sync`. That needs a new migration, because
  `outbox.kind` is a check constraint.
- **Worker bundle.** The Worker is built in the pnpm workspace and embedded
  in the binary with `go:embed`, like the dashboard (D032), so Connect
  Cloudflare and upgrade redeploys need no Node on the host. The bundle
  carries a version, and serve redeploys it when the version differs.
- **New packages** follow "no package before its code". Expect
  `storage` to gain the S3 adapter in place, a `routes` (or `publish`) package
  for the route publisher and reconciler when it outgrows `outputs`, and
  `cloudflare` for the API client used by Connect Cloudflare.

## Tasks

Order: T01 → T02 → T03 → T04 → T05, with T06 alongside T03–T05 because it
depends only on T01. T07 comes last.

Each task is a vertical slice with its own tests. Port behaviour and tests
from the sources listed, but don't copy code paths that assume the old data
model. `clipmux-lms` paths are under `~/clipmux-lms`, `vod` paths are under
`~/vod` (archived), and old plans are under the tag `archive/open-source-p17`
in clipmux-lms.

| Task | Scope | Port from |
| --- | --- | --- |
| T01 | **done** (D034) — **S3-compatible `Storage` adapter** for R2 and S3 on `aws-sdk-go-v2` (D011). It sits behind the same `Storage` interface, with 32 MiB parts that ignore the declared size (D010), path-style addressing, and browser URLs signed for the public endpoint (architecture "Compatibility"). `PresignPut` is create-once with a signed `If-None-Match: *`, and a 412 counts as "already written" (D016). `Capabilities()` lands here (deferred by D010) and reports reachability per pair, so the dashboard shows only valid combinations. `storage_configs` rows (`r2`, `s3`) are read with secrets sealed under D017 (associated data `storage_configs` / `{id}`). Each asset uses its own `storage_id`, and new uploads use the project default. The uploads, worker sign, commit, delivery, delete and setup-test paths all resolve the store per asset. Bucket CORS (any origin, PUT, exposes `ETag`) is applied and checked. Contract tests run one shared suite against fs and a local S3 store in CI (Garage or SeaweedFS, picked by the conformance checks below), including create-once, multipart resume, `List` paging and `DeletePrefix`. Request timeouts are bounded, and no error carries a presigned URL. | clipmux-lms `server/src/utils/R2.ts`, `server/src/utils/media-cleanup.ts` (`deletePrefix`, `DeleteObjects` error handling); `archive/open-source-p17:docs/plans/P21-s3-compatible-storage.md` (C2 client options and timeouts, C3 conformance checks, C4 store spike: SeaweedFS / Garage / RustFS, MinIO excluded) |
| T02 | **done** (D035) — **`Delivery` interface and route publisher.** `Delivery` (`BaseURL`, `Publish`, `Unpublish`, `Routes`, `Capabilities`) has two implementations. Built-in confirms only, as today (D013). Worker writes `_routes/{pid}.json` and `_keys.json` to the R2 store with `Put`, deletes routes on unpublish, and lists `_routes/` for the reconciler. Route records follow the design notes above (`v`, `app_id`, `asset_id`, `output_id`, `policy`, `backend`, `outputs`). A new `keys_sync` outbox kind (migration) rewrites `_keys.json`. The reconciler runs every 15 minutes: it lists `_routes/`, compares the routes with Postgres, deletes revoked or unknown IDs, rewrites any that differ, and rewrites `_keys.json`. Disable and enable an app through `route_sync`. Revocation keeps its 200 or 202 `revoking` contract (D021). Retired-output cleanup (known gap) republishes routes so `outputs` stops naming deleted files. Tests use real Postgres plus the T01 local store: drift repair, a failed write retried, and revocation confirmed only after the delete lands. | architecture "Route records"; D013, D019, D021; clipmux-lms `server/src/utils/media-cleanup.ts` (prefix listing) |
| T03 | **done** (D036) — **Cloudflare Worker** in TypeScript at `worker/`, added to the pnpm workspace (D030 toolchain: TypeScript 7, Biome, pinned versions). It is ES256 on WebCrypto with strict base64url and exact claim names (D015). Its key derivation is the D012 HMAC, using a version ring from Worker secrets. It reads routes and `_keys.json` through the R2 binding and caches each for 60 s. It generates the master, `no-store`, with `{output_id}/` prefixes. Bytes use the edge cache (Cache API) keyed on the storage key, immutable for a year. The extension allowlist, `nosniff`, Content-Type from the extension, Range, HEAD and CORS `*` match the Go handler. An `origin` backend passes requests to the built-in handler. Statuses are 401 `token_expired`, 403 `denied` and 404. It must pass `internal/delivery/testdata/vectors.json` and the `internal/contentkey` vectors unchanged, from fixtures built as R2 objects. It is tested on local workerd (`@cloudflare/vitest-pool-workers` or Miniflare) in CI, on every PR that touches `worker/` or the vectors. A Go-side test proves the publisher's route records and `_keys.json` are what the Worker fixtures assume. | clipmux-lms `delivery/src/index.ts` (MIME table, Cache-Control, CORS), `delivery/src/resource-tokens.ts`, `delivery/test/{playback,security}.spec.ts` case tables (HEAD and Range semantics, encoded paths, keys never served without authorization), `delivery/wrangler.jsonc` (R2 binding, custom domain, staging env); vod `delivery/src/index.ts` (`resolveRange`, `cacheControlFor`), `delivery/src/claims-cache-range.test.ts` |
| T04 | **done** (D037) — **Connect Cloudflare.** The API (owner session, `internal/manage`) asks for a token and the media domain. It verifies the token and records its ID and hash, but never the raw value. It checks that the zone is active in that account and that Workers Paid is on, subject to the open question below. It creates the bucket or uses an existing one (and warns when the token isn't scoped to one bucket), applies CORS, and derives S3 credentials (token ID and SHA-256 of the value), sealed into a new `r2` `storage_configs` row that becomes the default. It uploads the embedded Worker with the R2 binding, the content-key ring as secrets and the origin URL as a var. It removes the record that points the media hostname at the app and attaches the Worker as a custom domain, as one step. It switches the project's delivery to Worker and runs the setup test straight away. Then it offers the library move (T05). A dashboard card on the setup or settings page shows each step's status and says that the token stays live in Cloudflare. When storage authentication fails, it shows "Reconnect Cloudflare" instead of failing jobs. An upgrade redeploys the Worker when its version changes, asking for a token if none is available. Manual `wrangler` instructions ship as the fallback. The Cloudflare API is tested against a recorded fake, with no real account in unit tests. | vod `docs/deploy.md` ("Minimum Cloudflare surface", Path A wrangler steps), `scripts/bootstrap.sh` (wizard flow); clipmux-lms `delivery/wrangler.jsonc`; architecture "Optional upgrades" |
| T05 | **done** (D038) — **`move` job.** A control-plane job (D008: executors never claim it) under the one-mutating-job rule, with 409 `job_active` and `supersede`. It copies the asset's source and current output, plus retired outputs still in the 7-day window so mid-session viewers aren't cut off, from the old store to the new one. It verifies every size against the manifest and the listing. In one transaction it switches `storage_id` and writes `route_sync` rows with the new `backend`. After publication it deletes the old prefixes, after a delay longer than the 60 s route cache. Each pass is safe to repeat, like delete (D019). `video.asset.updated` reports it. A library move queues one `move` per asset that isn't on the default store, with bounded concurrency. Its progress (moved, pending, failed) shows on a Jobs page or card in the dashboard, and the dashboard lists assets that haven't moved, because they still need the control plane (architecture "Risks"). | architecture "Per-asset storage"; D019 (`internal/lifecycle` delete job shape); P21 C2 `copyStorageObject` deadline handling |
| T06 | **done** (D039) — **`POST /v1/assets` import from URL** (the LMS gate puts library import in Phase 2). The request takes `url`, `playback_policy`, `passthrough` and an `Idempotency-Key`. It creates the asset (`processing`) and an `import` job. A worker downloads the URL and writes `uploads/{asset_id}/source` through `sign` for source parts (architecture "Worker API"), then continues as a transcode. SSRF controls: http and https only, with no credentials in the URL. Private, loopback, link-local, multicast, reserved, CGNAT, IPv6 ULA and IPv4-mapped ranges are blocked after DNS, and the resolved IP is pinned for each connection with a dialer `Control` check. Every redirect is checked again, with a cap. The download has a size cap and the idle watchdog (D016). Failure codes are `import_url_refused` (permanent) and `import_failed` (transient). Agent sources (`agent_source_id`) wait for Phase 3. Tests cover every blocked range, a redirect to a private address and DNS rebinding with a local resolver. | vod `transcoding/clipmux_transcoder/utils/network.py` (`_host_resolves_to_public_ip`, redirect handling); architecture "Threats and controls" (SSRF row) and "Open decisions" (SSRF hardening); `plans/lms-migration.md` (library import) |
| T07 | **done, including the real-account run** (D041, D090) — **Exit test**, automated in CI, as listed above. It extends `internal/e2e` (D033): the real binary and the T01 local store, with the Worker on workerd in front of serve as the media origin. The Phase 1 Chromium suites run against the Worker hostname. The revocation timing asserts ≤ 60 s plus a small margin, without a test-only cache knob, so the shipped TTL is what's proven. A second job runs the same suite against a real Cloudflare account if the open question below says so. | D033 harness; `internal/e2e`, `e2e/web` |

## Open questions

These are for the user to decide before T03 or T04 starts. Each answer becomes a
decision record.

1. **CI: a real Cloudflare account, or only workerd?** Local workerd proves
   the Worker's logic and the shared vectors. It can't join Go's S3 writes to
   the Worker's R2 binding, because Miniflare's R2 is local storage, not
   Garage. So a workerd-only exit test needs a test-only bucket shim behind
   the Worker's reads, and the real R2 binding, custom domain, Cache API and
   Connect Cloudflare API calls would be proven only by hand. A real account
   (a dedicated test zone, a scoped token in CI secrets, a Workers Paid
   subscription) proves the whole path. Recommendation: workerd on every PR,
   plus a real-account job on main, weekly and by hand, like the Safari job
   (D033).
2. **Is Workers Paid a hard requirement?** The architecture says the adapter
   "should require" it, for the request quota and Cloudflare's video terms.
   Free also has a 10 ms CPU limit per request, which ES256 verification and
   master generation may approach. Options: refuse to connect without Paid,
   or warn and allow Free for trials. Recommendation: require it, and check
   the subscription during Connect Cloudflare.
3. **How is the content key secret rotated now that the Worker holds a
   copy?** There is no rotation command yet (D022, known gaps). With the
   Worker, a new version must also reach the Worker's secrets before any
   output uses it. That needs a token with Workers Scripts edit permission,
   which the app deliberately forgets after setup. Options:
   - (a) Rotation asks the owner to paste a token again, and deploys the
     ring before new outputs use the new version.
   - (b) Keep the token sealed in Postgres, which widens what a database and
     secrets-key leak exposes.
   - (c) Rotation stays manual with `wrangler secret put` plus a
     `clipmux admin` command.

   A leaked secret also means re-encoding, and a stolen Cloudflare token can
   read Worker secrets (architecture threats table). Recommendation: (a),
   with ordering enforced. A version becomes active for new outputs only
   after the Worker reports it holds that version.
4. **Serve segments straight from an R2 custom domain?** This is the
   architecture's open decision. Encrypted segments would come from a public
   custom domain on a bucket without sources, and only playlists and keys
   would run through the Worker, cutting its requests by more than 99%. The
   cost: revoking a playback ID would stop key fetches, not segment fetches,
   and sources would need a separate bucket. Recommendation: not in Phase 2,
   so revocation semantics stay as the exit test states. Revisit with real
   request counts, and keep it in known-gaps.
5. **Should the built-in handler cache routes?** Today every request reads
   Postgres, so revocation is immediate (known gaps). A short cache, matching
   the Worker's 60 s or less, would cut queries per segment, but it adds a
   revocation delay that must be documented. It matters more now that the
   `origin` backend sends unmoved assets through serve. Recommendation: cache
   in process for a few seconds, with revocation busting the entry on the
   replica that handled it, and document the delay.
6. **Leader election for more than one replica.** Phase 2 adds the
   reconciler and the move scheduler to the sweepers that already assume one
   `serve`. Options: `pg_try_advisory_lock` per loop (per the architecture),
   or keep the one-replica assumption documented through Phase 2. Every loop
   is already idempotent, so duplicates cost work, not correctness, except
   timing. Recommendation: one advisory-lock leader for all background loops,
   added in T02 alongside the reconciler.
