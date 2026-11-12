# What CI runs, and when

Hosted Actions minutes are metered, so CI is deliberately lean: one job on
every pull request, everything else by hand or on a release tag. There is no
GPU workflow and nothing runs on a schedule.

| Workflow | Runs on | When |
| --- | --- | --- |
| `ci.yml`: gofmt, vet, Go tests against Postgres, TypeScript typecheck and tests | hosted Linux | every pull request, by hand |
| `image.yml`: the image under compose, the setup test, pushes `sha-{commit}` | hosted Linux | by hand (pushes from main); `v*` tags |
| `agent.yml`: the amd64 and arm64 agent tarballs | hosted Linux | by hand; `v*` tags |
| `exit.yml`: the Phase 1–5 exit tests, Chromium and ExoPlayer | hosted Linux | by hand (`soak` for 30 minutes); `v*` tags |
| `exit-safari.yml`: Safari | hosted macOS (10×) | by hand; `v*` tags |
| `install.yml`: the installers end to end on two local release candidates | hosted Linux | by hand |
| `release.yml`: npm publish of the `@clipmux` packages (player, uploader, and the SDK `@clipmux/node`) | hosted Linux | `v*` tags; by hand (a dry run by default) |
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
