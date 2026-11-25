import { ArrowLeftIcon } from '@primer/octicons-react'
import { Label, LinkButton } from '@primer/react'
import type { ReactNode } from 'react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxHeader, BoxRow } from '../components/Box'
import { PageHeader } from '../components/PageHeader'
import { Ago } from '../components/Time'
import { Trans, useLocale, useT } from '../i18n/locale'
import { aiDevItemSchema, fixRounds, resolution, workItemMode, workItemPhase, workItemState } from '../lib/aidev'
import { list } from '../lib/envelope'
import { clock, enumLabel, short } from '../lib/format'
import { PRLink, RetryForm, StopForm } from './ai_dev'
import { DesignBasis, RelatedTasksTable } from './ai/DesignBasis'
import { designBasisSchema, relatedTaskSchema } from './ai/docs'
import { AgentSays, ReportBox, reportSchema } from './ai/report'
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
  const t = useT()
  const { locale } = useLocale()
  const it = data.Item
  const st = workItemState(it.State, locale)
  const rounds = fixRounds(it, locale)
  const phase = workItemPhase(it, locale)
  const finished = it.Finished
  return (
    <Box as="section" aria-labelledby="ai-dev-summary">
      <BoxHeader id="ai-dev-summary" title={t('run.summary')} />
      <BoxRow className={css.block}>
        <div className={css.labels}>
          {phase && <Label variant="secondary">{phase}</Label>}
          <Label variant={st.variant}>{st.text}</Label>
          {it.Hold && <Label variant="attention">{data.HoldLabel}</Label>}
          {it.RequirementsChanged && <Label variant="attention">{t('aiDev.requirementsChanged')}</Label>}
        </div>
        {it.Reason && <p className={it.State === 'blocked' ? css.reason : css.p}>{it.Reason}</p>}
        <dl className={css.kv}>
          <KV k={t('aiReq.repo')}>
            <a href={data.RepoURL}>{it.Repo}</a>
          </KV>
          <KV k={t('aiDevItem.mode')}>{workItemMode(it, locale)}</KV>
          {it.DesignPR > 0 && (
            <KV k={t('aiDevItem.designPR')}>
              <PRLink github={data.GitHub} repo={it.Repo} pr={it.DesignPR} />
            </KV>
          )}
          <KV k={t('aiDevItem.base')}>
            <code>{data.BaseRef}</code> · {t('aiDevItem.start')} <Commit data={data} sha={data.BaseSHA} label={t('aiDevItem.baseHead')} />
          </KV>
          <KV k={t('aiDevItem.branch')}>
            <code>{it.Branch}</code>
            {it.HeadSHA && (
              <>
                {' '}
                · head <Commit data={data} sha={it.HeadSHA} label="head" />
              </>
            )}
          </KV>
          <KV k="PR">{it.PR ? <PRLink github={data.GitHub} repo={it.Repo} pr={it.PR} /> : t('aiDevItem.noPR')}</KV>
          {data.DeliveryBranch && (
            <KV k={t('aiDevItem.delivery')}>
              <code>{data.DeliveryBranch}</code> · <Commit data={data} sha={data.DeliverySHA} label={t('aiDevItem.deliveredCommit')} />
            </KV>
          )}
          <KV k={t('aiDevItem.fixes')}>{rounds || t('aiDevItem.noFixes', { count: it.MaxFixRounds })}</KV>
          {data.FixedHead && (
            <KV k={t('aiDevItem.fixedHead')}>
              <Commit data={data} sha={data.FixedHead} label={t('aiDevItem.fixedHeadTitle')} />
            </KV>
          )}
          <KV k="Agent">{it.Agent || t('devReport.unknown')}</KV>
          {data.TaskURL && (
            <KV k={t('aiDevItem.task')}>
              <a href={data.TaskURL}>{data.TaskURL.replace('/ai/tasks/', '#')}</a>
            </KV>
          )}
          <KV k={t('aiReq.time')}>
            <Trans id="aiReq.created" tags={{ ago: () => <Ago t={it.Created} /> }} />
            <Trans id="aiDevItem.updatedAt" tags={{ ago: () => <Ago t={it.Updated} /> }} />
            {finished && <Trans id="aiDevItem.finishedAt" tags={{ ago: () => <Ago t={finished} /> }} />}
          </KV>
        </dl>
      </BoxRow>
    </Box>
  )
}

function Issues({ data }: { data: Data }) {
  const t = useT()
  const { locale } = useLocale()
  return (
    <Box as="section" aria-labelledby="ai-dev-issues">
      <BoxHeader id="ai-dev-issues" title="Issue" />
      {data.Item.Issues.length === 0 && <BoxRow className={css.muted}>{t('aiDevItem.noIssues')}</BoxRow>}
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
            {is.Resolution && t('aiDevItem.resolution', { resolution: resolution(is.Resolution, locale) })}
            {is.Closed && t('aiDevItem.closed')}
            {!is.Active && t('aiDevItem.released')}
          </span>
        </BoxRow>
      ))}
    </Box>
  )
}

