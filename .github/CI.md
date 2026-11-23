# What CI runs, and when

The repository is public, so CI stays simple and safe for fork pull
requests: one workflow on every pull request and every push to main, with a
read-only token and no secrets. The release workflows run on a `v*` tag;
everything heavier runs only by hand. Nothing runs on a schedule, and no
workflow uses a self-hosted runner.

| Workflow | Runs on | When |
| --- | --- | --- |
| `ci.yml`: gofmt, vet and Go tests against Postgres (`go`); TypeScript typecheck and tests, the clipmux.com build with its docs lints, and Biome (`web`); the docs' Python, Go and PHP snippets against the vector files (`snippets`); `docker build --check` on the Dockerfile (`dockerfile`) | hosted Linux | every pull request and push to main; by hand |
| `image.yml`: the image under compose, the setup test, then pushes each architecture by digest and joins them as `sha-{commit}` (or the tag and `sha-{commit}`, from `release.yml`) with attestations, and reports which layers are new since the last release | hosted Linux | by hand (pushes from main); called by `release.yml` |
| `agent.yml`: the amd64 and arm64 agent tarballs | hosted Linux | by hand; called by `release.yml` |
| `exit.yml`: the Phase 1–5 exit tests, Chromium and ExoPlayer, and launch exit check 8 against `examples/nextjs-lms` | hosted Linux | by hand (`soak` for 30 minutes), before a release |
| `exit-safari.yml`: Safari | hosted macOS | by hand, before a release |
| `toolchain/check_restore.sh` (not a workflow): the disaster-recovery drill on a fresh VM, timed per phase, with a throwaway bucket; `--env FILE --yes` restores a saved `.env` of a real install instead (T29.12) | a fresh VM, as root | by hand, before a release |
| `install.yml`: the installers end to end on two local release candidates, which must share every layer but the binary's | hosted Linux | by hand |
| `release.yml`: waits for `ci.yml` (and `exit.yml`, unless a `-rc`) on the commit, then the image, the agent tarballs and `clipmux` binaries, the GitHub Release with `SHA256SUMS` and notes from `CHANGELOG.md`, and npm publish of the `@clipmux` packages with provenance (D136) | hosted Linux | `v*` tags; by hand only the npm job (a dry run by default) |
| `exit-llm.yml`: real OpenAI and Claude | hosted Linux | by hand, only while `REAL_LLM` is `true` |
| `exit-providers.yml`: real RunPod and Modal | hosted Linux | by hand, only while `REAL_PROVIDERS` is `true` |

`ci.yml` installs the PostgreSQL 18 client tools, because the backup and restore tests run
`pg_dump` and `pg_restore` (the runner's own are 16, which refuse a newer server). It doesn't
install ffmpeg, the packager or whisper.cpp, so the media
and speech tests skip there, and it skips the race detector, the S3 contract
suite, `make lint` (staticcheck, govulncheck), `make sqlc-check`, gitleaks and
shellcheck. Run those locally before merging anything that touches them:

```bash
make lint && make sqlc-check && make test-db
```

The image builds use a registry cache per architecture,
`ghcr.io/visheshgubrani/clipmux-buildcache:{amd64,arm64}`, not the Actions
cache (D142): it has no size cap and doesn't expire, so an unchanged tool is
never rebuilt and a release reuses the previous one's layers. `image.yml` on
main or a tag writes it; every other build only reads it. After a change to
the `Dockerfile` or `toolchain/`, run `install.yml` on the branch: it fails if
two builds that differ only in `VERSION` don't share every layer below the
binary (`toolchain/check_layers.sh`).

Run a heavier suite on a branch when the change needs it:

```bash
gh workflow run exit.yml --ref BRANCH
```
