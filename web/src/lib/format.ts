import { formatClock } from '../i18n/datetime'
import { defaultLocale, type Locale } from '../i18n/locale-id'
import { isMsgKey, msg, translate, type MsgKey, type Params } from '../i18n/messages'

// 时间、摘要、状态和事件的说明（从 Go 模板函数移植，规则与服务端一致）。
// 需要文字的函数都显式接收语言 locale（React 组件经 useLocale 取得）；不传时为默认语言 zh-CN（还没有迁移的页面）。

const tr = (locale: Locale, id: MsgKey, params?: Params) => translate(locale, msg(id, params))

/** Unix 秒，或 Go 编码的时间字符串（RFC 3339）。0、空串、null 和 Go 的零值时间都表示"没有"。 */
export type TimeLike = number | string | null | undefined

/** toDate 把时间转换为 Date；没有时间时返回 null。 */
export function toDate(t: TimeLike): Date | null {
  if (t === null || t === undefined || t === 0 || t === '') return null
  if (typeof t === 'number') return new Date(t * 1000)
  if (t.startsWith('0001-01-01')) return null
  const d = new Date(t)
  return Number.isNaN(d.getTime()) ? null : d
}

const pad = (n: number) => String(n).padStart(2, '0')

/** clock 把时间格式化为本地时间（精确到分钟），例如 2026-11-05 14:03、Nov 5, 2026, 2:03 PM。 */
export function clock(t: TimeLike, locale: Locale = defaultLocale): string {
  const d = toDate(t)
  if (!d) return ''
  return formatClock(locale, d)
}

/** timeOfDay 把时间格式化为本地的时:分:秒，例如 14:03:09。 */
export function timeOfDay(t: TimeLike): string {
  const d = toDate(t)
  if (!d) return ''
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

/** ago 把时间格式化为"多久之前"；48 小时以前的显示日期和时间。 */
export function ago(t: TimeLike, now: number = Date.now(), locale: Locale = defaultLocale): string {
  const d = toDate(t)
  if (!d) return ''
  const s = Math.max(0, Math.floor((now - d.getTime()) / 1000))
  if (s < 60) return tr(locale, 'time.secondsAgo', { count: s })
  if (s < 3600) return tr(locale, 'time.minutesAgo', { count: Math.floor(s / 60) })
  if (s < 48 * 3600) return tr(locale, 'time.hoursAgo', { count: Math.floor(s / 3600) })
  return formatClock(locale, d)
}

/** dur 返回从 a 到 b（Unix 秒）的时长，例如"45 秒""3 分 26 秒""1 小时 5 分"（en：45s、3m 26s、1h 5m）；b 为 0 表示到现在，a 为 0 时返回空串。 */
export function dur(a: number, b: number, now: number = Date.now(), locale: Locale = defaultLocale): string {
  if (a === 0) return ''
  const end = b === 0 ? Math.floor(now / 1000) : b
  const s = Math.max(end - a, 0)
  if (s < 60) return tr(locale, 'duration.seconds', { s })
  if (s < 3600) return tr(locale, 'duration.minutes', { m: Math.floor(s / 60), s: s % 60 })
  return tr(locale, 'duration.hours', { h: Math.floor(s / 3600), m: Math.floor((s % 3600) / 60) })
}

/** short 返回 commit SHA、action key 等摘要的前 12 个字符。 */
export function short(s: string): string {
  return s.length > 12 ? s.slice(0, 12) : s
}

/** shortDigest 返回 digest 去掉算法前缀后的前 12 位。 */
export function shortDigest(d: string): string {
  const i = d.indexOf(':')
  return short(i >= 0 ? d.slice(i + 1) : d)
}

/** shortVersion 缩短版本号：较长的 commit SHA 只保留前 12 位，其他超过 24 个字符的截断。返回值与原值相同表示没有缩短。 */
export function shortVersion(v: string): string {
  if (v.length > 12 && /^[0-9a-f]+$/.test(v)) return v.slice(0, 12)
  if (v.length > 24) return truncate(v, 24)
  return v
}

/** truncate 把 s 截断为最多 n 个字符（超出时以"…"结尾）。 */
export function truncate(s: string, n: number): string {
  const chars = Array.from(s)
  return chars.length > n ? chars.slice(0, n - 1).join('') + '…' : s
}

/** size 把字节数格式化为 B、KiB、MiB 等（二进制前缀，与服务端 bytesize.Format 一致）。 */
export function size(n: number): string {
  if (n < 1024) return `${n} B`
  const units = 'KMGTP'
  let div = 1024
  let exp = 0
  for (let m = Math.floor(n / 1024); m >= 1024 && exp < units.length - 1; m = Math.floor(m / 1024)) {
    div *= 1024
    exp++
  }
  return `${(n / div).toFixed(1)} ${units.charAt(exp)}iB`
}

/** 状态的大类，决定图标和颜色。 */
export type Tone = 'ok' | 'bad' | 'run' | 'wait' | 'muted'

/** labelVariant 返回状态大类对应的 Primer Label 样式。 */
export function labelVariant(t: Tone): 'success' | 'danger' | 'attention' | 'secondary' {
  switch (t) {
    case 'ok':
      return 'success'
    case 'bad':
      return 'danger'
    case 'run':
    case 'wait':
      return 'attention'
    case 'muted':
      return 'secondary'
    default: {
      const _exhaustive: never = t
      return _exhaustive
    }
  }
}

/** enumLabel 返回 group.value 的说明（例如 runState.queued）；value 为空时用 emptyKey，词典中没有时原样返回 value。 */
export function enumLabel(locale: Locale, group: string, value: string, emptyKey?: string): string {
  const key = `${group}.${value === '' && emptyKey ? emptyKey : value}`
  return isMsgKey(key) ? tr(locale, key) : value
}

/** stateTone 返回 job、run、Runner、outbox 等状态的大类。 */
export function stateTone(state: string): Tone {
  switch (state) {
    case 'succeeded':
    case 'success':
    case 'done':
      return 'ok'
    case 'failed':
    case 'timed_out':
    case 'failure':
    case 'dead':
    case 'revoked':
      return 'bad'
    case 'running':
    case 'active':
      return 'run'
    case 'pending':
    case 'queued':
    case 'waiting':
      return 'wait'
    default:
      return 'muted'
  }
}

/** runState 返回 run 状态的说明；未知的值原样返回。 */
export function runState(state: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'runState', state)
}

