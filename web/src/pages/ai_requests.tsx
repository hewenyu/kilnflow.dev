import { CodeReviewIcon, FilterIcon, SearchIcon } from '@primer/octicons-react'
import { ActionList, ActionMenu } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxFooter, BoxRow } from '../components/Box'
import boxCss from '../components/Box.module.css'
import { EmptyState } from '../components/EmptyState'
import { PageHeader } from '../components/PageHeader'
import { StatusIcon } from '../components/StatusIcon'
import { Ago } from '../components/Time'
import { Trans, useLocale, useT, useText } from '../i18n/locale'
import type { Text } from '../i18n/messages'
import { list, textSchema } from '../lib/envelope'
import { refName, short } from '../lib/format'
import { requestState, requestTitle } from './ai/parts'
import { aiRequestSchema, aiRequestURL, type AIRequest } from './ai/schema'
import css from './ai/ai.module.css'
import table from './settings_people.module.css'

// AI 请求列表（PLAN #30.9）：AI 审计和 AI 开发的请求，与 CI 的运行分开。按仓库、类型、状态筛选；有进行中的请求时自动刷新。

/** 筛选的选项：Label 是服务端给的消息。 */
const optionSchema = z.object({ Value: z.string(), Label: textSchema })

const schema = z.object({
  Requests: list(aiRequestSchema),
  /** 筛选条件（已校验，无效的取值视为不筛选）。 */
  Filter: z.object({ Repo: z.string(), Type: z.string(), State: z.string(), PR: z.number() }),
  Repos: list(z.string()),
  Types: list(optionSchema),
  States: list(optionSchema),
  /** 列出的最大条数。 */
  Limit: z.number(),
})
type Data = z.infer<typeof schema>
type Filter = Data['Filter']

/** aiRequestsURL 返回带筛选条件的 AI 请求列表地址（与服务端读取的参数相同）。 */
export function aiRequestsURL(f: Filter): string {
  const q = new URLSearchParams()
  if (f.Repo) q.set('repo', f.Repo)
  if (f.Type) q.set('type', f.Type)
  if (f.State) q.set('state', f.State)
  if (f.PR) q.set('pr', String(f.PR))
  const s = q.toString()
  return s ? `/ai/requests?${s}` : '/ai/requests'
}

const filtered = (f: Filter) => f.Repo !== '' || f.Type !== '' || f.State !== '' || f.PR !== 0

/** FilterMenu 是一个筛选条件的菜单：all 是不筛选时的文字，set 返回换成 v 后的筛选条件。 */
function FilterMenu({
  label,
  all,
  value,
  options,
  set,
}: {
  label: string
  all: string
  value: string
  options: { Value: string; Label: Text }[]
  set: (v: string) => Filter
}) {
  const t = useT()
  const text = useText()
  const cur = options.find((o) => o.Value === value)
  return (
    <ActionMenu>
      <ActionMenu.Button size="small" leadingVisual={FilterIcon}>
        {t('aiRequests.filterLabel', { label, value: cur ? text(cur.Label) : all })}
      </ActionMenu.Button>
      <ActionMenu.Overlay width="small">
        <ActionList selectionVariant="single">
          <ActionList.LinkItem href={aiRequestsURL(set(''))} active={value === ''} aria-checked={value === ''}>
            {all}
          </ActionList.LinkItem>
          {options.map((o) => (
            <ActionList.LinkItem key={o.Value} href={aiRequestsURL(set(o.Value))} active={value === o.Value} aria-checked={value === o.Value}>
              {text(o.Label)}
            </ActionList.LinkItem>
          ))}
        </ActionList>
      </ActionMenu.Overlay>
    </ActionMenu>
  )
}

/** subject 返回请求的对象：PR、工作项或 ref。 */
function Subject({ r }: { r: AIRequest }) {
  const t = useT()
  if (r.PRNumber) return <>PR #{r.PRNumber}</>
  if (r.WorkItemURL) return <a href={r.WorkItemURL}>{t('workItem.ref', { id: r.WorkItemID })}</a>
  return <span className="mono">{refName(r.Ref)}</span>
}

