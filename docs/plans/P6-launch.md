# Phase 6 — Launch

Source: [architecture.md](../architecture.md), "First run and packaging" and
"Risks and open decisions", with [decisions.md](../decisions.md) where they
differ (D086–D088 set this phase's scope). Phase 6 takes ClipMux from a
private repository that passes in CI to a public v1 that strangers can
install in one line and that the LMS already runs on:

- **Prove what's built:** every real-account, device and drill run that
  Phases 2–5 left pending passes and is recorded. Nothing ships as beta
  (D088).
- **Distribution:** the licence and community files, a release on each
  `vX.Y.Z` tag (a public image, agent tarballs, CLI binaries, npm packages),
  and one-line installs for the server and for a PC agent.
- **The dashboard designed:** a design layer (tokens, brand, dark mode,
  phones), the missing pages and actions, and error handling throughout.
- **Docs and agent skills:** a current README, a docs site, an integrator
  quickstart with token signing in several languages, and agent skills a
  coding agent installs with `npx skills add visheshgubrani/clipmux`.
- **The SDK and the four-step integration** (Track E, D112): `@clipmux/node`,
  an OpenAPI spec of the integration API, `npx @clipmux/node init`, and
  browser elements that need no configuration.
- **The LMS cutover** (D087), on a release image installed by the one-liner,
  through a soak window, before the repository goes public.

The integration API keeps every existing route and field. This phase adds
the owner routes the dashboard needs (T12) and, for the SDK, `GET /v1/app`
and app pairings (T23, D112). It changes nothing in the output format,
tokens or the worker path grammar.

**Before this phase:** Phase 5's branch (`phase5/production`, never pushed)
is pushed, its CI runs on GitHub (`ci.yml`, `exit.yml`, `exit-safari.yml`,
`agent.yml`), and it merges to main.

**Exit test:** the launch checklist. A fresh VM installs ClipMux in one line
and plays its sample with no build. Every pending real run is recorded. The
LMS has served production on ClipMux through its soak window. Then the
repository goes public.

1. **One-line install.** On a fresh Ubuntu 24.04 and a fresh Debian 12 VM
   with only Docker, `curl -fsSL …/install.sh | sh` pulls the release image
   with no build and prints the setup URL and token. The dashboard's setup
   test passes behind Caddy with real certificates. Re-running the script
   at the next release upgrades in place and keeps the data, keys and
   sessions. `--uninstall` keeps the volumes unless `--purge` is given.
2. **An agent pairs from the dashboard's command.** On Linux the agent
   one-liner installs the tarball with a systemd unit and claims a job. On
   macOS the dashboard's Docker command runs, and its image tag is the
   server's own version. Both survive a reboot.
3. **The release is complete.** A `vX.Y.Z` tag produces:
   - `ghcr.io/visheshgubrani/clipmux:vX.Y.Z` and `:latest` for amd64 and
     arm64, with provenance;
   - agent tarballs for linux amd64 and arm64;
   - `clipmux` binaries for linux, darwin and windows (`push`, `listen`);
   - `SHA256SUMS` and release notes from `CHANGELOG.md`;
   - `@clipmux/player` and `@clipmux/uploader` on npm with provenance.

   `clipmux version` prints the tag.
4. **Every pending run is recorded** as a decision:
   - the real Cloudflare zone, which closes Phase 2 (D041);
   - RunPod and Modal on real accounts, encode and speech (D062, D084);
   - `exit-llm.yml` (D085);
   - Safari and ExoPlayer showing subtitles (D085);
   - the drills (retention, backup and restore, the three rotations, TOTP in
     a browser, a transcription on RunPod);
   - the by-hand checks (D050).
5. **The LMS runs on ClipMux** for the soak window (open question 5), with
   `clipmux-lms` read-only and no rollback taken.
6. **The dashboard passes:**
   - every page's Playwright suite in Chromium and at a 375 px viewport, with
     no horizontal scroll and no CSP violation;
   - every load failing at the API shows an error and a retry, never an
     endless "Loading…";
   - every destructive action asks first in a dialog.
7. **An agent integrates from the skills alone.** The skill evals: a coding
   agent with only the installed skills and a running instance adds upload,
   webhook and signed playback to a sample app, and the video plays. The
   sample is `skills/evals/lms-starter`, and the API key never reaches its
   client code (D112).
8. **The four-step integration works** (Track E, D112):
   - SDK-signed tokens pass every case in the delivery vectors through the
     Worker's verifier; `webhooks.verify` and the Python, Go and PHP
     snippets pass the webhook vectors;
   - every OpenAPI operation is routed, every live SDK response validates
     against the spec, and regenerating the SDK's types produces no diff;
   - in Playwright against `examples/nextjs-lms`: `init` approved through
     `/connect` writes the env file, the uploader element uploads, the
     webhook (and separately `listen`) marks the lesson ready, the player
     plays it signed through `session-url` across a 30 s token refresh, a
     denied viewer gets `clipmux-denied`, `doctor` passes, and no secret
     appears in the CLI's output.
9. **Clean to publish:**
   - a full-history secret scan finds nothing live;
   - the licence files are in place;
   - the image and the tarball ship `licenses/` and a written source offer
     for FFmpeg;
   - a security review of the threat table is recorded.

## Design notes

### Licence (D086)

- **AGPL-3.0** for the repository: the server, the worker and the CLI.
- **MIT** for `packages/player`, `packages/uploader` and `worker/`, each with
  its own `LICENSE` and `"license"` field, so apps embed them freely.
- **FFmpeg** is built `--enable-gpl` with x264 and x265, so it is GPLv2+.
  ClipMux runs it as a separate process, never linked. The image gets the
  tarball's `licenses/` directory and a `SOURCE.md` naming the pinned source
  digests in `toolchain/versions.env` and the build scripts that use them.
- **CONTRIBUTING, CODE_OF_CONDUCT and SECURITY** start from the archived
  `vod` templates (`~/vod`), rewritten for this repository. SECURITY points at
  GitHub private advisories.

### Releases

- **One workflow** (`release.yml`) runs on a `v*` tag, after `ci.yml` and
  the exit suites pass on that commit:
  - the image job's build with `VERSION=vX.Y.Z`, pushed with the tag and
    `:latest`, no longer `sha-` only;
  - `agent.yml`'s tarballs;
  - `go build` for the CLI targets;
  - checksums and GitHub's build provenance attestations;
  - the npm publish.

  Release candidates (`vX.Y.Z-rc.N`) don't move `:latest`; the LMS cutover
  runs on them.
- **The dashboard's agent command pins the server's version**
  (`internal/agents/http.go`, the Docker line). Today it has no tag, so it
  means `:latest`, which is never pushed. A `dev` build keeps `sha-{commit}`.
- **`CHANGELOG.md`** in Keep a Changelog form: one section per release, with
  upgrade notes such as migrations and new settings.

### One-line install

- **`deploy/install.sh`**, POSIX sh and shellcheck-clean, served from the
  release assets (and later the docs site). It:
  - checks Docker and Compose v2, the architecture, free disk and ports 80
    and 443;
  - writes `/opt/clipmux/compose.yml` (an operator copy of
    `deploy/compose.yml` that uses `image:` at the release tag; dev keeps
    `--build`) and `.env`, generating `POSTGRES_PASSWORD` and
    `CLIPMUX_SECRETS_KEY` with `openssl rand` or `/dev/urandom`;
  - runs `docker compose up -d --wait` and prints the setup URL, the token
    from the logs, and the secrets key with "store this off the host".

  Re-run, it upgrades: it keeps `.env`, moves the tag and pulls.
  `--uninstall` stops the stack and keeps the volumes unless `--purge`. It
  never pipes a second download into a shell, and it checks the downloaded
  compose file against `SHA256SUMS`.
- **`deploy/install-agent.sh`**, for Linux. It downloads the release tarball
  for the machine's architecture, checks it against `SHA256SUMS`, unpacks it
  to `/opt/clipmux-agent` (or `~/.local` without root), and writes a systemd
  unit (a user unit without root). Then it pairs with
  `--server URL --pair CODE`. The dashboard shows this one-liner next to the
  Docker command. macOS and Windows stay on Docker (the native sandbox is an
  open decision).

### Dashboard

- **Design first.** A written spec and a mock come before code. The spec
  covers:
  - the tokens (colour, type, spacing and radius, light and dark);
  - the brand (logo, wordmark, favicon);
  - the navigation shape, tables on phones, empty states, and the first-run
    flow.

  The owner signs the spec off before T11.
- **Implementation stays on what's there:** Tailwind v4, with the tokens as
  CSS variables in `index.css`, and the shadcn-style kit in `components/ui/`.
  New components follow the same pattern: a toast, a confirmation dialog, a
  skeleton and an error boundary. Icons come from one icon set. Every new
  dependency is checked against the CSP (D065).
- **No private path.** Every page uses the public API with the owner's
  session. The routes the pages need and don't have are T12's.

### Agent skills

- **`skills/`** at the repository root, in the `SKILL.md` format that
  `npx skills add` installs:
  - `clipmux-integrate`, for an app's backend: create an upload with
    `passthrough`, verify the webhook HMAC, re-fetch the asset, sign ES256
    playback tokens, find by passthrough, captions and chapters;
  - `clipmux-player`, for its frontend: `@clipmux/uploader`, and
    `<clipmux-player>` with `getToken()` refresh and `chapters-track`.

  They point at the docs site rather than copying it, so they stay short and
  current.
- **Evaluated with skill-creator.** Each skill has evals against a sample app
  and a local instance, and the evals run when the API or the packages
  change.

## Tasks

Order:

- **Track A** (T01–T05) starts at once: its secrets, accounts, zone and
  hardware are the long pole.
- **Track B:** T06 first (the licence unblocks publishing), then T07, then
  T08 and T09.
- **Track C:** T10, then T11 and T12 alongside, then T13, then T14. Done (D098, D099, D101); Analytics joined it (D100), with its API in Phase 7 (D102).
- **Track D:** T15–T18 alongside B and C. T16 and T17 after T09, so that
  examples name the published versions.
- **Track E** (D112): T21 first; T22 after T23; T23 and T27 alongside;
  T24 after T22; T25 after T24; T26 after T23 and T24; T28 after T25–T27.
  T16 and T17 (rescoped) after T28.
- **T19** after T02, Track E, a release candidate from T07 and T08, and the Phase 7
  exit ([P7-sharing-analytics.md](P7-sharing-analytics.md), D102), so the
  candidate carries share links and analytics. D114 drops the release
  candidate and Track E from this: T19 runs on a `sha-` image from `main`.
- **T20** last.

| Task | Scope | Port from |
| --- | --- | --- |
| T01 | **done (D095, D097), run locally; CI secrets not set** — **Real accounts.** Add the secrets `RUNPOD_API_KEY`, `MODAL_TOKEN_ID`, `MODAL_TOKEN_SECRET`, `GHCR_PULL_TOKEN`, `ANTHROPIC_API_KEY` and `OPENAI_API_KEY`. Run `exit-providers.yml` (encode and speech on both providers, cold starts against the 10-minute claim deadline, digest-only images, every machine stopped) and `exit-llm.yml`. Measure whisper on Modal's L4 (P5 open question 6). Record the Phase 4 exit. | D062, D077, D084, D085 |
| T02 | **done (D090)** — **A real Cloudflare zone** with Workers Paid. Connect Cloudflare (the plan check, the script upload, the custom domain); the setup test and the Chromium suites through the Worker hostname; a revoked ID's track at the edge within 65 s; a move to R2 that plays its subtitles; content-key rotation redeploying the ring. Read Cloudflare's terms again for video on Workers and R2. Record the Phase 2 exit. | D037, D041, D081 |
| T03 | **Devices.** Extend `exit-safari.yml` (native HLS) and `exit.yml`'s ExoPlayer job to show the subtitles track with a cue at its time. This settles P5 open question 8; the fallback is 6 s segments sliced at delivery. | D085 |
| T04 | **done (D094)** — **Drills, automated on the real binary:** retention after backdating; `pg_dump` and data restore with the same secrets key; secrets-key, content-key and signing-key rotation; TOTP enrolment, sign-in and a recovery code in Playwright; a transcription routed to RunPod with `primary_offline`. Each joins `exit.yml`. | D069–D071, D078 |
| T05 | **By hand, recorded:** the M4 Mac's lid-close suspend with the Docker agent; the self-hosted GPU runner registered for `gpu.yml`; English, Hindi and Hinglish samples recorded and detected (P5 checks 12 and T14). | D050, D077 |
| T06 | **done (D091)** — **Licence, history and community.** `LICENSE` files as D086; `"license"` in each `package.json`; `licenses/` and `SOURCE.md` in the image; CONTRIBUTING (with the DCO or CLA choice, open question 1), CODE_OF_CONDUCT, SECURITY. Scan the whole history with gitleaks and trufflehog; anything live is rotated first, then scrubbed, then verified with `git grep` (AGENTS.md). Run a security review of the threat table and record it. | `~/vod` templates, D005 |
| T07 | **`v1.0.0-rc.1` released (D136); installs on Ubuntu 24.04, Debian 12 and the clipmux.com redirect still to do** — **Releases.** `release.yml` on `v*` tags; the image pushed as `vX.Y.Z` and `:latest` (not for `-rc`) with attestations; tarballs, CLI binaries and `SHA256SUMS` on the GitHub Release; `CHANGELOG.md`; the agent's Docker command pinned to the server's version. Tests: the command for a `v1.2.3` build names `:v1.2.3`; a `dev` build names `sha-{commit}`; the workflow runs end to end on a `-rc` tag in this repository. | `ci.yml` image job, `agent.yml`, D058, D061 |
| T08 | **done (D093)**: passed end to end on this machine; `install.yml` runs on GitHub with the next pull request touching the installers, and on a real `-rc` after T07 — **One-line installs.** `deploy/install.sh` and `deploy/install-agent.sh` as designed; an operator compose using `image:`; the agent one-liner on the Workers page. Tests: shellcheck; a CI job on a clean runner that installs a release candidate, passes the setup test, re-runs to upgrade to a second candidate, and uninstalls; the agent script pairing against it. | `deploy/compose.yml`, `toolchain/pack_agent.sh` |
| T09 | **done (D092)**: publishes on the next `v*` tag; then deprecate 0.1.x — **npm.** Drop `"private"` from both packages, a publish step in `release.yml` with provenance, READMEs that show the backend half too, `npm deprecate @clipmux/*@0.1.x`. | D006, D029, D084 |
| T10 | **done (D098)** — **Dashboard design spec and mock**, signed off by the owner: the tokens, the brand, navigation, tables on phones, empty, loading and error states, the first-run flow with next-step cards. | architecture "First boot", "Optional upgrades" |
| T11 | **done (D101)** — **The design layer:** tokens and dark mode in `index.css`; the brand; icons; responsive layouts; toast, confirmation dialog, skeleton and error boundary in `components/ui/`; every load with a `catch` and a retry (Library, Apps and the app-level `refresh` hang today); every native `confirm()` replaced, and revoking keys and webhooks and disabling an app asking first. | `dashboard/src/components/ui/` |
| T12 | **done (D099)** — **Owner routes the pages need:** change password; library search by passthrough and title, with deleted assets filtered on the server (today a page can say "No videos yet." while "More" still shows); disconnect Cloudflare, back to built-in delivery; remove a storage config and rotate its credentials; API key names. Tests on real Postgres; `docs/api.md` updated. | `internal/accounts`, `internal/stores`, known gaps |
| T13 | **done (D101)**, with an Analytics page whose API is Phase 7 (D100, D102) — **Pages:** next-step cards after the setup test; a Settings page (URLs out of `/setup`, password); the asset page's delete and playback IDs (add, revoke, public or signed) and the library's thumbnails and search; upload choosing the app and the policy; webhooks with the deliveries log, redeliver and two-step secret rotation (D069); Jobs filter controls; Disconnect Cloudflare; storage configs. | known-gaps "Bootstrap" |
| T14 | **done (D101)** — **Playwright:** Apps, signing keys, webhooks, TOTP, Delivery, Upload and `/workers/connect` (none today), and every suite again at 375 px. | `e2e/web/tests` |
| T15 | **partial**: the README is done, with screenshots from a demo instance; the docs site and landing page are planned in [P6-T15-site.md](P6-T15-site.md) — **README and the docs site.** A README that says what ClipMux is, with screenshots, the one-liner and links. A docs site (open question 3) with `llms.txt`, from the structure of `~/vod/docs-site`. | `~/vod/docs-site` |
| T16 | **Integrator docs** (rescoped, D112): the quickstart is the four steps (`init`, one route file, two elements, three callbacks); token signing and webhook verification snippets in Python, Go and PHP under `docs/snippets/`, each tested in CI against both vector files; `docs/api.md` with a request and a response for every route; `llms.txt` and `llms-full.txt` from the docs and the spec (with T15). | `docs/api.md`, D015, D036 |
| T17 | **Agent skills** `clipmux-integrate` and `clipmux-player` (rescoped, D112): the SDK path, `doctor`, and each error code with what to do; skill-creator evals against `skills/evals/lms-starter` (exit check 7). | — |
| T18 | **Operator docs** for the release image and the installers: install, upgrade between releases, uninstall, the agent one-liner, and `docs/agents.md` without "until launch". | `docs/operations.md`, `docs/agents.md` |
| T19 | **Rescoped (D114)**: the legacy `clipmux-lms` is replaced outright on the shared VPS (no import or soak), behind the host's Caddy, with Modal encoding and an amd64+arm64 `sha-` image; the LMS side is Edural's P36. Original scope: **The LMS cutover**, as `lms-migration.md`'s sketch: install the LMS's instance with the one-liner on a release candidate; create its App; add the second adapter in the LMS; import the library (open questions 4 and 6); switch playback, with the LMS signing `vid` so unique viewers count (P7 open question 1); keep `clipmux-lms` read-only through the soak window; retire it, and move the `clipmux.com` hostnames (open question 5). | `lms-migration.md` |
| T21 | **done (D112)** — D112; AGENTS.md's package list and `examples/`; the webhook vectors `internal/webhooks/testdata/vectors.json`. | — |
| T22 | **done (D112)** — **OpenAPI spec** `internal/openapi/openapi.json` (OpenAPI 3.1; JSON, so the standard library reads it): the integration API, delivery URLs, `GET /v1/app`, the public app-pairing routes, `webhooks:`, and each operation's error codes. Embedded and served at `GET /openapi.json` without auth. Go test: every spec operation is routed (401/403 unauthenticated, never 404/405), and every row of `docs/api.md`'s Integration table is in the spec. | `docs/api.md` |
| T23 | **done (D113)** — **Server for the SDK:** `GET /v1/app` (any API key: app, scopes, URLs, the maximum token lifetime, signing kids with public keys); the credentials bundle (an API key, a signing key, an optional webhook endpoint, one transaction, D108's loopback rule) through `POST /v1/apps/{id}/credentials`, `clipmux admin create-integration` and app pairing (`/v1/app-pairings`, a device flow like agent pairing, sealed until one collecting poll, 10 minutes, D067's limits); the dashboard's `/connect` page and "Connect your app" panel. Tests on real Postgres and in Playwright. | `internal/agents` pairing |
| T24 | **done (D112)** — **`@clipmux/node` core:** env-configured client; the resources; `playback.token`, `urls` and `session` (native lifetime); `webhooks.verify`; automatic idempotency keys and retries; `ClipMuxError` with hints; types generated from the spec and checked in CI. Tests against both vector files and the Worker's verifier. | `internal/push` client rules |
| T25 | **done (D112)** — **Handlers:** `clipmuxHandler` (`upload`, `playback`, `webhook`; default-deny; JSON only; re-fetch and version order) and `@clipmux/node/express`. | — |
| T26 | **done (D112)** — **CLI:** `init` (app pairing, env-file merge, gitignore warning, framework next steps, `--no-open`), `doctor`, `listen` (D048, re-signing with the app's secret), `upload`. No secret is printed. | `internal/listen`, `internal/push` |
| T27 | **done** — **Browser packages:** player 0.5.0 (`session-url`, native flag, typed events, React 19 JSX types) and uploader 0.4.0 (`createUrl` with built-in resume, `<clipmux-uploader>`). Backward compatible. | `packages/*` |
| T28 | **partial**: `release.yml` publishes `packages/node` (workspace ranges pinned) and the package ships `AGENTS.md`; the examples and the exit run are open — **Examples and the Track E exit:** `examples/nextjs-lms` and `examples/express`; the exit check 8 Playwright run; `release.yml` publishes `packages/node`; the package ships an `AGENTS.md`. | `e2e/web` fixture |
| T20 | **Exit and launch day.** The exit checks above, recorded as a decision. Then make the repository and the ghcr package public, tag `v1.0.0`, put the docs site live, unarchive `vod` to add a banner linking here and archive it again, and announce. | — |

## Open questions

Each answer becomes a decision record before the task that needs it.

1. **DCO or CLA?** (T06) A CLA keeps dual licensing possible later; a DCO
   (`Signed-off-by`) is lighter for contributors. Recommendation: DCO, unless
   dual licensing is planned.
2. **Which languages get token-signing examples?** (T16; architecture's open
   decision) **Answered (D112):** a TypeScript SDK, `@clipmux/node`, and
   tested snippets for Python, Go and PHP.
3. **Where does the docs site live, and on what?** (T15) AGENTS.md allows
   TypeScript only in the dashboard, the packages and the Worker, and
   `vod`'s site is Next.js. Recommendation: its own repository
   (`visheshgubrani/clipmux-docs`), served from `clipmux.com` once the LMS
   frees it, with `llms.txt` generated from the same Markdown.
   **Answered (2026-11-18):** `site/` in this repository, Next.js with
   Fumadocs, serving the landing page and the docs on `clipmux.com`; see
   [P6-T15-site.md](P6-T15-site.md). Recorded as D133.
4. **The LMS library's subtitles:** regenerate them with whisper, or build
   `source: uploaded` tracks and import the existing VTTs? (T19; P5 open
   question 10)
5. **The soak window, and the hostnames.** (T19) How long the LMS runs on
   ClipMux before launch (recommendation: 14 days), and whether the new
   instance takes over `api.clipmux.com` and `media.clipmux.com`.
6. **Is the per-key write limit (2 rps, burst 60) enough for the library
   import?** (T19; P5 open question 5) It needs the import's peak rate; a
   per-key override can come with it.
7. **The architecture's defaults:** a 7-day maximum token lifetime, retry
   budgets of 5 and 3, a 15-minute reconciler, and per-output keys derived
   from one secret. Confirm them before v1 fixes them in docs. (T16)
8. **Image signing beyond provenance?** (T07) Recommendation: GitHub's
   attestations are enough for v1; cosign can come later.
