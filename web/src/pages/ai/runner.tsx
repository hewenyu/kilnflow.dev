import { PlayIcon } from '@primer/octicons-react'
import { Label, LinkButton } from '@primer/react'
import { ConfirmForm, Form, SubmitButton } from '../../components/Form'
import { runnerIdle } from '../../components/RunnerSchema'
import { StatusIcon } from '../../components/StatusIcon'
import { Ago } from '../../components/Time'
import { clock, shortVersion } from '../../lib/format'
import css from './ai.module.css'
import { OrcaStatus } from './orca'
import { aiRequestURL, aiTaskURL, type AIAgent, type AIRunner } from './schema'
import { lookup } from '../../lib/lookup'

// AI 执行节点列表和详情页共用的部分（PLAN #33）：本机状态（Orca、Docker、Agent）、并发、正在执行的 AI 任务及其在 Orca 中的
// 关联、管理员操作。暂停、恢复、吊销、删除与 CI Runner 共用表单地址（POST /runners/{name}/{op}），完成后服务端回到
// AI 执行节点页面。

const readyStates: Record<string, string> = { ready: '就绪', unavailable: '不可用', unknown: '未上报' }
const agentStates: Record<string, string> = { available: '可用', unavailable: '不可用', backoff: '暂停使用', unknown: '未上报' }

/** shortAgentVersion 取 --version 输出中的版本号（例如 "2.1.32 (Claude Code)" → 2.1.32）。 */
export function shortAgentVersion(v: string): string {
  return /\d+\.\d+(\.\d+)?/.exec(v)?.[0] ?? v
}

/** roleTitle 返回角色的说明（标签的 title）。 */
export function roleTitle(role: string): string {
  return role === 'development' ? '从标了 kiln:ready 的 Issue 选题、开发并交付为 PR' : '只执行 PR 的 AI 审计'
}

function Ready({ state, reason }: { state: string; reason: string }) {
  const text = lookup(readyStates, state) ?? state
  return (
    <span className={state === 'ready' ? undefined : state === 'unavailable' ? css.warn : css.muted}>
      {text}
      {reason && `（${reason}）`}
    </span>
  )
}

function Agent({ a }: { a: AIAgent }) {
  // 恢复时间只属于"暂停使用"：其它状态下 Server 可能还留着上次的时间，不能显示成这个状态的预计恢复
  const until = a.State === 'backoff' ? clock(a.BackoffUntil) : ''
  return (
    <>
      <span className="mono">{a.Name || a.Agent}</span>{' '}
      {a.State === 'available' ? (
        <span title={a.Version}>{a.Version ? shortAgentVersion(a.Version) : '可用'}</span>
      ) : (
        <span className={a.State === 'unknown' ? css.muted : css.warn}>
          {lookup(agentStates, a.State) ?? a.State}
          {a.Reason && `（${a.Reason}）`}
          {until && (
            <>
              ，预计 {until} 恢复
            </>
          )}
        </span>
      )}
    </>
  )
}

/** AIRunnerFacts 是 AI 执行节点的平台、连接器版本、协议、并发、Orca、Docker、Agent 和最后心跳。 */
export function AIRunnerFacts({ runner: r }: { runner: AIRunner }) {
  const ver = shortVersion(r.Version)
  return (
    <dl className={css.kv}>
      <dt>平台</dt>
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
      <dt>协议</dt>
      <dd>
        {r.Protocol || '未上报'}
        {r.NeedsUpgrade && (
          <span className={css.warn}>
            （连接器的协议版本过旧，Server 不支持，不派任务：用 <code>npm install -g @yuebanlaosiji/kilnflow-orca@latest</code> 升级后{' '}
            <code>kiln-orca service restart</code>）
          </span>
        )}
      </dd>
      <dt>并发</dt>
      <dd>{r.Capacity > 0 ? `${r.Tasks.length} / ${r.Capacity}` : <span className={css.muted}>未上报</span>}</dd>
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
      <dt>工具链</dt>
      <dd>
        <Ready state={r.ToolchainState} reason={r.ToolchainReason} />
      </dd>
      <dt>Agent</dt>
      <dd>
        {r.Agents.length === 0
          ? '未上报'
          : r.Agents.map((a, i) => (
              <span key={a.Agent}>
                {i > 0 && '；'}
                <Agent a={a} />
              </span>
            ))}
      </dd>
      <dt>最后心跳</dt>
      <dd>
        <Ago t={r.LastSeen} fallback="从未" />
        {r.ReportedAt && (
          <span className={css.muted}>
            ，状态 <Ago t={r.ReportedAt} />上报
          </span>
        )}
      </dd>
    </dl>
  )
}

