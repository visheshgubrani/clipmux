# Phase 3 — PC agent

Source: [architecture.md](../architecture.md), "Build order", "Compute", "Local
sources", "Worker sandbox" and "Optional upgrades", with
[decisions.md](../decisions.md) where they differ (D042 sets this phase's
scope). Phase 3 lets an owner's own machine encode for the instance:

- **Agent mode:** `clipmux worker` against a remote server, with one key per
  agent, owner approval and revocation.
- **Connect my PC:** the dashboard card that pairs a new agent.
- **Capability reporting and sandboxing on agents**, including hardware
  encoding with a CPU fallback.
- **`clipmux push`**, with **`--local`** for sources that stay on the agent.
- **`clipmux listen`** for webhooks on a laptop.

The integration API, the output format, tokens and delivery stay the same. An
agent pulls work through the same worker API as the built-in worker, with no
separate path.

Phase 2 exited on the harness (D041). Its real-account run is task T00, a
parallel track that needs Cloudflare secrets rather than code from this phase
(D042).

**Exit test:** sleeping a PC mid-job recovers it, and a local push uploads only
outputs.

Concretely, automated in CI (T09) on the real binary unless marked otherwise:

1. **A sleeping agent recovers on its own.** The built-in worker is off, and one
   approved agent runs in its own process group. The test SIGSTOPs the group
   mid-encode for longer than the 90 s lease and its sweep: the attempt goes
   `lost` and the job is requeued. After SIGCONT, the same attempt heartbeats
   back to `running` with `lost_count` 1, commits its output, and the asset
   plays.
2. **A sleeping agent is replaced.** While the agent is stopped, the built-in
   worker claims the requeued job on a new attempt and output. After SIGCONT,
   the agent is told `stop`, the committed output is the new one, and nothing
   in the old prefix is referenced.
3. **A local push uploads only outputs.** `clipmux push --local` hands a file to
   the agent. The asset's store never holds an `uploads/{asset_id}/` object.
   Every path signed for the attempt is output grammar, and the asset plays,
   signed and encrypted. A re-encode is pinned to the same agent.
4. **Revocation.** A revoked agent claims nothing more, and its running attempt
   is fenced at once rather than after its lease.
5. **GPU encoding with CPU fallback.** In CI, a fake ffmpeg drives selection and
   fallback. On real hardware:
   - On the self-hosted runner, VAAPI encodes on an AMD RX 6600 XT and the
     manifest records `vaapi`. A forced encoder failure falls back to CPU
     within the same attempt and still commits.
   - On Modal, the same golden checks pass for `nvenc` (L4) and `cuda-decode`
     (A100, which has NVDEC but no NVENC).
6. **By hand:** with the agent in Docker Desktop on an M4 Mac, closing the lid
   mid-job and reopening it recovers the job. This is a real suspend, which
   SIGSTOP can't fully imitate (monotonic clocks, dropped TCP connections).

## Design notes

Each task is planned against these points. Each is settled in that task's
decision record.

- **Pairing, not a key in the command.** The Connect my PC card shows a one-line
  command with a single-use pairing code that lasts 15 minutes.
  `clipmux worker pair --server URL CODE` exchanges it for the agent's own key
  (stored as SHA-256, like API keys) and writes server and key to a 0600 config
  file. The key never reaches argv (D012), and the agent arrives `pending`. This
  amends the architecture's `clipmux worker --server URL --key KEY` (D042).
- **`AgentKeys` is the second `WorkerAuth`.** The seam already exists
  (`internal/workerapi`, D008). An agent key yields
  `queue.Worker{Executor: "agent", AgentID, LocalOnly}`, which the claim query
  already filters on (pinned jobs, local scope). Unknown and revoked keys get
  the same 401. A pending agent long-polls but claims nothing. Every
  authenticated call updates `last_seen_at` and `last_ip`, with the writes
  coalesced so heartbeats don't write a row each.
