'use client'

import { type ReactNode, useState } from 'react'
import { arrowKeys } from './tabs'

export type Step = { title: string; why: ReactNode }
export type Framework = { key: string; label: string }
// code[framework][step] is the step's highlighted code and its file name.
export type StepCode = Record<string, { file: string; code: ReactNode }[]>

export function FourSteps({
  steps,
  frameworks,
  code,
  other,
}: {
  steps: Step[]
  frameworks: Framework[]
  code: StepCode
  other: { key: string; label: string; code: ReactNode }[]
}) {
  const [step, setStep] = useState(0)
  const [fw, setFw] = useState(frameworks[0]?.key ?? '')
  const [lang, setLang] = useState(other[0]?.key ?? '')
  const shown = code[fw]?.[step]

  return (
    <div className="steps-grid">
      <div>
        <div className="steps" role="tablist" aria-label="Integration steps" aria-orientation="vertical">
          {steps.map((s, i) => (
            <button
              key={s.title}
              type="button"
              role="tab"
              className="step"
              aria-selected={i === step}
              tabIndex={i === step ? 0 : -1}
              onClick={() => setStep(i)}
              onKeyDown={arrowKeys}
            >
              <span className="n">{i + 1}</span>
              <span className="ttl">{s.title}</span>
              <span className="why">{s.why}</span>
            </button>
          ))}
        </div>
        <p className="aside">
          The handler is a plain Fetch handler, so it also runs in SvelteKit, Remix, Bun, Deno and Cloudflare
          Workers.
        </p>
        <details className="other">
          <summary>Any other language</summary>
          <div className="panel">
            <div className="panel-head" role="tablist" aria-label="Language">
              {other.map((o) => (
                <button
                  key={o.key}
                  type="button"
                  role="tab"
                  className="tab"
                  aria-selected={o.key === lang}
                  tabIndex={o.key === lang ? 0 : -1}
                  onClick={() => setLang(o.key)}
                  onKeyDown={arrowKeys}
                >
                  {o.label}
                </button>
              ))}
              <span className="filename">sign a playback token</span>
            </div>
            <div className="code">{other.find((o) => o.key === lang)?.code}</div>
          </div>
        </details>
      </div>
      <div className="panel">
        <div className="panel-head" role="tablist" aria-label="Framework">
          {frameworks.map((f) => (
            <button
              key={f.key}
              type="button"
              role="tab"
              className="tab"
              aria-selected={f.key === fw}
              tabIndex={f.key === fw ? 0 : -1}
              onClick={() => {
                setFw(f.key)
                setStep(1)
              }}
              onKeyDown={arrowKeys}
            >
              {f.label}
            </button>
          ))}
          <span className="filename">{shown?.file}</span>
        </div>
        <div className="code">{shown?.code}</div>
      </div>
    </div>
  )
}
