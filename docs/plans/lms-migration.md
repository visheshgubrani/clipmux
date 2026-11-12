# LMS migration

The LMS (private `visheshgubrani/LMS`, the product Edural on edural.com) moves
from `clipmux-lms` to ClipMux. Since D114 this is a replacement, not a
migration: the LMS has no users and no videos to keep. `clipmux-lms` is
switched off once the new instance passes its setup test.

The LMS side is Edural's phase P36 (`docs/plans/P36-clipmux-go.md` in the LMS
repository). It holds the flow-by-flow mapping from the old API to this one.

## What the LMS uses (verified 2026-11-12)

- One server adapter, `api/src/integrations/clipmux.ts`. Only the media
  module imports it. Today it is configured by `CLIPMUX_API_URL`,
  `CLIPMUX_API_KEY` and `CLIPMUX_WEBHOOK_SECRET`.
- Vendored `@clipmux/uploader` 0.2.0 and `@clipmux/player` 0.2.0 (Vidstack),
  packed from a `clipmux-lms` commit by `scripts/bundle-clipmux.sh`.
- One webhook, `POST /api/v1/hooks/clipmux`, with HMAC over `${ts}.${body}`
  (the same scheme and `X-Webhook-*` headers ClipMux's webhooks use). Event names are ignored, and the video is
  re-fetched. A one-minute reconcile job does the same.
- One organization for all academies. Tenant separation lives in the LMS,
  through the upload-attempt ID, which becomes `passthrough`.

## What it needs from ClipMux

| LMS need | ClipMux equivalent |
| --- | --- |
| A signed upload tied to an LMS record | `POST /v1/uploads` with `passthrough`, created by the LMS server; the browser sends the bytes with `@clipmux/uploader` 0.4.0 |
| Find a video by LMS reference | `GET /v1/assets?passthrough=` |
| Signed HLS, renewable in the player | The LMS signs ES256 tokens with `vid` (`@clipmux/node` `playback.session`); `<clipmux-player>` 0.5.0 renews through `getToken` |
| A webhook trigger, then a re-fetch | Asset webhooks + `GET /v1/assets/{id}` |
| Delete a video | `DELETE /v1/assets/{id}` |
| Subtitles, chapters, transcript for the AI features | `generate: {subtitles, chapters}`; the transcript at `GET /v1/assets/{id}/tracks/{tid}/transcript` |
| Edge delivery | The Cloudflare Worker on `media.clipmux.com`, R2 storage |
| Encoding | Modal, with no fallback (the host is too small for the built-in worker) |

## Cutover (D114)

1. **ClipMux prerequisites:**
   - an amd64 and arm64 `sha-` image from `main`;
   - the proxy override `deploy/compose.proxy.yml`;
   - Connect naming the Worker after its hostname.
2. **Install.** On the VPS, beside the legacy stack, behind the host's Caddy at `app.clipmux.com` (DNS-only). Then the owner and the URLs.
3. **Connect.** Detach the legacy Worker from `media.clipmux.com`, then Connect Cloudflare there with a new bucket. Connect Modal, then an LLM for chapters. The setup test passes.
4. **Credentials.** `clipmux admin create-integration --app edural --webhook-url https://api.edural.com/api/v1/hooks/clipmux`, into the LMS's `api.env` only.
5. **The LMS.** It ships P36 and passes its end-to-end checks.
6. **Remove `clipmux-lms`:**
   - its stack, after a final dump;
   - its Caddy blocks;
   - its Worker and buckets;
   - its Modal app;
   - its queues, analytics and OAuth apps.

   Keep `uploads.clipmux.com`: it is the LMS's own file bucket, not video.
