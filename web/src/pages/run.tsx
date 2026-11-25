import {
  CheckIcon,
  ClockIcon,
  GitCommitIcon,
  GitMergeQueueIcon,
  GitPullRequestIcon,
  PlayIcon,
  StopIcon,
  SyncIcon,
  TagIcon,
} from '@primer/octicons-react'
import { ActionList, ActionMenu, Link, Timeline } from '@primer/react'
import type { ReactNode } from 'react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Avatar } from '../components/Avatar'
import { Box, BoxHeader } from '../components/Box'
import { Callout } from '../components/Callout'
import { ConfirmForm } from '../components/Form'
import { RefLabel } from '../components/RefLabel'
import { RunHeader } from '../components/RunHeader'
import { RunSidebar } from '../components/RunSidebar'
import { StatusIcon } from '../components/StatusIcon'
import { Ago, Duration } from '../components/Time'
import { Msg, Trans, useLocale, useT, useText } from '../i18n/locale'
import { list, textSchema } from '../lib/envelope'
import { clock, eventLabel, jobStatesSummary, refKind, refName, rerunLabel, runState, short } from '../lib/format'
import { jobViewSchema, mergeQueueEntrySchema, runSchema, type Run } from '../lib/schemas'
import { JobGraph } from './run/JobGraph'
import { RerunButton, rerunFormSchema } from './run/RerunDialog'
import css from './run.module.css'

// run 详情页（GitHub Actions 运行页的布局）：左侧是概要和 job 列表，右侧是标题和操作、触发信息、按阶段排列的 job、活动。
// 明确正在查看的是哪一次运行（rerun）；地址中的 rerun 无效时给出提示，不显示任何一次运行的 job。

const rerunViewSchema = z.object({
  N: z.number(),
  /** 这次运行的状态；没有记录时为空。 */
  State: z.string(),
})

const activitySchema = z.object({
  ID: z.number(),
  Actor: z.string(),
  Action: z.string(),
  Detail: z.record(z.string(), z.unknown()).nullable(),
  At: z.string(),
  /** 这条活动在 run 详情页上的说明（不含操作者）：服务端给的消息。 */
  RunText: textSchema,
})
type Activity = z.infer<typeof activitySchema>

/** PR 的合并队列状态（PLAN #28）。 */
const mergeQueueSchema = z.object({
  /** 加入表单的字段。 */
  Repo: z.string(),
  PR: z.number(),
  /** PR 在队列中最近的一项（排队中的优先）；没有记录时为 null。 */
  Entry: mergeQueueEntrySchema.nullable(),
  /** 排队中的位置（从 1 开始），不在排队时为 0。 */
  Pos: z.number(),
  /** 这个仓库的合并队列页。 */
  URL: z.string(),
})
type MergeQueue = z.infer<typeof mergeQueueSchema>

const schema = z.object({
  Run: runSchema,
  Repo: z.string(),
  /** "返回运行列表"的地址（从筛选后的列表进入时）。 */
  Back: z.string(),
  Reruns: list(rerunViewSchema),
  /** 正在查看的运行；地址中的 rerun 无效时为 null，Invalid 是原值。 */
  Viewed: rerunViewSchema.nullable(),
  Invalid: z.string(),
  Jobs: list(jobViewSchema),
  Stages: list(z.object({ N: z.number(), Jobs: list(jobViewSchema) })),
  States: list(z.string()),
  Activity: list(activitySchema),
  GitHub: z.string(),
  /** run 还没有结束：可以取消，不能重跑。 */
  Active: z.boolean(),
  /** 重跑的说明；run 没有结束时为 null。 */
  Form: rerunFormSchema.nullable(),
  /** 正在查看的这次运行的日志和产物按保留策略清理的时间。 */
  Purged: z.string().nullable(),
  /** PR 的合并队列状态；不是 PR 的 run 为 null。 */
  MergeQueue: mergeQueueSchema.nullable(),
  Start: z.number(),
  End: z.number(),
  /** 正在查看的不是最新一次运行。 */
  History: z.boolean(),
})
type Data = z.infer<typeof schema>

