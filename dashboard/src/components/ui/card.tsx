import type { HTMLAttributes } from 'react'
import { cn } from '../../lib/utils.ts'

export function Card({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div className={cn('rounded-lg border border-zinc-200 bg-white p-6 shadow-sm', className)} {...props} />
  )
}

export function CardTitle({ className, ...props }: HTMLAttributes<HTMLHeadingElement>) {
  return <h2 className={cn('text-lg font-semibold', className)} {...props} />
}

export function Muted({ className, ...props }: HTMLAttributes<HTMLParagraphElement>) {
  return <p className={cn('text-sm text-zinc-500', className)} {...props} />
}

/** A secret shown once, with a copy button. */
export function Secret({ label, value }: { label: string; value: string }) {
  return (
    <div className="grid gap-2 rounded-md border border-amber-300 bg-amber-50 p-3">
      <div className="text-sm font-medium">{label} — shown once; store it now.</div>
      <pre className="overflow-x-auto whitespace-pre-wrap break-all rounded bg-white p-2 font-mono text-xs">
        {value}
      </pre>
      <button
        type="button"
        className="justify-self-start text-xs underline"
        onClick={() => navigator.clipboard.writeText(value)}
      >
        Copy
      </button>
    </div>
  )
}
