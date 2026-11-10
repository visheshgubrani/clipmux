# Cloudflare delivery

ClipMux serves playback itself by default. Connecting Cloudflare moves it to
the ClipMux Worker, which reads an R2 bucket and caches at the edge. Apps,
tokens, playback IDs and `base_url` don't change (docs/architecture.md,
"Delivery").

## Connect Cloudflare (dashboard)

You need:

- a domain that is an **active zone** in your Cloudflare account, and a media
  hostname in it (for example `media.example.com`) that is a different
  registrable domain from, or at least a different hostname than, the
  dashboard;
- **Workers Paid** on the account: video delivery exceeds the free plan's
  request quota and terms;
- a user or account-owned API token with **Workers Scripts: Edit**, **Workers Routes: Edit**,
  **Workers R2 Storage: Edit** and **Billing: Read** (account), **Zone: Read** and **DNS: Edit**
  (the media zone). To limit R2 to one bucket, create the bucket first and
  scope the token to it.

Open **Delivery → Connect Cloudflare**, paste the token and the hostname, and
connect. ClipMux verifies the token, finds the zone, checks Workers Paid,
creates the bucket if needed, derives R2 credentials from the token (its ID
and the SHA-256 of its value), checks the bucket and applies CORS, deploys
the Worker with the content key secrets, writes every route record, removes
the DNS records at the hostname, attaches the Worker as a custom domain, and
runs the setup test through it. Each step is safe to repeat: fix what failed
and connect again.

ClipMux keeps only the token's ID. Because R2's credentials derive from the
token, **revoking or rolling it cuts off storage**; the Delivery page then
shows *Reconnect Cloudflare*. The token's Workers and DNS permissions stay
live in Cloudflare: narrow or revoke them there when you no longer need to
redeploy. A ClipMux upgrade that carries a newer Worker asks for a token to
redeploy it.

After connecting, new uploads go to R2. Older videos keep playing through
this server (the Worker fetches them from origin) until they are moved.

## Manual setup with wrangler

If you'd rather not paste a token, deploy the Worker yourself:

1. Build it: `make worker` (or take `internal/cloudflare/worker.js` from the
   release).
2. Create an R2 bucket and an R2 API token (Object Read & Write on it). Add
   the bucket to ClipMux as storage with that token's access key and secret
   (endpoint `https://<account_id>.r2.cloudflarestorage.com`, region `auto`).
3. Deploy with this `wrangler.jsonc`:

   ```jsonc
   {
     "name": "clipmux-media",
     "main": "worker.js",
     "compatibility_date": "2026-07-01",
     "r2_buckets": [{ "binding": "MEDIA", "bucket_name": "<bucket>" }],
     "vars": { "ORIGIN_URL": "https://<your ClipMux public URL>", "PROJECT_ID": "prj_default" },
     "analytics_engine_datasets": [{ "binding": "ANALYTICS", "dataset": "clipmux_delivery" }],
     "routes": [{ "pattern": "<media hostname>", "custom_domain": true }]
   }
   ```

   and set the content key secrets, which `clipmux admin` prints:
   `wrangler secret put CONTENT_KEYS`. Without `ANALYTICS_KEY` (the
   project's analytics key, 64 hex digits) the Worker still counts views,
   watch time and bytes, but no unique viewers.
4. Switch the project's delivery to the Worker on that bucket.

Steps 2 and 4 and the `CONTENT_KEYS` and `ANALYTICS_KEY` exports have no CLI yet (known gap); the
dashboard flow is the supported path in this release.


## Retained real-account staging

The opt-in browser harness targets only `app-test.clipmux.com` and
`media-test.clipmux.com`. It never drops the staging database. It is not
part of ordinary test runs.

1. Save the API token alone (no variable assignment) in
   `data/cloudflare-staging/cloudflare-token`, mode 0600. User and
   account-owned tokens are supported. Run `python3 deploy/cloudflare-staging.py preflight` to verify Workers Paid before provisioning.
2. Start dedicated Postgres:
   `docker compose -p clipmux-cloudflare-staging -f deploy/compose.dev.yml up -d --wait postgres`.
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
   CLIPMUX_E2E_LIVE_DATABASE_URL='postgres://clipmux:clipmux@127.0.0.1:5444/clipmux?sslmode=disable' \
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
   the dedicated Postgres container without removing
   volumes. Keep the private state and cloud resources for reuse.

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
