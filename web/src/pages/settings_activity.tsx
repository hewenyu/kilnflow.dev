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
import { Msg, useT } from '../i18n/locale'
import { list, textSchema } from '../lib/envelope'
import css from './settings_people.module.css'

// 活动：审计日志，按时间倒序，可以按操作者筛选。

const entrySchema = z.object({
  ID: z.number(),
  Actor: z.string(),
  Action: z.string(),
  Detail: z.record(z.string(), z.unknown()).nullable(),
  At: z.string(),
  /** 说明（不含操作者）和相关页面的地址（没有时为空）。 */
  Text: textSchema,
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
  const t = useT()
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description={t('activityPage.description')}>{t('settingsNav.activity')}</Subhead>
      <div className={css.toolbar} role="search" aria-label={t('activityPage.filterLabel')}>
        <ActionMenu>
          <ActionMenu.Button leadingVisual={FilterIcon}>{t('activityPage.actor', { actor: data.Actor || t('activityPage.all') })}</ActionMenu.Button>
          <ActionMenu.Overlay width="small">
            <ActionList selectionVariant="single" aria-label={t('activityPage.actorList')}>
              <ActionList.LinkItem href={actorURL('')} active={data.Actor === ''} aria-checked={data.Actor === ''}>
                {t('activityPage.all')}
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
            {t('activityPage.clear')}
          </LinkButton>
        )}
      </div>
      <Box>
        {data.Entries.length > 0 ? (
          <ul className={css.rows} aria-label={t('activityPage.listLabel')}>
            {data.Entries.map((e) => (
              <BoxRow as="li" key={e.ID} className={css.act}>
                <Avatar login={e.Actor} size={20} />
                <div className={css.actText}>
                  <b>{e.Actor}</b> {e.Link ? (
                    <a href={e.Link}>
                      <Msg text={e.Text} />
                    </a>
                  ) : (
                    <Msg text={e.Text} />
                  )}
                </div>
                <span className={css.actAt}>
                  <Ago t={e.At} />
                </span>
              </BoxRow>
            ))}
          </ul>
        ) : (
          <EmptyState icon={HistoryIcon} title={t('activityPage.emptyTitle')}>
            {data.Actor ? t('activityPage.emptyActor', { actor: data.Actor }) : t('activityPage.emptyAll')}
          </EmptyState>
        )}
      </Box>
      {(data.OlderURL || data.LatestURL) && (
        <nav className={css.pager} aria-label={t('pager.label')}>
          {data.LatestURL && <LinkButton href={data.LatestURL}>{t('activityPage.latest')}</LinkButton>}
          {data.OlderURL && (
            <LinkButton href={data.OlderURL} rel="next">
              {t('activityPage.older')}
            </LinkButton>
          )}
        </nav>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, Activity)
