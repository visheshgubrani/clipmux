# Known gaps

The standing record of deliberate gaps and deferred work. Add a limitation here
when you introduce or discover it; remove it when it is closed.

## Bootstrap (2026-11-05)

- Phase 1 beyond T09 (M1 done): upload → encode → encrypt → commit → signed
  playback works end to end, with signed webhooks, asset delete and catch-up.
  No re-encode or playback-ID endpoints until T09b; no dashboard, first-boot
  setup or player package yet.
- Delivery reads Postgres on every request (route, output, signing key);
  there is no route cache yet, so revocation is immediate but every segment
  costs a few queries. The server's internal signing key (dashboard preview,
  share links) arrives with T10.
- Serving playback from the same origin as the API and dashboard is the
  default; set `CLIPMUX_MEDIA_URL` to a separate registrable domain in
  production so a hostile stored file can't reach dashboard cookies.
- Retired outputs and files from failed or lost attempts are not deleted yet
  (the 7-day retire window and 30-minute cleanup from the architecture).
- Worker sandbox is the M1 subset: empty environment, demuxer and protocol
  allowlists, wall-clock limits. No network namespace, rlimits or CPU cap
  yet, so the built-in worker can still compete with the API for CPU.
- Workers report no free disk space at claim, and the server doesn't check
  it; a full disk fails the attempt (transient).
- The content key secret is a file in the data directory, not yet encrypted
  in Postgres (T10). Back it up: losing it makes every output unplayable.
- Sign caps file count per attempt, not total bytes; a presigned filesystem
  PUT is capped at 5 GiB per object. `GET /v1/assets` looks up by
  `passthrough` or lists changes `updated_since`; a general listing for the
  dashboard comes with T10. Listing loads each asset's playback IDs with one
  query per asset.
- Idempotency keys are kept forever; a cleanup window (e.g. 24 h) is not
  enforced yet.
- If the create transaction fails after storage started a multipart upload,
  the abort is best effort; a failed abort leaves an orphaned staging
  directory that nothing sweeps.
- No rate limiting on the upload endpoints yet. The queue, `/worker/v1/{claim,heartbeat,fail}` and the
  lost-lease sweeper run, but nothing enqueues jobs until T05 and no worker
  process exists until T06. Claim payloads carry no source URL or content key
  until T04/T08.
- Filesystem storage preallocates uploads as sparse files, so disk space is
  not reserved up front; a full disk fails the part write. Abandoned
  multipart uploads are cleaned up by the upload sweeper (T05), and `tmp/`
  leftovers from a crash are not swept yet.
- Every asset uses the one filesystem store; `storage_configs` rows are not
  read yet (per-asset storage arrives with R2 in Phase 2).
- Sweepers assume one `serve` replica (no `pg_try_advisory_lock` leader yet).
- `clipmux serve` prints a fresh setup token on every boot (no owner table
  until T10). The schema has no owner/session tables yet.
- Webhooks: endpoints are created only with `clipmux admin create-webhook`;
  there is no way to list, disable or rotate one until the dashboard (T10).
  No per-endpoint circuit breaker, so a dead endpoint costs up to 10 s per
  delivery for a day. Outbox and delivery rows are never pruned. Owner-set
  endpoint URLs are not filtered for private addresses (D018). A failed
  re-encode is visible only through its webhook, not through catch-up.
- The secrets key (D017) can't be rotated yet. Webhook secrets are sealed with
  it; adapter secrets, the internal signing key and the content key secret
  move under it in T10.
- `clipmux push|listen` print "not implemented"; `clipmux admin` has
  create-key, create-signing-key and create-webhook.
- No Dockerfile or production compose yet (P1-T10). The FFmpeg recipe in
  `toolchain/` is lifted from the archived repo and has not been built here.
- No captions, chapters or transcripts. The LMS depends on them, so they gate
  the LMS migration (plans/lms-migration.md); OSS-V2 leaves them as an open
  roadmap decision.

## Open decisions carried from architecture.md

- Licence and ffmpeg GPL compliance (gated on going public, D005).
- Native sandbox for Windows/macOS agents (v1 requires Docker, WSL2 or a VM).
- Serving segments from an R2 custom domain with only playlists/keys through the Worker.
- Token-signing SDK languages.
- When captions, storyboards and viewer analytics enter the roadmap.
- Confirm defaults: 7-day max token lifetime, retry budgets 5/3, 15-minute
  reconciler, per-output keys derived from one secret.
- Audit leftovers: leader election with `pg_try_advisory_lock` for multiple
  replicas; shared Go/Worker test vectors; where the `vid` claim is logged; SSRF
  hardening (pin resolved IP, block IPv6 ULA and IPv4-mapped); timeouts for jobs
  pinned to an agent that never returns; route caching in the built-in handler;
  backup docs for Postgres, the secrets-encryption key and the content key secret.
