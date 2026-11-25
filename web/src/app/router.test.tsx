import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import errorFixture from '../../dev/fixtures/error.json'
import tokenFixture from '../../dev/fixtures/login.token.json'
import type { Envelope } from '../lib/envelope'
import { deferred, envelope, renderPage, withData } from '../test/render'
import { translate } from '../i18n/messages'
import { RouterProvider, useRouter } from './router'

// 前端内跳转、表单提交和定时更新：用假的 fetch 代替 kiln-server。

interface Call {
  url: string
  method: string
  headers: Record<string, string>
  body: string
}

const json = (v: unknown, status = 200) =>
  new Response(JSON.stringify(v), { status, headers: { 'Content-Type': 'application/json; charset=utf-8' } })

/** errorPage 返回一个错误页的页面数据，标题为 heading（用来区分页面）。 */
function errorPage(heading: string, links: { Label: string; URL: string; Primary: boolean }[] = []): Envelope {
  return withData(envelope(errorFixture, { Title: heading }), { Heading: heading, Links: links })
}

let calls: Call[] = []
let respond: (c: Call) => Response | Promise<Response>

beforeEach(() => {
  calls = []
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const headers: Record<string, string> = {}
      new Headers(init?.headers).forEach((v, k) => {
        headers[k] = v
      })
      const body = init?.body instanceof URLSearchParams ? init.body.toString() : ''
      const c = { url: typeof input === 'string' ? input : input instanceof Request ? input.url : input.href, method: init?.method ?? 'GET', headers, body }
      calls.push(c)
      return Promise.resolve(respond(c))
    }),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('前端内跳转', () => {
  it('点击站内链接时带 X-Kiln-UI 取页面数据，显示新页面并记入 history', async () => {
    respond = () => json(errorPage('运行列表'))
    renderPage(errorPage('出错了', [{ Label: '返回运行列表', URL: '/runs?state=failed', Primary: false }]), '/runs/9')
    await userEvent.click(screen.getByRole('link', { name: '返回运行列表' }))
    expect(await screen.findByRole('heading', { level: 1, name: '运行列表' })).toBeInTheDocument()
    expect(calls).toHaveLength(1)
    expect(calls[0]?.url).toBe('/runs?state=failed')
    expect(calls[0]?.headers['x-kiln-ui']).toBe('1')
    expect(window.location.pathname + window.location.search).toBe('/runs?state=failed')
    expect(document.title).toBe('运行列表 · Kiln')
  })

  it('首次打开时内嵌页面带 flash：显示一次，并从地址栏清理（刷新不会重复弹出）', () => {
    const flash = { id: 'account.unbound' }
    renderPage(envelope(errorFixture, { Flash: flash }), '/runs?flash=' + encodeURIComponent(JSON.stringify(flash)))
    expect(screen.getByText('已解除绑定，admin token 登录已恢复。')).toBeInTheDocument()
    expect(window.location.pathname).toBe('/runs')
    expect(window.location.search).toBe('')
  })

  it('服务端返回重定向时跳到重定向的地址；flash 显示一次并从地址中去掉', async () => {
    const flash = { id: 'account.bound', params: { login: 'octocat' } }
    const to = '/runs/8?flash=' + encodeURIComponent(JSON.stringify(flash))
    respond = (c) => (c.url === '/runs/7' ? json({ Redirect: to }) : json({ ...errorPage('run 8'), Flash: flash }))
    renderPage(errorPage('出错了', [{ Label: '去 run 7', URL: '/runs/7', Primary: true }]), '/')
    await userEvent.click(screen.getByRole('link', { name: '去 run 7' }))
    expect(await screen.findByRole('heading', { level: 1, name: 'run 8' })).toBeInTheDocument()
    expect(calls.map((c) => c.url)).toEqual(['/runs/7', to])
    expect(screen.getByText('已绑定 GitHub 账号 octocat，现在以 octocat 的身份登录。')).toBeInTheDocument()
    expect(window.location.search).toBe('')
  })

  it('地址带 #hash 时，新页面渲染后滚动到新页面中的那个元素', async () => {
    const scrolled: (string | null)[] = []
    Element.prototype.scrollIntoView = function (this: Element) {
      scrolled.push(this.textContent)
    }
    try {
      respond = () => json(errorPage('新页面'))
      // 当前页面中也有 id 为 err-h 的标题：要滚动到的是新页面中的那个
      renderPage(errorPage('旧页面', [{ Label: '去新页面', URL: '/runners#err-h', Primary: false }]), '/')
      await userEvent.click(screen.getByRole('link', { name: '去新页面' }))
      expect(await screen.findByRole('heading', { level: 1, name: '新页面' })).toBeInTheDocument()
      expect(scrolled).toEqual(['新页面'])
      expect(window.location.hash).toBe('#err-h')
    } finally {
      Reflect.deleteProperty(Element.prototype, 'scrollIntoView')
    }
  })

  /**
   * clickAndWatch 点击一个链接，返回它是否被前端路由截获。监听器加在 window 的冒泡阶段：路由的监听器在 document 的
   * 冒泡阶段（先于 window），所以这里看到的 defaultPrevented 是路由真正的决定；加在链接上会在路由之前执行，看到的
   * 永远是 false。jsdom 不支持整页跳转，记录之后阻止默认行为。
   */
  async function clickAndWatch(link: HTMLElement): Promise<boolean> {
    const seen: boolean[] = []
    const watch = (e: MouseEvent) => {
      seen.push(e.defaultPrevented)
      e.preventDefault()
    }
    window.addEventListener('click', watch)
    try {
      await userEvent.click(link)
    } finally {
      window.removeEventListener('click', watch)
    }
    const [prevented] = seen
    if (prevented === undefined) throw new Error('点击事件没有到达 window')
    return prevented
  }

  it('退出、下载等地址整页跳转，不在前端内取数据', async () => {
    respond = () => json(errorPage('x'))
    renderPage(errorPage('出错了', [{ Label: '下载日志', URL: '/jobs/3/log/download', Primary: false }]), '/')
    expect(await clickAndWatch(screen.getByRole('link', { name: '下载日志' }))).toBe(false)
    expect(calls).toHaveLength(0)
  })

  it('AI 任务的日志下载地址整页跳转，普通页面链接仍在前端内跳转', async () => {
    respond = () => json(errorPage('任务 123'))
    renderPage(
      errorPage('出错了', [
        { Label: '下载完整日志', URL: '/ai/tasks/123/log/download', Primary: false },
        { Label: '回到任务', URL: '/ai/tasks/123', Primary: false },
      ]),
      '/ai/tasks/123',
    )
    expect(await clickAndWatch(screen.getByRole('link', { name: '下载完整日志' }))).toBe(false)
    expect(calls).toHaveLength(0)
    // 豁免只针对 log/download：同一任务的普通页面照常被前端路由截获
    expect(await clickAndWatch(screen.getByRole('link', { name: '回到任务' }))).toBe(true)
    expect(await screen.findByRole('heading', { level: 1, name: '任务 123' })).toBeInTheDocument()
    expect(calls.map((c) => c.url)).toEqual(['/ai/tasks/123'])
  })

  it('首次打开取数据失败时显示原因，不整页重新加载（否则会无限刷新）', async () => {
    respond = () => json({ unexpected: true })
    renderPage(null, '/runs')
    expect(await screen.findByText('服务器返回的页面数据格式不对，请刷新页面重试。')).toBeInTheDocument()
    expect(calls.map((c) => c.url)).toEqual(['/runs'])
  })

  it('重定向到 javascript: 等非 http(s) 地址时不跳转，说明原因', async () => {
    respond = () => json({ Redirect: 'javascript:alert(1)' })
    renderPage(errorPage('当前页', [{ Label: '去别处', URL: '/runs', Primary: false }]), '/')
    await userEvent.click(screen.getByRole('link', { name: '去别处' }))
    expect(await screen.findByText('不支持跳转到这个地址：javascript:alert(1)')).toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 1, name: '当前页' })).toBeInTheDocument()
    expect(window.location.pathname).toBe('/')
  })

  it('跳转失败时保留当前页面，并在顶部说明原因', async () => {
    respond = () => new Response('服务器出错了，请稍后重试。', { status: 500, headers: { 'Content-Type': 'text/plain' } })
    renderPage(errorPage('当前页', [{ Label: '去别处', URL: '/runs', Primary: false }]), '/')
    await userEvent.click(screen.getByRole('link', { name: '去别处' }))
    expect(await screen.findByText('服务器出错了，请稍后重试。')).toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 1, name: '当前页' })).toBeInTheDocument()
  })
})

