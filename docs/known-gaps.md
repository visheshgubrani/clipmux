# Known gaps

The standing record of deliberate gaps and deferred work. Add a limitation here
when you introduce or discover it; remove it when it is closed.

## Bootstrap (2026-11-05)

- Connect Cloudflare does not configure hostname security filters. Browser
  Integrity Check can reject legitimate API/media clients with error 1010.
  Real staging uses an owner-approved `bic: false` configuration rule scoped
  only to its two test hostnames (D090); production settings remain unchanged.
- Real Cloudflare tests are opt-in local runs. The proposed main/weekly
  real-account CI workflow and its repository secrets are still unconfigured.

- Phase 1 has exited (D040): the Go, Chromium, Safari and ExoPlayer exit
  suites and the image job pass in CI. iPhone and AirPlay are checked by hand
  only; Firefox isn't in CI.
- Phase 7 closed with open items (D107), to settle before P6-T19 as the
  owner chooses:
  - A share link's HLS URL is checked as a signed playlist that loads, not
    played in hls.js and ExoPlayer.
  - Edge counts match Analytics Engine exactly but not the traffic, because
    it samples even one viewer (D106); a per-request data point index is
    untried.
  - On the real zone the Worker answered an origin-backed video without
    reaching serve (D106), unexplained, so "a forwarded fetch counts once"
    is shown by the vectors and the Worker's tests only.
  - No end-to-end log scan has run with share links; their redaction is
    unit-tested.
- Share links (D104) are made on a video's Sharing tab or over the API. The
  dashboard offers fixed expiries (a day to 90 days, or none) where the API
  takes any time. A link's expiry can't be changed: revoke it and make another.
  Built-in delivery refuses a revoked link's ID at once; the Worker within
  its 60 s route cache, so a token already issued plays at the edge until
  then. The share page needs the dashboard build: a binary built without it
  answers 404 there, though the link's playback endpoint still works. The sample preview waits for Play rather than autoplaying.
  There is no audit log of owner actions, and no per-attempt GPU or cost
  on Jobs.
- The dashboard's main bundle is about 230 KB gzipped; the player and the
  chart load on their own pages, but the other pages aren't split by route.
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
- A removed storage config stays as a row without its credentials, since assets,
  move jobs and the delivery history still name it. Rotated credentials
  reach this serve's open stores at once; a second replica keeps the old
  ones until it restarts.
- The library's `q` is an ILIKE scan of titles and passthroughs, with no
  trigram index: fine for tens of thousands of assets, not millions.
- Phase 5 exited in CI (D085); its drills now run on the real binary (D094)
  and `exit-llm.yml` passed on real accounts (D095). Still to run: the
  providers' speech on real RunPod and Modal, and Safari and ExoPlayer
  showing subtitles. The drain check doesn't yet cover a throttled segment
  GET or a part PUT finishing.
- Real providers pass the Phase 4 exit (D097), but only as a local run with
  keys in `.env`: `exit-providers.yml` needs the repository secrets and
  `REAL_PROVIDERS` before it runs in CI. Whisper on a provider runs on
  Vulkan, at about 3–4 times slower than real time per 11 s clip (model
  fetch included); CUDA speech stays refused (D077). Modal's GPU type isn't
  recorded per attempt, so a measurement can't name the card it ran on. A
  failed real run can leave a RunPod registry credential behind when the
  test's disconnect is refused (409); it was deleted by hand.
- Security review findings (docs/security-review.md, D091), open until
  fixed:
  - Without Landlock, the built-in worker's tools can read serve's
    `/proc/<pid>/environ`, which holds the secrets key and the database
    password (medium).
  - The global sign-in failure limit (20 per 15 minutes) refuses every
    address, so anyone can lock the owner out. The setup-token and pairing
    limits work the same way (medium).
  - An unsandboxed agent's "owner jobs" are the Dashboard app's, which API
    keys and URL imports can also create.
  - Files can be added to an output after its commit; delivery serves any
    grammar-valid name, not only the manifest's.
  - S3/R2 part URLs don't bind the part length.
  - Seccomp allows namespace creation.
