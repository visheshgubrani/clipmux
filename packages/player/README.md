# @clipmux/player

Plays ClipMux videos with Video.js 10 and refreshes expired playback tokens
without losing the viewer's place.

```html
<script type="module">import '@clipmux/player/element'</script>
<clipmux-player playback-id="pid_…" session-url="/api/clipmux/playback"></clipmux-player>
```

With `session-url` the page needs only the playback ID: the element POSTs
`{"playback_id": "pid_…", "native": false}` to your backend, which checks the
viewer and answers with the playback session, the same shape as ClipMux's
`POST /s/{secret}/playback`:

```json
{
  "playback_id": "pid_…",
  "base_url": "https://media.example.net",
  "token": "eyJ…",
  "expires_at": "…",
  "poster_url": "https://media.example.net/v/eyJ…(poster-only token)/pid_…/poster.jpg",
  "title": "Lesson 42",
  "duration_seconds": 612.4,
  "tracks": [{ "id": "trk_…", "kind": "chapters", "status": "ready" }]
}
```

The `@clipmux/node` handler's `/playback` route answers exactly this, and its
`PlaybackSession` type is assignable to this package's. The
element plays from `base_url` with `token`, shows `poster_url` unless you set
`poster`, marks the first ready chapters track unless you set
`chapters-track`, and asks the same URL again whenever the token expires. A
`401` or `403` from it fires `clipmux-denied`; any other failure,
`clipmux-error` with its `status`. `native` is `true` when the browser will
play HLS itself (no MediaSource), where tokens can't be refreshed, so the
backend signs one that outlives the session. A `getToken` you set as well wins
for refreshes; `player.session` holds the answer in use.

Or bring your own token. The element asks `getToken` for the first token and
for every refresh, saying which playback ID and whether the browser plays HLS
natively:

```ts
import '@clipmux/player/element'

const player = document.querySelector('clipmux-player')!
player.getToken = async ({ playbackId, native }) => {
  const res = await fetch(`/api/playback-token?playback_id=${playbackId}&native=${native}`)
  return res.ok ? res.text() : null // null denies
}
```

```html
<clipmux-player base-url="https://media.example.net" playback-id="pid_…"></clipmux-player>
```

How refresh works: tokens ride in the URL path, so on a `401 token_expired` the
player calls `getToken({ playbackId, native })` once, puts the new token into that request and every
later one, and retries; hls.js never sees the error. `null` (or a `403` from
delivery) stops playback and fires `clipmux-denied`.

**Native HLS can't refresh.** Where the browser plays HLS itself (iOS before
17.1, AirPlay, Chromecast), the token must outlive the session: sign it for the
larger of 2 × the video's duration or 4 hours. The element tells your backend
(`native` in the `session-url` POST, or `getToken`'s argument), and
`@clipmux/node` signs for it: `playback.token(id, { native, durationSeconds })`.
iOS 17.1+ plays through hls.js (ManagedMediaSource) and refreshes.

For another player, `rewriteTokenUrl`, `TokenSession` and `createRefreshLoader`
(an hls.js `loader`) are exported without the UI.

## Events and types

| Event              | `detail`                                                                                                  |
| ------------------ | --------------------------------------------------------------------------------------------------------- |
| `clipmux-token`    | `{ token }`: a fresh token is in use                                                                      |
| `clipmux-denied`   | `{}`: the app said no (`null`, `401`/`403` from `session-url`) or delivery answered `403`; playback stops |
| `clipmux-error`    | `{ status, track? }`: `0` for a network or media error                                                    |
| `clipmux-chapters` | `{ chapters }`, in seconds                                                                                |

They bubble and are typed on `HTMLElementEventMap`, so
`player.addEventListener('clipmux-chapters', (e) => e.detail.chapters)` needs
no cast. For React 19, import the JSX types once; they type the attributes,
the `token`/`getToken`/`hls` properties and `onclipmux-*` handlers, and add no
React dependency:

```ts
import type {} from '@clipmux/player/react'

<clipmux-player playback-id={pid} session-url="/api/clipmux/playback" onclipmux-denied={() => setDenied(true)} />
```

## Subtitles and chapters

Subtitles listed in the master playlist show in the player's captions menu;
hls.js loads their playlist and VTT through the same refresh loader as the
video, so a refreshed token keeps them loading. Chapters are never listed in
the master: name the asset's ready chapters track and the player fetches its
VTT with the playback token and marks the chapters on the time slider.

```html
<clipmux-player base-url="…" playback-id="pid_…" chapters-track="trk_…"></clipmux-player>
```

`player.chapters` holds them in seconds (`PlayerChapter`) once
`clipmux-chapters` fires. (`@clipmux/node`'s `Chapter` is the API's, in
milliseconds.) For
another player, `trackUrl` gives a track's sidecar VTT URL, `fetchTrack` loads
it with a `TokenSession`, and `parseChapters` reads a chapters VTT.

## Your backend: signing playback tokens

The player never sees a key. Your backend signs a short-lived ES256 JWT with
the app's signing key (Dashboard → Apps, or `clipmux admin create-signing-key`)
for each viewer it allows, and `getToken` fetches it. With `@clipmux/node`:

```ts
const token = await clipmux.playback.token(playbackId, { native, viewerId: user.id })
```

Or with [`jose`](https://github.com/panva/jose), in any JavaScript runtime:

```ts
import { SignJWT, importPKCS8 } from 'jose'

const key = await importPKCS8(process.env.CLIPMUX_SIGNING_KEY!, 'ES256') // the PEM
const kid = process.env.CLIPMUX_SIGNING_KID! // kid_…

// GET /api/playback-token?playback_id=…, after your own access check
export async function playbackToken(playbackId: string): Promise<string> {
  return new SignJWT({})
    .setProtectedHeader({ alg: 'ES256', kid })
    .setSubject(playbackId) // sub: the playback ID
    .setAudience('v') // v: playback; t: the poster
    .setIssuedAt()
    .setExpirationTime('10m') // the player refreshes it
    .sign(key)
}
```

A public playback ID needs no token: set `playback-id` and leave `getToken`
unset. The full contract, including posters and the maximum lifetime, is the
Delivery section of [docs/api.md](https://github.com/visheshgubrani/clipmux/blob/main/docs/api.md#delivery).

## Content-Security-Policy

A page with the player must allow, besides its own origin:
- `connect-src` and `media-src`: the media origin (`base_url`), and `blob:`
  in `media-src` for Media Source playback;
- `img-src`: the media origin, for the poster;
- `style-src 'unsafe-inline'`: Video.js injects its skin's styles.

`npx @clipmux/node doctor` prints the exact origins. hls.js runs on the main
thread, so no `worker-src` is needed.

## Licence

MIT. The ClipMux server is AGPL-3.0; this package is MIT so apps embed it freely.