/** AIWork 是 AI 执行节点正在执行的 AI 任务（没有时说明它在做什么）：每个任务的进度和在 Orca 中的关联。 */
export function AIWork({ runner: r }: { runner: AIRunner }) {
  if (r.Tasks.length === 0) return <p className={css.p}>{runnerIdle(r)}</p>
  return (
    <>
      {r.Drain && <p className={css.p}>已暂停接单，等 {r.Tasks.length} 个 AI 任务执行完。</p>}
      <ul className={css.tasks} aria-label={`${r.Name} 正在执行的 AI 任务`}>
        {r.Tasks.map((t) => (
          <li key={t.TaskID}>
            <StatusIcon state="running" size={14} />
            <a href={aiTaskURL(t.TaskID)}>{t.Name}</a>
            <span className={css.muted}>
              {t.Repo}，<a href={aiRequestURL(t.RequestID)}>{t.Type === 'develop' ? 'AI 开发' : 'AI 审计'}请求 #{t.RequestID}</a>，<Ago t={t.Started} />
              开始
            </span>
            <OrcaStatus stage={t.Stage} text={t.Text} orca={t.Orca} className={css.taskMeta} />
          </li>
        ))}
      </ul>
    </>
  )
}

/** RoleLabel 是 AI 执行节点的角色标签。 */
export function RoleLabel({ runner: r }: { runner: AIRunner }) {
  return (
    <Label variant="accent" title={roleTitle(r.Role)}>
      {r.RoleLabel}
    </Label>
  )
}

const opAction = (name: string, op: string) => `/runners/${encodeURIComponent(name)}/${op}`

/** AIRunnerOps 是一个 AI 执行节点的管理员操作：暂停或恢复接单、吊销；已吊销的可以重新注册或删除。 */
export function AIRunnerOps({ runner: r, longPoll, leaseTTL }: { runner: AIRunner; longPoll: string; leaseTTL: string }) {
  const n = r.Tasks.length
  if (r.Revoked) {
    const kind = r.Role === 'development' ? 'ai-dev' : 'ai'
    return (
      <>
        <LinkButton size="small" href={`/runners/new?name=${encodeURIComponent(r.Name)}&kind=${kind}`}>
          重新注册…
        </LinkButton>
        {n > 0 ? (
          <span className={css.muted}>{n} 个 AI 任务结束后可以删除</span>
        ) : (
          <ConfirmForm
            action={opAction(r.Name, 'delete')}
            button="删除…"
            buttonProps={{ size: 'small' }}
            title={`删除 ${r.Name}？`}
            confirmLabel={`确认删除 ${r.Name}`}
          >
            <ul className={css.list}>
              <li>{r.Name} 从 AI 执行节点列表中移除。删除不能撤销。</li>
              <li>它执行过的 attempt 按保留策略保留，AI 任务页面仍显示名称 {r.Name}。</li>
              <li>名称 {r.Name} 可以重新注册，注册后是一个新的 AI 执行节点。</li>
            </ul>
          </ConfirmForm>
        )}
      </>
    )
  }
  return (
    <>
      {r.Drain ? (
        <Form action={opAction(r.Name, 'undrain')} aria-label={`恢复 ${r.Name} 接单`}>
          <SubmitButton size="small" leadingVisual={PlayIcon} title="恢复后有空闲时会领取新的 AI 任务">
            恢复接单
          </SubmitButton>
        </Form>
      ) : (
        <ConfirmForm
          action={opAction(r.Name, 'drain')}
          danger={false}
          button="暂停接单…"
          buttonProps={{ size: 'small' }}
          title={`暂停 ${r.Name} 接收新的 AI 任务？`}
          confirmLabel="确认暂停接单"
        >
          <ul className={css.list}>
            <li>{r.Name} 不再领取新的 AI 任务，在它正在进行的一轮领取结束后生效（最长约 {longPoll}）。</li>
            <li>{n > 0 ? `正在执行的 ${n} 个 AI 任务继续执行到结束，不会被中断。` : '它现在没有正在执行的 AI 任务。'}</li>
          </ul>
        </ConfirmForm>
      )}
      <ConfirmForm
        action={opAction(r.Name, 'revoke')}
        button="吊销…"
        buttonProps={{ size: 'small' }}
        title={`吊销 ${r.Name}？`}
        confirmLabel={`确认吊销 ${r.Name}`}
      >
        <ul className={css.list}>
          <li>立即生效：{r.Name} 之后的所有请求都会被拒绝，这个节点的 kiln-orca 会退出，重新注册前不会再领取任务。</li>
          {n > 0 ? (
            <li>
              正在执行的 {n} 个 AI 任务会被中断，约 {leaseTTL} 后（租约过期）结束；AI 任务领取后不自动重试，需要时手动重跑。
              <b>只想让它停下来时，先"暂停接单"，等任务结束后再吊销。</b>
            </li>
          ) : (
            <li>它现在没有正在执行的 AI 任务。</li>
          )}
          <li>
            恢复方式：生成新的注册令牌，在这台机器上重新执行 <code>kiln-orca enroll</code>（这个节点用了 <code>--home</code>{' '}
            时带上同一个数据目录），再启动服务。吊销不能撤销。
          </li>
        </ul>
      </ConfirmForm>
    </>
  )
}
