# What CI runs, and when

The repository is private, so hosted Actions minutes are metered: 2,000 a
month, with macOS at 10× and each job rounded up to the started minute.
Automatic runs stay small. The expensive suites run when they're asked for,
or on a release tag, and the self-hosted runner, which costs nothing, takes
the slow real-whisper tests.

| Workflow | Runs on | When |
| --- | --- | --- |
| `ci.yml`: `go` and `checks` (with gitleaks over the whole history and shellcheck of the installers) | hosted Linux | every pull request, by hand |
| `gpu.yml`: GPU encodes and all real whisper.cpp tests | self-hosted (RX 6600 XT) | pull requests touching media, the sandbox, the worker, the setup test, transcripts or the toolchain; weekly; by hand. Off unless `GPU_RUNNER` is `true` |
| `image.yml`: the image under compose, the setup test, pushes `sha-{commit}` | hosted Linux | pull requests touching the image; by hand (pushes from main); `v*` tags |
| `agent.yml`: the amd64 and arm64 agent tarballs | hosted Linux | pull requests touching the Dockerfile or the toolchain; by hand; `v*` tags |
| `exit.yml`: the Phase 1–5 exit tests, Chromium and ExoPlayer | hosted Linux | by hand (`soak` for 30 minutes); `v*` tags |
| `exit-safari.yml`: Safari | hosted macOS (10×) | by hand; `v*` tags |
| `install.yml`: `deploy/install.sh` and `install-agent.sh` end to end on two local release candidates | hosted Linux | pull requests touching the installers or compose files; by hand |
| `release.yml`: npm publish of `@clipmux/player` and `@clipmux/uploader` (more with P6-T07) | hosted Linux | `v*` tags; by hand (a dry run by default) |
| `exit-llm.yml`: real OpenAI and Claude | hosted Linux | weekly and by hand, only while `REAL_LLM` is `true` |
| `exit-providers.yml`: real RunPod and Modal | hosted Linux | after `image.yml` on main, weekly and by hand, only while `REAL_PROVIDERS` is `true` |

**Before you merge**
- Wait for `go` and `checks`, and for `gpu.yml` when it ran.
- For a pull request that touches playback, delivery, the player or the
  dashboard, run `exit.yml` by hand:

```bash
gh workflow run exit.yml --ref BRANCH
```

- Run `exit-safari.yml` too when the pull request touches what Safari plays.

Hosted runs set `CLIPMUX_SKIP_SPEECH=1`, so the tests that need real
whisper.cpp skip there instead of failing. The setup of the self-hosted
runner is in `gpu.yml`'s header.
