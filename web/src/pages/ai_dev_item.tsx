import { ArrowLeftIcon } from '@primer/octicons-react'
import { Label, LinkButton } from '@primer/react'
import type { ReactNode } from 'react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxHeader, BoxRow } from '../components/Box'
import { PageHeader } from '../components/PageHeader'
import { Ago } from '../components/Time'
import { aiDevItemSchema, fixRounds, resolution, workItemMode, workItemPhase, workItemState } from '../lib/aidev'
import { list } from '../lib/envelope'
import { clock, short } from '../lib/format'
import { PRLink, RetryForm, StopForm } from './ai_dev'
import { DesignBasis, RelatedTasksTable } from './ai/DesignBasis'
import { designBasisSchema, relatedTaskSchema } from './ai/docs'
import { AgentSays, ReportBox, reportSchema, unknownText } from './ai/report'
import css from './ai_dev_item.module.css'
import { lookup } from '../lib/lookup'

// 工作项详情（PLAN #26.15）：概要、Issue、选题和交付报告（Agent 自报的内容，附适用范围）、评论意图（C19 至多发一次）、
// attempt 分支（推进后或保留期满后删除）。

const commentSchema = z.object({
  /** Issue 或 PR 编号。 */
  Number: z.number(),
  Kind: z.string(),
  /** pending、attempted、done、unknown、failed。 */
  State: z.string(),
  Error: z.string(),
  Updated: z.string(),
})
type Comment = z.infer<typeof commentSchema>

const branchSchema = z.object({
  Branch: z.string(),
  SHA: z.string(),
  /** delivery 或 checkpoint。 */
  Kind: z.string(),
  Created: z.string(),
  /** 删除的时间；还没有删除时为 null，Expires 是保留期满（届时删除）的时间。 */
  Deleted: z.string().nullable(),
  Expires: z.string().nullable(),
})
type Branch = z.infer<typeof branchSchema>

const schema = z.object({
  Item: aiDevItemSchema,
  BaseRef: z.string(),
  BaseSHA: z.string(),
  /** 选题时 Agent 给出的计划。 */
  Plan: z.string(),
  /** 当前（或最近一次）为它执行的 AI 任务的详情页；没有时为空。 */
  TaskURL: z.string(),
  DeliveryBranch: z.string(),
  DeliverySHA: z.string(),
  FixedHead: z.string(),
  Reports: list(reportSchema),
  Comments: list(commentSchema),
  Branches: list(branchSchema),
  RepoURL: z.string(),
  Ready: z.string(),
  HoldLabel: z.string(),
  GitHub: z.string(),
  /** 工作项冻结的设计依据；历史页面的数据没有这一项时为 null。 */
  DesignBasis: designBasisSchema.nullable().default(null),
  /** 与这个工作项关联的开发、返修和审计任务及依据关系。 */
  RelatedTasks: list(relatedTaskSchema).default([]),
})
type Data = z.infer<typeof schema>

function Commit({ data, sha, label }: { data: Data; sha: string; label: string }) {
  if (!sha) return null
  return data.GitHub ? (
    <a href={`${data.GitHub}/${data.Item.Repo}/commit/${sha}`} rel="noreferrer" className="mono" title={`${label} ${sha}`}>
      {short(sha)}
    </a>
  ) : (
    <code title={`${label} ${sha}`}>{short(sha)}</code>
  )
}

function KV({ k, children }: { k: string; children: ReactNode }) {
  return (
    <>
      <dt>{k}</dt>
      <dd>{children}</dd>
    </>
  )
}

