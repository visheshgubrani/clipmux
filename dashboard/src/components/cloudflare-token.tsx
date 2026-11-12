import { useMutation } from '@tanstack/react-query'
import { ExternalLinkIcon } from 'lucide-react'
import * as React from 'react'
import { type CloudflareInspection, post } from '@/api'
import { buttonVariants } from '@/components/ui/button'

// Getting a Cloudflare token (D109): a link opens Cloudflare's create-token
// form with the permissions a flow needs already chosen; the owner creates
// it and pastes it back, and the server reads what it reaches so nothing
// is typed by hand.

/** Opens Cloudflare's create-token form, pre-filled, in a new tab. */
export function CreateTokenLink({ url }: { url: string }) {
  return (
    <a
      href={url}
      target="_blank"
      rel="noopener noreferrer"
      className={buttonVariants({ variant: 'outline', size: 'sm', className: 'w-fit' })}
    >
      <ExternalLinkIcon data-icon="inline-start" />
      Create token on Cloudflare
    </a>
  )
}

/**
 * What a pasted token reaches, read a moment after the owner stops typing.
 * The token goes to this server only, for that one call.
 */
export function useInspection(token: string, accountID: string) {
  const inspect = useMutation({
    mutationFn: (body: { token: string; account_id: string | undefined }) =>
      post<CloudflareInspection>('/v1/cloudflare/inspect', body),
  })
  const { mutate, reset } = inspect
  React.useEffect(() => {
    const t = token.trim()
    if (!t) {
      reset()
      return
    }
    const timer = setTimeout(() => mutate({ token: t, account_id: accountID || undefined }), 400)
    return () => clearTimeout(timer)
  }, [token, accountID, mutate, reset])
  return inspect
}
