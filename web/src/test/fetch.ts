import { vi } from 'vitest'

// 测试用的假 fetch：记录前端发出的请求，按 respond 返回响应（各页面和 app 的测试共用）。

export interface Call {
  url: string
  method: string
  body: URLSearchParams
}

/** json 返回 JSON 响应（页面数据或 {"Redirect": ...}）。 */
export const json = (v: unknown, status = 200) =>
  new Response(JSON.stringify(v), { status, headers: { 'Content-Type': 'application/json; charset=utf-8' } })

/** stubFetch 用假的 fetch 代替全局 fetch，返回记录请求的数组；respond 可以返回永不完成的 Promise 模拟请求进行中。 */
export function stubFetch(respond: (c: Call) => Response | Promise<Response>): Call[] {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const body = init?.body instanceof URLSearchParams ? init.body : new URLSearchParams()
      const c = { url: typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, method: init?.method ?? 'GET', body }
      calls.push(c)
      return Promise.resolve(respond(c))
    }),
  )
  return calls
}
