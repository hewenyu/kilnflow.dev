import { lookup } from '../../lib/lookup'
import css from './ai.module.css'
import type { OrcaBinding } from './schema'

// AI 执行节点页面和 AI 任务页面共用的 Orca 部分（PLAN #33.4、#33.6）：心跳上报的进度，以及一次执行在 Orca 中的关联
// （dispatch、worktree、启动请求），用于在 Orca 中找到对应的 Agent 和 worktree。

const stages: Record<string, string> = {
  prepare: '准备',
  start: '启动 Agent',
  running: '执行中',
  finishing: '结束处理',
}

/** stageText 返回进度阶段的显示名称（未知的阶段原样显示）。阶段由节点上报，可以是任意字符串，用 lookup 查表。 */
export function stageText(stage: string): string {
  return lookup(stages, stage) ?? stage
}

/** Progress 是心跳上报的进度，例如"执行中：运行测试"。 */
function Progress({ stage, text }: { stage: string; text: string }) {
  return (
    <span>
      {stage && stageText(stage)}
      {stage && text && '：'}
      {text}
    </span>
  )
}

interface Ref {
  key: string
  label: string
  value: string
  title?: string
}

/** orcaRefs 返回执行在 Orca 中的关联：dispatch、worktree（有名称时用名称，否则用路径），给了 startRequest 时还有启动请求。 */
function orcaRefs(o: OrcaBinding, startRequest?: string): Ref[] {
  const refs: Ref[] = []
  const wt = o.worktree_name || o.worktree_path
  if (o.dispatch_id) refs.push({ key: 'dispatch', label: 'dispatch', value: o.dispatch_id })
  if (wt) refs.push({ key: 'worktree', label: 'worktree', value: wt, title: o.worktree_path })
  if (startRequest) refs.push({ key: 'start', label: '启动请求', value: startRequest })
  return refs
}

function OrcaRefs({ items }: { items: Ref[] }) {
  return (
    <span className={css.orca}>
      Orca：
      {items.map((it, i) => (
        <span key={it.key}>
          {i > 0 && '，'}
          {it.label}{' '}
          <span className="mono" title={it.title}>
            {it.value}
          </span>
        </span>
      ))}
    </span>
  )
}

/** OrcaStatus 是一次执行的进度和在 Orca 中的关联（一行，className 给出边距和字号）；都没有上报时不显示。 */
export function OrcaStatus({
  stage,
  text,
  orca,
  startRequest,
  className,
}: {
  stage: string
  text: string
  orca: OrcaBinding
  startRequest?: string
  className?: string
}) {
  const refs = orcaRefs(orca, startRequest)
  const progress = stage !== '' || text !== ''
  if (!progress && refs.length === 0) return null
  return (
    <div className={`${css.orcaLine} ${className ?? ''}`}>
      {progress && <Progress stage={stage} text={text} />}
      {refs.length > 0 && <OrcaRefs items={refs} />}
    </div>
  )
}
