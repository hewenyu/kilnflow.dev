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
import { Msg, Trans, useLocale, useT } from '../i18n/locale'
import { defaultLocale, msg, translate, type Locale, type MsgKey } from '../i18n/messages'
import { list, textSchema } from '../lib/envelope'
import { short } from '../lib/format'
import { lookup } from '../lib/lookup'
import { mergeQueueEntrySchema, type MergeQueueEntry } from '../lib/schemas'
import css from './merge_queue.module.css'

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
  /** 原因（服务端的消息；没有原因码的旧记录是原样的中文说明）。 */
  Reason: textSchema,
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
export function enqueuedBy(by: string, locale: Locale = defaultLocale): string {
  if (by.startsWith('github:')) return translate(locale, msg('mergeQueue.byLabel', { login: by.slice('github:'.length) }))
  if (by === 'auto') return translate(locale, msg('mergeQueue.byAuto'))
  return by || translate(locale, msg('mergeQueue.byUnknown'))
}

/** toneClass 返回状态的样式：服务端给出的语义级别（StatusTone）是失败或需要人确认时突出显示。 */
const toneClass = (tone: string) => (tone === 'bad' || tone === 'warn' ? css.reason : undefined)

function PRLink({ data, repo, pr }: { data: Data; repo: string; pr: number }) {
  if (!data.GitHub) return <b>#{pr}</b>
  return (
    <a href={`${data.GitHub}/${repo}/pull/${pr}`} rel="noreferrer" className={css.pr}>
      #{pr}
    </a>
  )
}

function Commit({ data, repo, sha, label }: { data: Data; repo: string; sha: string; label: string }) {
  // label 是已经翻译好的说明（title 中放在完整 SHA 之前）
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
  const t = useT()
  const { locale } = useLocale()
  return (
    <BoxRow as="li" className={css.row}>
      <span className={css.pos} aria-label={t('mergeQueue.position', { n })}>
        {n}
      </span>
      <div className={css.text}>
        <div className={css.title}>
          <PRLink data={data} repo={q.Repo} pr={e.PR} />
          {e.BaseRef && <Label variant="secondary">→ {e.BaseRef}</Label>}
          {head && <Label variant="accent">{t('mergeQueue.processing')}</Label>}
          <span className={css.status}>{e.Status ? <Msg text={e.Status} /> : t('mergeQueue.waiting')}</span>
        </div>
        <p className={css.meta}>
          {e.HeadSHA && (
            <>
              {t('mergeQueue.head')} <Commit data={data} repo={q.Repo} sha={e.HeadSHA} label={t('mergeQueue.head')} /> ·{' '}
            </>
          )}
          <Trans id="mergeQueue.enqueued" params={{ by: enqueuedBy(e.EnqueuedBy, locale) }} tags={{ ago: () => <Ago t={e.EnqueuedAt} /> }} />
        </p>
      </div>
      {admin && (
        <ConfirmForm
          action="/merge-queue/remove"
          fields={{ repo: q.Repo, pr: String(e.PR) }}
          button={t('mergeQueue.remove')}
          buttonProps={{ size: 'small' }}
          title={t('mergeQueue.removeTitle', { pr: e.PR })}
          confirmLabel={t('mergeQueue.removeConfirm')}
          cancelLabel={t('mergeQueue.removeCancel')}
        >
          {t('mergeQueue.removeText', { label: data.Label })}
        </ConfirmForm>
      )}
    </BoxRow>
  )
}

/** removals 是冲突出队的类别（PLAN #28.3）：按 PR 的归属，AI 开发的 PR 交回返修，其余的等提交者解决。 */
const removals: Record<string, { text: MsgKey; variant: 'accent' | 'attention' }> = {
  conflict_dev: { text: 'mergeQueue.removalDev', variant: 'accent' },
  conflict_author: { text: 'mergeQueue.removalAuthor', variant: 'attention' },
}

