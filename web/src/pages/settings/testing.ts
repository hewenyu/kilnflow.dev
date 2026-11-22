import { json, stubFetch, type Call } from '../../test/fetch'

// 设置页测试共用：记录前端发出的请求（表单提交），服务端一律返回重定向。

/** stubSubmit 替换 fetch：记录请求，返回重定向到 redirect。返回记录的请求。 */
export function stubSubmit(redirect = '/settings'): Call[] {
  return stubFetch(() => json({ Redirect: redirect }))
}

/** hidden 返回表单中隐藏字段的值（没有时为 null）。 */
export function hidden(form: Element, name: string): string | null {
  const el = form.querySelector(`input[type="hidden"][name="${name}"]`)
  return el instanceof HTMLInputElement ? el.value : null
}
