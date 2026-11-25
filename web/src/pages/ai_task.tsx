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
import { Msg, useLocale, useT } from '../i18n/locale'
import { isEmpty } from '../i18n/messages'
import { list, textSchema } from '../lib/envelope'
import { labelVariant, outcome, outcomeTone } from '../lib/format'
import { AIDevelopSection } from './ai/AIDevelop'
import { AIReviewSection } from './ai/AIReview'
import { TaskDesignBasis } from './ai/DesignBasis'
import { taskDocsSchema } from './ai/docs'
import { OrcaStatus } from './ai/orca'
import { CancelRequest, requestTitle, TaskState, taskState, unix } from './ai/parts'
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
  /** 地址中的 attempt 无效的说明（服务端给的消息；页面状态码 404，不加载日志）。 */
  AttemptErr: textSchema,
  Terminal: z.boolean(),
  RequestTerminal: z.boolean(),
  /** 可以只重跑这个任务（已结束的审计请求中的任务）。 */
  CanRerun: z.boolean(),
  /** 没有执行过、也不会再有日志。 */
  NoLog: z.boolean(),
  /** 没有日志时的说明（服务端给的消息）。 */
  LogEmpty: textSchema,
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
  const t = useT()
  const { locale } = useLocale()
  return (
    <nav aria-label={t('aiTask.navLabel')}>
      <ul className={css.nav}>
        <li>
          <a href={aiRequestURL(data.Request.ID)}>{t('run.summary')}</a>
        </li>
        {data.Tasks.map((task) => (
          <li key={task.ID} aria-current={task.ID === data.Task.ID ? 'page' : undefined}>
            <StatusIcon state={task.State} size={14} label={taskState(task.State, locale)} />
            <a href={aiTaskURL(task.ID)}>{task.DisplayName}</a>
          </li>
        ))}
      </ul>
    </nav>
  )
}

function Header({ data }: { data: Data }) {
  const t = useT()
  const { locale } = useLocale()
  const task = data.Task
  return (
    <header className={jobCss.header}>
      <h2 className={jobCss.title}>
        <StatusIcon state={task.State} size={20} label={taskState(task.State, locale)} />
        <span className={jobCss.titleText}>{task.DisplayName}</span>
        <span className={jobCss.titleID}>#{task.ID}</span>
      </h2>
      <ul className={jobCss.meta} aria-label={t('aiTask.metaLabel')}>
        <li className={jobCss.metaState}>
          <TaskState t={task} />
        </li>
        {task.Started && (
          <li title={t('aiTasks.colDuration')}>
            <Duration start={unix(task.Started)} end={unix(task.Finished)} />
          </li>
        )}
        <li title={t(task.Platform ? 'aiTask.platformRan' : 'aiTask.platformCan')}>
          <code>{taskPlatform(task, locale)}</code>
        </li>
        {task.Agent && <li title="Agent">{task.Agent}</li>}
        {task.Runner && <li title={t('aiTasks.colRunner')}>{task.Runner}</li>}
      </ul>
      {task.Reason && <p className={task.State === 'failed' || task.State === 'timed_out' ? css.bad : css.muted}>{task.Reason}</p>}
    </header>
  )
}

function AttemptRow({ a, taskID, viewing }: { a: AIAttempt; taskID: number; viewing: boolean }) {
  const t = useT()
  const { locale } = useLocale()
  const tone = outcomeTone(a.Outcome)
  const running = a.Outcome === ''
  const sep = t('list.separator')
  const versions = [a.RunnerVersion && `kiln-orca ${a.RunnerVersion}`, a.AgentVersion && `Agent ${a.AgentVersion}`, a.AgentModel && t('aiTask.model', { model: a.AgentModel })]
    .filter(Boolean)
    .join(sep)
  return (
    <BoxRow className={`${jobCss.attempt} ${viewing ? jobCss.attemptViewing ?? '' : ''}`} aria-current={viewing ? 'true' : undefined}>
      <StatusIcon state={a.Outcome || 'running'} tone={tone} label={outcome(a.Outcome, locale)} />
      <div className={jobCss.attemptMain}>
        <div className={jobCss.attemptTitle}>
          <strong>{t('job.attemptN', { n: a.N })}</strong>
          <Label variant={labelVariant(tone)}>{outcome(a.Outcome, locale)}</Label>
          {a.Current && <Label variant="accent">{t('job.attemptCurrent')}</Label>}
        </div>
        <p className={jobCss.attemptMeta}>
          {a.Runner || '—'}
          {a.Platform && (
            <>
              {' '}
              <code>{a.Platform}</code>
            </>
          )}
          {sep}
          <Duration start={a.Started} end={a.Finished} />
          {a.ErrorClass && `${sep}${a.ErrorClass}`} <code>#{a.ID} · epoch {a.Epoch}</code>
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
        {viewing ? (
          <span className={css.muted}>{t('job.attemptViewing')}</span>
        ) : (
          <Link href={`${aiTaskURL(taskID)}?attempt=${a.ID}`}>{t('job.attemptViewLog')}</Link>
        )}
      </div>
    </BoxRow>
  )
}

