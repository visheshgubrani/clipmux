# Retained real-account staging

How the maintainers test Cloudflare delivery against a real account, moved here from [cloudflare.md](cloudflare.md).

The opt-in browser harness targets only `app-test.clipmux.com` and
`media-test.clipmux.com`. It never drops the staging database. It is not
part of ordinary test runs.

1. Save the API token alone (no variable assignment) in
   `data/cloudflare-staging/cloudflare-token`, mode 0600. User and
   account-owned tokens are supported. Run `python3 deploy/cloudflare-staging.py preflight` to verify Workers Paid before provisioning.
2. Start staging's own Postgres, on loopback port 5445 beside the dev
   database's 5444, in the named volume `clipmux-staging_pgdata`:
   `python3 deploy/cloudflare-staging.py db-start` (`db-stop` stops it and
   keeps the volume).
3. Build the app to `data/cloudflare-staging/bin/clipmux` and install the
   pinned packager with
   `toolchain/install_packager.sh data/cloudflare-staging/bin`.
   Run `python3 deploy/cloudflare-staging.py start`, then `bootstrap`.
   The helper retains private owner and integration credentials; it prints none.
4. Provision a dedicated `clipmux-staging` Cloudflare Tunnel to loopback port
   8086 and initially point both test hostnames at it. Keep `CLIPMUX_SELF_DIAL`
   unset. Do not change the existing `media.clipmux.com` service. Once the
   credentials and ingress are saved as private `tunnel.json` and `tunnel.yml`,
   use `python3 deploy/cloudflare-staging.py tunnel-start` to reconnect.
5. Run `python3 deploy/cloudflare-staging.py prepare` before connecting the
   Worker. It uploads a playable 70 MiB, 30-second baseline MP4 to filesystem
   storage and saves private `live.json`. Its final MP4 free box pads it to
   exactly three upload parts without changing the media.
6. Build the browser workspace. From `e2e/web`, run:
   ```bash
   CLIPMUX_E2E_LIVE_CONFIG="$PWD/../../data/cloudflare-staging/live.json" pnpm run test:browser
   ```
   This runs the dashboard Connect flow twice, checks the setup test, then
   exercises playback and multipart upload/resume against the real endpoints.
   Cloudflare credentials stay in the test process, never the fixture HTTP API.
   The fixture listens only on loopback and rejects unexpected Host values.
7. After the browser suite passes, run the live acceptance test from the
   repository root (this consumes the original filesystem baseline by moving it
   to R2, and stops the app at the end):
   ```bash
   CLIPMUX_E2E=1 \
   CLIPMUX_E2E_BIN="$PWD/data/cloudflare-staging/bin/clipmux" \
   CLIPMUX_E2E_LIVE_ACCEPTANCE=1 \
   CLIPMUX_E2E_LIVE_CONFIG="$PWD/data/cloudflare-staging/live.json" \
   CLIPMUX_E2E_LIVE_DATABASE_URL='postgres://clipmux:clipmux@127.0.0.1:5445/clipmux?sslmode=disable' \
   go test ./internal/e2e -run '^TestCloudflareLiveAcceptance$' -count=1 -timeout 25m -v
   ```
   This checks origin fallback, generated captions, ID and signing-key
   revocation, content-key rotation, R2 HEAD/Range, and a fresh moved output
   with the app stopped. Speech uses a deterministic local test provider;
   Cloudflare, R2 storage and delivery remain real. Later browser playback
   runs can reuse the moved asset, including its newer content-key version.
   To verify the player with the control plane stopped, run only
   `tests/playback.spec.ts` in the live browser mode. That mode supplies tokens
   from its loopback fixture, without querying the app.
8. Stop the app with `python3 deploy/cloudflare-staging.py stop` and stop
   the connector with `python3 deploy/cloudflare-staging.py tunnel-stop`, then
   Postgres with `python3 deploy/cloudflare-staging.py db-stop`, which keeps
   its volume. Keep the private state and cloud resources for reuse.

The browser tests alone do not close the phase exit: also record origin
fallback, a move with uncached playback while serve is stopped, signing-key
and playback-ID revocation within 65 seconds, captions, and content-key
rotation with old and new outputs. Record results in `docs/decisions.md` and
leave unverified items in `docs/known-gaps.md`. Never share raw browser traces,
request dumps or private staging files.


Browser Integrity Check can reject API clients with Cloudflare error 1010.
For this staging run the owner approved a persistent configuration rule with
`bic: false` matching only the two test hostnames. The zone-wide setting and
production hostnames were preserved. Such a rule is a separate operator
choice; Connect does not silently change security filters.

The November 8 real-account run used Workers Paid and R2 Paid. Cloudflare's
[CDN service-specific terms](https://www.cloudflare.com/service-specific-terms-application-services/)
require an applicable paid service, such as the Developer Platform, for video.
