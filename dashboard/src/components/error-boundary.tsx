import * as React from 'react'
import { ErrorState } from '@/components/query-state'

/** A page that throws while rendering shows what failed and Try again, not a blank screen. */
export class ErrorBoundary extends React.Component<
  { children: React.ReactNode; resetKey?: string },
  { error: unknown }
> {
  override state: { error: unknown } = { error: null }

  static getDerivedStateFromError(error: unknown) {
    return { error }
  }

  override componentDidUpdate(prev: { resetKey?: string }) {
    if (prev.resetKey !== this.props.resetKey && this.state.error) this.setState({ error: null })
  }

  override render() {
    if (this.state.error)
      return (
        <div className="mx-auto w-full max-w-xl px-4 pt-16">
          <ErrorState
            what="this page"
            error={this.state.error}
            onRetry={() => this.setState({ error: null })}
          />
        </div>
      )
    return this.props.children
  }
}
