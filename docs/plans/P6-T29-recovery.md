# Phase 6 — T29 Backups and recovery

**Status: in progress (2026-11-24).** D148 records the ARM64 capacity baseline.
D145 records a provisional B2 restore with playback and the exact read-only
restore of a production backup. Launch exit check 10 stays open pending the
cold run on the fixed image and the production cutover with scheduled backup
verification.

The detailed plan behind a new T29 row of [P6-launch.md](P6-launch.md).
Decisions land in [../decisions.md](../decisions.md) as D143 onward, as
each task finishes.

## Context

ClipMux already survives crashes. A killed process or a killed Postgres
loses nothing committed: job leases and the lost-lease sweep, the outbox,
the leader lock, the ordered drain (D065) and idempotent commits cover it.
What it can't survive is **losing state**. The product takes no backups and
has no restore, and the operator docs
(`site/content/docs/self-host/operate/backups.mdx`) only describe a
`pg_dump` from cron.

Production (the shared VPS of D114), read-only on 2026-11-23:

- **One arm64 vServer:** 2 vCPU, 3.7 GiB RAM, no swap, 38 GB disk at 69%,
  shared with the LMS (Edural) and other apps. Up 245 days, with 27 kernel
  updates and libc6 waiting on a reboot.
- **ClipMux state:** Postgres is 14 MB. Media is in R2, the default store.
  Encoding runs on Modal and RunPod. The secrets key exists only in the
  stack's `.env` and same-disk copies of it.
- **Edural's database** is 74 MB, dumped by its deploy script to the same
  disk.
- **No backup leaves the host.** There is no cron job or timer; the only
  dumps are ad hoc ones taken before deploys.
- **Monitoring:** Uptime Kuma runs on the same host, so it can't report the
  host going down. Nothing scrapes `/metrics`, so
  `deploy/prometheus/alerts.yml` never fires.

### Restore hazards in today's code

Bringing back an older database destroys newer data on its own. This
applies to a `pg_dump` restore, a VPS snapshot rollback, and a fresh
install connected to a bucket that's already in use.

- **Route records.** `routes.Reconcile` (`internal/routes/routes.go:322-326`)
  deletes every `_routes/{pid}.json` the database doesn't know. Within 15
  minutes, every video uploaded after the backup stops playing at the edge,
  and `_keys.json` drops any newer app keys.
- **Re-encoded outputs.** `SweepOrphans`
  (`internal/retention/files.go:215-253`) deletes `out_*` prefixes with no
  row, including outputs re-encoded after the backup.
- **Fresh install on a used bucket.** The first reconcile wipes all of
  `_routes/`.

### Settled with the owner on 2026-11-23

- **Backups go to Backblaze B2**, a provider independent of both Hetzner
  and Cloudflare, with object lock and keys that can't delete. The product
  accepts any S3-compatible bucket or a local path.
- **Postgres is backed up by scheduled, encrypted dumps:** every hour by
  default, every 15 minutes on prod. Continuous WAL archiving (PITR) is not
  built.
- **Media backup covers sources only.** Outputs can be re-encoded from them.
- **Prod gets interim protection now** (Track A), and the built-in feature
  **gates v1.0.0** (Track B).

## What done looks like

A self-hoster can trust ClipMux with their library:

- Backups are on by default, encrypted, off-site once a target is set, and
  checked automatically by a real restore.
- A restore is one command, and on a new host one installer flag.
- No part of the system can turn a restore into data loss.
- Every recovery time below has been measured, not estimated.

## Recovery targets

These are recorded as D145. The disaster-recovery page states them, and the
drills measure them.

