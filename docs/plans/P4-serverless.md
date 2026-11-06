# Phase 4 — Serverless

Source: [architecture.md](../architecture.md), "Build order", "Compute"
("Routing and fallback"), "Adapters", "Compatibility" and "Optional upgrades",
with [decisions.md](../decisions.md) where they differ (D052 sets this phase's
scope). Phase 4 lets a cloud GPU encode for the instance, as the primary
executor or as a fallback when the primary is away:

- **Routing:** a primary executor and an optional fallback, as project
  settings, following the architecture's routing table.
- **The dispatcher:** wakes a provider from the outbox with a one-time key that
  claims a single job. It also stops provider machines and reconciles them.
- **Fallback limits:** a concurrency limit and a monthly spend limit.
- **Adapters:** RunPod (on-demand pods through its REST API) and Modal
  (Sandboxes through its Go SDK), each with a Connect card.
- **The worker image** in a private registry, pinned to the server's build.
- **Carried from Phase 3:** `push --local --upload-source`, the self-contained
  agent tarball, and the Playwright suite for the Workers and Jobs pages.

The integration API, the output format, tokens and delivery stay the same. A
serverless worker is `clipmux worker --once`, which pulls through the same
worker API as every other executor, with no separate path.

Phase 3 exited on the real binary (D050), and NVIDIA was proven on Modal
(D051). Its by-hand checks and Phase 2's real-account run carry into T00, a
parallel track (D052).

**Exit test:** fallback follows the routing table, and a corrupt source never
hits fallback.

Concretely, automated in CI (T10) on the real binary unless marked otherwise.
The harness is `internal/e2e` (D033, D050) with a fake RunPod API, an httptest
server like `fakeCloudflare`. It runs each pod's start command and environment
as a local `clipmux worker --once`, passing on PATH and the media tool
variables. Stopping the pod kills that process. So serve's real RunPod adapter,
dispatcher, keys and reaper run unchanged. Connect's refusal of a private
public URL has a development override, as `create-key --allow-loopback` does.

1. **The primary comes first.** The primary is PCs and the fallback is RunPod.
   With an agent online, a job runs on it with `route_reason` `primary`, and the
   fake sees no pod.
2. **A primary that goes offline sends work to the fallback.**
   - The agent's process group is SIGSTOPped. The test enqueues only after the
     API shows the agent offline (60 s), so no long-poll already waiting on the
     server can hand the job to the frozen agent.
   - The job routes to RunPod with `primary_offline`.
   - The one-time key claims, the output commits and plays, and the pod is
     stopped once the attempt ends.
   - A second claim with that key gets 401.
3. **A lost lease while the primary is offline.** The agent is stopped
   mid-encode. After the lease is lost, the retry wakes the fallback on a new
   attempt and output. Once continued, the agent is told to stop.
4. **Queue age.** The primary is online but busy. With queue-age fallback set
   to 1 minute through the owner API, which is a product setting rather than a
   test knob, a second job wakes the fallback with `queue_age`. With the
   setting off, it waits.
5. **Limits.**
   - With concurrency 1, a second fallback job waits with `fallback_limit`.
   - Once the month's spend plus a wake's reservation would pass the limit,
     jobs wait for the primary.
6. **A corrupt source never hits fallback.**
   - A source the probe rejects errors the asset with `unsupported_source`.
   - A fixture that passes the probe but reliably fails the CPU encode is built
     and proven in T01. It retries on the primary only. It still doesn't reach
     the fallback when the primary then goes offline, nor on a later re-encode
     of the errored asset.
   - The fake records no pod at all.
7. **Pinned jobs wait.** A `push --local` job whose agent is offline never
   wakes the fallback.
8. **No orphans and no doubles.**
   - Serve restarted after a wake and before the claim: the same pod claims,
     because the worker retries until 401 or its deadline.
   - A crash around `Wake`: exactly one pod exists.
   - A pod whose dispatch has ended: the reconciler's pass at boot stops it,
     matching on the provider's instance ID.
9. **Real accounts** (`exit-providers.yml`: on main, weekly and by hand,
   skipped without its secrets):
   - Connect RunPod and Connect Modal each run the setup test through that
     provider, against serve exposed by a tunnel.
   - The manifest records `nvenc`.
   - Afterwards, each provider's list API shows no ClipMux machine.

