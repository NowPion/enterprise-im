import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

const backendOrigin = process.env.IM_BACKEND_ORIGIN || 'http://127.0.0.1:8080'
const wsOrigin = backendOrigin.replace(/^http/, 'ws')
const vitePort = Number(process.env.IM_VITE_PORT || 5173)

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: Number.isFinite(vitePort) && vitePort > 0 ? vitePort : 5173,
    proxy: {
      '/api': {
        target: backendOrigin,
        changeOrigin: true,
      },
      '/uploads': {
        target: backendOrigin,
        changeOrigin: true,
      },
      '/ws': {
        target: wsOrigin,
        ws: true,
        changeOrigin: true,
      },
    },
  },
})
