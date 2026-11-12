// React 19 JSX types for <clipmux-player>. Types only: React is not a
// dependency. Import it once in an app that renders the element from JSX.
//
//   import type {} from '@clipmux/player/react'
import type * as React from 'react'
import type { ClipMuxPlayerElement, ClipMuxPlayerProps } from './element.ts'

declare module 'react' {
  namespace JSX {
    interface IntrinsicElements {
      'clipmux-player': React.DetailedHTMLProps<
        React.HTMLAttributes<ClipMuxPlayerElement>,
        ClipMuxPlayerElement
      > &
        ClipMuxPlayerProps
    }
  }
}