- **Trust rule.** A sandboxed agent may take any job. An unsandboxed agent takes
  only jobs pinned to it and the owner's own uploads (the Dashboard app's
  assets), never public uploads (architecture "Worker sandbox").
  - Today's `CHECK (sandboxed OR scope = 'local')` is stricter than the
    architecture, so a migration relaxes it and the claim gains the predicate.
  - Sandbox status is self-reported. That is acceptable, because an agent that
    lies exposes only its own machine.
  - Agents run the sandbox probe by default. `--unsandboxed` opts out
    explicitly, where today `--sandbox` opts in.
- **Waiting is not failing.** A job pinned to an offline agent waits in the queue
  without spending either retry budget. A lease lost to sleep counts against the
  lost budget (5), never the failure budget (3). The only bound on waiting is
  the pinned-job timeout (`agent_unavailable`), which closes an audit leftover.
- **An agent that loses the server keeps working.** It doesn't stop itself after
  a lease of silence. The server decides through `stop`, because a lost attempt
  stays live until a newer one exists (D008). Expired PUT and source URLs are
  signed again (D016).
- **No `Executor` interface yet.** `Wake` gets its second implementation with
  Modal in Phase 4, so Phase 3 adds none (AGENTS.md). Every executor pulls. The
  built-in worker and agents compete for jobs (open question 2).
- **Hardware encoding** lives in `internal/media`, shared by agents and the
  built-in worker, so a GPU on the server host works through compose `gpus:` or
  `devices:`. The FFmpeg build already has NVENC, NVDEC, CUVID, VAAPI and
  `cuda-llvm` (`toolchain/build_ffmpeg.sh`).
  - **Backends:**
    - `nvenc`: CUDA decode, `scale_cuda`, `h264_nvenc`.
    - `vaapi`: VAAPI decode, `scale_vaapi`, `h264_vaapi`. This covers AMD
      (radeonsi) and Intel (iHD) on Linux.
    - `cuda-decode`: NVIDIA cards without NVENC. The GPU decodes and libx264
      encodes.
    - `cpu`: today's libx264 pipeline.
  - **Selection.** `ffmpeg -encoders` lists `h264_nvenc` on a machine with no
    NVIDIA card, so it proves nothing. A backend is offered only after a real
    two-frame probe encode when the worker starts. A per-source preflight then
    picks a hybrid (CPU decode, GPU encode) when the GPU can't decode the
    source's codec or bit depth.
  - **Parity.** Every backend gets the same ladder bitrates, maxrate and
    bufsize. Keyframes are forced every 6 s, GOPs are closed, scene-cut
    keyframes are off, and every backend uses the high profile, level by
    height, CFR and 8-bit 4:2:0 (D012). HDR tone-mapping stays on the CPU.
    Tests check that keyframes land on segment boundaries for every backend,
    because Shaka cuts at keyframes.
  - **Fallback inside the attempt.** Only an encoder failure (device init, GPU
    memory, or an encode the device refuses) moves to the next backend. Order:
    `nvenc` → `vaapi` → `cuda-decode` → `cpu`.
    - The whole ladder runs again on that backend within the same attempt, so
      "one attempt, all renditions" still holds.
    - The heartbeat keeps the lease meanwhile, and the fallback costs no retry
      budget.
    - A corrupt source or a packaging failure never falls back.
    - `manifest.json` records the backend chain as `encoder`, and so does the
      attempt's `worker_info`, so the Jobs page can show it.
  - **Override.** `--encoder` / `CLIPMUX_ENCODER` names the first backend to
    try. The CPU fallback still applies.
- **GPU sandbox (D027).**
  - Landlock opens read-write only the device nodes the probe found
    (`/dev/dri/renderD*`, `/dev/nvidia*`), plus the `/sys` and `/proc/driver`
    paths the drivers read.
  - Seccomp still refuses `socket()`.
  - CUDA reserves more virtual address space than the 8 GiB `RLIMIT_AS`
    allows, so GPU backends replace it with `RLIMIT_DATA`. This is to be
    confirmed on hardware.
  - The tool's environment gains only a whitelist of driver variables.
- **Agents never fetch arbitrary URLs.** Imports stay in serve (D039). On
  filesystem storage, an agent needs the public URL to be reachable, because
  signed URLs point at it. `--storage-via-server` stays a built-in worker flag
  (architecture "Compatibility", D023).
