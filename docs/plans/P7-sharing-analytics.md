# Phase 7 — Sharing and analytics

**Status: closed with open items (D107, 2026-11-11).** The features ship; what the exit didn't prove is in [known-gaps.md](../known-gaps.md).

Source: [architecture.md](../architecture.md), "Integration flow" (share
links) and "Tokens" (the `vid` claim), with [decisions.md](../decisions.md)
where they differ (D102–D104 set this phase's scope). Phase 7 adds the two
things v1 promised and doesn't have:

- **Share links for private videos.** The owner copies a link to a signed
  video for someone with no account. The link opens a small page on the
  public URL. That page fetches short-lived tokens for the link's own signed
  playback ID. A link can expire, and revoking it stops playback. A plain HLS
  URL is offered beside it for players that can't open a page.
- **Viewer analytics behind the API D100 fixed.** Views, watch time,
  bandwidth and unique viewers are counted where delivery answers the viewer,
  for every player. They are rolled up in Postgres and served by
  `GET /v1/analytics`, which the dashboard is already built against.

It runs beside Phase 6's remaining tasks, and P6-T19 (the LMS cutover) waits
for its exit (D102). The release candidate the LMS installs then carries
Phase 7, and the soak exercises delivery counting on real traffic.

The integration API only grows. The output format, the worker path grammar
and the token claims don't change: `vid` was already a claim, and it is now
read. Delivery's URLs stay as they are; the route record gains one additive
field, `share`.

**Before this phase:** Track C's dashboard work (D101) is committed.

**Exit test:**

1. **A share link plays.** In a fresh browser profile with no session, a
   share link to a signed video plays, with subtitles.
   - Revoked, it stops within 30 s on built-in delivery and within 65 s on
     the real Worker.
   - Expired, it shows "expired", and `POST /s/{secret}/playback` answers
     `410 share_expired`.
   - Its HLS URL plays in hls.js and ExoPlayer without the page.
2. **Both delivery adapters count alike.** The Go handler and the Worker
   agree with every counting vector in `internal/delivery/testdata/vectors.json`.
3. **Built-in counts are exact.** A scripted set of viewers through built-in
   delivery gives exactly the expected `totals`, `series`, `previous` and
   `top_assets`.
   - Dashboard previews, library posters and the setup test add nothing.
   - A fetch the Worker forwards to origin counts once.
4. **Edge counts are pulled exactly.** On the real zone (D090), the scripted
   playback through the Worker reaches `/v1/analytics` after a pull. The
   pulled views, watch seconds, bytes and viewers equal Analytics Engine's
   own weighted sums exactly, and the script within 1% (at least one whole
   unit), since Analytics Engine samples even one viewer's burst (D106).
5. **Counts survive a stop.** SIGTERM loses no counts; `kill -9` loses at
   most one 15 s flush.
6. **Nothing leaks.** The suites' log checks find no share secret, token or
   `vid` in any log line.
7. **The dashboard passes.** Every dashboard suite passes in both projects
   (desktop Chromium and 375 px), with no CSP violation and no horizontal
   scroll.

## Design notes

### Analytics: what counts (D103)

Counting happens in the delivery adapter that answers the viewer: the
built-in handler, or the Cloudflare Worker.

A response is counted only when all of these hold:

- it is a `GET`, not a `HEAD`;
- its status is 200 or 206;
- its route is active.

Refusals never count. A 206 that doesn't start at byte 0 counts its bytes
only: views and watch time need the response from the first byte, so a
player's later ranges don't count a segment twice.

| Metric | Rule |
| --- | --- |
| `views` | One per generated `master.m3u8` |
| `watch_seconds` | `media.SegmentSeconds` (6) per `audio/seg_*.m4s`. An output with no audio counts its video segments. Audio is one rendition, so an ABR switch doesn't count twice. It overcounts by the player's buffer-ahead, about 30 s per view. |
| `bytes_delivered` | Body bytes of every counted response: segments, init, playlists, the key, tracks, the poster |
| `unique_viewers` | Distinct viewer keys. A viewer key is HMAC-SHA256(analytics key, `app_id ‖ 0x00 ‖ vid`) cut to 16 bytes, from tokens that carry `vid`. Keys are kept per day per asset, and a range counts distinct keys across its days. `null` when no counted view in the range had a `vid` |

Two kinds of request are never counted:

- **Tokens signed by the internal key,** unless the playback ID belongs to a
  share link. This leaves out dashboard previews, library posters and the
  setup test.
  - Built-in delivery knows the internal key by its null `app_id`.
  - The Worker knows it the same way, from `_keys.json`.
  - Share playback IDs carry `"share": true` in their route record.
- **Fetches the Worker forwards to origin.** They carry `X-ClipMux-Edge: 1`
  and are counted at the edge instead. Spoofing the header hides only the
  spoofer's own requests.

**Privacy:** nothing stores an IP, a User-Agent, a referrer, a token or a raw
`vid`. Viewer keys are keyed hashes, so a student ID can't be recovered from
one by guessing.

### Analytics: storage

Postgres, not a second database (D103). The API needs hourly totals per
playback ID, which stay small. A busy LMS adds about 12,000 rows a day.

- **`playback_hourly`**: `project_id, source, hour, app_id, asset_id,
  playback_id, views, watch_seconds, bytes`, keyed by
  `(project_id, source, hour, asset_id, playback_id)`, with an index on
  `(project_id, asset_id, hour)`. `source` is `origin` or `edge`.
  - Origin rows **add** (`views = views + excluded.views`), so each replica
    flushes on its own.
  - Edge rows are **replaced** with absolute values recomputed per hour, so
    pulling an hour again never counts it twice.
- **`playback_viewers`**: `(project_id, day, asset_id, viewer)`, plus
  `app_id`. One row per viewer per asset per day.
- **The analytics key** is a random 32-byte secret per project, generated at
  boot like the content key secret and sealed. It joins `secrets.Columns`.
- **Daily rollup (T03):** `playback_daily` (the same totals per UTC day) and
  `playback_viewer_days` (one row per viewer per app per day) are written in
  the same flush. Ranges of a week or more read them; only `24h` reads hourly
  rows. Measured on 180 days of 1,157 playback IDs played every hour (5
  million hourly rows' worth) and 2.08 million viewer rows, a warm 90-day
  read took 189 ms for the project, 221 ms for one app and 2 ms for one asset
  (`CLIPMUX_ANALYTICS_SCALE=1`). Without the rollup it took 1.8 s, 5.7 s and
  1.1 s.
- **Retention (D105):** hourly rows go after 14 days; daily rows and both
  viewer tables after 180, which is what two 90-day ranges need. There are
  no lifetime totals.

### Analytics: collection

**Built-in delivery** records into `internal/analytics`:

- `serve()` wraps the writer in a byte counter that keeps `Unwrap` and
  `ReadFrom`. After the response it calls `Handler.Record` with an
  `analytics.Event`: the route, the file kind, the bytes, the views, the
  watch seconds and the token's `vid` (T01). serve sets `Record` to the
  recorder's (T02).
- The recorder aggregates in memory, capped at 100,000 keys. A full map
  flushes early.
- A flusher writes one batched `unnest` upsert every 15 s. serve owns it and
  waits for it, but it isn't gated on the leader: each replica flushes its
  own counts. A last flush runs after `srv.Shutdown`.

**The Worker** writes to Workers Analytics Engine:

- It parses `vid` and computes the viewer key with a new `ANALYTICS_KEY`
  Worker secret.
- It writes one data point per counted response:
  - blobs: project, app, asset, playback ID, kind and viewer key;
  - doubles: bytes and watch seconds;
  - index: the asset.
- Connect adds the `analytics_engine` binding and the secret, and the Worker
  version bumps. The existing `needs_redeploy` asks the owner to redeploy.
- Workers Paid includes 10 million data points a month, about 8,300
  viewer-hours. Cloudflare doesn't bill Analytics Engine yet.

**Pulling the edge into Postgres:** "Connect edge analytics" on the Delivery
page takes a second Cloudflare token with only **Account Analytics: Read**.

- Connect Cloudflare's own token is forgotten after setup and can't be
  reused, so this is a separate token. It is read-only.
- The token is sealed like the AI keys and checked with one SQL API query.
- A leader loop, `analytics_edge`, runs every 5 minutes. Each pass
  recomputes the hours from the watermark minus one hour up to now, with
  `_sample_interval`-weighted sums filtered by project. Viewer keys come
  from master rows grouped by day, asset and key.

### Analytics: API

- **`GET /v1/analytics?range=&app_id=`** and
  **`GET /v1/analytics/assets/{id}?range=`**, as D100 fixed them:
  - `series` buckets are hourly for `24h` and UTC days otherwise,
    zero-filled, oldest first;
  - `previous` is the range of the same length just before;
  - `top_assets` lists at most 10 live assets by views;
  - totals include deleted assets' history.
- **Added: `collecting`**,
  `{builtin: bool, edge: "connected" | "not_connected" | "not_used"}`. With
  Cloudflare delivery and no analytics token, the dashboard can say "Connect
  edge analytics" instead of showing silent zeros.
- **Added: the `analytics:read` scope.** An API key with it reaches the same
  routes for its own app's assets only, so an app such as the LMS can read
  its lessons' views. The scope is opt-in and not among the default scopes.
  The owner's session sees every app.

### Share links (D104)

**A link** is a row in `share_links`:
`id (shr_…), project_id, app_id, asset_id, playback_id, name, expires_at,
secret_hash, secret_sealed, created_at, revoked_at`.

- **Its own playback ID.** Each link owns one signed playback ID, created in
  the same transaction through `AddPlaybackID`'s path. It counts toward the
  1000 IDs an asset may have.
- **The URL** is `{public_url}/s/{secret}`, where the secret is
  `crypto/rand.Text()` (about 130 bits).
  - Lookup is by the secret's SHA-256.
  - The sealed copy lets the owner copy the link again, and it joins
    `secrets.Columns`.

**Routes** are integration routes. The owner's session reaches them as the
asset's app, as it does playback IDs today.

- `POST /v1/assets/{id}/shares` (`assets:write`), `{name?, expires_at?}`.
  It is idempotent, and allowed while the asset is still processing.
- `GET /v1/assets/{id}/shares` (`assets:read`). Each link has a `status`
  (`active`, `expired` or `revoked`) and its `views_30d`.
- `DELETE /v1/shares/{id}` (`assets:write`). It revokes the link's playback
  ID through the existing unpublish: `200` once revoked, `202` while
  revoking.
- `POST /v1/shares/{id}/stream-url` (`assets:write`) is the HLS URL for
  other players. It is a `v` token signed with the internal key, and its
  `exp` is the earlier of the link's expiry and the project maximum. It
  can't refresh, and revoking the link ends it.

**The public endpoint** is `POST /s/{secret}/playback`, `{viewer_id?}`.

- It needs no session or key and answers the link's playback:
  `{title?, duration_seconds, base_url, playback_id, token, expires_at,
  poster_url, tracks}`.
- **Its token:**
  - signed with the internal key;
  - `aud` is `v`, and `sub` is the link's playback ID;
  - `exp` is the earlier of the link's expiry and now + max(2 × duration,
    4 h);
  - `vid` is `viewer_id` when it matches `[A-Za-z0-9_-]{16,64}`.
- **Errors:**
  - `404 share_not_found` for an unknown or revoked link, or a deleted
    asset;
  - `410 share_expired`;
  - `409 asset_not_ready`.
- **Limits:** successes are 2 a second per address, with bursts of 20.
  Failures count toward the playback refusal limiter (D067).
- **Logging:** the route names the secret's wildcard `{token}`, so the
  access line redacts it (D064), and `logging.Scrub` learns the `/s/` path.

**The page** is `GET /s/{secret}`, a second Vite entry in the dashboard
(`share.html`) with `<clipmux-player>` and none of the dashboard's bundle.

- **Headers:**
  - its own CSP: scripts from self, media and connections to the media URL;
  - `frame-ancestors 'none'`;
  - `Referrer-Policy: no-referrer`;
  - `X-Robots-Tag: noindex`;
  - `no-store`.
- **Tokens:** the HTML is static. Its script reads the secret from the path
  and calls `/playback`, and `getToken()` refreshes through the same call.
- **The viewer ID** is a random 128-bit value in `localStorage`, read and
  written in `try`/`catch`. It lets share links count unique viewers.
- **States:** playing, expired, not found and still processing, each laid
  out down to 375 px.

**Expiry** is enforced twice:

- Tokens never outlive the link.
- A leader loop, `shares`, runs every minute and revokes expired links'
  playback IDs through the outbox, so their route records also leave the
  Worker.

**The other paths that pick a playback ID** leave share IDs alone:

- Dashboard previews (`firstLive`) and library posters (`posterID`) skip
  them.
- "Make private" never revokes them.
- The playback IDs table labels each one with its link's name.

Deleting an asset already unpublishes every ID, so its links read `revoked`.

**Why not a presigned URL:** a presigned storage URL hands out AES-128
ciphertext with no key, since keys are derived at delivery. It also can't be
revoked, and it bypasses route records. A link to a page that mints tokens
keeps every delivery check.

### Threats added

| Threat | Control |
| --- | --- |
| A share link is passed on | It plays until it expires or is revoked; one link per recipient, so one can end without the others; revocation lands within the route cache (30 s built-in, 60 s at the Worker) |
| Share secrets guessed | About 130 bits, looked up by hash, with failures counted by the playback refusal limiter |
| A share's HLS URL leaks | It lasts at most the project maximum (7 days) and ends when the link is revoked |
| Analytics identify a viewer | Only keyed hashes of `vid` are kept, per day, for 180 days; no IP, User-Agent or token is stored |

## Tasks

Order:

- **T01** first: the vectors set the counting behaviour for both adapters.
- **Two tracks then run alongside,** because they touch different files:
  - analytics, T02 then T03;
  - share links, T06 then T07.
- **T04 then T05** follow T02.
- **T08** follows T03, T05 and T07.
- **T09 and T10** follow T08.
- **T11** is last.

| Task | Scope | Port from |
| --- | --- | --- |
| T01 | **Counting vectors** (done). Each counted case in `internal/delivery/testdata/vectors.json` carries the event delivery records (kind, bytes, views, watch seconds, `vid`); a case without one records nothing. `viewer_keys` pin `analytics.ViewerKey`. Built-in delivery emits the events through `Handler.Record` with a byte counter, skipping the internal key and `X-ClipMux-Edge`, so the vectors are green on the Go side. | D015, D036 |
| T02 | **`internal/analytics`** (done): migration 00028 (`playback_hourly`, `playback_viewers`, the analytics key), the recorder, the flusher with its final flush, and the retention rule. serve wires `Handler.Record` (from T01) to the recorder. Tests on real Postgres with literal counts. | `internal/retention`, `clipmux-lms` `delivery/src/index.ts` (`logBandwidth`) |
| T03 | **The analytics API** (done) and the `analytics:read` scope. Ranges, buckets and `previous` are tested against worked examples. The 90-day query is measured on 5 million seeded rows. `docs/api.md` moves the routes out of "Planned". | D100 |
| T04 | **The Worker's counting** (done). The Worker parses `vid`, hashes it under `ANALYTICS_KEY`, and writes one data point per counted response (D103 has the layout), checked against every vector in Node and run in workerd. Origin fetches carry `X-ClipMux-Edge: 1`. Route records gain `no_audio` and `share` (the flag is set from T06). Connect binds the `clipmux_delivery` dataset, `PROJECT_ID` and `ANALYTICS_KEY`; the rebuilt bundle changes the Worker version, so connected installs see `needs_redeploy`. | `clipmux-lms` `delivery/wrangler.jsonc`, `delivery/src/index.ts` |
| T05 | **Edge pull** (done). `POST`/`GET`/`DELETE /v1/cloudflare/analytics` keep a sealed read-only token (migration 00029, `edge_analytics`), checked with `SHOW TABLES`. The `analytics_edge` leader loop runs every 5 minutes: each pass recomputes from an hour before its watermark, at most a day at a time, replaces those hours' edge rows, rebuilds the edge daily rows of the days they touch, and adds the edge viewers. History starts 13 days back, inside the hourly retention. Tested against an `httptest` fake of the SQL API that weights by `_sample_interval`, including a repeated pull that changes nothing and a late point. Share links take migration 00030. | `clipmux-lms` `server/src/routes/usage.ts` |
| T06 | **Share links** (done): migration 00030 and `internal/shares`, the routes, `/s/{secret}/playback`, the stream URL, the `shares` expiry loop, and previews and posters skipping share IDs. Route records and a new vector mark share IDs, so both adapters count them. Tests on real Postgres through built-in delivery. | architecture "Integration flow" |
| T07 | **The share page** (done): `dashboard/share.html`, a second Vite entry in plain TypeScript with `<clipmux-player>` and none of the dashboard's bundle; `GET /s/{token}` (`web.Share`) with the dashboard's CSP minus the storage buckets, `noindex` and `no-store`; `/s/` reserved from the dashboard's fallback. (The redaction of `/s/` landed in T06.) Playwright in both projects with `watchCSP` and `noHorizontalScroll`: plays with subtitles in a fresh context, revoked, expired, unknown. | `dashboard/src/player.tsx` |
| T08 | **The dashboard** (done): share links on the video page's Sharing tab (create with a name and an expiry, copy link, the stream URL in a dialog, revoke with `useConfirm`, views per link), and the playback IDs table naming each link's ID; Analytics live, with the "isn't counted yet" notice when `collecting.edge` is `not_connected`; the edge analytics card on Delivery. Playwright in both projects: a link made, watched with no session, its view counted, its stream URL played and revoked; Analytics counting a real play; the edge states from the API's answers. | `dashboard/src/pages/video.tsx`, `analytics.tsx`, `delivery.tsx` |
| T09 | **Docs** (done): architecture's Delivery gains an Analytics section and the route record's `no_audio` and `share`; Integration API gains the share and analytics routes and a Share links section; six threat rows. operations gains Analytics (what is counted, the flush, the metrics), its retention rows, what backups carry and three troubleshooting rows. cloudflare.md gains Edge analytics (the dataset, what a data point holds, the read-only token, the limits). api.md and known-gaps lose their stale pointers. | — |
| T10 | **The real zone** (done, D106): staging re-provisioned on its own Postgres; Connect binds the dataset; the read-only token pulls; the pull equals Analytics Engine's weighted sums exactly; Analytics Engine samples even one viewer, so the script doesn't match; a revoked share link stopped at the edge inside 65 s. An origin bypass is unexplained. | D090 |
| T11 | **Exit** (closed with open items, D107): items 2, 3, 5 (SIGTERM) and 7 passed; 1, 4 and 6 partly, with what's open in known-gaps. The owner decides which P6-T19 needs. | — |

## Open questions

Each answer becomes a decision record before the task that needs it.

1. **What `vid` does the LMS sign?** (P6-T19) Recommendation: its student
   ID. ClipMux keeps only a keyed hash, and the student sees no more than
   their own ID in the URL.
2. **Is a 180-day retention right for every install?** (T02) Answered by
   D105: fixed at 180 days in v1, the most the API's ranges need. A setting
   can come when someone asks for longer.
3. **One Analytics Engine dataset per Cloudflare account, or one per
   install?** (T04) Answered in T04 as recommended: one dataset, `clipmux_delivery`. Every
   data point carries its project, and the pull filters on it, so two
   installs on one account don't mix.
4. **Can a link's expiry be changed?** (T06) Answered in T06 as
   recommended: not in v1.
   Revoke the link and create another; the old recipients stop either way.
