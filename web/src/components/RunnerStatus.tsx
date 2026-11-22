import { AlertFillIcon, CheckCircleFillIcon, CircleSlashIcon, XCircleFillIcon } from '@primer/octicons-react'
import { Label } from '@primer/react'
import type { RunnerConn } from './RunnerSchema'
import { StatusIcon } from './StatusIcon'
import css from './RunnerStatus.module.css'

// Runner 的状态图标和标签：CI Runner 和 AI 执行节点共用（running 是正在执行的 job 或 AI 任务数）。

/** runnerStatusText 返回 Runner 状态的完整说明（图标的读屏文字）。 */
export function runnerStatusText(r: RunnerConn, running: number): string {
  if (r.Revoked) return '已吊销'
  const accept = r.Drain ? '已暂停接单' : '接单中'
  if (!r.Online) return `离线，${accept}`
  if (running > 0) return `在线，正在执行 ${running} 个任务`
  return `在线，${accept}`
}

/** RunnerIcon 是 Runner 的状态图标：已吊销、离线、正在执行、已暂停接单、空闲。 */
export function RunnerIcon({ runner, running, size = 16 }: { runner: RunnerConn; running: number; size?: number }) {
  const label = runnerStatusText(runner, running)
  if (runner.Revoked) return <XCircleFillIcon size={size} className={css.bad} aria-label={label} />
  if (!runner.Online) return <AlertFillIcon size={size} className={css.warn} aria-label={label} />
  if (running > 0) return <StatusIcon state="running" size={size} label={label} />
  if (runner.Drain) return <CircleSlashIcon size={size} className={css.muted} aria-label={label} />
  return <CheckCircleFillIcon size={size} className={css.ok} aria-label={label} />
}

/** RunnerLabels 是连接和接单状态的标签。 */
export function RunnerLabels({ runner }: { runner: RunnerConn }) {
  const conn = runner.Revoked ? 'danger' : runner.Online ? 'success' : 'attention'
  return (
    <>
      <Label variant={conn}>{runner.ConnLabel}</Label>
      {!runner.Revoked && (runner.Drain ? <Label variant="attention">已暂停接单</Label> : <Label variant="success">接单中</Label>)}
    </>
  )
}
