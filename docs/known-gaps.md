# Known gaps

The standing record of deliberate gaps and deferred work. Add a limitation here
when you introduce or discover it; remove it when it is closed.

## Bootstrap (2026-11-05)

- Connect Cloudflare does not configure hostname security filters. Browser
  Integrity Check can reject legitimate API/media clients with error 1010.
  Real staging uses an owner-approved `bic: false` configuration rule scoped
  only to its two test hostnames (D090); production settings remain unchanged.
- A dashboard page names the media origin it was served with in its
  Content-Security-Policy. Connecting or disconnecting Cloudflare reloads
  the page that did it; any other tab open from before can't play until it
  is reloaded, and its player says so. The setup test runs from serve, not a
  browser, so it can't catch a failure only a browser's policy causes.
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
  `SCHED_IDLE` yields to the API, and only compose `cpus:` caps it. The
  built-in worker needs Landlock (D120); with `CLIPMUX_UNSANDBOXED_WORKER`
  on a kernel without it, its tools can read any file serve's user can,
  the data directory's keys included. On an agent or a provider (Modal's
  gVisor reports no Landlock) Landlock stays best effort. Agents off Linux
  have no sandbox.
- Imports have no known source size, so the claim's free-disk check passes
  them; a full disk fails the attempt (transient).
- Agent pairing codes are 8 characters (about 39 bits); wrong codes are
  limited per address and overall (D067), and a code lasts 15 minutes and
  works once. So failures from many addresses can keep pairing refused for
  15 minutes; `clipmux admin create-pairing` on the host still works (D121).
- An owner signing in from a device they never used before still waits out
  the global sign-in limit during an attack; only a known-device cookie
  skips it (D121).
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
  `REAL_PROVIDERS` before it runs in CI. A failed real run can leave a
  RunPod registry credential behind when the test's disconnect is refused
  (409); it was deleted by hand.
- Speech on NVIDIA (D131): the amd64 image transcribes with CUDA, proven
  only by `deploy/modal/gpu_test.py`, not yet by a production
  transcription. Modal's gVisor offers no Vulkan device even with
  `graphics` asked for (measured on an L4 and a T4); that is Modal's
  choice, not ClipMux's.
- NVENC on Modal took about 45–50 s of video work for 2 s of 4K on an L4
  and a T4 (D131), against 2.3 s on VAAPI locally. Whether that is a fixed
  cost per encode under gVisor or a per-frame one is not yet measured. The agent
  tarball and arm64 images carry no CUDA backend, so an agent's NVIDIA card
  uses Vulkan, and an NVIDIA card without the Vulkan ICD uses the CPU.
- Renditions (D130): the commit can't tell a worker that stopped short of
  rungs above the largest it made from a smaller source, since the server
  never sees the source. There is no project-wide default or cap, and rung
  bitrates don't follow the source's own bitrate. A 4K ladder on a CPU-only
  worker is slow, and its 8 GiB memory limit with six encoders is
  unmeasured. `SignBudget`'s 20,000 files cover about 4 h 45 min of 4K.
- A full-GPU stage on a card that can't decode the source's codec (a T4 and
  AV1) fails and hands the ladder to the hybrid stage, which now scales on
  the GPU but still decodes on the CPU (D129). The GPU's codecs aren't
  probed ahead of time.
- A captioned video wakes one serverless machine only when the provider is
  the transcription's route at the encode's commit: the primary, or a setup
  test's executor (D128). With a provider reached only as the fallback
  ("my PCs, else RunPod"), the transcription still wakes a second machine.
  The hand-off is proven with the fake RunPod's local pods; it hasn't run
  on real Modal or RunPod yet. The transcription follows the encode on the
  machine rather than running beside it.
