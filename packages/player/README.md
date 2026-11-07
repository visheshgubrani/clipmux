# @clipmux/player

Plays ClipMux videos with Video.js 10 and refreshes expired playback tokens
without losing the viewer's place.

```ts
import '@clipmux/player/element'

const player = document.querySelector('clipmux-player')!
player.getToken = async () => (await fetch('/api/playback-token')).text() // or null to deny
player.token = await player.getToken()
```

```html
<clipmux-player base-url="https://media.example.net" playback-id="pid_…"></clipmux-player>
```

How refresh works: tokens ride in the URL path, so on a `401 token_expired` the
player calls `getToken()` once, puts the new token into that request and every
later one, and retries; hls.js never sees the error. `null` (or a `403` from
delivery) stops playback and fires `clipmux-denied`.

**Native HLS can't refresh.** Where the browser plays HLS itself (iOS before
17.1, AirPlay, Chromecast), the token must outlive the session: sign it for the
larger of 2 × the video's duration or 4 hours. iOS 17.1+ plays through hls.js
(ManagedMediaSource) and refreshes.

For another player, `rewriteTokenUrl`, `TokenSession` and `createRefreshLoader`
(an hls.js `loader`) are exported without the UI.

## Subtitles and chapters

Subtitles listed in the master playlist show in the player's captions menu;
hls.js loads their playlist and VTT through the same refresh loader as the
video, so a refreshed token keeps them loading. Chapters are never listed in
the master: name the asset's ready chapters track and the player fetches its
VTT with the playback token and marks the chapters on the time slider.

```html
<clipmux-player base-url="…" playback-id="pid_…" chapters-track="trk_…"></clipmux-player>
```

`player.chapters` holds them in seconds once `clipmux-chapters` fires. For
another player, `trackUrl` gives a track's sidecar VTT URL, `fetchTrack` loads
it with a `TokenSession`, and `parseChapters` reads a chapters VTT.
