import { resolve } from 'node:path'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// Builds into internal/web/dist, which the Go binary embeds: the dashboard
// (index.html) and the share page (share.html, D104), which shares none of
// the dashboard's bundle. `pnpm dev` proxies the API to a local
// `clipmux serve`.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { '@': resolve(import.meta.dirname, 'src') } },
  build: {
    outDir: resolve(import.meta.dirname, '../internal/web/dist'),
    emptyOutDir: true,
    target: 'es2022',
    rollupOptions: {
      input: {
        main: resolve(import.meta.dirname, 'index.html'),
        share: resolve(import.meta.dirname, 'share.html'),
      },
    },
  },
  server: {
    proxy: Object.fromEntries(['/v1', '/v', '/p', '/_storage'].map((p) => [p, 'http://localhost:8080'])),
  },
})
