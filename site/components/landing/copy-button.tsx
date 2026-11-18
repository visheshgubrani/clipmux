'use client'

import { useState } from 'react'

// Copies `text`; says so, or says to copy by hand if the browser refuses.
export function CopyButton({ text, className = '' }: { text: string; className?: string }) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle')
  return (
    <button
      type="button"
      className={`h-8 flex-none cursor-pointer rounded-md border border-[var(--input)] px-3 text-[13px] font-semibold hover:border-graphite ${className}`}
      onClick={() => {
        navigator.clipboard.writeText(text).then(
          () => setState('copied'),
          () => setState('failed'),
        )
        setTimeout(() => setState('idle'), 1600)
      }}
    >
      {state === 'copied' ? 'Copied' : state === 'failed' ? 'Select and copy' : 'Copy'}
    </button>
  )
}
