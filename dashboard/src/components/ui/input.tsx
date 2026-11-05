import type { InputHTMLAttributes } from 'react'
import { cn } from '../../lib/utils.ts'

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      className={cn(
        'h-9 w-full rounded-md border border-zinc-300 bg-white px-3 text-sm placeholder:text-zinc-400 focus-visible:outline-2 focus-visible:outline-zinc-900',
        className,
      )}
      {...props}
    />
  )
}

export function Label({
  children,
  ...props
}: InputHTMLAttributes<HTMLLabelElement> & { children: React.ReactNode }) {
  return (
    // biome-ignore lint/a11y/noLabelWithoutControl: callers nest the <Input> inside
    <label className="grid gap-1.5 text-sm font-medium" {...props}>
      {children}
    </label>
  )
}
