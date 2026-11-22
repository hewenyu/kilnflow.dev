import { formatClock } from './datetime'
import en from './locales/en.json'
import zh from './locales/zh-CN.json'
import type { Locale } from './locale-id'

// 界面文案的词典与翻译（设计见 docs/I18N.md）。这里只有纯函数，不依赖 React，非 React 模块（api.ts、format.ts）
// 也能用；语言总是由调用方显式传入，不保存"当前语言"之类的模块级状态。
//
// 词典是 locales/ 下的 JSON（随前端构建内嵌进 kiln-server，Go 的契约测试也读它）。文案的写法：
//   - {name}：参数原样插入；{name, number}：按语言格式化数字；{name, time}：Unix 秒，按语言格式化为本地时间；
//   - 复数：值写成 { "one": "…", "other": "…" }（类别见 Intl.PluralRules），按参数 count 选择；
//   - 富文本：<tag>…</tag>，只有渲染时提供了同名元素的标签才生效（见 Trans），参数中的 < > 永远是普通文字。

export { defaultLocale, isLocale, localeNames, locales, type Locale } from './locale-id'

/** 复数文案：各类别的写法，other 必须有。 */
type PluralForms = Readonly<Partial<Record<Intl.LDMLPluralRule, string>> & { other: string }>
type Entry = string | PluralForms

/** MsgKey 是词典中的键（编译期检查）。 */
export type MsgKey = keyof typeof zh

// 两种语言的键集合必须相同：缺了任何一个键都是编译错误（参数一致性由 messages.test.ts 检查）。
const zhDict: Readonly<Record<keyof typeof en, Entry>> = zh
const enDict: Readonly<Record<MsgKey, Entry>> = en
const dictionaries: Readonly<Record<Locale, Readonly<Record<string, Entry | undefined>>>> = { 'zh-CN': zhDict, en: enDict }

/** isMsgKey 表示 k 是词典中的键（按状态值拼出键时检查）。 */
export function isMsgKey(k: string): k is MsgKey {
  return Object.hasOwn(zh, k)
}

/** dictionary 返回语言的词典（测试检查完整性用）。 */
export function dictionary(locale: Locale): Readonly<Record<string, Entry | undefined>> {
  return dictionaries[locale]
}

export type ParamValue = string | number
export type Params = Readonly<Record<string, ParamValue>>

/**
 * Text 是要显示的一段文字：
 *   - string：原样显示的文字（服务端的对象名、原始错误说明，以及还没有迁移成消息的旧文案）；
 *   - { id, params }：消息，显示时按当前语言从词典取文案。
 */
export type Text = string | MessageRef

export interface MessageRef {
  readonly id: string
  readonly params?: Params
}

/** msg 构造前端自己的消息（键在编译期检查）。 */
export function msg(id: MsgKey, params?: Params): MessageRef {
  return params ? { id, params } : { id }
}

/** 服务端消息的格式上限：超出的视为格式不对。 */
const maxIDLength = 100
const maxParams = 16
const maxParamLength = 2000
const idPattern = /^[a-z][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)+$/

/** invalidMessage 是格式不对的消息（显示通用的兜底说明）。 */
const invalidMessage: MessageRef = { id: 'message.invalid' }

/**
 * parseText 把服务端返回的文字（页面数据中的 string 或 {id, params}）转换为 Text，不会失败：
 * 格式不对的消息转换为兜底说明，不合格的参数被丢掉（显示时按缺参处理），页面不会因为一段文字白屏。
 */
export function parseText(v: unknown): Text {
  if (typeof v === 'string') return v
  if (typeof v !== 'object' || v === null || Array.isArray(v)) return invalidMessage
  const id: unknown = Reflect.get(v, 'id')
  if (typeof id !== 'string' || id.length > maxIDLength || !idPattern.test(id)) return invalidMessage
  const raw: unknown = Reflect.get(v, 'params')
  if (raw === undefined || raw === null) return { id }
  if (typeof raw !== 'object' || Array.isArray(raw)) return { id }
  const params: Record<string, ParamValue> = {}
  let n = 0
  for (const [k, p] of Object.entries(raw)) {
    if (++n > maxParams) break
    if ((typeof p === 'string' && p.length <= maxParamLength) || (typeof p === 'number' && Number.isFinite(p))) params[k] = p
  }
  return { id, params }
}

/** isEmpty 表示没有文字（空串）。 */
export function isEmpty(t: Text | null | undefined): t is '' | null | undefined {
  return t === null || t === undefined || t === ''
}

