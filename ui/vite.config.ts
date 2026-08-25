import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { readFileSync } from 'fs'
import { resolve } from 'path'

const pkg = JSON.parse(readFileSync(resolve(__dirname, 'package.json'), 'utf-8'))

const isPages = process.env.VITE_FORCE_DEMO === 'true';
const appVersion = process.env.VITE_APP_VERSION || pkg.version;

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  // Demo builds use a RELATIVE base ('./') so one artifact works under any
  // mount point - /marbor/demo/, /marbor1/demo/, wherever this repo's Pages
  // site gets served from. A hardcoded absolute base made every asset
  // request 404 when a fork deployed under a different first path segment
  // (the white-screen demo bug of 2026-08-25): the HTML loaded, but its
  // script/link tags pointed at the other deployment's asset URLs.
  base: isPages ? './' : '/',
  define: {
    __APP_VERSION__: JSON.stringify(appVersion),
  },
  server: {
    proxy: {
      '/admin': 'http://localhost:8080',
      '/login': 'http://localhost:8080',
    },
  },
  build: {
    outDir: isPages ? 'dist' : '../internal/admin/web/dist',
    emptyOutDir: true,
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (id.includes('node_modules')) {
            if (id.includes('react') || id.includes('react-dom') || id.includes('react-router-dom')) {
              return 'vendor-react';
            }
            if (id.includes('recharts')) {
              return 'vendor-charts';
            }
          }
        },
      },
    },
  }
})
