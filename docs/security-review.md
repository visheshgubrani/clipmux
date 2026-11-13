# Security review: the threat table against the code

Reviewed 2026-11-08 for P6-T06, on the branch that followed Phase 5
(`ci/lean-and-green`). Each row of [architecture.md](architecture.md)'s
"Threats and controls" was traced to the code that enforces it. Method:
reading the code, plus `go test` of `tokens`, `publicnet`, `logging`,
`media` and `sandbox`. Re-run this review before v1.0.0 and after any change
to a control below.

**Result:** of the 31 rows, 22 hold as written and 9 hold only in part.
None is missing. Six findings fall outside the table: two medium and four
low. No high.

**Update 2026-11-14:** all six findings are fixed (D120 to D124), and the
rows they touched were re-checked against the new code: the sandbox, agent,
worker-output and upload rows now hold, and 26 of the 31 rows hold as
written. The remaining partial rows are the known gaps they name.

| Threat | Control in code | Verdict | Notes |
| --- | --- | --- | --- |
| Hostile media exploits ffmpeg or the packager | `sandbox/sandbox_linux.go` (rlimits, no_new_privs, Landlock, seccomp with namespaces denied); `media/run.go` demuxer and `file`-only protocol allowlist; packager key in a flag file; empty tool environment; serve and workers undumpable; the built-in worker refuses to start without Landlock | Holds | Findings 1 and 6 fixed (D120). An owner who sets `CLIPMUX_UNSANDBOXED_WORKER` without Landlock accepts that the tools can read serve's files |
| Unsandboxed agent processes public uploads | `workerapi.go` marks it untrusted; `job_claimable_by` (migration 00035) gives it pinned jobs, or Dashboard-app jobs that aren't URL imports; the Dashboard app takes no API keys (`409 builtin_app`) | Holds | Finding 3 fixed (D123); sandbox status is self-reported, by design |
| Worker writes outside its output, or uploads hostile files | Path grammar at sign; server-side prefix; commit lists storage and checks the playlists (`outputs/outputs.go`); write-once PUTs; built-in delivery serves only the manifest's files; `retention.SweepUnlisted` deletes the rest 16 minutes after commit | Holds | Finding 4 fixed (D124); on the Worker a late file can be served until the sweep runs (known gap) |
| Hostile file served from the media domain | Extension allowlist and `nosniff` in `delivery.go`; storage GETs `octet-stream` with `CSP: sandbox` | Partial | Same origin as the dashboard unless `CLIPMUX_MEDIA_URL` is set (known gap) |
| Worker or agent key leaked | Hashed per-agent keys, pairing approval, revoke with fencing, local scope, re-encode by agent | Holds | |
| Provider machine sees one job's source and key | Attempt-scoped token; `sandbox_required` for dispatches | Holds | |
| One-time dispatch key leaked | Hashed, valid while waking and before the claim deadline, claims only its job | Holds | A lost lease reopens it until the deadline; bounded |
| Encoder-killing file runs up a bill | `fallback_refused`; concurrency and spend under a row lock | Holds | Spend is an estimate (known gap) |
| Crash leaves a paid machine running | Outbox wakes and stops, reaper, reconciler at boot and every 15 min | Holds | |
| SSRF through import from URL | `publicnet`: checked at dial time after DNS and on each redirect; no proxy | Holds | |
| Server asks an agent for arbitrary files | Opaque `src_` IDs, root-checked paths on the agent, attach checks ownership | Holds | |
| Token stripped from a signed URL | A signed ID on `/p/` is refused; routes default-deny | Holds | |
| Playback token shared | Playback IDs rotate | Partial | `vid` is parsed but logged nowhere (architecture's open checklist item) |
| Long-lived playback token leaked | `iat` required and not in the future; `exp - iat` ≤ the project maximum; same in the Worker | Holds | |
| Signing key leaked | Per-app ES256, `kid` revocation | Partial | Takes effect after the route cache: ≤ 30 s built-in (D072), ≤ 60 s on the Worker (D090 measured 6 s) |
| Bucket, R2 credentials or disk leaked | AES-128 segments; keys derived, never stored | Holds | Sources stay plaintext, as designed |
| Content key leaked | Per-output keys | Partial | A retired output stays servable ≥ 7 days before it is purged (D070) |
| Content key secret leaked | Sealed in Postgres, a Worker secret; rotates (D071) | Partial | Rotation doesn't re-encode existing outputs (known gap) |
| One app reads or deletes another's videos | Every asset lookup is keyed by app; `kid` checked against the route's app | Holds | |
| Revocation fails at storage | `route_sync` outbox, reconciler | Holds | |
| Stale worker overwrites a newer run | Per-attempt output prefix, fenced commit | Holds | |
| Upload abuse | Size cap, parts within the declared size, each part URL signed for its length, size check at complete, hashed upload token | Holds | Finding 5 fixed (D122) |
| Owner account claimed first | Setup token, constant-time, daily rotation, limited per address | Holds | No global limit since D121, so others can't lock the installer out |
| Secrets read from the database | AES-256-GCM with table and row as associated data; key outside Postgres | Holds | |
| Cloudflare token stolen | Only the token ID and its SHA-256 are kept, sealed | Holds | |
| Forged webhooks | HMAC-SHA256 over `ts.body`; sealed secrets; redirects not followed | Holds | |
| Subtitles or chapters read from storage | Tracks sealed with AES-GCM, the storage key as associated data | Holds | |
| Thumbnail token reads a transcript | Audience `t` refused on `tracks/`; the transcript API needs `assets:read` | Holds | |
| Transcript or audio sent to third parties | Off until the owner connects a provider | Holds | |
| Prompt injection via speech | `<`/`>` stripped, strict schema, local checks | Holds | Not re-tested here |
| Speech or LLM key leaked | Sealed in `ai_connections`, opened only in serve | Holds | |

## Findings beyond the table

Each finding below is fixed; its decision says how.

1. **Fixed (D120). Medium: without Landlock, the built-in worker's tools can read serve's
   environment, which holds the secrets key and the database password.**
   serve and the built-in worker run as one user, and Go processes stay
   dumpable, so `/proc/<serve>/environ` is readable by an exploited ffmpeg on
   a kernel or Docker without Landlock (the sandbox still counts as enforced
   there). Having no socket, the tool could still smuggle the value out in
   the public `poster.jpg`. The known gap says "any file the worker's user
   can"; it doesn't say that this includes the master key. **Fix:**
   `prctl(PR_SET_DUMPABLE, 0)` in serve, or a separate uid for the worker, or
   refuse the built-in worker without Landlock.
2. **Fixed (D121). Medium: anyone can lock the owner out of sign-in.** The global limit of
   20 failures per 15 minutes (`accounts/limiter.go`) refuses every address,
   so 20 wrong passwords from four addresses every 15 minutes keep the owner
   out. The setup-token and pairing limiters work the same way. **Fix:**
   delay rather than refuse at the global limit, or exempt an address with a
   recent success, and amend D067.
3. **Fixed (D123). Low: an unsandboxed agent's "owner jobs" are the Dashboard app's,** and
   API keys and URL imports can create those. **Fix:** refuse API keys for
   the built-in app, or gate on the uploader rather than the app.
4. **Fixed (D124). Low: files can be added to an output after its commit.** Verification
   runs before the commit, PUT URLs last 15 minutes, and delivery serves any
   grammar-valid name under a committed output. **Fix:** serve only what the
   committed manifest lists.
5. **Fixed (D122). Low: S3/R2 part URLs don't bind the part's length.** A holder of an
   upload token can PUT up to 5 GiB per part; complete then fails, but the
   bytes stay until the upload is aborted. The filesystem adapter already
   enforces it. **Fix:** sign `ContentLength` into the part URL.
6. **Fixed (D120). Low: seccomp allows namespace creation** (`unshare`, `setns`, `clone`
   with `CLONE_NEWUSER`), which exposes kernel attack surface on a native
   agent. Docker's default profile already blocks it. **Fix:** deny them.

Checked and sound: secret comparisons (constant-time or by hash), request
body limits, the dashboard's CSP, COOP, `frame-ancestors` and `nosniff`, the
`__Host-` HttpOnly SameSite=Strict session cookie with cross-origin
protection, redirects (configured hosts only; no open redirect), metrics and
pprof on their own listener, `X-Request-Id` trusted only from proxies, log
scrubbing, and TOTP replay refusal.

## History scan

The whole history was scanned on 2026-11-08: all refs, 99 commits.

- **gitleaks v8.28.0:** 61 hits, all public by design. They are the delivery
  contract's test keys and tokens (`internal/delivery/testdata/vectors.json`)
  and the dev SeaweedFS's fixed credentials (`deploy/seaweedfs-dev.json`,
  `Makefile`, the workflows). With `.gitleaks.toml` allowing exactly those,
  it finds nothing.
- **trufflehog 3.90.8:** 0 verified and 7 unverified. The unverified are
  the same test keys, plus `user:…@example.com` in `publicnet_test.go`.
- **`.env` and keys:** no `.env` file other than the examples has ever been
  committed, and no `.pem` or `.key` file either.

Nothing live, so nothing to rotate or scrub. gitleaks runs locally before
merging, not in CI: the lean pull-request job (74634dc) dropped it, as
[.github/README.md](../.github/README.md) says. A 51 MB build of the binary
was committed at the root and removed from the tree on 2026-11-14; it stays
in history and holds no secret.