/** 缺少参数或参数类型不对时，插入的占位符。 */
export const missingParam = '…'

const pluralRules = new Map<Locale, Intl.PluralRules>()
const numberFormats = new Map<Locale, Intl.NumberFormat>()

// 两个 Map 只是 Intl 对象的缓存（按语言惰性创建、内容不变），不是可变的翻译状态。
function plural(locale: Locale): Intl.PluralRules {
  let r = pluralRules.get(locale)
  if (!r) {
    r = new Intl.PluralRules(locale)
    pluralRules.set(locale, r)
  }
  return r
}

/** formatNumber 按语言格式化数字（千位分隔等）。 */
export function formatNumber(locale: Locale, n: number): string {
  let f = numberFormats.get(locale)
  if (!f) {
    f = new Intl.NumberFormat(locale)
    numberFormats.set(locale, f)
  }
  return f.format(n)
}

/** template 返回消息在语言 locale 下的文案（已按 count 选好复数形式）；没有这个键时返回 undefined。 */
function template(locale: Locale, id: string, params: Params | undefined): string | undefined {
  const e = dictionaries[locale][id]
  if (e === undefined) return undefined
  if (typeof e === 'string') return e
  const count = params?.count
  if (typeof count !== 'number') return e.other
  return e[plural(locale).select(count)] ?? e.other
}

const placeholder = /\{(\w+)(?:,\s*(number|time|duration))?\}/g

/** formatSpan 按语言写出时长（秒）：整分钟写成分钟（"2 分钟"、"2 minutes"），其余写成秒。 */
export function formatSpan(locale: Locale, seconds: number): string {
  const s = Math.round(seconds)
  const [id, count] = s >= 60 && s % 60 === 0 ? ['span.minutes', s / 60] : ['span.seconds', s]
  const params = { count }
  return fill(locale, template(locale, id, params) ?? String(count), params)
}

/** fill 把文案中的参数占位符替换为参数值；缺少参数或类型不对时插入 missingParam。 */
function fill(locale: Locale, s: string, params: Params | undefined): string {
  return s.replace(placeholder, (_, name: string, kind: string | undefined) => {
    const v = params?.[name]
    if (v === undefined) return missingParam
    switch (kind) {
      case 'number':
        return typeof v === 'number' ? formatNumber(locale, v) : missingParam
      case 'time':
        return typeof v === 'number' ? formatClock(locale, new Date(v * 1000)) : missingParam
      case 'duration':
        return typeof v === 'number' && Number.isFinite(v) ? formatSpan(locale, v) : missingParam
      default:
        return String(v)
    }
  })
}

/** Segment 是富文本文案的一段：普通文字，或 <tag>…</tag> 包住的文字。 */
export type Segment = { tag: null; text: string } | { tag: string; text: string }

const tagPattern = /<([a-z]+)>(.*?)<\/\1>/gs

/**
 * segments 把消息切分为文字段和标签段（参数在切分之后才代入，参数中的 < > 不会被当作标签）。
 * 不认识的消息、格式不对的消息返回兜底说明。
 */
export function segments(locale: Locale, text: Text): Segment[] {
  if (typeof text === 'string') return [{ tag: null, text }]
  let params = text.params
  let s = template(locale, text.id, params)
  if (s === undefined) {
    params = { id: text.id }
    s = template(locale, 'message.unknown', params) ?? text.id
  }
  const out: Segment[] = []
  let last = 0
  for (const m of s.matchAll(tagPattern)) {
    if (m.index > last) out.push({ tag: null, text: fill(locale, s.slice(last, m.index), params) })
    out.push({ tag: m[1] ?? '', text: fill(locale, m[2] ?? '', params) })
    last = m.index + m[0].length
  }
  if (last < s.length) out.push({ tag: null, text: fill(locale, s.slice(last), params) })
  return out
}

/** translate 返回文字在语言 locale 下的纯文本（富文本标签去掉，只保留其中的文字）：用于 aria-label、title、document.title 等。 */
export function translate(locale: Locale, text: Text): string {
  return segments(locale, text)
    .map((s) => s.text)
    .join('')
}

/** placeholders 返回文案中的参数名和格式（测试检查两种语言的参数一致）。 */
export function placeholders(entry: Entry): Map<string, string> {
  const out = new Map<string, string>()
  const forms: string[] = typeof entry === 'string' ? [entry] : Object.values(entry)
  for (const f of forms) {
    for (const m of f.matchAll(placeholder)) out.set(m[1] ?? '', m[2] ?? '')
  }
  return out
}
