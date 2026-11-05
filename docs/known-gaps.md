# Known gaps

The standing record of deliberate gaps and deferred work. Add a limitation here
when you introduce or discover it; remove it when it is closed.

## Bootstrap (2026-11-05)

- Phase 1 is feature-complete pending the exit test's Safari and ExoPlayer
  runs (T12 CI jobs). The dashboard is minimal: no jobs or workers pages, no
  share links, no playback-ID management in the UI, and a sample preview
  that waits for Play rather than autoplaying.
- Superseded and failed re-encodes leave their writing outputs and files in
  place, like lost attempts (cleanup is the 7-day / 30-minute gap below).
- Delivery reads Postgres on every request (route, output, signing key);
  there is no route cache yet, so revocation is immediate but every segment
  costs a few queries.
- Serving playback from the same origin as the API and dashboard is the
  default; set `CLIPMUX_MEDIA_URL` to a separate registrable domain in
  production so a hostile stored file can't reach dashboard cookies.
- Retired outputs and files from failed or lost attempts are not deleted yet
  (the 7-day retire window and 30-minute cleanup from the architecture).
- The media tool sandbox (D027) gives the CPU a priority, not a quota:
  `SCHED_IDLE` yields to the API, and only compose `cpus:` caps it. Landlock
  is best effort: on kernels before 5.13 (or with it disabled) the tools can
  read any file the worker's user can. Agents off Linux have no sandbox.
- Workers report no free disk space at claim, and the server doesn't check
  it; a full disk fails the attempt (transient).
- Content key secrets live in Postgres sealed with the secrets key (D022).
  Back up Postgres and the secrets key together: losing either makes every
  output unplayable. There is no command to rotate the content key secret.
- Sign caps file count per attempt, not total bytes; a presigned filesystem
  PUT is capped at 5 GiB per object. `GET /v1/assets` looks up by
  `passthrough` or lists changes `updated_since`; the owner's `GET
  /v1/library` lists every app's assets. Listing loads each asset's playback IDs with one
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
  Saving new URLs updates the storage signer of the serve that saved them
  only; another replica keeps the old public URL until it restarts.
- The owner can't turn on TOTP yet. The sign-in limiter and the setup token
  live in one serve's memory (one replica assumed); expired sessions are
  never pruned.
- Webhooks: an endpoint's secret can't be rotated (create a new endpoint and
  disable the old one). No per-endpoint circuit breaker, so a dead endpoint costs up to 10 s per
  delivery for a day. Outbox and delivery rows are never pruned. Owner-set
  endpoint URLs are not filtered for private addresses (D018). A failed
  re-encode is visible only through its webhook, not through catch-up.
- The secrets key (D017) can't be rotated yet. Webhook secrets, the internal
  signing key and the content key secrets are sealed with it; adapter
  secrets join them with R2/S3 in Phase 2.
- `clipmux push|listen` print "not implemented"; `clipmux admin` has
  create-key, create-signing-key and create-webhook.
- The image is ~575 MB and built from source on a cache miss (~20 min); it
  isn't published, so every install builds it until launch.
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
