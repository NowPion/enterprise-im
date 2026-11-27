import { defineConfig, devices } from '@playwright/test'

const baseURL = process.env.IM_E2E_BASE_URL || 'http://127.0.0.1:5175'

export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  outputDir: '/tmp/pw-stage9-chat-results',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 90_000,
  expect: {
    timeout: 15_000,
  },
  reporter: [['list']],
  use: {
    baseURL,
    trace: 'off',
    video: 'off',
    screenshot: 'only-on-failure',
    actionTimeout: 15_000,
    navigationTimeout: 20_000,
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
    {
      name: 'firefox',
      testMatch: 'core-single-upload-recall.spec.ts',
      use: { ...devices['Desktop Firefox'] },
    },
  ],
})
