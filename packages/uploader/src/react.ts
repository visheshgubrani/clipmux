// React 19 JSX types for <clipmux-uploader>. Types only: React is not a
// dependency. Import it once in an app that renders the element from JSX.
//
//   import type {} from '@clipmux/uploader/react'
import type * as React from 'react'
import type { ClipMuxUploaderElement, ClipMuxUploaderProps } from './element.ts'

declare module 'react' {
  namespace JSX {
    interface IntrinsicElements {
      'clipmux-uploader': React.DetailedHTMLProps<
        React.HTMLAttributes<ClipMuxUploaderElement>,
        ClipMuxUploaderElement
      > &
        ClipMuxUploaderProps
    }
  }
}
