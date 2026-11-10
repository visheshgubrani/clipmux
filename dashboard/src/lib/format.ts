// Formatting in the dashboard's voice: timecode for durations, plain
// relative times, and sizes people read at a glance.

/** 24:15, or 1:02:07 past an hour; null when unknown. */
export function timecode(seconds: number | null | undefined): string | null {
  if (seconds == null || !Number.isFinite(seconds)) return null
  const s = Math.max(0, Math.round(seconds))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const ss = String(s % 60).padStart(2, '0')
  return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`
}

export const clock = (ms: number) => timecode(ms / 1000) ?? '0:00'

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })
const dateFmt = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric' })
const fullFmt = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' })

/** "12 minutes ago", "yesterday", or "Nov 6" past a week. */
export function ago(iso: string | null | undefined): string {
  if (!iso) return 'never'
  const diff = (Date.parse(iso) - Date.now()) / 1000
  const abs = Math.abs(diff)
  if (abs < 45) return 'just now'
  if (abs < 3600) return rtf.format(Math.round(diff / 60), 'minute')
  if (abs < 86400) return rtf.format(Math.round(diff / 3600), 'hour')
  if (abs < 7 * 86400) return rtf.format(Math.round(diff / 86400), 'day')
  return dateFmt.format(new Date(iso))
}

export const fullDate = (iso: string) => fullFmt.format(new Date(iso))

export function bytes(n: number): string {
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let v = n
  let i = 0
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000
    i++
  }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}

export function hours(seconds: number): string {
  if (seconds < 3600) return `${Math.round(seconds / 60)} min`
  return `${new Intl.NumberFormat().format(Math.round(seconds / 3600))} h`
}

export const count = (n: number) => new Intl.NumberFormat().format(n)

export const dollars = (cents: number) => `$${(cents / 100).toFixed(2)}`

/** A video's name for people: its title, else its reference, else its ID. */
export function videoName(a: { title?: string | undefined; passthrough?: string | undefined; id: string }) {
  return a.title || a.passthrough || a.id
}

/** Hello in the owner's time of day. */
export function greeting(d = new Date()): string {
  const h = d.getHours()
  if (h < 5) return 'Good evening'
  if (h < 12) return 'Good morning'
  if (h < 18) return 'Good afternoon'
  return 'Good evening'
}

/** A video's page. The app goes along so the page can act as it (the ClipMux-App header). */
export const videoPath = (id: string, app?: string | undefined) =>
  `/videos/${id}${app ? `?app=${encodeURIComponent(app)}` : ''}`
