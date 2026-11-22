import { ArrowLeftIcon, GitMergeQueueIcon } from '@primer/octicons-react'
import { CounterLabel, FormControl, Label, LinkButton, Select, TextInput } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxHeader, BoxRow } from '../components/Box'
import { EmptyState } from '../components/EmptyState'
import { ConfirmForm, Form, SubmitButton } from '../components/Form'
import { PageHeader } from '../components/PageHeader'
import { Ago } from '../components/Time'
import { list } from '../lib/envelope'
import { short } from '../lib/format'
import { mergeQueueEntrySchema, type MergeQueueEntry } from '../lib/schemas'
import css from './merge_queue.module.css'
import { lookup } from '../lib/lookup'

// 合并队列（PLAN #28）：按仓库列出排队中的 PR（第一个是队首，正在处理）和最近结束的项。
// PR 加上 label kiln:merge 即入队；管理员也可以在这里入队、移出（等同于加上、去掉这个 label）。

/** autoTagView：自动打 tag 的记录（PLAN #29）。 */
const autoTagSchema = z.object({
  SHA: z.string(),
  PR: z.number(),
  /** PR 的目标分支（PLAN #29.4）：只有合入发版分支的才会打 tag；旧记录为空。 */
  BaseRef: z.string(),
  /** PR 合入时的 label（决定版本号升哪一位）。 */
  Labels: list(z.string()),
  MergedBy: z.string(),
  /** 真实的发布发起者是否是机器人。 */
  SenderIsBot: z
    .boolean()
    .optional()
    .transform((v) => v ?? false),
  /** 认定的 AI 开发工作项；0 表示没有可信归属。 */
  WorkItemID: z
    .number()
    .optional()
    .transform((v) => v ?? 0),
  /** queue（合并队列合入）或 webhook（在队列之外合入）。 */
  Source: z.string(),
  /** pending、tagging、tagged、planned、skipped、failed。 */
  State: z.string(),
  Tag: z.string(),
  Reason: z.string(),
  Tries: z.number(),
  Created: z.string(),
  Updated: z.string(),
  /** failed 或 skipped：可以重试。 */
  Retryable: z.boolean(),
})
type AutoTag = z.infer<typeof autoTagSchema>

const queueSchema = z.object({
  Repo: z.string(),
  /** 只看这个仓库的地址。 */
  URL: z.string(),
  Queued: list(mergeQueueEntrySchema),
  Recent: list(mergeQueueEntrySchema),
  /** 最近的自动打 tag 记录，新的在前；仓库没有开启时为空。 */
  AutoTags: list(autoTagSchema)
    .optional()
    .transform((v) => v ?? []),
})
type Queue = z.infer<typeof queueSchema>

const schema = z.object({
  /** 只看的仓库；为空表示全部仓库。 */
  Repo: z.string(),
  Queues: list(queueSchema),
  /** 加入表单的仓库选项（只有管理员有）。 */
  Repos: list(z.string()),
  Label: z.string(),
  GitHub: z.string(),
})
type Data = z.infer<typeof schema>

/** enqueuedBy 返回入队者的说明：github:<login> 是在 GitHub 上给 PR 加了 label，auto 是验证通过后自动入队（PLAN #29.5）。 */
export function enqueuedBy(by: string): string {
  if (by.startsWith('github:')) return `${by.slice('github:'.length)}（加 label）`
  if (by === 'auto') return 'Kiln（验证通过后自动入队）'
  return by || '未知'
}

function PRLink({ data, repo, pr }: { data: Data; repo: string; pr: number }) {
  if (!data.GitHub) return <b>#{pr}</b>
  return (
    <a href={`${data.GitHub}/${repo}/pull/${pr}`} rel="noreferrer" className={css.pr}>
      #{pr}
    </a>
  )
}