- The security review's six findings are fixed (D120 to D124). What they
  leave:
  - The Cloudflare Worker serves any grammar-valid name under a committed
    output; a file added after commit goes only when the hourly retention
    loop sweeps it, 16 to about 76 minutes after the commit (D124).
  - The Worker sends no `Strict-Transport-Security` header. The zone's
    HSTS setting (SSL/TLS, Edge Certificates) covers it; serve sends its
    own on https.
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
- The image is ~575 MB and built from source on a cache miss (~20 min).
  `release.yml` pushes it as the release tag with attestations (D136).
  Until v1.0.0, `clipmux.com/install.sh` points at the newest release
  candidate by hand, because `/releases/latest` skips prereleases; each
  new `-rc` moves the redirect in `deploy/site/clipmux.com.caddy`, and
  launch (P6-T20) points it back at `releases/latest`. The VPS's Caddy
  imports its own copy (`/etc/caddy/clipmux.conf`, which also serves
  `app.clipmux.com`), so a moved redirect is edited there too and Caddy
  reloaded.
- The one-line install has passed `toolchain/check_release.sh` on Ubuntu
  24.04 (amd64), not yet on Debian 12, or end to end on an arm64 server
  (the arm64 image passes its setup test in `image.yml`) (D136).
- The installer's setup URL comes from clipmux.com's CDN (Cloudflare's
  `/cdn-cgi/trace`): it moves with the zone, and an IPv6-only server gets
  only its own address. It can't tell whether the cloud firewall lets
  ports 80 and 443 in, so it says to check.
- A release isn't gated on `exit-safari.yml` (the owner's Mac) or the real
  provider and LLM suites; a release that isn't a candidate waits only for
  `ci.yml` and `exit.yml` on its commit. The `clipmux` binaries don't embed
  the dashboard (they are for `push`, `listen` and `admin`; `serve` from
  one shows the placeholder page), and they aren't signed or notarised, so
  macOS quarantines a downloaded one until `xattr -d com.apple.quarantine`.
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
- Connecting Cloudflare to a new media hostname deploys a new Worker named
  after it (D114). The previous hostname's Worker and custom domain stay in
  the account, serving from route records the bucket no longer updates,
  until the owner removes them in Cloudflare.
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
- Provider spend (D055) is Modal's bill up to its last reported hour plus
  an estimate since (D119); RunPod's is the estimate alone, from the hourly
  rate set in Connect, since ClipMux reads no RunPod bill. Modal's report is
  before credits, and a workspace plan without billing reports keeps the
  estimate. A machine that runs past its reservation isn't stopped at the
  limit, so a month can overshoot by that much.
- Modal has no fake provider in the CI exit, only RunPod; its real path is
  proven only by the real-account run (D097).
- Routing (D053) records the built-in worker's liveness per project, not per
  replica: with several serve replicas, any one worker's calls keep "the
  server" online. A job barred from the fallback because its file failed
  waits for the primary without a bound, shown as `fallback_refused`.
