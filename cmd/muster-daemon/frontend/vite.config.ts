import react from '@vitejs/plugin-react'
import wails from '@wailsio/runtime/plugins/vite'
import { defineConfig } from 'vite'

export default defineConfig({
  base: './',
  server: {
    host: '127.0.0.1',
    port: 9245,
    strictPort: true,
  },
  plugins: [react(), wails('./src/bindings')],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
})
