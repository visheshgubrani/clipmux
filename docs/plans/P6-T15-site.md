# Phase 6 — T15 site: clipmux.com, the landing page and the docs

The detailed plan behind the T15 row of [P6-launch.md](P6-launch.md), with
the parts of T16 (`llms.txt`, snippets) and T18 (operator docs) that land in
the site. Drafted 2026-11-18. Decisions land in
[../decisions.md](../decisions.md) as each task finishes.

## Context

ClipMux has accurate prose, but it's spread over `docs/*.md` and three
package READMEs. It's written for people who already know the system, full
of decision and task IDs, and has almost no task-shaped guides. Strangers
arrive at launch (T20). They need two things:

- **A landing page** that says what ClipMux is in one screen and proves it,
  with a real video, real code and real numbers.
- **Docs in the shape of Mux's and Cloudflare Stream's**, one path for
  people who run a server and one for people who add video to an app.
  Coding agents must be able to read them as well as people can.

Settled with the owner on 2026-11-18 (D133):

- **The site lives in this repository, in `site/`.** It's one app serving
  `clipmux.com`: the landing page at `/` and the docs at `/docs`. Docs change
  in the same pull request as the behaviour they describe. This answers P6
  open question 3.
- **The landing page is light, with a black poster.** It uses the
  dashboard's editing-room brand (D098). The one loud element is a black
  video frame playing a real ClipMux stream. Dark mode follows the system.

The references were for ideas only:
- MausBot: an interactive "pick a job" picker.
- Grok Bot: a product UI that assembles as you scroll.
- Resend: code-first integration tabs and a live event stream.
- Mux docs: a "paste this into your agent" prompt.
- Cloudflare Stream docs: a task-shaped sidebar, with agent links (skills,
  `llms.txt`) in the sidebar itself.

## What done looks like

1. `pnpm --filter site build` writes a static site. `make site-check`
   passes: links, frontmatter, the no-internal-IDs lint, routes and env vars
   checked against their sources, and snippets tested against the vector
   files.
2. The landing page and every docs page work at 375 px and at 1280 px, in
   light and dark, with no horizontal scroll and no console errors. The
   landing page scores at least 95 for performance and 100 for accessibility
   in a mobile Lighthouse run.
3. Every docs page has a Markdown twin at `<url>.md`. `/llms.txt`,
   `/llms-full.txt`, `/agents.md` and `/openapi.json` are served.
4. **Agent check.** A coding agent given only "Read
   https://clipmux.com/agents.md and add ClipMux to this app" integrates the
   P6 exit-check-7 starter against a local instance, and the video plays.
5. **Human check.** A fresh VM installs and plays its sample by following
   only the Self-host quickstart. This is P6 exit check 1, run from the docs.
6. The site is deployed to a preview hostname. It goes live on `clipmux.com`
   at T20.

## Stack and repository shape

- **`site/`: Next.js 16 with Fumadocs** (core, ui, mdx, openapi), built with
  `output: 'export'` as static files.
  - It shares the dashboard's stack: React 19, Tailwind v4, lucide icons and
    `@fontsource-variable/archivo`.
  - The old `~/vod/docs-site` ran on Fumadocs, so its plumbing ports
    directly: the `.mdx` rewrite, the `llms` routes, page actions and Orama
    search. Its brand, its content and its 570 lines of `!important`
    overrides don't come across.
- **Tokens.** `site/app/tokens.css` is a copy of the dashboard's variables
  in `dashboard/src/index.css`. This follows the "duplicate once" rule; a
  shared package comes with a third consumer. Fumadocs is themed only
  through its `--color-fd-*` variables, never by overriding its layout.
- **The API reference is generated** from `internal/openapi/openapi.json`
  by `site/scripts/generate-reference.mjs`, as plain MDX: tables and code
  blocks, one page per tag. Changed from fumadocs-openapi in T15.3, because
  its interactive component leaves a page's Markdown twin empty, and agents
  read the twins. The same script builds the webhook events, the errors
  page (from the spec and `site/data/errors.json`) and the changelog.
- **Search** is Orama's static index, run in the browser, with no server.
- **OG images** come from one branded template at build: the mark, the wide
  title and a black poster band.
- **Workspace.** `site` joins `pnpm-workspace.yaml`, and Biome covers
  `site/**`. `images.unoptimized` keeps `sharp` and its install script out.
- **Hosting** is the VPS that runs the production instance (changed from
  Cloudflare Workers static assets in T15.10). Its Caddy serves the static
  build with `deploy/site/clipmux.com.caddy`, which mirrors `_headers` and
  `_redirects`; Cloudflare stays in front as the proxy it already was.
  `make site-deploy` uploads a release and swaps a `current` symlink.
