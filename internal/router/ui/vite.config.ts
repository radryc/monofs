import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

// https://vite.dev/config/
export default defineConfig({
  // Serve from a sub-path (e.g. /monofs/ behind a path-routing ALB) when the
  // deployment sets VITE_BASE. Defaults to root for local/native deployments.
  base: process.env.VITE_BASE || '/',
  plugins: [tailwindcss(), vue()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
})
