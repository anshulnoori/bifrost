import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './features/session',
  testMatch: 'oidc.spec.ts',
  timeout: 30000,
  use: {
    ...devices['Desktop Chrome'],
    viewport: { width: 1280, height: 900 }, deviceScaleFactor: 2,
    launchOptions: { executablePath: process.env.CHROMIUM_EXECUTABLE },
    baseURL: process.env.BASE_URL || 'http://localhost:8080',
  },
  reporter: 'list',
})
