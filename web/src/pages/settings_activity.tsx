import { FilterIcon, HistoryIcon } from '@primer/octicons-react'
import { ActionList, ActionMenu, LinkButton } from '@primer/react'
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

// 活动：审计日志，按时间倒序，可以按操作者筛选。

const entrySchema = z.object({
  ID: z.number(),
  Actor: z.string(),
  Action: z.string(),
  Detail: z.record(z.string(), z.unknown()).nullable(),
  At: z.string(),
  /** 中文说明（不含操作者）和相关页面的地址（没有时为空）。 */
  Text: z.string(),
  Link: z.string(),
})

const schema = z.object({
  Section: z.string(),
  /** 按操作者筛选的条件（为空表示全部）。 */
  Actor: z.string(),
  Actors: list(z.string()),
  Entries: list(entrySchema),
  /** 下一页（更早）的地址；回到最新一页的地址（在第一页时为空）。 */
  OlderURL: z.string(),
  LatestURL: z.string(),
})
type Data = z.infer<typeof schema>

const actorURL = (a: string) => (a ? `/settings/activity?actor=${encodeURIComponent(a)}` : '/settings/activity')

function Activity({ data }: PageProps<Data>) {
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description="谁在什么时候做了什么：登录、启动和取消构建、重跑、Runner 操作和配置修改。多人使用时，在这里确认某个变化是谁做的。">活动</Subhead>
      <div className={css.toolbar} role="search" aria-label="筛选活动">
        <ActionMenu>
          <ActionMenu.Button leadingVisual={FilterIcon}>操作者：{data.Actor || '全部'}</ActionMenu.Button>
          <ActionMenu.Overlay width="small">
            <ActionList selectionVariant="single" aria-label="操作者">
              <ActionList.LinkItem href={actorURL('')} active={data.Actor === ''} aria-checked={data.Actor === ''}>
                全部
              </ActionList.LinkItem>
              {data.Actors.map((a) => (
                <ActionList.LinkItem key={a} href={actorURL(a)} active={a === data.Actor} aria-checked={a === data.Actor}>
                  {a}
                </ActionList.LinkItem>
              ))}
            </ActionList>
          </ActionMenu.Overlay>
        </ActionMenu>
        {data.Actor && (
          <LinkButton variant="invisible" href="/settings/activity">
            清空筛选
          </LinkButton>
        )}
      </div>
      <Box>
        {data.Entries.length > 0 ? (
          <ul className={css.rows} aria-label="活动">
            {data.Entries.map((e) => (
              <BoxRow as="li" key={e.ID} className={css.act}>
                <Avatar login={e.Actor} size={20} />
                <div className={css.actText}>
                  <b>{e.Actor}</b> {e.Link ? <a href={e.Link}>{e.Text}</a> : e.Text}
                </div>
                <span className={css.actAt}>
                  <Ago t={e.At} />
                </span>
              </BoxRow>
            ))}
          </ul>
        ) : (
          <EmptyState icon={HistoryIcon} title="没有活动">
            {data.Actor ? `${data.Actor} 还没有记录在案的操作。` : '还没有任何操作记录。'}
          </EmptyState>
        )}
      </Box>
      {(data.OlderURL || data.LatestURL) && (
        <nav className={css.pager} aria-label="分页">
          {data.LatestURL && <LinkButton href={data.LatestURL}>回到最新</LinkButton>}
          {data.OlderURL && (
            <LinkButton href={data.OlderURL} rel="next">
              更早的活动
            </LinkButton>
          )}
        </nav>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, Activity)
