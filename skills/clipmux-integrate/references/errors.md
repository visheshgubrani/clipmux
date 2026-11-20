# ClipMux error codes and what to do

Every API error is `{"error": {"code": "…", "message": "…", …details}}`; the
SDK throws a `ClipMuxError` with `status`, `code` and `details`. Branch on
`code`. Only `rate_limited`, `idempotency_in_progress`, 5xx and network
errors are worth retrying, and the SDK and the elements already retry those.
A full page per code: https://clipmux.com/docs/reference/errors#<code>.

## From the API

| Code                      | Status | Do this                                                                                                                   |
| ------------------------- | ------ | ------------------------------------------------------------------------------------------------------------------------- |
| `invalid_request`         | 400    | Fix the request; `message` names the field. Don't retry it unchanged                                                      |
| `invalid_cursor`          | 400    | Drop the `cursor`, or pass the `next_cursor` you got                                                                      |
| `invalid_parts`           | 400    | Parts missing or ETags wrong: list stored parts, upload the rest, complete again (the uploader does this)                 |
| `import_url_refused`      | 400    | Import only public http(s) URLs without credentials, or upload the file                                                   |
| `unauthorized`            | 401    | Wrong or revoked API key or upload token: run `npx @clipmux/node doctor`                                                  |
| `token_expired`           | 401    | Sign a new playback token; `<clipmux-player>` refreshes by itself                                                         |
| `denied`                  | 403    | The token doesn't cover this playback ID or app, is poster-only, or its signing key is revoked. Don't retry               |
| `insufficient_scope`      | 403    | Create a key with the needed scope (defaults `uploads:write`, `assets:read`, `assets:write`; `analytics:read` on request) |
| `upload_mismatch`         | 403    | Use the upload token returned with this upload                                                                            |
| `not_found`               | 404    | No such object in this app (check the key is the owning app's), or revoked                                                |
| `agent_source_not_found`  | 404    | The PC agent's file is unknown or attached: `clipmux push --local` again                                                  |
| `pairing_not_found`       | 404    | Pairing unknown or already collected: run `init` again                                                                    |
| `share_not_found`         | 404    | The share link, its video or its app is gone                                                                              |
| `asset_deleted`           | 409    | The video was deleted; stop changing it                                                                                   |
| `asset_not_ready`         | 409    | Wait for `video.asset.ready` (the `asset` callback with `status: 'ready'`)                                                |
| `job_active`              | 409    | Another job runs on the video (`details.job_id`): wait, or pass `supersede: true`                                         |
| `no_audio`                | 409    | The video has no audio, so no subtitles                                                                                   |
| `subtitles_not_ready`     | 409    | Chapters need ready subtitles: make or wait for them                                                                      |
| `chapters_not_connected`  | 409    | The owner connects an LLM under Captions and AI in the dashboard                                                          |
| `track_not_ready`         | 409    | Wait for `video.asset.track.ready`                                                                                        |
| `source_missing`          | 409    | The source wasn't kept; it can't be re-encoded                                                                            |
| `source_unavailable`      | 409    | The source is on a revoked PC agent; push it again                                                                        |
| `too_many_playback_ids`   | 409    | At most 1000 per video: revoke some                                                                                       |
| `upload_not_active`       | 409    | The upload finished, was cancelled or expired: create a new one with a new Idempotency-Key                                |
| `upload_missing`          | 409    | Storage lost the parts: create a new upload                                                                               |
| `upload_expired`          | 409    | Not completed in time: create a new upload                                                                                |
| `size_mismatch`           | 409    | The file's size differs from the declared `size`: new upload with the right size                                          |
| `idempotency_in_progress` | 409    | Same key still running: wait and retry with the same key                                                                  |
| `share_expired`           | 410    | Make a new share link                                                                                                     |
| `share_revoked`           | 409    | Make a new share link                                                                                                     |
| `idempotency_key_reused`  | 422    | Use a new Idempotency-Key for a different request                                                                         |
| `too_large`               | 413    | Over the server's or storage's limit; set the uploader's `max-size` to tell users early                                   |
| `rate_limited`            | 429    | Wait `Retry-After` (`details.retry_after`) seconds; the SDK already does                                                  |
| `too_many_pairings`       | 429    | Ten pairings wait for the owner: approve or let one expire                                                                |

## From the SDK and the route handler

| Code                                                       | Where                                                            | Do this                                                                                                   |
| ---------------------------------------------------------- | ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| `ClipMuxConfigError`                                       | Thrown when a call needs a missing `CLIPMUX_*` variable          | Run `init`, or set the named variable server side                                                         |
| `missing_header`, `bad_signature`, `stale`, `invalid_body` | `WebhookVerificationError.reason`; the webhook route answers 400 | Wrong secret, a re-serialized body (Express: before `express.json()`), or a clock more than 5 minutes off |
| `clipmux_misconfigured`                                    | 500 from your route to the browser                               | The server's key or app is wrong: run `doctor`                                                            |
| `clipmux_unavailable`                                      | 502 from your route to the browser                               | ClipMux unreachable or failing; the elements retry                                                        |
| `unsupported_media_type`                                   | 415 from your route                                              | The browser routes take only `Content-Type: application/json`                                             |
| `network`, `timeout`, `asset_errored`                      | `ClipMuxError.code` with status 0, or from `waitUntilReady`      | Unreachable server, or the video failed: read its `error_code` below                                      |

## A video's `error_code` (status `errored`)

| Code                                                                      | Meaning, and what to do                                                                       |
| ------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------- |
| `upload_expired`, `upload_cancelled`                                      | Never uploaded: upload again                                                                  |
| `import_failed`, `import_too_large`, `import_url_refused`                 | The URL import failed: check the URL is public and within limits                              |
| `unsupported_source`                                                      | Not a video ffmpeg can read: tell the uploader                                                |
| `transcode_failed`, `processing_failed`, `commit_failed`, `upload_failed` | Encoding failed after retries: the job's logs in the dashboard say why; `reencode` once fixed |
| `source_download_failed`, `source_changed`, `source_missing`              | Storage problem on the server: tell its owner                                                 |
| `scratch_full`, `job_timeout`, `worker_lost`, `worker_stopped`            | Worker capacity: the owner checks workers, then `reencode`                                    |
| `agent_unavailable`                                                       | The PC agent holding the source is offline                                                    |
| `app_disabled`                                                            | The owner disabled the app                                                                    |

## A track's `error_code` (subtitles or chapters)

| Code                                                                                                                      | Meaning, and what to do                                                                             |
| ------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `no_audio`                                                                                                                | Nothing to transcribe                                                                               |
| `transcribe_failed`, `transcribe_unavailable`, `transcript_invalid`                                                       | Speech recognition failed or has no worker: retry with `tracks.create` later                        |
| `speech_api_rejected`, `speech_api_unavailable`                                                                           | The owner checks the speech API's key and model                                                     |
| `subtitles_failed`                                                                                                        | Chapters need subtitles, which failed                                                               |
| `llm_not_connected`, `llm_unavailable`, `model_rejected`, `model_refused`, `invalid_model_output`, `transcript_too_large` | Chapters failed at the LLM: the owner checks its connection; the video and subtitles are unaffected |
