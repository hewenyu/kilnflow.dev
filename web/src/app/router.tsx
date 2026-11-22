import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { isEmpty, msg, type Text } from '../i18n/messages'
import { fetchPage, inApp, isWebURL, loginPath, type Fetched } from '../lib/api'
import type { Envelope } from '../lib/envelope'

// 前端内的跳转：地址和页面都由服务端决定（同一个地址带 X-Kiln-UI 请求头取页面数据），前端只负责
// history、取数据、提交表单和定时刷新，不维护自己的路由表。

/** View 是当前显示的页面。 */
export interface View {
  env: Envelope
  /** 页面地址（路径和查询参数）。 */
  url: string
  /** 每次跳转加 1；定时刷新不变（页面组件据此保留输入、展开状态等）。 */
  seq: number
  /** 表单提交返回的页面（例如校验失败）：地址没有变，不定时刷新，免得丢掉错误提示。 */
  fromPost: boolean
  /** 跳转后显示的提示（服务端重定向时带的 ?flash=，服务端校验后放在页面数据中）；null 表示没有。 */
  flash: Text | null
}

interface Router {
  view: View | null
  /** 跳转失败（网络错误、服务器错误）时的说明，显示在页面顶部；null 表示没有。存消息而不是译好的文字，切换语言后随之更新。 */
  failure: Text | null
  /** 正在跳转（显示顶部进度条）。 */
  loading: boolean
  /** 自动更新时发现登录已过期。 */
  expired: boolean
  navigate: (to: string, opts?: { replace?: boolean }) => Promise<void>
  /** submit 提交表单（POST），自动带上 csrf；成功后按服务端的重定向跳转。 */
  submit: (opts: { action: string; body: URLSearchParams }) => Promise<void>
  /** refresh 重新取当前页面的数据（不滚动、不改地址）；返回是否成功。 */
  refresh: () => Promise<boolean>
  dismissFlash: () => void
  dismissFailure: () => void
}

const RouterContext = createContext<Router | null>(null)

/** useRouter 返回前端的跳转和提交函数。 */
export function useRouter(): Router {
  const r = useContext(RouterContext)
  if (!r) throw new Error('useRouter 必须在 RouterProvider 内使用')
  return r
}

const here = () => window.location.pathname + window.location.search

/** withoutFlash 去掉地址中的 flash 参数（提示只显示一次，刷新页面不再出现）。 */
function withoutFlash(u: URL): string {
  if (!u.searchParams.has('flash')) return u.pathname + u.search
  u.searchParams.delete('flash')
  const q = u.searchParams.toString()
  return u.pathname + (q ? '?' + q : '')
}

const maxRedirects = 5

/** flashOf 返回页面数据中的提示（空串表示没有）。 */
function flashOf(env: Envelope): Text | null {
  return isEmpty(env.Flash) ? null : env.Flash
}

/** decodeHash 返回 #hash 中的元素 id（解码失败时原样使用）。 */
function decodeHash(hash: string): string {
  const id = hash.slice(1)
  try {
    return decodeURIComponent(id)
  } catch {
    return id
  }
}

