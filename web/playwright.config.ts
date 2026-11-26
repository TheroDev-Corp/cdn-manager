import { defineConfig } from '@playwright/test'

const service = process.env.CDN_TEST_BASE_URL

export default defineConfig({
  testDir: './tests',
  use: {
    baseURL: service || 'http://127.0.0.1:5174',
    channel: process.env.PLAYWRIGHT_CHANNEL,
    actionTimeout: 15_000,
    navigationTimeout: 15_000,
  },
  webServer: service ? undefined : {
    command: 'bun run dev --host 127.0.0.1 --port 5174 --strictPort',
    url: 'http://127.0.0.1:5174',
  },
})
