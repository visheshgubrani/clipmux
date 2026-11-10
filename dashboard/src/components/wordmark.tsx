import { cn } from '@/lib/utils'

/** The mark: a frame with a play head and the "mux" bar beside it. */
export function Mark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 32 32" aria-hidden="true" className={cn('size-7 shrink-0', className)}>
      <rect width="32" height="32" rx="7" className="fill-primary" />
      <rect x="7" y="9.5" width="2.5" height="13" rx="1.25" fill="#fff" opacity="0.6" />
      <path d="M12.5 9.5v13l10.5-6.5z" fill="#fff" />
    </svg>
  )
}

/** The mark with "ClipMux" set wide, the brand's one typographic move. */
export function Wordmark({ className }: { className?: string }) {
  return (
    <span className={cn('flex items-center gap-2.5', className)}>
      <Mark />
      <span className="text-[17px] font-semibold tracking-[-0.005em] [font-stretch:118%]">ClipMux</span>
    </span>
  )
}