describe('表单提交', () => {
  it('POST 时带上 csrf 和表单字段；校验失败返回的页面在当前地址显示', async () => {
    const failed = envelope(tokenFixture)
    respond = () => json(failed, 422)
    const start = withData(envelope(tokenFixture, { CSRF: 'tok' }), { Form: { Values: {}, Errors: {}, Failed: false } })
    renderPage(start, '/login')
    await userEvent.type(screen.getByLabelText(/admin token/, { selector: 'input' }), 'secret-1')
    await userEvent.click(screen.getByRole('button', { name: '用 admin token 登录' }))
    await waitFor(() => expect(calls).toHaveLength(1))
    const c = calls[0]
    expect(c?.method).toBe('POST')
    expect(c?.url).toBe('/login')
    expect(c?.headers['content-type']).toBe('application/x-www-form-urlencoded')
    const body = new URLSearchParams(c?.body)
    expect(body.get('token')).toBe('secret-1')
    expect(body.get('next')).toBe('/')
    expect(body.get('csrf')).toBe('tok')
    expect(await screen.findByText(/admin token 不正确/)).toBeInTheDocument()
    expect(window.location.pathname).toBe('/login')
  })
})

describe('过期的响应', () => {
  // 提交表单后跳转到别的页面（或者又提交了一次），之前那次提交的响应不能再改动界面

  let router: ReturnType<typeof useRouter> | null = null
  function Probe() {
    router = useRouter()
    return <p data-testid="view">{router.view ? `${router.view.url}|${translate('zh-CN', router.view.env.Title)}` : ''}</p>
  }
  const shown = () => screen.getByTestId('view').textContent
  const r = () => {
    if (!router) throw new Error('RouterProvider 还没有渲染')
    return router
  }
  function start(url: string) {
    window.history.replaceState(null, '', url)
    render(
      <RouterProvider initial={errorPage('启动构建')}>
        <Probe />
      </RouterProvider>,
    )
  }
  /** pending 让每个请求都返回一个由测试决定何时完成的响应；get 按方法、地址和请求体中的片段找到那个请求。 */
  function pending() {
    const waits: { c: Call; resolve: (v: Response) => void }[] = []
    respond = (c) => {
      const d = deferred<Response>()
      waits.push({ c, resolve: d.resolve })
      return d.promise
    }
    const find = (key: string, part = '') => {
      const [method, url] = key.split(' ')
      return waits.find((w) => w.c.method === method && w.c.url === url && w.c.body.includes(part))
    }
    return {
      has: (key: string) => find(key) !== undefined,
      get: (key: string, part = '') => {
        const w = find(key, part)
        if (!w) throw new Error(`没有请求 ${key} ${part}`)
        return w
      },
    }
  }

  it('表单地址不在站内时不发请求，说明原因（提交的 Promise 不会以异常结束）', async () => {
    start('/runs/new')
    await act(async () => {
      await r().submit({ action: 'javascript:void(0)', body: new URLSearchParams() })
    })
    expect(calls).toHaveLength(0)
    expect(r().failure).toEqual({ id: 'router.unsupportedAction', params: { url: 'javascript:void(0)' } })
    expect(shown()).toBe('/runs/new|启动构建')
  })

  it.each([
    ['页面', () => json(errorPage('workflow 表单'), 422)],
    ['重定向', () => json({ Redirect: '/runs/8' })],
    ['错误', () => new Response('服务器出错了', { status: 500, headers: { 'Content-Type': 'text/plain' } })],
  ])('提交后跳转到别的页面，旧提交返回%s时不改动当前页面', async (_kind, reply) => {
    const waits = pending()
    start('/runs/new')
    let submitted: Promise<void> = Promise.resolve()
    act(() => {
      submitted = r().submit({ action: '/runs/new', body: new URLSearchParams({ repo: 'o/r' }) })
    })
    act(() => {
      void r().navigate('/runners')
    })
    await waitFor(() => expect(waits.has('GET /runners')).toBe(true))
    await act(async () => {
      waits.get('GET /runners').resolve(json(errorPage('Runner 列表')))
      await Promise.resolve()
    })
    await waitFor(() => expect(shown()).toBe('/runners|Runner 列表'))
    await act(async () => {
      waits.get('POST /runs/new', 'repo=o%2Fr').resolve(reply())
      await submitted
    })
    expect(shown()).toBe('/runners|Runner 列表')
    expect(window.location.pathname).toBe('/runners')
    expect(r().failure).toBeNull()
    expect(r().loading).toBe(false)
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual(['POST /runs/new', 'GET /runners'])
  })

  it('连续提交乱序返回时只显示最后一次提交的结果', async () => {
    const waits = pending()
    start('/runs/new')
    const done: Promise<void>[] = []
    act(() => {
      done.push(r().submit({ action: '/runs/new', body: new URLSearchParams({ n: '1' }) }))
      done.push(r().submit({ action: '/runs/new', body: new URLSearchParams({ n: '2' }) }))
    })
    await act(async () => {
      waits.get('POST /runs/new', 'n=2').resolve(json(errorPage('第二次提交'), 422))
      await done[1]
    })
    expect(shown()).toBe('/runs/new|第二次提交')
    await act(async () => {
      waits.get('POST /runs/new', 'n=1').resolve(json(errorPage('第一次提交'), 422))
      await done[0]
    })
    expect(shown()).toBe('/runs/new|第二次提交')
    expect(r().loading).toBe(false)
  })

  it('跳转期间提交了表单：跳转的响应不再覆盖提交的结果', async () => {
    const waits = pending()
    start('/runs/new')
    act(() => {
      void r().navigate('/runners')
    })
    let submitted: Promise<void> = Promise.resolve()
    act(() => {
      submitted = r().submit({ action: '/runs/new', body: new URLSearchParams({ n: '1' }) })
    })
    await act(async () => {
      waits.get('POST /runs/new', 'n=1').resolve(json(errorPage('提交结果'), 422))
      await submitted
    })
    await act(async () => {
      waits.get('GET /runners').resolve(json(errorPage('Runner 列表')))
      await new Promise((ok) => setTimeout(ok, 0))
    })
    expect(shown()).toBe('/runs/new|提交结果')
    expect(window.location.pathname).toBe('/runs/new')
  })

  it.each([
    ['页面', () => json(errorPage('过期页面'))],
    ['登录重定向', () => json({ Redirect: '/login' })],
  ])('刷新期间提交表单，旧刷新返回%s时不改动页面或登录状态', async (_kind, reply) => {
    const waits = pending()
    start('/runs/new')
    let refreshed: Promise<boolean> = Promise.resolve(true)
    let submitted: Promise<void> = Promise.resolve()
    act(() => { refreshed = r().refresh() })
    act(() => {
      submitted = r().submit({ action: '/runs/new', body: new URLSearchParams() })
    })
    await act(async () => {
      waits.get('GET /runs/new').resolve(reply())
      await refreshed
    })
    expect(shown()).toBe('/runs/new|启动构建')
    expect(r().expired).toBe(false)
    expect(r().loading).toBe(true)
    await act(async () => {
      waits.get('POST /runs/new').resolve(json(errorPage('提交结果'), 422))
      await submitted
    })
    expect(shown()).toBe('/runs/new|提交结果')
  })

  it('提交进行中不启动刷新请求', async () => {
    const waits = pending()
    const pendingResponse = respond
    respond = (c) => c.method === 'GET' ? json(errorPage('不应刷新的页面')) : pendingResponse(c)
    start('/runs/new')
    let submitted: Promise<void> = Promise.resolve()
    act(() => {
      submitted = r().submit({ action: '/runs/new', body: new URLSearchParams() })
    })
    await act(async () => { await r().refresh() })
    expect(calls.map((c) => c.method)).toEqual(['POST'])
    await act(async () => {
      waits.get('POST /runs/new').resolve(json(errorPage('提交结果'), 422))
      await submitted
    })
  })

  it('并发刷新乱序返回时只采用最后发起的刷新', async () => {
    const replies = [deferred<Response>(), deferred<Response>()]
    let next = 0
    respond = () => {
      const reply = replies[next++]
      if (!reply) throw new Error('多余的刷新请求')
      return reply.promise
    }
    start('/runs/new')
    const done: Promise<boolean>[] = []
    act(() => {
      done.push(r().refresh(), r().refresh())
    })
    await act(async () => {
      replies[1]?.resolve(json(errorPage('最新页面')))
      await done[1]
    })
    await act(async () => {
      replies[0]?.resolve(json(errorPage('过期页面')))
      await done[0]
    })
    expect(shown()).toBe('/runs/new|最新页面')
  })

  it('跳转完成后，旧页面未返回的提交不阻止新页面刷新', async () => {
    const submission = deferred<Response>()
    let reads = 0
    respond = (c) => c.method === 'POST' ? submission.promise : json(errorPage(`Runner ${++reads}`))
    start('/runs/new')
    let submitted: Promise<void> = Promise.resolve()
    act(() => {
      submitted = r().submit({ action: '/runs/new', body: new URLSearchParams() })
    })
    await act(async () => { await r().navigate('/runners') })
    await act(async () => { await r().refresh() })
    expect(shown()).toBe('/runners|Runner 2')
    await act(async () => {
      submission.resolve(json(errorPage('过期提交'), 422))
      await submitted
    })
    expect(shown()).toBe('/runners|Runner 2')
  })
})

