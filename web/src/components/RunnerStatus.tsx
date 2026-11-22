import { AlertFillIcon, CheckCircleFillIcon, CircleSlashIcon, XCircleFillIcon } from '@primer/octicons-react'
import { Label } from '@primer/react'
import { useT } from '../i18n/locale'
import { connLabel, type RunnerConn, type TFn } from './RunnerSchema'
import { StatusIcon } from './StatusIcon'
import css from './RunnerStatus.module.css'

// Runner 的状态图标和标签：CI Runner 和 AI 执行节点共用（running 是正在执行的 job 或 AI 任务数）。

/** runnerStatusText 返回 Runner 状态的完整说明（图标的读屏文字）。 */
export function runnerStatusText(r: RunnerConn, running: number, t: TFn): string {
  if (r.Revoked) return t('runnerConn.revoked')
  if (!r.Online) return t(r.Drain ? 'runnerStatus.offlinePaused' : 'runnerStatus.offlineAccepting')
  if (running > 0) return t('runnerStatus.running', { count: running })
  return t(r.Drain ? 'runnerStatus.onlinePaused' : 'runnerStatus.onlineAccepting')
}

/** RunnerIcon 是 Runner 的状态图标：已吊销、离线、正在执行、已暂停接单、空闲。 */
export function RunnerIcon({ runner, running, size = 16 }: { runner: RunnerConn; running: number; size?: number }) {
  const t = useT()
  const label = runnerStatusText(runner, running, t)
  if (runner.Revoked) return <XCircleFillIcon size={size} className={css.bad} aria-label={label} />
  if (!runner.Online) return <AlertFillIcon size={size} className={css.warn} aria-label={label} />
  if (running > 0) return <StatusIcon state="running" size={size} label={label} />
  if (runner.Drain) return <CircleSlashIcon size={size} className={css.muted} aria-label={label} />
  return <CheckCircleFillIcon size={size} className={css.ok} aria-label={label} />
}

/** RunnerLabels 是连接和接单状态的标签。 */
export function RunnerLabels({ runner }: { runner: RunnerConn }) {
  const t = useT()
  const conn = runner.Revoked ? 'danger' : runner.Online ? 'success' : 'attention'
  return (
    <>
      <Label variant={conn}>{connLabel(runner, t)}</Label>
      {!runner.Revoked &&
        (runner.Drain ? <Label variant="attention">{t('runnerAccept.paused')}</Label> : <Label variant="success">{t('runnerAccept.accepting')}</Label>)}
    </>
  )
}