function RecentRow({ data, q, e }: { data: Data; q: Queue; e: MergeQueueEntry }) {
  const t = useT()
  const merged = e.State === 'merged'
  const removed = lookup(removals, e.Removal)
  const ago = { ago: () => <Ago t={e.FinishedAt} fallback="—" /> }
  return (
    <BoxRow as="li" className={css.row}>
      <div className={css.text}>
        <div className={css.title}>
          <PRLink data={data} repo={q.Repo} pr={e.PR} />
          {e.BaseRef && <Label variant="secondary">→ {e.BaseRef}</Label>}
          {merged ? <Label variant="done">{t('mergeQueue.merged')}</Label> : <Label variant="secondary">{t('mergeQueue.removed')}</Label>}
          {removed && <Label variant={removed.variant}>{t(removed.text)}</Label>}
          {merged && e.MergeSHA && <Commit data={data} repo={q.Repo} sha={e.MergeSHA} label={t('mergeQueue.mergeCommit')} />}
        </div>
        <p className={css.meta}>
          {e.Reason && (
            <span className={merged ? undefined : css.reason}>
              <Msg text={e.Reason} /> ·{' '}
            </span>
          )}
          <Trans id={merged ? 'mergeQueue.mergedAt' : 'mergeQueue.removedAt'} tags={ago} />
          {/* 队列合并后关闭关联 issue 的进度或失败原因（PLAN #28.3）：样式按服务端给出的语义级别，不看文字 */}
          {merged && e.Status && (
            <span className={toneClass(e.StatusTone)}>
              {' · '}
              <Msg text={e.Status} />
            </span>
          )}
        </p>
      </div>
    </BoxRow>
  )
}

const autoTagStates: Record<string, { text: MsgKey; variant: 'secondary' | 'accent' | 'success' | 'attention' | 'danger' }> = {
  pending: { text: 'autoTagState.pending', variant: 'secondary' },
  tagging: { text: 'autoTagState.tagging', variant: 'accent' },
  tagged: { text: 'autoTagState.tagged', variant: 'accent' },
  planned: { text: 'autoTagState.planned', variant: 'success' },
  skipped: { text: 'autoTagState.skipped', variant: 'attention' },
  failed: { text: 'autoTagState.failed', variant: 'danger' },
}

/** autoTagActive 表示记录还在处理中（页面自动刷新）。 */
/** mergeConfigPath 是合并队列和自动打 tag 的配置文件（internal/mergecfg.Path）。 */
const mergeConfigPath = '.kiln/merge.yml'

const autoTagActive = (t: AutoTag) => t.State === 'pending' || t.State === 'tagging' || t.State === 'tagged'

function AutoTagTitle({ data, q, t }: { data: Data; q: Queue; t: AutoTag }) {
  const tr = useT()
  if (!t.Tag) return <span className={css.muted}>{tr('mergeQueue.noVersion')}</span>
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
  const tr = useT()
  const st = lookup(autoTagStates, t.State)
  return (
    <BoxRow as="li" className={css.row}>
      <div className={css.text}>
        <div className={css.title}>
          <AutoTagTitle data={data} q={q} t={t} />
          <Label variant={st?.variant ?? 'secondary'}>{st ? tr(st.text) : t.State}</Label>
          <Commit data={data} repo={q.Repo} sha={t.SHA} label={tr('mergeQueue.mergedCommitLabel')} />
        </div>
        <p className={css.meta}>
          {t.Reason && (
            <span className={t.State === 'failed' ? css.reason : undefined}>
              <Msg text={t.Reason} /> ·{' '}
            </span>
          )}
          <Trans
            id={t.Source === 'queue' ? 'mergeQueue.viaQueue' : 'mergeQueue.viaOutside'}
            tags={{ pr: () => <PRLink data={data} repo={q.Repo} pr={t.PR} /> }}
          />
          {t.BaseRef && ` → ${t.BaseRef}`}
          {t.MergedBy && tr(t.SenderIsBot ? 'mergeQueue.mergedByBot' : 'mergeQueue.mergedBy', { login: t.MergedBy })}
          {t.WorkItemID > 0 && (
            <>
              {' · '}
              <a href={`/ai-dev/${t.WorkItemID}`}>{tr('mergeQueue.workItem', { id: t.WorkItemID })}</a>
            </>
          )}
          {t.Labels.length > 0 && tr('mergeQueue.labels', { labels: t.Labels.join(tr('mergeQueue.labelSep')) })} · <Ago t={t.Updated} />
          {t.Tries > 1 && tr('mergeQueue.tries', { count: t.Tries })}
        </p>
      </div>
      {admin && t.Retryable && (
        <Form action="/merge-queue/autotag/retry" aria-label={tr('mergeQueue.retryLabel', { sha: t.SHA.slice(0, 7) })}>
          <input type="hidden" name="repo" value={q.Repo} />
          <input type="hidden" name="sha" value={t.SHA} />
          <SubmitButton size="small">{tr('mergeQueue.retry')}</SubmitButton>
        </Form>
      )}
    </BoxRow>
  )
}

