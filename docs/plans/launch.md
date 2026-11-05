# Going public — checklist

Run after the Phase 1 exit test passes (D005).

- [ ] Choose the licence, including how the image complies with FFmpeg's licence when it bundles a GPL build (Shaka Packager is BSD).
- [ ] Add LICENSE, CONTRIBUTING, CODE_OF_CONDUCT, SECURITY (templates in the archived `visheshgubrani/vod`).
- [ ] Make `visheshgubrani/clipmux` public.
- [ ] Unarchive `visheshgubrani/vod`, link this repo from its README banner, re-archive.
- [ ] Publish `@clipmux/player` and `@clipmux/uploader` at versions above 0.2.0; `npm deprecate` `@clipmux/*@0.1.x`.
- [ ] Publish the image to `ghcr.io/visheshgubrani/clipmux` (never `clipmux/server` or `clipmux/web`).
- [ ] Docs site (structure from the archived repo's `docs-site/`).
- [ ] Domain plan: `clipmux.com`, `api.`, `media.` stay on clipmux-lms until the LMS migrates; pick docs/marketing hostnames that don't collide.
