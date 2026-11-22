import type { ReactNode } from 'react'
import css from './Subhead.module.css'

/**
 * Subhead 是设置页等的分节标题（GitHub 的 Subhead）。level 1 是页面标题，2 是页面内的分节（spaced 时与上文隔开）。
 * actions 放在右侧，description 是标题下的说明。
 */
export function Subhead({
  children,
  level = 1,
  id,
  spaced,
  danger,
  actions,
  description,
}: {
  children: ReactNode
  level?: 1 | 2
  id?: string
  spaced?: boolean
  danger?: boolean
  actions?: ReactNode
  description?: ReactNode
}) {
  const H = level === 1 ? 'h1' : 'h2'
  const cls = [css.subhead, level === 2 && css.small, spaced && css.spaced, danger && css.danger].filter(Boolean).join(' ')
  return (
    <div className={cls}>
      <H className={css.heading} id={id}>
        {children}
      </H>
      {actions}
      {description && <p className={css.description}>{description}</p>}
    </div>
  )
}