function Attempts({ data }: { data: Data }) {
  const t = useT()
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
        <BoxRow className={css.muted}>{t('aiTask.attemptsEmpty')}</BoxRow>
      ) : (
        <>
          {data.Attempts.map((a) => (
            <AttemptRow key={a.ID} a={a} taskID={data.Task.ID} viewing={a.ID === data.Attempt} />
          ))}
          <BoxFooter className={jobCss.footNote}>{t('aiTask.attemptsNote')}</BoxFooter>
        </>
      )}
    </Box>
  )
}

function RerunTask({ data }: { data: Data }) {
  const t = useT()
  const { locale } = useLocale()
  const task = data.Task
  return (
    <ConfirmForm
      action={`${aiTaskURL(task.ID)}/rerun`}
      button={t('aiTask.rerunButton')}
      buttonProps={{ size: 'small' }}
      danger={false}
      title={t('aiTask.rerunTitle', { name: task.DisplayName })}
      confirmLabel={t('aiReq.rerunConfirm')}
    >
      {t('aiTask.rerunBody', { name: task.DisplayName, title: requestTitle(data.Request, locale) })}
    </ConfirmForm>
  )
}

function Log({ data }: { data: Data }) {
  const t = useT()
  if (!isEmpty(data.AttemptErr)) return <p className={jobCss.logBlocked}>{t('aiTask.logBlocked')}</p>
  if (data.NoLog) {
    return (
      <section className={jobCss.noLog} aria-labelledby="log-h">
        <h2 id="log-h" className={jobCss.noLogTitle}>
          {t('logView.title')}
        </h2>
        <p>
          <Msg text={data.LogEmpty} />
        </p>
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
      note={data.Viewing ? t('aiTask.attemptNote', { n: data.Viewing.N }) : undefined}
    />
  )
}

function AITaskPage({ data }: PageProps<Data>) {
  const t = useT()
  const { locale } = useLocale()
  const r = data.Request
  useLive(!data.Terminal || !data.RequestTerminal)
  return (
    <div className={jobCss.page}>
      <PageHeader
        title={requestTitle(r, locale)}
        description={`${r.Repo} · ${r.PRNumber ? `PR #${r.PRNumber}` : r.Ref}`}
        actions={
          <>
            <LinkButton href={aiRequestURL(r.ID)} leadingVisual={ArrowLeftIcon}>
              {t('aiTask.requestSummary')}
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
          {!isEmpty(data.AttemptErr) && (
            <Callout tone="critical" title={t('aiTask.attemptInvalidTitle')}>
              <Msg text={data.AttemptErr} /> <Link href={aiTaskURL(data.Task.ID)}>{t('aiTask.viewCurrent')}</Link>
            </Callout>
          )}
          {data.Viewing && (
            <Callout tone="info" title={t('aiTask.viewingTitle', { n: data.Viewing.N })}>
              {t('aiTask.notCurrent')} <Link href={aiTaskURL(data.Task.ID)}>{t('aiTask.viewCurrent')}</Link>
            </Callout>
          )}
          <Log data={data} />
          {r.Type === 'develop' ? <AIDevelopSection dev={data.Develop} /> : <AIReviewSection review={data.Review} />}
          {data.DesignBasis && <TaskDesignBasis view={data.DesignBasis} />}
          <Attempts data={data} />
          <SpecSection spec={data.Spec} title={t('aiTask.spec')} />
        </div>
      </div>
    </div>
  )
}

export default definePage(schema, AITaskPage)
