// What the exit-check suites share: a command they run, and a free port.
import { type ChildProcess, spawn } from 'node:child_process'
import { createServer } from 'node:net'
import { expect } from '@playwright/test'

/** A command the suite runs, with everything it printed. */
export class Run {
  readonly proc: ChildProcess
  output = ''
  readonly exited: Promise<number | null>

  constructor(cwd: string, cmd: string, args: string[], env: Record<string, string | undefined> = {}) {
    // Its own process group, so stop() reaches what npx starts as well.
    this.proc = spawn(cmd, args, { cwd, env: { ...process.env, ...env }, detached: true })
    const add = (b: Buffer) => {
      this.output += b.toString()
    }
    this.proc.stdout?.on('data', add)
    this.proc.stderr?.on('data', add)
    this.exited = new Promise((done) => this.proc.on('exit', (code) => done(code)))
  }

  async waitFor(re: RegExp, timeout = 30_000): Promise<RegExpMatchArray> {
    let m: RegExpMatchArray | null = null
    try {
      await expect.poll(() => (m = this.output.match(re)), { timeout }).not.toBeNull()
    } catch (err) {
      throw new Error(`no ${re} in the output:\n${this.output}`, { cause: err })
    }
    return m as unknown as RegExpMatchArray
  }

  async stop(): Promise<number | null> {
    const group = (signal: NodeJS.Signals) => {
      try {
        if (this.proc.pid) process.kill(-this.proc.pid, signal)
      } catch {
        // already gone
      }
    }
    if (this.proc.exitCode === null && this.proc.signalCode === null) {
      group('SIGINT')
      const killed = setTimeout(() => group('SIGKILL'), 5_000)
      await this.exited
      clearTimeout(killed)
    }
    return this.exited
  }
}

export function freePort(): Promise<number> {
  return new Promise((done, fail) => {
    const s = createServer()
    s.on('error', fail)
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address() as { port: number }
      s.close(() => done(port))
    })
  })
}
