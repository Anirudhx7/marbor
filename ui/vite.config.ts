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
  base: isPages ? '/marbor/demo/' : '/',
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
