import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { highlight } from 'fumadocs-core/highlight'
import Link from 'next/link'
import type { ReactNode } from 'react'
import { Wordmark } from '@/components/brand-mark'
import { CopyCommand } from '@/components/copy-command'
import { CopyButton } from '@/components/landing/copy-button'
import { type Cue, type DemoEvent, DemoReel, type ReelChapter } from '@/components/landing/demo-reel'
import { type Feature, Features } from '@/components/landing/features'
import { FourSteps, type StepCode } from '@/components/landing/four-steps'
import { StackEditor, type Track } from '@/components/landing/stack-editor'
import { type Chapter, Timeline } from '@/components/landing/timeline'
import demo from '@/data/demo.json'
import { darkTheme, lightTheme } from '@/lib/code-theme'
import { formatStars, stars } from '@/lib/github'
import { INSTALL_COMMAND, REPO_URL, SITE_URL } from '@/lib/site'
import './landing.css'

// The landing page (D133), built from the signed-off mock: a chapter track,
// the poster, four steps, the stack editor, security, features, agents and
// install. Code is highlighted here, at build time.

const code = (source: string, lang: string) =>
  highlight(source, { lang, themes: { light: lightTheme, dark: darkTheme }, defaultColor: false })

const CHAPTERS: Chapter[] = [
  { id: 'watch', label: 'Watch' },
  { id: 'integrate', label: 'Integrate' },
  { id: 'stack', label: 'Your stack' },
  { id: 'security', label: 'Security' },
  { id: 'features', label: 'Features' },
  { id: 'agents', label: 'Agents' },
  { id: 'install', label: 'Install' },
]

const REEL: ReelChapter[] = [
  { name: 'Upload', from: 0, to: 8, src: '/screenshots/videos-dark.webp', alt: "ClipMux's video library" },
  { name: 'Encode', from: 8, to: 16, src: '/screenshots/encoding-dark.webp', alt: 'The Encoding page' },
  { name: 'Captions', from: 16, to: 24, src: '/screenshots/captions-dark.webp', alt: 'Captions and AI' },
  { name: 'Playback', from: 24, to: 34, src: '/screenshots/video-dark.webp', alt: 'A video with subtitles' },
  {
    name: 'Apps and keys',
    from: 34,
    to: 40,
    src: '/screenshots/apps-dark.webp',
    alt: 'Apps, keys and webhooks',
  },
]

const CUES: Cue[] = [
  [0.3, 3.8, 'Drop a video on the library, or upload it from your own app.'],
  [4, 7.8, 'Parts go straight to storage, so a dropped connection resumes.'],
  [8.2, 11.8, 'It encodes on this server’s GPU, or on Modal or RunPod.'],
  [12, 15.8, 'Every rendition is encrypted before it’s stored.'],
  [16.2, 19.8, 'Subtitles come from whisper.cpp, on your own machine.'],
  [20, 23.8, 'An LLM titles the chapters. You choose which one.'],
  [24.2, 28.8, 'The player asks your backend for a token, and plays.'],
  [29, 33.8, 'Revoke a playback ID and it stops at the edge within a minute.'],
  [34.2, 39.6, 'Each app gets its own keys, signing keys and webhooks.'],
]

const CALLBACKS = `({
  // Who may upload, and what the upload is. Return null to refuse.
  async upload(req, { file }) {
    const user = await requireInstructor(req)
    return { passthrough: \`lesson_\${lessonId(req)}\`, title: file.name, generate: { subtitles: true } }
  },
  // Who may watch. Return null to refuse.
  async playback(req, { playbackId }) {
    const user = await getUser(req)
    return (await canWatch(user, playbackId)) ? { viewerId: user.id } : null
  },
  // A verified webhook, with the asset re-fetched so it is current.
  async asset(asset) {
    await db.lesson.update({ where: { ref: asset.passthrough }, data: { status: asset.status } })
  },
})`