- Contributions are signed off with a DCO (D091), but nothing enforces the
  sign-off yet.
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
  `deploy/install.sh` (D093) is written and tested against images in a
  local registry, but has no public release to install until P6-T07, and
  the setup URL it prints is the host's first local address, which behind
  NAT isn't the one to open.
- The manual wrangler path has no CLI or dashboard action for switching
  delivery or printing `CONTENT_KEYS` (R2 storage is Storage → Add R2
  bucket, D110); the dashboard Connect flow is the supported path, now
  verified on a real account (D090).
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
  registering the self-hosted GPU runner. The Jobs page shows which
  provider ran an attempt, but not its GPU or its estimated cost.
- Disconnecting Cloudflare (D099) leaves the Worker, its custom domain and
  the token in the Cloudflare account, since no token is kept to remove
  them; the bucket loses the Worker's route records and keys, so the Worker
  refuses what it is asked. A route sync already in flight as delivery
  switches may write one record back; it names an ID that was live then.
- Analytics (D100, D103): edge counts reach `/v1/analytics` only once the
  owner connects edge analytics with a second, read-only Cloudflare token
  (P7-T05); until then `collecting.edge` says `not_connected`. Installs
  connected before P7-T04 count at the edge only once Connect is run again
  (`needs_redeploy`).
  - A first connect pulls 13 days back, inside the hourly retention, not
    Analytics Engine's three months. A pull stalled longer than that resumes
    13 days back, and the gap is lost.
  - Each pass asks for a whole day of hourly rows per playback ID in one
    query. A project busy enough to hit the SQL API's result limits would
    need smaller passes; none is known to.
  - Analytics Engine samples, even one viewer's burst (D106), so edge counts
    are estimates: on staging, bytes came out 0.012% over the truth. Sums
    are weighted by `_sample_interval`, but sampled-out points take their
    viewer keys with them, so edge unique viewers can undercount.
  - Edge analytics is connected from Delivery's card (P7-T08), and that card
    and the Analytics notice are tested only against the API's answers
    until the real zone run (P7-T10). There is no player beacon, so watch
    time is segments served, not seconds watched.
  - Unique viewers are kept per UTC day, so a range counts the days it
    touches: `24h` counts yesterday's and today's viewers.
  - The dashboard can't create an `analytics:read` key yet; ask for it in
    `POST /v1/apps/{id}/api-keys`.
  - A replica killed without SIGTERM loses the counts since its last flush,
    at most 15 seconds. A full recorder (100,000 keys between flushes)
    drops new keys and counts them in `clipmux_analytics_dropped_total`.
  - Daily rows go after 180 days and hourly ones after 14 (D105), so there
    are no lifetime totals.
  - The Worker counts a response's bytes from its Content-Length, so a
    viewer who aborts a segment counts it whole at the edge; built-in
    delivery counts the bytes it wrote. Only a body without a length is
    counted as it streams, and then only if it is read to the end.
  - The dashboard previews a public playback ID through its plain public
    URL, with no token, so those previews count as views. Signed IDs are
    previewed under the internal key and don't count.
- Provider spend (D055) is an estimate from the hourly rate set in Connect,
  not the provider's bill, and a machine that runs past its reservation
  isn't stopped at the limit, so a month can overshoot by that much.
- Modal has no fake provider in the CI exit, only RunPod; its real path is
  proven only by the real-account run (D097).
- Routing (D053) records the built-in worker's liveness per project, not per
  replica: with several serve replicas, any one worker's calls keep "the
  server" online. A job barred from the fallback because its file failed
  waits for the primary without a bound, shown as `fallback_refused`.
- The content key secret can't be rotated; when it can, rotation must
  redeploy the Worker's ring through Connect before new outputs use it.
