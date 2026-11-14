import { Autocomplete as AutocompletePrimitive } from '@base-ui/react/autocomplete'
import type * as React from 'react'
import { cn } from '@/lib/utils'

/**
 * A text input with suggestions (Base UI's Autocomplete): the value is
 * whatever is typed, and a suggestion only fills it in.
 */
const Autocomplete = AutocompletePrimitive.Root

function AutocompleteInput({
  className,
  ...props
}: React.ComponentProps<typeof AutocompletePrimitive.Input>) {
  return (
    <AutocompletePrimitive.Input
      data-slot="autocomplete-input"
      className={cn(
        'h-8 w-full min-w-0 rounded-lg border border-input bg-transparent px-2.5 py-1 text-base transition-colors outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:pointer-events-none disabled:cursor-not-allowed disabled:bg-input/50 disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-3 aria-invalid:ring-destructive/20 md:text-sm dark:bg-input/30 dark:disabled:bg-input/80 dark:aria-invalid:border-destructive/50 dark:aria-invalid:ring-destructive/40',
        className,
      )}
      {...props}
    />
  )
}

function AutocompleteContent({
  className,
  children,
  ...props
}: React.ComponentProps<typeof AutocompletePrimitive.Popup>) {
  return (
    <AutocompletePrimitive.Portal>
      <AutocompletePrimitive.Positioner className="isolate z-50 outline-none" sideOffset={4}>
        <AutocompletePrimitive.Popup
          data-slot="combobox-content"
          className={cn(
            'z-50 max-h-[min(var(--available-height),18rem)] w-(--anchor-width) min-w-48 origin-(--transform-origin) overflow-x-hidden overflow-y-auto rounded-lg bg-popover p-1 text-popover-foreground shadow-md ring-1 ring-foreground/10 duration-100 outline-none data-open:animate-in data-open:fade-in-0 data-open:zoom-in-95 data-closed:animate-out data-closed:fade-out-0 data-closed:zoom-out-95',
            className,
          )}
          {...props}
        >
          {children}
        </AutocompletePrimitive.Popup>
      </AutocompletePrimitive.Positioner>
    </AutocompletePrimitive.Portal>
  )
}

const AutocompleteList = AutocompletePrimitive.List
const AutocompleteCollection = AutocompletePrimitive.Collection

function AutocompleteGroup({
  className,
  ...props
}: React.ComponentProps<typeof AutocompletePrimitive.Group>) {
  return (
    <AutocompletePrimitive.Group
      data-slot="autocomplete-group"
      className={cn('py-0.5', className)}
      {...props}
    />
  )
}

function AutocompleteGroupLabel({
  className,
  ...props
}: React.ComponentProps<typeof AutocompletePrimitive.GroupLabel>) {
  return (
    <AutocompletePrimitive.GroupLabel
      data-slot="autocomplete-group-label"
      className={cn('px-1.5 py-1 text-xs font-medium text-muted-foreground', className)}
      {...props}
    />
  )
}

function AutocompleteItem({ className, ...props }: React.ComponentProps<typeof AutocompletePrimitive.Item>) {
  return (
    <AutocompletePrimitive.Item
      data-slot="autocomplete-item"
      className={cn(
        'relative flex cursor-default items-center gap-1.5 rounded-md px-1.5 py-1 text-sm outline-hidden select-none data-highlighted:bg-accent data-highlighted:text-accent-foreground data-disabled:pointer-events-none data-disabled:opacity-50',
        className,
      )}
      {...props}
    />
  )
}

function AutocompleteEmpty({
  className,
  ...props
}: React.ComponentProps<typeof AutocompletePrimitive.Empty>) {
  return (
    <AutocompletePrimitive.Empty
      data-slot="autocomplete-empty"
      className={cn('px-1.5 py-1 text-sm text-muted-foreground empty:hidden', className)}
      {...props}
    />
  )
}

export {
  Autocomplete,
  AutocompleteCollection,
  AutocompleteContent,
  AutocompleteEmpty,
  AutocompleteGroup,
  AutocompleteGroupLabel,
  AutocompleteInput,
  AutocompleteItem,
  AutocompleteList,
}
