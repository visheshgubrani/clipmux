// ClipMux in Express: one route for the uploader, the player and webhooks,
// mounted before express.json() so the webhook's raw body reaches it.
//
//   npx @clipmux/node init --server https://video.example.com
//   pnpm build && pnpm start

import { toNodeListener } from '@clipmux/node/express'
import { clipmuxHandler } from '@clipmux/node/handlers'
import express from 'express'
import { addVideo, allVideos, updateVideo, videoByPlaybackId } from './videos.ts'

const app = express()

app.post(
  '/api/clipmux/:action',
  toNodeListener(
    clipmuxHandler({
      // Someone chose a file. This demo lets anyone upload: check your own
      // session here, and return null to refuse (403).
      upload(_req, { file }) {
        const video = addVideo(file.name || 'Untitled')
        return { passthrough: video.passthrough, title: video.title }
      },
      // Someone pressed play. Only this app's ready videos play; anything
      // else is refused, and the player fires clipmux-denied.
      playback(_req, { playbackId }) {
        return videoByPlaybackId(playbackId)?.status === 'ready' ? {} : null
      },
      // A verified webhook, with the asset re-fetched so it's current.
      asset(asset) {
        updateVideo(asset)
      },
    }),
  ),
)

// Anything else this app has can parse JSON as usual, after the route.
app.use(express.json())
app.use('/assets', express.static(new URL('dist', import.meta.url).pathname))

const esc = (s: string) =>
  s.replace(
    /[&<>"']/g,
    (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c] ?? c,
  )

const page = (title: string, body: string) => `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>${esc(title)}</title>
  <style>
    body { font-family: system-ui, sans-serif; max-width: 48rem; margin: 0 auto; padding: 1rem; }
    clipmux-player { display: block; aspect-ratio: 16 / 9; background: #000; }
  </style>
  <script type="module" src="/assets/client.js"></script>
</head>
<body>${body}</body>
</html>`

app.get('/', (_req, res) => {
  const list = allVideos()
    .map(
      (v) =>
        `<li>${v.playbackId && v.status === 'ready' ? `<a href="/watch/${esc(v.playbackId)}">${esc(v.title)}</a>` : esc(v.title)} (${esc(v.status)})</li>`,
    )
    .join('')
  res.send(
    page(
      'Videos',
      `<h1>Videos</h1><ul>${list || '<li>No videos yet.</li>'}</ul>
       <h2>Upload</h2><clipmux-uploader create-url="/api/clipmux/upload" accept="video/*"></clipmux-uploader>`,
    ),
  )
})

app.get('/watch/:playbackId', (req, res) => {
  const video = videoByPlaybackId(req.params.playbackId)
  if (!video) {
    res.status(404).send(page('Not found', '<p>No such video. <a href="/">Back</a></p>'))
    return
  }
  // The page gets the playback ID only; the session URL signs a token.
  res.send(
    page(
      video.title,
      `<p><a href="/">← Videos</a></p><h1>${esc(video.title)}</h1>
       <clipmux-player playback-id="${esc(req.params.playbackId)}" session-url="/api/clipmux/playback"></clipmux-player>`,
    ),
  )
})

const port = Number(process.env.PORT ?? 3000)
app.listen(port, () => console.log(`http://localhost:${port}`))
