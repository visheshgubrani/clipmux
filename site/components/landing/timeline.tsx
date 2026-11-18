'use client'

import { useEffect, useRef, useState } from 'react'

export type Chapter = { id: string; label: string }

// The page's chapter track: a marker per section at its share of the page,
// and the violet playhead at the scroll position. It moves only with scroll,
// so it needs nothing for reduced motion.
export function Timeline({ chapters }: { chapters: Chapter[] }) {
  const [starts, setStarts] = useState<number[]>(() => chapters.map((_, i) => i / chapters.length))
  const [progress, setProgress] = useState(0)
  const startsRef = useRef(starts)

  useEffect(() => {
    const max = () => Math.max(1, document.documentElement.scrollHeight - window.innerHeight)
    const layout = () => {
      const next = chapters.map((c, i) => {
        if (i === 0) return 0
        const el = document.getElementById(c.id)
        return el ? Math.min(1, Math.max(0, (el.offsetTop - 90) / max())) : i / chapters.length
      })
      startsRef.current = next
      setStarts(next)
    }
    const tick = () => setProgress(Math.min(1, Math.max(0, window.scrollY / max())))
    layout()
    tick()
    const ro = new ResizeObserver(layout)
    ro.observe(document.body)
    window.addEventListener('scroll', tick, { passive: true })
    return () => {
      ro.disconnect()
      window.removeEventListener('scroll', tick)
    }
  }, [chapters])

  let current = 0
  starts.forEach((s, i) => {
    if (progress >= s - 0.001) current = i
  })

  return (
    <nav className="timeline" aria-label="Page chapters">
      <div className="track">
        <div className="played" style={{ width: `${progress * 100}%` }} />
        {chapters.map((c, i) => (
          <a
            key={c.id}
            href={`#${c.id}`}
            // Labels near the right edge sit left of their tick, so they never overflow.
            className={`marker${i === current ? ' on' : ''}${(starts[i] ?? 0) > 0.85 ? ' end' : ''}`}
            style={{ left: `${(starts[i] ?? 0) * 100}%` }}
            aria-current={i === current ? 'location' : undefined}
          >
            <span>{c.label}</span>
          </a>
        ))}
        <div className="playhead" style={{ left: `${progress * 100}%` }} aria-hidden="true" />
      </div>
    </nav>
  )
}
