import { PlayIcon } from '@primer/octicons-react'
import { Label, LinkButton } from '@primer/react'
import { ConfirmForm, Form, SubmitButton } from '../../components/Form'
import { runnerIdle, runnerKindLabel, type TFn } from '../../components/RunnerSchema'
import { StatusIcon } from '../../components/StatusIcon'
import { Ago } from '../../components/Time'
import { Trans, useLocale, useT, type Tags } from '../../i18n/locale'
import { isMsgKey } from '../../i18n/messages'
import { clock, shortVersion } from '../../lib/format'
import css from './ai.module.css'
import { OrcaStatus } from './orca'
import { aiRequestURL, aiTaskURL, type AIAgent, type AIRunner } from './schema'

// AI 执行节点列表和详情页共用的部分（PLAN #33）：本机状态（Orca、Docker、Agent）、并发、正在执行的 AI 任务及其在 Orca 中的
// 关联、管理员操作。暂停、恢复、吊销、删除与 CI Runner 共用表单地址（POST /runners/{name}/{op}），完成后服务端回到
// AI 执行节点页面。

/** stateText 返回 Orca、Docker、工具链或 Agent 的状态名称（不认识的状态原样显示）。 */
function stateText(group: 'ready' | 'agent', state: string, t: TFn): string {
  const key = `aiRunner.${group}.${state}`
  return isMsgKey(key) ? t(key) : state
}

/** shortAgentVersion 取 --version 输出中的版本号（例如 "2.1.32 (Claude Code)" → 2.1.32）。 */
export function shortAgentVersion(v: string): string {
  return /\d+\.\d+(\.\d+)?/.exec(v)?.[0] ?? v
}

/** roleTitle 返回角色的说明（标签的 title）。 */
export function roleTitle(role: string, t: TFn): string {
  return t(role === 'development' ? 'aiRunner.roleDevTitle' : 'aiRunner.roleReviewTitle')
}

function Ready({ state, reason }: { state: string; reason: string }) {
  const t = useT()
  return (
    <span className={state === 'ready' ? undefined : state === 'unavailable' ? css.warn : css.muted}>
      {stateText('ready', state, t)}
      {reason && t('aiRunner.reason', { reason })}
    </span>
  )
}

function Agent({ a }: { a: AIAgent }) {
  const t = useT()
  const { locale } = useLocale()
  // 恢复时间只属于"暂停使用"：其它状态下 Server 可能还留着上次的时间，不能显示成这个状态的预计恢复
  const until = a.State === 'backoff' ? clock(a.BackoffUntil, locale) : ''
  return (
    <>
      <span className="mono">{a.Name || a.Agent}</span>{' '}
      {a.State === 'available' ? (
        <span title={a.Version}>{a.Version ? shortAgentVersion(a.Version) : stateText('agent', 'available', t)}</span>
      ) : (
        <span className={a.State === 'unknown' ? css.muted : css.warn}>
          {stateText('agent', a.State, t)}
          {a.Reason && t('aiRunner.reason', { reason: a.Reason })}
          {until && t('aiRunner.backoffUntil', { time: until })}
        </span>
      )}
    </>
  )
}

const rich: Tags = { b: (c) => <b>{c}</b>, code: (c) => <code>{c}</code> }

/** AIRunnerFacts 是 AI 执行节点的平台、连接器版本、协议、并发、Orca、Docker、Agent 和最后心跳。 */
export function AIRunnerFacts({ runner: r }: { runner: AIRunner }) {
  const t = useT()
  const ver = shortVersion(r.Version)
  return (
    <dl className={css.kv}>
      <dt>{t('aiRunner.platform')}</dt>
      <dd className="mono">{r.Platform || '—'}</dd>
      <dt>kiln-orca</dt>
      <dd>
        {r.Version ? (
          <span className="mono" title={r.Version}>
            {ver}
          </span>
        ) : (
          '—'
        )}
      </dd>
      <dt>{t('aiRunner.protocol')}</dt>
      <dd>
        {r.Protocol || t('aiRunner.notReported')}
        {r.NeedsUpgrade && (
          <span className={css.warn}>
            <Trans id="aiRunner.needsUpgrade" tags={rich} />
          </span>
        )}
      </dd>
      <dt>{t('aiRunner.capacity')}</dt>
      <dd>{r.Capacity > 0 ? `${r.Tasks.length} / ${r.Capacity}` : <span className={css.muted}>{t('aiRunner.notReported')}</span>}</dd>
      <dt>Orca</dt>
      <dd>
        <Ready state={r.OrcaState} reason={r.OrcaReason} />
        {r.OrcaVersion && (
          <>
            {' '}
            <span className="mono" title={r.OrcaRuntimeID ? `runtime ${r.OrcaRuntimeID}` : undefined}>
              {shortVersion(r.OrcaVersion)}
            </span>
          </>
        )}
      </dd>
      <dt>Docker</dt>
      <dd>
        <Ready state={r.DockerState} reason={r.DockerReason} />
      </dd>
      <dt>{t('aiRunner.toolchain')}</dt>
      <dd>
        <Ready state={r.ToolchainState} reason={r.ToolchainReason} />
      </dd>
      <dt>Agent</dt>
      <dd>
        {r.Agents.length === 0
          ? t('aiRunner.notReported')
          : r.Agents.map((a, i) => (
              <span key={a.Agent}>
                {i > 0 && t('aiRunner.agentSeparator')}
                <Agent a={a} />
              </span>
            ))}
      </dd>
      <dt>{t('runners.factHeartbeat')}</dt>
      <dd>
        <Ago t={r.LastSeen} fallback={t('runners.never')} />
        {r.ReportedAt && (
          <span className={css.muted}>
            <Trans id="aiRunner.reportedAt" tags={{ ago: () => <Ago t={r.ReportedAt} /> }} />
          </span>
        )}
      </dd>
    </dl>
  )
}

