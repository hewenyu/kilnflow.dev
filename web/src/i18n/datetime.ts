import type { Locale } from './locale-id'

// 日期时间的本地化格式（浏览器的 Intl.DateTimeFormat，按浏览器的时区）。
// zh-CN 沿用 Dashboard 一贯的 2026-11-05 14:03（数字、24 小时制）；en 用英文习惯的 Nov 5, 2026, 2:03 PM。

const clockFormats = new Map<Locale, Intl.DateTimeFormat>()

// 只是按语言缓存的 Intl 对象（内容不变），不是可变的翻译状态。
function clockFormat(locale: Locale): Intl.DateTimeFormat {
  let f = clockFormats.get(locale)
  if (!f) {
    f =
      locale === 'en'
        ? new Intl.DateTimeFormat('en', { year: 'numeric', month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' })
        : new Intl.DateTimeFormat(locale, { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })
    clockFormats.set(locale, f)
  }
  return f
}

/** formatClock 把时间格式化为本地时间（精确到分钟）。 */
export function formatClock(locale: Locale, d: Date): string {
  const f = clockFormat(locale)
  if (locale === 'en') return f.format(d)
  const parts = f.formatToParts(d)
  const part = (type: Intl.DateTimeFormatPartTypes) => parts.find((p) => p.type === type)?.value ?? ''
  return `${part('year')}-${part('month')}-${part('day')} ${part('hour')}:${part('minute')}`
}
