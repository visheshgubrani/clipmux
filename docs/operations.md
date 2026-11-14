# Running ClipMux

How to install, upgrade, back up, watch and secure one ClipMux install. The
supported production shape is one `serve` process with Postgres and Caddy
(D063); every setting is in [configuration.md](configuration.md).

## Install

```bash
cp deploy/compose.env.example deploy/.env      # set POSTGRES_PASSWORD and CLIPMUX_SECRETS_KEY
docker compose -f deploy/compose.yml up -d --build --wait
docker compose -f deploy/compose.yml logs app | grep setup_token
```

1. Open `http://<host>`, create the owner with the setup token from the log.
   The token works for a day; after that the log shows a new one (D071).
2. Set the public URL and the media URL. Point both hostnames' DNS at this
   host first: Caddy gets a certificate for each on its first request. Use a
   media hostname on another registrable domain, so a hostile stored file
   can't reach dashboard cookies.
3. The setup test runs a sample through the whole path; the dashboard shows
   each check.
4. Turn on two-factor sign-in under your email in the dashboard's header.

`CLIPMUX_SECRETS_KEY` is `v1:` and 64 hex digits:

```bash
echo "v1:$(openssl rand -hex 32)"
```

Store it outside the host as well as in `deploy/.env` (see Backups).

### Behind your own proxy

On a host where another proxy already owns ports 80 and 443 (a host Caddy
serving other sites, say), add `deploy/compose.proxy.yml`. It leaves Caddy
out and has the setup test dial the public URLs through your proxy:

```bash
docker compose -f deploy/compose.yml -f deploy/compose.proxy.yml up -d --build --wait
```

