import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The dev server proxies the API and the WebSocket to mirrord so that the
// browser sees a single origin. That matters beyond convenience: it means the
// production CSP (`connect-src 'self'`) is exercised in development too,
// rather than being discovered to be wrong at deploy time.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://127.0.0.1:8080', changeOrigin: true },
      '/ws': { target: 'ws://127.0.0.1:8080', ws: true },
      '/metrics': { target: 'http://127.0.0.1:8080' },
    },
  },
  build: {
    outDir: 'dist',
    // The vehicle decoder and the renderer are the only hot paths; everything
    // else is small enough that a single chunk beats the request overhead of
    // splitting it.
    chunkSizeWarningLimit: 900,
  },
})