function Commit({ data, repo, sha, label }: { data: Data; repo: string; sha: string; label: string }) {
  if (!sha) return null
  return data.GitHub ? (
    <a href={`${data.GitHub}/${repo}/commit/${sha}`} rel="noreferrer" className="mono" title={`${label} ${sha}`}>
      {short(sha)}
    </a>
  ) : (
    <code title={`${label} ${sha}`}>{short(sha)}</code>
  )
}

function QueuedRow({ data, q, e, n, head, admin }: { data: Data; q: Queue; e: MergeQueueEntry; n: number; head: boolean; admin: boolean }) {
  return (
    <BoxRow as="li" className={css.row}>
      <span className={css.pos} aria-label={`第 ${n} 个`}>
        {n}
      </span>
      <div className={css.text}>
        <div className={css.title}>
          <PRLink data={data} repo={q.Repo} pr={e.PR} />
          {e.BaseRef && <Label variant="secondary">→ {e.BaseRef}</Label>}
          {head && <Label variant="accent">正在处理</Label>}
          <span className={css.status}>{e.Status || '等待处理'}</span>
        </div>
        <p className={css.meta}>
          {e.HeadSHA && (
            <>
              head <Commit data={data} repo={q.Repo} sha={e.HeadSHA} label="head" /> ·{' '}
            </>
          )}
          由 {enqueuedBy(e.EnqueuedBy)} 于 <Ago t={e.EnqueuedAt} /> 入队
        </p>
      </div>
      {admin && (
        <ConfirmForm
          action="/merge-queue/remove"
          fields={{ repo: q.Repo, pr: String(e.PR) }}
          button="移出"
          buttonProps={{ size: 'small' }}
          title={`把 #${e.PR} 移出合并队列？`}
          confirmLabel="移出合并队列"
          cancelLabel="返回"
        >
          Kiln 会去掉 PR 上的 label {data.Label}，这个 PR 不再自动合并。之后重新加上 label 或在这里加入，会排到队尾。
        </ConfirmForm>
      )}
    </BoxRow>
  )
}

/** removals 是冲突出队的类别（PLAN #28.3）：按 PR 的归属，AI 开发的 PR 交回返修，其余的等提交者解决。 */
const removals: Record<string, { text: string; variant: 'accent' | 'attention' }> = {
  conflict_dev: { text: '冲突：已交给 AI 开发', variant: 'accent' },
  conflict_author: { text: '冲突：等待提交者解决', variant: 'attention' },
}

function RecentRow({ data, q, e }: { data: Data; q: Queue; e: MergeQueueEntry }) {
  const merged = e.State === 'merged'
  const removed = lookup(removals, e.Removal)
  return (
    <BoxRow as="li" className={css.row}>
      <div className={css.text}>
        <div className={css.title}>
          <PRLink data={data} repo={q.Repo} pr={e.PR} />
          {e.BaseRef && <Label variant="secondary">→ {e.BaseRef}</Label>}
          {merged ? <Label variant="done">已合并</Label> : <Label variant="secondary">已移出</Label>}
          {removed && <Label variant={removed.variant}>{removed.text}</Label>}
          {merged && e.MergeSHA && <Commit data={data} repo={q.Repo} sha={e.MergeSHA} label="合并提交" />}
        </div>
        <p className={css.meta}>
          {e.Reason && <span className={merged ? undefined : css.reason}>{e.Reason} · </span>}
          {merged ? '合并于 ' : '移出于 '}
          <Ago t={e.FinishedAt} fallback="—" />
          {/* 队列合并后关闭关联 issue 的进度或失败原因（PLAN #28.3） */}
          {merged && e.Status && <span className={e.Status.includes('失败') ? css.reason : undefined}> · {e.Status}</span>}
        </p>
      </div>
    </BoxRow>
  )
}

const autoTagStates: Record<string, { text: string; variant: 'secondary' | 'accent' | 'success' | 'attention' | 'danger' }> = {
  pending: { text: '等待处理', variant: 'secondary' },
  tagging: { text: '正在打 tag', variant: 'accent' },
  tagged: { text: '已打 tag，正在规划发布', variant: 'accent' },
  planned: { text: '已规划发布', variant: 'success' },
  skipped: { text: '已跳过', variant: 'attention' },
  failed: { text: '失败', variant: 'danger' },
}

