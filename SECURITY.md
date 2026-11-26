# Security policy

ClipMux holds storage credentials, content keys, signing keys and webhook
secrets for the people who run it, and plays media to the public. Please
report vulnerabilities privately.

## Reporting a vulnerability

**Don't open a public issue, discussion or pull request.** Use GitHub's
private vulnerability reporting: on
[visheshgubrani/clipmux](https://github.com/visheshgubrani/clipmux), open
**Security → Report a vulnerability**. Include what you can of:

- the version (`clipmux version`) or commit, and how it is deployed
  (compose, the agent tarball, a serverless provider, Cloudflare delivery);
- a minimal reproduction: requests, files, configuration;
- what an attacker gains;
- a fix, if you have one.

You'll get an acknowledgement within 3 days and an update at least every 7
days until it is resolved. Please give us 90 days, or until a fixed release
ships if that is sooner, before disclosing. We credit reporters in the
advisory unless you ask us not to.

## Scope

In scope: everything in this repository, which is the `clipmux` binary
(serve, the built-in worker, PC agents, `push`, `listen`, `admin`), the
image and agent tarball as released, the Cloudflare Worker (`worker/`),
`@clipmux/node`, `@clipmux/player`, `@clipmux/uploader`, and the installers in
`deploy/`.

Out of scope: a deployment's own configuration (a leaked key, a public
bucket, a disabled sandbox), vulnerabilities in Cloudflare, RunPod, Modal or
another provider's platform, and denial of service by sheer volume. A
vulnerable dependency we ship is in scope.

## What we defend

The threat table in [docs/architecture.md](docs/architecture.md) ("Threats
and controls") is the model, and [docs/security-review.md](docs/security-review.md)
records its latest review against the code. In short:

- **Playback** is ES256 JWTs per app, checked on every request; route
  records default-deny, and segments are AES-128 encrypted with keys served
  only to valid tokens.
- **Workers are untrusted.** Executors hold no storage credentials: they get
  short-lived signed URLs for names that pass a path grammar, playlists are
  checked at commit, and delivery serves only allowlisted extensions. FFmpeg,
  ffprobe, the packager and whisper.cpp run sandboxed (seccomp, rlimits,
  no_new_privs, Landlock where the kernel has it).
- **Secrets at rest** (storage and provider credentials, signing keys,
  webhook secrets, the content key secret, TOTP secrets, AI keys) are sealed
  in Postgres with a key held outside it, and rotate (D071).
- **Logs** never hold Authorization headers, tokens, presigned URLs, content
  keys or secrets; the exit tests check every line.

[docs/known-gaps.md](docs/known-gaps.md) lists the limitations we know of
and accept for now; a report that one of them is worse than it says is
welcome.

## Secrets in this repository

None are committed, and `.env*` files other than `.env.example` are ignored.
The project's predecessor once leaked an R2 key into git history. The rule
since: **rotate first, then scrub history, then verify with `git grep`**.
If you think a secret reached this repository, report it privately as
above.

## Supported versions

The latest release gets security fixes.
