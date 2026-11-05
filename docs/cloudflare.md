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
- an API token with **Workers Scripts: Edit**, **Workers Routes: Edit**,
  **Workers R2 Storage: Edit** (account), **Zone: Read** and **DNS: Edit**
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
     "vars": { "ORIGIN_URL": "https://<your ClipMux public URL>" },
     "routes": [{ "pattern": "<media hostname>", "custom_domain": true }]
   }
   ```

   and set the content key secrets, which `clipmux admin` prints:
   `wrangler secret put CONTENT_KEYS`.
4. Switch the project's delivery to the Worker on that bucket.

Steps 2 and 4 and the `CONTENT_KEYS` export have no CLI yet (known gap); the
dashboard flow is the supported path in this release.