/** runPageURL 返回 run 详情页的地址：rerun 为 null 时是最新一次，back 是运行列表的地址。 */
export function runPageURL(id: number, rerun: number | null, back: string): string {
  const q = new URLSearchParams()
  if (rerun !== null) q.set('rerun', String(rerun))
  if (back) q.set('back', back)
  const s = q.toString()
  return `/runs/${id}${s ? '?' + s : ''}`
}

/** useStateText 返回 run 状态的说明；没有记录时为"状态未知"。 */
function useStateText(): (s: string) => string {
  const t = useT()
  const { locale } = useLocale()
  return (s: string) => (s ? runState(s, locale) : t('run.stateUnknown'))
}

/** RerunPicker 切换查看这个 run 的每一次运行（GitHub 的 "Latest #2" 菜单）。 */
function RerunPicker({ data }: { data: Data }) {
  const t = useT()
  const { locale } = useLocale()
  const stateText = useStateText()
  const r = data.Run
  const viewed = data.Viewed
  return (
    <ActionMenu>
      <ActionMenu.Button aria-label={viewed ? t('run.pickerViewing', { rerun: rerunLabel(viewed.N, locale) }) : t('run.pickerInvalid')}>
        {viewed ? rerunLabel(viewed.N, locale) : `rerun=${data.Invalid}`}
        {viewed?.N === r.CurrentRerun && <span className={css.latest}>{t('run.latest')}</span>}
      </ActionMenu.Button>
      <ActionMenu.Overlay width="medium">
        <ActionList aria-label={t('run.reruns')}>
          {data.Reruns.toReversed().map((v) => (
            <ActionList.LinkItem key={v.N} href={runPageURL(r.ID, v.N === r.CurrentRerun ? null : v.N, data.Back)} active={viewed?.N === v.N} aria-current={viewed?.N === v.N ? 'page' : undefined}>
              <ActionList.LeadingVisual>
                <StatusIcon state={v.State} label={stateText(v.State)} />
              </ActionList.LeadingVisual>
              {rerunLabel(v.N, locale)}
              <ActionList.TrailingVisual>
                {v.N === r.CurrentRerun ? t('run.latest') : viewed?.N === v.N ? <CheckIcon /> : null}
              </ActionList.TrailingVisual>
            </ActionList.LinkItem>
          ))}
        </ActionList>
      </ActionMenu.Overlay>
    </ActionMenu>
  )
}

/** MergeQueueNotice 是 PR 在合并队列中的状态；没有记录时不显示。队列给出的状态和原因（Status、Reason）按当前语言显示。 */
function MergeQueueNotice({ mq }: { mq: MergeQueue }) {
  const t = useT()
  const text = useText()
  const e = mq.Entry
  if (!e) return null
  const link = <Link href={mq.URL}>{t('run.mqLink')}</Link>
  if (e.State === 'queued') {
    const detail = [e.BaseRef ? t('run.mqTarget', { branch: e.BaseRef }) : '', t('run.mqPos', { pos: mq.Pos }), text(e.Status)].filter(Boolean).join(' · ')
    return (
      <Callout title={t('run.mqQueued', { detail })}>
        {t('run.mqQueuedText', { pr: mq.PR })}
        {link}
      </Callout>
    )
  }
  if (e.State === 'merged') {
    return (
      <Callout tone="success" title={e.Reason ? text(e.Reason) : (e.MergeSHA ? t('run.mqMergedSHA', { sha: short(e.MergeSHA) }) : t('run.mqMerged'))}>
        {link}
      </Callout>
    )
  }
  return (
    <Callout tone="warning" title={e.Reason ? t('run.mqRemovedReason', { reason: text(e.Reason) }) : t('run.mqRemoved')}>
      {t('run.mqRemovedText')}
      {link}
    </Callout>
  )
}

