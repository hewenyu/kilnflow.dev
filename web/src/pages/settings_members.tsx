import { PeopleIcon } from '@primer/octicons-react'
import { Label, LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Avatar } from '../components/Avatar'
import { Box, BoxRow } from '../components/Box'
import { EmptyState } from '../components/EmptyState'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
import { Msg, Trans, useT } from '../i18n/locale'
import { list, textSchema } from '../lib/envelope'
import { toDate } from '../lib/format'
import { settingsTags, withLink } from './settings/common'
import css from './settings_people.module.css'

// 成员：谁能登录 Dashboard、是什么角色、最近一次登录。名单来自 Server 配置，这里只读。

const memberSchema = z.object({
  /** GitHub 登录名；名单中写的是数字 ID 时为 ID。 */
  Login: z.string(),
  /** admin 或 user */
  Role: z.string(),
  /** 管理员绑定的账号。 */
  Bound: z.boolean(),
  /** 权限来自哪里（绑定的 GitHub 账号、KILN_ADMINS、KILN_USERS）。 */
  Source: textSchema,
  /** 最近一次登录的时间，从未登录时为零值。 */
  LastLogin: z.string(),
  /** 当前登录的用户。 */
  Me: z.boolean(),
})

const schema = z.object({
  Section: z.string(),
  Members: list(memberSchema),
  Admins: z.number(),
  Users: z.number(),
  /** 管理员已经绑定 GitHub 账号。 */
  Bound: z.boolean(),
})
type Data = z.infer<typeof schema>

const numTags = { ...settingsTags, b: (c: string) => <b className="num">{c}</b> }
const accountLink = withLink('/settings/account')

function Members({ data }: PageProps<Data>) {
  const t = useT()
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description={<Trans id="membersPage.description" params={{ admins: data.Admins, users: data.Users }} tags={numTags} />}>
        {t('settingsNav.members')}
      </Subhead>
      <Box>
        {data.Members.length > 0 ? (
          <ul className={`${css.rows} ${css.members}`} aria-label={t('membersPage.listLabel')}>
            {data.Members.map((m) => (
              <BoxRow as="li" key={m.Login}>
                <div className={css.person}>
                  <Avatar login={m.Login} size={32} />
                  <div>
                    <div className={css.personName}>
                      {m.Login}
                      {m.Me && <Label variant="accent">{t('membersPage.you')}</Label>}
                      {m.Role === 'admin' ? <Label variant="done">{t('role.admin')}</Label> : <Label>{t('role.member')}</Label>}
                    </div>
                    <p className={css.muted}>
                      {t('membersPage.from')} <Msg text={m.Source} />
                    </p>
                  </div>
                </div>
                <div className={css.cells}>
                  <span>
                    {toDate(m.LastLogin) ? (
                      <Trans id="membersPage.lastLogin" tags={{ ago: () => <Ago t={m.LastLogin} /> }} />
                    ) : (
                      t('membersPage.never')
                    )}
                  </span>
                  <span>
                    <LinkButton
                      size="small"
                      variant="invisible"
                      href={`/settings/activity?actor=${encodeURIComponent(m.Login)}`}
                      aria-label={t('membersPage.activityLabel', { login: m.Login })}
                    >
                      {t('membersPage.activity')}
                    </LinkButton>
                    <LinkButton
                      size="small"
                      variant="invisible"
                      href={`/runs?actor=${encodeURIComponent(m.Login)}`}
                      aria-label={t('membersPage.runsLabel', { login: m.Login })}
                    >
                      {t('membersPage.runs')}
                    </LinkButton>
                  </span>
                </div>
              </BoxRow>
            ))}
          </ul>
        ) : (
          <EmptyState icon={PeopleIcon} title={t('membersPage.emptyTitle')}>
            <Trans id="membersPage.emptyText" tags={accountLink} />
          </EmptyState>
        )}
      </Box>
      <details className={css.help}>
        <summary>{t('membersPage.help')}</summary>
        <p>
          <Trans id={data.Bound ? 'membersPage.helpListBound' : 'membersPage.helpListUnbound'} tags={accountLink} />
        </p>
        <p>{t('membersPage.helpRemoved')}</p>
      </details>
    </SettingsLayout>
  )
}

export default definePage(schema, Members)
