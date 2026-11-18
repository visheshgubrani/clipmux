# Integrating ClipMux

The integration guide moved to the docs site, where it is kept current with
the code: start at the [quickstart](https://clipmux.com/docs/build/quickstart), then
[webhooks](https://clipmux.com/docs/build/webhooks), [playback](https://clipmux.com/docs/build/play/sign-tokens) and the
[production checklist](https://clipmux.com/docs/build/going-live/production-checklist). What
follows is the record of the first live integration, kept here for reference.

## Edural, the worked example

Edural's integration lives in its repository:
- `api/src/integrations/clipmux.ts`, the only importer of `@clipmux/node`;
- `api/src/modules/media/`, which holds state and the sweep;
- `sdk/src/react/player.tsx`, the student player;
- the portal's upload queue and preview.

| Edural route | ClipMux call |
| --- | --- |
| `POST /academies/{a}/lessons/{l}/video/uploads` | `uploads.create` (passthrough = attempt ID, idempotency key = attempt ID) |
| `POST /academies/{a}/video-uploads/{u}/complete` | `assets.get` → reconcile |
| `DELETE /academies/{a}/video-uploads/{u}` | `uploads.cancel` (best effort; the sweep deletes the asset) |
| `POST /hooks/clipmux` | `webhooks.verify` → `assets.get` → reconcile |
| `POST /academies/{a}/lessons/{l}/video/playback` (teacher preview) | `playback.session`, `vid = staff:<id>` |
| `POST /student/lessons/{l}/playback` | `playback.session`, `vid = student:<id>` or `anon:<random>` |
| minute sweep | `assets.get`, `assets.findByPassthrough`, `assets.delete`, `tracks.transcript` |

Verified live on 2026-11-15, against `app.clipmux.com` with `@clipmux/node`
0.1.0, `@clipmux/uploader` 0.5.0 and `@clipmux/player` 0.6.0 from npm:
- The browser upload made three calls (parts sign, part PUT to R2, complete), all 200.
- A 66-second clip was ready in about 80 s on Modal; `video.asset.ready` and `video.asset.track.ready` reached the backend through `listen`.
- The transcript was stored from the track event, and the AI draft started.
- The teacher preview and the student player both played from `edural.clipmux.com`: master, variant, key, segments, poster and subtitles all 200.
- A renewal was signed per viewer (15 min), and native renewals got 4 h tokens.
- A refused renewal returned `null`, and the app fell back to its enroll page.

## Troubleshooting (met during the Edural integration)

| Symptom | Cause | Fix |
| --- | --- | --- |
| `DELETE /v1/assets/{id}` → 500 `internal`; the server logs `delete tracks: … violates check constraint "tracks_check"` | servers before this fix couldn't mark an `errored` track deleted (for example chapters `llm_not_connected`) | fixed in the server (Unreleased); upgrade. Until then, retry from a sweep |
| Chrome logs `Refused to get unsafe header "Retry-After"` once per part | the uploader reads `Retry-After` from every part PUT, and the bucket's CORS doesn't expose it | harmless; fix pending in the uploader (known-gaps.md) |
| `ERR_PNPM_…` minimum release age on install | pnpm refuses packages younger than its threshold | keep the `minimumReleaseAgeExclude` entry pnpm wrote |
| Delivery answers 403 for a URL with a token | the token is malformed or signed with another app's key | check `CLIPMUX_SIGNING_KID` with `doctor`; an expired token is 401, not 403 |
| A chapters track always ends `errored` | no LLM connected for chapters | Captions settings → connect an LLM; existing assets need `tracks.create(id, { kind: 'chapters' })` |