/** canEnqueue 表示可以把这个 PR 加入合并队列：不在排队中，也没有被合并（PR 是否打开由服务端检查）。 */
function canEnqueue(mq: MergeQueue | null): mq is MergeQueue {
  return mq !== null && mq.Entry?.State !== 'queued' && mq.Entry?.State !== 'merged'
}

/** Notices 是标题下方的提示：合并队列状态、无效的 rerun、正在查看历史、配置错误和警告、已清理。 */
function Notices({ data }: { data: Data }) {
  const t = useT()
  const { locale } = useLocale()
  const r = data.Run
  const latestURL = runPageURL(r.ID, null, data.Back)
  const latestLink = { link: (c: string) => <Link href={latestURL}>{c}</Link> }
  const items: ReactNode[] = []
  if (data.MergeQueue?.Entry) {
    items.push(<MergeQueueNotice key="mq" mq={data.MergeQueue} />)
  }
  if (data.Invalid) {
    const first = rerunLabel(0, locale)
    items.push(
      <Callout key="invalid" tone="critical" title={t('run.invalidTitle', { value: data.Invalid })}>
        <Trans
          id={r.CurrentRerun > 0 ? 'run.invalidRange' : 'run.invalidOne'}
          params={{ count: data.Reruns.length, first, last: rerunLabel(r.CurrentRerun, locale) }}
          tags={latestLink}
        />
      </Callout>,
    )
  } else if (data.History && data.Viewed) {
    const viewed = rerunLabel(data.Viewed.N, locale)
    items.push(
      <Callout
        key="history"
        tone="warning"
        title={data.Viewed.State ? t('run.historyTitleState', { rerun: viewed, state: runState(data.Viewed.State, locale) }) : t('run.historyTitle', { rerun: viewed })}
      >
        <Trans id="run.historyText" params={{ rerun: rerunLabel(r.CurrentRerun, locale), state: runState(r.State, locale) }} tags={latestLink} />
      </Callout>,
    )
  } else {
    if (r.ConfigError !== null) {
      items.push(
        <Callout key="cfg" tone="critical" title={t('run.configError')}>
          <pre className={css.pre}>{r.ConfigError}</pre>
        </Callout>,
      )
    }
    if (r.Warnings !== null) {
      items.push(
        <Callout key="warn" tone="warning" title={t('run.warnings')}>
          <pre className={css.pre}>{r.Warnings}</pre>
        </Callout>,
      )
    }
  }
  if (!data.Invalid && data.Purged !== null) {
    items.push(
      <Callout key="purged" title={t('run.purgedTitle')}>
        {t('run.purgedText', { time: clock(data.Purged, locale) })}
      </Callout>,
    )
  }
  return items.length > 0 ? <div className={css.notices}>{items}</div> : null
}

function gh(base: string, path: string): string {
  return base ? `${base}/${path}` : ''
}