| Scenario | Today | Target |
| --- | --- | --- |
| `serve` crashes or is OOM-killed | Docker restarts it in under 30 s. Jobs requeue within about 2 min (90 s lease + 15 s sweep). | Unchanged. |
| Postgres crashes | Restart and crash recovery take under 1 min. No committed data is lost. | Unchanged. |
| Host reboot or patching | About 2 min for every app. Playback is unaffected. | Same, in a set window. |
| Bad upgrade or migration | A dump exists only if someone took one. | Automatic backup before any migration. Roll back in 15 min or less, with nothing lost. |
| Disk or VPS lost | Everything is lost. Without the secrets key, every video is unplayable. | `install.sh --restore` on a new host, then repoint DNS. Back in 30 min or less. At most 15 min of data lost on prod, 1 h by default. |
| An older database comes back | Newer videos stop playing and re-encodes are deleted. | Restore fence: nothing is deleted, and a report lists what's newer than the database. |
| Fresh install pointed at a used bucket | Every route record is wiped. | The store's epoch marker has another install ID, so the install is fenced. |
| R2 objects deleted, or the Cloudflare account lost | Unrecoverable. | Restore sources from the backup target and re-encode them, in hours. |
| Secrets key lost | Unrecoverable, by design. | Recovery kit kept off the host. Backups are encrypted to it and to the operator's own key. |
| Control plane down | R2-backed videos keep playing through the Worker. | Same, documented and probed from outside. |

## Exit check 10: Recovery

Added to P6's launch checklist.

1. Start a fresh VM that has only Docker and the saved `.env`.
2. Run `install.sh --restore`. It restores from B2 and brings back a working
   install.
3. A video uploaded before the backup plays.
4. A video uploaded *after* the backup is still in the bucket and appears in
   `clipmux admin restore report`. Nothing was deleted.

The time each phase took (image pull, restore, first playback) is recorded
as a decision and becomes the published recovery time.

---

## Track A: protect production now

Ops work on the production VPS, outside this repository. Every command that
changes the host is confirmed with the owner before it runs.

**The owner does these first:**

1. Create a private B2 bucket.
   - Turn on object lock, with a default retention of 7 days.
   - Add lifecycle rules: `*/hourly/` hidden after 3 days, `*/daily/` after
     35 days, `*/monthly/` after 400 days.
2. Create two application keys on that bucket.
   - One for the VPS, with `writeFiles` and `listFiles` but no
     `deleteFiles`.
   - One with read access, kept off the server.
3. Run `age-keygen` on a laptop. The identity goes in a password manager;
   only the public recipient goes on the server.
4. Copy `CLIPMUX_SECRETS_KEY` and Edural's env secrets into the password
   manager.
5. Create a healthchecks.io check, to raise an alert when a backup is
   missing.
6. Create an external uptime monitor (UptimeRobot or Better Stack, free
   tier).

**Then, on the VPS:**

| Step | Scope |
| --- | --- |
| A1 | **Hourly off-site dumps.** `/usr/local/sbin/vps-backup`, run by `vps-backup.timer` (hourly, `Persistent=true`), with its settings in `/etc/vps-backup.env` (mode 0600). For each database (ClipMux, Edural) it pipes `docker exec … pg_dump -Fc` through `age -r <recipient>` into `rclone rcat b2:…/<db>/hourly/<ts>.dump.age`. The day's first dump is also written under `daily/`, the month's first under `monthly/`. Once a day it tars and encrypts the configs (stack `.env` files, `/etc/caddy/`, compose files) under `config/`. It pings healthchecks.io at start, on success and on failure. `age` and `rclone` come from Ubuntu apt. |
| A2 | **Prove a restore.** On the laptop, which holds the identity: pull the latest dumps, decrypt them, `pg_restore` each into a scratch Postgres 18 container, and compare row counts with prod. Time it and write it down. |
| A3 | **Add a 2 GB swap file** with `vm.swappiness=10`. |
| A4 | **Free disk space.** Prune images no container uses and the build cache (about 7.7 GB). Once A2 has passed, trim the dumps on the same disk to the last 3 of each. |
| A5 | **Reboot** in a low-traffic window the owner picks, to apply the pending kernel and libc6 updates. Check that every container comes back. Then decide whether unattended-upgrades reboots on its own at a fixed quiet hour. |
| A6 | **External probes:** `https://app.clipmux.com/readyz`, Edural's API health, `edural.com`, and one segment of a known public test asset on the media hostname (playback without the origin). The healthchecks.io alert covers backups; Uptime Kuma stays for internal detail. |
| A7 | **Optional:** Hetzner's daily server backups as a second layer. Turn them on only after T29.2 ships, because rolling back to a snapshot is the hazard the fence exists for. |

## Track B: the product

Each slice is red → green against real Postgres (`*_db_test.go`,
`dbtest.New`). The fence comes first, because it makes every restore path
safe, including manual ones and snapshot rollbacks.

### Design notes

