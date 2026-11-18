'use client'

import { type ReactNode, useState } from 'react'

export type Piece = { value: string; label: string; text: string; leaves: string }
export type Track = { name: string; title: string; pieces: Piece[] }

// Three tracks, like an editing timeline: pick where videos are stored,
// encoded and served. The app's code panel never changes, which is the point.
export function StackEditor({ tracks, code, file }: { tracks: Track[]; code: ReactNode; file: string }) {
  const [chosen, setChosen] = useState<Record<string, string>>(() =>
    Object.fromEntries(tracks.map((t) => [t.name, t.pieces[0]?.value ?? ''])),
  )
  const [pulse, setPulse] = useState(0)

  return (
    <>
      <form onSubmit={(e) => e.preventDefault()}>
        {tracks.map((t) => (
          <fieldset key={t.name} className="trk">
            <legend>{t.title}</legend>
            <div className="clips">
              {t.pieces.map((p) => (
                <label key={p.value} className="clip">
                  <input
                    type="radio"
                    name={t.name}
                    value={p.value}
                    checked={chosen[t.name] === p.value}
                    onChange={() => {
                      setChosen((c) => ({ ...c, [t.name]: p.value }))
                      setPulse((n) => n + 1)
                    }}
                  />
                  <span>{p.label}</span>
                </label>
              ))}
            </div>
          </fieldset>
        ))}
      </form>
      <div className="stack-grid">
        <div aria-live="polite">
          {tracks.map((t) => {
            const p = t.pieces.find((x) => x.value === chosen[t.name])
            if (!p) return null
            return (
              <div key={t.name} className="desc">
                <h3>
                  {t.title}: {p.label}
                </h3>
                <p>{p.text}</p>
                <p className="leaves">
                  <b>Leaves your servers:</b> {p.leaves}
                </p>
              </div>
            )
          })}
        </div>
        <div className="panel">
          <div className="panel-head">
            <span className="filename" style={{ marginLeft: 0 }}>
              {file}
            </span>
            {/* Remounted on each choice, so the pulse plays again. */}
            <span key={pulse} className={`unchanged${pulse ? ' pulse' : ''}`}>
              <svg viewBox="0 0 24 24" aria-hidden="true">
                <path d="M5 12.5l4.5 4.5L19 7.5" />
              </svg>
              Unchanged
            </span>
          </div>
          <div className="code">{code}</div>
        </div>
      </div>
    </>
  )
}
