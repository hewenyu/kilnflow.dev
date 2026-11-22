import { InboxIcon, PlayIcon, SearchIcon } from '@primer/octicons-react'
import { Button, FormControl, LinkButton, Select, TextInput } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { liveSeconds, useLive } from '../app/live'
import { Box, BoxHeader } from '../components/Box'
import { EmptyState } from '../components/EmptyState'
import { PageHeader } from '../components/PageHeader'
import { Form } from '../components/Form'
import { RunRow } from '../components/RunRow'
import { list } from '../lib/envelope'
import { runState } from '../lib/format'
import { runViewSchema } from '../lib/schemas'
import css from './runs.module.css'

// 运行列表（/runs）：按仓库、状态、workflow、ref、发起人筛选；条件在地址中，可以分享。每 5 秒自动更新。

const filterSchema = z.object({
  Repo: z.string(),
  State: z.string(),
  Workflow: z.string(),
  Ref: z.string(),
  Actor: z.string(),
  /** 设置了任一筛选条件。 */
  Active: z.boolean(),
})
type Filter = z.infer<typeof filterSchema>

const schema = z.object({
  Filter: filterSchema,
  /** 与筛选中的仓库全名完全相同的仓库；为空表示按子串匹配或没有按仓库筛选。 */
  RepoExact: z.string(),
  Runs: list(runViewSchema),
  States: list(z.object({ Value: z.string(), Label: z.string() })),
  Workflows: list(z.string()),
  /** 不是最新一页；NewestID、OldestID 是本页第一个和最后一个 run。 */
  Paged: z.boolean(),
  NewestID: z.number(),
  OldestID: z.number(),
  NewerURL: z.string(),
  OlderURL: z.string(),
  LatestURL: z.string(),
  Self: z.string(),
  /** 启动构建的地址（按仓库筛选时预选仓库）。 */
  NewURL: z.string(),
  GitHub: z.string(),
  /** 当前用户的 GitHub 登录名；还没有绑定 GitHub 账号的 admin token 登录为空。 */
  Me: z.string(),
  AllURL: z.string(),
  MineURL: z.string(),
  /** 正在看"我发起的"。 */
  Mine: z.boolean(),
})
type Data = z.infer<typeof schema>

/** filterSummary 返回筛选条件的说明，例如"已筛选：仓库 tester/demo，状态 失败。" */
export function filterSummary(f: Filter, repoExact: string): string {
  if (!f.Active) return ''
  const parts: string[] = []
  if (repoExact) parts.push(`仓库 ${repoExact}`)
  else if (f.Repo) parts.push(`仓库名包含"${f.Repo}"`)
  if (f.State) parts.push(`状态 ${runState(f.State)}`)
  if (f.Workflow) parts.push(`workflow ${f.Workflow}`)
  if (f.Ref) parts.push(`ref ${f.Ref}`)
  if (f.Actor) parts.push(`发起人 ${f.Actor}`)
  return `已筛选${parts.length > 0 ? '：' + parts.join('，') : ''}。`
}

function FilterForm({ data }: { data: Data }) {
  const f = data.Filter
  const repoHint = data.RepoExact ? `只看 ${data.RepoExact}` : f.Repo ? `仓库名包含"${f.Repo}"` : '完整名称或其中一部分'
  return (
    <Form action="/runs" method="get" className={css.filter} aria-label="筛选构建">
      <FormControl>
        <FormControl.Label>仓库</FormControl.Label>
        <TextInput type="search" name="repo" defaultValue={f.Repo} placeholder="owner/name 或其中一部分" spellCheck={false} autoCapitalize="off" size="small" block />
        <FormControl.Caption className="visually-hidden">{repoHint}</FormControl.Caption>
      </FormControl>
      <FormControl>
        <FormControl.Label>状态</FormControl.Label>
        <Select name="state" defaultValue={f.State} size="small" block>
          <Select.Option value="">全部状态</Select.Option>
          {data.States.map((s) => (
            <Select.Option key={s.Value} value={s.Value}>
              {s.Label}
            </Select.Option>
          ))}
        </Select>
      </FormControl>
      <FormControl>
        <FormControl.Label>workflow</FormControl.Label>
        <TextInput type="search" name="workflow" defaultValue={f.Workflow} list="runs-workflows" placeholder="例如 ci.yml" spellCheck={false} autoCapitalize="off" size="small" block />
        <datalist id="runs-workflows">
          {data.Workflows.map((w) => (
            <option key={w} value={w} />
          ))}
        </datalist>
      </FormControl>
      <FormControl>
        <FormControl.Label>ref</FormControl.Label>
        <TextInput type="search" name="ref" defaultValue={f.Ref} placeholder="main 或 v1.2.0" spellCheck={false} autoCapitalize="off" size="small" block />
      </FormControl>
      <FormControl>
        <FormControl.Label>发起人</FormControl.Label>
        <TextInput type="search" name="actor" defaultValue={f.Actor} placeholder="GitHub 登录名" spellCheck={false} autoCapitalize="off" size="small" block />
      </FormControl>
      <div className={css.filterActions}>
        <Button type="submit" size="small" leadingVisual={SearchIcon}>
          筛选
        </Button>
        {f.Active && (
          <LinkButton href="/runs" size="small" variant="invisible">
            清空筛选
          </LinkButton>
        )}
      </div>
    </Form>
  )
}

