// Before every build, dev server and typecheck, gets what the landing page
// shows from outside site/:
//   - the dashboard screenshots, from docs/assets/screenshots (their one home
//     in the repository), into public/screenshots;
//   - the repository's star count, from GitHub's API, into data/github.json
//     (null when GitHub can't be reached; the nav then shows no number).
// Both outputs are git-ignored. Reading the count here keeps it out of
// Next.js's fetch cache, which would otherwise keep the first answer forever.
import { cpSync, writeFileSync } from 'node:fs'

cpSync(
  new URL('../../docs/assets/screenshots/', import.meta.url),
  new URL('../public/screenshots/', import.meta.url),
  {
    recursive: true,
  },
)

let stars = null
try {
  const res = await fetch('https://api.github.com/repos/visheshgubrani/clipmux', {
    headers: { Accept: 'application/vnd.github+json' },
    signal: AbortSignal.timeout(5000),
  })
  if (res.ok) {
    const body = await res.json()
    if (typeof body.stargazers_count === 'number') stars = body.stargazers_count
  }
} catch {
  // Offline or rate-limited: build without a count.
}
writeFileSync(new URL('../data/github.json', import.meta.url), `${JSON.stringify({ stars })}\n`)
console.log(`prepare-landing: screenshots copied, ${stars ?? 'no'} stars`)