/** jobState 返回 job 状态的说明；未知的值原样返回。 */
export function jobState(state: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'jobState', state)
}

/** jobStatesSummary 返回一组 job 状态的文字说明，例如"4 个 job：成功 1，执行中 2，等待上游 1"。 */
export function jobStatesSummary(states: readonly string[], locale: Locale = defaultLocale): string {
  const counts = new Map<string, number>()
  for (const s of states) {
    const l = jobState(s, locale)
    counts.set(l, (counts.get(l) ?? 0) + 1)
  }
  const parts = Array.from(counts, ([l, n]) => tr(locale, 'jobSummary.part', { label: l, count: n }))
  return tr(locale, 'jobSummary.text', { count: states.length, parts: parts.join(tr(locale, 'list.separator')) })
}

/** outcome 返回 attempt 结果（空值表示还在执行）的说明；未知的值原样返回。 */
export function outcome(o: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'outcome', o, 'running')
}

/** outcomeTone 返回 attempt 结果的大类。 */
export function outcomeTone(o: string): Tone {
  switch (o) {
    case '':
      return 'run'
    case 'succeeded':
      return 'ok'
    case 'failed':
    case 'infra_error':
    case 'timed_out':
      return 'bad'
    default:
      return 'muted'
  }
}

/** eventLabel 返回触发事件的说明；未知的值原样返回。 */
export function eventLabel(event: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'event', event)
}

/** refName 返回 ref 的简短名称，例如 refs/heads/main → main、refs/tags/v1.2.0 → v1.2.0。 */
export function refName(ref: string): string {
  for (const p of ['refs/heads/', 'refs/tags/']) {
    if (ref.startsWith(p)) return ref.slice(p.length)
  }
  return ref
}

/** refKind 返回 ref 的种类：分支、tag；其他 ref 返回 null。 */
export function refKind(ref: string): 'branch' | 'tag' | null {
  if (ref.startsWith('refs/heads/')) return 'branch'
  if (ref.startsWith('refs/tags/')) return 'tag'
  return null
}

/** rerunLabel 返回第 n 次运行的名称：0 是"首次运行"，其余是"第 n 次重跑"。 */
export function rerunLabel(n: number, locale: Locale = defaultLocale): string {
  return n === 0 ? tr(locale, 'rerun.first') : tr(locale, 'rerun.nth', { n })
}

/** releaseState 返回发布状态的说明；未知的值原样返回。 */
export function releaseState(state: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'releaseState', state)
}

/** targetState 返回发布目标状态的说明；未知的值原样返回。 */
export function targetState(state: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'targetState', state)
}

/** targetTone 返回发布目标状态的大类。 */
export function targetTone(state: string): Tone {
  switch (state) {
    case 'committed':
      return 'ok'
    case 'failed':
      return 'bad'
    case 'staged':
      return 'wait'
    default:
      return 'muted'
  }
}

/** targetKind 返回发布目标种类的说明；未知的值原样返回。 */
export function targetKind(kind: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'targetKind', kind)
}

/** outboxState 返回 outbox 投递状态的说明；未知的值原样返回。 */
export function outboxState(state: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'outboxState', state)
}

/** tokenActor 是还没有绑定 GitHub 账号时 admin token 的操作者（与服务端 admin.TokenActor 相同）。 */
export const tokenActor = 'admin'

const githubLogin = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$/

/** avatarURL 返回登录名的 GitHub 头像地址；admin token、Bot 和不合格式的登录名没有头像，返回 null。 */
export function avatarURL(login: string, px = 96): string | null {
  if (login === '' || login === tokenActor || login.endsWith('[bot]') || !githubLogin.test(login)) return null
  return `https://avatars.githubusercontent.com/${login}?s=${px}`
}

/** avatarHue 返回登录名首字母头像的底色色相（0–359，与服务端相同的 FNV-1a 哈希）。 */
export function avatarHue(login: string): number {
  let h = 0x811c9dc5
  for (const b of new TextEncoder().encode(login.toLowerCase())) {
    h ^= b
    h = Math.imul(h, 0x01000193) >>> 0
  }
  return h % 360
}
