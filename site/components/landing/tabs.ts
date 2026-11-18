import type { KeyboardEvent } from 'react'

// Arrow keys move between the tabs of one tablist, as WAI-ARIA tabs do.
export function arrowKeys(e: KeyboardEvent<HTMLElement>) {
  const step =
    e.key === 'ArrowRight' || e.key === 'ArrowDown'
      ? 1
      : e.key === 'ArrowLeft' || e.key === 'ArrowUp'
        ? -1
        : 0
  if (!step) return
  const list = e.currentTarget.closest('[role="tablist"]')
  const tabs = list ? [...list.querySelectorAll<HTMLElement>('[role="tab"]')] : []
  const i = tabs.indexOf(e.currentTarget)
  const next = tabs[(i + step + tabs.length) % tabs.length]
  if (!next) return
  e.preventDefault()
  next.focus()
  next.click()
}
