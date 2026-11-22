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
import { list } from '../lib/envelope'
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
  /** 这条活动在 run 详情页上的说明（不含操作者）。 */
  RunText: z.string(),
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

const stateText = (s: string) => (s ? runState(s) : '状态未知')

/** RerunPicker 切换查看这个 run 的每一次运行（GitHub 的 "Latest #2" 菜单）。 */
function RerunPicker({ data }: { data: Data }) {
  const r = data.Run
  const viewed = data.Viewed
  return (
    <ActionMenu>
      <ActionMenu.Button aria-label={`正在查看${viewed ? rerunLabel(viewed.N) : '的运行无效'}，切换查看其他运行`}>
        {viewed ? rerunLabel(viewed.N) : `rerun=${data.Invalid}`}
        {viewed?.N === r.CurrentRerun && <span className={css.latest}>最新</span>}
      </ActionMenu.Button>
      <ActionMenu.Overlay width="medium">
        <ActionList aria-label="这个 run 的每一次运行">
          {data.Reruns.toReversed().map((v) => (
            <ActionList.LinkItem key={v.N} href={runPageURL(r.ID, v.N === r.CurrentRerun ? null : v.N, data.Back)} active={viewed?.N === v.N} aria-current={viewed?.N === v.N ? 'page' : undefined}>
              <ActionList.LeadingVisual>
                <StatusIcon state={v.State} label={stateText(v.State)} />
              </ActionList.LeadingVisual>
              {rerunLabel(v.N)}
              <ActionList.TrailingVisual>
                {v.N === r.CurrentRerun ? '最新' : viewed?.N === v.N ? <CheckIcon /> : null}
              </ActionList.TrailingVisual>
            </ActionList.LinkItem>
          ))}
        </ActionList>
      </ActionMenu.Overlay>
    </ActionMenu>
  )
}

/** MergeQueueNotice 是 PR 在合并队列中的状态；没有记录时不显示。 */
function MergeQueueNotice({ mq }: { mq: MergeQueue }) {
  const e = mq.Entry
  if (!e) return null
  const link = <Link href={mq.URL}>查看合并队列</Link>
  if (e.State === 'queued') {
    return (
      <Callout title={`合并队列：${e.BaseRef ? `目标 ${e.BaseRef} · ` : ''}第 ${mq.Pos} 个${e.Status ? ` · ${e.Status}` : ''}`}>
        PR #{mq.PR} 在合并队列中，验证通过且不落后于目标分支时自动合并。{link}
      </Callout>
    )
  }
  if (e.State === 'merged') {
    return (
      <Callout tone="success" title={e.Reason || `已由合并队列合并${e.MergeSHA ? `（${short(e.MergeSHA)}）` : ''}`}>
        {link}
      </Callout>
    )
  }
  return (
    <Callout tone="warning" title={`已移出合并队列${e.Reason ? `：${e.Reason}` : ''}`}>
      重新加上 label 或在合并队列页加入即可再试。{link}
    </Callout>
  )
}

/** canEnqueue 表示可以把这个 PR 加入合并队列：不在排队中，也没有被合并（PR 是否打开由服务端检查）。 */
function canEnqueue(mq: MergeQueue | null): mq is MergeQueue {
  return mq !== null && mq.Entry?.State !== 'queued' && mq.Entry?.State !== 'merged'
}