- **CI** is the `web` job of `ci.yml`, on every pull request and push to
  `main` (the separate `site.yml` folded in when the repository went public).
- **Make targets:** `make site` (dev server) and `make site-check` (build
  and every lint below).

**Risk: TypeScript 7.** D112 found that `openapi-typescript` needs
TypeScript 5's compiler API. Next's typegen, fumadocs-mdx or
fumadocs-openapi may hit the same problem. T15.1 opens with a spike. If it
fails, `site/` pins its own `typescript` 5.x dev dependency, which pnpm
allows per package.

## URL map

| URL | What |
| --- | --- |
| `/` | Landing page |
| `/docs` | Introduction: what ClipMux is, and which path to take |
| `/docs/build/…` | Building with ClipMux (integrators) |
| `/docs/self-host/…` | Running ClipMux (operators) |
| `/docs/reference/…` | API, webhooks, errors, limits, env vars, CLI, SDKs, changelog |
| `/docs/resources/…` | Migration guides, architecture, FAQ, contributing |
| `/changelog` | Redirects to `/docs/reference/changelog` |
| `/docs/<page>.md` | The Markdown twin of every page |
| `/llms.txt`, `/llms-full.txt` | Agent index and full text; also `/docs/build/llms-full.txt` and `/docs/self-host/llms-full.txt` |
| `/agents.md` | The onboarding prompt an agent fetches |
| `/openapi.json` | A copy of the spec from the build |
| `/install.sh`, `/install-agent.sh` | 302 to the latest GitHub release asset, never a copy (the installers check `SHA256SUMS`) |
| `/sitemap.xml`, `/robots.txt` | Search engines and AI crawlers allowed |

## The landing page

### Design plan (frontend-design pass 1)

**Subject.** Self-hosted video infrastructure, for developers who'd
otherwise pay Mux or Cloudflare Stream, and for people who self-host.

**The page's job.** Make someone believe in one screen that this is real.
Then hand them the one-liner, the four-step integration, or the docs.

**Colour.** These are the dashboard's tokens. Nothing new is added.

| Name | Light | Dark | Use |
| --- | --- | --- | --- |
| Canvas | `#F5F5F7` | `#121118` | The page |
| Screen | `#FFFFFF` | `#1A1922` | Code panels, the track editor, the install box |
| Ink | `#18171E` | `#EDECF3` | Text |
| Graphite | `#6B6A75` | `#A3A2AE` | Secondary text, hairlines' labels |
| Tally violet | `#6D28D9` | `#7C3AED` fill, `#A78BFA` text | Primary button, links, the playhead, focus |
| Poster | `#000000` | `#000000` | The hero video frame, and nothing else |

Hairlines use `#E2E1E6` / `#2B2A35`. Nothing else gets a shadow, tint or
gradient. Code highlighting is a Shiki theme kept as data: violet keywords,
graphite comments, ink text.

