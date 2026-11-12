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
        native: resolve(import.meta.dirname, 'native.html'),
        session: resolve(import.meta.dirname, 'session.html'),
        uploaderElement: resolve(import.meta.dirname, 'uploader-element.html'),
      },
    },
  },
})