#### Configuration lives in env, not the dashboard (D143)

A restore needs the backup target before any database exists, so the
target belongs with `DATABASE_URL` and the secrets key. The `.env` file is
then the whole recovery kit.

- **`CLIPMUX_BACKUP_URL`** takes `s3://bucket/prefix?endpoint=…&region=…`
  or `file:///path`. The default is `file://{data}/backups`, daily, keeping
  7; the dashboard warns that it isn't off-site.
- **`CLIPMUX_BACKUP_ACCESS_KEY_ID`** and
  **`CLIPMUX_BACKUP_SECRET_ACCESS_KEY`**.
- **`CLIPMUX_BACKUP_INTERVAL`** defaults to `1h`; `0` turns backups off.
- **`CLIPMUX_BACKUP_RECIPIENTS`** adds optional age recipients, such as the
  operator's offline key.
- **`CLIPMUX_BACKUP_PING_URL`** is a dead-man's-switch URL, for
  healthchecks.io or an Uptime Kuma push monitor.
- **`CLIPMUX_BACKUP_BEFORE_MIGRATE`** defaults to `true`.

#### Encryption is age (D143)

`filippo.io/age` is a justified dependency: a reviewed streaming format,
and operators can open a backup with the standard `age` CLI and
`pg_restore`, without ClipMux.

- Each file is encrypted to an X25519 identity derived from the secrets key
  (HKDF, info `clipmux-backup-v1`), plus any `CLIPMUX_BACKUP_RECIPIENTS`.
- `clipmux admin backup identity` prints the derived identity.
- After a secrets-key rotation, keep the previous key until the oldest
  backup has expired. The rotation page says so.

#### Layout on the target

```text
{prefix}/db/{hourly|daily|weekly|monthly}/{ts}-{id}.dump.age
{prefix}/db/…/{ts}-{id}.json        manifest: time, ClipMux version, goose version,
                                    Postgres version, size, SHA-256, key version
{prefix}/sources/{asset_id}.age
{prefix}/sources/{asset_id}.deleted tombstone
```

**Promotion:** the first backup of a day is copied to `daily/`, of a week to
`weekly/`, and of a month to `monthly/`.

**Pruning (D146):** ClipMux prunes only when its key may delete; the target
probe finds out which. The recommended setup is a key that can't delete,
plus object lock and lifecycle rules, so ransomware on the host can't wipe
the backups. The probe then reports the target as "immutable".

#### Restore fence and store epoch (D144)

- **Epoch marker.** The leader writes `_clipmux/epoch.json` to each media
  store, holding the install ID and a generation. Each pass commits
  generation+1 to Postgres first, then writes the marker.
- **When it fences.** A store whose install ID differs, or whose generation
  is ahead of the database's, means the database went back in time or
  belongs to another install. Both fence.
  `clipmux admin restore` also sets the fence directly.
- **While fenced, nothing deletes from storage.**
  - These skip their deletes: `SweepOrphans`, `PurgeRetired`,
    `PurgeRetiredTracks`, `PurgeAbandoned`, `SweepUnlisted`, and the delete
    branch of `routes.Reconcile`.
  - `PublishKeys` keeps the union of the database's keys and the existing
    `_keys.json`.
  - Route upserts still run, so changes made after the restore still reach
    the edge.
- **The fence is visible** as the metric `clipmux_restore_fenced`, an
  alert, and a dashboard banner. It lifts only through
  `clipmux admin restore finish --yes`.

### Tasks

Order:

- T29.1 first.
- T29.2 before anything that restores.
- T29.3, then T29.4 and T29.5.
- T29.6–T29.8 in any order after T29.3.
- T29.9 alongside each slice; a change in behaviour updates its docs page in the same PR.
- T29.10 and T29.11 for the exit, then T29.12.

