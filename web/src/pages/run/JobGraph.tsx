import { Fragment } from 'react'
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

/** jobDetail 返回 job 节点的完整说明（悬停提示）：状态、当前阶段、耗时、Runner、平台、尝试次数、缓存、等待的上游和原因。 */
export function jobDetail(j: JobView, now: number): string {
  const parts = [jobState(j.State)]
  if (j.State === 'running' && j.Progress) parts.push(progressSummary(j.Progress))
  if (j.Started) parts.push(`耗时 ${dur(j.Started, j.Finished, now)}`)
  if (j.Runner) parts.push(`Runner ${j.Runner}`)
  if (j.Platform) parts.push(j.Platform)
  if (j.AttemptCount > 1) parts.push(`第 ${j.AttemptCount} 次尝试`)
  if (j.ReusedFrom !== null) parts.push(`沿用 #${j.ReusedFrom} 的结果`)
  else if (j.CachedFrom !== null) parts.push(`命中缓存 #${j.CachedFrom}`)
  if (j.Needs.length > 0) parts.push(`依赖 ${j.Needs.join('、')}`)
  if (j.State === 'waiting' && j.WaitingOn.length > 0) parts.push(`等待 ${j.WaitingOn.join('、')} 完成`)
  if (j.Reason) parts.push(j.Reason)
  if (j.State === 'pending' && j.WaitReason) parts.push(`等待：${j.WaitReason}`)
  return parts.join(' · ')
}

/**
 * JobMeta 是节点的第二行：执行中的 job 显示耗时和当前阶段；否则失败原因优先，其次是等待的上游、沿用 / 缓存，
 * 最后是耗时（或状态）和 Runner。
 */
function JobMeta({ job: j, now }: { job: JobView; now: number }) {
  if (j.State === 'running' && j.Progress) {
    return <span className={`${css.meta ?? ''} num`}>{[dur(j.Started, j.Finished, now), progressSummary(j.Progress)].filter(Boolean).join(' · ')}</span>
  }
  if (j.Reason) return <span className={`${css.meta ?? ''} ${css.reason ?? ''}`}>{j.Reason}</span>
  if (j.State === 'pending' && j.WaitReason) return <span className={css.meta}>等待：{j.WaitReason}</span>
  if (j.State === 'waiting' && j.WaitingOn.length > 0) return <span className={css.meta}>等待 {j.WaitingOn.join('、')}</span>
  if (j.ReusedFrom !== null) {
    return (
      <span className={css.meta}>
        沿用 <a href={`/jobs/${j.ReusedFrom}`}>#{j.ReusedFrom}</a> 的结果
      </span>
    )
  }
  if (j.CachedFrom !== null) {
    return (
      <span className={css.meta}>
        命中缓存 <a href={`/jobs/${j.CachedFrom}`}>#{j.CachedFrom}</a>
      </span>
    )
  }
  const parts = [j.Started ? dur(j.Started, j.Finished, now) : jobState(j.State)]
  if (j.Runner) parts.push(j.Runner)
  if (j.AttemptCount > 1) parts.push(`第 ${j.AttemptCount} 次尝试`)
  return <span className={`${css.meta ?? ''} num`}>{parts.join(' · ')}</span>
}

function JobNode({ job: j, into, out, now }: { job: JobView; into: boolean; out: boolean; now: number }) {
  const cls = [css.node, into && css.in, out && css.out].filter(Boolean).join(' ')
  return (
    <li className={cls} title={jobDetail(j, now)}>
      <span className={css.icon}>
        <StatusIcon state={j.State} />
      </span>
      <span className={css.text}>
        <a className={css.name} href={`/jobs/${j.ID}`}>
          {j.DisplayName}
          <span className="visually-hidden">，{jobState(j.State)}</span>
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
  return (
    <div className={css.scroll} tabIndex={0} role="group" aria-label="job 依赖图（可横向滚动）">
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
              <ol className={css.stage} aria-label={`阶段 ${s.N}`}>
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
