---
type: llm
focus: trace
weight: 1
---

No file a browser loads (a 'use client' module, or anything it imports) imports @clipmux/node or reads a CLIPMUX_* variable, and no CLIPMUX_* variable has a NEXT_PUBLIC_ prefix.