function QueueBox({ data, q, admin }: { data: Data; q: Queue; admin: boolean }) {
  const t = useT()
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
        <ol className={css.rows} aria-label={t('mergeQueue.queuedList', { repo: q.Repo })}>
          {q.Queued.map((e, i) => (
            <QueuedRow key={e.PR} data={data} q={q} e={e} n={positions[i] ?? i + 1} head={heads.has(e.PR)} admin={admin} />
          ))}
        </ol>
      ) : (
        <BoxRow className={css.muted}>{t('mergeQueue.noQueued')}</BoxRow>
      )}
      <BoxRow className={css.subhead}>{t('mergeQueue.recent')}</BoxRow>
      {q.Recent.length > 0 ? (
        <ul className={css.rows} aria-label={t('mergeQueue.recentList', { repo: q.Repo })}>
          {q.Recent.map((e) => (
            <RecentRow key={`${e.PR}-${e.EnqueuedAt}`} data={data} q={q} e={e} />
          ))}
        </ul>
      ) : (
        <BoxRow className={css.muted}>{t('mergeQueue.noRecent')}</BoxRow>
      )}
      {q.AutoTags.length > 0 && (
        <>
          <BoxRow className={css.subhead}>{t('mergeQueue.autoTags', { path: mergeConfigPath })}</BoxRow>
          <ul className={css.rows} aria-label={t('mergeQueue.autoTagList', { repo: q.Repo })}>
            {q.AutoTags.map((a) => (
              <AutoTagRow key={a.SHA} data={data} q={q} t={a} admin={admin} />
            ))}
          </ul>
        </>
      )}
    </Box>
  )
}

/** AddForm 是管理员把 PR 加入合并队列的表单。 */
function AddForm({ data }: { data: Data }) {
  const t = useT()
  return (
    <Form action="/merge-queue/add" className={css.add} aria-label={t('mergeQueue.add')}>
      <FormControl required>
        <FormControl.Label>{t('mergeQueue.repo')}</FormControl.Label>
        <Select name="repo" defaultValue={data.Repo || data.Repos[0]}>
          {data.Repos.map((r) => (
            <Select.Option key={r} value={r}>
              {r}
            </Select.Option>
          ))}
        </Select>
      </FormControl>
      <FormControl required>
        <FormControl.Label>{t('mergeQueue.prNumber')}</FormControl.Label>
        <TextInput name="pr" inputMode="numeric" pattern="#?[0-9]+" placeholder={t('mergeQueue.prPlaceholder')} className={css.prInput} />
      </FormControl>
      <SubmitButton leadingVisual={GitMergeQueueIcon}>{t('mergeQueue.add')}</SubmitButton>
    </Form>
  )
}

const code = { code: (c: string) => <code>{c}</code> }

function MergeQueuePage({ data, env }: PageProps<Data>) {
  const t = useT()
  const admin = env.Session?.Admin ?? false
  useLive(data.Queues.some((q) => q.Queued.length > 0 || q.AutoTags.some(autoTagActive)))
  return (
    <div className={css.page}>
      <PageHeader
        title={data.Repo ? t('mergeQueue.headingRepo', { repo: data.Repo }) : t('mergeQueue.heading')}
        description={<Trans id="mergeQueue.description" params={{ label: data.Label }} tags={code} />}
        actions={
          data.Repo && (
            <LinkButton href="/merge-queue" leadingVisual={ArrowLeftIcon}>
              {t('mergeQueue.allRepos')}
            </LinkButton>
          )
        }
      />

      {admin && data.Repos.length > 0 && <AddForm data={data} />}

      {data.Queues.length > 0 ? (
        data.Queues.map((q) => <QueueBox key={q.Repo} data={data} q={q} admin={admin} />)
      ) : (
        <Box>
          <EmptyState icon={GitMergeQueueIcon} title={t('mergeQueue.emptyTitle')}>
            <Trans id={admin ? 'mergeQueue.emptyTextAdmin' : 'mergeQueue.emptyText'} params={{ label: data.Label }} tags={code} />
          </EmptyState>
        </Box>
      )}
    </div>
  )
}

export default definePage(schema, MergeQueuePage)
