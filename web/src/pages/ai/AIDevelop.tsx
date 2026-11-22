import { Link } from '@primer/react'
import { Box, BoxBody, BoxHeader } from '../../components/Box'
import { ReportBox } from './report'
import css from '../job.module.css'
import type { AIDevelop } from './schema'

// AI 开发任务保存的报告（PLAN #26.15）。没有领取的选题（没有选中、dry_run、领取失败）不属于任何工作项，选题的计划和
// 未选的原因只能在这里查看；属于工作项的链接到工作项的详情页。

export function AIDevelopSection({ dev }: { dev: AIDevelop | null | undefined }) {
  return (
    <>
      <Box as="section" aria-labelledby="ai-dev-h">
        <BoxHeader title="AI 开发" id="ai-dev-h" />
        <BoxBody>
          {dev?.WorkItemURL ? (
            <p className={css.emptyText}>
              这次执行属于 <Link href={dev.WorkItemURL}>工作项</Link>：完整的记录（Issue、评论、分支）在工作项的详情页。
            </p>
          ) : (
            <p className={css.emptyText}>
              这次执行没有领取工作项（没有选中、dry_run 或者领取失败）：保存的选题报告只在这里查看。
            </p>
          )}
          {!dev?.Reports.length && <p className={css.emptyText}>还没有报告：Agent 写出有效的报告并提交后显示在这里。</p>}
        </BoxBody>
      </Box>
      {dev?.Reports.map((r, i) => (
        <ReportBox key={`${r.TaskID}-${r.Kind}`} r={r} n={i} />
      ))}
    </>
  )
}
