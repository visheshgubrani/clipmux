# Phase 5 — Production ready, with subtitles and chapters

Source: [architecture.md](../architecture.md), "Delivery" ("captions and extra
audio tracks can later be added to it without a new output"), "Storage
layout", "Open decisions", and [decisions.md](../decisions.md) where they
differ (D063 sets this phase's scope). Phase 5 makes ClipMux a complete
product that can be run in production and that the LMS can migrate to:

- **Track A, hardening (first):** structured logs with request IDs and
  redaction; deadlines, panic recovery and a real drain; metrics; rate limits;
  database timeouts and a migration lock; retention of rows and files; key
  rotation and TOTP; a route cache in built-in delivery; operator docs.
- **Track B, subtitles, transcripts and chapters:** whisper.cpp on workers as a
  routed `transcribe` job, or a speech API the owner connects (D075), a
  canonical transcript, WebVTT rendered and encrypted by serve, subtitles in
  the generated master and as a sidecar URL, and chapters from an LLM the
  owner connects (any OpenAI-compatible API, local like Ollama or online, or
  Claude through its Go SDK).
- **LMS parity:** every need in [lms-migration.md](lms-migration.md) is met
  and proven by a contract test. The cutover itself comes after this phase.

The integration API keeps every existing route and field; this phase only
adds. The output format, tokens and the worker path grammar don't change:
workers still can't write a `.vtt`, and a transcript reaches serve in the
`complete` body.

The supported production shape is **one serve replica** (D063). Limiters, the
route cache and the drain flag live in one process; HA stays a known gap.

Phase 4 exited in CI (D062). Its real-account run, Phase 2's real Cloudflare
run and Phase 3's by-hand checks carry into T00, a parallel track.

**Exit test:** a full run of every phase's suite leaks no secret into its logs
and drains cleanly on SIGTERM; retention deletes exactly what nothing plays;
an upload asking for subtitles and chapters becomes ready first, then plays
its subtitles in hls.js, Safari and ExoPlayer, and the LMS parity contract
passes.

Concretely, automated in CI (T20) on the real binary through `internal/e2e`
unless marked otherwise. The shipped constants apply, with no test knobs:
retention is exercised by backdating rows in Postgres (D033's rule).

**Hardening**

1. **No secret in any log.** Every instance in the rig (serve, the built-in
   child, agents, `--once` pods) records each secret it handled: API keys,
   upload tokens, playback tokens, `whsec_` secrets, pairing codes, agent keys
   and the `cmxd_` keys the fake RunPod receives. After the whole Phase 1–5
   suite runs with `CLIPMUX_LOG_FORMAT=json` and `CLIPMUX_LOG_LEVEL=debug`,
   none of those values appears in the logs, nor do `cmx_`, `cmxa_`, `cmxd_`,
   `whsec_`, `/v/eyJ`, `X-Amz-Signature` or `sig=`. Every line parses and has
   `time`, `level` and `msg`. No transcript text and no LLM key appears either.
   The one exception is the setup token line at boot (open question 3).
2. **SIGTERM drains.** In flight at the signal: a throttled segment GET, a
   32 MiB part PUT, a `/v1/events` long-poll and the built-in worker
   mid-encode.
   - `/readyz` answers 503 within 1 s.
   - The GET and the PUT finish with all their bytes, and the long-poll answers
     empty within 2 s.
   - The attempt ends `worker_stopped` within 15 s, not `lost` at 90 s, and
     its job requeues without spending the failure budget.
   - serve exits 0 within 30 s; after a restart the job commits and plays.
3. **Retention.** After backdating and `clipmux admin prune`:
   - a retired output older than the window loses its files and leaves the
     route's `outputs` (in the fake Cloudflare bucket too), while the current
     output and one retired a day ago still play from pinned URLs;
   - a lost attempt's files go at 31 minutes and stay at 29;
   - an orphan `out_` prefix older than 24 h goes, a younger one stays, and so
     does `media/{asset}/tracks/`;
   - rows past their windows are gone and the rest remain, by literal counts,
     and `clipmux listen --since 10m` still replays.
4. **Limits.** The 6th bad setup token, the 11th wrong pairing code from one
   address and the 61st `POST /v1/uploads` in a burst on one key get 429
   `rate_limited` with `Retry-After`, while a second key still passes. 40
   viewers behind one address play a signed video with no 429. A spoofed
   `X-Forwarded-For` from an untrusted peer is ignored.
5. **Soak.** 60 s on pull requests, 30 minutes nightly (`CLIPMUX_E2E_SOAK=1`):
   50 HLS clients on 3 assets plus uploads and listing. No 5xx, a route-cache
   hit ratio of at least 0.95, and goroutines and heap back to baseline after.
6. **Observability.** `/metrics` on `CLIPMUX_METRICS_ADDR` passes `promtool
   check metrics`, shows the upload's committed attempt, and is 404 on the
   public listener.
7. **Backup and upgrade.** A `pg_dump` of the schema and a copy of the data
   directory, restored with the same secrets key, plays the asset and signs a
   webhook that verifies. A binary older than the schema refuses to boot with
   `schema_newer`.
8. **Rotation.** Secrets key v1 → v2 with `CLIPMUX_SECRETS_KEY_PREVIOUS` and
   `admin reseal-secrets`, then boot on v2 alone. Content key v2 against the
   fake Cloudflare: the Worker's ring is redeployed before any output uses v2,
   and old outputs play. An old signing key plays until its `retired_at`, then
   gets 403.
9. **Browser.** With the CSP enforced, the Playwright suites raise no
   `securitypolicyviolation`; TOTP enrolment and sign-in work, and a recovery
   code works exactly once.

**Subtitles, transcripts and chapters**

10. **Video never waits for captions.** `video.asset.ready` arrives before
    `video.asset.track.ready`. With whisper frozen (SIGSTOP), a second upload
    still becomes ready.
11. **Failures stay contained.** A failed transcription leaves the asset
    ready, the subtitles track `errored` (`transcribe_failed`) and the
    chapters track `subtitles_failed`. A silent video gets `no_audio` with no
    job.
12. **Languages are detected.** An English and a Hindi sample (recorded by the
    owner) come back `en` and `hi`; a Hinglish one is `hi` or `en` with
    non-empty text.
13. **Subtitles survive a re-encode.** The same track is listed under the new
    output with unchanged cue times, and a transcription running across the
    re-encode's commit still completes.
14. **Players show the track**, with a cue active at its known time ±0.5 s:
    hls.js in Chromium, native HLS in Safari (`exit-safari.yml`) and
    ExoPlayer (`exit.yml`).
15. **Tokens and storage.** A `t` token gets 403 on `tracks/`. A token
    refreshed mid-play still loads the VTT. The stored VTT is ciphertext and
    the edge serves plaintext. A revoked ID's track is 404 at the edge within
    65 s.
16. **Delete and move.** Delete leaves no track row, transcript or object. An
    asset with tracks moves to R2 and plays its subtitles from there.
17. **Routing.** With only PCs without speech online and RunPod as the
    fallback, a transcription goes to RunPod with `primary_offline`. A failed
    transcription doesn't bar the asset's re-encode from the fallback, nor the
    reverse.
18. **Chapters.** Both adapters run against fakes in CI. `exit-llm.yml` (main,
    weekly, by hand; skipped without its secrets) generates chapters through a
    real Claude account and a real OpenAI-compatible endpoint.
19. **LMS parity.** A contract test mirrors the shapes in the LMS's own fake
    (`LMS/api/src/test/harness.ts`): track states map to pending, completed
    and failed; the transcript can be fetched the moment its track reads
    ready; the sidecar VTT loads; chapters convert to seconds; every stage
    change sends a webhook; delete leaves nothing.
20. **Online speech.** Against a fake OpenAI-compatible speech server: a
    project in `api` mode with no worker that can transcribe gets subtitles
    routed `speech_api`; `local_then_api` sends a transcription there with
    `primary_offline` while only workers without speech are online and keeps
    it local once one with speech is; a SIGTERM mid-request hands the attempt
    back `worker_stopped`; the API key appears in no row, response or log.
    `exit-llm.yml` transcribes the English sample through a real speech API.

## Design notes

Each task is planned against these points and settled in its own decision
record. Two independent design reviews checked them against the code before
this plan was written.

### Logs

- **`internal/logging`** builds the logger from `CLIPMUX_LOG_FORMAT` (`text`,
  the default, or `json`) and `CLIPMUX_LOG_LEVEL`. A handler adds attributes
  from the context: `request_id`, `job`, `attempt`, `kind`. serve passes both
  variables to the built-in worker.
- **Redaction moves into one place.** `worker.redact()`
  (`internal/worker/worker.go:625-649`) becomes `logging.RedactURL` and
  `RedactError`, on its second real use: import failures, which today log the
  full `*url.Error` with its query (`internal/lifecycle/imports.go:162`). A
  `ReplaceAttr` backstop scrubs `cmx_`, `cmxa_`, `cmxd_`, `whsec_` and query
  strings from any attribute.
- **Access log.** One line per request: the route pattern, the path rebuilt
  from the pattern with `{token}` and `{code}` redacted, status, bytes,
  duration, client address and request ID. Never the query, `Referer` or
  `Authorization`. Successful delivery logs at debug.
- **Request IDs.** `X-Request-Id` is believed only from
  `CLIPMUX_TRUSTED_PROXIES`, otherwise a fresh `req_…`, and is always echoed.
- `http.Server.ErrorLog` goes through slog. Handlers log `r.Pattern`, never
  the path, which can carry a live pairing code (`internal/manage/manage.go:254,277`,
  `internal/agents/http.go:46`).

### HTTP robustness and shutdown

- **Server:** `ReadHeaderTimeout` 10 s (as today), `IdleTimeout` 120 s,
  `MaxHeaderBytes` 64 KiB, and no global read or write timeout, which would
  cut long-polls and streams.
- **Deadlines per route class**, through `http.ResponseController`: JSON API
  read 30 s and write 60 s; long-polls (claim, events) the wait plus 10 s;
  streams (`/_storage/`, delivery objects) a rolling 60 s idle deadline, like
  the worker's idle watchdog (D016); Connect, the setup test, asset delete and
  worker `complete` 5 minutes. Every wrapping `ResponseWriter` implements
  `Unwrap`, or `ResponseController` and `ServeContent` break.
- **Panics.** A recovery middleware answers 500 if nothing was written, and
  re-panics on `http.ErrAbortHandler`. `sweeper.Every`
  (`internal/sweeper/sweeper.go:15-32`) recovers and gives each loop a pass
  deadline, so a panic in one sweeper no longer kills serve.
- **Drain, in order:** set the draining flag (`/readyz` 503, long-polls answer
  empty, claims hand out nothing); SIGINT the built-in child, which reports
  `worker_stopped` (counted against the lost-lease budget, and a capacity code
  for the fallback bar); `srv.Shutdown` with 25 s and a `BaseContext` no
  longer cancelled by the signal; cancel the loops and wait at most 10 s;
  close the pool. Compose sets `stop_grace_period: 40s`.
- **The sandbox's CPU limit.** `RLIMIT_CPU` (`internal/sandbox/sandbox.go:56`,
  6 h) counts every thread, so a multi-threaded encode or transcription is
  killed long before 6 hours of wall time. The limit scales with the threads
  the tool is given.
- **Headers.** `nosniff` everywhere. The dashboard gets a CSP built per
  request from the media URL and the storage endpoints (`script-src 'self';
  object-src 'none'; base-uri 'none'; frame-ancestors 'none'; worker-src
  blob:` …), `Referrer-Policy: no-referrer`, COOP and Permissions-Policy. Once
  the public URL is https, HSTS and a 308 to https for the public and media
  hostnames (`X-Forwarded-Proto` believed only from trusted proxies).

### Metrics and health

- **Hand-rolled Prometheus text format** in `internal/metrics`: counter, gauge
  and histogram vectors and scrape-time collectors, about 300 lines. The
  format is small and stable, and `client_golang` would bring `common`,
  `procfs` and a global registry for a gap the standard library covers.
- **Its own listener**, `CLIPMUX_METRICS_ADDR`, off by default. Caddy proxies
  only the app port, so it needs no auth. pprof is on the same listener only
  with `CLIPMUX_PPROF=true`.
- **Families:** HTTP requests and durations by pattern (long-polls excluded),
  in-flight requests, delivery bytes, route-cache results; jobs by kind,
  status, `routed_to` and `route_reason`, the oldest queued job by kind,
  finished attempts by executor, result and error code; outbox pending and
  oldest pending by kind; webhook delivery results and open endpoint
  breakers; dispatches, wakes and month spend by provider; the DB pool;
  leader; sweeper pass durations, errors, panics and last success; rate-limit
  rejections; retention deletes; agents online and the built-in worker;
  build info and the runtime.
- Labels come from database values, and an unknown error code becomes
  `other`, so new job kinds and tracks add series with no code change. Gauges
  read from SQL are cached for 10 s.
- `/healthz` stays liveness only. `/readyz` answers 503 while the database
  ping fails, while draining or before boot finishes, naming the check.

### Rate limits

- **`internal/ratelimit`:** GCRA token buckets, failure windows (the sign-in
  limiter, generalised) and `ClientIP`, moved from `accounts`
  (`internal/accounts/accounts.go:258-288`): the rightmost untrusted
  `X-Forwarded-For` hop when the peer is a trusted proxy, IPv6 grouped by /64.
  In memory, evicted lazily, at most 100,000 keys.
- **Applied where the caller is already known**, not through a route table:

  | Surface | Keyed by | Limit |
  | --- | --- | --- |
  | `POST /v1/session` | address, global | 5 / 20 failures per 15 min (unchanged) |
  | `POST /v1/setup/owner` | address, global | 5 / 20 failures per 15 min |
  | `POST /worker/v1/pair` | address, global | 10 / 100 failures per 15 min |
  | `POST /worker/v1/pairings`, `…/poll` | address | 10 per hour; 2 per second |
  | Integration API (`apikeys.Authenticator.Require`) | API key or owner session | 20 rps, burst 200; writes 2 rps, burst 60 |
  | `/v1/uploads/{id}/*` | upload token | 10 rps, burst 50 |
  | `GET /v1/events` | API key | 4 concurrent long-polls |
  | `/v/`, `/p/` | address | refusals only (401, 403, 404): 300 per 5 min |
  | `/_storage/`, `/worker/v1/attempts/*`, claims | — | exempt: signed or scoped to an attempt |

- **Delivery counts refusals, never successes.** A classroom behind one NAT
  looks like abuse if requests are counted; a scanner shows up as refusals.
- **429:** `httpjson.Problem` with code `rate_limited`, detail `retry_after`
  and a `Retry-After` header, which the uploader (D031) and `push` (D044)
  already honour. `too_many_attempts` and `too_many_pairings` keep their
  codes. No `RateLimit-*` headers until that draft is an RFC.

### Database

- `db.Open` uses `pgxpool.ParseConfig` and adds `statement_timeout=60s` and
  `idle_in_transaction_session_timeout=5min` unless the URL sets them. No
  global `lock_timeout`: route sync holds outbox locks across storage calls.
- Boot retries the ping from 1 s to 10 s for 60 s, never logging the URL.
- **Migrations take goose's Postgres session lock**, keyed per schema like
  D035's leader. Today serve and `clipmux admin` (`cmd/clipmux/admin.go:90`)
  can migrate at the same moment. Boot refuses a schema newer than the binary
  (`schema_newer`). `clipmux admin migrate status|up|down-to N --yes`.

### Retention

The windows are constants, because each follows from another rule:

| What | When it goes |
| --- | --- |
| Retired outputs | the larger of 7 days and the project's maximum token lifetime, plus 1 h |
| Files of a failed, lost, released or superseded attempt | 30 min after a newer attempt replaces it or the job ends (PUT URLs last 15 min) |
| Orphan `media/{asset}/out_*` prefixes | daily, objects older than 24 h |
| Filesystem `tmp/` and multipart staging | 24 h |
| Retired tracks | with the output window |
| `idempotency_keys` | 24 h |
| Sessions | at expiry |
| Pairings | 24 h after expiry |
| `webhook_deliveries` and webhook events | 30 days after they finish |
| Other outbox kinds | 7 days after they're done |
| Setup tests | the last 20 are kept |
| Jobs, attempts, dispatches | kept: they are history and spend |

- A purge sets `outputs.purged_at` and queues a `refresh` route_sync, so the
  record's `outputs` map drops the output. Delivery treats a purged output as
  absent.
- **`attempts.output_id` becomes nullable.** `upload_source` attempts stop
  creating an empty `writing` output, and track B's transcription attempts
  write none.
- **No dead letter for `route_sync`, `keys_sync` or `stop`.** Giving up would
  strand a revocation, a ready asset or a paid machine. They keep their capped
  backoff; after an hour pending they count as stuck in a metric, one error
  log and `clipmux admin outbox`. `wake` already ends its dispatch (D055), and
  webhook deliveries end `failed` at 24 h (D018).
- **Webhooks:** a per-endpoint breaker opens after 5 failures in a row for 1
  minute, doubling to 1 hour; an open endpoint is neither tried nor charged a
  try, and `give_up_at` is unchanged. Secret rotation is two-step (a pending
  `whsec_`, then activate), keeping D018's single signature. Owners list
  deliveries and redeliver one (a fresh 24 h).

### Keys and the owner account

- **Secrets key ring.** The sealed format already carries a key version
  (D017). `CLIPMUX_SECRETS_KEY_PREVIOUS` opens old values, `clipmux admin
  reseal-secrets` reseals every row, and boot refuses a version the ring
  lacks. A test lists every sealed column from `information_schema`, so a new
  table can't be forgotten.
- **Content key secret.** `POST /v1/content-keys/rotate` adds a version and,
  on Worker delivery, redeploys `CONTENT_KEYS` through Connect before any
  output uses it (D037). Track keys derive from the same ring.
- **Signing keys.** `clipmux admin rotate-signing-key --app` sets the old
  key's `retired_at` to now plus the maximum token lifetime, so live sessions
  end naturally. `admin list-keys` and `admin revoke-key`.
- **TOTP on the standard library** (RFC 6238, HMAC-SHA1, 6 digits, 30 s, ±1
  step), the secret sealed under `owners/{id}`, the last accepted step stored
  against replay, 10 single-use recovery codes stored as SHA-256. Enrolling
  needs the password and a code, and ends other sessions. Sign-in without a
  code answers 401 `totp_required`; a wrong code counts toward the existing
  limit. `clipmux admin reset-totp` for a locked-out owner.
- The setup token expires after 24 h and sits behind the setup limiter.

### Delivery performance

- **A route cache** in `internal/delivery` holds, per playback ID, the
  projection the Worker already reads (`routes.Record`: current output,
  `outputs`, policy, denied, and now tracks) plus signing keys by `kid`.
  Hits live 30 s and unknown IDs 5 s, against the Worker's 60 s and 5 s.
- **Invalidation after commit,** from `outputs.Committer.SyncRoutes` through
  `OnPublished(pids)` and `OnKeys()` hooks. That is complete by construction:
  every change the Worker must see already writes a `route_sync` or
  `keys_sync` row. A generation counter refuses to store a load that started
  before an invalidation.
- `assets.LoadMany` loads every listed asset's playback IDs in one query.

### Tracks

- **A `tracks` table, per asset, not per output,** so subtitles survive a
  re-encode: the timeline is the source's, and every output starts at zero.
  - `id trk_…`, `kind` (`subtitles`, `chapters`), `source` (`generated`;
    `uploaded` reserved), `status`, `language`, `name`, `error_code`,
    `key_version`, `duration_ms`, `chapters jsonb`, timestamps.
  - At most one ready and one preparing generated track per asset and kind.
  - A separate `transcripts` table holds the canonical transcript, keyed by
    track, so loading an asset stays light.
- **States:** `preparing` → `publishing` (stored and its VTT written) →
  `ready` (its route_sync rows confirmed, or at once without playback IDs, as
  in `outputs.commit`) → `retired` (a newer track of the kind became ready;
  servable for the retire window, so a session holding the old master still
  loads it). `preparing` → `errored`, any → `deleted`. The API shows
  `publishing` as `preparing` and hides retired tracks. `version` rises on
  every change the view shows (D020).
- **Tracks are immutable like outputs.** Regenerating creates a new track and
  retires the old one only when the new one is ready, so a stale job can't
  overwrite and viewers never see a gap.
- **Interactions:**

  | Event | Effect |
  | --- | --- |
  | Upload, import or attach with `generate` | Track rows inserted `preparing` in the create transaction |
  | An output commits | A `transcribe` job for each preparing subtitles track with no active job, or `no_audio` when the manifest has no audio |
  | Re-encode | No effect on tracks; a running transcription keeps reading its input output |
  | Asset errored before any output | Tracks stay preparing; a later successful re-encode picks them up |
  | Delete | Cancels both job families, marks tracks deleted and deletes transcripts; the delete job removes the objects |
  | Move | Exclusive with track jobs; copies ready, publishing and retired track objects |
  | Revoked ID or disabled app | Track paths follow the route: 404 or 403 |
  | Source deleted after ready | No effect: transcription reads the output |
  | `push --local` | The transcription isn't pinned |
  | Agent revoked mid-transcription | Requeued with `agent_revoked`, a capacity code |

- **Error codes:** `no_audio`, `transcribe_failed` (budget spent),
  `transcript_invalid`, `transcribe_unavailable` (no worker claimed it within
  24 h, matching the LMS's own limit), and for chapters `subtitles_failed`,
  `llm_not_connected`, `model_rejected`, `model_refused`,
  `invalid_model_output`, `transcript_too_large`; through a speech API
  `speech_api_unavailable` (a capacity code) and `speech_api_rejected`.

### Two job families

- `jobs_one_active_per_asset` (`internal/db/migrations/00001_core_model.sql:180`)
  becomes two partial unique indexes: the **output family** (import,
  transcode, reencode, upload_source, move, delete) and the **track family**
  (transcribe, chapters, move, delete). Move and delete are in both, so they
  stay exclusive with everything, and two writers of the same thing never
  overlap. A transcription waiting for a worker with speech no longer blocks
  re-encodes. This refines, and doesn't weaken, "one mutating job per asset".
- `GetActiveJobForAsset` and `lockForChange`
  (`internal/lifecycle/lifecycle.go:193-225`) become family-aware, and delete
  cancels both families.
- **The fallback bar is scoped by family** (D053): one failed transcription
  doesn't bar the asset's re-encode from the fallback, nor the reverse.
  Transcription capacity codes are `source_download_failed` (also for the
  audio stream), `scratch_full` and `model_unavailable`; `transcribe_failed`,
  `transcript_invalid` and `job_timeout` bar it.

### Transcription on workers

- **The input is the committed output's audio rendition, not the source.**
  serve streams it decrypted at `GET /worker/v1/attempts/{id}/audio`, joining
  `init.mp4` and the segments; the CBC decrypt in
  `internal/setuptest/setuptest.go:620` moves to `media` on its second use.
  That works after the source was deleted, never pins a job to an offline PC,
  moves about 1 MB a minute, and needs no content key on the worker.
- **Alignment is by construction.** The encode resets both streams to zero
  (`asetpts=PTS-STARTPTS`, `internal/media/pipeline.go:25`, and
  `setpts=PTS-STARTPTS`, `internal/media/encoders.go:169`), so clipmux-lms's
  audio-offset trim must not be ported: it would shift captions away from
  what viewers hear.
- **Rejected: whisper inside the transcode attempt** (clipmux-lms D024).
  Regeneration would still need its own job, a whisper failure would mix
  into the encode's budgets and fallback bar, workers without speech couldn't
  take captioned transcodes, and outputs are immutable. The cost is one more
  wake per captioned video on a serverless primary (open question 9).
- **A speech slot.** Claims are FIFO across kinds, so the worker runs a second
  claim loop with `kinds: ["transcribe"]` beside its media loop, and on CPU
  whisper gets half the threads. A long transcription never delays the next
  upload's encode.
- **Claims.** The worker probes a real transcription at start, as D046 probes
  encoders, and reports `speech: {backends, model}`. `job_claimable_by` takes
  the kind and the worker's speech, with a disk rule per kind (about 1 GiB
  plus the audio). Verified providers count as capable: the image ships
  whisper and their setup test proves it. The transcription claim carries
  `speech: {input_output_id, duration_seconds, model}` and no source, output
  or content key. `contract_version` stays 1: old workers never report
  speech.
- **The pipeline:** ffmpeg makes 16 kHz mono PCM, then `whisper-cli -l auto
  -ojf --vad` runs under `clipmux sandbox-exec`, with the GPU rules of D046
  and D051. The model (large-v3-turbo GGML, quantization from T11) and the
  Silero VAD model are fetched by the worker, never by the sandboxed tool,
  into `{data_dir}/models`, verified against digests in
  `toolchain/versions.env`. Weights are not baked into the image, which
  would slow every serverless cold start.
- **The transcript comes back in `complete`.** No new file type is written by
  a worker, so the path grammar and the commit checks are unchanged.

### Speech API (D075)

- **Three modes per project,** `projects.speech_mode`: `local` (the default,
  whisper.cpp as above), `api` (every transcription to the connected API) and
  `local_then_api` (the primary first, the API as the transcriptions'
  fallback). `PUT /v1/settings/speech {mode}` refuses the last two with 409
  `speech_not_connected` without a verified connection; disconnecting resets
  the mode to `local` and the router moves queued jobs back.
- **Routing.** A class `speech_api`, valid only for `transcribe`.
  `decideRoute` is unchanged; `Route` feeds it, for transcriptions:

  | mode | Primary | Fallback |
  | --- | --- | --- |
  | `local` | the project's primary | the project's fallback |
  | `api` | `speech_api` | none |
  | `local_then_api` | the project's primary | `speech_api` (queue age applies) |

  `speech_api` is up when a verified connection exists and always has room.
  The family-scoped fallback bar applies. `attempts.executor` gains
  `speech_api`; `projects.fallback_executor` stays a machine.
- **The runner lives in serve** (`internal/speechapi`): a leader loop claims
  `speech_api` jobs through `queue.Claim`, `Heartbeat`, `Fail` and
  `CommitAttempt`, with no HTTP hop, so the key never reaches a worker. At
  the drain it hands its attempt back `worker_stopped`.
- **Audio** comes from the reader behind `/worker/v1/attempts/{id}/audio`:
  the input output's audio rendition decrypted, grouped as `init.mp4` plus
  whole segments into chunks of at most 24 MiB (about 25 minutes at 128
  kb/s), each starting at the playlist's summed `EXTINF`. No ffmpeg in serve.
- **Request:** multipart `POST {base}/audio/transcriptions` with `model`,
  `response_format=verbose_json`, `timestamp_granularities[]` `segment` and
  `word`, `temperature=0` and `file=chunk.m4a`; later chunks carry the first
  chunk's detected `language` and the previous chunk's last ~200 characters
  as `prompt`. 408, 409, 429, 5xx and network errors retry 4 times (2/4/8 s or
  `Retry-After` capped at 60 s), then `speech_api_unavailable`, a capacity
  code; other 4xx `speech_api_rejected`; an unreadable body
  `transcript_invalid`.
- **To canonical:** `transcripts.FromOpenAI` clamps each chunk's times to its
  span and shifts them by its start, maps Whisper's language names to ISO
  639-1 (else `und`) and records the model as `{model}@{host}`. Then the same
  `tracks.CommitTranscript` as a worker's `complete`.
- **Connect** (`GET`, `POST /v1/settings/speech/connect`, `DELETE`, owner
  session): steps `credentials` (`GET {base}/models`), `transcription` (the
  speech sample in the exact chunk format, so a provider that refuses
  fragmented MP4 fails here) and `saved`. The key is optional (a keyless
  Speaches box) and sealed in `ai_connections`; the base URL isn't filtered,
  as for webhooks (D018). The docs and the card say audio leaves the owner's
  infrastructure.

### Transcripts and WebVTT (serve)

- **The canonical transcript, v1:** integer milliseconds rounded as
  floor(x·1000+0.5), segments `s<n>` and words `s<n>w<m>`, the detected
  language as ISO 639-1, the model as `name@sha256`, and the output it was
  made from. Control characters and NUL are stripped.
- **Validation** under the job's liveness lock: UTF-8, times monotonic and
  within the duration, size caps. A bad transcript is 409
  `transcript_invalid`.
- **WebVTT is rendered by serve**, with clipmux-lms's cue rules: a new cue
  when the text would pass 84 characters, the span 4000 ms or a gap 800 ms;
  single line, numbered, HTML-escaped, overlaps fixed, clipped to the
  duration; one cue per segment without words; an empty transcript is exactly
  `WEBVTT\n\n`. The header carries `X-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000`,
  which browsers ignore and HLS uses.
- **Encrypted at rest**, keeping "storage holds only ciphertext": AES-128-GCM
  with `HMAC-SHA256(secret_v, "clipmux-track-key-v1\n" + asset_id + "\n" +
  track_id)[:16]`, stored as `0x01 | key_version u32 | 12-byte nonce |
  ciphertext+tag` with the storage key as associated data, so a tampered
  object can't inject cues. Vectors computed independently in Python, as in
  D012.
- **Written under the job lock** to `media/{asset_id}/tracks/{track_id}.vtt`,
  in `complete`'s transaction, as D054 makes outside calls under a row lock.
  The prefix is under `media/{asset_id}/`, so the delete job already removes
  it; the move job copies track objects.

### Delivery and the Worker

- **Vectors first** (`internal/delivery/testdata/vectors.json`, AGENTS.md):
  the master with and without subtitles (exact bytes), the subtitle playlist,
  VTT bytes and Content-Type, a `t` token refused, unknown, deleted and
  purged tracks, another asset's track, a public ID, chapters never listed,
  HEAD. Then `-update` and `-update-bucket`.
- **The route record** gains `"tracks": {"trk_…": {"kind", "language",
  "name", "duration_ms", "key_version", "listed"}}`, omitted when empty, so
  existing records and their ETags don't change and `v` stays 1. A track
  change is a `route_sync` with `op: "track"`. The Worker's version changes,
  so `GET /v1/cloudflare` reports `needs_redeploy`.
- **The master** gains, after `RewriteMaster`, an
  `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Hindi",LANGUAGE="hi",AUTOSELECT=YES,DEFAULT=NO,URI="tracks/{trk}.m3u8"`
  per listed subtitles track and `SUBTITLES="subs"` on every
  `#EXT-X-STREAM-INF`. Names come from a fixed table for the LMS's languages
  (en, hi, bn, ta, te, mr, gu, kn, ml, pa, ur), else the code.
- **`tracks/{trk}.m3u8`** is generated: one VOD segment for the whole
  duration, `no-store`. **`tracks/{trk}.vtt`** is decrypted and served as
  `text/vtt; charset=utf-8`, private or public, immutable. The same URL is
  the sidecar the LMS player loads. Chapters are a `kind=chapters` VTT at the
  same path, never listed in the master.
- `.vtt` is allowed only under `tracks/`; a `t` token gets 403 there. Today a
  `tracks/` path falls into the output lookup and 404s
  (`internal/delivery/delivery.go:157`), so the branch goes before it.
- The Worker (`worker/src/delivery.ts`) caches the ciphertext and decrypts with
  WebCrypto after its cache.

### Integration API

- **Asking for tracks:** `"generate": {"subtitles": true, "chapters": true}`
  on `POST /v1/uploads`, `POST /v1/assets` (import and agent source) and the
  agent's attach. Chapters imply subtitles. Named after the HLS track type:
  whisper output is subtitles, not closed captions. The object leaves room
  for a language hint later.
- **The asset view** gains `tracks: [{id, kind, source, status, language,
  name, error_code}]` (`status` is `preparing`, `ready` or `errored`) and
  `duration_seconds`.
- `GET /v1/assets/{id}/tracks/{trk}`, with `chapters: [{start_ms, end_ms,
  title}]` for a chapters track.
- `GET /v1/assets/{id}/tracks/{trk}/transcript` → `{track_id, language,
  duration_ms, text, segments: [{id, start_ms, end_ms, text}]}`, words with
  `?words=true`, 409 `track_not_ready` before ready.
- `POST /v1/assets/{id}/tracks {kind, supersede?}` regenerates: 409
  `asset_not_ready`, `no_audio`, `chapters_not_connected` or `job_active`.
  `DELETE /v1/assets/{id}/tracks/{trk}` answers 200, or 202 while unpublishing.
- **Webhooks:** `video.asset.track.ready`, `video.asset.track.errored` and
  `video.asset.track.deleted`, with the asset view and `track: {id, kind,
  error_code}` beside `job`.

### Chapters (serve)

- **`internal/chapters`:** the prompt, with the transcript as untrusted data
  between delimiters and the rules in the system prompt; the schema; local
  checks (1–10 chapters, the first segment required, in order, no duplicates,
  titles 1–100 characters on one line); timestamps derived locally (the first
  at 0, the last ending at the duration).
- **A consumer-side `Model`** interface, `Complete(ctx, Prompt)
  (json.RawMessage, error)`, with errors carrying `Transient` and `Code`. Two
  implementations justify it:
  - **OpenAI-compatible, on the standard library:** `POST
    {base}/chat/completions` with a strict `json_schema` response format, or
    `json_object` for providers without it. Credentials are checked with `GET
    {base}/models`. Covers OpenAI, Ollama, vLLM, Groq and OpenRouter.
  - **Claude, on `github.com/anthropics/anthropic-sdk-go`**, never through an
    OpenAI-compatible shim: `Messages.New` with the model (default
    `claude-opus-5-5`, editable), a system prompt, and `output_config` with
    effort `low` and a json_schema format. No `thinking`, sampling parameters
    or prefill, which Opus 5.5 refuses. SDK retries are 0 and the request
    timeout 120 s, so the job's budget governs. Errors are classified with
    `errors.As` on `*anthropic.Error`: 429, 5xx and network are transient,
    other 4xx `model_rejected`; `stop_reason == "refusal"` is a permanent
    `model_refused`. Credentials are checked with `Models.Get`.
  - Structured outputs can't express item counts or string lengths, so those
    checks stay local for both.
- **The `chapters` job runs in serve,** claimed by the control loop beside
  move and delete, queued when the subtitles track becomes ready. No segments
  → `[]` without a call. Input over 512 KB is refused, never truncated. One
  corrective re-prompt on invalid output, then `invalid_model_output`.
  Transient errors back off, 3 tries. It renders and writes the chapters VTT
  under the job lock, like subtitles.
- **Connect:** `GET`, `POST /v1/settings/chapters/connect` and `DELETE`
  (owner session), with steps `credentials`, `structured_output` (a real call
  on a fixed three-segment transcript) and `saved`, reusing `dispatch.Step`.
  The key is sealed in `ai_connections` (D017, D075), beside the speech API's. The base URL isn't filtered
  for private addresses (Ollama on a LAN), like webhook endpoints (D018). The
  docs say chapters send transcript text to the connected provider.

### New packages

Following "no package before its code": `logging`, `metrics`, `ratelimit`,
`retention`, `transcripts` and `chapters`. Speech tools join `media`; tracks
live where their writers are (`queue`, `lifecycle`, `delivery`, `routes`)
unless the task finds a `tracks` package earns its place.

## Tasks

Order:

- **Track A first.** HTTP lane: T01 → T02 → T03 → T04. Data lane: T05 → T06 →
  T07 → T09 (T09 also after T03, for its counters). T08 at any time, its
  setup-token limit after T04. T10 once the variables settle.
- **Track B.** T11 and T12 touch no shared code and may start alongside track
  A. T13 after T05 and T07. T14 after T11–T13, T14b after T14. T15 and T16
  after T13, alongside T14, with seeded tracks. T17 after T16. T18 after
  T14b–T17. T19 after T14b and T15.
- T20 last. T00 whenever its secrets and hardware are available.

Each task is a vertical slice with its own tests. Port behaviour and tests
from the sources listed, but don't copy code paths that assume the old data
model. `clipmux-lms` paths are under `~/clipmux-lms`, LMS paths under
`~/LMS`.

| Task | Scope | Port from |
| --- | --- | --- |
| T00 | **Not started: needs its secrets and hardware** — **Carried tracks**, in parallel. Phase 4's `exit-providers.yml` (secrets `RUNPOD_API_KEY`, `MODAL_TOKEN_ID`, `MODAL_TOKEN_SECRET`, `GHCR_PULL_TOKEN`), run again after T02 because the deadlines touch Connect. Phase 2's real Cloudflare zone (D041). Phase 3's by-hand checks: the M4 Mac's lid close and the self-hosted GPU runner. Each exit is recorded when it passes. | D041, D050, D062 |
| T01 | **done** (D064) — **Logs.** `internal/logging`, the context handler, redaction moved from the worker, the `ReplaceAttr` backstop, request IDs, the access log, `ErrorLog`, handlers logging `r.Pattern`, imports logging the host only, `logger.With` in the worker, the log variables passed to the built-in child. Tests: a golden JSON line for `/v/{token}/pid/master.m3u8` with the token redacted; a pairing path redacted; an untrusted `X-Request-Id` replaced; a `*url.Error` with `X-Amz-Signature` scrubbed; `cmx_ABC` scrubbed; an import failure logs only the host. | clipmux-lms `server/src/lib/logger.ts`, `app.ts:174-211`; `internal/worker/worker.go` (`redact`) |
| T02 | **done** (D065) — **HTTP robustness.** Server settings, deadline classes, panic recovery in handlers, sweeper loops and webhook senders, the drain order with `worker_stopped`, the thread-scaled CPU limit, security headers, HSTS and the https redirect. Tests: a panicking handler gets 500 and serving continues; a task that panics on its first pass runs its second; a pass is cancelled at its deadline; a slow GET survives 3 minutes while bytes flow and is cut after 60 s idle; a claim isn't cut at 60 s; `Shutdown` lets an in-flight request finish; `worker_stopped` gives `lost_count` 1 and `failed_count` 0; an 8-thread tool isn't killed at 6 h / 8 of wall time; the CSP names the saved media URL; the redirect applies only to configured hosts. | `internal/server/server.go`, `internal/sweeper/sweeper.go`, `internal/sandbox` |
| T03 | **done** (D066) — **Metrics, health and pprof.** `internal/metrics`, `CLIPMUX_METRICS_ADDR`, `CLIPMUX_PPROF`, a second listener owned by serve, instrumentation, `db/queries/metrics.sql`, the `/readyz` checks, and `GET /v1/system` (leader, last loop successes, stuck outbox) for the dashboard. Tests: golden exposition for literal values (escaping, buckets, order); two queued transcodes give `clipmux_jobs{kind="transcode",status="queued",…} 2`; a backdated outbox row gives its age; an unknown code becomes `other`; the public listener 404s `/metrics`; `/readyz` 503 while draining. | — |
| T04 | **done** (D067) — **Rate limits.** `internal/ratelimit`, with the sign-in limiter and `ClientIP` moved in keeping their literals; applied in `accounts`, `agents`, `apikeys.Authenticator.Require`, the upload-token check, the events cap and delivery refusals. `docs/api.md` gains a Limits section. Tests with a fake clock: burst 3 at 1 rps refuses the 4th with `retry_after` 1 and allows it at +1 s; IPv6 /64 grouping; the trusted-proxy cases ported from `accounts`; the 6th setup failure; the 61st create on key A while key B passes; the 5th concurrent events poll; the 301st refusal, with successes never counted. | `internal/accounts/limiter.go`; clipmux-lms `server/src/app.ts:50-108` (limits, headers) |
| T05 | **done** (D068) — **Database.** Pool config and session timeouts, boot retry, the migration lock per schema, `schema_newer`, `admin migrate`, `CLIPMUX_TOOL_MEMORY_MB` validated. Tests (real Postgres): two `Migrate` calls on one schema both succeed and one applies; two schemas migrate concurrently; a database at N+1 refuses with `schema_newer`; `SHOW statement_timeout` is `60s` and the URL overrides it; `pg_sleep(61)` fails with 57014; the ping retry stops at its bound. | `internal/db/{db,migrate}.go`, D035 |
| T06 | **done** (D069) — **Row retention, outbox health and webhooks.** `internal/retention` (hourly leader loop, batched DELETE rules), `admin prune [--dry-run]`, `admin outbox [--retry ID]`, the endpoint breaker, two-step secret rotation, the deliveries list and redeliver, `dispatches.key_sealed` cleared once a dispatch ends. Tests (real Postgres): each rule deletes exactly what is past its window; an event with a pending delivery stays; a refused endpoint opens after 5 failures and is skipped for a minute while another sends; a probe success closes it; activate changes the signature (a literal HMAC); redelivering a delivered row is 409; a stuck `route_sync` shows in `admin outbox`. | D018, D035; clipmux-lms `server/src/index.ts` (pruning loops) |
| T07 | **done** (D070) — **File retention.** `outputs.purged_at`, the file rules, the daily orphan sweep with a cursor in `retention_cursors`, the filesystem `tmp/` and staging sweep (a filesystem method, not on `Storage`), `attempts.output_id` nullable and `upload_source` without an output. Tests on the filesystem and SeaweedFS stores: outputs current, retired 1 day, retired 8 days, lost 31 minutes and lost 29 minutes leave exactly the 8-day and 31-minute ones purged, and the record's `outputs` lacks the 8-day one; an orphan prefix over 24 h is deleted, a younger one and `tracks/x` are kept; a `tmp` file at 25 h is deleted and one at 1 h kept. | architecture "Storage layout"; `internal/outputs`, `internal/storage` |
| T08 | **done** (D071) — **Keys and the owner account.** The secrets key ring and `admin reseal-secrets`, content key rotation through Connect, `admin rotate-signing-key`, `list-keys`, `revoke-key`, TOTP with a dashboard card, `admin reset-totp`, the setup token's expiry. Tests: every sealed column in `information_schema` is covered by reseal, and booting on v2 before resealing refuses naming the table; RFC 6238 vectors (T=59 → 287082, T=1111111109 → 081804); a replayed step is refused; a recovery code works once; the Worker redeploy is recorded before a v2 claim. | `internal/secrets`, `internal/contentkey`, D017, D022, D037 |
| T09 | **done** (D072) — **Delivery performance.** `internal/delivery/cache.go`, the Committer hooks, `assets.LoadMany`. Tests: all of `vectors.json` passes with a warm cache; a revoke is 404 on the next request without waiting out the TTL; a key revoke is 403; an invalidation during a load caches nothing stale; listing 100 assets makes 2 queries (counted with a pgx tracer). | `internal/routes` (`Record`), D035, D036 |
| T10 | **done** (D073) — **Operator docs and compose.** `docs/configuration.md` with a test that every `CLIPMUX_*` and `DATABASE_URL` literal in the Go source is documented and in `.env.example` (test-only variables in their own section); `docs/operations.md`: install, upgrade, backup and restore (Postgres, the secrets key, the data directory or bucket), monitoring with `deploy/prometheus/alerts.yml` checked by `promtool`, a security checklist, troubleshooting by request ID; compose with JSON logs and rotation (50 MB × 5), `stop_grace_period: 40s`, app and Caddy healthchecks, the metrics port on the internal network only. CI's image job sees the healthchecks turn healthy and a stop exit 0 within the grace period. | `deploy/`, `.env.example`, architecture "Open decisions" (backup docs) |
| T11 | **done** (D077; the Modal L4 run and the owner's recordings pending) — **Speech toolchain and measurement**, the long pole. `toolchain/versions.env` gains whisper.cpp's source digest and the large-v3-turbo GGML and Silero VAD models, pinned to a commit with sha256; `toolchain/build_whisper.sh` (CPU variants, Vulkan); the Dockerfile toolchain stage, `libvulkan1` and `mesa-vulkan-drivers`, `verify_toolchain.sh`, `pack_agent.sh` and the launcher; the sandbox's environment allowlist for Vulkan; `media.Tools.Whisper`, `SpeechWAVArgs`, `WhisperArgs`. Measures the real-time factor on the built-in worker's CPU, the RX 6600 XT (Vulkan) and a Modal L4 (Vulkan with `graphics`, against CUDA), picks the quantization, and checks detection on owner-recorded English, Hindi and Hinglish clips. Tests: a golden transcription under the full sandbox (skipped without the tool, required in CI); CPU whisper still opens no socket; a clip with 3 s of leading silence keeps its timeline under VAD. | clipmux-lms `transcoding/models.py`, `transcoding/video/transcription.py`; D046, D051, D061 |
| T12 | **done** (D076) — **`internal/transcripts`**, pure Go: the canonical v1 type, whisper.cpp JSON to canonical, `Validate`, the WebVTT cue builder. Tests ported from `test_transcript.py`: rounding at minute and hour boundaries (59.9994 → `00:00:59.999`, 59.9996 → `00:01:00.000`, 3599.9996 → `01:00:00.000`), escaping, overlaps, the empty transcript's exact bytes, Devanagari, Bengali, Tamil and Urdu untouched, segments without words. | clipmux-lms `transcoding/video/transcript.py`, `transcoding/timing.py`, `transcoding/tests/test_transcript.py` |
| T13 | **done** (D078) — **Tracks and the job families.** One migration: `tracks`, `transcripts`, the `transcribe` and `chapters` kinds, `jobs.track_id`, the two family indexes, `attempts.input_output_id`, `projects.builtin_speech`, `ai_connections`, `job_claimable_by(kind, speech)`; a Down that cancels and deletes the new kinds before restoring the single index. `kinds` on claims, the family-scoped fallback bar, family-aware `lockForChange` and delete, the `SpeechWait` sweeper, the router including transcriptions. Tests (real Postgres): a transcription and a re-encode active on one asset; a second transcription 409 `job_active`; a move refused while a transcription runs; delete cancels both and fences the transcription; with only PCs without speech online the job goes to the fallback with `primary_offline`; a `transcribe_failed` doesn't bar the re-encode, nor the reverse; `Release` with a null output; 24 h gives `transcribe_unavailable`. | `internal/db/queries/{queue,lifecycle}.sql`, D053, D059 |
| T14 | **done** (D079; the owner's en/hi samples pending) — **Transcription, worker to commit.** `GET /worker/v1/attempts/{id}/audio` with `media.DecryptSegment`; the speech claim loop; `--speech auto\|off\|cpu\|vulkan\|cuda` (`CLIPMUX_SPEECH`); model download by digest; the probe and the claim's `speech`; `complete {transcript}` validated, rendered, encrypted and written under the lock, then `publishing`; a speech check in `clipmux worker doctor`. Tests: ffprobe reads the `/audio` bytes as AAC with the manifest's duration; a stale attempt writes nothing; out-of-order times give 409 `transcript_invalid`; the English clip comes back `en` and the Hindi clip `hi`; with one worker, a second upload's transcode is claimed while a transcription runs. | clipmux-lms `transcoding/pipeline.py`, `transcoding/tests/test_pipeline.py` (video never waits; ASR failure; no audio) |
| T14b | **done** (D080) — **Speech API** (D075). `projects.speech_mode`, the `speech_api` class in the router, `internal/speechapi` (the OpenAI-compatible client, the chunker, the serve runner, Connect), `PUT /v1/settings/speech`, `transcripts.FromOpenAI`. Tests against an httptest speech server that checks the multipart fields and ffprobes each chunk: `api` mode with no worker speech routes `speech_api`; `local_then_api` goes there with `primary_offline` and stays local once a worker has speech; 429 then success; 400 gives `speech_api_rejected`; three chunks shift cue times by their starts and the later ones carry the detected language; the key is in no row, error or log; disconnecting resets the mode and reroutes; a drain hands the attempt back `worker_stopped`. | clipmux-lms commit `a2ca144` (P32, unmerged: request shape, retries) |
| T15 | **done** (D081; Safari and ExoPlayer at T20) — **Delivery and the Cloudflare Worker.** Vectors first; the route record's `tracks`, `finishTrackPublication` from `SyncRoutes`, the master, the subtitle playlist, VTT decryption, `.vtt` only under `tracks/`, `worker/src/delivery.ts`, track-key vectors in `internal/contentkey`. Tests: `TestVectors`, `vectors.test.ts`, `workerd.test.ts`, the reconciler rewriting a record that drifted. | D015, D035, D036; clipmux-lms `delivery/src/index.ts` (VTT serving) |
| T16 | **done** (D082) — **Integration API and webhooks.** `generate` on create, import and attach; `tracks` and `duration_seconds` on the view; the track, transcript, regenerate and delete routes; the three webhook events; `docs/api.md`. Tests: the view's JSON as literals; chapters imply subtitles; a regeneration keeps the old track until the new one is ready; idempotency across the new POSTs. | clipmux-lms `server/src/routes/api.ts:480-579`, `server/src/utils/media-actions.ts`, `media-actions.spec.ts` |
| T17 | **done** (D083) — **Chapters.** `internal/chapters`, both adapters, the control job, Connect, the `subtitles_failed` cascade. Tests ported from `chapter.spec.ts` against httptest fakes for both adapters (the Claude fake through `option.WithBaseURL`): segment IDs to times, grounding refusals, multilingual titles, silence without a call, transient against permanent codes, one corrective retry then a permanent failure, the untrusted-content rules in the prompt; the key absent from every row and error. | clipmux-lms `server/src/utils/chapters.ts`, `chapter-queue.ts`, `chapter.spec.ts`, `docs/phases/02-server-chapters.md` |
| T18 | **done** (D084) — **Player and dashboard.** `@clipmux/player`: the subtitles menu from hls.js and chapter markers from the chapters VTT, with subtitle requests through the refresh loader (D029). Dashboard: track status on the asset page, a transcript view, regenerate and delete, upload toggles, a Connect LLM card, speech backends on the Workers page. Playwright covers each. | clipmux-lms `player/src/ClipMuxPlayer.tsx:162-171,753-768`, `web/hooks/use-enrichment-updates.ts` |
| T19 | **done** (D084; provider speech proven only by `exit-providers.yml`, T00) — **Setup test and providers.** A sample with speech, recorded by the owner (today's `sample.mp4` is a 440 Hz tone, `toolchain/make_sample.sh`); checks `subtitles_ready`, `play_subtitles` (a cue with the expected word within ±0.5 s), `thumbnail_token_refused_track` and `stored_track_encrypted`, skipped when no worker has speech and required in a provider's setup test; real Modal and RunPod measurements in `exit-providers.yml`. | D026, D054 |
| T20 | **done in CI** (D085; real accounts, Safari, ExoPlayer and the drills it lists pending) — **Exit test**, as listed above. It extends `internal/e2e` (D033, D050, D062), adds `exit-llm.yml` (chapters and a speech API) and the LMS parity contract test, records the exit decision and updates the known gaps (HA, OpenTelemetry, per-process limiters and cache, history growth, unfiltered owner webhook URLs, word timing coarser than whisperX, the source A/V offset the encode drops) and the threats table (transcript text to the LLM provider, track keys). | D033, D050, D062; `LMS/api/src/test/harness.ts` |

## Open questions

Each answer becomes a decision record before the task that needs it.

1. **Default log format?** (T01) Recommendation: text, with compose setting
   JSON.
2. **CSP enforced or report-only first?** (T02) Recommendation: enforced,
   since the Playwright suites catch breakage. TOTP shows its secret as text,
   so the dashboard needs no QR dependency.
3. **The setup token in the log?** (T08) Recommendation: keep printing it,
   with its 24 h expiry and an exemption in the log check, rather than a file
   under the data directory.
4. **Audit log?** (T08) Recommendation: a known gap this phase.
5. **Is the per-key write limit (2 rps, burst 60) enough for the LMS's bulk
   import?** (T04) Needs the import's peak rate; a per-key override can come
   later.
6. **Whisper on NVIDIA: Vulkan or CUDA, and which quantization?** (T11)
   Decided by its measurements. CUDA adds hundreds of MB to the image; Vulkan
   needs `graphics` in `NVIDIA_DRIVER_CAPABILITIES` and is unproven under
   Modal's gVisor. The L4 measurement needs T00's Modal secrets; an owner
   without a GPU can use a speech API meanwhile (D075).
7. **Speech on the built-in worker by default?** (T14) Recommendation: `auto`
   turns it on only with at least about 4 cores and 6 GiB, settled by T11's
   CPU measurement.
8. **Do Safari and ExoPlayer accept a single-segment WebVTT playlist?** (T15)
   The fallback is 6 s segments sliced at delivery.
9. **Two wakes per captioned video on a serverless primary?** (T14)
   Recommendation: accept for now; a `--once` machine could later claim its
   asset's follow-up transcription. `local_then_api` or `api` avoids the
   second wake (D075).
10. **The LMS library at cutover:** regenerate with whisper, or import its
    existing VTTs as `uploaded` tracks in a follow-up? (after the phase)
11. **Refusals from Claude?** (T17) Recommendation: `model_refused`, with no
    server-side fallback model, so the owner's choice of model is the one
    that runs.