/** Summary 是触发信息的摘要（GitHub 运行页顶部的卡片）：触发方式、状态、耗时、workflow 文件。 */
function Summary({ data }: { data: Data }) {
  const t = useT()
  const { locale } = useLocale()
  const stateText = useStateText()
  const r = data.Run
  const kind = refKind(r.Ref)
  const commitURL = gh(data.GitHub, `${data.Repo}/commit/${r.SHA}`)
  const repoURL = gh(data.GitHub, data.Repo)
  const state = data.Viewed?.State ?? ''
  return (
    <Box className={css.summary}>
      <div className={css.cell}>
        <span className={css.label}>
          {r.PRNumber !== null
            ? t('run.triggeredByPR', { event: eventLabel(r.Event, locale), pr: r.PRNumber })
            : t('run.triggeredBy', { event: eventLabel(r.Event, locale) })}{' '}
          · <Ago t={r.CreatedAt} />
        </span>
        <span className={css.trigger}>
          {r.SenderLogin ? (
            <>
              <Avatar login={r.SenderLogin} size={20} />
              <strong>{r.SenderLogin}</strong>
            </>
          ) : (
            <span className={css.muted}>{r.Source}</span>
          )}
          <span className={css.muted} title={`commit ${r.SHA}`}>
            <GitCommitIcon size={16} aria-hidden />{' '}
            {commitURL ? (
              <a href={commitURL} rel="noreferrer" className="mono">
                {short(r.SHA)}
              </a>
            ) : (
              <code>{short(r.SHA)}</code>
            )}
          </span>
          <RefLabel gitRef={r.Ref} href={kind && data.GitHub ? gh(data.GitHub, `${data.Repo}/tree/${refName(r.Ref)}`) : undefined} />
        </span>
      </div>
      <div className={css.cell}>
        <span className={css.label}>{t('run.state')}</span>
        <span className={css.value}>{data.Viewed ? stateText(state) : '—'}</span>
      </div>
      <div className={css.cell}>
        <span className={css.label}>{t('run.totalDuration')}</span>
        <span className={css.value}>
          <Duration start={data.Start} end={data.End} />
        </span>
      </div>
      <div className={css.cell}>
        <span className={css.label}>{t('run.repoWorkflow')}</span>
        <span className={css.plain}>
          {repoURL ? (
            <a href={repoURL} rel="noreferrer">
              {data.Repo}
            </a>
          ) : (
            data.Repo
          )}
          <br />
          <code>{r.Workflow}</code>
        </span>
      </div>
    </Box>
  )
}

function TriggerIcon({ run: r }: { run: Run }) {
  if (r.Event === 'workflow_dispatch') return <PlayIcon />
  if (r.Event === 'pull_request') return <GitPullRequestIcon />
  if (r.Event === 'schedule') return <ClockIcon />
  return refKind(r.Ref) === 'tag' ? <TagIcon /> : <GitCommitIcon />
}

function ActivityIcon({ activity: a }: { activity: Activity }) {
  if (a.Action === 'run.cancel') return <StopIcon />
  if (a.Action === 'run.rerun') return <SyncIcon />
  return <PlayIcon />
}

/** RunActivity 是这个 run 的活动：谁触发、取消和重跑了它，以及运行结束。 */
function RunActivity({ data }: { data: Data }) {
  const t = useT()
  const { locale } = useLocale()
  const r = data.Run
  return (
    <section aria-labelledby="run-activity-h">
      <h2 className={css.sectionTitle} id="run-activity-h">
        {t('run.activity')}
      </h2>
      <Timeline clipSidebar>
        <Timeline.Item condensed>
          <Timeline.Badge>
            <TriggerIcon run={r} />
          </Timeline.Badge>
          <Timeline.Body className={css.event}>
            {r.SenderLogin && (
              <>
                <Avatar login={r.SenderLogin} size={20} /> <strong>{r.SenderLogin}</strong>{' '}
              </>
            )}
            {r.Event === 'workflow_dispatch' ? t('run.activityDispatch') : t('run.triggered', { event: eventLabel(r.Event, locale) })}
            <span className={css.muted}>
              {' · '}
              <Ago t={r.CreatedAt} />
            </span>
          </Timeline.Body>
        </Timeline.Item>
        {data.Activity.filter((a) => a.Action !== 'run.dispatch').map((a) => {
          return (
            <Timeline.Item key={a.ID} condensed>
              <Timeline.Badge>
                <ActivityIcon activity={a} />
              </Timeline.Badge>
              <Timeline.Body className={css.event}>
                <Avatar login={a.Actor} size={20} /> <strong>{a.Actor}</strong> <Msg text={a.RunText} />
                <span className={css.muted}>
                  {' · '}
                  <Ago t={a.At} />
                </span>
              </Timeline.Body>
            </Timeline.Item>
          )
        })}
        {r.FinishedAt !== null && (
          <Timeline.Item condensed>
            <Timeline.Badge>
              <StatusIcon state={r.State} />
            </Timeline.Badge>
            <Timeline.Body className={css.event}>
              {t('run.finished', { state: runState(r.State, locale) })}
              <span className={css.muted}>
                {' · '}
                <Ago t={r.FinishedAt} />
              </span>
            </Timeline.Body>
          </Timeline.Item>
        )}
      </Timeline>
    </section>
  )
}

