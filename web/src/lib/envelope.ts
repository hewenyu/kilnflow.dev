import { z } from 'zod'
import { parseText, type Text } from '../i18n/messages'

// 服务端返回的页面数据（internal/server/web/ui.go 的 uiPage 和 uiRedirect）。

export const sessionSchema = z.object({
  Login: z.string(),
  Admin: z.boolean(),
  /** 用 admin token 登录。 */
  ViaToken: z.boolean(),
  /** 这是管理员绑定的 GitHub 账号。 */
  Bound: z.boolean(),
})
export type Session = z.infer<typeof sessionSchema>

/**
 * textSchema 是服务端给的一段文字：字符串原样显示，{id, params} 是按当前语言显示的消息（见 i18n/messages.ts）。
 * 不会校验失败：格式不对的消息显示兜底说明，不让整页打不开。
 */
export const textSchema = z.unknown().transform((v): Text => parseText(v ?? ''))

export const envelopeSchema = z.object({
  /** 页面名称，决定用哪个页面组件。 */
  Page: z.string(),
  /** 页面标题（document.title）。 */
  Title: textSchema,
  /** 当前所在的导航标签（overview、runs、merge_queue、ai_dev、runners、settings、login、error）。 */
  Nav: z.string(),
  Status: z.number(),
  Session: sessionSchema.nullable(),
  /** 表单提交时带的 csrf 字段；未登录时为空。 */
  CSRF: z.string(),
  OAuth: z.boolean(),
  /** 正在进行或排队的 run 数。 */
  Active: z.number(),
  /** 跳转后显示一次的提示（服务端校验过的 ?flash=）；空串表示没有。 */
  Flash: textSchema,
  Data: z.unknown(),
})
export type Envelope = z.infer<typeof envelopeSchema>

export const redirectSchema = z.object({ Redirect: z.string() })

/** 服务端常用的可空切片：Go 的 nil 切片编码为 null，这里统一转换为空数组。 */
export function list<T extends z.ZodType>(item: T) {
  return z
    .array(item)
    .nullable()
    .transform((v) => v ?? [])
}

/** formState：表单校验失败时回填的值和错误（internal/server/web 的 formState）。 */
export const formStateSchema = z
  .object({
    Values: z.record(z.string(), z.string()).nullable(),
    Errors: z.record(z.string(), textSchema).nullable(),
    Failed: z.boolean(),
  })
  .nullable()
export type FormState = z.infer<typeof formStateSchema>

/** fieldError 返回服务端回填的字段错误（没有时为 undefined）；name 为 "_" 时是整个表单的错误。 */
export function fieldError(form: FormState, name: string): Text | undefined {
  const e = form?.Errors?.[name]
  return e === '' ? undefined : e
}

/** fieldValue 返回服务端回填的字段值（没有时为空串）。 */
export function fieldValue(form: FormState, name: string): string {
  return form?.Values?.[name] ?? ''
}
