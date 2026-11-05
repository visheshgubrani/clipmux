import { resolve } from 'node:path'
import { defineConfig } from 'vite'

// The exit suites' pages, served by TestPlayerFixture from dist/.
export default defineConfig({
  build: {
    target: 'es2022',
    rollupOptions: {
      input: {
        hlsjs: resolve(import.meta.dirname, 'hlsjs.html'),
        player: resolve(import.meta.dirname, 'player.html'),
        uploader: resolve(import.meta.dirname, 'uploader.html'),
      },
    },
  },
})
