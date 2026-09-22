import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './features/headroom',
  timeout: 30000,
  use: { ...devices['Desktop Chrome'], baseURL: process.env.BASE_URL || 'http://localhost:3000', launchOptions: { executablePath: process.env.CHROMIUM_PATH } },
  reporter: 'list',
})