/** autoTagActive 表示记录还在处理中（页面自动刷新）。 */
const autoTagActive = (t: AutoTag) => t.State === 'pending' || t.State === 'tagging' || t.State === 'tagged'

function AutoTagTitle({ data, q, t }: { data: Data; q: Queue; t: AutoTag }) {
  if (!t.Tag) return <span className={css.muted}>版本号未定</span>
  if (data.GitHub && (t.State === 'tagged' || t.State === 'planned')) {
    return (
      <a href={`${data.GitHub}/${q.Repo}/releases/tag/${encodeURIComponent(t.Tag)}`} rel="noreferrer" className={`mono ${css.pr ?? ''}`}>
        {t.Tag}
      </a>
    )
  }
  return <b className="mono">{t.Tag}</b>
}

function AutoTagRow({ data, q, t, admin }: { data: Data; q: Queue; t: AutoTag; admin: boolean }) {
  const st = lookup(autoTagStates, t.State) ?? { text: t.State, variant: 'secondary' as const }
  return (
    <BoxRow as="li" className={css.row}>
      <div className={css.text}>
        <div className={css.title}>
          <AutoTagTitle data={data} q={q} t={t} />
          <Label variant={st.variant}>{st.text}</Label>
          <Commit data={data} repo={q.Repo} sha={t.SHA} label="合入的 commit" />
        </div>
        <p className={css.meta}>
          {t.Reason && <span className={t.State === 'failed' ? css.reason : undefined}>{t.Reason} · </span>}
          <PRLink data={data} repo={q.Repo} pr={t.PR} /> {t.Source === 'queue' ? '经合并队列' : '在队列之外'}合入
          {t.BaseRef && ` → ${t.BaseRef}`}
          {t.MergedBy && `（${t.MergedBy}${t.SenderIsBot ? '，机器人' : ''}）`}
          {t.WorkItemID > 0 && (
            <>
              {' · '}
              <a href={`/ai-dev/${t.WorkItemID}`}>工作项 #{t.WorkItemID}</a>
            </>
          )}
          {t.Labels.length > 0 && `，label ${t.Labels.join('、')}`} · <Ago t={t.Updated} />
          {t.Tries > 1 && `，尝试 ${t.Tries} 次`}
        </p>
      </div>
      {admin && t.Retryable && (
        <Form action="/merge-queue/autotag/retry" aria-label={`重试 ${t.SHA.slice(0, 7)} 的自动打 tag`}>
          <input type="hidden" name="repo" value={q.Repo} />
          <input type="hidden" name="sha" value={t.SHA} />
          <SubmitButton size="small">重试</SubmitButton>
        </Form>
      )}
    </BoxRow>
  )
}

