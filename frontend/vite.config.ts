import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

// Where `npm run dev` forwards /api. The default is a local `go run`, which
// has no Docker on a dev PC; point it at the deployed dashboard to work on the
// UI with real data, e.g.
//   API_TARGET=https://homelab.<tailnet>.ts.net npm run dev
// That's safe: the API is read-only, and the URL is only reachable on the
// tailnet.
const apiTarget = process.env.API_TARGET ?? 'http://localhost:8080'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    // In development, forward API calls to the Go backend so the browser
    // sees one origin, the same as in production where Go serves both.
    proxy: {
      '/api': {
        target: apiTarget,
        // Send the target's own hostname, not localhost:5173, so HTTPS and
        // `tailscale serve` accept the request.
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
  },
})
