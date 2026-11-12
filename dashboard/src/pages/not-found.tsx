import { Link } from 'react-router'
import { PageBody } from '@/components/page'
import { Button } from '@/components/ui/button'
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyTitle } from '@/components/ui/empty'

export function NotFound() {
  return (
    <PageBody>
      <Empty className="mt-16">
        <EmptyHeader>
          <EmptyTitle>Nothing here</EmptyTitle>
          <EmptyDescription>This address doesn't match a page in the dashboard.</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button render={<Link to="/" />}>Go home</Button>
        </EmptyContent>
      </Empty>
    </PageBody>
  )
}