| Task | Scope |
| --- | --- |
| T29.1 | **Docs first.** This plan, a T29 row in P6 and exit check 10. Decisions D143 (built-in backups configured from env, encrypted with age), D144 (fence and epoch), D145 (recovery targets), D146 (sources-only mirror; pruning by lifecycle rules and keys that can't delete). Deferred items go to `docs/known-gaps.md`. |
| T29.2 | **Restore fence and store epoch.** <br>**Migration:** adds `install_epoch`, `restore_fence` (reason, backup ID and time, detected and lifted times) and `backups`. <br>**Epoch loop:** leader, every 5 min. <br>**Fenced deletes:** in `internal/retention/files.go` and `internal/routes/routes.go`. <br>**`clipmux admin restore report`:** lists the route records, output prefixes and sources the database doesn't know, read from store listings, each `manifest.json` and the route records. <br>**`clipmux admin restore finish --yes`.** <br>**Tests:** an older database next to a newer filesystem store deletes nothing; a fresh database on a used store is fenced. |
| T29.3 | **Backup engine (`internal/backup`).** <br>**Target:** the existing `storage.S3` and `storage.Filesystem` adapters, through the `storage` interface; `S3.Put` already streams through the transfer manager. <br>**Leader loop `backups`:** runs `pg_dump -Fc` at idle priority, with the password in a 0600 `PGPASSFILE` rather than the environment. It encrypts with age, hashing with SHA-256 as it streams, writes the dump and then its manifest, records a `backups` row, promotes, prunes when allowed, and pings. <br>**Admin commands:** `clipmux admin backup now\|list\|check\|identity`. <br>**Image:** the PostgreSQL 18 client in its own Dockerfile stage, pinned in `toolchain/versions.env` under a `PGCLIENT_` prefix (D142); `toolchain/check_layers.sh --strict` must still pass. <br>**CI:** the `go` job installs `postgresql-client-18`. Tests skip without it unless `CLIPMUX_REQUIRE_MEDIA_TOOLS=1`; update `.github/CI.md`. |
| T29.4 | **Restore and the installer.** <br>**`clipmux admin restore [--list] [--latest\|--id ID\|--file PATH] --yes`:** <ol><li>Refuse while a `serve` holds the leader lock (`internal/sweeper/leader.go:60`).</li><li>Download, decrypt, check the SHA-256.</li><li>Refuse a dump whose schema is newer than the binary (the `checkNotNewer` rule, `internal/db/migrate.go:125-139`).</li><li>Take a `pre_restore` safety backup.</li><li>`pg_restore --clean --if-exists --single-transaction --no-owner`.</li><li>Set the fence and print the next steps.</li></ol>**`deploy/install.sh --restore`:** given the saved `.env`, brings up Postgres, runs the restore in a one-off app container, starts the stack and prints `restore report`. Document the same flow for `compose.proxy.yml` on the reverse-proxy page. |
| T29.5 | **Backup before migrating.** When `serve` boots or `admin migrate up` runs with migrations pending, take a `pre_migrate` backup first. If it fails, refuse to migrate and log why, unless `CLIPMUX_BACKUP_BEFORE_MIGRATE=false`. The row is recorded after migrating, because `backups` may not exist yet. No upgrade path can migrate without a fresh backup. |
| T29.6 | **Automatic restore check.** A weekly leader loop `backup_verify`: <ol><li>Fetch the latest backup, decrypt it and check its hash.</li><li>Run `pg_restore --list`.</li><li>If the role has `CREATEDB`, as the compose superuser does, restore into `clipmux_verify_<id>`.</li><li>Check that `projects.secrets_check` opens with the current key, that the content key versions load, and that core row counts are no lower than the manifest's.</li><li>Drop the database.</li></ol>Results go to `backups.verified_at` and `verify_error`. |
| T29.7 | **Source mirror.** <br>**Migration:** adds `assets.source_backed_up_at`. <br>**Leader loop:** copies each committed `uploads/{asset}/source` to `sources/{asset}.age`, 20 per pass, oldest first. <br>**Deletes:** a deleted asset gets a tombstone. Pruning is `clipmux admin backup prune-sources` when the key may delete, otherwise a lifecycle rule. <br>**`clipmux admin backup restore-sources [--asset ID\|--all]`:** copies sources back into the media store and queues re-encodes through the existing job path. |
| T29.8 | **Observability and the dashboard.** <br>**Gauges,** following the database-backed pattern in `internal/server/observe.go:210-289`: <ul><li>`clipmux_backup_last_success_timestamp_seconds`</li><li>`clipmux_backup_last_verified_timestamp_seconds`</li><li>`clipmux_backup_last_bytes`</li><li>`clipmux_backup_sources_pending`</li><li>`clipmux_restore_fenced`</li><li>`clipmux_data_free_bytes` (statfs of the data dir)</li><li>`clipmux_db_size_bytes`</li></ul>**Alerts:** `ClipMuxBackupStale` (older than twice the interval), `ClipMuxBackupVerifyFailing`, `ClipMuxRestoreFenced`, `ClipMuxDiskLow` (under 10% free). <br>**SLO recording rules:** API availability and p95 latency, from `clipmux_http_request_duration_seconds`. <br>**Management API:** backup status (last, verified, target, mode, fence) and "back up now"; kept complete by `internal/manage/docs_test.go`. <br>**Dashboard, Settings → Backups:** a status card, the "not off-site" warning, and a checklist item "Recovery kit stored off this server" that the owner confirms. |
| T29.9 | **Docs.** <br>**Backups page:** rewrite `self-host/operate/backups.mdx` — automatic backups; the target, with a worked B2 example (object lock, lifecycle, a key that can't delete) and notes for R2, Hetzner and S3; the recovery kit; reading a backup with plain `age` and `pg_restore`. <br>**New `operate/disaster-recovery.mdx`:** the recovery-targets table as a runbook, one section per scenario, each with commands, expected time and how to verify. <br>**Corrections:** `operate/monitoring.mdx:27` should say compose declares the health check, not the image. Add the new metrics and alerts to the monitoring page, the env vars to the configuration reference (`internal/config/doc_test.go`), and the admin commands to the CLI page (`cmd/clipmux/docs_test.go`). <br>**Internal docs:** repoint the stale `docs/operations.md` references in `docs/known-gaps.md` and `docs/architecture.md:819`. <br>**Error codes:** add any new API error codes to `skills/clipmux-integrate/references/errors.md`. |
| T29.10 | **Drills (exit check 10).** <br>**`TestDrillBackupAndRestore`** (`internal/e2e/drills_db_test.go:189-254`) is extended to run the real commands: <ol><li>Back up.</li><li>Upload a second asset.</li><li>Restore the older backup.</li><li>Check that the second asset's route record and output survive and that `restore report` lists them.</li><li>Finish the fence and play the video.</li></ol>**New `toolchain/check_restore.sh`,** styled like `check_release.sh`: a disaster-recovery drill on a fresh VM, timed per phase. The results are recorded as a decision. |
| T29.11 | **Capacity baseline.** A load test on a VM shaped like prod (arm64, 2 vCPU, 4 GB), extending the P5 soak harness. It measures API p50 and p95 on the main routes as the request rate rises, plus claim latency and webhook delivery latency. The numbers and SLOs go in a capacity section of `requirements.mdx`, which also states, with numbers, that uploads and playback bypass the server on R2 and the Worker. |
| T29.12 | **Prod cutover.** <ol><li>Set `CLIPMUX_BACKUP_*` on the VPS: the B2 key that can't delete, a 15 min interval, the owner's age recipient.</li><li>Confirm the first backup and the first verify pass.</li><li>Run `check_restore.sh` against the prod backup on a throwaway VM.</li><li>Drop the ClipMux leg of the Track A timer and keep the Edural one.</li><li>Then do A7.</li></ol> |

### Run the capacity baseline

Run on an ARM64 Linux VM with 2 vCPU and 4 GiB available to the benchmark
and PostgreSQL 18. Point the test at a disposable database. The harness
builds the binary, seeds a 100-asset page, and runs four one-minute
stages at 10, 50, 100 and 200 API reads/s while completing one synthetic
upload/s. It prints per-route p50/p95, claim and delivery latency, HTTP
failures, rate limits and requests dropped by the load generator.

```bash
make db-up
CLIPMUX_E2E=1 CLIPMUX_E2E_CAPACITY=1 \
  TEST_DATABASE_URL='postgres://clipmux:clipmux@127.0.0.1:5444/clipmux_test?sslmode=disable' \
  go test -v -count=1 -run '^TestRecoveryCapacity$' ./internal/e2e
```

`CLIPMUX_E2E_CAPACITY_DURATION=5s` runs a quick harness check. Record the
one-minute stages, VM shape, Postgres version and any concurrent load with
the published baseline.

### Execute the production cutover

The production stack uses `/opt/clipmux-v2` and `compose.proxy.yml`.
The pre-cutover inspection on 2026-11-23 found a pre-backup image, no
`CLIPMUX_BACKUP_*` settings in the stack's environment file and no active
`vps-backup.timer`. The locked B2 target and verified off-host safety
copy are recorded in D145.

1. Confirm the currently deployed image, current backup coverage and the
   restart window with the owner. Stage a build containing the T29
   migrations, backup commands and PostgreSQL 18 client. Save the previous
   image reference and stack configuration off-host. If no off-host backup
   exists, take and verify the interim Track A dump before restarting
   production.
2. Put the B2 bucket URL, bucket-scoped write key without `deleteFiles`,
   owner’s public age recipient and `CLIPMUX_BACKUP_INTERVAL=15m` into the
   secret-managed production environment. Keep `readBucketRetentions` on
   the write key so the probe can see Object Lock. Keep a separate read-only
   backup key and the recovery kit off-host.
3. Roll out the backup-capable app and confirm `/readyz`. On the stack, run
   `docker compose exec -T app clipmux admin backup check` and confirm the
   B2 target reports Object Lock and a 15-minute schedule. Use
   `docker compose exec -T app clipmux admin backup list` to record the first
   scheduled backup ID and verify it with
   `docker compose exec -T app clipmux admin backup verify --id ID`.
   The full check must restore into a scratch database and load the content
   keys. Confirm a second scheduled backup arrives on cadence.
4. On a disposable VM, run `toolchain/check_restore.sh --env KIT --yes`
   with the off-host read-only key in the copy of the kit. This uses
   `install.sh --restore-only` and leaves only Postgres running. The
   production drill verifies the exact restored backup and reads the media
   report. Use the script's self-contained mode with disposable buckets for
   the full installer and playback test. Record cold image pull, restore,
   first playback and post-backup media checks in D145.
5. After verification, remove only the ClipMux leg of the Track A timer
   if present. Confirm the Edural leg and its alert keep working. Do A7
   only with the owner's approval.

### Reuse

- **Backup target:** the `storage.Storage` interface with its `S3` and
  `Filesystem` adapters. `Check` is the model for the target probe.
- **Loops:** `sweeper.Every`, `Leader.Only`, and the `passTimeouts` map in
  `internal/server/server.go:129-150`.
- **Backup key:** `internal/secrets` for the key load and version checks;
  the backup identity is derived from that key.
- **Restore guards:** `db.Migrate`, `checkNotNewer`, and the `migrate`
  admin command pattern (`cmd/clipmux/admin.go:406-469`).
- **Metrics and alerts:** the database-backed gauges and the
  `deploy/prometheus/alerts.yml` format.
- **Tests:** `dbtest.New`, with the existing drill test as the base for
  T29.10.

### Deferred (recorded in known gaps)

- **Continuous WAL archiving (PITR).** It can be added on top of the dumps.
- **High availability:** more than one `serve` replica, or a warm standby
  that restores continuously ("pilot light").
- **Adopt from storage:** rebuilding asset and output rows from route
  records and `manifest.json`. Until then, the fence and the report keep
  that data safe.
- **A trash window for deleted assets.**

## Open questions

Each answer becomes a decision record before the task that needs it.

1. **The reboot window, and whether hosts reboot on their own** (A5). The
   owner picks a quiet hour for Edural's users.
2. **The hourly-dump retention for Edural** (A1). The default matches
   ClipMux: 3 days hourly, 35 days daily, 400 days monthly.
3. **Whether gms and the other databases on the host join the Track A
   timer** (A1).

## Verification

- `make test-db` and `make lint`. Run them locally with
  `CLIPMUX_REQUIRE_MEDIA_TOOLS=1`, so a missing `pg_dump` fails rather than
  skips.
- `make sqlc-check`, `make site-check` and `make snippets-test`.
- `toolchain/check_layers.sh --strict` between two images built with the
  pg-client stage.
- The e2e drill: back up, upload, restore the older backup, confirm nothing
  was deleted, finish the fence, play the video.
- `toolchain/check_restore.sh` on a fresh VM, with its time recorded.
- **On prod:**
  - B2 shows the hourly objects and healthchecks.io is green.
  - A laptop restore of both databases matches the row counts.
  - Every container is healthy after the reboot.
  - The external probes are green.
  - In a window the owner approves, the media-hostname segment probe
    passes while the ClipMux app is stopped for 60 s.
