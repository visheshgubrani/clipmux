import { CheckIcon, CopyIcon } from 'lucide-react'
import * as React from 'react'
import { Button } from '@/components/ui/button'
import { InputGroup, InputGroupAddon, InputGroupButton, InputGroupInput } from '@/components/ui/input-group'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'

function useCopy() {
  const [copied, setCopied] = React.useState(false)
  const copy = async (value: string) => {
    await navigator.clipboard.writeText(value)
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }
  return { copied, copy }
}

/** An icon button that copies a value: IDs, URLs. */
export function CopyButton({
  value,
  label = 'Copy',
  className,
}: {
  value: string
  label?: string
  className?: string
}) {
  const { copied, copy } = useCopy()
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button
            variant="ghost"
            size="icon-xs"
            aria-label={label}
            className={cn('text-muted-foreground', className)}
            onClick={() => void copy(value)}
          />
        }
      >
        {copied ? <CheckIcon /> : <CopyIcon />}
      </TooltipTrigger>
      <TooltipContent>{copied ? 'Copied' : label}</TooltipContent>
    </Tooltip>
  )
}

/** An ID in monospace that truncates, with its copy button. */
export function CopyID({ value, className }: { value: string; className?: string }) {
  return (
    <span className={cn('flex min-w-0 items-center gap-1', className)}>
      <code className="truncate text-[13px]">{value}</code>
      <CopyButton value={value} label="Copy ID" />
    </span>
  )
}

/** A read-only field for a secret or a command, with Copy. Scrolls inside itself, never the page. */
export function CopyField({ value, label, className }: { value: string; label: string; className?: string }) {
  const { copied, copy } = useCopy()
  return (
    <InputGroup className={className}>
      <InputGroupInput readOnly value={value} aria-label={label} className="font-mono text-[13px]" />
      <InputGroupAddon align="inline-end">
        <InputGroupButton onClick={() => void copy(value)} aria-label={`Copy ${label}`}>
          {copied ? <CheckIcon data-icon="inline-start" /> : <CopyIcon data-icon="inline-start" />}
          {copied ? 'Copied' : 'Copy'}
        </InputGroupButton>
      </InputGroupAddon>
    </InputGroup>
  )
}

/** A block for multi-line values (a PEM key, a command that wraps), with Copy. */
export function CopyBlock({ value, label }: { value: string; label: string }) {
  const { copied, copy } = useCopy()
  return (
    <div className="flex min-w-0 items-start gap-2 rounded-lg border bg-muted/50 p-2 pl-3">
      <pre className="min-w-0 flex-1 overflow-x-auto py-1 font-mono text-[12.5px] leading-relaxed whitespace-pre-wrap break-all">
        {value}
      </pre>
      <Button variant="outline" size="sm" onClick={() => void copy(value)} aria-label={`Copy ${label}`}>
        {copied ? <CheckIcon data-icon="inline-start" /> : <CopyIcon data-icon="inline-start" />}
        {copied ? 'Copied' : 'Copy'}
      </Button>
    </div>
  )
}
