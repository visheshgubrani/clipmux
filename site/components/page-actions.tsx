'use client'

import { Popover, PopoverContent, PopoverTrigger } from 'fumadocs-ui/components/ui/popover'
import { Check, ChevronDown, Copy } from 'lucide-react'
import { useState } from 'react'

const button =
  'inline-flex h-8 cursor-pointer items-center gap-1.5 rounded-md border border-hairline bg-screen px-2.5 text-[13px] font-medium text-ink hover:border-graphite'
const item = 'block rounded-md px-2.5 py-1.5 text-[14px] text-ink no-underline hover:bg-fd-accent'

// Copy page, and open it as Markdown, in a chat assistant, or on GitHub.
// `markdownUrl` is the page's Markdown twin; `absoluteMarkdownUrl` is what an
// assistant is told to read.
export function PageActions({
  markdownUrl,
  absoluteMarkdownUrl,
  editUrl,
}: {
  markdownUrl: string
  absoluteMarkdownUrl: string
  editUrl: string
}) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle')
  const prompt = encodeURIComponent(`Read ${absoluteMarkdownUrl}, then help me with ClipMux.`)
  const open = [
    { label: 'View as Markdown', href: markdownUrl },
    { label: 'Open in Claude', href: `https://claude.ai/new?q=${prompt}` },
    { label: 'Open in ChatGPT', href: `https://chatgpt.com/?hints=search&q=${prompt}` },
    { label: 'Open in Cursor', href: `https://cursor.com/link/prompt?text=${prompt}` },
    { label: 'Edit on GitHub', href: editUrl },
  ]

  async function copy() {
    try {
      const res = await fetch(markdownUrl)
      if (!res.ok) throw new Error(`${res.status}`)
      await navigator.clipboard.writeText(await res.text())
      setState('copied')
    } catch {
      setState('failed')
    }
    setTimeout(() => setState('idle'), 1800)
  }

  return (
    <div className="not-prose flex flex-wrap gap-2">
      <button type="button" className={button} onClick={copy}>
        {state === 'copied' ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
        {state === 'copied' ? 'Copied' : state === 'failed' ? 'Copy failed' : 'Copy page'}
      </button>
      <Popover>
        <PopoverTrigger className={button}>
          Open in
          <ChevronDown className="size-3.5" />
        </PopoverTrigger>
        <PopoverContent align="start" className="flex w-52 flex-col p-1">
          {open.map((o) => (
            <a key={o.label} href={o.href} className={item} target="_blank" rel="noreferrer noopener">
              {o.label}
            </a>
          ))}
        </PopoverContent>
      </Popover>
    </div>
  )
}
