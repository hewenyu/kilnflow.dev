import {
  CpuIcon,
  DownloadIcon,
  FileIcon,
  FileSymlinkFileIcon,
  GitCommitIcon,
  KeyIcon,
  PackageIcon,
  ServerIcon,
  StopwatchIcon,
  SyncIcon,
} from '@primer/octicons-react'
import { Button, CounterLabel, Dialog, Label, Link, LinkButton, ProgressBar } from '@primer/react'
import { useState, type ReactNode } from 'react'
import { useRefreshLock } from '../../app/live'
import { Box, BoxBody, BoxFooter, BoxHeader, BoxRow } from '../../components/Box'
import { Callout } from '../../components/Callout'
import { Form, SubmitButton } from '../../components/Form'
import { KeepWorkspace } from '../run/RerunDialog'
import { StatusIcon } from '../../components/StatusIcon'
import { Duration, secondMs, useNow } from '../../components/Time'
import { Msg, Trans, useLocale, useT } from '../../i18n/locale'
import { defaultLocale, isEmpty, msg, translate, type Locale, type MsgKey, type Params } from '../../i18n/messages'
import { isWebURL } from '../../lib/api'
import {
  clock,
  dur,
  jobState,
  labelVariant,
  outcome,
  outcomeTone,
  releaseState,
  rerunLabel,
  short,
  shortDigest,
  size,
  stateTone,
  targetKind,
  targetState,
  targetTone,
} from '../../lib/format'
import { awaitingConfirm, phaseLabel, uploadDetail, uploading, uploadPercent } from '../../lib/progress'
import type { JobView } from '../../lib/schemas'
import {
  githubDetailOf,
  imageDetailOf,
  legacyProgressOf,
  npmDetailOf,
  uploadDetailOf,
  type Attempt,
  type JobPage,
  type Output,
  type Queue,
  type Release,
  type ReleaseTarget,
  type Rerun,
} from './schema'
import css from '../job.module.css'

/** runLink 返回 job 所在的那次运行的 run 详情页地址。 */
export function runLink(p: JobPage): string {
  return `/runs/${p.Run.ID}?rerun=${p.Job.Rerun}`
}

/** JobHeader 是主栏顶部：job 名称和状态、耗时等关键信息（run 的标题和操作在 RunHeader 中）。 */
export function JobHeader({ p }: { p: JobPage }) {
  const t = useT()
  const { locale } = useLocale()
  const j = p.Job
  const r = p.Run
  return (
    <header className={css.header}>
      <h2 className={css.title}>
        <StatusIcon state={j.State} size={20} label={jobState(j.State, locale)} />
        <span className={css.titleText}>{j.DisplayName}</span>
        <span className={css.titleID}>#{j.ID}</span>
      </h2>
      <ul className={css.meta} aria-label={t('job.info')}>
        <li className={css.metaState}>{jobState(j.State, locale)}</li>
        {j.Started > 0 && (
          <li title={t('run.duration')}>
            <StopwatchIcon size={16} />
            <Duration start={j.Started} end={j.Finished} />
          </li>
        )}
        {r.SHA && (
          <li title={`commit ${r.SHA}`}>
            <GitCommitIcon size={16} />
            <code>{short(r.SHA)}</code>
          </li>
        )}
        {(j.Platform || j.Executor) && (
          <li title={t('job.platformExecutor')}>
            <CpuIcon size={16} />
            <code>{[j.Platform, j.Executor].filter(Boolean).join(' · ')}</code>
          </li>
        )}
        {j.Runner && (
          <li title="Runner">
            <ServerIcon size={16} />
            {j.Runner}
          </li>
        )}
        {j.ActionKey !== null && (
          <li title={`action key ${j.ActionKey}`}>
            <KeyIcon size={16} />
            <code>{short(j.ActionKey)}</code>
          </li>
        )}
      </ul>
    </header>
  )
}

