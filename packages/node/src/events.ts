// The events long-poll's window, as internal/listen moves it: `since` is
// where each page starts and `after` the created_at of the newest event
// seen (the poll waits for a newer one). Both are RFC 3339 times; the
// server refuses anything else. The window moves up behind the last event
// with an overlap that catches events committed out of order, unless the
// page was full, when it moves to the last event exactly (an overlap could
// then read the same full page forever).

import type { Event } from './schema.gen.ts'

const OVERLAP_MS = 30_000
const PAGE = 100

export class EventWindow {
  since: string
  after: string | undefined
  #newest = Number.NEGATIVE_INFINITY

  constructor(since: Date) {
    this.since = since.toISOString()
  }

  /** Moves the window past a page of events (oldest first). */
  advance(page: readonly Event[]): void {
    const last = page.at(-1)
    if (!last) return
    for (const e of page) {
      // Keep the server's own string: it has microseconds a Date drops,
      // and a truncated `after` would make the newest event look new again.
      const t = Date.parse(e.created_at)
      if (t >= this.#newest) {
        this.#newest = t
        this.after = e.created_at
      }
    }
    if (page.length >= PAGE) {
      this.since = last.created_at
      return
    }
    const moved = Date.parse(last.created_at) - OVERLAP_MS
    if (moved > Date.parse(this.since)) this.since = new Date(moved).toISOString()
  }
}
