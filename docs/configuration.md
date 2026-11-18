# Configuration

Every setting a deployment uses is documented on the site, in
[site/content/docs/reference/configuration.mdx](../site/content/docs/reference/configuration.mdx)
(clipmux.com/docs/reference/configuration). This file keeps only the
variables that tests and development read, which the public page leaves
out. `internal/config/doc_test.go` fails when the Go source reads a variable
neither page names, or a deployed one the public page doesn't.

## Tests and development


| Variable | What it does |
| --- | --- |
| `TEST_DATABASE_URL` | Real-Postgres tests run against it; skipped when unset. |
| `CLIPMUX_TEST_S3_ENDPOINT`, `CLIPMUX_TEST_S3_ACCESS_KEY`, `CLIPMUX_TEST_S3_SECRET_KEY` | The S3 store the storage contract tests use (the dev SeaweedFS). |
| `CLIPMUX_REQUIRE_S3`, `CLIPMUX_REQUIRE_MEDIA_TOOLS` | In CI: a missing S3 store or media tool fails instead of skipping. |
| `CLIPMUX_SKIP_SPEECH` | `1` skips the tests that run real whisper.cpp, even under `CLIPMUX_REQUIRE_MEDIA_TOOLS`. CI sets it; run them locally. |
| `CLIPMUX_LIVE_APPROVAL`, `CLIPMUX_LIVE_APPROVAL_WAIT` | `1` starts a real Modal token flow (D109) and prints the page to open; with `_WAIT` it also waits for you to approve it, which creates a real token in your workspace. |
| `CLIPMUX_ANALYTICS_SCALE` | `1` runs the analytics scale test: it seeds 180 days of history and fails if a 90-day read takes over 500 ms (P7-T03). About two minutes. |
| `CLIPMUX_TEST_MODELS_DIR` | Where the speech tests find the pinned whisper and VAD models, by their upstream file names (`toolchain/fetch_speech_models.sh DIR` fetches them). Without it they skip, or fail under `CLIPMUX_REQUIRE_MEDIA_TOOLS`. |
| `CLIPMUX_E2E` and `CLIPMUX_E2E_*`, `CLIPMUX_FIXTURE_PORT`, `CLIPMUX_GPU_TEST` | The real-binary exit tests and the browser fixture (D033). |
