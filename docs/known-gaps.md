# Known gaps

The standing record of deliberate gaps and deferred work. Add a limitation here
when you introduce or discover it; remove it when it is closed.

## Bootstrap (2026-11-05)

- Everything in Phase 1 beyond the skeleton: no schema, queue, API, worker,
  delivery or dashboard yet. `clipmux serve` only serves `/healthz` and a
  placeholder page, prints a fresh setup token on every boot, and does not
  connect to Postgres yet (it only checks DATABASE_URL is set).
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