- Subtitles, transcripts and chapters exist (Phase 5, D076–D084) and meet
  the LMS's needs by contract test (D085); the cutover itself is the next
  step (plans/lms-migration.md). What they don't do yet: uploaded caption
  files (`source: uploaded` is reserved), more than one language per asset,
  a language hint, translation, or editing a transcript. Word timings come
  from whisper.cpp's tokens, coarser than clipmux-lms's whisperX alignment,
  and with a speech API from whatever the provider returns (a sentence per
  cue when it gives no word times). Cues follow the encoded output's
  timeline, which starts at zero; a source whose audio started late relative
  to its video (an A/V offset) keeps that offset dropped by the encode, as
  every output does. Language detection on the owner's own English, Hindi
  and Hinglish recordings is still unmeasured (they aren't recorded yet), as
  is NVIDIA speech on a named Modal GPU (D077; D097 measured an unrecorded
  one of L4, A10G or T4).
- Chapters send the transcript's text to the connected LLM provider on
  every generation; there is no spend estimate for them either. A Claude
  refusal is final (no fallback model, by design, D083).
- The setup test proves subtitles only when something can transcribe as it
  starts, so a first boot's test usually skips them, and a provider's setup
  test doesn't prove its speech (D084); `exit-providers.yml` does, once
  T00's secrets exist.
- The speech API (D075) has no spend cap or estimate: every transcription a
  project in `api` or `local_then_api` mode sends is billed by the provider.
  Chunks are cut at segment boundaries without overlap, so a word spoken
  across a boundary (about every 25 minutes) can be split or lost. Whether a
  provider accepts fragmented MP4 is proven only at Connect. serve sends one
  transcription to the API at a time (D080), so a burst of uploads in `api`
  mode queues behind it.
- One serve replica is the supported shape (D063): rate limits, the route
  cache, the speech and chapters runners and the drain flag live in one
  process. High availability, and OpenTelemetry tracing, are not built.
  Jobs, attempts and dispatches are kept forever as history and spend, so
  those tables only grow. Owner-configured URLs (webhook endpoints, the
  speech API, the chapters LLM) aren't filtered for private addresses, by
  design (D018).
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

- Native sandbox for Windows/macOS agents (v1 requires Docker, WSL2 or a VM).
- Serving segments from an R2 custom domain with only playlists/keys through the Worker.
- Token-signing SDK languages (P6 open question 2).
- When storyboards enter the roadmap (viewer analytics did, D100, built in Phase 7; captions in Phase 5).
- Confirm defaults: 7-day max token lifetime, retry budgets 5/3, 15-minute
  reconciler, per-output keys derived from one secret (P6 open question 7).
- Audit leftovers: none open. (`vid` is kept as a keyed hash in analytics,
  D103; the shared Go/Worker vectors are D015/D036, pinned-job timeouts
  D045, the route cache D072 and the backup docs docs/operations.md.)

## Onboarding (2026-11-12)

- Connect Cloudflare is a pre-filled token link and one paste, not OAuth
  (D109). Cloudflare's third-party OAuth needs exact redirect URIs, so a
  self-hosted install needs a ClipMux-owned public client on a verified
  domain and a hosted callback relay. Neither exists yet, and a check that
  an OAuth token can mint the long-lived R2 credentials hasn't been done.
- The Connect token template still asks for Workers Routes: Edit, though no
  call uses a routes endpoint; attaching the custom domain may not need it.
  Drop it once a real-zone run without it passes.
- Browser approval for Modal and RunPod drives their own CLIs' flows
  (`TokenFlowCreate`/`TokenFlowWait`, `createFlashAuthRequest`), which
  neither provider documents for integrations (D109). A change there turns
  `authorize` into `502 approval_unavailable` and the dialog offers paste.
  RunPod's page names its Flash CLI, and the key it issues isn't restricted
  to pods and registry auths; Modal's token is a full workspace token, as a
  pasted one is. On a real run (2026-11-12) Modal's approval returned a
  token to the server (`TestLiveModalApproval`), but RunPod's console showed
  the key on its own page for the owner to paste and marked the request
  consumed without giving the key to the poller, so for RunPod the flow
  ends in `shown` and the dialog asks for the paste. An approval through to
  a full Connect has run only against fakes.
- Pending approvals live in serve's memory: a restart, or a second
  replica, drops them, and the owner starts again.
- Registry credentials for the private worker image are still pasted; they
  go away when the image is public (P6-T07).
