# AGENTS.md

Guidelines for agents working on **ClipMux**, an open-source self-hosted VOD
platform being rebuilt from scratch. Read [docs/architecture.md](docs/architecture.md)
first; it is the specification. [docs/decisions.md](docs/decisions.md) records
anything decided since, and wins over the architecture doc where they differ.

## Repository

- Go module `github.com/visheshgubrani/clipmux`, one binary in `cmd/clipmux`
  (`serve`, `worker`, `push`, `listen`, `admin`).
- `internal/<package>`: each package's `doc.go` says what it owns and which
  Phase 1 task lands it. Adapter boundaries are `internal/storage`,
  `internal/executor` and `internal/delivery` — core logic never branches on a
  vendor; if it needs to, the interface is leaking.
- `web/`: dashboard SPA embedded with `go:embed`. It is an API client with no
  private path into the server (same for the CLI).
- `toolchain/versions.env` is the only place a media tool version, URL or digest
  is written. Shaka Packager must be >= 3.9.1.
- TypeScript lives only in the dashboard, the player/uploader packages and the
  Cloudflare Worker (Phase 2). The Worker and the Go delivery handler share JSON
  test vectors; change both together.

## Commands

```bash
make db-up / make db-down     # dev Postgres on :5444 (deploy/compose.dev.yml)
make test                     # go vet ./... && go test ./...
make test-db                  # same, with TEST_DATABASE_URL pointing at dev Postgres
make run                      # clipmux serve
gofmt -l .                    # must print nothing
```

## How we work

- Phases and tasks live in `docs/plans/`. Phase 1 has a walking-skeleton
  milestone (M1) before the rest; each phase starts only after the previous
  exit test passes.
- TDD at the seams: red → green vertical slices, one test per slice, expected
  values from literals or worked examples, never re-derived from the code.
- Database behaviour is asserted against real Postgres (`TEST_DATABASE_URL`),
  never with SQL-substring checks. Suites skip cleanly without it.
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