function AIRequests({ data }: PageProps<Data>) {
  const t = useT()
  const { locale } = useLocale()
  const f = data.Filter
  const col = {
    request: t('aiRequests.colRequest'),
    state: t('aiRequests.colState'),
    repo: t('aiRequests.colRepo'),
    subject: t('aiRequests.colSubject'),
    created: t('aiRequests.colCreated'),
  }
  useLive(data.Requests.some((r) => r.State === 'queued' || r.State === 'active'))
  return (
    <div className={css.page}>
      <PageHeader
        title={t('aiRequests.pageTitle')}
        description={
          <Trans
            id="aiRequests.description"
            tags={{ runners: (c) => <a href="/ai/runners">{c}</a>, runs: (c) => <a href="/runs">{c}</a> }}
          />
        }
      />
      <Box>
        <h2 className="visually-hidden">{t('aiRequests.listHeading')}</h2>
        <div className={`${boxCss.header ?? ''} ${css.filters}`}>
          <FilterMenu label={t('aiRequests.type')} all={t('aiRequests.allTypes')} value={f.Type} options={data.Types} set={(v) => ({ ...f, Type: v })} />
          <FilterMenu label={t('aiRequests.state')} all={t('aiRequests.allStates')} value={f.State} options={data.States} set={(v) => ({ ...f, State: v })} />
          {data.Repos.length > 0 && (
            <FilterMenu
              label={t('aiRequests.repo')}
              all={t('aiRequests.allRepos')}
              value={f.Repo}
              options={data.Repos.map((r) => ({ Value: r, Label: r }))}
              set={(v) => ({ ...f, Repo: v })}
            />
          )}
        </div>
        {filtered(f) && (
          <BoxRow>
            <span>
              {f.PR !== 0
                ? t('aiRequests.filteredPR', { pr: f.PR, count: data.Requests.length })
                : t('aiRequests.filtered', { count: data.Requests.length })}
            </span>{' '}
            <a href="/ai/requests">{t('aiRequests.clear')}</a>
          </BoxRow>
        )}
        {data.Requests.length > 0 ? (
          <table className={table.table} aria-label={t('aiRequests.pageTitle')}>
            <thead>
              <tr>
                <th scope="col">{col.request}</th>
                <th scope="col">{col.state}</th>
                <th scope="col">{col.repo}</th>
                <th scope="col">{col.subject}</th>
                <th scope="col">commit</th>
                <th scope="col">{col.created}</th>
              </tr>
            </thead>
            <tbody>
              {data.Requests.map((r) => (
                <tr key={r.ID}>
                  <td data-label={col.request}>
                    <a href={aiRequestURL(r.ID)}>{requestTitle(r, locale)}</a>
                    {r.RerunOf !== 0 && <span className={css.muted}>{t('aiRequests.rerunOf', { id: r.RerunOf })}</span>}
                  </td>
                  <td data-label={col.state}>
                    <span className={css.state}>
                      <StatusIcon state={r.State} size={14} />
                      {requestState(r.State, locale)}
                    </span>
                  </td>
                  <td data-label={col.repo}>{r.Repo}</td>
                  <td data-label={col.subject}>
                    <Subject r={r} />
                  </td>
                  <td data-label="commit" className={table.mono}>
                    {short(r.SHA)}
                  </td>
                  <td data-label={col.created} className={table.nowrap}>
                    <Ago t={r.Created} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : filtered(f) ? (
          <EmptyState icon={SearchIcon} title={t('aiRequests.noMatch')} secondary={{ label: t('aiRequests.clear'), href: '/ai/requests' }} />
        ) : (
          <EmptyState icon={CodeReviewIcon} title={t('aiRequests.empty')}>
            <Trans id="aiRequests.emptyBody" tags={{ code: (c) => <code>{c}</code> }} />
          </EmptyState>
        )}
        {data.Requests.length >= data.Limit && <BoxFooter>{t('aiRequests.limit', { limit: data.Limit })}</BoxFooter>}
      </Box>
    </div>
  )
}

export default definePage(schema, AIRequests)
