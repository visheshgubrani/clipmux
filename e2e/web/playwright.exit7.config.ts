import { defineConfig, devices } from '@playwright/test'

// Launch exit check 7 (P6-T17, D112): grades a skill eval. A coding agent,
// with only the installed skills, added ClipMux to a copy of
// skills/evals/lms-starter (CLIPMUX_EVAL_APP); this suite builds and starts
// that app and checks that upload, the webhook and signed playback work and
// that no secret reaches the browser. It runs against the exit-8 fixture,
// which must be the instance the agent's `init` was approved on, so it is
// reused, not restarted: start it first (skills/evals/README.md).
const port = 8097

export default defineConfig({
  testDir: 'exit7',
  timeout: 900_000,
  workers: 1,
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    ...devices['Desktop Chrome'],
    channel: process.env.CLIPMUX_E2E_CHANNEL,
    baseURL: `http://127.0.0.1:${port}`,
    trace: 'retain-on-failure',
    screenshot: 'off',
    video: 'off',
  },
  webServer: {
    command: 'go test ../../internal/e2e -run TestExampleFixture -count=1 -timeout 60m -v',
    env: { CLIPMUX_E2E: '1', CLIPMUX_E2E_EXAMPLE_FIXTURE: '1', CLIPMUX_FIXTURE_PORT: String(port) },
    url: `http://127.0.0.1:${port}/config`,
    timeout: 600_000,
    reuseExistingServer: true,
    stdout: 'pipe',
  },
})