function Empty({ data }: { data: Data }) {
  if (data.Paged) {
    return (
      <EmptyState icon={InboxIcon} title="这个方向上没有更多构建了" action={{ label: '回到最新', href: data.LatestURL }} />
    )
  }
  if (data.Filter.Active) {
    return (
      <EmptyState
        icon={SearchIcon}
        title="没有符合筛选条件的构建"
        action={data.RepoExact ? { label: `为 ${data.RepoExact} 启动构建`, href: data.NewURL } : undefined}
        secondary={{ label: '清空筛选', href: '/runs' }}
      >
        筛选会查找全部历史，不只是最近的构建。
      </EmptyState>
    )
  }
  return (
    <EmptyState icon={PlayIcon} title="还没有构建">
      推送代码、开 PR 或推送 tag 后会自动开始，也可以用右上角的“启动构建”手动运行。
    </EmptyState>
  )
}

function Runs({ data }: PageProps<Data>) {
  useLive(true)
  const f = data.Filter
  const summary = filterSummary(f, data.RepoExact)
  const count = data.Runs.length === 0 ? '' : data.Paged ? `显示 #${data.NewestID} 到 #${data.OldestID}` : `最新的 ${data.Runs.length} 个构建`
  // 按仓库筛选（或只有一个仓库）时不在每行重复仓库名
  const showRepo = data.RepoExact === ''
  return (
    <div className={css.page}>
      <PageHeader
        title="运行"
        description="每次推送、PR、tag 和手动触发都会生成一个 run；筛选条件保存在地址中，可以直接分享。"
        actions={
          <LinkButton href={data.NewURL} variant="primary" leadingVisual={PlayIcon}>
            启动构建
          </LinkButton>
        }
      />

      {/* 从来没有构建时筛选没有意义 */}
      {(data.Runs.length > 0 || f.Active || data.Paged) && <FilterForm data={data} />}

      <Box>
        <BoxHeader
          title={
            data.Me ? (
              <nav className={css.scope} aria-label="谁发起的">
                <a href={data.AllURL} aria-current={f.Actor === '' ? 'page' : undefined}>
                  全部
                </a>
                <a href={data.MineURL} aria-current={data.Mine ? 'page' : undefined}>
                  我发起的
                </a>
              </nav>
            ) : (
              '构建'
            )
          }
        >
          <span className={css.count}>
            {summary}
            {count && `${count}，每 ${liveSeconds} 秒自动更新。`}
          </span>
        </BoxHeader>
        {data.Runs.length > 0 ? (
          <div role="list" aria-label="构建">
            {data.Runs.map((r) => (
              <RunRow key={r.ID} run={r} showRepo={showRepo} />
            ))}
          </div>
        ) : (
          <Empty data={data} />
        )}
      </Box>

      {(data.NewerURL || data.OlderURL || data.Paged) && (
        <nav className={css.pager} aria-label="分页">
          {data.NewerURL && (
            <LinkButton href={data.NewerURL} rel="prev" size="small">
              较新
            </LinkButton>
          )}
          {data.Paged && (
            <LinkButton href={data.LatestURL} size="small" variant="invisible">
              最新
            </LinkButton>
          )}
          {data.OlderURL && (
            <LinkButton href={data.OlderURL} rel="next" size="small">
              较早
            </LinkButton>
          )}
        </nav>
      )}
    </div>
  )
}

export default definePage(schema, Runs)
