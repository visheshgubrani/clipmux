// .env files as `init` writes and `doctor`/`listen` read them: KEY=VALUE
// lines, values optionally in double quotes with \n escapes left as they
// are (the SDK reads a signing key either way). Merging replaces the lines
// of the keys it sets and appends the rest, leaving every other line alone.

export function parseEnv(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of text.split(/\r?\n/)) {
    const m = /^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$/.exec(line)
    if (!m) continue
    let v = (m[2] as string).trim()
    if (v.length >= 2 && ((v.startsWith('"') && v.endsWith('"')) || (v.startsWith("'") && v.endsWith("'")))) {
      v = v.slice(1, -1)
    } else {
      v = v.replace(/\s+#.*$/, '')
    }
    out[m[1] as string] = v
  }
  return out
}

/** Values need quotes when they hold anything but plain characters. */
function quote(v: string): string {
  return /^[A-Za-z0-9_./:@+=-]*$/.test(v) ? v : `"${v.replace(/\n/g, '\\n').replace(/"/g, '\\"')}"`
}

export function mergeEnv(text: string, values: Record<string, string>, header: string): string {
  const pending = new Map(Object.entries(values))
  const lines = text === '' ? [] : text.replace(/\r?\n$/, '').split(/\r?\n/)
  const out = lines.map((line) => {
    const m = /^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=/.exec(line)
    const key = m?.[1]
    if (key && pending.has(key)) {
      const v = pending.get(key) as string
      pending.delete(key)
      return `${key}=${quote(v)}`
    }
    return line
  })
  if (pending.size > 0) {
    if (out.length > 0 && out.at(-1) !== '') out.push('')
    out.push(header)
    for (const [k, v] of pending) out.push(`${k}=${quote(v)}`)
  }
  return `${out.join('\n')}\n`
}