function Summary({ data }: { data: Data }) {
  const it = data.Item
  const st = workItemState(it.State)
  const rounds = fixRounds(it)
  const phase = workItemPhase(it)
  return (
    <Box as="section" aria-labelledby="ai-dev-summary">
      <BoxHeader id="ai-dev-summary" title="概要" />
      <BoxRow className={css.block}>
        <div className={css.labels}>
          {phase && <Label variant="secondary">{phase}</Label>}
          <Label variant={st.variant}>{st.text}</Label>
          {it.Hold && <Label variant="attention">{data.HoldLabel}</Label>}
          {it.RequirementsChanged && <Label variant="attention">需求已变</Label>}
        </div>
        {it.Reason && <p className={it.State === 'blocked' ? css.reason : css.p}>{it.Reason}</p>}
        <dl className={css.kv}>
          <KV k="仓库">
            <a href={data.RepoURL}>{it.Repo}</a>
          </KV>
          <KV k="方式">{workItemMode(it)}</KV>
          {it.DesignPR > 0 && (
            <KV k="已合并的设计">
              <PRLink github={data.GitHub} repo={it.Repo} pr={it.DesignPR} />
            </KV>
          )}
          <KV k="目标分支">
            <code>{data.BaseRef}</code> · 起点 <Commit data={data} sha={data.BaseSHA} label="本轮开始时目标分支的 head" />
          </KV>
          <KV k="本轮的正式分支">
            <code>{it.Branch}</code>
            {it.HeadSHA && (
              <>
                {' '}
                · head <Commit data={data} sha={it.HeadSHA} label="head" />
              </>
            )}
          </KV>
          <KV k="PR">{it.PR ? <PRLink github={data.GitHub} repo={it.Repo} pr={it.PR} /> : '还没有开 PR'}</KV>
          {data.DeliveryBranch && (
            <KV k="待推进的交付">
              <code>{data.DeliveryBranch}</code> · <Commit data={data} sha={data.DeliverySHA} label="交付的 commit" />
            </KV>
          )}
          <KV k="本轮返修">{rounds || `还没有返修（最多 ${it.MaxFixRounds} 轮）`}</KV>
          {data.FixedHead && (
            <KV k="最近返修的 head">
              <Commit data={data} sha={data.FixedHead} label="已经为它建过返修的 head" />
            </KV>
          )}
          <KV k="Agent">{it.Agent || unknownText}</KV>
          {data.TaskURL && (
            <KV k="当前 AI 任务">
              <a href={data.TaskURL}>{data.TaskURL.replace('/ai/tasks/', '#')}</a>
            </KV>
          )}
          <KV k="时间">
            创建于 <Ago t={it.Created} /> · 更新于 <Ago t={it.Updated} />
            {it.Finished && (
              <>
                {' '}
                · 结束于 <Ago t={it.Finished} />
              </>
            )}
          </KV>
        </dl>
      </BoxRow>
    </Box>
  )
}

function Issues({ data }: { data: Data }) {
  return (
    <Box as="section" aria-labelledby="ai-dev-issues">
      <BoxHeader id="ai-dev-issues" title="Issue" />
      {data.Item.Issues.length === 0 && <BoxRow className={css.muted}>没有 Issue。</BoxRow>}
      {data.Item.Issues.map((is) => (
        <BoxRow key={is.Number} className={css.block}>
          {data.GitHub ? (
            <a href={`${data.GitHub}/${data.Item.Repo}/issues/${is.Number}`} rel="noreferrer" className={css.strong}>
              #{is.Number}
            </a>
          ) : (
            <b>#{is.Number}</b>
          )}{' '}
          {is.Title}
          <span className={css.muted}>
            {is.Resolution && ` · ${resolution(is.Resolution)}（Agent 自报）`}
            {is.Closed && ' · 已关闭'}
            {!is.Active && ' · 已释放'}
          </span>
        </BoxRow>
      ))}
    </Box>
  )
}

const commentKinds: Record<string, string> = {
  claimed: '领取说明',
  blocked: '受阻说明',
  dispositions: '审计问题的处置',
  stopped: '终止说明',
  no_change: '不需要改代码的说明',
}

const commentStates: Record<string, { text: string; variant: 'secondary' | 'attention' | 'success' | 'danger' }> = {
  pending: { text: '等待发送', variant: 'secondary' },
  attempted: { text: '正在发送', variant: 'attention' },
  done: { text: '已发送', variant: 'success' },
  unknown: { text: '结果不明，不自动重发', variant: 'attention' },
  failed: { text: '发送失败', variant: 'danger' },
}