function QueueBox({ data, q, admin }: { data: Data; q: Queue; admin: boolean }) {
  const id = `mq-${q.Repo.replace(/[^A-Za-z0-9_-]/g, '-')}`
  // 每条目标分支各自排队、互不阻塞（PLAN #28.5）：每个分支的第一个排队项都是这一条分支的队首（正在处理），排位按同一目标
  // 分支计算，与状态文案"目标 dev · 排在第 2 个"严格一致。
  const heads = new Set<number>()
  const seen = new Set<string>()
  const counts = new Map<string, number>()
  const positions = q.Queued.map((e) => {
    const n = (counts.get(e.BaseRef) ?? 0) + 1
    counts.set(e.BaseRef, n)
    if (!seen.has(e.BaseRef)) {
      seen.add(e.BaseRef)
      heads.add(e.PR)
    }
    return n
  })
  return (
    <Box as="section" aria-labelledby={id}>
      <BoxHeader
        id={id}
        title={
          <>
            {data.Repo ? q.Repo : <a href={q.URL}>{q.Repo}</a>} <CounterLabel>{q.Queued.length}</CounterLabel>
          </>
        }
      />
      {q.Queued.length > 0 ? (
        <ol className={css.rows} aria-label={`${q.Repo} 排队中的 PR`}>
          {q.Queued.map((e, i) => (
            <QueuedRow key={e.PR} data={data} q={q} e={e} n={positions[i] ?? i + 1} head={heads.has(e.PR)} admin={admin} />
          ))}
        </ol>
      ) : (
        <BoxRow className={css.muted}>没有排队中的 PR。</BoxRow>
      )}
      <BoxRow className={css.subhead}>最近结束</BoxRow>
      {q.Recent.length > 0 ? (
        <ul className={css.rows} aria-label={`${q.Repo} 最近结束的 PR`}>
          {q.Recent.map((e) => (
            <RecentRow key={`${e.PR}-${e.EnqueuedAt}`} data={data} q={q} e={e} />
          ))}
        </ul>
      ) : (
        <BoxRow className={css.muted}>还没有合并或移出的记录。</BoxRow>
      )}
      {q.AutoTags.length > 0 && (
        <>
          <BoxRow className={css.subhead}>自动打 tag（合入发版分支后按 .kiln/merge.yml 打 tag 并发布）</BoxRow>
          <ul className={css.rows} aria-label={`${q.Repo} 自动打 tag 的记录`}>
            {q.AutoTags.map((t) => (
              <AutoTagRow key={t.SHA} data={data} q={q} t={t} admin={admin} />
            ))}
          </ul>
        </>
      )}
    </Box>
  )
}

/** AddForm 是管理员把 PR 加入合并队列的表单。 */
function AddForm({ data }: { data: Data }) {
  return (
    <Form action="/merge-queue/add" className={css.add} aria-label="加入合并队列">
      <FormControl required>
        <FormControl.Label>仓库</FormControl.Label>
        <Select name="repo" defaultValue={data.Repo || data.Repos[0]}>
          {data.Repos.map((r) => (
            <Select.Option key={r} value={r}>
              {r}
            </Select.Option>
          ))}
        </Select>
      </FormControl>
      <FormControl required>
        <FormControl.Label>PR 号</FormControl.Label>
        <TextInput name="pr" inputMode="numeric" pattern="#?[0-9]+" placeholder="例如 42" className={css.prInput} />
      </FormControl>
      <SubmitButton leadingVisual={GitMergeQueueIcon}>加入合并队列</SubmitButton>
    </Form>
  )
}

function MergeQueuePage({ data, env }: PageProps<Data>) {
  const admin = env.Session?.Admin ?? false
  useLive(data.Queues.some((q) => q.Queued.length > 0 || q.AutoTags.some(autoTagActive)))
  return (
    <div className={css.page}>
      <PageHeader
        title={data.Repo ? `${data.Repo} 的合并队列` : '合并队列'}
        description={
          <>
            按目标分支分别排队，逐条验证各分支队首 PR 的 head，CI 和 AI 审计通过、不落后于目标分支时自动合并。给 PR 加上 label <code>{data.Label}</code> 即加入合并队列。
          </>
        }
        actions={
          data.Repo && (
            <LinkButton href="/merge-queue" leadingVisual={ArrowLeftIcon}>
              全部仓库
            </LinkButton>
          )
        }
      />

      {admin && data.Repos.length > 0 && <AddForm data={data} />}

      {data.Queues.length > 0 ? (
        data.Queues.map((q) => <QueueBox key={q.Repo} data={data} q={q} admin={admin} />)
      ) : (
        <Box>
          <EmptyState icon={GitMergeQueueIcon} title="合并队列是空的">
            给 PR 加上 label <code>{data.Label}</code> 即加入合并队列{admin ? '，也可以在上面填写仓库和 PR 号加入' : ''}。
          </EmptyState>
        </Box>
      )}
    </div>
  )
}

export default definePage(schema, MergeQueuePage)
