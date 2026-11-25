import { Link } from '@primer/react'
import { Box, BoxBody, BoxHeader } from '../../components/Box'
import { Trans, useT } from '../../i18n/locale'
import { ReportBox } from './report'
import css from '../job.module.css'
import type { AIDevelop } from './schema'

// AI 开发任务保存的报告（PLAN #26.15）。没有领取的选题（没有选中、dry_run、领取失败）不属于任何工作项，选题的计划和
// 未选的原因只能在这里查看；属于工作项的链接到工作项的详情页。

export function AIDevelopSection({ dev }: { dev: AIDevelop | null | undefined }) {
  const t = useT()
  const url = dev?.WorkItemURL
  return (
    <>
      <Box as="section" aria-labelledby="ai-dev-h">
        <BoxHeader title={t('aiDev.pageTitle')} id="ai-dev-h" />
        <BoxBody>
          {url ? (
            <p className={css.emptyText}>
              <Trans id="aiDevelop.inWorkItem" tags={{ link: (c) => <Link href={url}>{c}</Link> }} />
            </p>
          ) : (
            <p className={css.emptyText}>{t('aiDevelop.noWorkItem')}</p>
          )}
          {!dev?.Reports.length && <p className={css.emptyText}>{t('aiDevelop.noReports')}</p>}
        </BoxBody>
      </Box>
      {dev?.Reports.map((r, i) => (
        <ReportBox key={`${r.TaskID}-${r.Kind}`} r={r} n={i} />
      ))}
    </>
  )
}
