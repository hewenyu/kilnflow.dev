import { BaseStyles } from '@primer/react'
import { ThemeProvider } from '@primer/react/next'
import { render, type RenderResult } from '@testing-library/react'
import { App } from '../app/App'
import { envelopeSchema, type Envelope } from '../lib/envelope'

/** envelope 把测试数据（例如 dev/fixtures/ 中的 JSON）校验为页面数据；overrides 覆盖其中的字段。 */
export function envelope(json: unknown, overrides: Partial<Envelope> = {}): Envelope {
  return { ...envelopeSchema.parse(json), ...overrides }
}

/** withData 返回把 Data 中的字段替换后的页面数据。 */
export function withData(env: Envelope, data: Record<string, unknown>): Envelope {
  const base = typeof env.Data === 'object' && env.Data !== null ? env.Data : {}
  return { ...env, Data: { ...base, ...data } }
}

/** renderPage 用完整的前端（外壳、导航、定时刷新）渲染页面数据 env，地址为 url；env 为 null 时前端自己取数据。 */
export function renderPage(env: Envelope | null, url = '/'): RenderResult {
  window.history.replaceState(null, '', url)
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
