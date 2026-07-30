import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const rootDir = path.dirname(fileURLToPath(import.meta.url))

// Production build lands in the Go embed path so breakwaterd can serve it.
// Placeholder dist/index.html is overwritten by this build (see make web).
// Backend-only: commit keeps a minimal dist/ so go build never breaks.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: path.resolve(rootDir, '../server/internal/web/dist'),
    emptyOutDir: true,
    // No sourcemaps in embed tree (keeps go:embed / git small).
    sourcemap: false,
  },
  server: {
    proxy: {
      '/api': {
        target: 'https://127.0.0.1:8443',
        changeOrigin: true,
        secure: false,
      },
      '/healthz': {
        target: 'https://127.0.0.1:8443',
        changeOrigin: true,
        secure: false,
      },
    },
  },
})
