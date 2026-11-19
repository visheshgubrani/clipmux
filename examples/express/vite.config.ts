import { defineConfig } from 'vite'

// Bundles the two elements into dist/client.js, which the server serves at
// /assets/client.js. Any bundler does; the elements need one (or an import map).
export default defineConfig({
  build: {
    target: 'es2022',
    outDir: 'dist',
    // hls.js and the player UI: one ~1 MB file, loaded once.
    chunkSizeWarningLimit: 1200,
    rollupOptions: {
      input: 'src/client.ts',
      output: { entryFileNames: 'client.js', chunkFileNames: '[name]-[hash].js' },
    },
  },
})
