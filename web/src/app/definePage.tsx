import { Banner } from '@primer/react'
import type { ComponentType, ReactNode } from 'react'
import type { z } from 'zod'
import { Trans, useT } from '../i18n/locale'
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

/** InvalidData 说明页面数据格式不对（前后端版本不一致）：显示前几个问题的位置，按当前语言显示。 */
function InvalidData({ page, issues }: { page: string; issues: readonly z.core.$ZodIssue[] }) {
  const t = useT()
  const where = issues
    .slice(0, 3)
    .map((i) => t('page.dataIssue', { path: i.path.join('.') || t('page.dataRoot'), issue: i.message }))
    .join(t('list.separator'))
  return (
    <Banner
      variant="critical"
      title={t('page.dataInvalid')}
      description={<Trans id="page.dataInvalidDetail" params={{ page, where }} />}
      hideTitle={false}
    />
  )
}

/** definePage 定义页面：schema 校验服务端的页面数据（env.Data），通过后交给 Component。 */
export function definePage<S extends z.ZodType>(schema: S, Component: ComponentType<PageProps<z.output<S>>>): PageEntry {
  return {
    render(env) {
      const r = schema.safeParse(env.Data)
      if (!r.success) return <InvalidData page={env.Page} issues={r.error.issues} />
      return <Component data={r.data} env={env} />
    },
  }
}
