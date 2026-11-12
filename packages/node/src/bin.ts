#!/usr/bin/env node
// The `npx @clipmux/node` entry point: main() with the real process.

import { spawn } from 'node:child_process'
import { main } from './cli.ts'

function openBrowser(url: string): void {
  const [cmd, args] =
    process.platform === 'darwin'
      ? ['open', [url]]
      : process.platform === 'win32'
        ? ['cmd', ['/c', 'start', '', url]]
        : ['xdg-open', [url]]
  try {
    spawn(cmd as string, args as string[], { stdio: 'ignore', detached: true })
      .on('error', () => {})
      .unref()
  } catch {
    // no browser: the URL is printed above
  }
}

const ac = new AbortController()
process.once('SIGINT', () => ac.abort())
const code = await main(process.argv.slice(2), {
  out: (l) => process.stdout.write(`${l}\n`),
  err: (l) => process.stderr.write(`${l}\n`),
  env: process.env,
  cwd: process.cwd(),
  fetch: globalThis.fetch,
  sleep: (ms) => new Promise((r) => setTimeout(r, ms)),
  open: openBrowser,
  signal: ac.signal,
})
process.exit(code)