## Design notes

Each task is planned against these points. Each is settled in that task's
decision record. An independent review checked them against the code before
the plan was written.

### Routing

- **Settings** live on the project:
  - `primary_executor` is one of:
    - `workers`, the default: the built-in worker and PCs compete, as today,
      so an upgrade changes nothing;
    - `agents`: PCs only, and the built-in worker idles;
    - `builtin`, `modal` or `runpod`.
  - `fallback_executor` is null, `builtin`, `modal` or `runpod`, never the same
    as the primary.
  - `queue_age_fallback_minutes` is null (off, the default) or a number of
    minutes, with 10 suggested.

  A fallback of `builtin` means "my PC, else the server" with no cloud. That
  answers Phase 3's open question 2, and `CLIPMUX_BUILTIN_WORKER` stops being
  the only switch.
- **`routed_to` names the target while a job is queued.**
  - Today `StartJob` stamps it with the executor and `'claimed'`
    (`internal/queue/queue.go:241`), and nothing reads it for routing.
  - `CreateJob` instead stamps the primary, with reason `primary`, on
    `transcode` and `reencode` jobs only. That covers all four media enqueue
    paths (upload, import, local source, re-encode), and `delete` and `move`
    jobs are untouched.
  - `StartJob` stops overwriting it. `attempts.executor` and a new
    `attempts.route_reason` record who ran each attempt and why.
  - The migration backfills queued and running media jobs to `workers` with
    `primary`, and pinned jobs to `agents` with `pinned`.
  - The claim gains one condition: the worker's class matches `routed_to`.
    Pinned jobs are exempt, because the pinned predicate already decides them.
- **The router** is a leader loop (D035) that runs every 5 s. It locks one
  queued, unpinned media job at a time, only once `available_at` has passed, so
  a retry's backoff doesn't waste a wake. It writes only when the route
  changes. The rules apply in order:
  1. A pinned job keeps `pinned`.
  2. If the primary is available, the job stays on it with `primary`. There is
     one exception: if queue age is on, `now() - available_at` exceeds it, and
     the fallback is available, eligible and under its limits, the job goes to
     the fallback with `queue_age`.
  3. Otherwise, if the fallback is available, eligible and under its limits,
     the job goes to it with `primary_offline`.
  4. Otherwise it stays on the primary, with `no_fallback`, `fallback_limit` or
     `fallback_refused` saying why.

  A job woken on a fallback stays with that dispatch until it claims or its
  deadline passes. A job routed to the fallback but not yet woken returns to
  the primary when the primary comes back. Lost leases, transient failures and
  revocations all requeue through `RequeueJob`, so the router re-evaluates
  every retry.
- **"Available" uses the claim's own eligibility rules.**
  - The per-worker conditions in `queue.sql:20-30` (local scope, the trust
    rule, free disk) move into a SQL function, `job_claimable_by`. Both
    `LockNextClaimableJob` and the router call it, so the two can't disagree.
  - A pull executor is available for a job when an eligible worker was seen in
    the last 60 s, judged by its `sandboxed`, its scope and its last reported
    free disk. An online but unsandboxed PC therefore doesn't count for a
    public upload.
  - The built-in worker has no liveness record today. It gains
    `projects.builtin_seen_at` and its free disk, written by its claims and
    heartbeats and coalesced like `agents.last_seen_at`. It is per project,
    not per replica (a known gap).
  - A provider is available when it is connected, its setup test passed, it
    isn't in error backoff, it is under its limits, and its declared disk fits
    the job.
- **Fallback is for missing capacity, never for a failing file.**
  - The rule is an allowlist, kept per asset rather than per job, so a
    re-encode of an errored asset is barred too.
  - A job may go to the fallback only if every earlier attempt on its asset
    ended `lost`, or failed with `agent_revoked`, a `wake_*` code,
    `source_download_failed`, `upload_failed` or `scratch_full`.
  - Today `transcode_failed` is the worker's catch-all
    (`internal/worker/worker.go:286-326`): a failed mkdir, a malformed key, a
    probe error that isn't "unsupported", and every encode error. The worker
    splits it:
    - `scratch_full` for the work directory and disk, which counts as
      infrastructure;
    - `output_invalid` gets its own code instead of `commit_failed`, because
      the file can cause it;
    - the encode keeps `transcode_failed`, which counts as the file's fault.
  - Whether `job_timeout` is the file's fault is open question 3.