- **Local sources keep their path on the agent.** The server sees only
  `agent://{agent_id}/{source_id}` and an `ags_…` ID. Real paths, roots and file
  identity stay in the agent's local state, so the server can't ask for an
  arbitrary file (architecture threats table).
- **New packages** follow "no package before its code". Expect `agents` for
  pairing, `AgentKeys` and the owner API, and a CLI client package shared by
  `push` and `listen` once both need one.

## Tasks

Order:

- T01 → T02 → T05 → T06 → T08 → T09.
- T03 runs after T02, because it reports encoders through T02's capability
  claim.
- T04 and T07 depend on nothing in this phase and can run alongside.
- T00 runs whenever the Cloudflare secrets exist.

Each task is a vertical slice with its own tests. Port behaviour and tests
from the sources listed, but don't copy code paths that assume the old data
model. `vod` paths are under `~/vod` (archived), `clipmux-lms` paths are under
`~/clipmux-lms`, and old plans are under the tag `archive/open-source-p17` in
clipmux-lms (P19 CPU/GPU encoding, P20 self-hosted worker agent, P34 leases and
cancellation).

| Task | Scope | Port from |
| --- | --- | --- |
| T00 | **Phase 2 real-account exit** (parallel track, D042). Covers the setup test through the Worker and the Chromium suites against the Worker hostname. It adds an `exit.yml` job on a real test zone (main, weekly, by hand), skipped while its secrets are absent. It confirms against Cloudflare the Workers Paid plan-name match, the script upload format and the custom-domain call (D037). Then it records the Phase 2 exit as a decision and removes those known gaps. Needs a test zone, a scoped token and Workers Paid as repository secrets. | D037, D041; `docs/cloudflare.md` |
| T01 | **done** (D043) — **Agent identity.** Pairing works in both directions, and the PC the owner is using connects with one command and one click. A migration adds pairing codes: single use, 15 minutes, stored hashed, with an alphabet without look-alike characters. The owner API (`internal/manage` style, session only) creates a pairing, lists agents, and approves, revokes or rescopes one. `POST /worker/v1/pair` takes a code and a name, and returns the agent's key once. `AgentKeys` implements `WorkerAuth`. `clipmux worker pair` writes the 0600 config file, and `clipmux worker` reads it when `CLIPMUX_WORKER_KEY` is unset. Revoking fences the agent's running attempts and requeues their jobs at once. Attempts record their agent, so the API can list the outputs an agent produced for re-encoding. Real-Postgres tests: a code works once and expires; a pending agent claims nothing; revoking mid-attempt answers `stop` and requeues the job; unknown and revoked keys get the same 401. | vod `server/src/lib/agentToken.ts`, `server/src/middleware/agentAuth.ts`, `server/src/routes/transcoder.ts` (pair, revoke, whoami, heartbeat) |
| T02 | **done** (D043, D046) — **Agent mode and capability reporting.** Worker side: it clears stale attempt directories at start (only serve's supervisor does today). It backs off network errors and 5xx from 1 s doubling to 30 s, and waits 60 s after a 401. A pending agent waits, and a revoked one exits non-zero with a clear message. Media tools run in their own process group, so a stop or SIGTERM ends them all. The claim reports encoders proved by probe, GPUs, free disk in the work directory (a known gap today) and sandbox status. Server side: those go into `agents.capabilities` and `agents.sandboxed`. A claim is refused when free disk is below what the source needs (a factor of its size, decided here). The trust rule is a migration plus a claim predicate. Re-encoding an `agent://` source pins the job to that agent, and a revoked agent gives 409 `source_unavailable`; today `lifecycle/reencode.go` treats every `source_uri` as a storage key. Tests: claim eligibility on real Postgres for sandboxed and unsandboxed agents across public, owner and pinned jobs; worker backoff against a scripted fake server with the exact waits asserted. | vod `server/src/lib/agentCapabilities.ts`, `transcoding/clipmux_transcoder/agent/{client,daemon}.py`; P20 failure table (C11) and its scripted fake-server tests (P20-T06) |
| T03 | **done; NVENC and `cuda-decode` still need their Modal run** (D046) — **Hardware encoding and CPU fallback**, per the design notes: backends, probe selection, per-source preflight, encoder arguments at parity, fallback inside the attempt, the manifest's `encoder` field, the GPU sandbox rules, VAAPI runtime drivers in the image (`mesa-va-drivers`, `intel-media-va-driver`), and `verify_toolchain.sh` checking the encoders the build must have. Tests: argument builders against literal expected arguments per backend; selection and fallback with a fake ffmpeg (probe passes, probe fails, encode fails mid-ladder, source fails); the CPU golden test unchanged. A `gpu` workflow on a `[self-hosted, gpu, vaapi]` runner (the owner's Ubuntu machine with an RX 6600 XT; main, weekly, by hand) runs the golden encode, keyframe alignment, decryption and an SSIM threshold against the CPU output for each backend it finds. The runner's service user must be in the `render` group. The same checks run in the image on Modal, on an L4 (`nvenc`) and an A100 (`cuda-decode`): by hand before the exit, and as a manual workflow with a Modal token secret. Prefer Modal's Go SDK, so Phase 4's Modal adapter can reuse it. | vod `transcoding/clipmux_transcoder/encoding/{backends,selection,probe,failures,validation}.py`; clipmux-lms `transcoding/video/transcode.py` (NVENC arguments, CPU decode for HDR); P19 (one real encode as the probe, keyframe parity enforced by tests) |
| T04 | **done** (D044) — **`clipmux push`**, a Go client of the public upload API. The API key comes from `CLIPMUX_API_KEY` or the config file, never argv. It uploads 32 MiB parts, four at a time. It resumes from `GET /v1/uploads/{id}/parts` with a local state file keyed by path, size and modification time, and follows D031's retry rules. The `Idempotency-Key` is derived from the file's identity, so a re-run doesn't create a second asset. Flags: `--policy`, `--passthrough`, `--wait` (until ready, then print the playback ID), and directories walked for video files. It replaces the "not implemented" stub. Tests against a real serve on the filesystem and S3 stores: a push killed mid-upload resumes and sends only the missing parts; a re-run after completion creates nothing. | `packages/uploader` (D031: windows of 100 part URLs, retry and resume rules); vod `sdk/src/uploader.ts` |
| T05 | **done except `--upload-source`** (D045) — **`push --local` and pinned sources.** The agent's config names the roots it may read, and it serves a local control socket (0600; in Docker, bind-mount the socket's directory and the roots at the same paths). `clipmux push --local PATH` asks the agent to register a file under a root, refusing symlink escapes and anything but a regular file. The agent records root, relative path, size and modification time. The agent calls `POST /worker/v1/sources` and gets an `ags_…` ID (an `agent_sources` row). The CLI calls `POST /v1/assets {agent_source_id, playback_policy, passthrough}` with its API key. That creates the asset with `source_uri = agent://{agent_id}/{source_id}` and a transcode job pinned to the agent. A source attaches to one asset only, in its own project. The claim payload carries `agent_source_id` instead of `url` (architecture "Job payload"), and Landlock grants read access to that one file. A file that is missing or changed gives a permanent `source_missing` or `source_changed`. `--upload-source` also uploads the source through `sign` source parts after the commit, so later re-encodes can run anywhere. A job pinned to an agent unseen for 7 days (open question 3) fails with `agent_unavailable`. Deleting the asset tells the agent to forget the source. Tests: path refusal cases; the pinned job waits while its agent is offline without spending a budget; another agent can never claim it. | vod `server/src/routes/localImport.ts` (roots and relative paths, re-registration returns the same reference), `server/src/lib/localJobQueue.ts` (pinned, waiting reasons), `transcoding/clipmux_transcoder/{snapshot.py,agent/journal.py}`; architecture "Local sources" |
| T06 | **done; its Playwright suite comes with T09** (D047) — **Dashboard: Workers and Jobs pages, and the Connect my PC card.** The owner API adds `GET /v1/jobs` (filter by status, asset or agent) and `GET /v1/jobs/{id}`, showing attempts with executor, agent, encoder chain, phase and progress, error code and route reason (architecture "Routing": "where each job ran, each attempt"). The Workers page lists pending, approved and revoked agents with last seen, IP, ffmpeg and packager versions, GPUs and encoders, free disk, sandbox status and scope. From there the owner approves, revokes, rescopes, and re-encodes the outputs an agent produced. The card shows the pairing command in tarball and Docker variants, and the new agent appearing as pending. A Playwright suite runs in `e2e/web`. This closes the known gap of missing jobs and workers pages. | vod `web/components/dashboard/transcoders-panel.tsx`, `setup/src/pairing.ts` (the flag-order trap in a copy-paste command) |
| T07 | **done, without a new scope or server-side relay** (D048) — **`clipmux listen --forward-to URL`.** A new API-key scope, `events:listen`, lets a key receive its app's events without managing endpoints; managing endpoints stays the owner's (architecture "Accounts and tenancy"). `POST /v1/listen` creates an ephemeral relay with its own secret, which the CLI prints. The CLI long-polls `GET /v1/listen/{id}/events`. The publisher fans the app's events out to live relays the way it does to endpoints (D018). The CLI POSTs each event to the local URL with D018's headers and body, signed with the relay secret, and acknowledges with the local status. A relay expires after 1 hour without a poll. It replaces the "not implemented" stub. Real-Postgres tests: a local receiver verifies the LMS signature scheme byte for byte, and an expired relay receives nothing. | D018; vod `server-sdk/src/webhooks.ts` (verification, 300 s tolerance) |
| T08 | **done except the self-contained tarball (launch)** (D049) — **Agent packaging and `clipmux worker doctor`.** CI builds a Linux tarball for amd64 and arm64 as an artifact: `clipmux`, the pinned ffmpeg with its shared libraries, the packager, GPL notices and checksums, from the toolchain stage. Nothing is published before launch (D005, D006). The existing image doubles as the Docker agent (`clipmux worker`, with `--gpus all` or `--device /dev/dri`), documented for Linux, WSL2 and Docker Desktop. `doctor` checks reachability of the server and the public URL, the pairing, the sandbox probe, a probe encode per backend, free disk and clock skew, and the Connect my PC card links to it. Adds `docs/agents.md`. | vod `transcoding/Dockerfile.agent` (non-root, tini, healthcheck), `transcoding/clipmux_transcoder/agent/cli.py` (`doctor --full`) |
| T09 | **done on the real binary; the M4 lid-close and Modal runs are by hand** (D050) — **Exit test**, as listed above. It extends `internal/e2e` (D033) with a helper that pairs and approves an agent through the API, then starts `clipmux worker` in its own process group so SIGSTOP and SIGCONT reach the media tools too. It runs under the real 90 s lease with no knob, so the shipped defaults are what's proven. The GPU half runs on the self-hosted runner and on Modal, and the lid-close check is recorded in the exit decision. | D033 harness, `internal/e2e/exit_db_test.go` (`TestKilledWorkerRecoversOnANewAttemptAndOutput`); vod `transcoding/tests/test_agent_lease_and_paging.py` |

## Open questions

Each answer becomes a decision record before the task that needs it.

1. **Hardware this phase can't prove.** No Intel machine is available, so a real
   iHD run of the VAAPI path, and Quick Sync, stay a known gap. Native Windows
   (AMD's AMF) and native macOS (VideoToolbox) are out of Phase 3 by the
   platform choice in D042. Such agents run in WSL2, Docker or a VM.
2. **Should the built-in worker defer to agents?** In Phase 3 both pull, so on a
   host with a GPU PC paired, the built-in CPU worker still takes jobs when it
   is free. Recommendation: a setting that turns the built-in worker off
   (today only `CLIPMUX_BUILTIN_WORKER=false` does). Preference and fallback
   routing are Phase 4's routing table. Decide before T06.
3. **How long does a pinned job wait** for its agent before `agent_unavailable`?
   Recommendation: 7 days, matching the retire window. Decide before T05.
