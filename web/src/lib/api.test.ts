import { afterEach, describe, expect, it, vi } from 'vitest'
import { fetchPage, inApp, isWebURL, loginURL } from './api'

// 页面数据请求的错误说明、站外跳转的判断和不在前端内跳转的下载地址。

afterEach(() => {
  vi.unstubAllGlobals()
})

function reply(res: Response) {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(res)),
  )
}

describe('fetchPage', () => {
  it('服务端的纯文本错误原样作为说明', async () => {
    reply(new Response('服务器出错了，请稍后重试。\n', { status: 500, headers: { 'Content-Type': 'text/plain; charset=utf-8' } }))
    expect(await fetchPage({ url: '/runs' })).toEqual({ kind: 'error', status: 500, message: '服务器出错了，请稍后重试。' })
  })

  it('反向代理返回的 HTML 错误页不把标记显示给用户，只说明状态码', async () => {
    const html = '<html><head><title>502 Bad Gateway</title></head><body><center><h1>502 Bad Gateway</h1></center></body></html>'
    reply(new Response(html, { status: 502, headers: { 'Content-Type': 'text/html' } }))
    expect(await fetchPage({ url: '/runs' })).toEqual({ kind: 'error', status: 502, message: '服务器返回了 502，请稍后重试。' })
  })

  it('过长的纯文本说明截断', async () => {
    reply(new Response('x'.repeat(2000), { status: 500, headers: { 'Content-Type': 'text/plain' } }))
    const r = await fetchPage({ url: '/runs' })
    expect(r.kind === 'error' ? r.message.length : 0).toBe(500)
  })
})

describe('inApp', () => {
  it.each([
    ['/ai/tasks/123/log', null],
    ['/ai/tasks/123/log/download', null],
    ['/ai/tasks/123/log/download?attempt=31', null],
    ['/jobs/7/log', null],
    ['/jobs/7/download', null],
  ])('%s 整页请求（不经过前端路由）', (href, want) => {
    expect(inApp(href)).toBe(want)
  })

  it.each(['/ai/tasks/123', '/ai/tasks/123?attempt=31', '/ai/tasks/123/other', '/runs'])('%s 在前端内跳转', (href) => {
    expect(inApp(href)?.pathname).toBe(new URL(href, window.location.href).pathname)
  })
})

describe('isWebURL', () => {
  it.each([
    ['https://github.com/login/oauth/authorize?x=1', true],
    ['http://localhost:8080/runs', true],
    ['/runs', true],
    ['javascript:alert(1)', false],
    [' JavaScript:alert(1)', false],
    ['data:text/html,<script>alert(1)</script>', false],
  ])('%s → %s', (href, want) => {
    expect(isWebURL(href)).toBe(want)
  })
})

describe('loginURL', () => {
  it('把要回到的地址编码进 next', () => {
    expect(loginURL('/runs?state=failed')).toBe('/login?next=%2Fruns%3Fstate%3Dfailed')
  })
})