const ROUTES: Record<string, { file: string; source: string }> = {
  next: {
    file: 'app/api/clipmux/[action]/route.ts',
    source: `import { clipmuxHandler } from '@clipmux/node/handlers'\n\nexport const POST = clipmuxHandler${CALLBACKS}`,
  },
  express: {
    file: 'server.ts',
    source: `import { clipmuxHandler } from '@clipmux/node/handlers'\nimport { toNodeListener } from '@clipmux/node/express'\n\n// before express.json()\napp.post('/api/clipmux/:action', toNodeListener(clipmuxHandler${CALLBACKS}))`,
  },
  hono: {
    file: 'src/index.ts',
    source: `import { clipmuxHandler } from '@clipmux/node/handlers'\n\nconst clipmux = clipmuxHandler${CALLBACKS}\n\napp.post('/api/clipmux/:action', (c) => clipmux(c.req.raw))`,
  },
}

// The token-signing code for other languages is the docs' own snippets, the
// files `make snippets-test` runs against the delivery vectors.
const snippet = (file: string) => readFileSync(join(process.cwd(), 'snippets', file), 'utf8')
const SIGNING = [
  { key: 'py', label: 'Python', lang: 'python', source: snippet('python/playback_token.py') },
  { key: 'go', label: 'Go', lang: 'go', source: snippet('go/playback_token.go') },
  { key: 'php', label: 'PHP', lang: 'php', source: snippet('php/playback_token.php') },
]

const TRACKS: Track[] = [
  {
    name: 'storage',
    title: 'Storage',
    pieces: [
      {
        value: 'fs',
        label: 'This server',
        text: 'Videos are stored on the server’s own disk, next to Postgres. The simplest start; move to a bucket later without re-encoding.',
        leaves: 'nothing.',
      },
      {
        value: 's3',
        label: 'S3',
        text: 'Any S3-compatible bucket. Browsers upload parts straight to it, so large files never pass through your server.',
        leaves: 'video files, to the bucket you name.',
      },
      {
        value: 'r2',
        label: 'R2',
        text: 'Cloudflare R2, with no egress fees. Pairs with edge delivery, so the Worker reads segments next to viewers.',
        leaves: 'video files, to your R2 bucket.',
      },
    ],
  },
  {
    name: 'encoding',
    title: 'Encoding',
    pieces: [
      {
        value: 'local',
        label: 'This server’s GPU',
        text: 'The built-in worker uses NVENC or VAAPI when it finds a GPU, and the CPU when it doesn’t. ffmpeg runs under Landlock and seccomp.',
        leaves: 'nothing.',
      },
      {
        value: 'pc',
        label: 'A PC you own',
        text: 'Pair a computer with a GPU using one command. It claims jobs over HTTPS and never holds storage credentials.',
        leaves: 'nothing beyond your own machines.',
      },
      {
        value: 'modal',
        label: 'Modal',
        text: 'L4 or A10G GPUs by the minute, under a monthly spend limit. Modal gets URLs signed for one job, never your storage keys.',
        leaves: 'the source video, for the length of the job.',
      },
      {
        value: 'runpod',
        label: 'RunPod',
        text: 'Serverless GPUs by the second, with the same spend limit and the same signed, one-job URLs.',
        leaves: 'the source video, for the length of the job.',
      },
    ],
  },
  {
    name: 'delivery',
    title: 'Delivery',
    pieces: [
      {
        value: 'origin',
        label: 'ClipMux',
        text: 'The built-in handler checks tokens, derives keys and serves HLS straight from the server, behind Caddy.',
        leaves: 'nothing.',
      },
      {
        value: 'edge',
        label: 'Cloudflare edge',
        text: 'Connect Cloudflare in one dialog. The ClipMux Worker serves from R2 at the edge, and videos keep playing while your server is down.',
        leaves: 'viewer requests and segments, through Cloudflare.',
      },
    ],
  },
]

