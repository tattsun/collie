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
      // xfwd tells Collie who is really asking: `collie --no-auth` serves
      // only this machine, also when the dev server listens on the LAN.
      '/api': { target: 'http://127.0.0.1:7878', ws: true, xfwd: true },
    },
  },
})
