import type { TFn } from '../../components/RunnerSchema'
import { useT } from '../../i18n/locale'
import { isMsgKey } from '../../i18n/messages'
import css from './ai.module.css'
import type { OrcaBinding } from './schema'

// AI 执行节点页面和 AI 任务页面共用的 Orca 部分（PLAN #33.4、#33.6）：心跳上报的进度，以及一次执行在 Orca 中的关联
// （dispatch、worktree、启动请求），用于在 Orca 中找到对应的 Agent 和 worktree。

/** stageText 返回进度阶段的显示名称（未知的阶段原样显示）。阶段由节点上报，可以是任意字符串，只按词典中有的键查。 */
export function stageText(stage: string, t: TFn): string {
  const key = `orca.stage.${stage}`
  return isMsgKey(key) ? t(key) : stage
}

/** Progress 是心跳上报的进度，例如"执行中：运行测试"（上报的说明原样显示）。 */
function Progress({ stage, text }: { stage: string; text: string }) {
  const t = useT()
  return (
    <span>
      {stage && stageText(stage, t)}
      {stage && text && t('orca.separator')}
      {text}
    </span>
  )
}

interface Ref {
  key: string
  /** label 是关联的名称：dispatch、worktree 原样显示，启动请求按语言显示。 */
  label: string | { id: 'orca.startRequest' }
  value: string
  title?: string
}

/** orcaRefs 返回执行在 Orca 中的关联：dispatch、worktree（有名称时用名称，否则用路径），给了 startRequest 时还有启动请求。 */
function orcaRefs(o: OrcaBinding, startRequest?: string): Ref[] {
  const refs: Ref[] = []
  const wt = o.worktree_name || o.worktree_path
  if (o.dispatch_id) refs.push({ key: 'dispatch', label: 'dispatch', value: o.dispatch_id })
  if (wt) refs.push({ key: 'worktree', label: 'worktree', value: wt, title: o.worktree_path })
  if (startRequest) refs.push({ key: 'start', label: { id: 'orca.startRequest' }, value: startRequest })
  return refs
}

function OrcaRefs({ items }: { items: Ref[] }) {
  const t = useT()
  return (
    <span className={css.orca}>
      {t('orca.refs')}
      {items.map((it, i) => (
        <span key={it.key}>
          {i > 0 && t('list.separator')}
          {typeof it.label === 'string' ? it.label : t(it.label.id)}{' '}
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
