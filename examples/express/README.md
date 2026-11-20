# ClipMux in an Express app

One route answers the uploader, the player and ClipMux's webhooks:

```ts
app.post('/api/clipmux/:action', toNodeListener(clipmuxHandler({ upload, playback, asset })))
app.use(express.json()) // after the route: the webhook needs the raw body
```

- [`server.ts`](server.ts): the route, its three callbacks and two pages.
- [`src/client.ts`](src/client.ts): imports `<clipmux-uploader>` and
  `<clipmux-player>`. Vite bundles it to `dist/client.js`.
- [`videos.ts`](videos.ts): the videos, in memory; swap in your database.

The demo lets anyone upload; check your own session in `upload` and return
`null` to refuse. `playback` refuses any playback ID that isn't one of this
app's ready videos.

## Run it

```bash
pnpm install
npx @clipmux/node init --server https://video.example.com
pnpm build && pnpm start
npx @clipmux/node listen --forward-to localhost:3000/api/clipmux/webhook
```

`pnpm start` loads `.env.local` with `node --env-file-if-exists`. Open
http://localhost:3000, upload a video, and it's listed as ready when the
webhook arrives. `npx @clipmux/node doctor` checks the setup.
