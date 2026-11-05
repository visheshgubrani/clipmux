# LMS migration gate

The LMS (private `visheshgubrani/LMS`) moves from `clipmux-lms` to ClipMux once
ClipMux reaches Phase 2 (core + Cloudflare delivery) **and** every item below is
met. Then `clipmux-lms` retires.

## What the LMS uses today (verified 2026-11-05)

- One server adapter: `server/src/utils/clipmux.ts` + `clipmuxTranscription.ts`,
  configured by `CLIPMUX_API_URL` / `CLIPMUX_API_KEY`.
- Browser uploads through vendored `@clipmux/uploader` 0.2.0; playback through
  vendored `@clipmux/player` 0.2.0 (`client/vendor`, `dogfood-client/vendor`).
- One webhook, `POST /api/v1/callback/clipmux`, HMAC over `${ts}.${body}`; event
  names are ignored — it re-fetches the video. A 60 s reconciliation job does the same.
- All academies share one clipmux organization; tenant separation lives in the
  LMS through `correlation_ref` (the LMS upload-attempt ID).

## Gate

| LMS need | ClipMux equivalent | Status |
| --- | --- | --- |
| Single-file signed upload token tied to an LMS record | `POST /v1/uploads` with `passthrough` + `upload_token` | Phase 1 |
| Find a video by LMS reference (recovery) | `GET /v1/assets?passthrough=` | Phase 1 (T05) |
| Signed HLS, renewable in the player | App backend signs ES256 tokens; `@clipmux/player` (Video.js) `getToken()` | Phase 1 — LMS signs tokens itself instead of calling `/playback-token`, and swaps its vendored Vidstack player for the Video.js one |
| Webhook trigger + re-fetch | Outbox webhooks + `GET /v1/assets/{id}` | Phase 1 |
| Delete a video | `DELETE /v1/assets/{id}` | Phase 1 |
| Auto subtitles, chapters, transcript VTT (feed AI summary/quiz/chat) | **Not designed** | Decide: add to ClipMux before migration, or move transcription into the LMS |
| Edge delivery at today's scale | Cloudflare Worker + R2 | Phase 2 |
| Existing library | Import from URL from clipmux-lms R2 (or a one-off copy job) | Phase 2 |

## Cutover sketch

1. Create an App for the LMS; store its API key, signing key and webhook secret in LMS env.
2. Add a second adapter in the LMS behind the same interface; new uploads go to ClipMux.
3. Import the existing library; map old video IDs to new asset/playback IDs by `passthrough`.
4. Switch playback; keep clipmux-lms read-only for a rollback window; then retire it
   and move `clipmux.com` hostnames as planned in launch.md.
5. Update the LMS `vendor/manifest.json` source URLs (they still name the old repo).
