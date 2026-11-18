'use client'

import { useState } from 'react'

// A shell command with a copy button. The `$` prompt is not copied.
export function CopyCommand({ command }: { command: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="flex min-w-0 items-center gap-3 rounded-lg border border-hairline bg-screen py-1.5 pr-1.5 pl-4">
      <code className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap py-2 font-mono text-[14.5px]">
        <span className="select-none text-graphite">$ </span>
        {command}
      </code>
      <button
        type="button"
        className="h-8 flex-none cursor-pointer rounded-md border border-[var(--input)] px-3 text-[13px] font-semibold hover:border-graphite"
        onClick={() => {
          navigator.clipboard.writeText(command).then(
            () => {
              setCopied(true)
              setTimeout(() => setCopied(false), 1600)
            },
            () => setCopied(false),
          )
        }}
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  )
}
