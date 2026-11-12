import * as React from 'react'
import { cn } from '@/lib/utils'

/** A page's wide title, its one-line purpose, and its actions on the right. */
export function PageHeader({
  title,
  description,
  actions,
  className,
}: {
  title: React.ReactNode
  description?: React.ReactNode
  actions?: React.ReactNode
  className?: string
}) {
  return (
    <header className={cn('flex flex-wrap items-end justify-between gap-x-6 gap-y-3', className)}>
      <div className="flex min-w-0 flex-col gap-1.5">
        <h1 className="type-page-title text-balance">{title}</h1>
        {description && <p className="max-w-[68ch] text-sm text-muted-foreground">{description}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </header>
  )
}

/** A titled part of a page: a heading, an optional line, and its content. No box around it. */
export function Section({
  title,
  description,
  actions,
  children,
  className,
  ...props
}: {
  title: React.ReactNode
  description?: React.ReactNode
  actions?: React.ReactNode
  children: React.ReactNode
  className?: string
} & Omit<React.ComponentProps<'section'>, 'title'>) {
  // A section is a named region: its heading names it unless it's labelled.
  const id = React.useId()
  return (
    <section
      aria-labelledby={props['aria-label'] ? undefined : id}
      className={cn('flex flex-col gap-3', className)}
      {...props}
    >
      <div className="flex flex-wrap items-end justify-between gap-x-4 gap-y-2">
        <div className="flex min-w-0 flex-col gap-0.5">
          <h2 id={id} className="type-section">
            {title}
          </h2>
          {description && <p className="max-w-[68ch] text-sm text-muted-foreground">{description}</p>}
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
      {children}
    </section>
  )
}

/** The width every signed-in page sits in. */
export function PageBody({ className, ...props }: React.ComponentProps<'div'>) {
  return (
    <div
      className={cn(
        'mx-auto flex w-full max-w-[1200px] flex-col gap-10 px-4 pt-6 pb-16 sm:px-8 sm:pt-8',
        className,
      )}
      {...props}
    />
  )
}
