import { describe } from '@/api'
import { toast } from '@/components/ui/toast'

// Toasts name what happened with the action's own word ("Delete video" →
// "Video deleted"). Errors stay until dismissed.
export const notify = {
  done(title: string, description?: string) {
    toast.add({ title, ...(description ? { description } : {}), type: 'success' })
  },
  failed(title: string, err: unknown) {
    toast.add({ title, description: describe(err), type: 'error', timeout: 0 })
  },
}
