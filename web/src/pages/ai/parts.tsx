import { Label } from '@primer/react'
import { ConfirmForm } from '../../components/Form'
import { StatusIcon } from '../../components/StatusIcon'
import { Ago, Duration } from '../../components/Time'
import { Trans, useLocale, useT } from '../../i18n/locale'
import { defaultLocale, msg, translate, type Locale } from '../../i18n/messages'
import { enumLabel, eventLabel, short, toDate } from '../../lib/format'
import table from '../settings_people.module.css'
import css from './ai.module.css'
import { aiRequestURL, aiTaskURL, taskPlatform, type AIRequest, type AITask } from './schema'

// AI 请求详情页和 AI 任务详情页共用的部分：请求的信息、任务列表、重跑和取消。界面文字按语言显示；仓库、ref、任务名、
// 原因等服务端记下的内容原样显示。

/** unix 把 ISO 时间换成 Unix 秒（为空或 Go 的零值时间时为 0），给 Duration 用。 */
export function unix(t: string | null): number {
  const d = toDate(t)
  return d ? Math.floor(d.getTime() / 1000) : 0
}

/** requestState 返回 AI 请求状态的名称（与服务端的筛选选项一致：active 是"进行中"；locale 不传时为 zh-CN）。 */
export function requestState(state: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'aiRequestState', state)
}

/** taskState 返回 AI 任务状态的名称；未知的状态原样返回。 */
export function taskState(state: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'aiTaskState', state)
}

/** devModeText 返回 AI 开发的方式（select 选题、implement 实现、fix 返修）的名称；未知的原样返回。 */
export function devModeText(mode: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'aiDevMode', mode)
}

/** requestTitle 返回请求的标题，例如"AI 审计请求 #88"（与服务端给的页面标题相同）。 */
export function requestTitle(r: Pick<AIRequest, 'Type' | 'ID'>, locale: Locale = defaultLocale): string {
  return translate(locale, msg(r.Type === 'develop' ? 'aiRequest.pageTitleDevelop' : 'aiRequest.pageTitleReview', { id: r.ID }))
}

