import {
  type Icon,
  HistoryIcon,
  KeyAsteriskIcon,
  PeopleIcon,
  PlugIcon,
  RepoIcon,
  ShieldLockIcon,
  StackIcon,
  ToolsIcon,
  VersionsIcon,
} from '@primer/octicons-react'
import { ActionList, ActionMenu, NavList } from '@primer/react'
import type { ReactNode } from 'react'
import css from './SettingsLayout.module.css'

/** 设置的分区（与服务端各设置页数据中的 Section 一致）。 */
export type Section = 'conn' | 'vars' | 'envs' | 'repos' | 'upgrade' | 'ops' | 'account' | 'members' | 'activity'

interface Item {
  section: Section
  href: string
  label: string
  icon: Icon
}

const groups: readonly { title: string; items: readonly Item[] }[] = [
  {
    title: '构建',
    items: [
      { section: 'conn', href: '/settings', label: '连接与发布凭据', icon: PlugIcon },
      { section: 'vars', href: '/settings/variables', label: '变量与密钥', icon: KeyAsteriskIcon },
      { section: 'envs', href: '/settings/environments', label: '环境', icon: StackIcon },
      { section: 'repos', href: '/settings/repos', label: '仓库', icon: RepoIcon },
      { section: 'upgrade', href: '/settings/runner-upgrade', label: 'Runner 升级', icon: VersionsIcon },
      { section: 'ops', href: '/settings/ops', label: '运维', icon: ToolsIcon },
    ],
  },
  {
    title: '账号',
    items: [
      { section: 'account', href: '/settings/account', label: '登录与身份', icon: ShieldLockIcon },
      { section: 'members', href: '/settings/members', label: '成员', icon: PeopleIcon },
      { section: 'activity', href: '/settings/activity', label: '活动', icon: HistoryIcon },
    ],
  },
]

/** SettingsLayout 是设置页的布局：左侧分区导航，右侧 children。section 是当前分区。 */
export function SettingsLayout({ section, children }: { section: string; children: ReactNode }) {
  const current = groups.flatMap((g) => g.items).find((i) => i.section === section)
  return (
    <div className={css.layout}>
      {/* 窄屏：分区收在菜单中，不占页面高度 */}
      <div className={css.menu}>
        <ActionMenu>
          <ActionMenu.Button leadingVisual={current?.icon} block>
            {current ? `设置：${current.label}` : '设置分区'}
          </ActionMenu.Button>
          <ActionMenu.Overlay width="medium">
            <ActionList aria-label="设置分区">
              {groups.map((g) => (
                <ActionList.Group key={g.title}>
                  <ActionList.GroupHeading>{g.title}</ActionList.GroupHeading>
                  {g.items.map((i) => (
                    <ActionList.LinkItem key={i.section} href={i.href} active={section === i.section} aria-current={section === i.section ? 'page' : undefined}>
                      <ActionList.LeadingVisual>
                        <i.icon />
                      </ActionList.LeadingVisual>
                      {i.label}
                    </ActionList.LinkItem>
                  ))}
                </ActionList.Group>
              ))}
            </ActionList>
          </ActionMenu.Overlay>
        </ActionMenu>
      </div>
      <div className={css.side}>
        <NavList aria-label="设置分区">
          {groups.map((g) => (
            <NavList.Group key={g.title} title={g.title}>
              {g.items.map((i) => (
                <NavList.Item key={i.section} href={i.href} aria-current={section === i.section ? 'page' : undefined}>
                  <NavList.LeadingVisual>
                    <i.icon />
                  </NavList.LeadingVisual>
                  {i.label}
                </NavList.Item>
              ))}
            </NavList.Group>
          ))}
        </NavList>
      </div>
      <div className={css.content}>{children}</div>
    </div>
  )
}
