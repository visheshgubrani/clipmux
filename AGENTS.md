# AGENTS.md

Guidelines for agents working on **ClipMux**, an open-source self-hosted VOD
platform being rebuilt from scratch. Read [docs/architecture.md](docs/architecture.md)
first; it is the specification. [docs/decisions.md](docs/decisions.md) records
anything decided since, and wins over the architecture doc where they differ.

## Repository

- Go module `github.com/visheshgubrani/clipmux`, one binary in `cmd/clipmux`
  (`serve`, `worker`, `push`, `listen`, `admin`).
- Layout follows go.dev/doc/modules/layout for a server: commands in `cmd/`,
  everything else in `internal/`. Nothing is importable from outside.
- Current packages: `config`, `db` (migrations, sqlc-generated queries and
  models, `dbtest`), `ids`, `apikeys`, `agents`, `assets`, `uploads`, `queue`,
  `workerapi`, `worker`, `outputs`, `delivery`, `tokens`, `media`, `contentkey`,
  `storage` (filesystem and S3 adapters, `s3test`), `stores`, `routes`, `outbox`, `push`, `listen`,
  `dispatch` (serverless dispatches, keys, reaper), `runpod` (`runpodtest`), `modal`, `registry` (`registrytest`),
  `httpjson`, `server`, `sweeper`, `web` (embedded dashboard build), `openapi`
  (the integration API spec, D112).
- Media tests need ffmpeg, ffprobe and Shaka Packager ≥ 3.9.1
  (`toolchain/install_packager.sh`); they skip without them, and the lean PR
  CI doesn't install them, so run media and whisper.cpp tests locally (or set
  `CLIPMUX_REQUIRE_MEDIA_TOOLS=1` to make a missing tool a failure). What runs
  where is in `.github/CI.md`.
- Adapter boundaries (storage, executor, delivery) keep core logic from
  branching on a vendor; if it needs to, the interface is leaking.
- The dashboard and CLI are API clients with no private path into the server.
- `internal/delivery/testdata/vectors.json` is the delivery contract shared with
  the Phase 2 Cloudflare Worker. Change behaviour there first; regenerate test
  tokens with `go test ./internal/delivery -run TestVectors -update`.
- `internal/webhooks/testdata/vectors.json` is the webhook-signature contract
  for the SDK and the language snippets; regenerate with
  `go test ./internal/webhooks -run TestVectors -update` (D112).
- `skills/` holds the agent skills (`clipmux-integrate`, `clipmux-player`,
  P6-T17) that `npx skills add visheshgubrani/clipmux` installs. They link to
  clipmux.com pages by pinned anchors (`## Heading [#id]`); `make site-check`
  fails on an unpinned or missing one, and on an API error code missing from
  `skills/clipmux-integrate/references/errors.md`. Their eval is
  `skills/evals` (launch exit check 7).
- `toolchain/versions.env` is the only place a media tool version, URL or digest
  is written. Shaka Packager must be >= 3.9.1.
- TypeScript lives only in the dashboard, the packages (`packages/player`,
  `packages/uploader`, `packages/node`, the server SDK), the Cloudflare Worker
  (`worker/`), the example apps (`examples/`) and the site (`site/`). The Worker and the Go delivery handler share JSON
  test vectors; change both together.
- `site/` is clipmux.com (D133): the landing page and the docs, Next.js with
  Fumadocs, built as static files. User-facing docs move into
  `site/content/docs` as P6-T15.4 and T15.5 port them; `docs/` keeps the internal records (architecture,
  decisions, plans, design, known gaps). A change in behaviour updates its
  docs page in the same pull request. Code that pages show lives in
  `site/snippets/` and runs in tests: Python, Go and PHP against the vector
  files (`make snippets-test`), TypeScript in the site's typecheck. The
  management API page is kept complete by `internal/manage/docs_test.go`, the
  CLI page by `cmd/clipmux/docs_test.go`, the settings page by
  `internal/config/doc_test.go`. The plan is
  [docs/plans/P6-T15-site.md](docs/plans/P6-T15-site.md).

