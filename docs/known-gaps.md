# Known gaps

The standing record of deliberate gaps and deferred work. Add a limitation here
when you introduce or discover it; remove it when it is closed.

## Bootstrap (2026-11-05)

- Phase 1 has exited (D040): the Go, Chromium, Safari and ExoPlayer exit
  suites and the image job pass in CI. iPhone and AirPlay are checked by hand
  only; Firefox isn't in CI. The dashboard is minimal: no
  share links, no playback-ID management in the UI, and a sample preview
  that waits for Play rather than autoplaying.
- Built-in delivery caches routes, signing keys and outputs for 30 s
  (D072). On one node every published change is seen at the next request;
  a change that publishes nothing (the project's maximum token lifetime)
  takes up to 30 s, and so does another replica's.
- Serving playback from the same origin as the API and dashboard is the
  default; set `CLIPMUX_MEDIA_URL` to a separate registrable domain in
  production so a hostile stored file can't reach dashboard cookies.
- File retention (D070) checks 500 assets an hour for orphaned output
  prefixes, so a full pass over a large library takes days; retired and
  replaced outputs go on schedule regardless. Abandoned multipart uploads on
  R2/S3 (a failed abort) are left to a bucket lifecycle rule, which Connect
  doesn't set.
- The media tool sandbox (D027) gives the CPU a priority, not a quota:
  `SCHED_IDLE` yields to the API, and only compose `cpus:` caps it. Landlock
  is best effort: on kernels before 5.13 (or with it disabled) the tools can
  read any file the worker's user can. Agents off Linux have no sandbox.
- Imports have no known source size, so the claim's free-disk check passes
  them; a full disk fails the attempt (transient).
- Agent pairing codes are 8 characters (about 39 bits); wrong codes are
  limited per address and overall (D067), and a code lasts 15 minutes and
  works once.
- Content key secrets live in Postgres sealed with the secrets key (D022).
  Back up Postgres and the secrets key together: losing either makes every
  output unplayable (docs/operations.md). Rotating the content key secret
  (D071) doesn't re-encode existing outputs; each keeps its version.
- Sign caps file count per attempt, not total bytes; a presigned filesystem
  PUT is capped at 5 GiB per object. `GET /v1/assets` looks up by
  `passthrough` or lists changes `updated_since`; the owner's `GET
  /v1/library` lists every app's assets.
- If the create transaction fails after storage started a multipart upload,
  the abort is best effort; on the filesystem a staging directory left that
  way is swept after 7 days without writes (D070).
- Rate limits (D067), including the sign-in, setup and pairing windows,
  live in one serve process: a second replica doubles every budget. The
  request and loop counters behind `/metrics` (D066) are per process too;
  the job, outbox and dispatch counts come from Postgres.
- Filesystem storage preallocates uploads as sparse files, so disk space is
  not reserved up front; a full disk fails the part write. Abandoned
  multipart uploads are cleaned up by the upload sweeper (T05), and `tmp/`
  leftovers after 24 hours (D070).
- Storage configs other than the filesystem can be added only from code
  (`stores.AddS3`) until Connect Cloudflare (P2-T04); there is no API or
  dashboard form for a generic S3 bucket, and no way to remove a config or
  rotate its credentials.
- One serve replica is the supported shape (D063). One replica runs the
  background loops (advisory-lock leader, D035), but the rate limits, the
  sign-in and setup limits, the setup token, the drain flag, the route
  cache and the storage signer are per process. Saving new URLs updates the
  storage signer of the serve that saved them only; another replica keeps
  the old public URL until it restarts.
- No audit log (D071): owner actions and key operations log at info with
  their request IDs, but nothing durable records who did what. No
  OpenTelemetry tracing (D066).
- Webhooks: owner-set endpoint URLs are not filtered for private addresses
  (D018). A failed re-encode is visible only through its webhook, not
  through catch-up. When an endpoint's breaker closes, every delivery that
  came due meanwhile is tried at once (D069).
- `clipmux listen --since` and `GET /v1/events` reach back 30 days, the
  webhook retention window (D069). `clipmux push` keeps an
  unfinished push's upload token in a 0600 state file under the user's cache
  directory until it completes.
- The image is ~575 MB and built from source on a cache miss (~20 min). CI
  pushes it privately to ghcr on main for serverless providers (D058), but
  it isn't public, so every install builds it until launch.
- Phase 2's exit test passes on the binary with a fake Cloudflare API and
  the Worker's logic on Node in front of S3 (D041). Still open before the
  phase exits on Cloudflare: the setup test through the Worker, the Chromium
  suites against the Worker hostname, and a run on a real test zone.
- Connect Cloudflare is tested against a fake API, not a real account: the
  Workers Paid check (plan name containing "workers" and "paid"), the script
  upload format and the custom-domain call need a run against a real test
  zone before the Phase 2 exit (D037). The manual wrangler path has no CLI
  for adding R2 storage, switching delivery or printing `CONTENT_KEYS`.
- An asset pushed with `--local` alone re-encodes only on its PC, and only
  while the file is unchanged; `--upload-source` lifts that (D059). Each
  upload_source attempt leaves an empty `writing` output row, because every
  claim creates an output; nothing serves it and nothing cleans it up yet.
- GPU encoding is proven on real hardware for VAAPI on AMD (a self-hosted
  runner) and for NVENC and `cuda-decode` on Modal (D051); Intel's iHD driver
  and Quick Sync have no test machine. A GPU stage's failure isn't
  classified, so any one falls back to the next stage. GPU tools may open
  AF_UNIX SOCK_SEQPACKET sockets, which CUDA needs.
- The agent tarball (D061) needs glibc 2.39 or newer, and is a CI artifact
  until launch. Its arm64 build runs only in CI (no local arm64 machine), and
  bundles no NVIDIA libraries: NVENC on a native agent needs the host's
  driver, as in Docker. Without `/usr/share/libdrm/amdgpu.ids` on the host,
  AMD cards encode fine but print a warning about their name.
- Phase 3's exit passed on the real binary (D050), and NVIDIA on Modal
  (D051); still by hand: a real suspend of the M4 Mac's Docker agent, and
  registering the self-hosted GPU runner. The pairing page
  (`/workers/connect`) has no Playwright suite yet; the Workers and Jobs
  pages do (D060). The Jobs page shows which provider ran an attempt, but not
  its GPU or its estimated cost.
- Disconnecting Cloudflare (back to built-in delivery) has no endpoint yet.
- Provider spend (D055) is an estimate from the hourly rate set in Connect,
  not the provider's bill, and a machine that runs past its reservation
  isn't stopped at the limit, so a month can overshoot by that much.
- Phase 4's exit passed in CI on the real binary against a fake RunPod
  (D062), but not yet on real accounts: `exit-providers.yml` is written and
  needs the secrets `RUNPOD_API_KEY`, `MODAL_TOKEN_ID`, `MODAL_TOKEN_SECRET`
  and `GHCR_PULL_TOKEN`. Until it runs, these are unproven: Modal's SDK
  (D057, tested through a seam) and the assumption that a sandbox's command
  replaces the image's entrypoint; RunPod's real REST API (D056); both
  providers' cold starts against the 10-minute claim deadline; and whether
  both accept a digest-only image reference (D058). Modal has no fake
  provider in the CI exit, only RunPod.
- Routing (D053) records the built-in worker's liveness per project, not per
  replica: with several serve replicas, any one worker's calls keep "the
  server" online. A job barred from the fallback because its file failed
  waits for the primary without a bound, shown as `fallback_refused`.
- The content key secret can't be rotated; when it can, rotation must
  redeploy the Worker's ring through Connect before new outputs use it.
- No captions, chapters or transcripts. The LMS depends on them, so they gate
  the LMS migration (plans/lms-migration.md). Phase 5 adds them
  (plans/P5-production.md, D063).
- Phase 5's hardening track is complete (D074; D064–D073). Its exit checks
  still to run are the phase exit's (P5-T20): the soak run, the Playwright
  suites under the enforced CSP and TOTP in a browser, a backup-and-restore
  drill, rotation on the real binary, and CI's compose checks on a runner.
- Background loops' log lines carry job and attempt IDs where they log them
  by hand; only request lines and the worker's attempt lines carry them
  throughout. Each `/metrics` scrape counts finished attempts with a scan of
  `attempts`, cached for 10 s, and attempts are kept as history. There is no
  counter per rate limit, only 429s by route (D066, D067).
- The drain (D065) waits up to 20 s for the built-in worker and gives
  requests 15 s; an upload part or segment still running after that is cut.
  A PC agent whose server is unreachable while it stops can't hand its
  attempt back, and its lease lapses as before. Streams no longer use
  sendfile, which costs some CPU on large filesystem transfers.

## Open decisions carried from architecture.md

- Licence and ffmpeg GPL compliance (gated on going public, D005).
- Native sandbox for Windows/macOS agents (v1 requires Docker, WSL2 or a VM).
- Serving segments from an R2 custom domain with only playlists/keys through the Worker.
- Token-signing SDK languages.
- When captions, storyboards and viewer analytics enter the roadmap.
- Confirm defaults: 7-day max token lifetime, retry budgets 5/3, 15-minute
  reconciler, per-output keys derived from one secret.
- Audit leftovers: where the `vid` claim is logged. (The shared Go/Worker
  vectors are D015/D036, pinned-job timeouts D045, the route cache D072 and
  the backup docs docs/operations.md.)