/** JobRunNote 是标题区中 run 标题后面的补充：查看的是哪一次运行，是否是最新一次。 */
export function JobRunNote({ p }: { p: JobPage }) {
  const t = useT()
  const { locale } = useLocale()
  const j = p.Job
  if (j.Rerun === 0 && j.Rerun === p.Run.CurrentRerun) return null
  return (
    <>
      {j.Rerun > 0 && <Label variant="secondary">{rerunLabel(j.Rerun, locale)}</Label>}
      {j.Rerun !== p.Run.CurrentRerun && <Label variant="attention">{t('job.notLatest')}</Label>}
    </>
  )
}

// 长时间没有进展、没有上报时提示（只是提示，不判定失败）
const stallSeconds = 60
const silentSeconds = 30

/** JobProgress 说明执行中的 job 正在做什么：阶段、阶段耗时、最近进展；上传时显示文件和字节数。 */
export function JobProgress({ job: j }: { job: JobView }) {
  const now = useNow(secondMs, j.State === 'running')
  const t = useT()
  const { locale } = useLocale()
  if (j.State !== 'running') return null
  const p = j.Progress
  if (!p) {
    return <Callout title={t('job.progressTitle')}>{t('job.progressUnavailable')}</Callout>
  }
  const sec = Math.floor(now / 1000)
  const pct = uploadPercent(p)
  const phase = phaseLabel(p.Phase, locale)
  const since = (at: number) => dur(at, 0, now, locale)
  return (
    <Callout title={t('job.progressTitle')}>
      <div className={css.progress}>
        <p>
          <b>{phase}</b>
          {p.Detail && p.Detail !== phase && <>{t('list.colon')}{p.Detail}</>}
        </p>
        <p className={`${css.progressMeta ?? ''} num`}>{t('job.progressTimes', { phase: since(p.PhaseStartedAt), moved: since(p.MovedAt) })}</p>
        {uploading(p) && (
          <>
            {pct !== null && <ProgressBar progress={pct} barSize="small" aria-label={t('progress.uploadingPercent', { pct })} className={css.progressBar} />}
            <p className="num">{uploadDetail(p, locale)}</p>
            {awaitingConfirm(p) && <p>{t('job.progressAwaiting')}</p>}
          </>
        )}
        {p.Retries > 0 && (
          <p className={css.progressWarn}>{t('job.progressRetry', { n: p.Retries, error: p.LastError || t('job.progressUnknownError') })}</p>
        )}
        {sec - p.MovedAt >= stallSeconds && !awaitingConfirm(p) && (
          <p className={css.progressWarn}>{t('job.progressStalled', { d: since(p.MovedAt) })}</p>
        )}
        {sec - p.ReportedAt >= silentSeconds && <p className={css.progressWarn}>{t('job.progressSilent', { d: since(p.ReportedAt) })}</p>}
      </div>
    </Callout>
  )
}

/** QueueNotice 说明排队中、调度器没有记下跳过原因的 job 排在哪些 job 之后（调度解释）。 */
function QueueNotice({ job, queue }: { job: JobView; queue: Queue }) {
  const t = useT()
  return (
    <Callout title={t('job.queueTitle')}>
      {queue.Ahead > 0 ? (
        <Trans
          id="job.queueAhead"
          params={{ count: queue.Ahead, run: queue.FirstRunID }}
          tags={{ num: (c) => <span className="num">{c}</span>, link: (c) => <Link href={`/runs/${queue.FirstRunID}`}>{c}</Link> }}
        />
      ) : (
        t('job.queueNone')
      )}{' '}
      {t('job.queueRule', { platform: job.Platform, executor: job.Executor })}
    </Callout>
  )
}

