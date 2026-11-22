import { CpuIcon, PlusIcon } from '@primer/octicons-react'
import { LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { EmptyState } from '../components/EmptyState'
import { PageHeader } from '../components/PageHeader'
import { RunnerIcon, RunnerLabels } from '../components/RunnerStatus'
import { list } from '../lib/envelope'
import { AIRunnerFacts, AIRunnerOps, AIWork, RoleLabel } from './ai/runner'
import { aiRunnerCountsSchema, aiRunnerSchema, aiRunnerURL, type AIRunner } from './ai/schema'
import css from './ai/ai.module.css'
import runnersCss from './runners.module.css'

// AI 执行节点列表（PLAN #30.9、PLAN #33）：AI 执行节点（执行机器上的 Orca Server + kiln-orca 连接器）与 CI Runner 分开列出。
// 连接和接单状态与 CI Runner 相同；另外显示角色、协议、并发、Orca / Docker、本机 Agent 和正在执行的 AI 任务及其在 Orca 中的
// 关联。列表每 5 秒自动更新。

const schema = z.object({
  Runners: list(aiRunnerSchema),
  Counts: aiRunnerCountsSchema,
  Self: z.string(),
  /** 页面说明中用到的时长。 */
  OfflineAfter: z.string(),
  LeaseTTL: z.string(),
  LongPoll: z.string(),
})
type Data = z.infer<typeof schema>

function Row({ runner: r, data, admin }: { runner: AIRunner; data: Data; admin: boolean }) {
  return (
    <BoxRow as="li" id={`r-${r.ID}`} className={`${runnersCss.row} ${r.Revoked ? runnersCss.revoked : ''}`}>
      <span className={runnersCss.icon}>
        <RunnerIcon runner={r} running={r.Tasks.length} />
      </span>
      <div className={runnersCss.main}>
        <div className={runnersCss.head}>
          <h3 className={runnersCss.name}>
            <a href={aiRunnerURL(r.Name)}>{r.Name}</a>
          </h3>
          <RoleLabel runner={r} />
          <RunnerLabels runner={r} />
        </div>
        <AIRunnerFacts runner={r} />
        <AIWork runner={r} />
      </div>
      {admin && (
        <div className={runnersCss.ops}>
          <AIRunnerOps runner={r} longPoll={data.LongPoll} leaseTTL={data.LeaseTTL} />
        </div>
      )}
    </BoxRow>
  )
}

function AIRunners({ data, env }: PageProps<Data>) {
  useLive(true)
  const admin = env.Session?.Admin ?? false
  const c = data.Counts
  return (
    <div className={css.page}>
      <PageHeader
        title="AI 执行节点"
        description={
          <>
            执行 AI 审计和 AI 开发的节点：执行机器上的 Orca 通过连接器 kiln-orca 领取任务，一个节点可以同时执行多个任务。{data.OfflineAfter}
            没有联系 Server 时显示为离线。CI 的 Runner 见 <a href="/runners">Runner</a>。
          </>
        }
        actions={
          admin && (
            <LinkButton href="/runners/new" variant="primary" leadingVisual={PlusIcon}>
              注册新的 Runner
            </LinkButton>
          )
        }
      />
      {c.Total > 0 && c.Accepting === 0 && (
        <Callout tone="warning" title="没有在线并接单的 AI 执行节点">
          新的 AI 请求会一直排队，直到有 AI 执行节点上线{c.Paused > 0 ? '或恢复接单' : ''}。
        </Callout>
      )}
      <Box>
        <h2 className="visually-hidden">AI 执行节点列表</h2>
        {c.Total > 0 && (
          <BoxRow className={css.muted}>
            共 {c.Total} 个：{c.Online} 在线，{c.Accepting} 接单中，{c.Paused} 已暂停，{c.Offline} 离线，{c.Revoked} 已吊销；{c.Tasks} 个 AI 任务在执行。
          </BoxRow>
        )}
        {data.Runners.length > 0 ? (
          <ul className={runnersCss.rows} aria-label="AI 执行节点">
            {data.Runners.map((r) => (
              <Row key={r.ID} runner={r} data={data} admin={admin} />
            ))}
          </ul>
        ) : (
          <EmptyState icon={CpuIcon} title="还没有 AI 执行节点">
            注册时选择"AI 审计节点"或"AI 开发节点"。{admin ? '' : '注册 Runner 需要管理员。'}
          </EmptyState>
        )}
      </Box>
    </div>
  )
}

export default definePage(schema, AIRunners)