### Dispatches and keys

- **`internal/dispatch` owns the `Executor` interface**, on the consumer side:

  ```go
  type Executor interface {
      Wake(ctx context.Context, w Wake) (instanceID string, err error) // idempotent, named clipmux-{dispatch_id}
      Stop(ctx context.Context, instanceID string) error                // idempotent
      Running(ctx context.Context) ([]Instance, error)                  // for the reconciler
      Check(ctx context.Context) error                                  // credentials, for Connect and status
  }
  ```

  This amends the architecture's `Available`, `Wake` and `Capabilities`.
  "Available" is router logic, because pull executors are judged in SQL.
  `Stop` and `Running` exist so a crash can't leave a paid machine running.
  `Capabilities` stays deferred until the dashboard offers combinations
  (D034). RunPod and Modal are the two implementations, and tests use a fake.
- **Routing a job to a provider is one transaction.**
  - It inserts a `dispatches` row and a `wake` outbox row, because a wake is a
    side effect outside Postgres (AGENTS.md).
  - It mints the one-time key in that transaction: `cmxd_` plus `rand.Text()`,
    stored as SHA-256 and also sealed (D017, associated data `dispatches/{id}`).
  - The limits are checked under a row lock on the provider's config, so two
    routers can't overshoot.
- **The publisher only reads.**
  - Today's publishers make the outside call inside the transaction that holds
    the outbox row (`internal/outputs/outputs.go:316`). A key minted there
    would be invisible to a worker that starts at once. After a crash, the
    retry would mint another key for a machine that already holds the first.
  - So the publisher re-reads the dispatch and its job, as D035 rebuilds
    routes from current state. It skips a dispatch that was cancelled, unseals
    the key and calls `Wake`.
  - A retried wake carries the same key, so the machine can always claim.
  - `stop` is an outbox kind too. The migration's Down deletes rows of both
    kinds before restoring the check, as `00010` does.
- **Claiming with a key.**
  - `dispatch.Keys` is the third `WorkerAuth`, after the built-in key and agent
    keys (D008, D043). A key works only while its dispatch is `waking` and its
    deadline hasn't passed.
  - A dedicated `LockDispatchedJob` and
    `UPDATE dispatches … WHERE status = 'waking' RETURNING` run in the claim
    transaction. Locks are taken in the order job, dispatch, attempt, asset.
  - A second claim with the key is therefore 401. `Release`, for a claim whose
    payload couldn't be delivered (D016), puts the dispatch back to `waking`.
  - Heartbeat, sign, complete and fail keep using the attempt token.
  - The key reaches the machine only through its environment, never argv
    (D012).
- **Serverless workers must be sandboxed**, because they take public uploads.
  - A claim with a dispatch key is 409 `sandbox_required` unless rlimits,
    `no_new_privs` and seccomp hold. Today the trust rule applies only to
    agents (`internal/workerapi/workerapi.go:249`). Modal's gVisor passes
    without Landlock (D051).
  - `clipmux worker --once` with a `cmxd_` key confines its tools by default
    and refuses `--unsandboxed`. It clears its work directory first.
  - It keeps retrying network errors and empty polls until it gets a 401 or
    its deadline passes. Today `--once` gives up after one try
    (`cmd/clipmux/worker.go:175`), which would strand a pod whose serve was
    restarting.
- **The reaper**, a leader loop every 30 s, stops a dispatch's machine when:
  - its attempt has ended;
  - its job is no longer queued before the claim (cancelled, superseded, or
    revived by a late heartbeat);
  - its claim deadline passes. The default is 10 minutes, measured in T04 and
    T05. The job returns to routing with `wake_timeout` and spends no budget;
  - it outlives the job timeout plus 15 minutes.
