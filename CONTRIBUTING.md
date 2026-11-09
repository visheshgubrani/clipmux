# Contributing to ClipMux

Thanks for helping. ClipMux is self-hosted video infrastructure: one Go binary
(`cmd/clipmux`) with a leased Postgres job queue, immutable encrypted outputs
and filesystem-first storage. Read [docs/architecture.md](docs/architecture.md)
first; it is the specification, and [docs/decisions.md](docs/decisions.md)
records everything decided since. [AGENTS.md](AGENTS.md) holds the working
rules, for people and coding agents alike.

By taking part you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).
Security problems go to a private report, never an issue
([SECURITY.md](SECURITY.md)).

## Licence and sign-off

- The repository is **AGPL-3.0** ([LICENSE](LICENSE)). `packages/player`,
  `packages/uploader` and `worker/` are **MIT**, each with its own `LICENSE`,
  so apps embed them freely (D086).
- Contributions are accepted under the licence of the directory they touch,
  certified with the [Developer Certificate of Origin](https://developercertificate.org/)
  (DCO). There is no CLA. Sign off every commit:

  ```bash
  git commit -s -m "Explain the change"
  ```

  This adds `Signed-off-by: Your Name <you@example.com>`, which certifies
  that you wrote the change or have the right to submit it under the
  project's licence. A pull request whose commits aren't all signed off
  can't be merged; `git rebase --signoff main` fixes an existing branch.

## Set up

Requirements: Go (the version in `go.mod`), Docker with Compose, Node 24 and
pnpm (`corepack enable`).

```bash
make db-up        # dev Postgres on :5444
make test         # go vet ./... && go test ./...
make test-db      # the same, with the *_db_test.go tests against dev Postgres
make lint         # gofmt, vet, staticcheck -checks all, govulncheck
make run          # clipmux serve; it prints a setup token for the dashboard
pnpm install && pnpm -r run build   # dashboard, player, uploader, Worker
```

The media tests need FFmpeg, ffprobe and Shaka Packager ≥ 3.9.1
(`toolchain/install_packager.sh`); they skip without them. The real
whisper.cpp tests need `toolchain/build_whisper.sh` and its models
(`toolchain/fetch_speech_models.sh`). [.github/README.md](.github/README.md)
says which suites run where in CI.

## How changes are made

- **Tests first, at the seams.** A failing test, then the code that passes
  it, one vertical slice at a time. Expected values come from literals or
  worked examples, never from the code under test.
- **Database behaviour is tested against real Postgres** with
  `dbtest.New(t)`, in files named `*_db_test.go`; never with SQL-substring
  checks.
- **Schema changes are new goose migrations** in `internal/db/migrations`,
  each with a working Down. Never edit an applied migration. Queries live in
  `internal/db/queries` and are generated with `make sqlc`.
- **Keep it lean:** no package before its code, interfaces belong to their
  consumer, the standard library first, and duplicate once before
  abstracting. AGENTS.md has the rest.
- **The invariants are not negotiable:** executors hold no storage
  credentials; outputs are immutable and only `current_output_id` moves; one
  mutating job per asset; side effects outside Postgres run from outbox rows;
  route records default-deny; worker file names are untrusted; FFmpeg runs
  sandboxed.
- **Never commit secrets.** No `.env*` except `.env.example`; never log
  Authorization headers, tokens, presigned URLs, content keys or secrets.
- **The delivery contract** is `internal/delivery/testdata/vectors.json`,
  shared by the Go handler and the Cloudflare Worker. Change it first, and
  both sides together.
- **Media tool versions** are written only in `toolchain/versions.env`.

## Pull requests

- One change per pull request, with the decision it rests on (a `D0xx`
  entry, or a new one in `docs/decisions.md` when it decides something).
- `make lint` and `make test-db` pass locally; for TypeScript, `pnpm lint`,
  `pnpm typecheck` and `pnpm test`.
- A change to playback, delivery, the player or the dashboard also passes
  the exit suites (`exit.yml`), which a maintainer runs by hand.
- A limitation you introduce or find goes in
  [docs/known-gaps.md](docs/known-gaps.md).