/** Notices 是标题下方的提示：合并队列状态、无效的 rerun、正在查看历史、配置错误和警告、已清理。 */
function Notices({ data }: { data: Data }) {
  const r = data.Run
  const latestURL = runPageURL(r.ID, null, data.Back)
  const items: ReactNode[] = []
  if (data.MergeQueue?.Entry) {
    items.push(<MergeQueueNotice key="mq" mq={data.MergeQueue} />)
  }
  if (data.Invalid) {
    items.push(
      <Callout key="invalid" tone="critical" title={`没有这一次运行（rerun=${data.Invalid}）。`}>
        这个 run 共运行了 {data.Reruns.length} 次：{rerunLabel(0)}
        {r.CurrentRerun > 0 && `到${rerunLabel(r.CurrentRerun)}`}。<Link href={latestURL}>查看最新一次</Link>
      </Callout>,
    )
  } else if (data.History && data.Viewed) {
    items.push(
      <Callout key="history" tone="warning" title={`正在查看历史：${rerunLabel(data.Viewed.N)}${data.Viewed.State ? `（${runState(data.Viewed.State)}）` : ''}`}>
        这个 run 最新的是{rerunLabel(r.CurrentRerun)}，{runState(r.State)}。<Link href={latestURL}>查看最新一次</Link>。右上角的取消和重跑都作用于最新一次运行。
      </Callout>,
    )
  } else {
    if (r.ConfigError !== null) {
      items.push(
        <Callout key="cfg" tone="critical" title="配置错误">
          <pre className={css.pre}>{r.ConfigError}</pre>
        </Callout>,
      )
    }
    if (r.Warnings !== null) {
      items.push(
        <Callout key="warn" tone="warning" title="警告">
          <pre className={css.pre}>{r.Warnings}</pre>
        </Callout>,
      )
    }
  }
  if (!data.Invalid && data.Purged !== null) {
    items.push(
      <Callout key="purged" title="日志和产物已清理。">
        这次运行结束超过保留期，已在 {clock(data.Purged)} 按保留策略清理了日志和产物，只保留结果（状态、耗时、原因）。
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
  const r = data.Run
  const kind = refKind(r.Ref)
  const commitURL = gh(data.GitHub, `${data.Repo}/commit/${r.SHA}`)
  const repoURL = gh(data.GitHub, data.Repo)
  const state = data.Viewed?.State ?? ''
  return (
    <Box className={css.summary}>
      <div className={css.cell}>
        <span className={css.label}>
          通过{eventLabel(r.Event)}
          {r.PRNumber !== null && ` #${r.PRNumber}`}触发 · <Ago t={r.CreatedAt} />
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
        <span className={css.label}>状态</span>
        <span className={css.value}>{data.Viewed ? stateText(state) : '—'}</span>
      </div>
      <div className={css.cell}>
        <span className={css.label}>总耗时</span>
        <span className={css.value}>
          <Duration start={data.Start} end={data.End} />
        </span>
      </div>
      <div className={css.cell}>
        <span className={css.label}>仓库 · workflow 文件</span>
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
  const r = data.Run
  return (
    <section aria-labelledby="run-activity-h">
      <h2 className={css.sectionTitle} id="run-activity-h">
        活动
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
            {r.Event === 'workflow_dispatch' ? '手动启动了这个 run' : `触发了这个 run（${eventLabel(r.Event)}）`}
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
                <Avatar login={a.Actor} size={20} /> <strong>{a.Actor}</strong> {a.RunText}
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
              运行结束：{runState(r.State)}
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
              {data.History && data.Viewed && ` · ${rerunLabel(data.Viewed.N)}`}
            </span>
          </>
        }
      >
        <span className={css.hint}>
          {data.Jobs.length > 0 ? jobStatesSummary(data.States) : '没有 job'}
          {stages > 1 && `；按依赖分成 ${stages} 个阶段，从左到右执行`}
        </span>
      </BoxHeader>
      {data.Jobs.length > 0 ? (
        <JobGraph stages={data.Stages} />
      ) : (
        <p className={css.empty}>这次运行没有 job{r.ConfigError !== null && '（配置错误）'}。</p>
      )}
    </Box>
  )
}

/** EnqueueButton 把 PR 加入合并队列（管理员）。 */
function EnqueueButton({ mq }: { mq: MergeQueue }) {
  return (
    <ConfirmForm
      action="/merge-queue/add"
      fields={{ repo: mq.Repo, pr: String(mq.PR) }}
      button="加入合并队列"
      buttonProps={{ leadingVisual: GitMergeQueueIcon }}
      title={`把 PR #${mq.PR} 加入合并队列？`}
      confirmLabel="加入合并队列"
      danger={false}
    >
      Kiln 会给 PR 加上 label kiln:merge，按入队顺序验证它的 head：CI 和 AI 审计都通过、且不落后于目标分支时自动合并；落后时先更新 PR 分支，等新的 head 验证通过再合并。
    </ConfirmForm>
  )
}

function RunPage({ data, env }: PageProps<Data>) {
  useLive(data.Active || data.MergeQueue?.Entry?.State === 'queued')
  const admin = env.Session?.Admin ?? false
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
                button="取消运行"
                buttonProps={{ leadingVisual: StopIcon }}
                title="取消这个 run？"
                confirmLabel="取消这个 run"
                cancelLabel="返回"
              >
                取消这个 run 的{rerunLabel(r.CurrentRerun)}：正在执行的 job 会被终止，还没有开始的 job 不再执行。已经完成的 job 不受影响，已经发布的 Release 或镜像不会撤回。
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
