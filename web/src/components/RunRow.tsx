import { CalendarIcon, StopwatchIcon } from '@primer/octicons-react'
import { Label } from '@primer/react'
import { useLocale, useT } from '../i18n/locale'
import type { Locale } from '../i18n/messages'
import { eventLabel, jobStatesSummary, refKind, refName, rerunLabel, runState, short } from '../lib/format'
import { runTitle, runURL, type RunView } from '../lib/schemas'
import { Avatar } from './Avatar'
import { RefLabel } from './RefLabel'
import { StatusIcon } from './StatusIcon'
import { Ago, Duration } from './Time'
import css from './RunRow.module.css'

/** runSummary 返回 run 状态的完整说明（图标的读屏文字和悬停提示），例如"运行中 · 3 个 job：成功 1，执行中 2"。 */
export function runSummary(r: Pick<RunView, 'State' | 'States'>, locale?: Locale): string {
  const s = runState(r.State, locale)
  return r.States.length > 0 ? `${s} · ${jobStatesSummary(r.States, locale)}` : s
}

/**
 * RunRow 是运行列表中的一行：showRepo 为 false 时不显示仓库（例如已按仓库筛选）。
 * 用在运行列表和总览中，外层放在 Box 中（role="list"）。
 */
export function RunRow({ run, showRepo = true }: { run: RunView; showRepo?: boolean }) {
  const { locale } = useLocale()
  const t = useT()
  const summary = runSummary(run, locale)
  // run.GitHub 为空时（没有配置 GitHub 网页地址）不链接到 GitHub
  const gh = run.GitHub ? `${run.GitHub}/${run.Repo}` : ''
  return (
    <div className={css.row} role="listitem">
      <span className={css.icon} title={summary}>
        <StatusIcon state={run.State} label={summary} />
      </span>
      <div className={css.main}>
        <a className={css.title} href={runURL(run)}>
          {runTitle(run)} <span className="num">#{run.ID}</span>
        </a>
        <span className={css.labels}>
          {run.CurrentRerun > 0 && <Label variant="secondary">{rerunLabel(run.CurrentRerun, locale)}</Label>}
          {run.ConfigError !== null && <Label variant="danger">{t('run.configError')}</Label>}
        </span>
        <div className={css.meta}>
          {showRepo && (
            <span>
              {gh ? (
                <a href={gh} rel="noreferrer">
                  {run.Repo}
                </a>
              ) : (
                run.Repo
              )}{' '}
              ·
            </span>
          )}
          <span>{eventLabel(run.Event, locale)}</span>
          {run.PRNumber !== null && <span className="num">#{run.PRNumber}</span>}
          {run.SHA && (
            <>
              <span>·</span>
              <code title={`commit ${run.SHA}`}>
                {gh ? (
                  <a href={`${gh}/commit/${run.SHA}`} rel="noreferrer">
                    {short(run.SHA)}
                  </a>
                ) : (
                  short(run.SHA)
                )}
              </code>
            </>
          )}
          {run.SenderLogin && (
            <>
              <span>·</span>
              <span className={css.who}>
                <Avatar login={run.SenderLogin} size={16} />
                {run.SenderLogin}
              </span>
            </>
          )}
        </div>
      </div>
      <div className={css.ref}>
        <RefLabel
          gitRef={run.Ref}
          name={refKind(run.Ref) === null ? run.GitHubCtx.head_ref || undefined : undefined}
          href={gh && refKind(run.Ref) ? `${gh}/tree/${refName(run.Ref)}` : undefined}
        />
      </div>
      <div className={css.times}>
        <span>
          <CalendarIcon size={14} aria-hidden />
          <Ago t={run.CreatedAt} />
        </span>
        <span title={t('run.duration')}>
          <StopwatchIcon size={14} aria-hidden />
          <Duration start={run.Start} end={run.End} />
        </span>
      </div>
    </div>
  )
}
