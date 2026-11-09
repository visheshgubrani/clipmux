import { defineConfig, devices } from '@playwright/test'

const live = Boolean(process.env.CLIPMUX_E2E_LIVE_CONFIG)

// The browser exit suites. TestPlayerFixture boots the real clipmux, encodes
// a video and serves the built pages; it needs TEST_DATABASE_URL and the
// media tools, like the Go exit tests.
export default defineConfig({
  testDir: 'tests',
  testMatch: live ? ['**/playback.spec.ts', '**/uploader.spec.ts', '**/cloudflare.spec.ts'] : '**/*.spec.ts',
  testIgnore: live ? [] : '**/cloudflare.spec.ts',
  timeout: 180_000,
  workers: 1,
  reporter: live ? './live-reporter.ts' : process.env.CI ? 'github' : 'list',
  use: {
    baseURL: 'http://127.0.0.1:8099',
    trace: live ? 'off' : 'retain-on-failure',
    screenshot: 'off',
    video: 'off',
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'], channel: process.env.CLIPMUX_E2E_CHANNEL } },
  ],
  webServer: {
    command: 'go test ../../internal/e2e -run TestPlayerFixture -count=1 -timeout 60m -v',
    env: { CLIPMUX_E2E: '1', CLIPMUX_E2E_FIXTURE: '1' },
    url: 'http://127.0.0.1:8099/config',
    timeout: 600_000,
    reuseExistingServer: !live && !process.env.CI,
    stdout: 'pipe',
  },
})
