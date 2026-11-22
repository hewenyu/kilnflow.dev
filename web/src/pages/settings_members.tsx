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
import { list } from '../lib/envelope'
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
  Source: z.string(),
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

function Members({ data }: PageProps<Data>) {
  return (
    <SettingsLayout section={data.Section}>
      <Subhead
        description={
          <>
            能登录 Dashboard 的 GitHub 账号。管理员可以修改设置、管理 Runner；成员可以查看、启动构建、重跑和取消。共{' '}
            <b className="num">{data.Admins}</b> 个管理员、<b className="num">{data.Users}</b> 个成员。
          </>
        }
      >
        成员
      </Subhead>
      <Box>
        {data.Members.length > 0 ? (
          <ul className={`${css.rows} ${css.members}`} aria-label="成员">
            {data.Members.map((m) => (
              <BoxRow as="li" key={m.Login}>
                <div className={css.person}>
                  <Avatar login={m.Login} size={32} />
                  <div>
                    <div className={css.personName}>
                      {m.Login}
                      {m.Me && <Label variant="accent">你</Label>}
                      {m.Role === 'admin' ? <Label variant="done">管理员</Label> : <Label>成员</Label>}
                    </div>
                    <p className={css.muted}>来自 {m.Source}</p>
                  </div>
                </div>
                <div className={css.cells}>
                  <span>
                    最近登录 <Ago t={m.LastLogin} fallback="从未登录" />
                  </span>
                  <span>
                    <LinkButton size="small" variant="invisible" href={`/settings/activity?actor=${encodeURIComponent(m.Login)}`} aria-label={`${m.Login} 的活动`}>
                      活动
                    </LinkButton>
                    <LinkButton size="small" variant="invisible" href={`/runs?actor=${encodeURIComponent(m.Login)}`} aria-label={`${m.Login} 发起的运行`}>
                      运行
                    </LinkButton>
                  </span>
                </div>
              </BoxRow>
            ))}
          </ul>
        ) : (
          <EmptyState icon={PeopleIcon} title="还没有成员">
            现在只能用 admin token 登录。先在 <a href="/settings/account">登录与身份</a> 中绑定你的 GitHub 账号。
          </EmptyState>
        )}
      </Box>
      <details className={css.help}>
        <summary>怎样添加或移除成员</summary>
        <p>
          成员名单在 Server 的配置中：<code>KILN_ADMINS</code>（管理员）和 <code>KILN_USERS</code>（成员），写 GitHub 登录名或数字 ID，逗号分隔，修改后重启
          Server 生效。绑定的 GitHub 账号
          {!data.Bound && (
            <>
              （在 <a href="/settings/account">登录与身份</a> 中绑定）
            </>
          )}
          自动是管理员，不需要写进名单。
        </p>
        <p>从名单中移除的账号，已经登录的会话在下一次请求时失效，不用等会话过期。</p>
      </details>
    </SettingsLayout>
  )
}

export default definePage(schema, Members)
