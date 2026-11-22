import { Fragment, createContext, useCallback, useContext, useLayoutEffect, useMemo, useState, type ReactNode } from 'react'
import { defaultLocale, isLocale, matchLocale, type Locale } from './locale-id'
import { msg, segments, translate, type MsgKey, type Params, type Text } from './messages'

// 当前语言：LocaleProvider 放在 RouterProvider 之上（见 App.tsx），切换语言只重新渲染，不改变页面的 key，
// 页面不会重新挂载，表单输入、打开的对话框和 view.seq 都保留。

/** storageKey 是浏览器中保存手动选择的语言的键。 */
export const storageKey = 'kiln.locale'

/** readStoredLocale 读取手动选择的语言；浏览器存储不可用（隐私模式、被禁用）或值不认识时返回 null。 */
export function readStoredLocale(): Locale | null {
  try {
    const v = window.localStorage.getItem(storageKey)
    return isLocale(v) ? v : null
  } catch {
    return null
  }
}

/** storeLocale 保存手动选择的语言；浏览器存储不可用时只在内存中生效（刷新后按浏览器语言重新判断）。 */
function storeLocale(l: Locale): void {
  try {
    window.localStorage.setItem(storageKey, l)
  } catch {
    // 存不了：只在这个页面中生效
  }
}

/** browserLanguages 返回浏览器的语言偏好（按优先级）。 */
function browserLanguages(): readonly string[] {
  const n = window.navigator
  return n.languages.length > 0 ? n.languages : [n.language]
}

/** initialLocale 是打开页面时的语言：手动选择的 > 浏览器的语言偏好 > 默认语言。 */
export function initialLocale(): Locale {
  return readStoredLocale() ?? matchLocale(browserLanguages()) ?? defaultLocale
}

interface LocaleCtx {
  locale: Locale
  setLocale: (l: Locale) => void
}

// 没有 LocaleProvider 时（单独渲染组件的测试）是默认语言，切换不生效
const Ctx = createContext<LocaleCtx>({ locale: defaultLocale, setLocale: () => undefined })

/** LocaleProvider 提供当前语言，并同步 <html lang>。 */
export function LocaleProvider({ children }: { children: ReactNode }) {
  const [locale, setState] = useState<Locale>(initialLocale)
  useLayoutEffect(() => {
    document.documentElement.lang = locale
  }, [locale])
  const setLocale = useCallback((l: Locale) => {
    storeLocale(l)
    setState(l)
  }, [])
  const value = useMemo(() => ({ locale, setLocale }), [locale, setLocale])
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

/** useLocale 返回当前语言和切换语言的函数。 */
export function useLocale(): LocaleCtx {
  return useContext(Ctx)
}

/** useT 返回按当前语言翻译前端消息的函数（返回纯文本，用于属性、标题和普通文字）。 */
export function useT(): (id: MsgKey, params?: Params) => string {
  const { locale } = useLocale()
  return useCallback((id: MsgKey, params?: Params) => translate(locale, msg(id, params)), [locale])
}

/** useText 返回按当前语言显示 Text（服务端的消息或原样的文字）的函数（纯文本）。 */
export function useText(): (t: Text) => string {
  const { locale } = useLocale()
  return useCallback((t: Text) => translate(locale, t), [locale])
}

/** Tags 是富文本标签的渲染方式：文案中的 <name>…</name> 渲染为 tags[name](…)；没有提供的标签按普通文字显示。 */
export type Tags = Readonly<Record<string, (children: string) => ReactNode>>

/**
 * Msg 按当前语言显示一段文字（服务端给的 Text 或前端的消息）。富文本标签只用 tags 中提供的元素，
 * 参数永远是普通文字，不会被当作 HTML 或标签。
 */
export function Msg({ text, tags }: { text: Text; tags?: Tags }) {
  const { locale } = useLocale()
  return (
    <>
      {segments(locale, text).map((s, i) => {
        const render = s.tag === null ? undefined : tags?.[s.tag]
        return <Fragment key={i}>{render ? render(s.text) : s.text}</Fragment>
      })}
    </>
  )
}

/** Trans 按当前语言显示前端的消息 id（可以带富文本标签）。 */
export function Trans({ id, params, tags }: { id: MsgKey; params?: Params; tags?: Tags }) {
  return <Msg text={msg(id, params)} tags={tags} />
}