**Type.** One family, Archivo Variable (weight and width axes), the
dashboard's.
- **Display:** the hero headline at `wdth` 125 (Archivo's widest), weight
  750, line-height 0.93, `clamp(36px, 9.6vw, 116px)`. Below 36 px "infrastructure" no longer fits a 375 px screen at this width (found in T15.1). This is the one
  typographic move. The dashboard's page titles use 118%; the landing page
  goes all the way.
- **Section headings:** `wdth` 118, weight 650, 36/40 px.
- **Body:** 17/1.55 on the landing page and 15/1.6 in the docs, at most 72
  characters a line.
- **Scale:** 13, 15, 17, 21, 28, 36, 48, 72 and 112 px.
- **Code** uses the dashboard's `ui-monospace` stack, and only for real code.
  Timecodes use tabular figures in Archivo, not monospace.

**Layout.**
- A 1200 px column, everything left-aligned. Only the poster spans the full
  column.
- Sections are rows divided by hairlines, never cards (D098).
- **The page is a timeline.** A thin bar sticks under the nav, styled like
  the player's chapter track. It shows the page's chapters as markers
  (Watch, Integrate, Your stack, Security, Features, Agents, Install), and
  the 2 px violet playhead moves with scroll.
  - Clicking a marker jumps to that section.
  - On phones the labels go and the line stays.
  - This is the page's only ambient motion, and it holds still under
    reduced motion.

**Principles.**
1. The product is the hero. The first thing on the page is a ClipMux video
   playing, delivered by ClipMux.
2. Every number is real and sourced: from the demo video's API record, or
   from production measurements in a decision.
3. Code is copy-pasteable and identical to the docs, drawn from the same
   tested files.
4. Motion answers a click (choosing a clip, switching a tab). Nothing fades
   in on scroll.
5. The page says what ClipMux doesn't do (no DRM, a single replica) as
   plainly as what it does.

### Review against the defaults (frontend-design pass 2)

| Default it drifted toward | What changed, and why |
| --- | --- |
| Near-black page with glow and one accent: all three references, and generic tell 2 | A light canvas. The black poster becomes the loud element, as in the dashboard. |
| A centred hero with a giant wordmark (MausBot, Grok Bot) | A left-aligned wide headline. The hero image is the product itself, a playing video. |
| A bento grid of feature cards (tell 4, and banned by D098) | Feature rows with one screenshot panel that changes with the selected row. |
| A logo wall and stat tiles | One production line (Edural) and measured numbers linked to their source. |
| A fade-up entrance on every section | One scroll-linked playhead. Every other motion is a response to a click. |
| Mono labels, all-caps eyebrows, `→` on links, `A · B · C` meta (tell 5) | Banned outright. D098 already bans most of them. |

Kept from the references, in ClipMux's own form:
- MausBot's job picker becomes the track editor.
- Resend's code tabs become the four steps, and its event stream becomes
  the demo video's real webhook log.
- Mux's agent prompt becomes the Agents section.

### Sections

The nav: the mark and the wordmark (Archivo 700 at 118%); Docs, API,
Changelog, and GitHub with its star count, fetched at build time; and an
**Install** button. There's no Pricing link: it's free software.

**1. Watch (hero)**

```
Video infrastructure
you host.                                        (wdth 125, left)

Upload, encode and stream encrypted HLS from servers you own.
An open-source alternative to Mux and Cloudflare Stream.

┌──────────────────────────────────────────────────────────────┐
│ curl -fsSL https://clipmux.com/install.sh | sudo sh   [Copy] │
└──────────────────────────────────────────────────────────────┘
Linux with Docker. Postgres, Caddy and the dashboard come with it.
[Read the docs]   [GitHub ★ n]

┌────────────────────────── poster, black, 16:9 ───────────────┐
│                                                              │
│              <clipmux-player>, poster first, ▶               │
│                                                              │
│  ━━━━━━━━━━━|━━━━━━━━━━━━|━━━━━━━━━━━|━━━━━━  chapters       │
└──────────────────────────────────────────────────────────────┘
What this video went through
 00:00   Uploaded             4 min 12 s, 1080p source
 00:21   Encoding started     Modal L4
 01:03   video.asset.ready    5 renditions, AES-128
 02:40   video.asset.track.ready   English subtitles, whisper.cpp
 02:52   video.asset.track.ready   Chapters, Claude
```

- **The player** is the real `<clipmux-player>`. It plays a public playback
  ID from production (`app.clipmux.com`), served encrypted by the Worker at
  `media.clipmux.com`. The page is its own proof.
  - It loads on click; the poster image is the largest paint.
  - Subtitles and chapters are on by default.
  - No autoplay.
- **The event log** is the demo video's real event history, with times
  relative to the upload. The numbers above are illustrative.
  - A script (`pnpm --filter site demo`) reads the video and its events
    through the API with a read-only key, and writes
    `site/data/demo.json`.
  - That file is checked in, so CI needs no secret.

**2. Integrate: "Add video to your app in four steps"**

This is a real sequence, so the steps are numbered:
1. Connect the app (`npx @clipmux/node init`).
2. Add one route file.
3. Add two elements.
4. Check it (`doctor`, `listen`).

- Steps sit on the left. The selected step's code shows on the right, on
  Screen.
- A framework switch shows only frameworks with a tested example: Next.js,
  Express and Hono at launch.
- "Any other language" opens the Python, Go and PHP token-signing snippets
  and links to the REST reference.
- One line under it: "The browser never sees an API key or a signing key."

**3. Your stack: "Change where videos are stored, encoded or served. Your
code stays the same."**

```
Storage    [ This server ] [ S3 ] [ R2 ]
Encoding   [ This server's GPU ] [ A PC you own ] [ Modal ] [ RunPod ]
Delivery   [ ClipMux ] [ Cloudflare edge ]
───────────────────────────────────────────────────────────────
Modal: L4 or A10G GPUs, by the minute, under a monthly spend
limit. Modal gets URLs signed for one job, never your storage keys.
Leaves your servers: the source video, for the length of the job.

Your app's code                                       Unchanged
┌──────────────────────────────────────────────────────────────┐
│ export const POST = clipmuxHandler({ upload, playback, … })  │
└──────────────────────────────────────────────────────────────┘
```

- The memorable interaction is the track editor, drawn like a three-track
  editing timeline. Each track is a radio group.
- Choosing a clip changes its description, which always says what leaves
  your servers (a D098 rule). The code panel stays the same, with an
  "Unchanged" mark.
- The descriptions come from `docs/executors.md`, `docs/cloudflare.md` and
  the storage adapters.

**4. Security: "Encrypted before it's stored"**

- A leaked segment URL is shown above a hex dump of its first bytes. The
  caption: "AES-128 ciphertext. The key is derived when a signed viewer
  asks, and is never stored."
- Then rows, each linking to its docs page:
  - playback tokens your backend signs, with no call to ClipMux;
  - keys derived at delivery;
  - encoders never hold storage credentials;
  - ffmpeg under Landlock and seccomp;
  - default-deny routes and two-factor sign-in.
- Last row, in plain words: "This deters copying; it isn't DRM. Anyone who
  can watch can record their screen." The architecture asks the docs to say
  so.

**5. Features: "Everything from upload to analytics"**

- Rows on the left; a screenshot panel on the right shows the selected
  row's screen, as a light or dark pair.
- The rows:
  - uploads that resume;
  - a ladder up to 4K;
  - encoding on this server or a cloud GPU;
  - subtitles and chapters on your own machines;
  - apps, keys and webhooks;
  - share links and analytics.
- The encoding row carries one measured line, linked to the Encoding
  performance page: "A 54-minute 4K lecture, six renditions, encoded on a
  Modal L4 in 18 minutes" (D132).
- Screenshots come from `docs/assets/screenshots/`. Analytics and sharing
  shots are new captures from the demo instance.

**6. Agents: "Hand it to your coding agent"**

```
Paste this into Claude Code, Cursor or Codex:
┌──────────────────────────────────────────────────────────────┐
│ Read https://clipmux.com/agents.md, then add ClipMux uploads,│
│ webhooks and signed playback to this app.            [Copy]  │
└──────────────────────────────────────────────────────────────┘
npx skills add visheshgubrani/clipmux
llms.txt   llms-full.txt   openapi.json   every page as Markdown
```

**7. Install: "Run it on your own server"**

- The one-liner again, with the requirements: Linux on amd64 or arm64,
  Docker with Compose v2, ports 80 and 443, and two DNS names.
- "Read the install guide."
- "ClipMux serves Edural's course videos in production."
- The licence in one line: AGPL-3.0 for the server, MIT for everything your
  apps embed.

**Footer.** Docs, API reference, Changelog, GitHub, Security policy and
Licence.

**Quality floor.**
- JavaScript: Next.js's own runtime (React DOM and the app router) is about
  145 KB gzipped on modern browsers, so the original 100 KB budget can't hold
  on this stack (found in T15.8). The landing page's own code is about 3 KB,
  every section is rendered to HTML at build, and Fumadocs' provider loads
  only on docs pages.
- Zero layout shift: one preloaded, subset variable woff2.
- Visible 2 px violet focus.
- The track editor works from the keyboard.
- Reduced motion respected and AA contrast.
- The page reads correctly with JavaScript off, except the player.

### Copy and process

- **Voice** follows D098: plain words, sentence case, buttons that name
  their action, no hype.
- **Headline.** "Video infrastructure you host." The alternative is "Stream
  video from servers you own." The owner picks one at mock sign-off.
- **Mock first.** As with the dashboard (D098), a clickable mock in light
  and dark comes first, and the owner signs it off before T15.8 is built.
  The mock is the
  [ClipMux landing mock](https://claude.ai/artifact/6xL3k84S8WSEn295rjShZt),
  signed off on 2026-11-18 (D133).
  Where this file and the mock differ, this file wins.

## The docs

### Principles: simple for people and for agents

- **One job per page,** and the title names the task: "Sign playback
  tokens", not "Tokens".
- **Every guide has the same shape:**
  - a one-sentence description, which feeds the meta tags and `llms.txt`;
  - "Before you start";
  - steps;
  - "Check it works";
  - "Next".
- **Numbered steps only for real sequences.**
- **Plain words** (D098):
  - "video" in prose;
  - "asset" only where the API says it, mapped once in Concepts;
  - "private" and "public" for what viewers see, `signed` and `public` in
    the API.
- **Code is complete and copy-pasteable.**
  - Placeholders are always the same: `https://video.example.com`,
    `https://media.example.com`, `lesson_42`, `pid_…`.
  - Snippets in other languages are included from files that CI tests.
- **Say what leaves the servers.** A callout marks every step that sends
  data to a third party: speech APIs, LLMs, Cloudflare, cloud GPUs.
- **Nothing internal:** no decision or task IDs, no "until launch", no
  staging runbooks.
- **Nothing invented.** Every route, field and env var a page names is
  checked against its source (see `site-check`).

### Agent surface

- **`/llms.txt`** follows the format: an H1, a blockquote summary, sections
  for Build, Self-host, Reference and Optional, and absolute `.md` URLs.
  - It's built from each page's frontmatter.
  - The old site's version lacked the summary and sections, and used
    relative URLs.
- **`/llms-full.txt`,** plus one per tab, so an agent can load only
  integrator or only operator docs.
- **Every page as Markdown** at `<url>.md`, built statically.
  - Fumadocs' processed Markdown means Tabs and Steps flatten to readable
    text.
  - `Accept: text/markdown` negotiation is a later improvement, through a
    small Worker in front of the static assets.
- **`/agents.md`** is the prompt from the landing page. It's built from
  `packages/node/AGENTS.md`, which ships with the SDK, plus a header that
  links the skills, `llms.txt` and the API reference. That keeps one source.
- **Page actions** on every page: Copy page, View as Markdown, Open in
  Claude, ChatGPT and Cursor, and Edit on GitHub.
- **The sidebar** has an "AI and agents" group in the Build tab, as
  Cloudflare does: skills, `agents.md`, `llms.txt` and the OpenAPI spec.
- **Stable anchors.** The agent skills (T17) link to page anchors, so
  renaming a heading is a breaking change. `site-check` lists anchors the
  skills use.

### Page template

```
[mark] ClipMux   Build   Self-host   Reference        Search ⌘K   GitHub   ◐
┌ sidebar ─────────┐ ┌ page ──────────────────────────────┐ ┌ On this page ┐
│ Get started      │ │ Sign playback tokens  (wdth 118)   │ │ Before you…  │
│   Quickstart     │ │ Let your backend decide who        │ │ Create a key │
│   First video…   │ │ watches, with tokens it signs.     │ │ Sign         │
│ Upload           │ │ [Copy page] [Open in ▾]            │ │ Check it…    │
│ Play             │ │                                    │ └──────────────┘
│   Player         │ │ Before you start                   │
│ > Sign tokens    │ │ 1. Create a signing key            │
│ …                │ │ 2. Sign a token                    │
└──────────────────┘ │    [Node] [Python] [Go] [PHP]      │
                     │ Check it works                     │
                     │ Next: Posters and thumbnails       │
                     └────────────────────────────────────┘
```

- Fumadocs' layout, themed with the tokens.
- Language tabs are synced across the page and remembered.
- Three callout kinds: Note, Warning and "Leaves your servers".

### Information architecture

Each line is a page, with its source: an existing file to port, "new", or
"generated". Pages marked *(later)* aren't needed for launch. Everything
else is.

**`/docs` Introduction.** What ClipMux is in one paragraph, the diagram from
the README, and three doors:
- "Run a server" leads to Self-host.
- "Add video to an app" leads to Build.
- "I'm a coding agent" leads to `/agents.md`.

Source: README.

**Build** (integrators)

- *Get started*
  - **Quickstart: add video to a Next.js app.** The four steps. Source:
    `packages/node/README.md`, `docs/integration.md`.
  - **Your first video with curl.** Create an upload, send the parts,
    poll or take a webhook, sign a token, play. Source: new, from
    `docs/api.md`.
  - **AI and agents.** `agents.md`, skills, `llms.txt`, page Markdown,
    OpenAPI. Source: new.
- *Concepts*
  - **How ClipMux works.** App, video (asset), output, playback ID, track,
    upload. Source: architecture "Core model", "Why Output exists".
  - **A video's lifecycle.** The state diagram: `waiting_upload`,
    `processing`, `ready`, `errored`, `deleted`. Source: architecture
    "Asset states".
  - **Playback IDs and tokens.** Claims, `v` and `t` audiences, lifetime,
    revocation. Source: architecture "Tokens".
  - **What encryption does and doesn't do.** Source: architecture "Content
    protection".
- *Upload*
  - **Choose an upload method.** Source: new.
  - **Upload from the browser** with `<clipmux-uploader>`. Source: uploader
    README.
  - **Headless uploads and Uppy.** Source: uploader README, D011. This also
    fixes the README's dead link to an Uppy section.
  - **Import from a URL.** Source: new, from the `docs/api.md` row.
  - **Upload from the command line** with `clipmux push` and
    `npx @clipmux/node upload`. Source: new, from usage text.
  - **Choose renditions.** The ladder up to 4K and `renditions`. Source:
    D130, new.
  - **Link videos to your records** with `passthrough`. Source:
    `docs/integration.md` §2.
- *Play*
  - **Add the player.** `<clipmux-player>`, `session-url` or `getToken`,
    events, React types. Source: player README.
  - **Sign playback tokens.** Node, Python, Go, PHP, or any ES256 JWT
    library. Source: `docs/integration.md` §6, T16 snippets.
  - **Public videos and embeds.** Source: new.
  - **Posters and thumbnails,** including `og:image`. Source: node README,
    new.
  - **Other players and native apps:** hls.js, Video.js, Shaka, AVPlayer,
    ExoPlayer, AirPlay and Chromecast token rules. Source: new.
  - **Share links.** Source: new, from api.md and D104.
- *Captions and chapters*
  - **Subtitles and transcripts.** Source: `docs/integration.md` §5, new.
  - **Chapters.** Source: new.
  - **Upload your own captions** *(later; not supported yet)*.
- *Webhooks*
  - **Receive webhooks,** and map their state to your records. Source:
    `docs/integration.md` §4.
  - **Verify signatures** in Node, Python, Go and PHP. Source: T16
    snippets, tested against `internal/webhooks/testdata/vectors.json`.
  - **Develop locally with `listen`.** Source: `docs/integration.md` §9.
  - **Retries, redelivery and secret rotation.** Source: `docs/api.md`.
- *Analytics*
  - **Views, watch time and unique viewers,** including signing `vid`.
    Source: new, from api.md, D103 and D105.
- *Frameworks*
  - **Next.js, Express, Hono, SvelteKit, Remix, Cloudflare Workers, Bun and
    Deno.** One short page each, the handler wired in. Source: node README
    "Other frameworks", `examples/`.
  - **Any language,** with REST and the OpenAPI spec. Source: new.
- *Going live*
  - **Production checklist.** Source: `docs/integration.md` §10.
  - **Troubleshooting:** each error and what to do. Source:
    `docs/integration.md`, `packages/node/AGENTS.md`.

**Self-host** (operators)

- *Get started*
  - **Requirements and sizing:** CPU, GPU, disk (3× the source),
    speech-model RAM, and one replica only. Source: new.
  - **Install with one line.** Source: README, `docs/operations.md`.
  - **First run:** the owner account, URLs, the setup test and two-factor.
    Source: README, architecture "First run".
  - **A tour of the dashboard.** Source: `docs/design.md` "Pages".
- *Install options*
  - **Install from source with Compose.** Source: README.
  - **Behind your own reverse proxy.** Source: `docs/operations.md`.
  - **Upgrade and uninstall.** Source: `docs/operations.md`, T18.
- *Storage*
  - **This server, S3 or R2, and moving videos between them.** Source: new,
    from architecture "Adapters", D110, D117.
- *Encoding*
  - **On this server:** NVENC, VAAPI or CPU. Source: `docs/configuration.md`
    and the encoder table in `docs/agents.md`.
  - **Modal.** Source: `docs/executors.md`.
  - **RunPod.** Source: `docs/executors.md`.
  - **A PC you own.** Source: `docs/agents.md`.
  - **Routing, fallback and spend limits.** Source: architecture "Routing
    and fallback", `docs/executors.md`.
  - **Encoding performance:** measured times per GPU. Source: D129–D132.
- *Delivery*
  - **Built-in delivery.** Source: new.
  - **Cloudflare edge delivery** (Worker and R2). Source:
    `docs/cloudflare.md` lines 1–154.
  - **Edge analytics.** Source: `docs/cloudflare.md`.
- *Captions and AI*
  - **whisper.cpp on your GPU or CPU.** Source: `docs/configuration.md`,
    D131.
  - **Speech APIs.** Source: new, from api.md and configuration.
  - **LLMs for chapters:** Anthropic, OpenAI-compatible, Ollama. Source:
    new.
- *Operate*
  - **Backups and restore.** Source: `docs/operations.md`.
  - **Monitoring:** metrics, logs and health. Source: `docs/operations.md`.
  - **Retention.** Source: `docs/operations.md`.
  - **Rotate keys and secrets.** Source: `docs/operations.md`.
  - **Security checklist.** Source: `docs/operations.md`.
  - **Troubleshooting.** Source: `docs/operations.md`.
- **Security model.** Source: architecture "Security", summarised from
  `docs/security-review.md`.
- **Limitations:** no DRM, one replica, one caption language, no uploaded
  captions, and the like. Source: the public subset of
  `docs/known-gaps.md`.

**Reference**

- **API reference.** The integration API: 34 operations in 11 tags.
  Generated from `internal/openapi/openapi.json`.
- **Webhook events.** The 8 events, with payloads. Generated from the
  spec's `webhooks:`.
- **Errors.** Every HTTP code, asset `error_code` and track `error_code`,
  each with what to do. Source: new, with the spec's per-route `enum`s.
- **Limits and defaults:** rate limits, the 312 GiB upload, 32 MiB parts,
  the 7-day token lifetime, 64 KiB bodies, analytics retention and the
  like. Source: new; P6 open question 7 confirms the defaults first.
- **Environment variables.** Source: `docs/configuration.md`, kept complete
  by `internal/config/doc_test.go`.
- **CLI: `clipmux`:** `serve`, `worker`, `push`, `listen`, `admin` and
  `version`. Source: usage text in `cmd/clipmux`.
- **CLI: `npx @clipmux/node`:** `init`, `doctor`, `listen` and `upload`.
  Source: node README.
- **SDKs:** `@clipmux/node`, `@clipmux/player` and `@clipmux/uploader`.
  Source: the READMEs; TypeDoc comes *(later)*.
- **Management API:** the owner routes the dashboard uses, marked "may
  change". Source: `docs/api.md` (see open question 5).
- **Changelog.** Source: `CHANGELOG.md`, rendered at build.

**Resources**

- **Migrate from Mux:** a concept map (asset, playback ID, signed JWT and
  `passthrough` map almost one to one) and the steps. Source: new; the old
  vod page is reference only.
- **Migrate from Cloudflare Stream.** Source: new.
- **Architecture overview.** Source: a summary of
  `docs/architecture.md`, linking to the full file.
- **FAQ:** "Is this DRM?", "Can I run more than one replica?", "What does
  it cost to run?", "Why AGPL?". Source: new.
- **Contributing** and **Security policy.** Source: `CONTRIBUTING.md` and
  `SECURITY.md`.

### Where content lives afterwards

- **User-facing docs move into `site/content/docs/`:** `operations`,
  `configuration`, `integration`, `api` (its prose; the integration table is
  retired once the spec carries its text), `executors`, `agents`, and the
  public part of `cloudflare`.
  - Each moved file becomes a two-line stub linking to its page. That keeps
    the links in decisions and plans working.
  - `internal/config/doc_test.go` moves its check to the new configuration
    page.
- **`docs/` keeps the internal records:** `architecture`, `decisions`,
  `plans/`, `design`, `known-gaps`, `security-review`, and the Cloudflare
  staging runbook (as `docs/cloudflare-staging.md`).
- **The package READMEs stay,** because npm shows them. They're trimmed to
  a quickstart plus links, and the long form lives in the docs.
- **The spec gets richer** (T15.3), which is Go-side work with tests:
  - tag descriptions and `x-tagGroups`;
  - operation and property descriptions ported from `docs/api.md`;
  - one example request and response per operation, which also covers
    T16's "a request and a response for every route";
  - no "D112" in `info.description`.
- **Drift found while inventorying,** fixed as pages are ported:
  - the uploader README's Uppy link;
  - the stale content-key line in `known-gaps.md`;
  - `worker --pair` help naming the removed Workers page;
  - `CLIPMUX_SPEECH` missing `cuda`;
  - `api.md`'s upload row missing `renditions`.
- **AGENTS.md:**
  - TypeScript is allowed in `site/`;
  - `site` joins the layout notes;
  - "a behaviour change updates its docs page in the same PR";
  - `make site` and `make site-check` join the commands.

### `make site-check`

- Static build, and internal links and anchors resolve.
- Every page has a `title` and a one-sentence `description`.
- No `D\d{3}`, `P\d-T\d+` or "until launch" in `site/content`.
- Every backticked `METHOD /path` in the docs exists in `openapi.json`.
- Every `CLIPMUX_*` name exists in the configuration reference.
- The Python, Go and PHP snippets pass both vector files. This is T16's CI
  requirement; the Go snippet gets its own `go.mod`, so `go vet ./...` at
  the root stays clean.
- `llms.txt` is in the expected format, and every page has a `.md` twin.

## Tasks

| Task | Scope |
| --- | --- |
| T15.1 | **done** (TypeScript 7 works with Next 16.4.6 and Fumadocs 16.19.3; no pinned TypeScript 5) — **Scaffold** (decided in D133). The TypeScript 7 spike; `site/` with Next 16, Fumadocs and static export; tokens, Archivo and the pre-paint theme script; the workspace and Biome entries; `make site`; empty landing and docs shells; `site.yml` (build only). Update AGENTS.md. |
| T15.2 | **done**; the Markdown twins are built under `/raw` and moved to `/docs` by `site/scripts/postbuild.mjs`, and the snippet tests wait for T16 — **Docs plumbing.** Page template; MDX components (Steps, synced Tabs, the three callouts, Files, TypeTable); page actions; `.md` twins; `llms.txt`, `llms-full.txt` and the per-tab files; Orama static search; OG template; sitemap, robots, `_headers` and `_redirects`; `make site-check` with every lint above. |
| T15.3 | **done**: the spec has tag names, descriptions and groups, a description for every operation and webhook, and examples that `internal/openapi/openapi_test.go` validates against their schemas; the reference is generated as plain MDX (see "Stack"); the configuration page moved to the site, with `internal/config/doc_test.go` reading it; a Go test keeps the CLI page complete — **Generated reference.** Enrich `openapi.json` (Go tests stay green; T22's tests still pass); the API reference and webhook events through fumadocs-openapi; errors, limits, env vars, both CLIs and the changelog page. |
| T15.4 | **Build content.** Every launch page in Build, ported or written; T16's snippets and their tests land here. |
| T15.5 | **Self-host content.** Every launch page in Self-host (T18 lands here); stubs left for the moved `docs/*.md`. |
| T15.6 | **Resources.** Migration from Mux and from Cloudflare Stream, the architecture overview, FAQ, contributing, security policy. |
| T15.7 | **Agent surface.** `/agents.md` built from `packages/node/AGENTS.md`; the sidebar's agent group; anchors the skills (T17) use; the agent check (done-criterion 4). |
| T15.8 | **built**, waiting on open question 1: the hero steps through the dashboard's screens with `site/data/demo.json`'s example events until the demo recording is on production; the security rows link to the Build and Self-host introductions until their pages exist (T15.4–T15.5); the star count is fetched at build and left out while the repository is private — **Landing page.** First the clickable mock in light and dark, signed off by the owner. Then build: the timeline nav, the hero with the live player and `demo.json`, four steps, the track editor, security, features with new screenshots, agents, install; meet the quality floor; OG image. |
| T15.9 | **Repoint.** README, package READMEs, `docs/` links and the dashboard's help links point at `clipmux.com/docs`; `known-gaps.md` updated. |
| T15.10 | **changed**: the VPS, not Workers static assets. `make site-deploy` and `deploy/site/clipmux.com.caddy`; the first release is uploaded to `/srv/clipmux-site`, and Caddy switches `clipmux.com` from its redirect to the site once the owner approves the change — **Deploy.** Workers static assets on a preview hostname behind Cloudflare Access; `site.yml` deploys on `main`. At T20 `clipmux.com` moves from its redirect to the repository onto the site. |

**Order:**
- T15.1, then T15.2.
- Then T15.3, T15.4, T15.5 and T15.6 can run side by side, because they
  touch different content folders.
- T15.8's mock starts right after T15.1, and its build after sign-off.
- T15.7 after T15.2 and T17.
- T15.9 after the content tasks.
- T15.10's preview after T15.2. Production comes at T20.

Before each commit: `make site-check`, plus `make lint` and `make test` when
Go changed (T15.3), and `pnpm -r typecheck test`.

## Open questions

Each answer becomes a decision before the task that needs it.

1. **What plays in the hero?** (T15.8) Recommendation: a 40-second
   screen recording of ClipMux itself (upload, encode, then playing with
   generated subtitles and chapters), on production with a public playback
   ID. The fallback is a CC-BY Blender open movie.
2. **Site analytics?** (T15.2) Recommendation: Cloudflare Web Analytics,
   which is cookieless and needs no consent banner. The other choice is
   none.
3. **Comparison pages** (`/compare/mux`, `/compare/cloudflare-stream`)?
   Recommendation: the two migration guides at launch, and marketing
   comparison pages after.
4. **Versioned docs?** Recommendation: one "latest" set, matching the
   latest release, with the version shown in the sidebar. Versioning comes
   with the first breaking API change.
5. **Should the management API be in the public reference?** Recommendation:
   yes, marked "used by the dashboard; may change", because self-hosters
   script it. A machine-readable spec for it comes after launch.
6. **The headline.** **Answered (D133):** "Video infrastructure you host."

## Verification

- `make site-check` is green, and `pnpm --filter site build` writes `out/`
  with no warnings.
- Playwright, against the static build served locally:
  - the landing page and one page per docs tab, at 1280 and 375 px, in
    light and dark;
  - no horizontal scroll and no console errors;
  - the track editor driven by keyboard;
  - the copy buttons and the theme switch.
- A mobile Lighthouse run of the landing page, by hand (CI stays lean): at
  least 95 for performance and 100 for accessibility.
- `curl` against the preview:
  - `/llms.txt`, a page's `.md` (`content-type: text/markdown`) and
    `/agents.md`;
  - `/install.sh`, which returns a 302 to the release asset.
- The agent check and the human check from "What done looks like", recorded
  as a decision with T20's exit checks.
