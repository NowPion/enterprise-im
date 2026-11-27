import { defineConfig, devices } from '@playwright/test'

const vitePort = process.env.IM_VITE_PORT || '5173'
const baseURL = process.env.PLAYWRIGHT_BASE_URL || `http://127.0.0.1:${vitePort}`

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  timeout: 60_000,
  expect: {
    timeout: 10_000,
  },
  reporter: [
    ['list'],
    ['html', { open: 'never', outputFolder: 'playwright-report' }],
  ],
  outputDir: 'test-results',
  use: {
    baseURL,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'off',
  },
  projects: [
    {
      name: 'social',
      testMatch: /social\/.*\.spec\.ts/,
      use: { ...devices['Desktop Chrome'] },
    },
    {
      name: 'chat',
      testMatch: /chat\/.*\.spec\.ts/,
      use: { ...devices['Desktop Chrome'] },
    },
    {
      name: 'reconnect',
      testMatch: /reconnect\/.*\.spec\.ts/,
      use: { ...devices['Desktop Chrome'] },
    },
    {
      name: 'admin',
      testMatch: /admin\/.*\.spec\.ts/,
      use: { ...devices['Desktop Chrome'] },
    },
    {
      name: 'admin-firefox',
      testMatch: /admin\/.*kick.*\.spec\.ts/,
      use: { ...devices['Desktop Firefox'] },
    },
  ],
})