/** RequestFacts 是请求的信息：仓库、事件、ref、commit、PR 或工作项、发起人、时间、重跑来源。 */
export function RequestFacts({ r, prURL }: { r: AIRequest; prURL?: string }) {
  const t = useT()
  const { locale } = useLocale()
  const ago = { ago: () => <Ago t={r.Created} /> }
  return (
    <dl className={css.kv}>
      <dt>{t('aiReq.state')}</dt>
      <dd>
        <span className={css.state}>
          <StatusIcon state={r.State} size={14} />
          {requestState(r.State, locale)}
        </span>
      </dd>
      <dt>{t('aiReq.repo')}</dt>
      <dd>{r.Repo}</dd>
      <dt>{t('aiReq.event')}</dt>
      <dd>
        {eventLabel(r.Event, locale)}
        {r.Trigger && <span className={css.muted}>{t('aiReq.trigger', { trigger: r.Trigger })}</span>}
        {r.Sender && <span className={css.muted}>{t('aiReq.sender', { sender: r.Sender })}</span>}
      </dd>
      <dt>ref</dt>
      <dd className="mono">{r.Ref}</dd>
      <dt>commit</dt>
      <dd className="mono" title={r.SHA}>
        {short(r.SHA)}
      </dd>
      {r.PRNumber !== 0 && (
        <>
          <dt>PR</dt>
          <dd>{prURL ? <a href={prURL}>#{r.PRNumber}</a> : `#${r.PRNumber}`}</dd>
        </>
      )}
      {r.WorkItemURL && (
        <>
          <dt>{t('aiReq.workItem')}</dt>
          <dd>
            <a href={r.WorkItemURL}>#{r.WorkItemID}</a>
            {r.DevMode && (
              <span className={css.muted}>
                {r.DevMode === 'fix'
                  ? t('aiReq.devModeRound', { mode: devModeText(r.DevMode, locale), round: r.DevRound })
                  : t('aiReq.devMode', { mode: devModeText(r.DevMode, locale) })}
              </span>
            )}
          </dd>
        </>
      )}
      {r.RerunOf !== 0 && (
        <>
          <dt>{t('aiReq.rerunOf')}</dt>
          <dd>
            <a href={aiRequestURL(r.RerunOf)}>#{r.RerunOf}</a>
          </dd>
        </>
      )}
      <dt>{t('aiReq.time')}</dt>
      <dd>
        <Trans id="aiReq.created" tags={ago} />
        {r.Finished && <Trans id="aiReq.finished" tags={{ ago: () => <Ago t={r.Finished} /> }} />}
        {r.Purged && <span className={css.muted}>{t('aiReq.purged')}</span>}
      </dd>
    </dl>
  )
}

/** TaskState 是任务状态的图标和文字；沿用结果、请求取消时补充说明。 */
export function TaskState({ t: task }: { t: AITask }) {
  const t = useT()
  const { locale } = useLocale()
  const name = taskState(task.State, locale)
  return (
    <span className={css.state}>
      <StatusIcon state={task.State} size={14} label={name} />
      {name}
      {task.ReusedFrom !== 0 && (
        <Label variant="secondary" title={t('aiTask.reusedTitle')}>
          {t('aiTask.reused', { id: task.ReusedFrom })}
        </Label>
      )}
      {task.CancelRequested && task.State === 'running' && <Label variant="attention">{t('aiTask.cancelling')}</Label>}
    </span>
  )
}

/** TasksTable 是请求中的 AI 任务（每个平台一个）。 */
export function TasksTable({ tasks }: { tasks: AITask[] }) {
  const t = useT()
  const { locale } = useLocale()
  const col = { task: t('aiTasks.colTask'), state: t('aiTasks.colState'), platform: t('aiTasks.colPlatform'), runner: t('aiTasks.colRunner'), duration: t('aiTasks.colDuration') }
  return (
    <table className={table.table} aria-label={t('aiTasks.label')}>
      <thead>
        <tr>
          <th scope="col">{col.task}</th>
          <th scope="col">{col.state}</th>
          <th scope="col">{col.platform}</th>
          <th scope="col">Agent</th>
          <th scope="col">{col.runner}</th>
          <th scope="col">{col.duration}</th>
        </tr>
      </thead>
      <tbody>
        {tasks.map((task) => (
          <tr key={task.ID}>
            <td data-label={col.task}>
              <a href={aiTaskURL(task.ID)}>{task.DisplayName}</a>
              {task.Reason && <div className={task.State === 'failed' || task.State === 'timed_out' ? table.err : css.muted}>{task.Reason}</div>}
            </td>
            <td data-label={col.state}>
              <TaskState t={task} />
            </td>
            <td data-label={col.platform} className={table.mono}>
              {taskPlatform(task, locale)}
            </td>
            <td data-label="Agent">{task.Agent || '—'}</td>
            <td data-label={col.runner}>{task.Runner || '—'}</td>
            <td data-label={col.duration} className={table.nowrap}>
              <Duration start={unix(task.Started)} end={unix(task.Finished)} />
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

/** CancelRequest 是取消请求的表单（请求结束前）。 */
export function CancelRequest({ r }: { r: AIRequest }) {
  const t = useT()
  const { locale } = useLocale()
  return (
    <ConfirmForm
      action={`${aiRequestURL(r.ID)}/cancel`}
      button={t('aiReq.cancelButton')}
      buttonProps={{ size: 'small' }}
      title={t('aiReq.cancelTitle', { title: requestTitle(r, locale) })}
      confirmLabel={t('aiReq.cancelConfirm')}
      cancelLabel={t('run.cancelBack')}
    >
      {t('aiReq.cancelBody')}
    </ConfirmForm>
  )
}

/** RerunRequest 是重跑已结束的审计请求：只重跑没有成功的任务（有时），或全部重跑。重跑会新建一个请求。 */
export function RerunRequest({ r, hasFailed }: { r: AIRequest; hasFailed: boolean }) {
  const t = useT()
  const { locale } = useLocale()
  const action = `${aiRequestURL(r.ID)}/rerun`
  const title = requestTitle(r, locale)
  return (
    <>
      {hasFailed && (
        <ConfirmForm
          action={action}
          fields={{ mode: 'failed' }}
          button={t('aiReq.rerunFailedButton')}
          buttonProps={{ size: 'small', variant: 'primary' }}
          danger={false}
          title={t('aiReq.rerunFailedTitle', { title })}
          confirmLabel={t('aiReq.rerunConfirm')}
        >
          {t('aiReq.rerunFailedBody')}
        </ConfirmForm>
      )}
      <ConfirmForm
        action={action}
        fields={{ mode: 'all' }}
        button={t('aiReq.rerunAllButton')}
        buttonProps={{ size: 'small' }}
        danger={false}
        title={t('aiReq.rerunAllTitle', { title })}
        confirmLabel={t('aiReq.rerunAllConfirm')}
      >
        {t('aiReq.rerunAllBody')}
      </ConfirmForm>
    </>
  )
}