describe('定时更新', () => {
  it('页面声明需要更新时每 5 秒重新取当前地址；登录过期时停止并提示重新登录', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const { useLive } = await import('./live')
    const { definePage } = await import('./definePage')
    const { pages } = await import('../pages')
    const { z } = await import('zod')
    // 临时把错误页换成一个声明需要更新的页面
    const original = pages.error
    function LivePage({ data }: { data: { Heading: string } }) {
      useLive(true)
      return <h1>{data.Heading}</h1>
    }
    pages.error = definePage(z.object({ Heading: z.string() }), LivePage)
    try {
      let n = 0
      respond = () => {
        n++
        return n === 1 ? json(errorPage('第二次')) : json({ Redirect: '/login?next=%2Fruns' })
      }
      renderPage(errorPage('第一次'), '/runs')
      await act(() => vi.advanceTimersByTimeAsync(5000))
      expect(await screen.findByRole('heading', { name: '第二次' })).toBeInTheDocument()
      expect(calls[0]?.url).toBe('/runs')
      await act(() => vi.advanceTimersByTimeAsync(5000))
      expect(await screen.findByText(/登录已过期，自动更新已停止。/)).toBeInTheDocument()
      expect(screen.getByRole('link', { name: '重新登录' })).toHaveAttribute('href', '/login?next=%2Fruns')
      await act(() => vi.advanceTimersByTimeAsync(20000))
      expect(calls).toHaveLength(2)
    } finally {
      pages.error = original
    }
  })
})