/** JobNotices 是标题下方的说明：失败原因、结果来源、正在查看历史尝试、地址中的 attempt 无效。job 的原因和等待原因是调度器记下的原文。 */
export function JobNotices({ p }: { p: JobPage }) {
  const t = useT()
  const { locale } = useLocale()
  const j = p.Job
  const jobLink = (id: number) => ({ link: (c: string) => <Link href={`/jobs/${id}`}>{c}</Link> })
  return (
    <div className={css.notices}>
      {!isEmpty(p.AttemptErr) && (
        <Callout tone="critical" title={t('job.attemptErrTitle')}>
          <Msg text={p.AttemptErr} />{' '}
          <Trans id={p.Attempts.length > 0 ? 'job.attemptErrPick' : 'job.attemptErrCurrent'} tags={jobLink(j.ID)} />
        </Callout>
      )}
      {j.State === 'waiting' && j.WaitingOn.length > 0 && (
        <Callout title={t('job.waitingTitle')}>
          {t(p.Publish ? 'job.waitingPublish' : 'job.waitingStart', { names: j.WaitingOn.join(t('list.enum')) })}
        </Callout>
      )}
      {j.State === 'pending' && j.WaitReason && (
        <Callout tone="warning" title={t('job.waitRunnerTitle')}>
          {j.WaitReason}
        </Callout>
      )}
      {j.State === 'pending' && !j.WaitReason && p.Queue && <QueueNotice job={j} queue={p.Queue} />}
      {j.Reason !== null && (
        <Callout tone={stateTone(j.State) === 'bad' ? 'critical' : 'info'} title={stateTone(j.State) === 'bad' ? t('job.reasonFailed') : t('job.reasonNote')} hideTitle>
          {j.Reason}
        </Callout>
      )}
      <JobProgress job={j} />
      {j.ReusedFrom !== null ? (
        <Callout title={t('job.reusedTitle')} hideTitle>
          <Trans id="job.reusedText" params={{ job: j.ReusedFrom }} tags={jobLink(j.ReusedFrom)} />
        </Callout>
      ) : (
        j.CachedFrom !== null && (
          <Callout title={t('job.cachedTitle')} hideTitle>
            <Trans id="job.cachedText" params={{ job: j.CachedFrom }} tags={jobLink(j.CachedFrom)} />
          </Callout>
        )
      )}
      {p.Viewing && (
        <Callout tone="warning" title={t('job.viewingTitle', { n: p.Viewing.N })}>
          <Trans
            id="job.viewingText"
            params={{ n: p.Viewing.N, id: p.Viewing.ID, outcome: outcome(p.Viewing.Outcome, locale), state: jobState(j.State, locale) }}
            tags={jobLink(j.ID)}
          />
        </Callout>
      )}
    </div>
  )
}

/** JobActions 是右上角的操作：run 进行中时取消整个 run，结束后重跑这个 job。 */
export function JobActions({ p }: { p: JobPage }) {
  const t = useT()
  const { locale } = useLocale()
  if (p.RunActive) return <CancelRun runID={p.Run.ID} />
  if (!p.Rerun) return null
  if (!p.Rerun.Latest) {
    return (
      <p className={css.actionNote}>
        {t('job.rerunNotLatest', { rerun: rerunLabel(p.Job.Rerun, locale) })}
        {p.Rerun.LatestID ? <Link href={`/jobs/${p.Rerun.LatestID}`}>{t('job.openLatest')}</Link> : t('job.noLatest')}
      </p>
    )
  }
  return <RerunJob p={p} rerun={p.Rerun} />
}

function CancelRun({ runID }: { runID: number }) {
  const [open, setOpen] = useState(false)
  useRefreshLock(open)
  const t = useT()
  return (
    <>
      <Button variant="danger" onClick={() => setOpen(true)}>
        {t('job.cancelRun')}
      </Button>
      {open && (
        <Dialog title={t('job.cancelTitle', { id: runID })} onClose={() => setOpen(false)} width="medium">
          <Form action={`/runs/${runID}/cancel`} aria-label={t('job.cancelRun')}>
            <p className={css.dialogText}>{t('job.cancelText', { id: runID })}</p>
            <div className={css.dialogActions}>
              <Button onClick={() => setOpen(false)}>{t('job.cancelKeep')}</Button>
              <SubmitButton variant="danger">{t('job.cancelRun')}</SubmitButton>
            </div>
          </Form>
        </Dialog>
      )}
    </>
  )
}

