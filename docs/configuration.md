# Configuration

Every setting is an environment variable; nothing is read from a file except
the keys serve keeps under `CLIPMUX_DATA_DIR`. `.env.example` lists each
variable with an example. A test (`internal/config/doc_test.go`) fails when the
Go source reads a variable this page or `.env.example` doesn't name.

A value that doesn't parse stops the command at start, naming the variable.

## clipmux serve

| Variable | Default | What it does |
| --- | --- | --- |
| `DATABASE_URL` | required | Postgres. pgxpool options go in the URL (`pool_max_conns=20`); session settings too (`statement_timeout=30s`). Unset, every session gets `statement_timeout=60s` and `idle_in_transaction_session_timeout=5min` (D068). serve waits up to a minute for Postgres at boot. |
| `CLIPMUX_LISTEN` | `:8080` | Where serve listens. Behind Caddy, keep it off the public interface. |
| `CLIPMUX_DATA_DIR` | `./data` | Filesystem storage, the storage signing key, and the secrets key when `CLIPMUX_SECRETS_KEY` is unset. Back it up (docs/operations.md). |
| `CLIPMUX_SECRETS_KEY` | `{data_dir}/secrets.key` | `v1:` and 64 hex digits; seals every secret kept in Postgres (D017). Losing it makes every stored secret, and every output, unusable. |
| `CLIPMUX_SECRETS_KEY_PREVIOUS` | unset | The key being rotated away from, only until `clipmux admin reseal-secrets` has run (D071). |
| `CLIPMUX_PUBLIC_URL` | chosen at first boot | The origin browsers and workers reach serve at. Set, it pins both URLs and the first-boot URL step is read-only (D023). |
| `CLIPMUX_MEDIA_URL` | `CLIPMUX_PUBLIC_URL` | The origin playback is served from (`base_url`); ideally another registrable domain. Needs `CLIPMUX_PUBLIC_URL`. |
| `CLIPMUX_TRUSTED_PROXIES` | none | Addresses or CIDR ranges (Caddy's) whose `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Request-Id` are believed. Rate limits, sign-in limits, request IDs and the https redirect all depend on it (D064, D065, D067). |
| `CLIPMUX_MAX_UPLOAD_BYTES` | the storage limit (~312 GiB) | The largest declared upload. |
| `CLIPMUX_BUILTIN_WORKER` | `true` | Start `clipmux worker` as serve's child. |
| `CLIPMUX_UNSANDBOXED_WORKER` | `false` | Run the built-in worker where the media tool sandbox doesn't hold. Only if the host isolates it another way (D027). |
| `CLIPMUX_TOOL_MEMORY_MB` | `8192` | Memory limit for each media tool. Passed to the built-in worker. |
| `CLIPMUX_FFMPEG`, `CLIPMUX_FFPROBE`, `CLIPMUX_PACKAGER` | found on `PATH` | The media tools. Shaka Packager must be 3.9.1 or later. |
| `CLIPMUX_LOG_FORMAT` | `text` | `text` or `json` (D064). Passed to the built-in worker. |
| `CLIPMUX_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Successful playback, storage and worker requests log at debug. |
| `CLIPMUX_METRICS_ADDR` | unset (off) | `host:port` for `/metrics`, a listener of its own with no auth: keep it off the internet (D066). |
| `CLIPMUX_PPROF` | `false` | Add Go's `/debug/pprof/` to the metrics listener. Needs `CLIPMUX_METRICS_ADDR`. |
| `CLIPMUX_SELF_DIAL` | unset | Where the setup test connects for every URL it checks, keeping each URL's hostname (compose: `caddy`) (D026). |
| `CLIPMUX_CLOUDFLARE_API` | Cloudflare's v4 API | Cloudflare's API base, for a proxy or a test double. |
| `CLIPMUX_R2_ENDPOINT` | `https://{account_id}.r2.cloudflarestorage.com` | R2's S3 endpoint, e.g. the EU jurisdiction's. |
| `CLIPMUX_RUNPOD_API` | RunPod's v1 REST API | RunPod's API base, for a test double. |
| `CLIPMUX_WORKER_IMAGE` | the image this build names | The image RunPod and Modal machines run (D058). |
| `CLIPMUX_EXECUTORS_ALLOW_PRIVATE_URL` | `false` | Connect RunPod or Modal while the public URL is private: development only. |

## clipmux worker

A PC agent paired with `--pair` keeps its server and key in `agent.json` and
needs none of these; the built-in worker gets its own from serve.

| Variable | Default | What it does |
| --- | --- | --- |
| `CLIPMUX_SERVER` | from `agent.json` | The server's public URL. |
| `CLIPMUX_WORKER_KEY` | from `agent.json` | The worker's key. Never in argv. |
| `CLIPMUX_AGENT_DIR` | the user's config directory | Where `agent.json`, `sources.json` and `agent.sock` live. |
| `CLIPMUX_ENCODER` | `auto` | The first encoder to try: `auto`, `nvenc`, `vaapi`, `cuda-decode` or `cpu` (D046). |
| `CLIPMUX_VAAPI_DEVICE` | every `/dev/dri/renderD*` | The VAAPI render node. |
| `CLIPMUX_TOOL_MEMORY_MB`, `CLIPMUX_FFMPEG`, `CLIPMUX_FFPROBE`, `CLIPMUX_PACKAGER`, `CLIPMUX_LOG_FORMAT`, `CLIPMUX_LOG_LEVEL` | as for serve | |

## clipmux push and clipmux listen

| Variable | Default | What it does |
| --- | --- | --- |
| `CLIPMUX_API_KEY` | required | The app's API key. Never in argv. |
| `CLIPMUX_PUSH_STATE_DIR` | the user's cache directory | Where unfinished pushes keep their upload tokens (0600). |
| `CLIPMUX_LISTEN_SECRET` | generated once, kept beside `agent.json` | The `whsec_` secret `listen` signs forwarded events with. |

## clipmux admin

`clipmux admin` reads `DATABASE_URL`, and where it needs secrets
`CLIPMUX_DATA_DIR`, `CLIPMUX_SECRETS_KEY` and `CLIPMUX_SECRETS_KEY_PREVIOUS`,
the same as serve. `clipmux admin` with no arguments lists its commands.

## Tests and development

| Variable | What it does |
| --- | --- |
| `TEST_DATABASE_URL` | Real-Postgres tests run against it; skipped when unset. |
| `CLIPMUX_TEST_S3_ENDPOINT`, `CLIPMUX_TEST_S3_ACCESS_KEY`, `CLIPMUX_TEST_S3_SECRET_KEY` | The S3 store the storage contract tests use (the dev SeaweedFS). |
| `CLIPMUX_REQUIRE_S3`, `CLIPMUX_REQUIRE_MEDIA_TOOLS` | In CI: a missing S3 store or media tool fails instead of skipping. |
| `CLIPMUX_E2E` and `CLIPMUX_E2E_*`, `CLIPMUX_FIXTURE_PORT`, `CLIPMUX_GPU_TEST` | The real-binary exit tests and the browser fixture (D033). |
