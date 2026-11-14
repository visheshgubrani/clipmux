import type * as React from 'react'
import { Label } from '@/components/ui/label'
import { RadioGroupItem } from '@/components/ui/radio-group'

/**
 * One choice in a RadioGroup of cards (Encoding, Captions): a radio, its
 * title and blurb, a status on the right, and whatever else it needs below.
 * The radio's id is `{group}-{value}`.
 */
export function Choice({
  group,
  value,
  title,
  body,
  status,
  disabled,
  children,
  ...rest
}: {
  group: string
  value: string
  title: string
  body: React.ReactNode
  status?: React.ReactNode
  disabled?: boolean
  children?: React.ReactNode
} & { [data: `data-${string}`]: string | undefined }) {
  const id = `${group}-${value}`
  return (
    <div
      {...rest}
      className="flex flex-col gap-3 rounded-lg border bg-card p-4 has-data-checked:border-primary has-data-checked:bg-primary/5"
    >
      <div className="flex flex-wrap items-start gap-x-3 gap-y-2">
        <RadioGroupItem value={value} id={id} disabled={disabled} className="mt-0.5" />
        <div className="flex min-w-0 flex-[1_1_16rem] flex-col gap-1">
          <Label htmlFor={id} className="font-medium">
            {title}
          </Label>
          <p className="text-sm text-muted-foreground">{body}</p>
        </div>
        {status}
      </div>
      {children && <div className="flex flex-col gap-3 pl-7">{children}</div>}
    </div>
  )
}