// 评论的种类（commentKind.*）和发送状态（commentState.*）的名称在词典中，这里只有状态的颜色；未知的值原样显示。
const commentStates: Record<string, 'secondary' | 'attention' | 'success' | 'danger'> = {
  pending: 'secondary',
  attempted: 'attention',
  done: 'success',
  unknown: 'attention',
  failed: 'danger',
}

function CommentRow({ c }: { c: Comment }) {
  const t = useT()
  const { locale } = useLocale()
  return (
    <BoxRow as="li" className={css.block}>
      <div className={css.labels}>
        <span>{t('aiDevItem.commentOn', { number: c.Number, kind: enumLabel(locale, 'commentKind', c.Kind) })}</span>
        <Label variant={lookup(commentStates, c.State) ?? 'secondary'}>{enumLabel(locale, 'commentState', c.State)}</Label>
      </div>
      <p className={css.meta}>
        {c.State === 'unknown' && `${t('aiDevItem.commentUnknown')} `}
        {c.Error && <span className={css.reason}>{c.Error} · </span>}
        <Ago t={c.Updated} />
      </p>
    </BoxRow>
  )
}

function BranchRow({ data, b }: { data: Data; b: Branch }) {
  const t = useT()
  const { locale } = useLocale()
  const deleted = b.Deleted
  return (
    <BoxRow as="li" className={css.block}>
      <code>{b.Branch}</code> <span className={css.muted}>{b.Kind === 'checkpoint' ? 'checkpoint' : t('aiDevItem.branchDelivery')}</span>{' '}
      <Commit data={data} sha={b.SHA} label={t('aiDevItem.branchCommit')} />
      <p className={css.meta}>
        <Trans id="aiDevItem.branchCreated" tags={{ ago: () => <Ago t={b.Created} /> }} />
        {deleted ? (
          <Trans id="aiDevItem.branchDeleted" tags={{ ago: () => <Ago t={deleted} /> }} />
        ) : (
          b.Expires && t('aiDevItem.branchExpires', { time: clock(b.Expires, locale) })
        )}
      </p>
    </BoxRow>
  )
}

function AIDevItemPage({ data, env }: PageProps<Data>) {
  const t = useT()
  const admin = env.Session?.Admin ?? false
  const it = data.Item
  useLive(it.Active)
  return (
    <div className={css.page}>
      <PageHeader
        title={t('workItem.ref', { id: it.ID })}
        description={
          <>
            {it.Repo}
            {it.Issues.length > 0 && ` · ${it.Issues.map((is) => `#${is.Number}`).join(t('list.enum'))}`}
          </>
        }
        actions={
          <>
            {admin && it.Retryable && <RetryForm item={it} />}
            {admin && it.Active && <StopForm item={it} ready={data.Ready} />}
            <LinkButton href={data.RepoURL} leadingVisual={ArrowLeftIcon}>
              {t('aiDevItem.back', { repo: it.Repo })}
            </LinkButton>
          </>
        }
      />

      <Summary data={data} />
      <Issues data={data} />
      <DesignBasis
        basis={data.DesignBasis}
        contentBase={`/ai-dev/${it.ID}/docs/content`}
        emptyText={t('aiDevItem.basisEmpty')}
        integrityHint={t('aiDevItem.basisHint')}
      />
      <RelatedTasksTable tasks={data.RelatedTasks} />
      {data.Plan && (
        <Box as="section" aria-labelledby="ai-dev-plan">
          <BoxHeader
            id="ai-dev-plan"
            title={
              <>
                {t('devReport.plan')} <AgentSays />
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
        <BoxHeader id="ai-dev-comments" title={t('aiDevItem.comments')} />
        {data.Comments.length > 0 ? (
          <ul className={css.rows} aria-label={t('aiDevItem.commentsLabel')}>
            {data.Comments.map((c, i) => (
              <CommentRow key={i} c={c} />
            ))}
          </ul>
        ) : (
          <BoxRow className={css.muted}>{t('aiDevItem.noComments')}</BoxRow>
        )}
      </Box>

      <Box as="section" aria-labelledby="ai-dev-branches">
        <BoxHeader id="ai-dev-branches" title={t('aiDevItem.branches')} />
        {data.Branches.length > 0 ? (
          <ul className={css.rows} aria-label={t('aiDevItem.branches')}>
            {data.Branches.map((b) => (
              <BranchRow key={b.Branch} data={data} b={b} />
            ))}
          </ul>
        ) : (
          <BoxRow className={css.muted}>{t('aiDevItem.noBranches')}</BoxRow>
        )}
      </Box>
    </div>
  )
}

export default definePage(schema, AIDevItemPage)
