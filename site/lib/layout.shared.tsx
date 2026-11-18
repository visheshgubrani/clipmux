import type { BaseLayoutProps } from 'fumadocs-ui/layouts/shared'
import { Wordmark } from '@/components/brand-mark'
import { REPO_URL } from '@/lib/site'

export function baseOptions(): BaseLayoutProps {
  return {
    nav: { title: <Wordmark />, url: '/' },
    githubUrl: REPO_URL,
  }
}
