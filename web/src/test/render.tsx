import { BaseStyles } from '@primer/react'
import { ThemeProvider } from '@primer/react/next'
import { fireEvent, render, screen, type RenderResult } from '@testing-library/react'
import type { z } from 'zod'
import { App } from '../app/App'
import { storageKey } from '../i18n/locale'
import { msg, translate, type Locale, type MsgKey, type Params } from '../i18n/messages'
import { envelopeSchema, type Envelope } from '../lib/envelope'

/** envelope 把测试数据（例如 dev/fixtures/ 中的 JSON）校验为页面数据；overrides 按服务端的格式覆盖其中的字段（例如 Flash 可以是字符串或消息）。 */
export function envelope(json: unknown, overrides: Partial<z.input<typeof envelopeSchema>> = {}): Envelope {
  const base = typeof json === 'object' && json !== null ? json : {}
  return envelopeSchema.parse({ ...base, ...overrides })
}

/** withData 返回把 Data 中的字段替换后的页面数据。 */
export function withData(env: Envelope, data: Record<string, unknown>): Envelope {
  const base = typeof env.Data === 'object' && env.Data !== null ? env.Data : {}
  return { ...env, Data: { ...base, ...data } }
}

/**
 * renderPage 用完整的前端（外壳、导航、定时刷新）渲染页面数据 env，地址为 url；env 为 null 时前端自己取数据。
 * 语言默认是 zh-CN（不受 jsdom 的浏览器语言影响），locale 指定时按用户手动选择的语言渲染；每个用例结束后清理（见 setup.ts）。
 */
export function renderPage(env: Envelope | null, url = '/', { locale = 'zh-CN' }: { locale?: Locale } = {}): RenderResult {
  window.history.replaceState(null, '', url)
  window.localStorage.setItem(storageKey, locale)
  return render(
    <ThemeProvider colorMode="light">
      <BaseStyles>
        <App initial={env} />
      </BaseStyles>
    </ThemeProvider>,
  )
}

/** deferred 返回一个由测试决定何时完成的 Promise（ES2023 还没有 Promise.withResolvers）。 */
export function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve: (v: T) => void = () => undefined
  const promise = new Promise<T>((r) => {
    resolve = r
  })
  return { promise, resolve }
}

/**
 * switchLocale 在已登录页面的身份菜单中切换语言（与用户的操作相同，不重新渲染页面）。
 * 对话框打开时页面的其他部分对读屏软件隐藏，所以按 hidden 查找并直接触发点击（不经过对话框外的点击关闭逻辑）。
 */
export async function switchLocale(name: 'English' | '简体中文'): Promise<void> {
  fireEvent.click(screen.getByRole('button', { name: /的菜单$|^Menu for /, hidden: true }))
  fireEvent.click(await screen.findByRole('menuitemradio', { name, hidden: true }))
}

/** tFor 返回按语言 locale 翻译前端消息的函数（与 useT 的返回值相同），测试中直接调用接收 t 的函数时用。 */
export function tFor(locale: Locale): (id: MsgKey, params?: Params) => string {
  return (id, params) => translate(locale, msg(id, params))
}

/**
 * chineseLeft 返回 root 的文字中去掉原样显示的内容（raw，例如 Runner 上报的错误）之后剩下的中文片段：
 * 英文界面中界面文字都应该来自词典，剩下的中文就是残留的硬编码文案。
 */
export function chineseLeft(root: HTMLElement, raw: readonly string[] = []): string[] {
  let text = root.textContent
  for (const r of raw) text = text.split(r).join(' ')
  return text.match(/[\u4e00-\u9fff][^a-zA-Z0-9]*/g) ?? []
}


/**
 * rawStrings 返回页面数据 data 中含中文的字符串（服务端原样给出的内容，例如报告、Issue 标题、原因），长的在前：
 * 作为 chineseLeft 的 raw。服务端给的界面文字是消息（{id, params}），不在其中。
 */
export function rawStrings(data: unknown): string[] {
  const out = new Set<string>()
  const walk = (v: unknown): void => {
    if (typeof v === 'string') {
      if (/[\u4e00-\u9fff]/.test(v)) out.add(v)
    } else if (Array.isArray(v)) {
      v.forEach(walk)
    } else if (typeof v === 'object' && v !== null) {
      Object.values(v).forEach(walk)
    }
  }
  walk(data)
  return [...out].sort((a, b) => b.length - a.length)
}