/** AIWork 是 AI 执行节点正在执行的 AI 任务（没有时说明它在做什么）：每个任务的进度和在 Orca 中的关联。 */
export function AIWork({ runner: r }: { runner: AIRunner }) {
  const t = useT()
  if (r.Tasks.length === 0) return <p className={css.p}>{runnerIdle(r, t)}</p>
  return (
    <>
      {r.Drain && <p className={css.p}>{t('aiRunner.pausedWaiting', { count: r.Tasks.length })}</p>}
      <ul className={css.tasks} aria-label={t('aiRunner.tasksLabel', { name: r.Name })}>
        {r.Tasks.map((task) => (
          <li key={task.TaskID}>
            <StatusIcon state="running" size={14} />
            <a href={aiTaskURL(task.TaskID)}>{task.Name}</a>
            <span className={css.muted}>
              <Trans
                id={task.Type === 'develop' ? 'aiRunner.taskMetaDevelop' : 'aiRunner.taskMetaReview'}
                params={{ repo: task.Repo, id: task.RequestID }}
                tags={{ link: (c) => <a href={aiRequestURL(task.RequestID)}>{c}</a>, ago: () => <Ago t={task.Started} /> }}
              />
            </span>
            <OrcaStatus stage={task.Stage} text={task.Text} orca={task.Orca} className={css.taskMeta} />
          </li>
        ))}
      </ul>
    </>
  )
}

/** RoleLabel 是 AI 执行节点的角色标签。 */
export function RoleLabel({ runner: r }: { runner: AIRunner }) {
  const t = useT()
  return (
    <Label variant="accent" title={roleTitle(r.Role, t)}>
      {runnerKindLabel('ai', r.Role, t)}
    </Label>
  )
}

const opAction = (name: string, op: string) => `/runners/${encodeURIComponent(name)}/${op}`

/** AIRunnerOps 是一个 AI 执行节点的管理员操作：暂停或恢复接单、吊销；已吊销的可以重新注册或删除。 */
export function AIRunnerOps({ runner: r, longPoll, leaseTTL }: { runner: AIRunner; longPoll: number; leaseTTL: number }) {
  const t = useT()
  const n = r.Tasks.length
  const name = r.Name
  if (r.Revoked) {
    const kind = r.Role === 'development' ? 'ai-dev' : 'ai'
    return (
      <>
        <LinkButton size="small" href={`/runners/new?name=${encodeURIComponent(r.Name)}&kind=${kind}`}>
          {t('runners.reenroll')}
        </LinkButton>
        {n > 0 ? (
          <span className={css.muted}>{t('aiRunner.deleteAfterTasks', { count: n })}</span>
        ) : (
          <ConfirmForm
            action={opAction(r.Name, 'delete')}
            button={t('runners.delete')}
            buttonProps={{ size: 'small' }}
            title={t('runners.deleteTitle', { name })}
            confirmLabel={t('runners.deleteConfirm', { name })}
          >
            <ul className={css.list}>
              <li>{t('aiRunner.deleteNote1', { name })}</li>
              <li>{t('aiRunner.deleteNote2', { name })}</li>
              <li>{t('aiRunner.deleteNote3', { name })}</li>
            </ul>
          </ConfirmForm>
        )}
      </>
    )
  }
  return (
    <>
      {r.Drain ? (
        <Form action={opAction(r.Name, 'undrain')} aria-label={t('runners.undrainLabel', { name })}>
          <SubmitButton size="small" leadingVisual={PlayIcon} title={t('aiRunner.undrainTitle')}>
            {t('runners.undrain')}
          </SubmitButton>
        </Form>
      ) : (
        <ConfirmForm
          action={opAction(r.Name, 'drain')}
          danger={false}
          button={t('runners.drain')}
          buttonProps={{ size: 'small' }}
          title={t('aiRunner.drainTitle', { name })}
          confirmLabel={t('runners.drainConfirm')}
        >
          <ul className={css.list}>
            <li>{t('aiRunner.drainNote1', { name, poll: longPoll })}</li>
            <li>{n > 0 ? t('aiRunner.drainNoteBusy', { count: n }) : t('aiRunner.drainNoteIdle')}</li>
          </ul>
        </ConfirmForm>
      )}
      <ConfirmForm
        action={opAction(r.Name, 'revoke')}
        button={t('runners.revoke')}
        buttonProps={{ size: 'small' }}
        title={t('runners.revokeTitle', { name })}
        confirmLabel={t('runners.revokeConfirm', { name })}
      >
        <ul className={css.list}>
          <li>{t('aiRunner.revokeNote1', { name })}</li>
          {n > 0 ? (
            <li>
              <Trans id="aiRunner.revokeNoteBusy" params={{ count: n, lease: leaseTTL }} tags={rich} />
            </li>
          ) : (
            <li>{t('aiRunner.drainNoteIdle')}</li>
          )}
          <li>
            <Trans id="aiRunner.revokeNote3" tags={rich} />
          </li>
        </ul>
      </ConfirmForm>
    </>
  )
}
