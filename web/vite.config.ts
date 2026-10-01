import path from 'node:path'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(import.meta.dirname, './src'),
    },
  },
  server: {
    // 开发环境 API 请求转发到 Go 后端；
    // 多 worktree 并行时后端端口会撞，用 ASSAY_API_PROXY 覆盖
    proxy: {
      '/api': process.env.ASSAY_API_PROXY ?? 'http://localhost:8080',
    },
  },
})
