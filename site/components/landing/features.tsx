'use client'

import Image from 'next/image'
import { useState } from 'react'
import { arrowKeys } from './tabs'

export type Feature = { title: string; text: string; number?: string; shot: string; caption: string }

// Feature rows; the selected row's dashboard screen shows beside them, in
// the light or dark capture to match the theme.
export function Features({ features }: { features: Feature[] }) {
  const [i, setI] = useState(0)
  const f = features[i]
  return (
    <div className="feat-grid">
      <div className="feats" role="tablist" aria-label="Features" aria-orientation="vertical">
        {features.map((x, n) => (
          <button
            key={x.title}
            type="button"
            role="tab"
            className="feat"
            aria-selected={n === i}
            tabIndex={n === i ? 0 : -1}
            onClick={() => setI(n)}
            onKeyDown={arrowKeys}
          >
            <span className="ttl">{x.title}</span>
            <span className="txt">{x.text}</span>
            {x.number ? <span className="num">{x.number}</span> : null}
          </button>
        ))}
      </div>
      {f ? (
        <figure className="shot">
          <Image
            className="shot-light"
            src={`/screenshots/${f.shot}-light.webp`}
            alt={f.caption}
            width={1600}
            height={1000}
            loading="lazy"
          />
          <Image
            className="shot-dark"
            src={`/screenshots/${f.shot}-dark.webp`}
            alt={f.caption}
            width={1600}
            height={1000}
            loading="lazy"
          />
          <figcaption>{f.caption}</figcaption>
        </figure>
      ) : null}
    </div>
  )
}
