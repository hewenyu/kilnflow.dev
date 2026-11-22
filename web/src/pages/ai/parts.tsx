import { Label } from '@primer/react'
import { ConfirmForm } from '../../components/Form'
import { StatusIcon } from '../../components/StatusIcon'
import { Ago, Duration } from '../../components/Time'
import { eventLabel, runState, short, toDate } from '../../lib/format'
import table from '../settings_people.module.css'
import css from './ai.module.css'
import { aiRequestURL, aiTaskURL, taskPlatform, type AIRequest, type AITask } from './schema'
import { lookup } from '../../lib/lookup'

// AI 请求详情页和 AI 任务详情页共用的部分：请求的信息、任务列表、重跑和取消。

/** unix 把 ISO 时间换成 Unix 秒（为空或 Go 的零值时间时为 0），给 Duration 用。 */
export function unix(t: string | null): number {
  const d = toDate(t)
  return d ? Math.floor(d.getTime() / 1000) : 0
}

/** requestState 返回 AI 请求状态的中文说明（与服务端的筛选选项一致：active 是"进行中"）。 */
export function requestState(state: string): string {
  return state === 'active' ? '进行中' : runState(state)
}

/** requestTitle 返回请求的标题，例如"AI 审计请求 #88"。 */
export function requestTitle(r: Pick<AIRequest, 'TypeLabel' | 'ID'>): string {
  return `${r.TypeLabel}请求 #${r.ID}`
}

const devModes: Record<string, string> = { select: '选题', fix: '返修' }

/** RequestFacts 是请求的信息：仓库、事件、ref、commit、PR 或工作项、发起人、时间、重跑来源。 */
export function RequestFacts({ r, prURL }: { r: AIRequest; prURL?: string }) {
  return (
    <dl className={css.kv}>
      <dt>状态</dt>
      <dd>
        <span className={css.state}>
          <StatusIcon state={r.State} size={14} />
          {requestState(r.State)}
        </span>
      </dd>
      <dt>仓库</dt>
      <dd>{r.Repo}</dd>
      <dt>事件</dt>
      <dd>
        {eventLabel(r.Event)}
        {r.Trigger && <span className={css.muted}>（{r.Trigger}）</span>}
        {r.Sender && <span className={css.muted}>，{r.Sender}</span>}
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
          <dt>工作项</dt>
          <dd>
            <a href={r.WorkItemURL}>#{r.WorkItemID}</a>
            {r.DevMode && (
              <span className={css.muted}>
                （{lookup(devModes, r.DevMode) ?? r.DevMode}
                {r.DevMode === 'fix' && `，第 ${r.DevRound} 轮`}）
              </span>
            )}
          </dd>
        </>
      )}
      {r.RerunOf !== 0 && (
        <>
          <dt>重跑自</dt>
          <dd>
            <a href={aiRequestURL(r.RerunOf)}>#{r.RerunOf}</a>
          </dd>
        </>
      )}
      <dt>时间</dt>
      <dd>
        创建于 <Ago t={r.Created} />
        {r.Finished && (
          <>
            ，结束于 <Ago t={r.Finished} />
          </>
        )}
        {r.Purged && <span className={css.muted}>；日志已按保留策略清理</span>}
      </dd>
    </dl>
  )
}

/** TaskState 是任务状态的图标和文字；沿用结果、请求取消时补充说明。 */
export function TaskState({ t }: { t: AITask }) {
  return (
    <span className={css.state}>
      <StatusIcon state={t.State} size={14} label={t.StateTitle} />
      {t.StateTitle}
      {t.ReusedFrom !== 0 && (
        <Label variant="secondary" title="沿用上一次的结果，没有执行">
          沿用 #{t.ReusedFrom}
        </Label>
      )}
      {t.CancelRequested && t.State === 'running' && <Label variant="attention">正在取消</Label>}
    </span>
  )
}

/** TasksTable 是请求中的 AI 任务（每个平台一个）。 */
export function TasksTable({ tasks }: { tasks: AITask[] }) {
  return (
    <table className={table.table} aria-label="AI 任务">
      <thead>
        <tr>
          <th scope="col">任务</th>
          <th scope="col">状态</th>
          <th scope="col">平台</th>
          <th scope="col">Agent</th>
          <th scope="col">AI 执行节点</th>
          <th scope="col">耗时</th>
        </tr>
      </thead>
      <tbody>
        {tasks.map((t) => (
          <tr key={t.ID}>
            <td data-label="任务">
              <a href={aiTaskURL(t.ID)}>{t.DisplayName}</a>
              {t.Reason && <div className={t.State === 'failed' || t.State === 'timed_out' ? table.err : css.muted}>{t.Reason}</div>}
            </td>
            <td data-label="状态">
              <TaskState t={t} />
            </td>
            <td data-label="平台" className={table.mono}>
              {taskPlatform(t)}
            </td>
            <td data-label="Agent">{t.Agent || '—'}</td>
            <td data-label="AI 执行节点">{t.Runner || '—'}</td>
            <td data-label="耗时" className={table.nowrap}>
              <Duration start={unix(t.Started)} end={unix(t.Finished)} />
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

/** CancelRequest 是取消请求的表单（请求结束前）。 */
export function CancelRequest({ r }: { r: AIRequest }) {
  return (
    <ConfirmForm
      action={`${aiRequestURL(r.ID)}/cancel`}
      button="取消请求…"
      buttonProps={{ size: 'small' }}
      title={`取消${requestTitle(r)}？`}
      confirmLabel="取消请求"
      cancelLabel="返回"
    >
      没有开始的任务立即结束；执行中的任务由 AI 执行节点确认后结束。
    </ConfirmForm>
  )
}

/** RerunRequest 是重跑已结束的审计请求：只重跑没有成功的任务（有时），或全部重跑。重跑会新建一个请求。 */
export function RerunRequest({ r, hasFailed }: { r: AIRequest; hasFailed: boolean }) {
  const action = `${aiRequestURL(r.ID)}/rerun`
  return (
    <>
      {hasFailed && (
        <ConfirmForm
          action={action}
          fields={{ mode: 'failed' }}
          button="重跑失败的任务…"
          buttonProps={{ size: 'small', variant: 'primary' }}
          danger={false}
          title={`重跑${requestTitle(r)}中没有成功的任务？`}
          confirmLabel="重跑"
        >
          新建一个 AI 审计请求：没有成功的任务重新执行，成功的任务沿用这次的结果。
        </ConfirmForm>
      )}
      <ConfirmForm
        action={action}
        fields={{ mode: 'all' }}
        button="全部重跑…"
        buttonProps={{ size: 'small' }}
        danger={false}
        title={`全部重跑${requestTitle(r)}？`}
        confirmLabel="全部重跑"
      >
        新建一个 AI 审计请求，全部任务重新执行（同一个 commit）。
      </ConfirmForm>
    </>
  )
}
