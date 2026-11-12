import { UploadIcon } from 'lucide-react'
import * as React from 'react'
import { PageBody, PageHeader } from '@/components/page'
import { Button } from '@/components/ui/button'
import { useUpload } from '@/components/upload'

/** /upload opens the Upload dialog; the dialog itself is available from anywhere. */
export function UploadPage() {
  const upload = useUpload()
  // biome-ignore lint/correctness/useExhaustiveDependencies: open once on arrival
  React.useEffect(() => upload.open(), [])
  return (
    <PageBody>
      <PageHeader
        title="Upload"
        description="Videos go to the Dashboard app unless you pick another under Advanced."
        actions={
          <Button onClick={() => upload.open()}>
            <UploadIcon data-icon="inline-start" />
            Choose videos
          </Button>
        }
      />
    </PageBody>
  )
}
