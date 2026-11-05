# Known gaps

The standing record of deliberate gaps and deferred work. Add a limitation here
when you introduce or discover it; remove it when it is closed.

## Bootstrap (2026-11-05)

- Phase 1 beyond T03: no integration API, worker, sign/complete, delivery or
  dashboard yet. The queue, `/worker/v1/{claim,heartbeat,fail}` and the
  lost-lease sweeper run, but nothing enqueues jobs until T05 and no worker
  process exists until T06. Claim payloads carry no source URL or content key
  until T04/T08.
- Sweepers assume one `serve` replica (no `pg_try_advisory_lock` leader yet).
- Webhook events are written to the outbox (errored, reencode_failed) but not
  delivered until T09; the event payload shape is provisional until then.
  `clipmux serve` connects to Postgres, applies migrations, serves `/healthz`,
  `/readyz` and a placeholder page, and prints a fresh setup token on every
  boot (no owner table until T10).
- The schema has no owner/session tables (T10) and no per-endpoint webhook
  delivery tracking; the `outbox` table holds events only (T09 extends it).
- Content-key and adapter-secret encryption columns exist, but nothing writes
  or reads them yet; the external encryption key arrives with T08/T10.
- `clipmux worker|push|listen|admin` print "not implemented".
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
