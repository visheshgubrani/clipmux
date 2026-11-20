---
tags: [clipmux, exit-7]
max_turns: 150
timeout_seconds: 3600
allowed_tools: [Read, Glob, Grep, Skill, Write, Edit, Bash, WebFetch]
---

This is our course site (Next.js). Add lesson videos with our ClipMux server at {{CLIPMUX_URL}}: the instructor uploads a video on the lesson page, ClipMux's webhook tells the app when it's ready, and enrolled students watch it with signed playback. Anyone who may not watch the lesson must not get the video. The app runs on port 3000, so the webhook URL is http://127.0.0.1:3000/api/clipmux/webhook.

I'm the ClipMux server's owner. When you need me to approve something, run `{{APPROVE}} '<the approval URL>'`, which is me approving it in the dashboard with that webhook URL.

Finish when `npm run build` passes and everything is checked; don't leave a server running.
