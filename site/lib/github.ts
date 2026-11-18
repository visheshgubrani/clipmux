import github from '@/data/github.json'

// The repository's star count, read at build by scripts/prepare-landing.mjs.
export const stars: number | null = github.stars

export function formatStars(n: number): string {
  return n >= 1000 ? `${(n / 1000).toFixed(n >= 10000 ? 0 : 1).replace(/\.0$/, '')}k` : String(n)
}