/** rerunEffects 返回重跑这个 job 的影响说明（每条一句）；locale 不传时为 zh-CN。 */
export function rerunEffects(r: Rerun, currentRerun: number, locale: Locale = defaultLocale): string[] {
  const t = (id: MsgKey, params?: Params) => translate(locale, msg(id, params))
  const out = [
    r.Force ? t('job.effectForce') : t('job.effectReplan'),
    r.Downstream.length > 0
      ? t('job.effectDownstream', { count: r.Downstream.length, names: r.Downstream.join(t('list.enum')) })
      : t('job.effectNoDownstream'),
    t('job.effectKept', { count: r.Kept }),
  ]
  if (r.Replan > 0) out.push(t('job.effectPurged', { count: r.Replan }))
  out.push(t('job.effectNew', { rerun: rerunLabel(currentRerun, locale) }))
  return out
}

function RerunJob({ p, rerun }: { p: JobPage; rerun: Rerun }) {
  const [open, setOpen] = useState(false)
  useRefreshLock(open)
  const t = useT()
  const { locale } = useLocale()
  return (
    <>
      <Button leadingVisual={SyncIcon} onClick={() => setOpen(true)}>
        {t('job.rerun')}
      </Button>
      {open && (
        <Dialog title={t('job.rerunTitle')} subtitle={p.Job.DisplayName} onClose={() => setOpen(false)} width="large">
          <Form action={`/jobs/${p.Job.ID}/rerun`} aria-label={t('job.rerun')}>
            <ul className={css.effects}>
              {rerunEffects(rerun, p.Run.CurrentRerun, locale).map((x) => (
                <li key={x}>{x}</li>
              ))}
              {rerun.Publish && (
                <li>
                  <Trans id="job.effectPublish" tags={{ strong: (c) => <strong>{c}</strong> }} />
                </li>
              )}
            </ul>
            <KeepWorkspace />
            <div className={css.dialogActions}>
              <Button onClick={() => setOpen(false)}>{t('form.cancel')}</Button>
              <SubmitButton variant="primary" leadingVisual={SyncIcon}>
                {t('job.rerunTitle')}
              </SubmitButton>
            </div>
          </Form>
        </Dialog>
      )}
    </>
  )
}

/** outputsIntro 返回产物来源的说明。 */
function OutputsIntro({ p }: { p: JobPage }) {
  const t = useT()
  const j = p.Job
  const jobLink = (id: number) => ({ link: (c: string) => <Link href={`/jobs/${id}`}>{c}</Link> })
  return (
    <p className={css.intro}>
      {j.ReusedFrom !== null ? (
        <Trans id="job.outputsReused" params={{ job: j.ReusedFrom }} tags={jobLink(j.ReusedFrom)} />
      ) : j.CachedFrom !== null ? (
        <Trans id="job.outputsCached" params={{ job: j.CachedFrom }} tags={jobLink(j.CachedFrom)} />
      ) : p.Producer ? (
        t('job.outputsProducer', { n: p.Producer.N, id: p.Producer.ID })
      ) : (
        t('job.outputsThis')
      )}
      {t('job.outputsDownload')}
    </p>
  )
}

function downloadURL(jobID: number, output: string, path: string): string {
  return `/jobs/${jobID}/download?${new URLSearchParams({ output, path }).toString()}`
}

