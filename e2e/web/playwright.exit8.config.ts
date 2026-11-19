import { defineConfig, devices } from '@playwright/test'

// Launch exit check 8 (P6-T28, D112): the four-step integration in
// examples/nextjs-lms, against the real binary. TestExampleFixture boots
// serve with an owner account and a 45 s clip; the suite runs the CLI and
// starts the example itself, once `init` has written its env file. It needs
// TEST_DATABASE_URL, the media tools and a built examples/nextjs-lms
// (`pnpm run test:exit8` builds it).
const port = 8097

export default defineConfig({
  testDir: 'exit8',
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
    reuseExistingServer: !process.env.CI,
    stdout: 'pipe',
  },
})
