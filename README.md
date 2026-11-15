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
make lint         # gofmt, vet, staticcheck (all checks), govulncheck
make run          # clipmux serve on :8080 (prints a setup token)
```

Open the dashboard, create an app under **Apps & API**, and create its API
key there. While the public URL is `localhost`, the dialog asks you to
confirm the key is for apps on this computer (D108). The same can be done
on the host:

```bash
export DATABASE_URL=postgres://clipmux:clipmux@localhost:5444/clipmux?sslmode=disable
go run ./cmd/clipmux admin create-key --app MyApp --allow-loopback  # API key for uploads and reads
go run ./cmd/clipmux admin create-signing-key --app MyApp           # ES256 key your backend signs playback tokens with
```

Playback URLs are `{base_url}/v/{token}/{playback_id}/master.m3u8` (signed) and
`{base_url}/p/{playback_id}/master.m3u8` (public); tokens are ES256 JWTs with
`sub` = playback ID, `aud` = `v` (or `t` for posters), `iat`, `exp` and the
`kid` from `create-signing-key`.

Media tools (needed from P1-T06): FFmpeg from the pinned recipe in
[toolchain/](toolchain/) and Shaka Packager via `toolchain/install_packager.sh`.

## Layout

```
cmd/clipmux/       the single binary: serve | worker | push | listen | admin
internal/          one package per job; a package is created by the task that
                   first needs it (see AGENTS.md)
internal/web/      embedded dashboard build (placeholder until P1-T10)
toolchain/         pinned FFmpeg + Shaka Packager recipe (versions.env is the source of truth)
deploy/            compose files
docs/              architecture, decisions, plans, known gaps; integration.md for app developers
```