const FEATURES: Feature[] = [
  {
    title: 'Uploads that resume',
    text: 'Browsers send parts straight to storage, so a dropped connection picks up where it stopped. Import from a URL, or push from the command line.',
    shot: 'videos',
    caption: 'The video library',
  },
  {
    title: 'Encoding here or on a cloud GPU',
    text: 'NVENC or VAAPI when the server has a GPU. Or Modal and RunPod by the minute, under a monthly spend limit. A ladder up to 4K.',
    number:
      'Measured on production: a 54-minute 4K lecture, six renditions, encoded on a Modal L4 in 18 minutes.',
    shot: 'encoding',
    caption: 'Encoding: this server or a cloud GPU',
  },
  {
    title: 'Subtitles and chapters on your machines',
    text: 'whisper.cpp on your GPU or CPU, or any OpenAI-compatible speech API. Chapters from Claude or any OpenAI-compatible model, Ollama included.',
    shot: 'captions',
    caption: 'Captions and AI: whisper.cpp, a speech API, an LLM for chapters',
  },
  {
    title: 'A player that refreshes its own tokens',
    text: '<clipmux-player> on Video.js, with subtitles, chapters and posters. Native HLS on Safari and iOS.',
    shot: 'video',
    caption: 'A video playing with its generated subtitles',
  },
  {
    title: 'Apps, keys and webhooks',
    text: 'One App per integration, each with scoped API keys, signing keys, HMAC-signed webhooks and its own videos.',
    shot: 'apps',
    caption: 'An app’s keys, signing keys and webhooks',
  },
]

const AGENT_PROMPT = `Read ${SITE_URL}/agents.md, then add ClipMux uploads, webhooks and signed playback to this app.`

function GitHubMark() {
  return (
    <svg viewBox="0 0 16 16" aria-hidden="true">
      <path d="M8 0C3.58 0 0 3.58 0 8a8 8 0 0 0 5.47 7.59c.4.07.55-.17.55-.38v-1.33c-2.23.48-2.7-1.07-2.7-1.07-.36-.92-.89-1.17-.89-1.17-.73-.5.05-.49.05-.49.8.06 1.23.83 1.23.83.72 1.22 1.87.87 2.33.66.07-.52.28-.87.5-1.07-1.78-.2-3.65-.89-3.65-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.6 7.6 0 0 1 4 0c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48v2.2c0 .21.15.46.55.38A8 8 0 0 0 16 8c0-4.42-3.58-8-8-8Z" />
    </svg>
  )
}

function Section({
  id,
  title,
  lede,
  children,
}: {
  id: string
  title: string
  lede?: ReactNode
  children: ReactNode
}) {
  return (
    <section className="chapter" id={id}>
      <div className="wrap">
        <div className="sec-head">
          <h2 className="h2">{title}</h2>
          {lede ? <p className="lede">{lede}</p> : null}
        </div>
        {children}
      </div>
    </section>
  )
}

