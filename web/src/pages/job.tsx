import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Msg, useT } from '../i18n/locale'
import { isEmpty } from '../i18n/messages'
import { RunHeader } from '../components/RunHeader'
import { RunSidebar } from '../components/RunSidebar'
import { LogViewer } from './job/LogViewer'
import { jobPageSchema, type JobPage } from './job/schema'
import { AttemptsSection, JobActions, JobHeader, JobNotices, JobRunNote, OutputsSection, ReleaseSection, runLink, SpecSection } from './job/sections'
import css from './job.module.css'

// job 详情页（GitHub Actions 的 job 页）：顶部是与 run 详情页相同的标题区，左侧是同一次运行中的 job，右侧是 job 的信息、实时日志，
// 下方是产物或发布结果、每次尝试和 job spec。job 未结束（或 run 还在进行）时每 5 秒刷新页面数据，日志自己轮询。

function logNote(p: JobPage, t: ReturnType<typeof useT>): string | undefined {
  if (p.Publish) return t('job.logNotePublish')
  if (p.Viewing) return t('graph.attemptN', { n: p.Viewing.N })
  return undefined
}

function Job({ data: p }: PageProps<JobPage>) {
  useLive(!p.Terminal || p.RunActive)
  const t = useT()
  const j = p.Job
  return (
    <div className={css.page}>
      <RunHeader
        run={p.Run}
        repo={p.Repo}
        back="/runs"
        state={j.Rerun === p.Run.CurrentRerun ? p.Run.State : null}
        runURL={runLink(p)}
        note={<JobRunNote p={p} />}
        actions={<JobActions p={p} />}
      />
      <div className={css.layout}>
        <aside className={css.side}>
          <RunSidebar summaryURL={runLink(p)} jobs={p.Jobs} current={j.ID} />
        </aside>
        <div className={css.content}>
          <JobHeader p={p} />
          <JobNotices p={p} />
          {!isEmpty(p.AttemptErr) ? (
            <p className={css.logBlocked}>{t('job.logBlocked')}</p>
          ) : p.NoLog ? (
            <section className={css.noLog} aria-labelledby="log-h">
              <h2 id="log-h" className={css.noLogTitle}>
                {t('logView.title')}
              </h2>
              <p>
                <Msg text={p.LogEmpty} />
              </p>
            </section>
          ) : (
            <LogViewer
              base={`/jobs/${j.ID}`}
              attempt={p.Attempt}
              explicit={p.Explicit}
              attemptKey={p.AttemptKey}
              state={j.State}
              emptyText={p.LogEmpty}
              note={logNote(p, t)}
            />
          )}
          {p.Publish ? (
            <ReleaseSection release={p.Release} />
          ) : (
            <OutputsSection p={p} />
          )}
          <AttemptsSection p={p} />
          <SpecSection spec={p.Spec} />
        </div>
      </div>
    </div>
  )
}

export default definePage(jobPageSchema, Job)
