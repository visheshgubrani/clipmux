# Dashboard design

This is the P6-T10 spec for the dashboard: the tokens, the brand, the
navigation, how tables behave on phones, the empty, loading and error states,
and the first-run flow.

The clickable mock is the
[ClipMux dashboard mock](https://claude.ai/artifact/XMu8H6RAk33jUTVdaktxbf).
It shows each screen in light and dark, with sample data. Where this file and
the mock differ, this file wins. The owner signs both off before T11, and the
sign-off is recorded in [decisions.md](decisions.md).

## Principles

This is design v2, rooted in the editing room: timecode, tally lamps, slates,
media bins, chapter markers on a timeline, and video shown on black. The
owner is one person, usually a developer or course creator, whose job here is
to get videos in, see they're ready, control who watches, and keep the
pipeline healthy without thinking about it.

- **Simple first.** Each page answers one question: Home ("is everything
  fine?"), Videos ("where is my video?"), a video ("what can I do with it?").
  Advanced controls, such as playback IDs and the reference, sit behind
  "Advanced" or a disclosure.
- **One loud element.** The poster (below) is the memorable thing. Everything
  around it is quiet: rows and sections, hairlines, one wide title per page.
- **Lists over cards.** Content is left-aligned and at most 1200 px wide.
  Lists are rows (`Item`), not boxes. A bordered panel appears only where
  something is truly a unit: the Analytics chart, a cloud provider, dialogs.
  No tinted icon tiles and no stat tiles.
- **Plain words.**
  - "Private" and "Public", not `signed` and `public` policies.
  - "Encoding", not executors or processing.
  - "Video", not asset.
  - API names appear only on Apps & API and in IDs.
  - Buttons name their action, and the toast repeats it ("Delete video" →
    "Video deleted").
  - Meta goes in separate muted fields or commas, never "A · B · C".
  - No all-caps labels and no eyebrows above headings.
- **Say what leaves the infrastructure.** Every option that sends data to a
  third party says so, at the place you choose it: speech API, LLM,
  Cloudflare, cloud GPUs.
- **No dead ends.** Every state has a next step:
  - loading shows a skeleton;
  - an error says what failed and what to do, without apologising, and
    offers **Try again**;
  - an empty list offers its one action.

  Nothing hangs on "Loading…".
- **Ask before destroying.** Every destructive action opens a dialog that
  names the thing and says what stops working. Native `confirm()` is never
  used.

## Tokens

The tokens are CSS variables in `dashboard/src/index.css`, wired into
Tailwind v4 with `@theme inline`. The light set is on `:root` and the dark set
on `.dark`. The palette has five names; the shadcn variables map onto them.

| Name | Light | Dark | Role |
| --- | --- | --- | --- |
| Canvas | `#F5F5F7` | `#121118` | page |
| Screen | `#FFFFFF` | `#1A1922` | surfaces, popovers |
| Ink | `#18171E` | `#EDECF3` | text |
| Graphite | `#6B6A75` | `#A3A2AE` | secondary text (≥ 4.5:1 on both grounds) |
| Tally violet | `#6D28D9` | fill `#7C3AED`, text `#A78BFA` | primary, focus, encoding |
| Ready green, Failed red | `#16803C`, `#C81E1E` | `#4ADE80`, `#F87171` | status only, always with a word |
| Black | `#000000` | `#000000` | posters and the player: video is shown on black |

| Variable | Light | Dark | Use |
| --- | --- | --- | --- |
| `--background` | `#F5F5F7` | `#121118` | Canvas |
| `--card`, `--popover` | `#FFFFFF` | `#1A1922` | Screen: dialogs, menus, inputs, the Analytics panel |
| `--foreground` | `#18171E` | `#EDECF3` | Ink |
| `--muted-foreground` | `#6B6A75` | `#A3A2AE` | Graphite |
| `--border`, `--input` | `#E4E3E9` | `#2C2B36` | hairlines |
| `--muted` | `#ECECF0` | `#23222C` | skeletons, selected rows and toggles, code |
| `--primary` | `#6D28D9` | `#7C3AED` | primary buttons, focus outline, chart line (white text ≥ 5.7:1) |
| `--tally` | `#6D28D9` | `#A78BFA` | links, active text, the encoding lamp outside posters |
| `--success` | `#16803C` | `#4ADE80` | ready, passed |
| `--warning` | `#B45309` | `#FBBF24` | data leaves, spend |
| `--destructive` | `#C81E1E` | `#F87171` | failed, delete |
| `--sidebar` | `#FFFFFF` | `#1A1922` | sidebar, phone top bar |
| `--chart-1..5` | violet `#6D28D9`, graphite `#6B6A75`, teal `#0D9488`, amber `#D97706`, sky `#0284C7` | violet `#7C3AED`, rest the same | series |

There are no `-soft` tints: status is a lamp plus a word, not a coloured
background. `--shadow-xs` is `0 0 #0000`; shadows appear on overlays alone.

- **Type:** one family, Archivo Variable (`wght` and `wdth`), bundled with
  `@fontsource-variable/archivo` and served from `'self'`.

  | Use | Size / weight | Utility |
  | --- | --- | --- |
  | Page title | 28/600, `font-stretch: 118%`, sentence case | `type-page-title` |
  | Section title | 15/600 | `type-section` |
  | Body | 14/400, line-height 1.5 | |
  | Secondary | 13 | |
  | Labels | 12 | |
  | Durations | tabular timecode figures: `0:42`, `24:15`, `1:02:07` | `type-timecode` |

  The wide page title is the one bold typographic move, the slate and
  lower-third voice; everything else is normal width. Numbers use
  `tabular-nums`. `ui-monospace` is only for real code: IDs, commands, keys.
- **Spacing:** a 4 px grid. Pages are padded 36/40 px on desktop and 16 px on
  phones, with content left-aligned at most 1200 px wide (820–960 px for
  settings-like pages). Each page has a wide title, a one-line description
  and its actions on the right; sections are a heading plus content,
  separated by space or a `Separator`.
- **Radius ladder:** one radius per kind of thing, never one everywhere.

  | Radius | For |
  | --- | --- |
  | 4 px | status chips and badges, the poster's chips |
  | 6 px | posters, the player, thumbnails, menu items, toggle items |
  | 8 px | buttons, inputs, menus, toasts, the Analytics panel |
  | 12 px | dialogs and sheets |

  Tally lamps are square (1 px corner). Avatars and check circles are round.
- **Lines and depth:** hairline `--border` rules separate rows and sections.
  Shadows only on overlays: menus, toasts, dialogs.
- **Focus:** a solid 2 px violet outline, offset 2 px.
- **Theme:** Light, Dark or System, from the account menu. It is stored in
  `localStorage` and applied before paint by `public/theme.js`. That is an
  external script, because the CSP refuses inline ones.

## The poster

The one memorable element, used in the Videos grid, Recent videos on Home,
"Needs attention" thumbnails and the list view (as a 64×36 thumbnail).

- **Frame:** 16:9 on true black `#000`, radius 6 px. The poster image fills
  it; until a playback ID is playable (`poster_url` is null) the frame stays
  black.
- **Timecode chip, bottom left:** the duration burned in, 11 px tabular
  figures in white on `rgb(0 0 0 / .72)`, radius 4 px. Hidden when the
  duration isn't known.
- **Tally lamp, top left:** a 6 px square, shown only when the video isn't
  ready.
  - Encoding: violet `#7C3AED`, with a 2 px black ring so it reads on any
    frame.
  - Failed: red `#F87171` inside a black chip with the word "Failed".
  - Waiting for upload: a hollow graphite square with the words.
- **Globe, top right:** only for **public** videos, in a 22 px black chip.
  Private is the default and unmarked.
- **Playhead:** while encoding, a 2 px violet line moves along the bottom
  edge. It is the only ambient motion in the product, and under
  `prefers-reduced-motion` it stands still as a full-width line.
- **Below the frame:** the title (14/500, one line, truncated), then one
  muted line: the time added, or "Encoding, 64%". CC and a ⋯ menu sit beside
  it in the grid.
- **Accessible name:** the title, plus the status when not ready ("Lesson 3:
  Functions, failed").

Status elsewhere uses the same lamp: a **badge** is a 22 px chip with a 1 px
hairline, radius 4 px, a 6 px lamp (green Ready, violet Encoding, red Failed,
hollow Waiting) and the word in Ink (Failed in red).

## Brand

- **Mark:** a Tally violet `#6D28D9` square with an 8 px corner (at 32 px)
  holding a white play triangle, with a translucent bar to its left (the
  "mux"). It is drawn in SVG and is also `public/favicon.svg`.
- **Wordmark:** "ClipMux" in Archivo 700 at `font-stretch: 118%`, set beside
  the mark, like the page titles.
- **No brand panel.** Sign in is one centred 360 px column: the mark and
  wordmark, a wide "Sign in", the fields and a full-width button.
- **Icons:** one set, `lucide-react`, at 16 px with a 2 px stroke. Status is
  never colour alone: it always comes with a word or an icon.

## Navigation

A left sidebar 248 px wide. From top to bottom:

1. The mark and wordmark (linking Home), then the primary **Upload** button.
2. **Home**, **Videos**, **Analytics**, **Jobs** (with a count of running jobs).
3. **Connections:**
   - **Encoding** (this server, your PCs, cloud GPUs, routing);
   - **Delivery** (built-in or Cloudflare, storage);
   - **Captions & AI**;
   - **Apps & API**.
4. At the bottom, **Settings**, then the account row: email, theme and sign
   out.

Below 768 px the sidebar hides. A 56 px top bar holds the menu button, the
page name and Upload, and the menu opens the sidebar as a sheet with 44 px
rows.

| Route | Page | Old route |
| --- | --- | --- |
| `/` | Home | `/` → `/library` |
| `/videos`, `/videos/:id` | Videos, a video | `/library`, `/assets/:id` (redirect) |
| `/upload` | Upload (also a dialog from anywhere) | same |
| `/analytics` | Analytics | new |
| `/jobs` | Jobs | same |
| `/encoding` | Encoding: this server, Modal or RunPod (D116) | `/workers` (redirect) |
| `/workers/connect` | Approve a PC | unchanged; only `clipmux worker` opens it (D116) |
| `/storage` | Storage: where uploaded originals go (D110, D117) | new |
| `/delivery` | Delivery: how videos reach viewers, and where encoded files are kept (D117) | same |
| `/captions` | Captions & AI | same |
| `/apps`, `/apps/:id` | Apps & API | same |
| `/settings` | Settings | `/account` (redirect); URLs move here from `/setup` |
| `/login`, `/setup` | Sign in; first run | same |

## Tables on phones

- **Data that is read by row** becomes `Item` rows below 640 px: jobs,
  agents, keys, storage configs and webhook deliveries. One component
  (`ResponsiveTable`) renders both layouts from the same column definition.
- **Wide numeric tables** keep their columns inside their own `overflow-x:
  auto` box: Top videos and the library's list view.
- **No page scrolls sideways at 375 px.** IDs truncate with a copy button.
  Commands and code scroll inside their own box.

## States

| State | Look |
| --- | --- |
| Loading | A skeleton in the shape of the content (posters, rows). It appears after 150 ms, so fast loads don't flash. |
| Error | An icon, "Couldn't load …", the API's status and code in muted text, and **Try again**, which re-runs the load. The rest of the page keeps working. |
| Empty | Left-aligned: a heading, one sentence on what goes here, and the one action (Upload, Add endpoint, Connect Modal). No icon tile. |
| Not collecting (Analytics) | As Empty, explaining what will be counted and that nothing goes to a third party. The product never shows sample data. |
| Edge not counted (Analytics) | With Cloudflare delivery and no edge analytics token: an Alert above the chart saying the numbers miss the edge, with Connect edge analytics, which opens Delivery's card. |
| Confirm | An alert dialog: "Delete "Lesson 2"?", what stops working, and Cancel plus a red button whose label names the action. |
| Toast | Bottom right, 5 s, for the result of an action, with an optional link (Open). Errors stay until dismissed. |
| Secrets | A "Save these now" dialog, a warning that they are shown once, a copy button per value, and "I've saved them". |
| 404 | "Nothing here", with a link to Home. |

## First run

1. **No owner yet.** Every route leads to `/setup` and **Create your
   account**. It asks for:
   - the setup token, with the `docker compose logs` command to find it;
   - an email;
   - a password of at least 12 characters.

   Sign-up then closes for good, and later visits show only **Sign in**.
2. **Your addresses.** The public URL and the media URL, with "keep localhost
   to try things out". **Save** lands on Home. The setup test isn't part of
   the first run (D115); it waits on Settings → General, with its checks and
   the sample in the player once it passes.
3. **Home** shows a **Finish setting up** checklist until each is done or
   hidden:
   - Store uploads in a bucket;
   - Connect Cloudflare;
   - Add a cloud GPU;
   - Turn on chapters;
   - Create an app.

**Sign in:**
- One centred 360 px column (see Brand). Email and password, then a code
  step when TOTP is on (`InputOTP`, six digits in two groups of three), with
  "Use a recovery code".
- "Forgot password?" explains `clipmux admin reset-password` on the host.
- A 429 says when to try again.
- After sign-in, `?next=` returns the user to the page they asked for, on this
  origin only.

## Pages

Home, Videos and a video are drawn in v2 in the mock. The other pages keep
the structure their mock boards show and take the v2 look: wide titles, rows
and sections instead of cards, the radius ladder, posters on black.

| Page | What it holds |
| --- | --- |
| Home | (1) A wide "Good afternoon", a one-line summary and Upload. (2) **Finish setting up**: a checklist of rows, each a check circle, the step, one line on what it does (and what leaves, for Cloudflare and cloud GPUs) and its action; "2 of 5 done" and Hide; shown until done or hidden. (3) **Needs attention**: failed and encoding videos as rows with a poster thumbnail, what happened and what to do, and Retry or the progress; only when there are any. (4) **Recent videos**: 8 posters, the main content, with "All videos". (5) A muted status line in the footer, one plain field each for encoding, delivery, uploads and captions, each label linking its page. No stat tiles: numbers live on Analytics |
| Videos | A wide title and Upload. A toolbar: search (`InputGroup`; title, reference, ID); a status `ToggleGroup` with counts (All, Ready, Encoding, Failed, Waiting, from `/v1/library/counts`); an app `Select` when there is more than one app; a grid or list `ToggleGroup`. The poster grid (title, then time added or "Encoding, 64%", CC, ⋯ menu), or a hairline `Table` (thumbnail and title, status badge, privacy, duration, captions, added) that becomes `Item` rows on phones. Drop anywhere to upload; Load more |
| A video | A breadcrumb; the wide title; a status badge; a ⋯ `DropdownMenu` (Rename, which opens a dialog; Re-encode; Copy ID; Delete video, which confirms). Tabs: **Overview**: the player on black with a **chapter timeline strip** under it, one labelled segment per chapter sized by its length, with its start time, which seeks; beside it the details as a description list (duration, privacy, added, app, renditions, captions, video ID with copy, reference), not a card. **Captions & chapters**: subtitles and chapters as timecoded rows, regenerate, delete, what was sent where. **Sharing**: Private/Public as radio rows, the embed when public, share links as rows (name, status, expiry, views in 30 days; copy link, stream URL and revoke), playback IDs under Advanced, a share link's ID named by its link. **Analytics**: one panel whose header is a metric `ToggleGroup` |
| Upload | Several files with progress, pause and cancel, resuming after a reload; Private/Public; subtitles; chapters (disabled with "Connect an LLM" when there is none); Advanced: app and reference |
| Analytics | Range (24 h, 7 d, 30 d, 90 d) and app; one chart card whose header is a segmented control showing each metric's total and change (Views, Watch time, Unique viewers, Bandwidth) and selecting the chart; this period against the previous one; Top videos |
| Jobs | Status chips, kind, worker, video ID; rows expand to attempts (where it ran, route reason, encoder fallback, error, Retry); live indicator |
| Encoding | One radio group (D116): This server (its GPU when it sees one, else the CPU, with Online), Modal and RunPod, each with Connect, then "Checking" or which check failed, then Ready with spend against the limit; "If it can't start a machine, encode on this server instead" while a cloud GPU is chosen |
| Storage | Where uploaded originals go: the list (videos on each, Default), Use for new uploads, Add R2 bucket, Add S3 storage, Rotate keys, Remove; the uploads' library move (D110, D117) |
| Delivery | Built-in or Cloudflare cards, each saying where encoded files are kept; the Connect Cloudflare dialog (hostname and bucket, no uploads choice) with its steps; the encoded files' library move; Disconnect; edge analytics (D117) |
| Captions & AI | Subtitles: three modes, each saying what leaves; Connect a speech API. Chapters: Claude or OpenAI-compatible |
| Apps & API | App list; Create app shows its secrets once; tabs for API keys (named, last used), Signing keys, Webhooks (health, deliveries, Redeliver, two-step secret rotation), Danger zone (disable) |
| Settings | **General** (URLs, setup test), **Account** (password, two-factor), **Security** (rotate the content key, backups), **About** (version, licences) |

## Implementation notes (T11)

- **Components:** shadcn 4.23 (`base-nova`: Base UI underneath, 32 px
  controls), added with the CLI into `dashboard/src/components/ui/` and
  restyled through the tokens in `index.css`. Only the components the pages use
  are added.
  - Triggers take `render`, not `asChild`.
  - Toasts are the `toast` component (Base UI's toast manager).
  - Choices in forms are `NativeSelect`, so a phone shows its own picker.
  - `Badge` gains `success`, `warning` and `tally` variants at the 4 px radius.
  - The generated files import `cn` from `@/lib/utils` (clsx and
    tailwind-merge), not shadcn's new `cn` package.
- **Dependencies:**
  - **Used:** `@tanstack/react-query` (server state, polling, retries,
    invalidation), `lucide-react`, `recharts` (through the shadcn chart),
    `input-otp`, `@fontsource-variable/archivo`, `tw-animate-css`.
    `shadcn` itself is a dev dependency, for its CSS and the CLI.
  - **Not used:** Aceternity or `motion`, a form library, a date library,
    `react-dropzone`, `cmdk`.
  - Every version is pinned exactly.
  - All run under the CSP: `script-src 'self'`, with inline styles allowed.
  - None loads from a CDN.
- **Split bundles:** the player (hls.js and Video.js) and the chart load only
  on the pages that use them.
- **Data loading:** TanStack Query, with query keys and options per resource
  in `src/queries.ts`.
  - A 4xx isn't retried; anything else is retried once.
  - Polling runs only while something is moving (an encode, a job, a move).
  - `<QueryState>` turns a query into the skeleton, error (with Try again) and
    content states above.
- **Confirmation:** `useConfirm()` gives every destructive action its dialog.
- **Errors:** an error boundary per route catches render errors, and a 401 on
  any request sends the owner to sign in and back.
- **Theme:** `public/theme.js` sets `.dark` before paint, and the account menu
  switches Light, Dark or System.
- **No private path:** every page uses the public `/v1` API with the owner's
  session.