export default async function Home() {
  const stepCode: StepCode = {}
  const install = await code(
    'npm i @clipmux/node @clipmux/uploader @clipmux/player\nnpx @clipmux/node init --server https://video.example.com',
    'bash',
  )
  const elements = await code(
    '<clipmux-uploader create-url="/api/clipmux/upload?lesson=42"></clipmux-uploader>\n\n<clipmux-player playback-id="pid_…" session-url="/api/clipmux/playback"></clipmux-player>',
    'html',
  )
  const check = await code(
    'npx @clipmux/node doctor\nnpx @clipmux/node listen --forward-to localhost:3000/api/clipmux/webhook',
    'bash',
  )
  for (const [fw, r] of Object.entries(ROUTES)) {
    stepCode[fw] = [
      { file: 'terminal', code: install },
      { file: r.file, code: await code(r.source, 'ts') },
      { file: 'lesson.html', code: elements },
      { file: 'terminal', code: check },
    ]
  }
  const signing = await Promise.all(SIGNING.map(async (s) => ({ ...s, code: await code(s.source, s.lang) })))
  const unchangedCode = await code(
    'export const POST = clipmuxHandler({\n  upload,    // who may upload\n  playback,  // who may watch\n  asset,     // a verified webhook\n})',
    'ts',
  )

  return (
    <div className="landing">
      <header className="top">
        <div className="wrap">
          <nav className="nav" aria-label="Main">
            <Link className="brand" href="/">
              <Wordmark />
            </Link>
            <div className="navlinks">
              <Link className="keep" href="/docs">
                Docs
              </Link>
              <Link href="/docs/reference/api">API</Link>
              <Link href="/docs/reference/changelog">Changelog</Link>
            </div>
            <span className="spacer" />
            <a className="gh" href={REPO_URL}>
              <GitHubMark />
              GitHub{stars !== null ? ` ${formatStars(stars)}` : ''}
            </a>
            <a className="btn btn-primary" href="#install">
              Install
            </a>
          </nav>
        </div>
        <div className="wrap">
          <Timeline chapters={CHAPTERS} />
        </div>
      </header>

      <main>
        <section className="chapter hero" id="watch">
          <div className="wrap">
            <h1 className="type-display">Video infrastructure you host.</h1>
            <p className="sub">
              Upload, encode and stream encrypted HLS from servers you own.{' '}
              <b>An open-source alternative to Mux and Cloudflare Stream.</b>
            </p>
            <div className="install">
              <CopyCommand command={INSTALL_COMMAND} />
              <p className="req">Linux with Docker. Postgres, Caddy and the dashboard come with it.</p>
            </div>
            <div className="ctas">
              <Link className="btn btn-quiet" href="/docs">
                Read the docs
              </Link>
              <a className="btn btn-quiet" href="#integrate">
                Add video to an app
              </a>
            </div>
            <DemoReel
              chapters={REEL}
              cues={CUES}
              events={demo.events as DemoEvent[]}
              illustrative={demo.illustrative}
            />
          </div>
        </section>

        <Section
          id="integrate"
          title="Add video to your app in four steps"
          lede="Uploads, webhooks and signed playback with the Node SDK. The browser never sees an API key or a signing key."
        >
          <FourSteps
            steps={[
              {
                title: 'Connect the app',
                why: (
                  <>
                    You approve it in the dashboard, and the credentials land in <code>.env.local</code>{' '}
                    without being printed.
                  </>
                ),
              },
              {
                title: 'Add one route file',
                why: 'Three callbacks decide who uploads, who watches, and what to store when a video is ready.',
              },
              {
                title: 'Add two elements',
                why: 'A resumable uploader and a player that refreshes its own tokens.',
              },
              {
                title: 'Check it',
                why: (
                  <>
                    <code>doctor</code> tests the connection, and <code>listen</code> forwards webhooks to
                    your laptop.
                  </>
                ),
              },
            ]}
            frameworks={[
              { key: 'next', label: 'Next.js' },
              { key: 'express', label: 'Express' },
              { key: 'hono', label: 'Hono' },
            ]}
            code={stepCode}
            other={signing}
          />
        </Section>

        <Section
          id="stack"
          title="Change where videos are stored, encoded or served. Your code stays the same."
          lede="Storage, encoding and delivery each sit behind an adapter. Pick a piece on each track."
        >
          <StackEditor tracks={TRACKS} code={unchangedCode} file="app/api/clipmux/[action]/route.ts" />
        </Section>

        <Section
          id="security"
          title="Encrypted before it’s stored"
          lede="Every output is AES-128 fMP4 HLS. Stored files, cached copies and leaked segment URLs are all ciphertext."
        >
          <div className="leak">
            <div className="url">GET https://media.example.com/v/…/pid_…/1080p/seg_00042.m4s</div>
            <pre className="hex">
              <span className="o">00000000</span>
              {'  9f 3a c1 7e 52 08 d4 e6  b1 0c 7f 29 e8 44 a3 5d\n'}
              <span className="o">00000010</span>
              {'  c6 71 2b f0 98 1e 6a d3  47 bc 05 e9 3f 82 ad 14\n'}
              <span className="o">00000020</span>
              {'  5e d8 63 0a f7 b4 29 c5  1d 86 ee 3b 70 9a 42 cf\n'}
              <span className="o">00000030</span>
              {'  08 f1 b6 4d 2c 95 7a e3  61 d0 3e a8 17 5b c9 84'}
            </pre>
            <p className="cap">
              AES-128 ciphertext. The key is derived when a signed viewer asks, and is never stored.
            </p>
          </div>
          <ul className="rows">
            <li>
              <h3>Your backend signs playback tokens</h3>
              <p>
                Short-lived ES256 tokens, signed locally with no call to ClipMux. Playback IDs are public or
                private, and revocable. <Link href="/docs/build">Signed playback</Link>
              </p>
            </li>
            <li>
              <h3>Keys are derived at delivery</h3>
              <p>
                Content keys come from one secret held outside the database. They are never written to
                storage. <Link href="/docs/self-host">How encryption works</Link>
              </p>
            </li>
            <li>
              <h3>Encoders never hold storage credentials</h3>
              <p>
                A worker gets URLs signed for its one job. Modal, RunPod or a PC at home never see your bucket
                keys. <Link href="/docs/self-host">Security model</Link>
              </p>
            </li>
            <li>
              <h3>ffmpeg runs in a sandbox</h3>
              <p>
                Landlock and seccomp, worker file names treated as untrusted, routes default-deny, and
                two-factor sign-in for the owner. <Link href="/docs/self-host">Security checklist</Link>
              </p>
            </li>
            <li className="honest">
              <h3>What it isn’t</h3>
              <p>
                This deters copying. It isn’t DRM: anyone who can watch a video can record their screen.{' '}
                <Link href="/docs/self-host">What encryption does and doesn’t do</Link>
              </p>
            </li>
          </ul>
        </Section>

        <Section
          id="features"
          title="Everything from upload to analytics"
          lede="One Go binary and Postgres, with a dashboard you’ll actually use, light and dark, on a laptop or a phone."
        >
          <Features features={FEATURES} />
        </Section>

        <Section
          id="agents"
          title="Hand it to your coding agent"
          lede="Install the ClipMux skills, or point your agent at the guide the SDK ships. Every docs page is also plain Markdown."
        >
          <div className="agent-grid">
            <div className="grid gap-4">
              <div className="prompt">
                <span className="label">Paste this into Claude Code, Cursor or Codex</span>
                <p className="q">{AGENT_PROMPT}</p>
                <div className="row">
                  <span className="label">The API key never reaches browser code.</span>
                  <CopyButton text={AGENT_PROMPT} />
                </div>
              </div>
              <CopyCommand command="npx skills add visheshgubrani/clipmux" />
            </div>
            <ul className="links">
              <li>
                <a href="/llms.txt">
                  <code>llms.txt</code>
                </a>
                <span>An index of every page</span>
              </li>
              <li>
                <a href="/llms-full.txt">
                  <code>llms-full.txt</code>
                </a>
                <span>All the docs in one file</span>
              </li>
              <li>
                <a href="/openapi.json">
                  <code>openapi.json</code>
                </a>
                <span>The integration API, OpenAPI 3.1</span>
              </li>
              <li>
                <a href="/docs.md">
                  <code>/docs/….md</code>
                </a>
                <span>Any page as Markdown</span>
              </li>
            </ul>
          </div>
        </Section>

        <section className="chapter" id="install">
          <div className="wrap">
            <div className="close-grid">
              <div>
                <h2 className="h2">Run it on your own server</h2>
                <div className="install">
                  <CopyCommand command={INSTALL_COMMAND} />
                </div>
                <ul className="reqs">
                  <li>Linux on amd64 or arm64</li>
                  <li>Docker with Compose v2</li>
                  <li>Ports 80 and 443 free</li>
                  <li>Two DNS names: one for the dashboard, one for media</li>
                </ul>
                <div className="ctas" style={{ marginTop: 24 }}>
                  <Link className="btn btn-primary" href="/docs/self-host">
                    Read the install guide
                  </Link>
                </div>
              </div>
              <div className="facts">
                <p>ClipMux serves Edural’s course videos in production.</p>
                <p>
                  AGPL-3.0 for the server.{' '}
                  <span>
                    MIT for everything your apps embed: the SDK, the uploader, the player and the Worker.
                  </span>
                </p>
                <p>Built on FFmpeg, Shaka Packager, whisper.cpp, Video.js, PostgreSQL and Caddy.</p>
              </div>
            </div>
          </div>
        </section>
      </main>

      <footer>
        <div className="wrap">
          <Link className="brand" href="/">
            <Wordmark />
          </Link>
          <nav aria-label="Footer">
            <Link href="/docs">Docs</Link>
            <Link href="/docs/reference/api">API reference</Link>
            <Link href="/docs/reference/changelog">Changelog</Link>
            <a href={REPO_URL}>GitHub</a>
            <a href={`${REPO_URL}/blob/main/SECURITY.md`}>Security policy</a>
            <a href={`${REPO_URL}/blob/main/LICENSE`}>Licence</a>
          </nav>
        </div>
      </footer>
    </div>
  )
}
