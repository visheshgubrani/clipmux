import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import type { FullResult, Reporter, TestCase, TestError, TestResult } from '@playwright/test/reporter'

// Never print raw Playwright errors: they can include credentials or signed URLs.
function redact(message: string): string {
  const path = process.env.CLIPMUX_E2E_LIVE_CONFIG
  if (path) {
    const state = dirname(resolve(path))
    for (const file of ['cloudflare-token', 'owner.json', 'integration.json']) {
      try {
        const raw = readFileSync(resolve(state, file), 'utf8').trim()
        const values: string[] = file.endsWith('.json') ? Object.values(JSON.parse(raw)) : [raw]
        for (const value of values) {
          if (typeof value === 'string' && value.length > 5) {
            message = message.replaceAll(value, '[redacted]')
            message = message.replaceAll(JSON.stringify(value).slice(1, -1), '[redacted]')
          }
        }
      } catch {
        return 'Diagnostic withheld: private redaction files unavailable.'
      }
    }
  }
  return message.replace(/https?:\/\/[^\s"'<>]+/g, '[URL]').slice(0, 4000)
}

export default class LiveReporter implements Reporter {
  private executed = 0
  onTestEnd(test: TestCase, result: TestResult): void {
    this.executed++
    process.stdout.write(`${result.status}: ${test.title}\n`)
    for (const error of result.errors) process.stdout.write(`${redact(error.message ?? '')}\n`)
  }
  onError(error: TestError): void {
    process.stderr.write(`${redact(error.message ?? 'Live infrastructure failure')}\n`)
  }
  onEnd(result: FullResult): void {
    process.stdout.write(
      this.executed ? `Live suite: ${result.status} (${this.executed} tests)\n` : 'No live tests executed.\n',
    )
  }
}