function OutputBlock({ jobID, o }: { jobID: number; o: Output }) {
  const t = useT()
  const hidden = o.FileCount - o.Files.length
  return (
    <div className={css.output}>
      <div className={css.outputHead}>
        <PackageIcon size={16} />
        <h3 className={css.outputName}>{o.Name}</h3>
        <span className={css.muted}>{o.Missing ? t('job.manifestGoneShort') : t('job.outputFiles', { count: o.FileCount, size: size(o.Size) })}</span>
      </div>
      {o.Missing ? (
        <p className={css.outputNote}>{t('job.manifestGone')}</p>
      ) : (
        <ul className={css.files}>
          {o.Files.map((f) => (
            <li key={f.path} className={css.file}>
              {f.symlink ? <FileSymlinkFileIcon size={16} className={css.fileIcon} /> : <FileIcon size={16} className={css.fileIcon} />}
              <span className={css.filePath}>
                <code>{f.path}</code>
                {f.exec && <Label variant="secondary">{t('job.fileExec')}</Label>}
                {f.symlink && <span className={css.muted}> → {f.symlink}</span>}
              </span>
              <span className={css.fileSize}>{f.symlink ? '—' : size(f.size)}</span>
              {f.digest ? (
                <code className={css.fileDigest} title={`sha256 ${f.digest}`}>
                  {shortDigest(f.digest)}
                </code>
              ) : (
                <span className={css.fileDigest}>—</span>
              )}
              <span className={css.fileAction}>
                {f.symlink ? (
                  <span className={css.muted}>{t('job.fileSymlink')}</span>
                ) : f.Missing ? (
                  <span className={css.bad}>{t('job.fileGone')}</span>
                ) : (
                  <LinkButton
                    href={downloadURL(jobID, o.Name, f.path)}
                    size="small"
                    leadingVisual={DownloadIcon}
                    aria-label={t('job.downloadFile', { output: o.Name, path: f.path })}
                  >
                    {t('job.download')}
                  </LinkButton>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}
      {!o.Missing && hidden > 0 && (
        <p className={css.outputNote}>{t('job.moreFiles', { count: hidden, shown: o.Files.length })}</p>
      )}
    </div>
  )
}

/** OutputsSection 是产物：每个输出的文件和下载。 */
export function OutputsSection({ p }: { p: JobPage }) {
  const t = useT()
  const { locale } = useLocale()
  const o = p.Outputs
  const has = o !== null && o.Manifest !== '' && (o.Missing || o.Outputs.length > 0)
  return (
    <Box as="section" aria-labelledby="outputs-h">
      <BoxHeader title={t('job.outputs')} id="outputs-h" />
      {has ? (
        <>
          <BoxBody className={css.introBody}>
            <OutputsIntro p={p} />
          </BoxBody>
          {o.Missing ? (
            <BoxBody>
              {p.Purged !== null ? (
                <Callout title={t('rerun.purgedTitle')} hideTitle>
                  {t('job.outputsPurged', { time: clock(p.Purged, locale) })}
                </Callout>
              ) : (
                <Callout tone="warning" title={t('job.manifestMissingTitle')} hideTitle>
                  {t('job.manifestMissing')}
                </Callout>
              )}
            </BoxBody>
          ) : (
            o.Outputs.map((out) => <OutputBlock key={out.Name} jobID={p.Job.ID} o={out} />)
          )}
        </>
      ) : (
        <BoxBody>
          <p className={css.emptyText}>
            {p.Declared.length > 0
              ? t(p.Terminal ? 'job.declaredFailed' : 'job.declaredPending', { names: p.Declared.join(t('list.enum')) })
              : t('job.noOutputs')}
          </p>
        </BoxBody>
      )}
    </Box>
  )
}

/**
 * ExternalLink 渲染发布结果中的外部地址。只接受 http(s)：发布目标和旧版进度来自服务端保存的
 * 平台返回内容，出现 javascript: 等地址时只显示文字，不生成可点击的链接。
 */
function ExternalLink({ href, children }: { href: string; children: ReactNode }) {
  if (!isWebURL(href)) return <span className={css.wrap}>{children}</span>
  return (
    <Link href={href} rel="noreferrer">
      {children}
    </Link>
  )
}

function TargetResult({ target: t }: { target: ReleaseTarget }) {
  const tr = useT()
  switch (t.Kind) {
    case 'github': {
      const d = githubDetailOf(t.Detail)
      return (
        <>
          {d.html_url && <ExternalLink href={d.html_url}>{d.html_url}</ExternalLink>}
          {d.draft && <span className={css.muted}>{tr('job.releaseDraft')}</span>}
        </>
      )
    }
    case 'image': {
      const d = imageDetailOf(t.Detail)
      return (
        <>
          {d.digest && <code className={css.wrap}>{d.digest}</code>}
          {d.tags && d.tags.length > 0 && <span> {tr('job.releaseTags', { tags: d.tags.join(tr('list.enum')) })}</span>}
        </>
      )
    }
    case 'upload': {
      const d = uploadDetailOf(t.Detail)
      return (
        <>
          {d.previous && <span className={css.muted}>{tr('job.uploadPrevious')}</span>}
          {t.State === 'committed' && d.latest && <span className={css.muted}>{tr('job.uploadLatest')}</span>}
          {t.State === 'committed' &&
            d.assets?.map((a, idx) => (
              <span key={`${idx}:${a.name}`} className={css.asset}>
                {a.url ? <ExternalLink href={a.url}>{a.name}</ExternalLink> : <span className={css.wrap}>{a.name}</span>}
              </span>
            ))}
        </>
      )
    }
    case 'npm': {
      const d = npmDetailOf(t.Detail)
      const pkg = d.package && d.version ? `${d.package}@${d.version}` : ''
      return (
        <>
          {t.State === 'committed' && pkg && (
            <span>
              {t.URL ? (
                <ExternalLink href={t.URL}>
                  <code>{pkg}</code>
                </ExternalLink>
              ) : (
                <code>{pkg}</code>
              )}
              {d.dist_tag && (
                <>
                  {' '}
                  · dist-tag <code>{d.dist_tag}</code>
                </>
              )}
              {d.access && ` · access ${d.access}`}
            </span>
          )}
          {d.previous && <span className={`${css.muted} ${css.block}`}>{tr('job.npmPrevious')}</span>}
        </>
      )
    }
    default:
      return null
  }
}

/** ReleaseSection 是发布 job 的发布进度和各目标的结果。 */
export function ReleaseSection({ release }: { release: Release | null }) {
  const t = useT()
  const { locale } = useLocale()
  if (!release) {
    return (
      <Box as="section" aria-labelledby="release-h">
        <BoxHeader title={t('job.logNotePublish')} id="release-h" />
        <BoxBody>
          <p className={css.emptyText}>{t('job.releaseNotStarted')}</p>
        </BoxBody>
      </Box>
    )
  }
  const legacy = release.Targets.length === 0 ? legacyProgressOf(release.Progress) : null
  return (
    <Box as="section" aria-labelledby="release-h">
      <BoxHeader title={t('job.logNotePublish')} id="release-h">
        <Label variant={labelVariant(stateTone(release.State))} title={release.State}>
          {releaseState(release.State, locale)}
        </Label>
      </BoxHeader>
      <BoxBody>
        <dl className={css.kv}>
          <dt>{t('job.releaseTarget')}</dt>
          <dd className={css.wrap}>{release.Target}</dd>
          <dt>tag</dt>
          <dd>
            <code>{release.Tag}</code>
          </dd>
          <dt>{t('job.releaseStage')}</dt>
          <dd>
            <code>{release.Stage}</code>
          </dd>
          {legacy?.html_url && (
            <>
              <dt>Release</dt>
              <dd className={css.wrap}>
                <ExternalLink href={legacy.html_url}>{legacy.html_url}</ExternalLink>
              </dd>
            </>
          )}
          {legacy?.images?.map((i) => (
            <div key={i.name + i.digest} className={css.kvRow}>
              <dt>{t('job.releaseImage')}</dt>
              <dd className={css.wrap}>
                <code>
                  {i.name}@{i.digest}
                </code>
                {i.tags && i.tags.length > 0 && ` ${t('job.releaseTags', { tags: i.tags.join(t('list.enum')) })}`}
              </dd>
            </div>
          ))}
          {release.Error && (
            <>
              <dt>{t('job.releaseError')}</dt>
              <dd className={`${css.wrap} ${css.bad}`}>{release.Error}</dd>
            </>
          )}
        </dl>
      </BoxBody>
      {release.Targets.map((x) => (
        <BoxRow key={`${x.Kind}:${x.Name}`} className={css.target}>
          <StatusIcon state={x.State} tone={targetTone(x.State)} label={targetState(x.State, locale)} />
          <div className={css.targetMain}>
            <div className={css.targetName}>
              <strong>{x.Name}</strong>
              <span className={css.muted}>{targetKind(x.Kind, locale)}</span>
              <Label variant={labelVariant(targetTone(x.State))} title={x.State}>
                {targetState(x.State, locale)}
              </Label>
            </div>
            {x.Kind === 'upload' && x.Name !== x.Target && <code className={`${css.muted} ${css.wrap}`}>{x.Target}</code>}
            {x.Error && <p className={css.targetError}>{x.Error}</p>}
            <div className={css.targetResult}>
              <TargetResult target={x} />
            </div>
          </div>
        </BoxRow>
      ))}
    </Box>
  )
}

function AttemptRow({ a, jobID, viewing }: { a: Attempt; jobID: number; viewing: boolean }) {
  const t = useT()
  const { locale } = useLocale()
  const tone = outcomeTone(a.Outcome)
  const meta = [
    a.ExitCode !== null ? t('job.attemptExit', { code: a.ExitCode }) : '',
    a.ErrorClass,
  ].filter(Boolean)
  return (
    <BoxRow className={`${css.attempt} ${viewing ? css.attemptViewing ?? '' : ''}`} aria-current={viewing ? 'true' : undefined}>
      <StatusIcon state={a.Outcome || 'running'} tone={tone} label={outcome(a.Outcome, locale)} />
      <div className={css.attemptMain}>
        <div className={css.attemptTitle}>
          <strong>{t('job.attemptN', { n: a.N })}</strong>
          <Label variant={labelVariant(tone)} title={a.Outcome}>
            {outcome(a.Outcome, locale)}
          </Label>
          {a.Current && <Label variant="accent">{t('job.attemptCurrent')}</Label>}
        </div>
        <p className={css.attemptMeta}>
          {a.Runner}
          {t('list.separator')}
          <Duration start={a.Started} end={a.Finished} />
          {meta.map((m) => t('list.separator') + m).join('')} <code>#{a.ID} · epoch {a.Epoch}</code>
        </p>
        {a.Message && <p className={css.attemptMsg}>{a.Message}</p>}
      </div>
      <div className={css.attemptAction}>
        {viewing ? (
          <span className={css.muted}>{t('job.attemptViewing')}</span>
        ) : (
          <Link href={`/jobs/${jobID}?attempt=${a.ID}`}>{t('job.attemptViewLog')}</Link>
        )}
      </div>
    </BoxRow>
  )
}

/** AttemptsSection 是每次尝试的结果。不会再有尝试的 job（命中缓存、跳过、发布）没有尝试时不显示。 */
export function AttemptsSection({ p }: { p: JobPage }) {
  const t = useT()
  if (p.Attempts.length === 0 && (p.Terminal || p.Publish)) return null
  return (
    <Box as="section" aria-labelledby="attempts-h">
      <BoxHeader
        title={
          <>
            {t('job.attempts')} {p.Attempts.length > 0 && <CounterLabel>{p.Attempts.length}</CounterLabel>}
          </>
        }
        id="attempts-h"
      />
      {p.Attempts.length === 0 ? (
        <BoxBody>
          <p className={css.emptyText}>{t('job.attemptsEmpty')}</p>
        </BoxBody>
      ) : (
        <>
          {p.Attempts.map((a) => (
            <AttemptRow key={a.ID} a={a} jobID={p.Job.ID} viewing={a.ID === p.Attempt} />
          ))}
          <BoxFooter className={css.footNote}>{t('job.attemptsNote')}</BoxFooter>
        </>
      )}
    </Box>
  )
}

/** SpecSection 是 job spec 或 AI 任务的执行输入（JSON，原文不翻译），默认收起；title 不传时为"Job spec"。 */
export function SpecSection({ spec, title }: { spec: string; title?: string }) {
  const t = useT()
  return (
    <details className={css.spec}>
      <summary>{title ?? t('job.spec')}</summary>
      <pre>{spec}</pre>
    </details>
  )
}