- **The reconciler** runs at boot and every 15 minutes. It compares
  `Running()` with live dispatches by instance ID, and stops any machine no
  live dispatch owns.
- **Wake failures:** a failed wake ends the dispatch with `wake_failed`, and
  the job returns to routing. Three in a row put the provider in a 5-minute
  backoff, so jobs wait for the primary instead of looping.
- **The setup test through a provider.**
  - The architecture asks for a real test job through the configured path
    after any change. The test's transcode is enqueued by the upload's
    complete call, which can't carry a route.
  - Instead, `setup_tests.executor` is set beside the test's asset, which is
    recorded before the upload completes
    (`internal/setuptest/setuptest.go:295`), and the router matches that
    asset. That route is exclusive: no other
    executor may claim it, so a test can't pass on the built-in worker.
  - The `encode` check fails as soon as its dispatch fails, rather than at the
    15-minute timeout.
  - A provider becomes routable only after its setup test passes.

### Limits and cost

- **Concurrency:** dispatches that haven't ended must stay below the
  provider's `max_concurrent`.
- **Spend:** the month to date (UTC) is the sum, over this month's dispatches,
  of each dispatch's rate times the time from wake to its end (or to now).
  - The hourly rate is prefilled from a price table in code and editable in
    Connect. Each dispatch keeps a copy, so a later change doesn't rewrite
    history.
  - A wake must fit its reservation within the limit (open question 1).
  - Running jobs are never killed at the limit. Connect shows the worst-case
    overshoot: concurrency × job timeout × rate.

### Providers

- **RunPod:**
  - On-demand pods through the REST API. Each pod gets the image, GPU types,
    cloud type, environment and start command, plus a registry auth that
    Connect created. T04 checks the endpoints against RunPod's current
    documentation.
  - RunPod restarts a pod's container when it exits, so the reaper's stop on
    attempt end is required, not an optimisation.
  - Pods default to Secure Cloud (open question 4).
  - `CLIPMUX_RUNPOD_API` overrides the base URL, for the fake, like
    `CLIPMUX_CLOUDFLARE_API` (D041).
- **Modal:**
  - A Sandbox through `libmodal`'s `modal-go`, pinned to a release at least two
    weeks old. T05 checks its API at the time.
  - Each sandbox uses the registry image through a secret Connect created, and
    runs `clipmux worker --once` with the key in its environment.
  - The GPU comes from config: L4 by default, with A10G and T4 accepted, as in
    D051.
  - The timeout is the job timeout plus a margin. Sandboxes are tagged with
    the project and the dispatch, so `Running` can list them.
- **The image:**
  - CI's image job pushes `ghcr.io/visheshgubrani/clipmux:sha-{commit}` on
    main, as a private package. This amends D005, D006 and D028, which said
    the image is never pushed. The package goes public at launch.
  - The binary carries its own image reference (`-ldflags`), so a provider
    runs the build that matches its server. Connect resolves the digest with
    the pull credential, and `CLIPMUX_WORKER_IMAGE` overrides the reference.
  - The image sets `NVIDIA_DRIVER_CAPABILITIES=compute,video,utility`, because
    NVENC needs `video`. The plain Ubuntu base is enough, because the driver
    is injected (D051).
- **Reachability:**
  - The architecture rules out a private control plane with Modal or RunPod,
    because their workers can't reach the worker API.
  - Connect resolves the public URL's host and refuses it unless
    `publicnet.Public` passes (`internal/publicnet/publicnet.go:46`).
  - This is a separate check from `settings.KeysAllowed`, which must keep
    accepting LAN installs (D023).
- **Trust:** a provider's machine sees the plaintext source and that output's
  content key, like every worker. The threats table gains a row for that, and
  one for a leaked one-time key: it claims one job, before a short deadline,
  and is stored hashed.
- **Lifecycle:** connecting again rotates the credentials.
  `DELETE /v1/executors/{kind}` disconnects, and is refused while the provider
  is the primary or while a dispatch is live.
- **New packages** follow "no package before its code". The router landed in
  `queue` (D053), which owns `routed_to`. Expect `dispatch` for the keys,
  publisher and reaper, and `runpod` and `modal` for the
  adapters and their Connect steps.

## Tasks

Order:

