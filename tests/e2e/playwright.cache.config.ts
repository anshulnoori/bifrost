import { defineConfig, devices } from '@playwright/test'

// Mock-only cache editor checks; no live-provider/global setup.
export default defineConfig({
  testDir: './features/config',
  testMatch: 'config.spec.ts',
  grep: /cache edits preserve authenticated scope/,
  timeout: 30000,
  use: { ...devices['Desktop Chrome'], baseURL: process.env.BASE_URL || 'http://localhost:3000',
    launchOptions: { executablePath: process.env.CHROMIUM_EXECUTABLE } },
  reporter: 'list',
})
