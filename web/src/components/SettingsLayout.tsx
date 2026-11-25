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
import { useT } from '../i18n/locale'
import type { MsgKey } from '../i18n/messages'
import css from './SettingsLayout.module.css'

/** 设置的分区（与服务端各设置页数据中的 Section 一致）。 */
export type Section = 'conn' | 'vars' | 'envs' | 'repos' | 'upgrade' | 'ops' | 'account' | 'members' | 'activity'

interface Item {
  section: Section
  href: string
  label: MsgKey
  icon: Icon
}

const groups: readonly { title: MsgKey; items: readonly Item[] }[] = [
  {
    title: 'settingsNav.groupBuild',
    items: [
      { section: 'conn', href: '/settings', label: 'settingsNav.conn', icon: PlugIcon },
      { section: 'vars', href: '/settings/variables', label: 'settingsNav.vars', icon: KeyAsteriskIcon },
      { section: 'envs', href: '/settings/environments', label: 'settingsNav.envs', icon: StackIcon },
      { section: 'repos', href: '/settings/repos', label: 'settingsNav.repos', icon: RepoIcon },
      { section: 'upgrade', href: '/settings/runner-upgrade', label: 'settingsNav.upgrade', icon: VersionsIcon },
      { section: 'ops', href: '/settings/ops', label: 'settingsNav.ops', icon: ToolsIcon },
    ],
  },
  {
    title: 'settingsNav.groupAccount',
    items: [
      { section: 'account', href: '/settings/account', label: 'settingsNav.account', icon: ShieldLockIcon },
      { section: 'members', href: '/settings/members', label: 'settingsNav.members', icon: PeopleIcon },
      { section: 'activity', href: '/settings/activity', label: 'settingsNav.activity', icon: HistoryIcon },
    ],
  },
]

/** sectionLabel 返回分区的名称键（面包屑等处用；不认识的分区返回 null）。 */
export function sectionLabel(section: string): MsgKey | null {
  return groups.flatMap((g) => g.items).find((i) => i.section === section)?.label ?? null
}

/** SettingsLayout 是设置页的布局：左侧分区导航，右侧 children。section 是当前分区。 */
export function SettingsLayout({ section, children }: { section: string; children: ReactNode }) {
  const t = useT()
  const current = groups.flatMap((g) => g.items).find((i) => i.section === section)
  return (
    <div className={css.layout}>
      {/* 窄屏：分区收在菜单中，不占页面高度 */}
      <div className={css.menu}>
        <ActionMenu>
          <ActionMenu.Button leadingVisual={current?.icon} block>
            {current ? t('settingsNav.current', { section: t(current.label) }) : t('settingsNav.label')}
          </ActionMenu.Button>
          <ActionMenu.Overlay width="medium">
            <ActionList aria-label={t('settingsNav.label')}>
              {groups.map((g) => (
                <ActionList.Group key={g.title}>
                  <ActionList.GroupHeading>{t(g.title)}</ActionList.GroupHeading>
                  {g.items.map((i) => (
                    <ActionList.LinkItem key={i.section} href={i.href} active={section === i.section} aria-current={section === i.section ? 'page' : undefined}>
                      <ActionList.LeadingVisual>
                        <i.icon />
                      </ActionList.LeadingVisual>
                      {t(i.label)}
                    </ActionList.LinkItem>
                  ))}
                </ActionList.Group>
              ))}
            </ActionList>
          </ActionMenu.Overlay>
        </ActionMenu>
      </div>
      <div className={css.side}>
        <NavList aria-label={t('settingsNav.label')}>
          {groups.map((g) => (
            <NavList.Group key={g.title} title={t(g.title)}>
              {g.items.map((i) => (
                <NavList.Item key={i.section} href={i.href} aria-current={section === i.section ? 'page' : undefined}>
                  <NavList.LeadingVisual>
                    <i.icon />
                  </NavList.LeadingVisual>
                  {t(i.label)}
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