export function RouterProvider({ initial, children }: { initial: Envelope | null; children: ReactNode }) {
  const [view, setView] = useState<View | null>(() => {
    if (!initial) return null
    const u = new URL(window.location.href)
    return { env: initial, url: withoutFlash(u), seq: 0, fromPost: false, flash: flashOf(initial) }
  })
  const [failure, setFailure] = useState<Text | null>(null)
  const [loading, setLoading] = useState(false)
  const [expired, setExpired] = useState(false)
  const inflight = useRef<AbortController | null>(null)
  const seq = useRef(0)
  // 每次发起跳转或表单提交加 1：响应到达时已经有更新的操作，就不再改动界面。
  // 只是不再显示旧操作的结果，服务端照常处理了那次提交
  const gen = useRef(0)
  const submitting = useRef<number | null>(null)
  const refreshGen = useRef(0)
  const viewRef = useRef(view)
  // 跳转后要滚动到的位置：地址中的 #hash（空串表示页面顶部）；null 表示不滚动（定时刷新、表单校验失败、前进后退）
  const pendingScroll = useRef<string | null>(null)
  useLayoutEffect(() => {
    viewRef.current = view
    // 新页面渲染完成后才能找到 #hash 指向的元素
    const hash = pendingScroll.current
    if (hash === null) return
    pendingScroll.current = null
    const target = hash ? document.getElementById(decodeHash(hash)) : null
    if (target) target.scrollIntoView()
    else window.scrollTo(0, 0)
  }, [view])

  const show = useCallback(
    (env: Envelope, url: URL, { push, fromPost }: { push: boolean | null; fromPost: boolean }) => {
      const path = withoutFlash(new URL(url.href))
      if (push !== null) {
        const target = path + url.hash
        if (push) window.history.pushState(null, '', target)
        else window.history.replaceState(null, '', target)
      }
      seq.current++
      pendingScroll.current = push !== null ? url.hash : null
      setView({ env, url: path, seq: seq.current, fromPost, flash: flashOf(env) })
      setFailure(null)
      setExpired(false)
    },
    [],
  )

  const load = useCallback(
    async (start: string, push: boolean | null): Promise<void> => {
      const mine = ++gen.current
      const stale = () => gen.current !== mine
      let to = start
      for (let depth = 0; ; depth++) {
        const u = inApp(to)
        if (!u) {
          // 站外地址（GitHub 授权等）整页跳转；只接受 http(s)，javascript: 等地址不执行
          if (isWebURL(to)) window.location.assign(to)
          else setFailure(msg('router.unsupportedTarget', { url: to }))
          return
        }
        inflight.current?.abort()
        const ctl = new AbortController()
        inflight.current = ctl
        setLoading(true)
        let r: Fetched
        try {
          r = await fetchPage({ url: u.pathname + u.search, signal: ctl.signal })
        } catch {
          return // 被新的跳转取消
        } finally {
          if (inflight.current === ctl) {
            inflight.current = null
            if (!stale()) setLoading(false)
          }
        }
        if (stale()) return // 期间又提交了表单
        switch (r.kind) {
          case 'page':
            show(r.env, u, { push, fromPost: false })
            return
          case 'redirect':
            if (depth >= maxRedirects) {
              setFailure(msg('router.tooManyRedirects'))
              return
            }
            to = r.to
            if (push === null) push = false
            continue
          case 'error':
            // 首次打开时也只显示原因，不整页重新加载：浏览器打开页面地址时服务端总是返回 index.html，
            // 重新加载后前端会再次取到同样的错误，陷入无限刷新（例如前后端版本不一致、反向代理返回 502）
            setFailure(r.message)
            return
          default: {
            const _exhaustive: never = r
            return _exhaustive
          }
        }
      }
    },
    [show],
  )

  const navigate = useCallback(
    (to: string, opts?: { replace?: boolean }) => load(to, !opts?.replace),
    [load],
  )

  const submit = useCallback(
    async ({ action, body }: { action: string; body: URLSearchParams }) => {
      const csrf = viewRef.current?.env.CSRF ?? ''
      if (csrf && !body.has('csrf')) body.set('csrf', csrf)
      const u = inApp(action)
      if (!u) {
        // 表单地址不在站内（或不是 http(s)）：说明原因，不让提交的 Promise 带着异常结束
        setFailure(msg('router.unsupportedAction', { url: action }))
        return
      }
      // 提交期间跳转到了别的页面或者又提交了一次：这次的响应（页面、重定向、错误）都不再显示
      const mine = ++gen.current
      const stale = () => gen.current !== mine
      submitting.current = mine
      setLoading(true)
      let r: Fetched
      try {
        r = await fetchPage({ url: u.pathname + u.search, body })
      } catch {
        if (!stale()) setFailure(msg('router.submitFailed'))
        return
      } finally {
        if (submitting.current === mine) submitting.current = null
        if (!stale()) setLoading(false)
      }
      if (stale()) return
      switch (r.kind) {
        case 'page':
          // 校验失败等：在当前地址显示服务端返回的页面（带表单错误）
          show(r.env, new URL(here(), window.location.href), { push: null, fromPost: true })
          return
        case 'redirect': {
          const next = inApp(r.to)
          const same = next !== null && withoutFlash(next) === here()
          await load(r.to, !same)
          return
        }
        case 'error':
          setFailure(r.message)
          return
        default: {
          const _exhaustive: never = r
          return _exhaustive
        }
      }
    },
    [load, show],
  )

  const refresh = useCallback(async (): Promise<boolean> => {
    const cur = viewRef.current
    const busy = () => inflight.current !== null || submitting.current === gen.current
    if (!cur || busy()) return true
    const operation = gen.current
    const mine = ++refreshGen.current
    let r: Fetched
    try {
      r = await fetchPage({ url: cur.url })
    } catch {
      return false
    }
    const now = viewRef.current
    // 提交、跳转（包括失败的操作）或更新的刷新都使当前响应失效。
    if (!now || now.seq !== cur.seq || gen.current !== operation || refreshGen.current !== mine || busy()) return true
    switch (r.kind) {
      case 'page':
        if (r.env.Page !== cur.env.Page) return true
        setView({ ...now, env: r.env })
        return true
      case 'redirect':
        if (new URL(r.to, window.location.href).pathname === loginPath) setExpired(true)
        return false
      case 'error':
        return false
      default: {
        const _exhaustive: never = r
        return _exhaustive
      }
    }
  }, [])

  // 首次打开：index.html 没有内嵌页面数据时取数据
  useEffect(() => {
    if (!viewRef.current) void load(window.location.href, false)
  }, [load])

  // 首次打开时页面数据内嵌在 index.html 中（服务端直接渲染）：地址中的 flash 只显示一次，
  // 清掉地址栏里的参数，刷新页面不会再次弹出同一个提示。
  useEffect(() => {
    if (!initial) return
    const u = new URL(window.location.href)
    if (!u.searchParams.has('flash')) return
    window.history.replaceState(null, '', withoutFlash(u) + u.hash)
  }, [initial])

  // 浏览器的前进、后退
  useEffect(() => {
    const onPop = () => void load(window.location.href, null)
    window.addEventListener('popstate', onPop)
    return () => window.removeEventListener('popstate', onPop)
  }, [load])

  // 站内链接在前端内跳转（新标签页、下载、外部链接和带修饰键的点击照常由浏览器处理）
  useEffect(() => {
    const onClick = (e: MouseEvent) => {
      if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
      if (!(e.target instanceof Element)) return
      const a = e.target.closest('a')
      if (!a || (a.target && a.target !== '_self') || a.hasAttribute('download')) return
      const href = a.getAttribute('href')
      if (!href || href.startsWith('#')) return
      const u = inApp(a.href)
      if (!u) return
      e.preventDefault()
      void navigate(u.pathname + u.search + u.hash)
    }
    document.addEventListener('click', onClick)
    return () => document.removeEventListener('click', onClick)
  }, [navigate])

  const value = useMemo<Router>(
    () => ({
      view,
      failure,
      loading,
      expired,
      navigate,
      submit,
      refresh,
      dismissFlash: () => setView((v) => (v ? { ...v, flash: null } : v)),
      dismissFailure: () => setFailure(null),
    }),
    [view, failure, loading, expired, navigate, submit, refresh],
  )
  return <RouterContext.Provider value={value}>{children}</RouterContext.Provider>
}
