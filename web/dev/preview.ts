import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import type { Plugin } from 'vite'

// 预览模式（KILN_PREVIEW=1 pnpm dev）：不连 kiln-server，用 dev/fixtures/ 中手写的页面数据显示各个页面，
// 用于调整样式和截图。地址与服务端相同；?fx=名称 选择同一页面的其他数据（<页面>.<名称>.json）。

const routes: [RegExp, string][] = [
  [/^\/$/, 'overview'],
  [/^\/login$/, 'login'],
  [/^\/error$/, 'error'],
  [/^\/runs$/, 'runs'],
  [/^\/runs\/new$/, 'run_new'],
  [/^\/runs\/\d+$/, 'run'],
  [/^\/jobs\/\d+$/, 'job'],
  [/^\/merge-queue$/, 'merge_queue'],
  [/^\/ai-dev$/, 'ai_dev'],
  [/^\/ai-dev\/\d+$/, 'ai_dev_item'],
  [/^\/runners$/, 'runners'],
  [/^\/runners\/(new|enroll\/\d+)$/, 'runner_new'],
  [/^\/runners\/cache\/[^/]+$/, 'runner_cache'],
  [/^\/runners\/detail\/[^/]+$/, 'runner'],
  [/^\/settings$/, 'settings'],
  [/^\/settings\/docker$/, 'settings_docker'],
  [/^\/settings\/npm$/, 'settings_npm'],
  [/^\/settings\/variables$/, 'settings_vars'],
  [/^\/settings\/scope$/, 'settings_scope'],
  [/^\/settings\/(variables|secrets)\/(new|edit)$/, 'settings_item'],
  [/^\/settings\/environments$/, 'settings_envs'],
  [/^\/settings\/environments\/new$/, 'settings_env_new'],
  [/^\/settings\/repos$/, 'settings_repos'],
  [/^\/settings\/ops$/, 'settings_ops'],
  [/^\/settings\/runner-upgrade$/, 'settings_upgrade'],
  [/^\/settings\/account$/, 'settings_account'],
  [/^\/settings\/members$/, 'settings_members'],
  [/^\/settings\/activity$/, 'settings_activity'],
]

export function preview(dir: string): Plugin {
  return {
    name: 'kiln-preview',
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const u = new URL(req.url ?? '/', 'http://preview')
        if (/^\/jobs\/\d+\/log$/.test(u.pathname)) {
          const file = join(dir, 'job.log')
          const text = existsSync(file) ? readFileSync(file, 'utf8') : ''
          const offset = Number(u.searchParams.get('offset') ?? '0')
          const body = Buffer.from(text).subarray(offset)
          res.setHeader('Content-Type', 'text/plain; charset=utf-8')
          res.setHeader('X-Next-Offset', String(offset + body.length))
          res.setHeader('X-Finished', 'true')
          res.setHeader('X-Job-State', 'failed')
          res.setHeader('X-Job-Terminal', 'true')
          // 请求的尝试就是当前尝试：预览不模拟切换到新的尝试
          const attempt = u.searchParams.get('attempt') ?? '31'
          res.setHeader('X-Attempt', attempt)
          res.setHeader('X-Current-Attempt', attempt)
          res.end(body)
          return
        }
        if (req.headers['x-kiln-ui'] !== '1') {
          next()
          return
        }
        res.setHeader('Content-Type', 'application/json; charset=utf-8')
        if (req.method === 'POST') {
          const back = new URL(req.headers.referer ?? '/', 'http://preview')
          res.end(JSON.stringify({ Redirect: back.pathname + '?flash=' + encodeURIComponent('（预览）已提交，没有实际执行。') }))
          return
        }
        const page = routes.find(([re]) => re.test(u.pathname))?.[1]
        const fx = u.searchParams.get('fx')
        const file = page && join(dir, fx ? `${page}.${fx}.json` : `${page}.json`)
        if (!file || !existsSync(file)) {
          res.statusCode = 404
          res.end(JSON.stringify({ Page: 'error', Title: '找不到这个页面', Nav: 'error', Status: 404, Session: null, CSRF: '', OAuth: false, Active: 0, Flash: '', Data: { Status: 404, Heading: '预览没有这个页面的数据', Reason: `dev/fixtures/ 中没有 ${page ?? u.pathname} 的数据。`, Ref: '', Links: [] } }))
          return
        }
        const env: unknown = JSON.parse(readFileSync(file, 'utf8'))
        res.end(JSON.stringify(env))
      })
    },
  }
}
