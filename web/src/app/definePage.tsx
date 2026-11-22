import { Banner } from '@primer/react'
import type { ComponentType, ReactNode } from 'react'
import type { z } from 'zod'
import type { Envelope } from '../lib/envelope'

/** PageProps 是页面组件的参数：校验过的页面数据和公共数据。 */
export interface PageProps<T> {
  data: T
  env: Envelope
}

/** PageEntry 是一个页面：校验页面数据并渲染。 */
export interface PageEntry {
  render: (env: Envelope) => ReactNode
}

/** definePage 定义页面：schema 校验服务端的页面数据（env.Data），通过后交给 Component。 */
export function definePage<S extends z.ZodType>(schema: S, Component: ComponentType<PageProps<z.output<S>>>): PageEntry {
  return {
    render(env) {
      const r = schema.safeParse(env.Data)
      if (!r.success) {
        const where = r.error.issues
          .slice(0, 3)
          .map((i) => `${i.path.join('.') || '（根）'}：${i.message}`)
          .join('；')
        return (
          <Banner
            variant="critical"
            title="页面数据格式不对"
            description={`前端和 kiln-server 的版本可能不一致，刷新页面重试。（${env.Page}：${where}）`}
            hideTitle={false}
          />
        )
      }
      return <Component data={r.data} env={env} />
    },
  }
}
