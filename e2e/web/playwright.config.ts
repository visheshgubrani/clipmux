import { defineConfig, devices } from '@playwright/test'

// The browser exit suites. TestPlayerFixture boots the real clipmux, encodes
// a video and serves the built pages; it needs TEST_DATABASE_URL and the
// media tools, like the Go exit tests.
export default defineConfig({
  testDir: 'tests',
  timeout: 180_000,
  workers: 1,
  reporter: process.env.CI ? 'github' : 'list',
  use: { baseURL: 'http://127.0.0.1:8099', trace: 'retain-on-failure' },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'], channel: process.env.CLIPMUX_E2E_CHANNEL } },
  ],
  webServer: {
    command: 'go test ../../internal/e2e -run TestPlayerFixture -count=1 -timeout 60m -v',
    env: { CLIPMUX_E2E: '1', CLIPMUX_E2E_FIXTURE: '1' },
    url: 'http://127.0.0.1:8099/config',
    timeout: 600_000,
    reuseExistingServer: !process.env.CI,
    stdout: 'pipe',
  },
})