function Jobs({ data }: { data: Data }) {
  const t = useT()
  const { locale } = useLocale()
  const r = data.Run
  const stages = data.Stages.length
  return (
    <Box as="section" aria-labelledby="run-jobs-h">
      <BoxHeader
        id="run-jobs-h"
        title={
          <>
            <code className={css.wf}>{r.Workflow}</code>
            <span className={css.on}>
              on: {r.Event}
              {data.History && data.Viewed && ` · ${rerunLabel(data.Viewed.N, locale)}`}
            </span>
          </>
        }
      >
        <span className={css.hint}>
          {data.Jobs.length > 0 ? jobStatesSummary(data.States, locale) : t('run.noJobs')}
          {stages > 1 && t('run.stages', { count: stages })}
        </span>
      </BoxHeader>
      {data.Jobs.length > 0 ? (
        <JobGraph stages={data.Stages} />
      ) : (
        <p className={css.empty}>{t(r.ConfigError !== null ? 'run.emptyJobsConfig' : 'run.emptyJobs')}</p>
      )}
    </Box>
  )
}

/** EnqueueButton 把 PR 加入合并队列（管理员）。 */
function EnqueueButton({ mq }: { mq: MergeQueue }) {
  const t = useT()
  return (
    <ConfirmForm
      action="/merge-queue/add"
      fields={{ repo: mq.Repo, pr: String(mq.PR) }}
      button={t('run.enqueue')}
      buttonProps={{ leadingVisual: GitMergeQueueIcon }}
      title={t('run.enqueueTitle', { pr: mq.PR })}
      confirmLabel={t('run.enqueue')}
      danger={false}
    >
      {t('run.enqueueText')}
    </ConfirmForm>
  )
}

function RunPage({ data, env }: PageProps<Data>) {
  useLive(data.Active || data.MergeQueue?.Entry?.State === 'queued')
  const admin = env.Session?.Admin ?? false
  const t = useT()
  const { locale } = useLocale()
  const r = data.Run
  const back = data.Back || '/runs'
  const viewedState = data.Viewed?.State ?? ''
  return (
    <div className={css.page}>
      <RunHeader
        run={r}
        repo={data.Repo}
        back={back}
        state={data.Viewed ? viewedState : null}
        actions={
          <>
            {data.Reruns.length > 1 && <RerunPicker data={data} />}
            {admin && canEnqueue(data.MergeQueue) && <EnqueueButton mq={data.MergeQueue} />}
            {data.Active ? (
              <ConfirmForm
                action={`/runs/${r.ID}/cancel`}
                button={t('run.cancel')}
                buttonProps={{ leadingVisual: StopIcon }}
                title={t('run.cancelTitle')}
                confirmLabel={t('run.cancelConfirm')}
                cancelLabel={t('run.cancelBack')}
              >
                {t('run.cancelText', { rerun: rerunLabel(r.CurrentRerun, locale) })}
              </ConfirmForm>
            ) : (
              data.Form && <RerunButton runID={r.ID} form={data.Form} current={r.CurrentRerun} state={r.State} />
            )}
          </>
        }
      />

      <div className={css.layout}>
        <aside className={css.side}>
          <RunSidebar summaryURL={runPageURL(r.ID, data.History && data.Viewed ? data.Viewed.N : null, data.Back)} jobs={data.Jobs} current="summary" />
        </aside>
        <div className={css.main}>
          <Notices data={data} />
          <Summary data={data} />
          {!data.Invalid && <Jobs data={data} />}
          <RunActivity data={data} />
        </div>
      </div>
    </div>
  )
}

export default definePage(schema, RunPage)
