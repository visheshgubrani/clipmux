'use client'

import Image from 'next/image'
import { useCallback, useEffect, useRef, useState } from 'react'

export type ReelChapter = { name: string; from: number; to: number; src: string; alt: string }
export type Cue = [from: number, to: number, text: string]
export type DemoEvent = { at: string; what: string; detail: string; code?: boolean }

const PLAY = 'M7 4.5v15l12.5-7.5z'
const PAUSE = 'M6.5 4.5h4v15h-4zM13.5 4.5h4v15h-4z'
const fmt = (s: number) => `${Math.floor(s / 60)}:${String(Math.floor(s % 60)).padStart(2, '0')}`

// The hero's poster: a player for the recorded demo, with chapters,
// subtitles and the video's event log filling in as it plays. Until the
// recording is on production it steps through the dashboard's screens
// (P6-T15-site.md, open question 1). It never autoplays.
export function DemoReel({
  chapters,
  cues,
  events,
  illustrative,
}: {
  chapters: ReelChapter[]
  cues: Cue[]
  events: DemoEvent[]
  illustrative: boolean
}) {
  const duration = chapters.at(-1)?.to ?? 0
  const [t, setT] = useState(0)
  const [playing, setPlaying] = useState(false)
  const [started, setStarted] = useState(false)
  const [captions, setCaptions] = useState(true)
  const last = useRef(0)

  useEffect(() => {
    if (!playing) return
    let raf = 0
    last.current = performance.now()
    const frame = (now: number) => {
      const dt = (now - last.current) / 1000
      last.current = now
      let done = false
      setT((prev) => {
        const next = prev + dt
        if (next >= duration) {
          done = true
          return duration
        }
        return next
      })
      if (done) setPlaying(false)
      else raf = requestAnimationFrame(frame)
    }
    raf = requestAnimationFrame(frame)
    return () => cancelAnimationFrame(raf)
  }, [playing, duration])

  const play = useCallback(() => {
    setStarted(true)
    setT((prev) => (prev >= duration ? 0 : prev))
    setPlaying(true)
  }, [duration])

  const current = Math.max(
    0,
    chapters.findIndex((c) => t >= c.from && t < c.to),
  )
  const chapterIndex = t >= duration ? chapters.length - 1 : current
  const cue = cues.find(([from, to]) => t >= from && t < to)
  // Event times map onto the reel in order, one per chapter boundary.
  const eventAt = (i: number) => chapters[Math.min(i, chapters.length - 1)]?.from ?? 0

  return (
    <>
      <div className="poster">
        <div className="screen">
          <div className="frame">
            {chapters.map((c, i) => (
              <Image
                key={c.src}
                src={c.src}
                alt={c.alt}
                className={i === chapterIndex ? 'on' : ''}
                priority={i === 0}
                width={1600}
                height={1000}
              />
            ))}
          </div>
          {started && captions && cue ? <div className="cue">{cue[2]}</div> : null}
          {!playing && !started ? (
            <button type="button" className="bigplay" onClick={play} aria-label="Play the demo">
              <svg viewBox="0 0 24 24" aria-hidden="true">
                <path d={PLAY} />
              </svg>
            </button>
          ) : null}
        </div>
        <div className="controls">
          <button
            type="button"
            onClick={() => (playing ? setPlaying(false) : play())}
            aria-label={playing ? 'Pause' : 'Play'}
          >
            <svg viewBox="0 0 24 24" aria-hidden="true">
              <path d={playing ? PAUSE : PLAY} />
            </svg>
          </button>
          <span className="tc">
            {fmt(t)} / {fmt(duration)}
          </span>
          <div className="chapters">
            {chapters.map((c) => {
              const w = t >= c.to ? 100 : t <= c.from ? 0 : ((t - c.from) / (c.to - c.from)) * 100
              return (
                <button
                  type="button"
                  key={c.name}
                  className="chap"
                  style={{ flex: c.to - c.from }}
                  aria-label={`Chapter: ${c.name}`}
                  title={c.name}
                  onClick={() => {
                    setT(c.from)
                    if (!playing) play()
                  }}
                >
                  <i style={{ width: `${w}%` }} />
                </button>
              )
            })}
          </div>
          <span className="chapname">{chapters[chapterIndex]?.name}</span>
          <button
            type="button"
            className="cc"
            aria-pressed={captions}
            aria-label="Subtitles"
            onClick={() => setCaptions((v) => !v)}
          >
            CC
          </button>
        </div>
      </div>
      <div className="log" aria-live="polite">
        <h3>What this video went through</h3>
        <ol>
          {events.map((e, i) => (
            <li key={`${e.at}-${e.what}`} className={started && t < eventAt(i) ? 'pending' : undefined}>
              <span className="t">{e.at}</span>
              <span className="e">{e.code ? <code>{e.what}</code> : e.what}</span>
              <span className="d">{e.detail}</span>
            </li>
          ))}
        </ol>
        {illustrative ? (
          <p className="note">
            Shown with example times until the demo recording is published; then the player streams it from
            media.clipmux.com and these rows come from its own event history.
          </p>
        ) : null}
      </div>
    </>
  )
}
