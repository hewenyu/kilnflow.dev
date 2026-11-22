import { ArrowLeftIcon } from '@primer/octicons-react'
import { CounterLabel, Label, Link, LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxFooter, BoxHeader, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { ConfirmForm } from '../components/Form'
import { PageHeader } from '../components/PageHeader'
import { StatusIcon } from '../components/StatusIcon'
import { Duration } from '../components/Time'
import { list } from '../lib/envelope'
import { labelVariant, outcome, outcomeTone } from '../lib/format'
import { AIDevelopSection } from './ai/AIDevelop'
import { AIReviewSection } from './ai/AIReview'
import { TaskDesignBasis } from './ai/DesignBasis'
import { taskDocsSchema } from './ai/docs'
import { OrcaStatus } from './ai/orca'
import { CancelRequest, requestTitle, TaskState, unix } from './ai/parts'
import { aiAttemptSchema, aiDevelopSchema, aiRequestSchema, aiRequestURL, aiReviewSchema, aiTaskSchema, aiTaskURL, taskPlatform, type AIAttempt } from './ai/schema'
import { LogViewer } from './job/LogViewer'
import { SpecSection } from './job/sections'
import css from './ai/ai.module.css'
import jobCss from './job.module.css'

// AI 任务详情页（PLAN #30.9）：左侧是同一个请求中的任务，右侧是任务的信息、实时日志、审计报告或开发报告、每次 attempt 和
// 冻结的执行输入。任务没有结束时每 5 秒刷新页面数据，日志自己轮询（与 job 页面共用日志组件）。

const schema = z.object({
  Task: aiTaskSchema,
  Request: aiRequestSchema,
  Tasks: list(aiTaskSchema),
  Attempts: list(aiAttemptSchema),
  /** 日志读取的 attempt（地址中指定的或当前的）；还没有 attempt 时为 0。 */
  Attempt: z.number(),
  Explicit: z.boolean(),
  /** 地址中指定的、不是当前的那次 attempt。 */
  Viewing: aiAttemptSchema.nullable(),
  /** 地址中的 attempt 无效（页面状态码 404，不加载日志）。 */
  AttemptErr: z.string(),
  Terminal: z.boolean(),
  RequestTerminal: z.boolean(),
  /** 可以只重跑这个任务（已结束的审计请求中的任务）。 */
  CanRerun: z.boolean(),
  /** 没有执行过、也不会再有日志。 */
  NoLog: z.boolean(),
  LogEmpty: z.string(),
  AttemptKey: z.string(),
  /** 冻结的执行输入（JSON，缩进过）。 */
  Spec: z.string(),
  Review: aiReviewSchema.nullable(),
  Develop: aiDevelopSchema.nullable(),
  /** 这个任务实际可读的设计依据；历史页面的数据没有这一项时为 null。 */
  DesignBasis: taskDocsSchema.nullable().default(null),
})
type Data = z.infer<typeof schema>

function TasksNav({ data }: { data: Data }) {
  return (
    <nav aria-label="同一个请求中的 AI 任务">
      <ul className={css.nav}>
        <li>
          <a href={aiRequestURL(data.Request.ID)}>概要</a>
        </li>
        {data.Tasks.map((t) => (
          <li key={t.ID} aria-current={t.ID === data.Task.ID ? 'page' : undefined}>
            <StatusIcon state={t.State} size={14} label={t.StateTitle} />
            <a href={aiTaskURL(t.ID)}>{t.DisplayName}</a>
          </li>
        ))}
      </ul>
    </nav>
  )
}

function Header({ data }: { data: Data }) {
  const t = data.Task
  return (
    <header className={jobCss.header}>
      <h2 className={jobCss.title}>
        <StatusIcon state={t.State} size={20} label={t.StateTitle} />
        <span className={jobCss.titleText}>{t.DisplayName}</span>
        <span className={jobCss.titleID}>#{t.ID}</span>
      </h2>
      <ul className={jobCss.meta} aria-label="AI 任务的信息">
        <li className={jobCss.metaState}>
          <TaskState t={t} />
        </li>
        {t.Started && (
          <li title="耗时">
            <Duration start={unix(t.Started)} end={unix(t.Finished)} />
          </li>
        )}
        <li title={t.Platform ? '执行的平台' : '可以执行的平台'}>
          <code>{taskPlatform(t)}</code>
        </li>
        {t.Agent && <li title="Agent">{t.Agent}</li>}
        {t.Runner && <li title="AI 执行节点">{t.Runner}</li>}
      </ul>
      {t.Reason && <p className={t.State === 'failed' || t.State === 'timed_out' ? css.bad : css.muted}>{t.Reason}</p>}
    </header>
  )
}

function AttemptRow({ a, taskID, viewing }: { a: AIAttempt; taskID: number; viewing: boolean }) {
  const tone = outcomeTone(a.Outcome)
  const running = a.Outcome === ''
  const versions = [a.RunnerVersion && `kiln-orca ${a.RunnerVersion}`, a.AgentVersion && `Agent ${a.AgentVersion}`, a.AgentModel && `模型 ${a.AgentModel}`]
    .filter(Boolean)
    .join('，')
  return (
    <BoxRow className={`${jobCss.attempt} ${viewing ? jobCss.attemptViewing ?? '' : ''}`} aria-current={viewing ? 'true' : undefined}>
      <StatusIcon state={a.Outcome || 'running'} tone={tone} label={outcome(a.Outcome)} />
      <div className={jobCss.attemptMain}>
        <div className={jobCss.attemptTitle}>
          <strong>第 {a.N} 次</strong>
          <Label variant={labelVariant(tone)}>{outcome(a.Outcome)}</Label>
          {a.Current && <Label variant="accent">当前</Label>}
        </div>
        <p className={jobCss.attemptMeta}>
          {a.Runner || '—'}
          {a.Platform && (
            <>
              {' '}
              <code>{a.Platform}</code>
            </>
          )}
          ，<Duration start={a.Started} end={a.Finished} />
          {a.ErrorClass && `，${a.ErrorClass}`} <code>#{a.ID} · epoch {a.Epoch}</code>
        </p>
        {versions && <p className={jobCss.attemptMeta}>{versions}</p>}
        {/* 进度只对执行中的 attempt 有意义；Orca 中的关联一直显示，用于在 Orca 中找到这次执行的会话和 worktree */}
        <OrcaStatus
          stage={running ? a.Stage : ''}
          text={running ? a.Text : ''}
          orca={a.Orca}
          startRequest={a.Orca.start_request_id}
          className={jobCss.attemptMeta}
        />
        {a.Message && <p className={jobCss.attemptMsg}>{a.Message}</p>}
      </div>
      <div className={jobCss.attemptAction}>
        {viewing ? <span className={css.muted}>正在查看日志</span> : <Link href={`${aiTaskURL(taskID)}?attempt=${a.ID}`}>查看日志</Link>}
      </div>
    </BoxRow>
  )
}

function Attempts({ data }: { data: Data }) {
  if (data.Attempts.length === 0 && data.Terminal) return null
  return (
    <Box as="section" aria-labelledby="attempts-h">
      <BoxHeader
        title={
          <>
            attempt {data.Attempts.length > 0 && <CounterLabel>{data.Attempts.length}</CounterLabel>}
          </>
        }
        id="attempts-h"
      />
      {data.Attempts.length === 0 ? (
        <BoxRow className={css.muted}>还没有开始执行。AI 执行节点领取后这里会列出每次 attempt，日志也会自动出现。</BoxRow>
      ) : (
        <>
          {data.Attempts.map((a) => (
            <AttemptRow key={a.ID} a={a} taskID={data.Task.ID} viewing={a.ID === data.Attempt} />
          ))}
          <BoxFooter className={jobCss.footNote}>AI 任务领取后不自动重试；重跑会新建请求，不算在这里。</BoxFooter>
        </>
      )}
    </Box>
  )
}

function RerunTask({ data }: { data: Data }) {
  const t = data.Task
  return (
    <ConfirmForm
      action={`${aiTaskURL(t.ID)}/rerun`}
      button="重跑这个任务…"
      buttonProps={{ size: 'small' }}
      danger={false}
      title={`重跑 ${t.DisplayName}？`}
      confirmLabel="重跑"
    >
      新建一个 AI 审计请求：只重新执行 {t.DisplayName}，其余任务沿用{requestTitle(data.Request)}的结果。
    </ConfirmForm>
  )
}

function Log({ data }: { data: Data }) {
  if (data.AttemptErr) return <p className={jobCss.logBlocked}>选择一次有效的 attempt 后显示日志。</p>
  if (data.NoLog) {
    return (
      <section className={jobCss.noLog} aria-labelledby="log-h">
        <h2 id="log-h" className={jobCss.noLogTitle}>
          日志
        </h2>
        <p>{data.LogEmpty}</p>
      </section>
    )
  }
  return (
    <LogViewer
      base={aiTaskURL(data.Task.ID)}
      attempt={data.Attempt}
      explicit={data.Explicit}
      attemptKey={data.AttemptKey}
      state={data.Task.State}
      emptyText={data.LogEmpty}
      note={data.Viewing ? `第 ${data.Viewing.N} 次 attempt` : undefined}
    />
  )
}

function AITaskPage({ data }: PageProps<Data>) {
  const r = data.Request
  useLive(!data.Terminal || !data.RequestTerminal)
  return (
    <div className={jobCss.page}>
      <PageHeader
        title={requestTitle(r)}
        description={`${r.Repo} · ${r.PRNumber ? `PR #${r.PRNumber}` : r.Ref}`}
        actions={
          <>
            <LinkButton href={aiRequestURL(r.ID)} leadingVisual={ArrowLeftIcon}>
              请求概要
            </LinkButton>
            {!data.RequestTerminal && <CancelRequest r={r} />}
            {data.CanRerun && <RerunTask data={data} />}
          </>
        }
      />
      <div className={jobCss.layout}>
        <aside className={jobCss.side}>
          <TasksNav data={data} />
        </aside>
        <div className={jobCss.content}>
          <Header data={data} />
          {data.AttemptErr && (
            <Callout tone="critical" title="地址中的 attempt 无效">
              {data.AttemptErr} <Link href={aiTaskURL(data.Task.ID)}>查看当前 attempt</Link>
            </Callout>
          )}
          {data.Viewing && (
            <Callout tone="info" title={`正在查看第 ${data.Viewing.N} 次 attempt`}>
              这不是任务当前的 attempt。<Link href={aiTaskURL(data.Task.ID)}>查看当前 attempt</Link>
            </Callout>
          )}
          <Log data={data} />
          {r.Type === 'develop' ? <AIDevelopSection dev={data.Develop} /> : <AIReviewSection review={data.Review} />}
          {data.DesignBasis && <TaskDesignBasis view={data.DesignBasis} />}
          <Attempts data={data} />
          <SpecSection spec={data.Spec} title="执行输入（spec）" />
        </div>
      </div>
    </div>
  )
}

export default definePage(schema, AITaskPage)
