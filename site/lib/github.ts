import { REPO_URL } from '@/lib/site'

// The repository's star count, fetched once at build time. Null while the
// repository is private or GitHub can't be reached; the nav then shows the
// link without a number.
export async function starCount(): Promise<number | null> {
  try {
    const res = await fetch(REPO_URL.replace('https://github.com/', 'https://api.github.com/repos/'), {
      headers: { Accept: 'application/vnd.github+json' },
      signal: AbortSignal.timeout(5000),
    })
    if (!res.ok) return null
    const body = (await res.json()) as { stargazers_count?: unknown }
    return typeof body.stargazers_count === 'number' ? body.stargazers_count : null
  } catch {
    return null
  }
}

export function formatStars(n: number): string {
  return n >= 1000 ? `${(n / 1000).toFixed(n >= 10000 ? 0 : 1).replace(/\.0$/, '')}k` : String(n)
}
