# ClipMux

Open-source, self-hosted video infrastructure — an alternative to Mux and
Cloudflare Stream. Any app uploads a video, gets a playback ID, and streams it as
encrypted HLS from infrastructure the owner controls.

> **Status: early rebuild, private.** This repository is a from-scratch rebuild of
> ClipMux on the architecture in [docs/architecture.md](docs/architecture.md)
> (one Go binary, a leased Postgres job queue, immutable encrypted outputs,
> filesystem-first storage). Nothing here is usable yet; work follows
> [docs/plans/P1-core-product.md](docs/plans/P1-core-product.md).

## Develop

Requirements: Go 1.27+, Docker with Compose.

```bash
make db-up        # Postgres 18 on localhost:5444
make test         # go vet + go test
make run          # clipmux serve on :8080 (prints a setup token)
```

Media tools (needed from P1-T06): FFmpeg from the pinned recipe in
[toolchain/](toolchain/) and Shaka Packager via `toolchain/install_packager.sh`.

## Layout

```
cmd/clipmux/       the single binary: serve | worker | push | listen | admin
internal/          config, server, auth, db, queue, api, workerapi, worker,
                   media, storage, executor, delivery, tokens, outbox, sweeper
web/               dashboard (embedded; placeholder until P1-T10)
toolchain/         pinned FFmpeg + Shaka Packager recipe (versions.env is the source of truth)
deploy/            compose files
docs/              architecture, decisions, plans, known gaps
```
