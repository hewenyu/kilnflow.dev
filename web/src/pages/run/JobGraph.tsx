import { Fragment } from 'react'
import { Trans, useLocale, useT } from '../../i18n/locale'
import { defaultLocale, msg, translate, type Locale, type MsgKey, type Params } from '../../i18n/messages'
import { dur, jobState } from '../../lib/format'
import { progressSummary } from '../../lib/progress'
import type { JobView } from '../../lib/schemas'
import { StatusIcon } from '../../components/StatusIcon'
import { secondMs, useNow } from '../../components/Time'
import css from './JobGraph.module.css'

/** Stage 是按 needs 分好的一个阶段（服务端 jobStage）。 */
export interface Stage {
  N: number
  Jobs: JobView[]
}

// 节点高度和间距（与 JobGraph.module.css 一致），用来计算连接区竖线的长度
const nodeHeight = 56
const nodeGap = 16

type T = (id: MsgKey, params?: Params) => string

/** jobDetail 返回 job 节点的完整说明（悬停提示）：状态、当前阶段、耗时、Runner、平台、尝试次数、缓存、等待的上游和原因。 */
export function jobDetail(j: JobView, now: number, locale: Locale = defaultLocale): string {
  const t: T = (id, params) => translate(locale, msg(id, params))
  const names = (list: readonly string[]) => list.join(t('list.enum'))
  const parts = [jobState(j.State, locale)]
  if (j.State === 'running' && j.Progress) parts.push(progressSummary(j.Progress, locale))
  if (j.Started) parts.push(t('graph.duration', { d: dur(j.Started, j.Finished, now, locale) }))
  if (j.Runner) parts.push(t('graph.runner', { name: j.Runner }))
  if (j.Platform) parts.push(j.Platform)
  if (j.AttemptCount > 1) parts.push(t('graph.attemptN', { n: j.AttemptCount }))
  if (j.ReusedFrom !== null) parts.push(t('graph.reused', { id: j.ReusedFrom }))
  else if (j.CachedFrom !== null) parts.push(t('graph.cached', { id: j.CachedFrom }))
  if (j.Needs.length > 0) parts.push(t('graph.needs', { names: names(j.Needs) }))
  if (j.State === 'waiting' && j.WaitingOn.length > 0) parts.push(t('graph.waitingFor', { names: names(j.WaitingOn) }))
  if (j.Reason) parts.push(j.Reason)
  if (j.State === 'pending' && j.WaitReason) parts.push(t('graph.waitReason', { reason: j.WaitReason }))
  return parts.join(' · ')
}

/**
 * JobMeta 是节点的第二行：执行中的 job 显示耗时和当前阶段；否则失败原因优先，其次是等待的上游、沿用 / 缓存，
 * 最后是耗时（或状态）和 Runner。
 */
function JobMeta({ job: j, now }: { job: JobView; now: number }) {
  const t = useT()
  const { locale } = useLocale()
  if (j.State === 'running' && j.Progress) {
    return (
      <span className={`${css.meta ?? ''} num`}>
        {[dur(j.Started, j.Finished, now, locale), progressSummary(j.Progress, locale)].filter(Boolean).join(' · ')}
      </span>
    )
  }
  if (j.Reason) return <span className={`${css.meta ?? ''} ${css.reason ?? ''}`}>{j.Reason}</span>
  if (j.State === 'pending' && j.WaitReason) return <span className={css.meta}>{t('graph.waitReason', { reason: j.WaitReason })}</span>
  if (j.State === 'waiting' && j.WaitingOn.length > 0) {
    return <span className={css.meta}>{t('graph.waiting', { names: j.WaitingOn.join(t('list.enum')) })}</span>
  }
  const from = j.ReusedFrom ?? j.CachedFrom
  if (from !== null) {
    return (
      <span className={css.meta}>
        <Trans
          id={j.ReusedFrom !== null ? 'graph.reusedLink' : 'graph.cachedLink'}
          params={{ id: from }}
          tags={{ link: (c) => <a href={`/jobs/${from}`}>{c}</a> }}
        />
      </span>
    )
  }
  const parts = [j.Started ? dur(j.Started, j.Finished, now, locale) : jobState(j.State, locale)]
  if (j.Runner) parts.push(j.Runner)
  if (j.AttemptCount > 1) parts.push(t('graph.attemptN', { n: j.AttemptCount }))
  return <span className={`${css.meta ?? ''} num`}>{parts.join(' · ')}</span>
}

function JobNode({ job: j, into, out, now }: { job: JobView; into: boolean; out: boolean; now: number }) {
  const t = useT()
  const { locale } = useLocale()
  const cls = [css.node, into && css.in, out && css.out].filter(Boolean).join(' ')
  return (
    <li className={cls} title={jobDetail(j, now, locale)}>
      <span className={css.icon}>
        <StatusIcon state={j.State} />
      </span>
      <span className={css.text}>
        <a className={css.name} href={`/jobs/${j.ID}`}>
          {j.DisplayName}
          <span className="visually-hidden">{t('graph.stateSuffix', { state: jobState(j.State, locale) })}</span>
        </a>
        <JobMeta job={j} now={now} />
      </span>
    </li>
  )
}

/** JobGraph 把 job 按阶段从左到右排列，相邻阶段之间画连接线。 */
export function JobGraph({ stages }: { stages: readonly Stage[] }) {
  const running = stages.some((s) => s.Jobs.some((j) => j.Started !== 0 && j.Finished === 0))
  const now = useNow(secondMs, running)
  const t = useT()
  return (
    <div className={css.scroll} tabIndex={0} role="group" aria-label={t('graph.label')}>
      <div className={css.graph}>
        {stages.map((s, i) => {
          const prev = stages[i - 1]
          const span = prev ? (Math.max(prev.Jobs.length, s.Jobs.length) - 1) * (nodeHeight + nodeGap) : 0
          return (
            <Fragment key={s.N}>
              {prev && (
                <div className={css.joint} aria-hidden>
                  <span className={css.line} style={{ height: span }} />
                </div>
              )}
              <ol className={css.stage} aria-label={t('graph.stage', { n: s.N })}>
                {s.Jobs.map((j) => (
                  <JobNode key={j.ID} job={j} into={i > 0} out={i < stages.length - 1} now={now} />
                ))}
              </ol>
            </Fragment>
          )
        })}
      </div>
    </div>
  )
}
