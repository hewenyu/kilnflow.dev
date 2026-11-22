import { msg, type Text } from '../i18n/messages'
import { envelopeSchema, redirectSchema, type Envelope } from './envelope'

// 页面数据的请求：带请求头 X-Kiln-UI: 1，服务端返回页面数据或重定向（JSON）。

export type Fetched =
  | { kind: 'page'; env: Envelope }
  | { kind: 'redirect'; to: string }
  | { kind: 'error'; status: number; message: Text }

// 错误说明是消息（不是译好的文字）：显示时按当前语言翻译，切换语言后立即更新。
/** 错误说明最多显示的字符数。 */
const maxErrorText = 500

/** loginPath 是登录页的地址；loginURL 返回登录后回到 next 的登录页地址。 */
export const loginPath = '/login'
export const loginURL = (next: string) => `${loginPath}?next=${encodeURIComponent(next)}`

/** fetchPage 请求 url 的页面数据；body 不为空时用 POST 提交表单。 */
export async function fetchPage({
  url,
  body,
  signal,
}: {
  url: string
  body?: URLSearchParams
  signal?: AbortSignal
}): Promise<Fetched> {
  let res: Response
  try {
    res = await fetch(url, {
      method: body ? 'POST' : 'GET',
      headers: {
        'X-Kiln-UI': '1',
        Accept: 'application/json',
        ...(body ? { 'Content-Type': 'application/x-www-form-urlencoded' } : {}),
      },
      body,
      credentials: 'same-origin',
      redirect: 'follow',
      signal,
    })
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err
    return { kind: 'error', status: 0, message: msg('api.network') }
  }
  const type = res.headers.get('Content-Type') ?? ''
  if (!type.includes('application/json')) {
    // 只显示纯文本的说明（kiln-server 的 http.Error，原样显示）；反向代理的 HTML 错误页等不能把标记原样显示给用户
    const text = type.startsWith('text/plain') ? (await res.text().catch(() => '')).trim().slice(0, maxErrorText) : ''
    return { kind: 'error', status: res.status, message: text || msg('api.status', { status: res.status }) }
  }
  let json: unknown
  try {
    json = await res.json()
  } catch {
    return { kind: 'error', status: res.status, message: msg('api.unparsable') }
  }
  const redirect = redirectSchema.safeParse(json)
  if (redirect.success) return { kind: 'redirect', to: redirect.data.Redirect }
  const env = envelopeSchema.safeParse(json)
  if (!env.success) return { kind: 'error', status: res.status, message: msg('api.malformed') }
  return { kind: 'page', env: env.data }
}

/** embeddedPage 读取 index.html 中内嵌的页面数据（服务端直接渲染错误页等时）；没有时返回 null。 */
export function embeddedPage(): Envelope | null {
  const el = document.getElementById('kiln-page')
  if (!el?.textContent) return null
  el.remove()
  try {
    const env = envelopeSchema.safeParse(JSON.parse(el.textContent))
    return env.success ? env.data : null
  } catch {
    return null
  }
}

// 不在前端内跳转的站内地址：GitHub 登录、日志和产物下载（CI job 与 AI 任务）、静态文件。
const fullPage = /^\/(?:auth\/|cas\/|assets\/|favicon\.svg$|jobs\/\d+\/(?:log|download)|ai\/tasks\/\d+\/(?:log|download))/

/** inApp 表示 url 是前端内的页面（同源且不是下载等地址）；返回解析后的 URL，否则返回 null。 */
export function inApp(href: string): URL | null {
  let u: URL
  try {
    u = new URL(href, window.location.href)
  } catch {
    return null
  }
  if (u.origin !== window.location.origin || fullPage.test(u.pathname)) return null
  return u
}

/** isWebURL 表示 href 是 http 或 https 地址（可以整页跳转过去）。 */
export function isWebURL(href: string): boolean {
  try {
    const { protocol } = new URL(href, window.location.href)
    return protocol === 'http:' || protocol === 'https:'
  } catch {
    return false
  }
}
