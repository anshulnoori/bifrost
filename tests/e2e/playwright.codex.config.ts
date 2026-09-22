import { defineConfig, devices } from '@playwright/test'

// Isolated, credential-free UI contract tests. No global live-provider/MCP setup.
export default defineConfig({
  testDir: './features/providers',
  testMatch: 'codex.spec.ts',
  timeout: 30000,
  use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 1000 }, deviceScaleFactor: 2, channel: 'chromium', launchOptions: { executablePath: process.env.CHROMIUM_EXECUTABLE }, baseURL: process.env.BASE_URL || 'http://localhost:8080' },
  reporter: 'list',
})
