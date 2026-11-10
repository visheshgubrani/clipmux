import * as React from 'react'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'

interface Ask {
  /** "Delete "Lesson 2"?" */
  title: string
  /** What stops working, and whether it can be undone. */
  description: React.ReactNode
  /** The button names the action: "Delete video", "Revoke key". */
  action: string
  destructive?: boolean
}

type Confirm = (ask: Ask) => Promise<boolean>

const ConfirmContext = React.createContext<Confirm | null>(null)

/** Every destructive action asks first, in a dialog (P6 exit check 6). */
export function ConfirmProvider({ children }: { children: React.ReactNode }) {
  const [ask, setAsk] = React.useState<(Ask & { resolve: (ok: boolean) => void }) | null>(null)
  const confirm = React.useCallback<Confirm>(
    (a) => new Promise<boolean>((resolve) => setAsk({ ...a, resolve })),
    [],
  )
  const close = (ok: boolean) => {
    ask?.resolve(ok)
    setAsk(null)
  }
  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      <AlertDialog open={ask !== null} onOpenChange={(open) => !open && close(false)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{ask?.title}</AlertDialogTitle>
            <AlertDialogDescription>{ask?.description}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              variant={ask?.destructive === false ? 'default' : 'destructive'}
              onClick={() => close(true)}
            >
              {ask?.action}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </ConfirmContext.Provider>
  )
}

export function useConfirm(): Confirm {
  const c = React.useContext(ConfirmContext)
  if (!c) throw new Error('useConfirm outside ConfirmProvider')
  return c
}
