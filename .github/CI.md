# What CI runs, and when

The repository is public, so CI stays simple and safe for fork pull
requests: one workflow on every pull request and every push to main, with a
read-only token and no secrets. The release workflows run on a `v*` tag;
everything heavier runs only by hand. Nothing runs on a schedule, and no
workflow uses a self-hosted runner.

| Workflow | Runs on | When |
| --- | --- | --- |
| `ci.yml`: gofmt, vet and Go tests against Postgres (`go`); TypeScript typecheck and tests, the clipmux.com build with its docs lints, and Biome (`web`); the docs' Python, Go and PHP snippets against the vector files (`snippets`) | hosted Linux | every pull request and push to main; by hand |
| `image.yml`: the image under compose, the setup test, pushes `sha-{commit}` (or the tag, from `release.yml`) with attestations | hosted Linux | by hand (pushes from main); called by `release.yml` |
| `agent.yml`: the amd64 and arm64 agent tarballs | hosted Linux | by hand; called by `release.yml` |
| `exit.yml`: the Phase 1–5 exit tests, Chromium and ExoPlayer, and launch exit check 8 against `examples/nextjs-lms` | hosted Linux | by hand (`soak` for 30 minutes), before a release |
| `exit-safari.yml`: Safari | hosted macOS | by hand, before a release |
| `install.yml`: the installers end to end on two local release candidates | hosted Linux | by hand |
| `release.yml`: waits for `ci.yml` (and `exit.yml`, unless a `-rc`) on the commit, then the image, the agent tarballs and `clipmux` binaries, the GitHub Release with `SHA256SUMS` and notes from `CHANGELOG.md`, and npm publish of the `@clipmux` packages with provenance (D136) | hosted Linux | `v*` tags; by hand only the npm job (a dry run by default) |
| `exit-llm.yml`: real OpenAI and Claude | hosted Linux | by hand, only while `REAL_LLM` is `true` |
| `exit-providers.yml`: real RunPod and Modal | hosted Linux | by hand, only while `REAL_PROVIDERS` is `true` |

`ci.yml` doesn't install ffmpeg, the packager or whisper.cpp, so the media
and speech tests skip there, and it skips the race detector, the S3 contract
suite, `make lint` (staticcheck, govulncheck), `make sqlc-check`, gitleaks and
shellcheck. Run those locally before merging anything that touches them:

```bash
make lint && make sqlc-check && make test-db
```

Run a heavier suite on a branch when the change needs it:

```bash
gh workflow run exit.yml --ref BRANCH
```
