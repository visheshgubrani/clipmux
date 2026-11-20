---
name: clipmux-player
description: "Add ClipMux video upload and playback to an app's frontend with the <clipmux-uploader> and <clipmux-player> web components (@clipmux/uploader, @clipmux/player), in React, Next.js, Vue, Svelte or plain HTML. Use whenever a page needs a video upload button or drop zone, a resumable upload, or a signed/private video player for ClipMux, or mentions session-url, create-url, clipmux-denied or chapters on the player. The backend routes these elements call come from the clipmux-integrate skill; set those up first."
---

# ClipMux in the browser: the uploader and the player

Both elements talk only to the app's own routes (`/api/clipmux/upload` and
`/api/clipmux/playback`, from `clipmuxHandler`; see the `clipmux-integrate`
skill). The page needs no ClipMux URL, key or token: that is how the API key
and signing key stay on the server, and how access stays the app's decision.
So never import `@clipmux/node` or read `CLIPMUX_*` in browser code, and
never sign a token into the HTML.

Docs: https://clipmux.com/docs (append `.md` to any page for Markdown).

## Upload: `<clipmux-uploader>`

```html
<script type="module">import '@clipmux/uploader/element'</script>
<clipmux-uploader create-url="/api/clipmux/upload?lesson=42" accept="video/*"></clipmux-uploader>
```

- Put your record's ID in `create-url`'s query: the `upload` callback reads it
  from `req.url` and returns the `passthrough`.
- It uploads in parts straight to storage, retries, and resumes after a
  dropped connection. Optional: `max-size` (bytes).
- Events (they bubble): `clipmux-progress`, `clipmux-uploaded` with
  `{ asset }` (now `processing`, not playable yet), `clipmux-error` with
  `{ code }` (`create_denied` when the callback returned `null`,
  `file_too_large`, `file_type_rejected`, or a ClipMux code).
- The video becomes playable when the webhook says `ready`, not at
  `clipmux-uploaded`: show "Processing", and re-render (poll the page, say)
  until your record has a playback ID.

[Uploader element](https://clipmux.com/docs/build/upload/browser#add-the-element)

## Play: `<clipmux-player session-url>`

```html
<script type="module">import '@clipmux/player/element'</script>
<clipmux-player playback-id="pid_…" session-url="/api/clipmux/playback"></clipmux-player>
```

- The page gets only the playback ID (stored from the webhook). The player
  POSTs it to `session-url`; the `playback` callback decides and the route
  answers with a signed session. It asks again before the token runs out, and
  tells the route when it plays native HLS (Safari) so a longer token is
  signed.
- `clipmux-denied` fires when the callback returned `null` (or 401/403):
  show your own sign-in or enrol state, not an error. `clipmux-error` with
  `status: 404` means the video was removed.
- Chapters and subtitles in the session show by themselves when the
  `playback` callback returns the `asset`; `chapters-track="trk_…"` names one
  explicitly, and `clipmux-chapters` gives `{ chapters }` in seconds.
- `player.media` is the inner video element for `currentTime`, `ended` and
  media events (resume, progress tracking).
- Your own token route instead: set `player.getToken = async ({ playbackId, native }) => …`
  and sign server side with `clipmux.playback.token(playbackId, { native, viewerId })`.
  Pass `native` through, or Safari before 17.1 stops after 15 minutes.

[Player](https://clipmux.com/docs/build/play/player#play-with-a-session-url),
[events](https://clipmux.com/docs/build/play/player#events),
[subtitles and chapters](https://clipmux.com/docs/build/play/player#subtitles-and-chapters)

## React and Next.js

The elements need the DOM (the player needs Media Source), so load them in
the browser only, in a client component, and import the JSX types:

```tsx
'use client'
import type { ClipMuxPlayerElement } from '@clipmux/player/element' // types only
import type {} from '@clipmux/player/react' // JSX types for <clipmux-player>
import type {} from '@clipmux/uploader/react' // and <clipmux-uploader>
import { useCallback, useEffect } from 'react'

export function LessonVideo({ playbackId }: { playbackId: string }) {
  useEffect(() => {
    void import('@clipmux/player/element')
    void import('@clipmux/uploader/element')
  }, [])
  // A ref callback, not an onclipmux-denied prop: React 19 doesn't attach
  // custom-event props to an element it hydrates from server HTML.
  const player = useCallback((el: ClipMuxPlayerElement | null) => {
    const onDenied = () => {
      /* show the locked state */
    }
    el?.addEventListener('clipmux-denied', onDenied)
    return () => el?.removeEventListener('clipmux-denied', onDenied)
  }, [])
  return <clipmux-player ref={player} playback-id={playbackId} session-url="/api/clipmux/playback" />
}
```

Keep `"skipLibCheck": true` in `tsconfig.json` (Video.js's declarations don't
all type-check). In Vue or Svelte, treat `clipmux-*` as custom elements and
import the element modules on mount.
[React and Next.js](https://clipmux.com/docs/build/play/player#react-and-nextjs),
[uploader in React](https://clipmux.com/docs/build/upload/browser#react)

## Content-Security-Policy

A page with the player allows the media origin in `connect-src`, `img-src`
and `media-src` (plus `blob:`), and `style-src 'unsafe-inline'`; the
uploader needs the ClipMux server and the storage origins in `connect-src`. `npx @clipmux/node doctor`
prints the exact origins.
[CSP](https://clipmux.com/docs/build/play/player#content-security-policy)

## Check it

As a user who may upload: the uploader reaches `state="done"`, and after
the webhook the player appears and plays. As one who may not watch:
`clipmux-denied` fires. The browser's network tab shows requests only to
the app and the media/storage origins, and no `cmx_` key anywhere in the
page's scripts.