## Commands

```bash
make db-up / make db-down     # dev Postgres on :5444 (deploy/compose.dev.yml)
make test                     # go vet ./... && go test ./...
make test-db                  # same, with TEST_DATABASE_URL pointing at dev Postgres
make run                      # clipmux serve (migrates the dev database on boot)
make lint                     # gofmt, vet, staticcheck -checks all, govulncheck
make sqlc / make sqlc-check   # regenerate / verify the generated code in internal/db (runs sqlc in Docker)
make site / make site-check   # clipmux.com dev server on :8100 / typecheck, build, docs lints, Biome
make snippets-test            # the docs' Python, Go and PHP snippets against both vector files
gofmt -l .                    # must print nothing
```

## Keeping it lean (go.dev guidance we follow)

- **No package before its code.** A package is created by the task that first
  needs it, and named for what it contains — never `util`, `common`, `types`,
  `api` grab-bags. No placeholder packages or speculative interfaces.
- **Interfaces belong to the consumer** and appear only when there is a second
  implementation or a real seam (e.g. `server.Pinger`, `workerapi.WorkerAuth`).
  Return concrete types. The storage adapters are the deliberate exception:
  one interface, several backends.
- **Generated code lives in the package that owns it** (`internal/db`), not in a
  package named after the generator.
- **Every goroutine has an owner that waits for it**; `serve` returns only
  after its loops stop, and requests inherit the server context.
- **Context first, errors last, errors wrapped with `%w` and the operation**,
  no ignored errors except where the API documents it cannot fail.
- **Use the standard library first** (`crypto/rand.Text` for secrets,
  `net/http` routing, `log/slog`); add a dependency only for a real gap.
- **Duplicate once before abstracting**: extract a helper on the second real use.
- `make lint` must pass: gofmt, vet, staticcheck with all checks, govulncheck.

## How we work

- Phases and tasks live in `docs/plans/`. Phase 1 has a walking-skeleton
  milestone (M1) before the rest; each phase starts only after the previous
  exit test passes.
- TDD at the seams: red → green vertical slices, one test per slice, expected
  values from literals or worked examples, never re-derived from the code.
- Database behaviour is asserted against real Postgres (`TEST_DATABASE_URL`),
  never with SQL-substring checks. Use `dbtest.New(t)`: each test gets its own
  migrated schema and skips cleanly without the variable. Name such files
  `*_db_test.go`.
- Schema changes are new goose migrations in `internal/db/migrations` (never
  edit an applied one) with a working Down. Queries go in `internal/db/queries`
  and are generated with `make sqlc`.
- Invariants from the architecture that must never be weakened: executors hold
  no storage credentials; outputs are immutable and only `current_output_id`
  moves; one mutating job per asset; side effects outside Postgres run from
  outbox rows; route records default-deny; worker file names are untrusted
  (path grammar on sign, playlist check at commit, extension allowlist at
  delivery); ffmpeg runs sandboxed.
- `docs/known-gaps.md` is the standing record of deliberate gaps and deferred
  work — add limitations there, not in ad-hoc handoff notes.

## Secrets and hygiene

- Never commit `.env*` except `.env.example`. Never log Authorization headers,
  tokens, presigned URLs, content keys or secrets.
- Adapter secrets, the internal signing key and the content key secret are
  encrypted in Postgres with a key held outside it.
- This product's predecessor leaked an R2 key into git history; the precedent is
  rotate first, then scrub, then verify with `git grep`.

## Relationship to other repos

- `visheshgubrani/clipmux-lms` (private) is the previous TypeScript ClipMux that
  runs the LMS in production. It is reference material only — port behaviour and
  tests from it, never code paths that assume its data model. The LMS migrates
  here at parity; see `docs/plans/lms-migration.md`.
- `visheshgubrani/vod` (public, archived) is the earlier open-source attempt.
