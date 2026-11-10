import { defineConfig, devices } from '@playwright/test'

const live = Boolean(process.env.CLIPMUX_E2E_LIVE_CONFIG)

// The browser exit suites. TestPlayerFixture boots the real clipmux, encodes
// a video and serves the built pages; it needs TEST_DATABASE_URL and the
// media tools, like the Go exit tests.
//
// Every suite runs twice (P6 exit check 6): in desktop Chromium and at a
// 375 px phone viewport. Each project gets a fixture of its own, so suites
// that change their install (TOTP, revoking a PC, deleting a track) start
// from the same state in both.
const fixture = (port: number) => ({
  command: 'go test ../../internal/e2e -run TestPlayerFixture -count=1 -timeout 60m -v',
  env: { CLIPMUX_E2E: '1', CLIPMUX_E2E_FIXTURE: '1', CLIPMUX_FIXTURE_PORT: String(port) },
  url: `http://127.0.0.1:${port}/config`,
  timeout: 600_000,
  reuseExistingServer: !live && !process.env.CI,
  stdout: 'pipe' as const,
})

const desktop = { ...devices['Desktop Chrome'], channel: process.env.CLIPMUX_E2E_CHANNEL }

export default defineConfig({
  testDir: 'tests',
  testMatch: live ? ['**/playback.spec.ts', '**/uploader.spec.ts', '**/cloudflare.spec.ts'] : '**/*.spec.ts',
  testIgnore: live ? [] : '**/cloudflare.spec.ts',
  timeout: 180_000,
  workers: 1,
  reporter: live ? './live-reporter.ts' : process.env.CI ? 'github' : 'list',
  use: {
    trace: live ? 'off' : 'retain-on-failure',
    screenshot: 'off',
    video: 'off',
  },
  projects: [
    { name: 'chromium', use: { ...desktop, baseURL: 'http://127.0.0.1:8099' } },
    // The phone pass is the dashboard's; the player pages already run on
    // phones in the Safari and ExoPlayer suites.
    ...(live
      ? []
      : [
          {
            name: 'phone',
            testIgnore: ['**/playback.spec.ts', '**/uploader.spec.ts', '**/cloudflare.spec.ts'],
            use: {
              ...desktop,
              viewport: { width: 375, height: 812 },
              isMobile: false,
              hasTouch: true,
              baseURL: 'http://127.0.0.1:8098',
            },
          },
        ]),
  ],
  webServer: live ? [fixture(8099)] : [fixture(8099), fixture(8098)],
})
