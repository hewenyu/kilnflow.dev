import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'
import { preview } from './dev/preview.ts'

// 开发时 vite 只提供前端，页面数据和表单提交都转发给本机的 kiln-server（KILN_SERVER，默认 http://localhost:8080）。
const server = process.env.KILN_SERVER ?? 'http://localhost:8080'

// 浏览器打开页面地址（不带 X-Kiln-UI）时由 vite 返回 index.html；其余请求转发给 kiln-server。
const passToServer = {
  target: server,
  changeOrigin: false,
  bypass(req: { headers: Record<string, string | string[] | undefined>; method?: string }) {
    const accept = req.headers.accept
    const wantsHTML = typeof accept === 'string' && accept.includes('text/html')
    if (req.method === 'GET' && wantsHTML && req.headers['x-kiln-ui'] !== '1') return '/index.html'
    return undefined
  },
}

// KILN_PREVIEW=1 时用 dev/fixtures/ 中的页面数据预览，不连 kiln-server（见 dev/preview.ts）
const previewMode = process.env.KILN_PREVIEW === '1'

export default defineConfig({
  plugins: [react(), ...(previewMode ? [preview(new URL('./dev/fixtures', import.meta.url).pathname)] : [])],
  build: {
    outDir: '../internal/server/web/ui/dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    // 只有一个入口、内嵌在 kiln-server 中，按内容摘要长期缓存；Primer 本身约 800 kB，不拆分
    chunkSizeWarningLimit: 1200,
  },
  server: {
    proxy: previewMode ? {} : {
      '^/(?!src/|@|node_modules/|favicon\\.svg).*': passToServer,
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    // 固定时区：时间格式化的用例不随运行测试的机器变化（Asia/Shanghai 不是 UTC，能发现误用 UTC 的代码）
    env: { TZ: 'Asia/Shanghai' },
    css: { modules: { classNameStrategy: 'non-scoped' } },
    // Primer React 的模块直接 import CSS，需要由 vite 处理
    server: { deps: { inline: [/@primer\/react/] } },
  },
})