function CommentRow({ c }: { c: Comment }) {
  const st = lookup(commentStates, c.State) ?? { text: c.State, variant: 'secondary' as const }
  return (
    <BoxRow as="li" className={css.block}>
      <div className={css.labels}>
        <span>
          #{c.Number} 上的{lookup(commentKinds, c.Kind) ?? c.Kind}
        </span>
        <Label variant={st.variant}>{st.text}</Label>
      </div>
      <p className={css.meta}>
        {c.State === 'unknown' && '评论没有幂等键：请求可能已经生效，为避免重复只发一次；请到 GitHub 上确认。 '}
        {c.Error && <span className={css.reason}>{c.Error} · </span>}
        <Ago t={c.Updated} />
      </p>
    </BoxRow>
  )
}

function BranchRow({ data, b }: { data: Data; b: Branch }) {
  return (
    <BoxRow as="li" className={css.block}>
      <code>{b.Branch}</code> <span className={css.muted}>{b.Kind === 'checkpoint' ? 'checkpoint' : '交付'}</span>{' '}
      <Commit data={data} sha={b.SHA} label="分支的 commit" />
      <p className={css.meta}>
        创建于 <Ago t={b.Created} />
        {b.Deleted ? (
          <>
            {' '}
            · 已于 <Ago t={b.Deleted} /> 删除
          </>
        ) : (
          b.Expires && ` · 推进后或 ${clock(b.Expires)} 保留期满后删除`
        )}
      </p>
    </BoxRow>
  )
}

function AIDevItemPage({ data, env }: PageProps<Data>) {
  const admin = env.Session?.Admin ?? false
  const it = data.Item
  useLive(it.Active)
  return (
    <div className={css.page}>
      <PageHeader
        title={`工作项 #${it.ID}`}
        description={
          <>
            {it.Repo}
            {it.Issues.length > 0 && ` · ${it.Issues.map((is) => `#${is.Number}`).join('、')}`}
          </>
        }
        actions={
          <>
            {admin && it.Retryable && <RetryForm item={it} />}
            {admin && it.Active && <StopForm item={it} ready={data.Ready} />}
            <LinkButton href={data.RepoURL} leadingVisual={ArrowLeftIcon}>
              {it.Repo} 的工作项
            </LinkButton>
          </>
        }
      />

      <Summary data={data} />
      <Issues data={data} />
      <DesignBasis
        basis={data.DesignBasis}
        contentBase={`/ai-dev/${it.ID}/docs/content`}
        emptyText="这个工作项还没有冻结设计文档。冻结只发生在仓库绑定了文档集时；页面不会用当前仓库绑定补造历史依据。"
        integrityHint="只展示冻结时保存的元数据；之后修改 Outline 中的文档或重新绑定都不会改变这里。"
      />
      <RelatedTasksTable tasks={data.RelatedTasks} />
      {data.Plan && (
        <Box as="section" aria-labelledby="ai-dev-plan">
          <BoxHeader
            id="ai-dev-plan"
            title={
              <>
                计划 <AgentSays />
              </>
            }
          />
          <BoxRow className={css.block}>
            <p className={css.pre}>{data.Plan}</p>
          </BoxRow>
        </Box>
      )}

      {data.Reports.map((r, i) => (
        <ReportBox key={`${r.TaskID}-${r.Kind}`} r={r} n={i} />
      ))}

      <Box as="section" aria-labelledby="ai-dev-comments">
        <BoxHeader id="ai-dev-comments" title="评论" />
        {data.Comments.length > 0 ? (
          <ul className={css.rows} aria-label="评论意图">
            {data.Comments.map((c, i) => (
              <CommentRow key={i} c={c} />
            ))}
          </ul>
        ) : (
          <BoxRow className={css.muted}>还没有要发的评论。</BoxRow>
        )}
      </Box>

      <Box as="section" aria-labelledby="ai-dev-branches">
        <BoxHeader id="ai-dev-branches" title="attempt 分支" />
        {data.Branches.length > 0 ? (
          <ul className={css.rows} aria-label="attempt 分支">
            {data.Branches.map((b) => (
              <BranchRow key={b.Branch} data={data} b={b} />
            ))}
          </ul>
        ) : (
          <BoxRow className={css.muted}>还没有 attempt 分支。</BoxRow>
        )}
      </Box>
    </div>
  )
}

export default definePage(schema, AIDevItemPage)
