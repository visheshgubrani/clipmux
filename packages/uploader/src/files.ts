// File checks of <clipmux-uploader>, apart from the DOM so they test in Node.

/** Whether file matches an accept list such as "video/*,.mkv". */
export function accepts(accept: string | null, file: { name: string; type: string }): boolean {
  const rules = (accept ?? '')
    .split(',')
    .map((r) => r.trim().toLowerCase())
    .filter(Boolean)
  if (rules.length === 0) return true
  const name = file.name.toLowerCase()
  const type = file.type.toLowerCase()
  return rules.some((r) => {
    if (r.startsWith('.')) return name.endsWith(r)
    if (r.endsWith('/*')) return type.startsWith(r.slice(0, -1))
    return type === r
  })
}

/** Bytes as people read them: 1.5 GB, 32 MB. */
export function formatBytes(n: number): string {
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (n >= 1000 && i < units.length - 1) {
    n /= 1000
    i++
  }
  return `${i === 0 || n >= 100 ? Math.round(n) : n.toFixed(1)} ${units[i]}`
}
