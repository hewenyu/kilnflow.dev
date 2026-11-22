import { HomeIcon } from '@primer/octicons-react'
import { ActionList, ActionMenu, NavList } from '@primer/react'
import { useLocale, useT } from '../i18n/locale'
import { jobState } from '../lib/format'
import type { JobView } from '../lib/schemas'
import { StatusIcon } from './StatusIcon'
import css from './RunSidebar.module.css'

/**
 * RunSidebar 是 run 和 job 详情页左侧的导航：概要（run 详情页）和这次运行（rerun）中的全部 job。
 * summaryURL 是 run 详情页的地址（查看的不是最新一次运行时带 ?rerun=n），current 是当前页面（'summary' 或 job ID）。
 */
export function RunSidebar({
  summaryURL,
  jobs,
  current,
  title: titleProp,
}: {
  summaryURL: string
  jobs: readonly JobView[]
  current: 'summary' | number
  title?: string
}) {
  const t = useT()
  const { locale } = useLocale()
  const title = titleProp ?? t('run.jobs')
  const cur = current === 'summary' ? null : jobs.find((j) => j.ID === current)
  return (
    <>
      {/* 窄屏：收在菜单中，不把标题推到第一屏以下 */}
      <div className={css.menu}>
        <ActionMenu>
          <ActionMenu.Button
            block
            // 传元素而不是每次渲染新建的组件：否则每次定时刷新都会重新挂载图标，执行中的动画从头开始
            leadingVisual={cur ? <StatusIcon state={cur.State} /> : HomeIcon}
          >
            {cur ? t('run.sidebarCurrent', { title, name: cur.DisplayName }) : t('run.summary')}
          </ActionMenu.Button>
          <ActionMenu.Overlay width="medium">
            <ActionList aria-label={t('run.sidebar')}>
              <ActionList.LinkItem href={summaryURL} active={current === 'summary'} aria-current={current === 'summary' ? 'page' : undefined}>
                <ActionList.LeadingVisual>
                  <HomeIcon />
                </ActionList.LeadingVisual>
                {t('run.summary')}
              </ActionList.LinkItem>
              {jobs.length > 0 && (
                <ActionList.Group>
                  <ActionList.GroupHeading>{title}</ActionList.GroupHeading>
                  {jobs.map((j) => (
                    <ActionList.LinkItem key={j.ID} href={`/jobs/${j.ID}`} active={current === j.ID} aria-current={current === j.ID ? 'page' : undefined}>
                      <ActionList.LeadingVisual>
                        <StatusIcon state={j.State} label={jobState(j.State, locale)} />
                      </ActionList.LeadingVisual>
                      {j.DisplayName}
                    </ActionList.LinkItem>
                  ))}
                </ActionList.Group>
              )}
            </ActionList>
          </ActionMenu.Overlay>
        </ActionMenu>
      </div>
      <div className={css.side}>
        <NavList aria-label={t('run.sidebar')}>
          <NavList.Item href={summaryURL} aria-current={current === 'summary' ? 'page' : undefined}>
            <NavList.LeadingVisual>
              <HomeIcon />
            </NavList.LeadingVisual>
            {t('run.summary')}
          </NavList.Item>
          {jobs.length > 0 && (
            <NavList.Group title={title}>
              {jobs.map((j) => (
                <NavList.Item key={j.ID} href={`/jobs/${j.ID}`} aria-current={current === j.ID ? 'page' : undefined}>
                  <NavList.LeadingVisual>
                    <StatusIcon state={j.State} label={jobState(j.State, locale)} />
                  </NavList.LeadingVisual>
                  <span className={css.name} title={j.DisplayName}>
                    {j.DisplayName}
                  </span>
                </NavList.Item>
              ))}
            </NavList.Group>
          )}
        </NavList>
      </div>
    </>
  )
}
