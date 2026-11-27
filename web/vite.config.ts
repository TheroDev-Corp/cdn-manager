import { defineConfig } from 'vite'

export default defineConfig({
  server: {
    proxy: Object.fromEntries(['/api', '/auth', '/mcp'].map(path => [path, 'http://localhost:8080'])),
  },
})
