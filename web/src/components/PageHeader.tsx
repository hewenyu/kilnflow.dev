import type { ReactNode } from 'react'
import css from './PageHeader.module.css'

/**
 * PageHeader 是列表页（总览、运行、Runner）顶部的标题：24px 标题、最多一行说明，右侧是页面的主要操作。
 * 详细说明放在页面的帮助（details）中，不放在这里。
 */
export function PageHeader({ title, description, actions }: { title: string; description?: ReactNode; actions?: ReactNode }) {
  return (
    <div className={css.head}>
      <div className={css.text}>
        <h1 className={css.title}>{title}</h1>
        {description && <p className={css.description}>{description}</p>}
      </div>
      {actions && <div className={css.actions}>{actions}</div>}
    </div>
  )
}
