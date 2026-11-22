// 语言标识（不依赖词典，datetime.ts、messages.ts、locale.tsx 共用）。

/** 支持的语言。 */
export const locales = ['zh-CN', 'en'] as const
export type Locale = (typeof locales)[number]

/** 默认语言：没有手动选择、浏览器语言也不匹配时使用。 */
export const defaultLocale: Locale = 'zh-CN'

/** 语言选择菜单中各语言的名称：总是用该语言自己的写法（不随界面语言变化）。 */
export const localeNames: Readonly<Record<Locale, string>> = { 'zh-CN': '简体中文', en: 'English' }

export function isLocale(v: unknown): v is Locale {
  return v === 'zh-CN' || v === 'en'
}

/**
 * matchLocale 按浏览器的语言偏好（navigator.languages，与 Accept-Language 相同的顺序）选择语言：
 * zh-* 为 zh-CN，en-* 为 en，都不匹配时返回 null。
 */
export function matchLocale(languages: readonly string[]): Locale | null {
  for (const l of languages) {
    const primary = l.toLowerCase().split('-')[0]
    if (primary === 'zh') return 'zh-CN'
    if (primary === 'en') return 'en'
  }
  return null
}
