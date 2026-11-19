# Examples

Two small apps that integrate ClipMux the way the
[quickstart](https://clipmux.com/docs/build/quickstart) does: `init`, one
route, two elements and three callbacks. Each uses only the published
packages, `@clipmux/node`, `@clipmux/uploader` and `@clipmux/player`.

| Example                    | What it shows                                                                                                                                                              |
| -------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`nextjs-lms`](nextjs-lms) | A course site on the Next.js App Router: instructors upload a lesson's video, enrolled students watch it, and anyone else is refused. Launch exit check 8 runs against it. |
| [`express`](express)       | The same handler in Express through `toNodeListener`, with server-rendered pages and a Vite-bundled script for the elements.                                               |

In this repository they are pnpm workspace packages, so they build against
the packages in `packages/`. Copied out, replace `workspace:*` with the
released versions.
