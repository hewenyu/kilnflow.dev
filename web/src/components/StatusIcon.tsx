import {
  AlertFillIcon,
  CheckCircleFillIcon,
  ClockIcon,
  DotFillIcon,
  SkipIcon,
  StopIcon,
  XCircleFillIcon,
} from '@primer/octicons-react'
import { stateTone, type Tone } from '../lib/format'
import css from './StatusIcon.module.css'

/** RunningIcon 是执行中的图标：转动的圆环。 */
function RunningIcon({ size, label }: { size: number; label?: string }) {
  return (
    <svg
      className={`${css.icon} ${css.run}`}
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      role={label ? 'img' : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
    >
      <circle cx="8" cy="8" r="6.25" stroke="currentColor" strokeOpacity="0.3" strokeWidth="1.5" />
      <path className={css.arc} d="M8 1.75a6.25 6.25 0 0 1 6.25 6.25" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
    </svg>
  )
}

/**
 * StatusIcon 是 job、run、attempt 等状态的图标（与 GitHub Actions 的状态图标一致）。
 * label 为空时图标只是装饰（旁边有文字），否则读屏软件读出 label。
 */
export function StatusIcon({ state, tone, size = 16, label }: { state: string; tone?: Tone; size?: number; label?: string }) {
  const t = tone ?? stateTone(state)
  const a11y = label ? { 'aria-label': label } : { 'aria-hidden': true as const }
  const cls = (c: string | undefined) => `${css.icon ?? ""} ${c ?? ""}`
  switch (state) {
    case 'cancelled':
      return <StopIcon size={size} className={cls(css.muted)} {...a11y} />
    case 'skipped':
      return <SkipIcon size={size} className={cls(css.muted)} {...a11y} />
    case 'waiting':
      return <ClockIcon size={size} className={cls(css.wait)} {...a11y} />
  }
  switch (t) {
    case 'ok':
      return <CheckCircleFillIcon size={size} className={cls(css.ok)} {...a11y} />
    case 'bad':
      return <XCircleFillIcon size={size} className={cls(css.bad)} {...a11y} />
    case 'run':
      return <RunningIcon size={size} label={label} />
    case 'wait':
      return <DotFillIcon size={size} className={cls(css.wait)} {...a11y} />
    case 'muted':
      return <AlertFillIcon size={size} className={cls(css.muted)} {...a11y} />
    default: {
      const _exhaustive: never = t
      return _exhaustive
    }
  }
}
