import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  build: { emptyOutDir: true, sourcemap: false },
  test: { environment: 'jsdom', setupFiles: ['./tests/setup.ts'], css: true, exclude: ['e2e/**', 'system/**', 'node_modules/**'] },
})
