# ClipMux in a Next.js course site

A small LMS on the App Router. Ada, the instructor, adds lessons and uploads
their videos. Ben, an enrolled student, watches them. Cy isn't enrolled, so
the player is refused and the page says so. The header's "Signed in as"
picker stands in for real sign-in.

The whole integration is:

- [`app/api/clipmux/[action]/route.ts`](app/api/clipmux/[action]/route.ts):
  one route file with `clipmuxHandler` and its three callbacks: who may
  upload, who may watch, and what a webhook changes.
- [`app/lessons/[id]/lesson-video.tsx`](app/lessons/[id]/lesson-video.tsx):
  one client component with `<clipmux-uploader create-url>` and
  `<clipmux-player session-url>`.
- `.env.local`, written by `init`.

The browser never sees the ClipMux URL, an API key or a signing key: the
elements talk only to `/api/clipmux/*`.

## Run it

You need a ClipMux server and its owner to approve the app.

```bash
pnpm install
npx @clipmux/node init --server https://video.example.com
pnpm dev
```

`init` prints a code and a link. The owner opens the link, checks the code,
picks or creates the app, and can enter a webhook URL. Without one, `init`
writes a local webhook secret, and you forward events while you develop:

```bash
npx @clipmux/node listen --forward-to localhost:3000/api/clipmux/webhook
```

Then open http://localhost:3000, sign in as Ada, add a lesson and upload a
video. The page updates when the webhook says it's ready. Switch to Ben to
watch, or to Cy to see a refusal. `npx @clipmux/node doctor` checks the
setup and prints the Content-Security-Policy sources your pages need.

Settings beyond `init`'s five, all optional:

| Variable        | Default | What it does                                                              |
| --------------- | ------- | ------------------------------------------------------------------------- |
| `LMS_TOKEN_TTL` | `15m`   | How long a playback token lasts; the player renews it before it runs out. |
| `LMS_DATA_DIR`  | `.data` | Where the lessons are kept, as one JSON file.                             |

## What to change for a real app

- `lib/users.ts` and `lib/session.ts`: your sign-in and enrolments.
- `lib/lessons.ts`: your database. The ClipMux side keeps a lesson's
  `status`, `assetId` and `playbackId`, found by the passthrough
  `lesson_<id>`.
- The `upload` callback: add `renditions` or `generate: { subtitles: true }`.

## The exit test

Launch exit check 8 runs against this app in Playwright
(`e2e/web/exit8`): `init` approved on `/connect`, an upload through the
element, the webhook and separately `listen` marking lessons ready, playback
across a 30 s token refresh, a refused viewer, `doctor`, and no secret in
the CLI's output.

```bash
make db-up
cd e2e/web && TEST_DATABASE_URL=postgres://clipmux:clipmux@localhost:5444/clipmux_test?sslmode=disable pnpm run test:exit8
```
