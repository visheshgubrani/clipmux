# Several encodes on one PC

**Status: proposed (D137, 2026-11-20).** Built after v1.0.0, once F21's
`x264Threads` change has landed: T03 builds on it.

Source: [decisions.md](../decisions.md) D137, with
[architecture.md](../architecture.md), "Compute". Today a PC agent or the
built-in worker runs one media job at a time, plus one transcription beside
it. A provider already runs several, one machine per job up to
`max_concurrent` (D132). This plan lets one PC run several encodes, and lets
its next encode start while the last one uploads.

Only the worker changes. The server already lets an agent hold several
attempts (D137 lists where), so the schema, the worker API, routing and the
dashboard stay as they are, and a newer agent works with an older server.

**Before this plan:** v1.0.0 has shipped, and F21's change to
`internal/media/encoders.go` is committed.

**Exit test:**

1. **A GPU PC runs them side by side.** Ten 1080p uploads are queued for one
   PC with NVENC or VAAPI and `--jobs auto`.
   - They finish sooner than with `--jobs 1`, by the margin T01 predicts.
   - No attempt fell back to the CPU because another encode held the GPU:
     every manifest names the GPU stage.
   - The `free_disk_bytes` the worker reported never went below zero.
2. **A CPU-only PC overlaps its uploads.** With `--jobs auto` it picks 1, and
   the same ten finish sooner than on the commit before T02, because each
   upload overlaps the next encode.
3. **Shutdown hands everything back.** A SIGTERM while three attempts run
   hands all three back with `worker_stopped`, and each job is claimed again
   at once.
4. **Serverless is unchanged.** A `--once` machine claims one job, as before.

## Tasks

Order:

- **T01** first: its numbers set `auto`.
- **T02** next. **T03, T04 and T05** then run alongside, since they touch
  different files.
- **T06** follows T02 and T03. **T07** is last.

| Task | Scope | Port from |
| --- | --- | --- |
| T01 | **Measure.** On the 12-thread dev machine (RX 6600 XT over VAAPI), one NVIDIA GeForce card and one L4: total throughput with 1, 2 and 3 encodes at once, for the golden 1080p ladder and a 6-rung 4K ladder; peak RSS per encode; and what a GeForce card does past its NVENC session cap. The numbers, and the `auto` rule they give, go into D137. | D129, D132 |
| T02 | **The claim loop** in `internal/worker/worker.go`. `Worker.Jobs` sets the encode slots. `Run` takes an in-flight token (at most 2 × Jobs) and a slot before each claim, and gives both back after an empty poll. `process` gives the slot back when `media.Transcode` returns, before the upload. `Run` waits for every attempt it started. Tests against the fake server in `worker_test.go` and `worker_db_test.go`: N attempts at once; no claim without a free slot; an upload overlapping the next encode; shutdown handing every attempt back. | — |
| T03 | **The share.** `media.Tools` gains the CPUs one encode may use; `x264Threads` (`internal/media/encoders.go`) and `sandbox.CPUBudget` (`internal/media/run.go`) read it, and zero means `runtime.NumCPU()` as today. Table tests next to F21's `x264Threads` cases. | F21 |
| T04 | **Contention retries alone.** A machine-wide `sync.RWMutex`, owned by the worker and passed through `media.Tools`. `encodeLadder` runs each stage under its read lock. A stage that fails while another encode ran gives its read lock back, takes the write lock and retries once before falling through; a stage that fails alone falls through as today. Tests with a fake stage that fails only beside another: it retries alone and keeps its backend, and a stage failing alone still reaches the CPU. | — |
| T05 | **The disk ledger.** The reported `free_disk_bytes` is the measured free space minus what running attempts may still write: each one's need under `job_claimable_by`'s rule (migration 00035) less what its directory holds. The rule is written in Go a second time, citing the SQL. The media and speech loops share the ledger. A `*_db_test.go` shows that two claims can't both take the last space. | — |
| T06 | **The setting.** `--jobs auto\|N` and `CLIPMUX_WORKER_JOBS` in `cmd/clipmux/worker.go`, from 1 to 16; `--once` forces 1. `internal/config` gains `CLIPMUX_WORKER_JOBS`, and `internal/server/builtin_worker.go` passes it to the built-in worker's environment. The worker logs its slots at start, and each `attempt committed` line says how many encodes ran beside it. The CLI and settings pages in `site/content/docs` are updated (`cmd/clipmux/docs_test.go` and `internal/config/doc_test.go` check them). | D129 |
| T07 | **Exit**, items 1–4, on real hardware. Media tests run locally with `CLIPMUX_REQUIRE_MEDIA_TOOLS=1`, since PR CI has no media tools. | — |

## Open questions

Each answer becomes a decision record before the task that needs it.

1. **Should uploads have a cap below N?** (T02) On a slow upstream, two
   uploads at once share the bandwidth and finish no sooner. Recommendation:
   measure in T01 before adding a second knob.
2. **Should a PC with two GPUs use both?** Today a worker encodes on one
   VAAPI device and NVENC's default device. Recommendation: not in this plan.
3. **Should the Encoding page show "k of N busy"?** It needs an additive
   claim field and somewhere to show it, since the Workers page is gone
   (D116). Recommendation: only if owners ask.