- T01 → T02 → T03 → T04 → T05 → T08 → T10.
- T06 runs alongside T01 to T03, and must be done before T04's real-account
  run.
- T07 and T09 depend on nothing in this phase and can run alongside.
- T00 runs whenever its secrets and hardware are available.

RunPod comes before Modal because its fake drives the CI exit test, and
Modal's SDK is the riskier dependency.

Each task is a vertical slice with its own tests. Port behaviour and tests
from the sources listed, but don't copy code paths that assume the old data
model. `vod` paths are under `~/vod` (archived), and `clipmux-lms` paths are
under `~/clipmux-lms`.

| Task | Scope | Port from |
| --- | --- | --- |
| T00 | **Carried tracks** (D052), run in parallel. Phase 2's real-account exit: the setup test through the Worker, the Chromium suites against the Worker hostname, and `exit.yml` on a real test zone. It needs the test zone, a scoped token and Workers Paid as repository secrets. Phase 3's by-hand checks: closing the lid of the M4 Mac mid-job with the Docker agent, and registering the self-hosted GPU runner. Each exit is recorded as a decision when it passes. | D037, D041, D050; `docs/cloudflare.md` |
| T01 | **done** (D053) — **The routing table.** A migration adds the routing settings, `projects.builtin_seen_at` and the built-in worker's free disk, `attempts.route_reason`, the `job_claimable_by` function, and the backfill. Enqueue stamps the primary, the router runs as a leader loop, the claim matches the class, and the built-in worker reports liveness. It adds the per-asset fallback allowlist and the worker's split failure codes (`scratch_full`, `output_invalid`). `GET` and `PUT /v1/settings/routing` (owner session). The jobs API shows `route_reason` on jobs and attempts, and `docs/api.md` gains both fields; it lacks `routed_to` today. It builds the fixture that passes the probe but fails the encode, and proves it. Tests on real Postgres, table-driven, with one case for each row of the architecture's routing table and a literal `(routed_to, route_reason)`: queue age evaluated while the primary is available; an unsandboxed PC not available for a public upload; the fallback barred per asset across a re-encode. | `internal/db/queries/queue.sql` (claim predicate), `agents.OnlineWithin`; architecture "Routing and fallback" |
| T02 | **done** (D054) — **Dispatches, one-time keys and `worker --once`**, against a fake `Executor`. A migration adds `executor_configs` (like `storage_configs`: config, sealed secrets, limits, rate and disk size), `dispatches`, `attempts.dispatch_id`, and the `wake` and `stop` outbox kinds. The key is minted at routing and the publisher only reads it. It adds `dispatch.Keys`, `LockDispatchedJob`, `Release` resetting the dispatch, `sandbox_required`, the retry rule for `--once`, and the setup test's exclusive route. Real-Postgres tests: a key claims once; an expired key and a second claim get 401; a retried wake reuses the key; a cancelled job is never woken; a released claim leaves the dispatch claimable. | `internal/workerapi` (`Auths`), `internal/agents` key handling (D043), `internal/outputs/outputs.go` (publisher), D035 |
| T03 | **done** (D055) — **The reaper, the reconciler, limits and spend.** Stopping on attempt end, on a job that is no longer queued, at the claim deadline and at the maximum lifetime. Reconciling by instance ID at boot and every 15 minutes. Concurrency held under parallel routers, the spend reservation, and wake backoff. Tests on real Postgres with the fake: each reason to stop; a wake timeout spends no budget; an orphan is stopped; neither limit is overshot under concurrent routing. | D035 (reconciler), D019 (passes safe to repeat) |
| T04 | **done; its real-account run and cold-start measurement come with T10** (D056) — **The RunPod adapter and Connect RunPod.** `POST /v1/executors/runpod/connect` (owner session) runs the steps `credentials`, `public_url`, `registry`, `image`, `saved` and `setup_test`, stopping at the first failure; each step can be repeated. It also adds `GET /v1/executors` and disconnect. Pods default to Secure Cloud. It measures a cold start against the claim deadline. The fake RunPod checks the bearer key on every call, records requests, and runs pods as local processes (T10 uses it). It starts `docs/executors.md`. Tests: the exact step order; the API key and registry credential appear in no stored row and no error; a container that keeps restarting is reaped. | `internal/cloudflare/{connect,client}.go`, `internal/cloudflare/connect_db_test.go` (`fakeCloudflare`) |
| T05 | **done; its real-account run comes with T10** (D057) — **The Modal adapter and Connect Modal**, in the same shape, on `modal-go`. Tests: the sandbox request it builds (image, command, environment, GPU, timeout and tags), against a fake at the SDK's seam; token errors that don't echo the token; a measured cold start. It adds Modal to `docs/executors.md`. | `deploy/modal/gpu_test.py` (GPU list), D051 |
| T06 | **done; the first push happens on merge to main** (D058) — **The worker image in a private registry.** CI's image job pushes `sha-{commit}` on main (`packages: write`). Build arguments put the image reference into the binary. Connect resolves its digest, and `CLIPMUX_WORKER_IMAGE` overrides it. The image sets `NVIDIA_DRIVER_CAPABILITIES`. | `.github/workflows/ci.yml` (image job), D028 |
| T07 | **done** (D059) — **`push --local --upload-source`** (the D045 gap). Once the pinned transcode is ready, a pinned `upload_source` job uploads the file through signed source parts to `uploads/{asset_id}/source`. Playback isn't delayed, and the one-mutating-job rule holds. `source_uri` then moves to storage, so later re-encodes route normally, fallback included. Tests: a later re-encode runs on another worker; a file edited after the encode fails `source_changed` before uploading. | D045; architecture "Local sources"; vod `server/src/routes/localImport.ts` |
| T08 | **done** (D060) — **Dashboard and Playwright.** A Routing card shows the primary, the fallback, queue age and the built-in worker's status. Connect RunPod and Connect Modal cards list their steps, like `delivery.tsx`, and show the month's spend against its limit and the live dispatches. The Jobs page shows each job's route and reason, and each serverless attempt's provider, GPU and estimated cost. A Playwright suite in `e2e/web` covers the Workers page (the connect card, agents, revoking, rescoping), the Jobs page and the new cards against the fake RunPod, which closes Phase 3's gap. | `dashboard/src/pages/{workers,jobs,delivery}.tsx`, D047 |
| T09 | **done; arm64 is proven in CI only** (D061) — **The self-contained Linux agent tarball** (the D049 gap). CI builds `clipmux-agent-linux-{amd64,arm64}.tar.gz` as a workflow artifact. It holds `clipmux`, FFmpeg with its shared libraries and a wrapper that sets the library path, the radeonsi and iHD VAAPI drivers, the packager, the GPL notices and checksums. `clipmux worker doctor` passes from it on a clean Ubuntu container and on the GPU runner. It is published only at launch. | vod `transcoding/Dockerfile.agent`; `toolchain/build_ffmpeg.sh` |
| T10 | **done for CI; the real-account run waits for its secrets** (D062) — **Exit test**, as listed above. It extends `internal/e2e` with the fake RunPod launching local `--once` workers, beside D050's agent helper. `exit-providers.yml` runs the real accounts with serve behind a tunnel. The shipped 90 s lease and 60 s offline rule apply, with no knob. It records the exit decision and updates the known gaps and the threats table. | D033, D050 (`internal/e2e/agents_db_test.go`) |

## Open questions

Each answer becomes a decision record before the task that needs it.

1. **Answered in D055.** **How is spend reserved before a wake?** (before T03) Recommendation:
   reserve the rate times an estimated duration from the source size, capped at
   the job timeout, and release it when the dispatch ends. Running jobs are
   never killed.
2. **How long may a woken machine take to claim?** (T04, T05) 10 minutes until
   cold starts are measured with the ~575 MB image on each provider.
3. **Is `job_timeout` the file's fault?** (before T01) Recommendation: yes, so
   it bars the fallback, because an encode that runs for six hours is
   suspicious. A related choice: should a job barred from the fallback, whose
   primary never returns, end after a bound like `PinnedWait`?
4. **Answered in D056: no.** **Should RunPod's Community Cloud be offered?** (T04) Its hosts would see
   plaintext sources and content keys. Recommendation: not in v1.
