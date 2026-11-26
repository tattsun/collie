import { defineConfig } from 'vite'

export default defineConfig({
  oxc: {
    jsx: { runtime: 'automatic', importSource: 'preact' },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2022',
    cssCodeSplit: true,
    reportCompressedSize: true,
    chunkSizeWarningLimit: 600,
  },
  server: {
    proxy: {
      '/api': { target: 'http://127.0.0.1:7878', ws: true },
    },
  },
})