- **Port.** The app listens on `127.0.0.1:${CLIPMUX_ADMIN_PORT}` (8080 by
  default; set another in `.env` if it's taken). Point your proxy at it:

  ```caddyfile
  video.example.com {
      reverse_proxy 127.0.0.1:8108
  }
  ```

- **Client addresses.** `CLIPMUX_TRUSTED_PROXIES` already trusts Docker's
  ranges, which is where a proxy on the host connects from. Your proxy must
  set `X-Forwarded-For` and `X-Forwarded-Proto`; Caddy, nginx's usual
  config and Traefik do.
- **No CDN proxy on the public hostname.** Point its DNS straight at the
  host (DNS only on Cloudflare). A proxy that challenges API clients
  (Cloudflare's Browser Integrity Check) refuses `@clipmux/node`, the CLI
  and workers, and it hides the client's address. Playback goes through the
  media hostname, which can be the Cloudflare Worker.
- **Small or shared hosts.** Set `CLIPMUX_BUILTIN_WORKER=false` in `.env`,
  and connect Modal, RunPod or a PC agent, so encoding doesn't compete with
  the host's other services. The setup test then runs on that executor.

## Upgrade

```bash
git pull
docker compose -f deploy/compose.yml up -d --build --wait
```

- serve applies new migrations at boot, holding a lock, so two processes
  never migrate at once. A binary older than the database refuses to start
  with `schema_newer`; run the newer one, or roll back with it first:
  `clipmux admin migrate down-to VERSION --yes`.
- `clipmux admin migrate status` lists what the database has.
- Stopping serve drains (D065): it stops handing out jobs, lets the built-in
  worker hand its job back, and finishes requests in flight, within compose's
  40-second grace period. Uploads resume, players retry, and nothing is lost.
- With Cloudflare delivery, the dashboard's Delivery page says when the
  Worker needs redeploying; Reconnect Cloudflare with a token does it.

### A release image behind your own proxy

A host installed from the release compose file and `compose.proxy.yml`
(D114) upgrades by moving `CLIPMUX_VERSION` to a newer image:

```bash
gh workflow run image.yml --ref main          # builds and pushes sha-{commit}
cd /opt/clipmux                               # where compose.yml and .env are
cp .env .env.bak-$(date +%Y%m%d%H%M)
sed -i 's/^CLIPMUX_VERSION=.*/CLIPMUX_VERSION=sha-<commit>/' .env
docker compose -f compose.yml -f compose.proxy.yml pull app
docker compose -f compose.yml -f compose.proxy.yml up -d --wait
docker compose exec app clipmux version        # the new commit
curl -fsS https://<public host>/readyz
```

- While the image package is private, the host needs `docker login ghcr.io`
  with a `read:packages` token, and so do Modal and RunPod.
- With `CLIPMUX_WORKER_IMAGE` unset, serve re-pins Modal and RunPod to its
  own image at boot (D119), so cloud encoding follows the upgrade.
- To roll back past a migration: with the new image still running,
  `docker compose exec app clipmux admin migrate down-to VERSION --yes`
  (the old image's newest migration, from `migrate status` before the
  upgrade), then restore `.env.bak-…` and `up -d --wait`.

## Release gate

Before a release reaches npm, the SDKs run against a live instance with a
dedicated app (on app.clipmux.com, "SDK release check"):

```bash
docker compose exec app clipmux admin create-integration --app "SDK release check" > live.env  # once; keep it private
node packages/node/scripts/live-check.ts --env-file live.env clip.mp4
```

The script uploads through `ClipMuxUpload`, checks a wrong-length part is
refused, waits for the asset, plays it through delivery (playlist, segment,
key, poster), checks the poster-only and native tokens, waits for subtitles
and the transcript, relays webhooks through `listen` into a receiver that
verifies them, and deletes what it made. It prints no secret. Revoke the
key afterwards with `clipmux admin revoke-key`.

## Backups

Three things, backed up together. Losing the secrets key makes every stored
secret and every output unplayable, because content keys derive from a
secret it seals.

| What | Where | How |
| --- | --- | --- |
| Postgres | the `pgdata` volume | `docker compose -f deploy/compose.yml exec -T postgres pg_dump -U clipmux -Fc clipmux > clipmux.dump`, daily |
| The secrets key | `deploy/.env` (`CLIPMUX_SECRETS_KEY`), or `{data_dir}/secrets.key` | once, and after every rotation, somewhere off the host |
| Media | the `data` volume (filesystem storage), or your R2/S3 buckets | snapshot the volume; for buckets, the provider's versioning or replication |

Outputs are AES-128 ciphertext, so a media backup is useless without the
other two; sources are stored as uploaded. Restore:

```bash
docker compose -f deploy/compose.yml up -d postgres
docker compose -f deploy/compose.yml exec -T postgres pg_restore -U clipmux -d clipmux --clean --if-exists < clipmux.dump
# restore the data volume (or point the bucket configs at the restored buckets)
docker compose -f deploy/compose.yml up -d --wait     # with the same CLIPMUX_SECRETS_KEY
```

serve checks at boot that its key opens what the database holds, and that
every output's content key version exists, and refuses to start otherwise.

Analytics are in Postgres too, so the dump carries them, and viewer keys
stay comparable because the analytics key is sealed with the secrets key.
Edge counts live in Cloudflare for about three months, but a restored
install pulls only the last 13 days of them again (docs/cloudflare.md).
Share links are rows like any other: a restore brings them back, along with
their playback IDs and expiries.

## Monitoring

- **Metrics.** Compose serves `/metrics` on `app:9090`, inside its network
  only. Run Prometheus on that network and load
  [`deploy/prometheus/alerts.yml`](../deploy/prometheus/alerts.yml): serve
  down, no loop leader, 5xx rate, failing or panicking loops, stuck outbox
  rows, queue backlog, no worker online, webhooks giving up, a full pool.
  `CLIPMUX_PPROF=true` adds Go's profiler on the same port.
- **Logs.** Compose logs JSON, one object per line, rotated at 5 × 50 MB.
  Every request logs one `http request` line with a `request_id`, also sent
  back as `X-Request-Id`: a user's error report with that ID finds the line
  and any error logged for the same request. Tokens, keys, pairing codes and
  URL signatures are redacted wherever they appear (D064).
- **Health.** `/healthz` answers while the process is up; `/readyz` only
  while Postgres answers and serve isn't draining.

## Analytics

serve counts the playback it delivers (D103; docs/api.md, "Analytics"): a
view per generated master playlist, 6 seconds of watch time per audio
segment, and the bytes of every counted response. Dashboard previews,
posters and the setup test don't count; share links do. Unique viewers need
the app to sign a `vid` into its tokens, and ClipMux keeps only a keyed hash
of it, per day. No IP address, User-Agent, referrer or token is stored.

- **Built in.** Each replica keeps counts in memory and flushes them every
  15 seconds, and once more on SIGTERM; a replica killed outright loses at
  most its last 15 seconds. `clipmux_analytics_flush_rows_total` counts what
  flushes wrote, and `clipmux_analytics_dropped_total` counts keys dropped
  because more than 100,000 piled up between flushes.
- **With Cloudflare delivery** the Worker counts at the edge, and Analytics
  shows those counts only once edge analytics is connected with a read-only
  token (Delivery → Edge analytics; docs/cloudflare.md). serve pulls every
  5 minutes as the `analytics_edge` loop.

## Retention

serve deletes what nothing can use any more, every hour (D069, D070).
`clipmux admin prune --dry-run` shows what the next pass would delete, and
`--files` runs the file rules too.

| What | Kept for |
| --- | --- |
| Retired outputs (after a re-encode) | the longer of 7 days and the longest token lifetime, plus an hour |
| Files of a failed or replaced attempt | 30 minutes |
| Output prefixes no row accounts for | swept continuously, 500 assets an hour |
| Filesystem staging (`tmp/`, abandoned multipart uploads) | 24 hours, 7 days |
| Idempotency keys | 24 hours |
| Dashboard sessions | until they expire, or 7 days idle |
| Webhook deliveries and events (`/v1/events`, `clipmux listen --since`) | 30 days after they finish |
| Other outbox rows | 7 days after they're done |
| Setup tests | the newest 20 |
| Analytics, hourly (the 24-hour range) | 14 days |
| Analytics, daily, and viewer keys | 180 days (D105): there are no lifetime totals |
| Share links | kept, with their state; an expired link's playback ID is revoked within a minute |
| Jobs, attempts, dispatches | kept: history and spend |

Route publications, key publications and machine stops are never given up
on. `clipmux admin outbox` lists any pending for over an hour, and
`--retry ID` makes one due now.

## Rotating keys

| Key | How | What keeps working |
| --- | --- | --- |
| Secrets key | Set the new key as `CLIPMUX_SECRETS_KEY` with the next version (`v2:…`) and the old one as `CLIPMUX_SECRETS_KEY_PREVIOUS`, restart, run `clipmux admin reseal-secrets`, then remove the previous key and restart | Everything; serve refuses to start if a sealed value opens with neither key, naming its column |
| Content key secret | `POST /v1/content-keys/rotate` (owner session); with Cloudflare delivery pass `{"cloudflare_token": …}`, as for Connect, so the Worker has the new version before any output uses it | Every existing output, on its own version; re-encode an output to move it |
| An app's signing key | `clipmux admin rotate-signing-key --app NAME`, then switch the app's backend to the new private key | Tokens signed with the old key, until the longest of them expires |
| A webhook secret | `POST /v1/webhook-endpoints/{id}/secret`, give the receiver the new secret, then `…/secret/activate` | Deliveries stay signed with the old secret until you activate |
| An API key | create a new one in the dashboard, then `clipmux admin revoke-key cmx_…` | Nothing signed with the revoked key |

A leaked key: revoke it at once (`clipmux admin revoke-key ID` for API and
signing keys), then rotate whatever it could reach. A leaked content key
secret: rotate it and re-encode the affected outputs.

## Security checklist

- The media hostname is on a different registrable domain from the public
  URL.
- `CLIPMUX_TRUSTED_PROXIES` names only Caddy's network; compose's default
  covers Docker's private ranges.
- The metrics port isn't published; compose only `expose`s it.
- The owner has two-factor sign-in on, and the recovery codes are stored
  offline. A locked-out owner: `clipmux admin reset-password` and
  `clipmux admin reset-totp` on the host.
- Backups hold the secrets key, and the backups are encrypted or offline.
- PC agents run sandboxed (the Workers page shows it); an unsandboxed agent
  only takes its own and the owner's uploads.
- Rate limits are per process (D067): run one serve, behind Caddy.
- Share links go one per person or group, so one can be revoked without the
  others. Anyone holding a link can watch until it expires or is revoked.

## Troubleshooting

| Symptom | Look at |
| --- | --- |
| An app gets 429 | `Retry-After`; the limits are in docs/api.md |
| Uploads stay `processing` | the Jobs page (where each job is routed and why), `clipmux_workers_online`, the built-in worker's lines in the log |
| A webhook stopped arriving | the endpoint's deliveries (`GET /v1/webhook-endpoints/{id}/deliveries`): after 5 failures in a row it is skipped for up to an hour; failed deliveries can be redelivered |
| Playback 404s at the edge after a change | `clipmux admin outbox`: a route publication that keeps failing |
| Analytics misses Cloudflare playback | Analytics' notice and Delivery → Edge analytics: the last pull and its error; `collecting.edge` in `GET /v1/analytics` |
| Analytics shows no unique viewers | the app's tokens carry no `vid`; previews and posters never count |
| A share link says it doesn't work | the link was revoked, its video deleted, or its app disabled; the video's Sharing tab shows each link's state |
| serve won't start | the last log line: `schema_newer`, a secrets key that opens nothing, a missing content key version, or a variable that doesn't parse |
