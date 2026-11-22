import type { ReactNode } from 'react'
import { useLocale, useT } from '../i18n/locale'
import { runState } from '../lib/format'
import { runTitle, type Run } from '../lib/schemas'
import { StatusIcon } from './StatusIcon'
import css from './RunHeader.module.css'

/**
 * RunHeader 是 run 和 job 详情页顶部的标题区（GitHub Actions 运行页和 job 页共用的标题）：
 * 位置、状态图标、run 的标题和编号，右侧是操作。两页的标题区相同，切换时左侧栏不会上下跳动。
 * state 为 null 时不显示状态图标（查看的运行没有记录）；runURL 不为空时位置中的 #ID 是指向 run 详情页的链接。
 */
export function RunHeader({
  run,
  repo,
  back,
  state,
  runURL,
  note,
  actions,
}: {
  run: Pick<Run, 'ID' | 'Workflow' | 'WorkflowName'>
  repo: string
  /** 位置中"运行"指向的列表地址（从筛选后的列表进入时是那个列表）。 */
  back: string
  state: string | null
  runURL?: string
  /** 标题后面的补充，例如正在查看的是第几次运行。 */
  note?: ReactNode
  actions?: ReactNode
}) {
  const t = useT()
  const { locale } = useLocale()
  return (
    <>
      <nav className={css.crumbs} aria-label={t('nav.breadcrumb')}>
        <a href={back}>{t('nav.runs')}</a>
        <span aria-hidden>/</span>
        <a href={`/runs?${new URLSearchParams({ repo }).toString()}`}>{repo}</a>
        <span aria-hidden>/</span>
        {runURL ? <a href={runURL}>#{run.ID}</a> : <span>#{run.ID}</span>}
      </nav>
      <header className={css.head}>
        <div className={css.titleRow}>
          {state !== null && (
            <span className={css.icon}>
              <StatusIcon state={state} size={24} label={state ? runState(state, locale) : t('run.stateUnknown')} />
            </span>
          )}
          <h1 className={css.title}>
            {runTitle(run)} <span className={css.id}>#{run.ID}</span>
          </h1>
          {note}
        </div>
        {actions && <div className={css.actions}>{actions}</div>}
      </header>
    </>
  )
}
