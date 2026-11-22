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
import { list } from '../lib/envelope'
import { refName, short } from '../lib/format'
import { requestState } from './ai/parts'
import { aiRequestSchema, aiRequestURL, type AIRequest } from './ai/schema'
import css from './ai/ai.module.css'
import table from './settings_people.module.css'

// AI 请求列表（PLAN #30.9）：AI 审计和 AI 开发的请求，与 CI 的运行分开。按仓库、类型、状态筛选；有进行中的请求时自动刷新。

const optionSchema = z.object({ Value: z.string(), Label: z.string() })

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
  options: { Value: string; Label: string }[]
  set: (v: string) => Filter
}) {
  const cur = options.find((o) => o.Value === value)
  return (
    <ActionMenu>
      <ActionMenu.Button size="small" leadingVisual={FilterIcon}>
        {label}：{cur ? cur.Label : all}
      </ActionMenu.Button>
      <ActionMenu.Overlay width="small">
        <ActionList selectionVariant="single">
          <ActionList.LinkItem href={aiRequestsURL(set(''))} active={value === ''} aria-checked={value === ''}>
            {all}
          </ActionList.LinkItem>
          {options.map((o) => (
            <ActionList.LinkItem key={o.Value} href={aiRequestsURL(set(o.Value))} active={value === o.Value} aria-checked={value === o.Value}>
              {o.Label}
            </ActionList.LinkItem>
          ))}
        </ActionList>
      </ActionMenu.Overlay>
    </ActionMenu>
  )
}

/** subject 返回请求的对象：PR、工作项或 ref。 */
function Subject({ r }: { r: AIRequest }) {
  if (r.PRNumber) return <>PR #{r.PRNumber}</>
  if (r.WorkItemURL) return <a href={r.WorkItemURL}>工作项 #{r.WorkItemID}</a>
  return <span className="mono">{refName(r.Ref)}</span>
}

function AIRequests({ data }: PageProps<Data>) {
  const f = data.Filter
  useLive(data.Requests.some((r) => r.State === 'queued' || r.State === 'active'))
  return (
    <div className={css.page}>
      <PageHeader
        title="AI 请求"
        description={
          <>
            AI 审计和 AI 开发的请求，每个请求按平台分为 AI 任务，由 <a href="/ai/runners">AI 执行节点</a> 执行。CI 的构建在 <a href="/runs">运行</a> 中。
          </>
        }
      />
      <Box>
        <h2 className="visually-hidden">AI 请求列表</h2>
        <div className={`${boxCss.header ?? ''} ${css.filters}`}>
          <FilterMenu label="类型" all="全部类型" value={f.Type} options={data.Types} set={(v) => ({ ...f, Type: v })} />
          <FilterMenu label="状态" all="全部状态" value={f.State} options={data.States} set={(v) => ({ ...f, State: v })} />
          {data.Repos.length > 0 && (
            <FilterMenu
              label="仓库"
              all="全部仓库"
              value={f.Repo}
              options={data.Repos.map((r) => ({ Value: r, Label: r }))}
              set={(v) => ({ ...f, Repo: v })}
            />
          )}
        </div>
        {filtered(f) && (
          <BoxRow>
            <span>
              已筛选{f.PR !== 0 && `，PR #${f.PR}`}；显示 {data.Requests.length} 个。
            </span>{' '}
            <a href="/ai/requests">清空筛选</a>
          </BoxRow>
        )}
        {data.Requests.length > 0 ? (
          <table className={table.table} aria-label="AI 请求">
            <thead>
              <tr>
                <th scope="col">请求</th>
                <th scope="col">状态</th>
                <th scope="col">仓库</th>
                <th scope="col">对象</th>
                <th scope="col">commit</th>
                <th scope="col">创建</th>
              </tr>
            </thead>
            <tbody>
              {data.Requests.map((r) => (
                <tr key={r.ID}>
                  <td data-label="请求">
                    <a href={aiRequestURL(r.ID)}>
                      {r.TypeLabel}请求 #{r.ID}
                    </a>
                    {r.RerunOf !== 0 && <span className={css.muted}>（重跑 #{r.RerunOf}）</span>}
                  </td>
                  <td data-label="状态">
                    <span className={css.state}>
                      <StatusIcon state={r.State} size={14} />
                      {requestState(r.State)}
                    </span>
                  </td>
                  <td data-label="仓库">{r.Repo}</td>
                  <td data-label="对象">
                    <Subject r={r} />
                  </td>
                  <td data-label="commit" className={table.mono}>
                    {short(r.SHA)}
                  </td>
                  <td data-label="创建" className={table.nowrap}>
                    <Ago t={r.Created} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : filtered(f) ? (
          <EmptyState icon={SearchIcon} title="没有符合筛选条件的 AI 请求" secondary={{ label: '清空筛选', href: '/ai/requests' }} />
        ) : (
          <EmptyState icon={CodeReviewIcon} title="还没有 AI 请求">
            仓库配置了 <code>.kiln/agent/config.yml</code> 后，PR 会触发 AI 审计；加了 <code>kiln:ready</code> 的 Issue 会触发 AI 开发。
          </EmptyState>
        )}
        {data.Requests.length >= data.Limit && <BoxFooter>只列出最近的 {data.Limit} 个，更早的请用筛选条件查找。</BoxFooter>}
      </Box>
    </div>
  )
}

export default definePage(schema, AIRequests)