- The content key secret rotates (`POST /v1/content-keys/rotate`, Settings'
  Content encryption key), but only new encodes use the new version; existing
  outputs keep theirs until they are re-encoded one by one, and nothing
  re-encodes a library in bulk. With Cloudflare delivery it needs a token,
  since the Worker is redeployed with the new version before any output uses it.
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
- The ghcr package is public since `v1.0.0-rc.1` (D136), so the stock
  worker image pulls without registry credentials. A private image (a
  fork's, say) still needs them pasted, and RunPod keeps a private image's
  credentials itself, so after an upgrade it re-pins only a public image
  (D119); a private one runs the old build until RunPod is connected again.
- Modal's GPU prices are a table in `internal/modal/gpus.go` (D111), copied
  from modal.com/pricing on 2026-11-12, for the rate shown in Connect and
  the estimate of the hour since Modal's last billed one (D119); they drift
  when Modal changes its prices until the table is refreshed. The NVENC lists for both providers are hand-kept too.
- The PC agent has left the dashboard (D116) and stays a command-line and
  API option (docs/agents.md): nothing in the dashboard lists, pairs or
  revokes a PC, and setting routing to `workers` or `agents` takes the API.
  Whether to remove it, or to bring a small list back, is decided after
  launch, from whether owners ask for it.

## SDK and agents (D112)

- No MCP server yet: `clipmux mcp` (stdio, an API key, tools from the
  OpenAPI operations) comes after launch.
- The only server SDK is TypeScript (`@clipmux/node`); Python, Go and PHP
  have tested snippets for tokens and webhooks, not clients.
- The OpenAPI spec covers the integration API, not the management API.
- An app pairing approved but never collected (the CLI was closed) leaves
  the API key and signing key it issued, live and unknown to anyone; the
  sealed copy is dropped, so they can't be recovered. The owner revokes
  them on the app's page (the API key is named after the pairing). Revoking
  them automatically at expiry needs a sweep that doesn't exist yet (D113).
- npm publishes with provenance but still authenticates with the
  `NPM_TOKEN` secret, a 2FA-bypass token that npm says it is restricting
  for direct publishing (D136). Trusted publishing replaces it once each
  package's trusted publisher is set on npmjs.com.
- `@clipmux/player`'s element types import Video.js 10.2's declaration
  files, which don't all type-check; an app needs `skipLibCheck: true`.
- The packages are ESM only. A CommonJS app needs Node ≥ 20.19 (or 22.12)
  to `require()` them, and TypeScript's `moduleResolution: node` (node10)
  can't resolve their subpath exports.
- Safari's native HLS with `getToken({ native })` (D126) is checked by
  reading the code and in Chrome with `native: false`; the Safari suite
  runs on the owner's Mac, not in this release's CI.
- **No agent skills yet (T17).** The landing page and the docs point agents at `/agents.md`, `llms.txt` and the Markdown twins; `npx skills add visheshgubrani/clipmux` comes back to the landing page's Agents section, and the skills' stable docs anchors get a lint, when the skills exist (D134).

## Found by the first live integration (Edural, 2026-11-15; docs/integration.md)

- `@clipmux/uploader` 0.5.0 reads `Retry-After` from every part PUT
  (`xhr.getResponseHeader`, `index.ts`). The bucket's CORS doesn't expose it,
  so Chrome logs "Refused to get unsafe header" once per part, and a real
  `Retry-After` from storage is never seen. Read it only on 429 or 503, and
  add `Retry-After` to the bucket's `ExposeHeaders` in Cloudflare Connect.
- `playback.session(playbackId)` with a string target has no duration, so a
  native token is always signed for 12 hours; an app that wants a tighter one
  must re-sign with `playback.token(id, { native, durationSeconds })`. A
  `durationSeconds` option on `session` would remove the second call.

## Dashboard uploads (D135, 2026-11-19)

- A reload stops a dashboard upload until its file is chosen again: a
  browser can't read a file after a reload without the user picking it.
  Remembering file handles (the File System Access API) is Chromium-only and
  asks for permission, so the dashboard lists the stopped uploads instead.
- That list is every upload this browser remembers that ClipMux still has
  open, so an upload running in another tab of the same browser shows there
  too, and Discard would cancel it.
- A video whose upload was cancelled outside the dashboard's dialog shows as
  Cancelled, but the Videos page's Failed filter still counts it:
  `/v1/library/counts` counts by status, and it is `errored`.

## Odd sources (bug hunt 2026-11-18, F5–F11 and F17)

- A source whose frame size changes partway through encodes on the CPU
  stage: VAAPI's full-GPU and hybrid stages fail when the change makes
  ffmpeg rebuild the filter graph, so a GPU worker spends one failed attempt
  per GPU stage before the CPU stage encodes it. NVENC is untested here.
- A source without a duration in its header is measured by reading every
  video packet's time (no decoding), which for a long recording is a full
  read of the file before the encode starts.
- Clips under 3.5 s are not loudness-normalised.

