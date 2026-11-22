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
import { Button, Checkbox, CounterLabel, Dialog, FormControl, Label, Link, LinkButton, ProgressBar } from '@primer/react'
import { useState, type ReactNode } from 'react'
import { useRefreshLock } from '../../app/live'
import { Box, BoxBody, BoxFooter, BoxHeader, BoxRow } from '../../components/Box'
import { Callout } from '../../components/Callout'
import { Form, SubmitButton } from '../../components/Form'
import { StatusIcon } from '../../components/StatusIcon'
import { Duration, secondMs, useNow } from '../../components/Time'
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
  const j = p.Job
  const r = p.Run
  return (
    <header className={css.header}>
      <h2 className={css.title}>
        <StatusIcon state={j.State} size={20} label={jobState(j.State)} />
        <span className={css.titleText}>{j.DisplayName}</span>
        <span className={css.titleID}>#{j.ID}</span>
      </h2>
      <ul className={css.meta} aria-label="job 的信息">
        <li className={css.metaState}>{jobState(j.State)}</li>
        {j.Started > 0 && (
          <li title="耗时">
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
          <li title="平台 · 执行器">
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
  const j = p.Job
  if (j.Rerun === 0 && j.Rerun === p.Run.CurrentRerun) return null
  return (
    <>
      {j.Rerun > 0 && <Label variant="secondary">{rerunLabel(j.Rerun)}</Label>}
      {j.Rerun !== p.Run.CurrentRerun && <Label variant="attention">不是最新一次运行</Label>}
    </>
  )
}

// 长时间没有进展、没有上报时提示（只是提示，不判定失败）
const stallSeconds = 60
const silentSeconds = 30

/** JobProgress 说明执行中的 job 正在做什么：阶段、阶段耗时、最近进展；上传时显示文件和字节数。 */
export function JobProgress({ job: j }: { job: JobView }) {
  const now = useNow(secondMs, j.State === 'running')
  if (j.State !== 'running') return null
  const p = j.Progress
  if (!p) {
    return (
      <Callout title="当前阶段">
        进度暂不可用：执行这个 job 的 Runner 没有上报执行阶段（可能是旧版 kiln-runner），请查看下方日志。
      </Callout>
    )
  }
  const sec = Math.floor(now / 1000)
  const pct = uploadPercent(p)
  return (
    <Callout title="当前阶段">
      <div className={css.progress}>
        <p>
          <b>{phaseLabel(p.Phase)}</b>
          {p.Detail && p.Detail !== phaseLabel(p.Phase) && <>：{p.Detail}</>}
        </p>
        <p className={`${css.progressMeta ?? ''} num`}>
          本阶段已用时 {dur(p.PhaseStartedAt, 0, now)} · 最近进展 {dur(p.MovedAt, 0, now)}前
        </p>
        {uploading(p) && (
          <>
            {pct !== null && <ProgressBar progress={pct} barSize="small" aria-label={`上传产物 ${pct}%`} className={css.progressBar} />}
            <p className="num">{uploadDetail(p)}</p>
            {awaitingConfirm(p) && <p>已全部发送，等待 Server 确认校验（确认后才算上传完成）。</p>}
          </>
        )}
        {p.Retries > 0 && (
          <p className={css.progressWarn}>
            第 {p.Retries} 次重试，上次失败：{p.LastError || '未知原因'}
          </p>
        )}
        {sec - p.MovedAt >= stallSeconds && !awaitingConfirm(p) && (
          <p className={css.progressWarn}>已 {dur(p.MovedAt, 0, now)}没有新的进展（仍在等待，不代表失败）。</p>
        )}
        {sec - p.ReportedAt >= silentSeconds && (
          <p className={css.progressWarn}>Runner 已 {dur(p.ReportedAt, 0, now)}没有上报，以上可能不是最新状态。</p>
        )}
      </div>
    </Callout>
  )
}

/** QueueNotice 说明排队中、调度器没有记下跳过原因的 job 排在哪些 job 之后（调度解释）。 */
function QueueNotice({ job, queue }: { job: JobView; queue: Queue }) {
  return (
    <Callout title="排队位置">
      {queue.Ahead > 0 ? (
        <>
          前面还有 <span className="num">{queue.Ahead}</span> 个待领取的 job，排在最前面的属于{' '}
          <Link href={`/runs/${queue.FirstRunID}`}>Run #{queue.FirstRunID}</Link>。
        </>
      ) : (
        '前面没有待领取的 job，等待 Runner 领取。'
      )}{' '}
      按调度顺序统计：优先级高的先，同优先级先提交的 Run 先（重跑按重跑的时间）；只计平台 {job.Platform}、executor {job.Executor}{' '}
      与它相同的待领取 job，不论它们能否在同一台 Runner 上执行。
    </Callout>
  )
}

/** JobNotices 是标题下方的说明：失败原因、结果来源、正在查看历史尝试、地址中的 attempt 无效。 */
export function JobNotices({ p }: { p: JobPage }) {
  const j = p.Job
  return (
    <div className={css.notices}>
      {p.AttemptErr && (
        <Callout tone="critical" title="无法显示这次尝试">
          {p.AttemptErr} <Link href={`/jobs/${j.ID}`}>查看当前尝试</Link>
          {p.Attempts.length > 0 && '，或在下方"尝试"中选择'}。
        </Callout>
      )}
      {j.State === 'waiting' && j.WaitingOn.length > 0 && (
        <Callout title="等待上游 job">
          等 {j.WaitingOn.join('、')} 完成后{p.Publish ? '发布' : '开始'}。
        </Callout>
      )}
      {j.State === 'pending' && j.WaitReason && (
        <Callout tone="warning" title="等待 Runner">
          {j.WaitReason}
        </Callout>
      )}
      {j.State === 'pending' && !j.WaitReason && p.Queue && <QueueNotice job={j} queue={p.Queue} />}
      {j.Reason !== null && (
        <Callout tone={stateTone(j.State) === 'bad' ? 'critical' : 'info'} title={stateTone(j.State) === 'bad' ? '失败原因' : '说明'} hideTitle>
          {j.Reason}
        </Callout>
      )}
      <JobProgress job={j} />
      {j.ReusedFrom !== null ? (
        <Callout title="沿用上一次运行的结果" hideTitle>
          沿用上一次运行的结果：<Link href={`/jobs/${j.ReusedFrom}`}>job #{j.ReusedFrom}</Link>（这个 job 没有重新执行，日志和产物都来自它）
        </Callout>
      ) : (
        j.CachedFrom !== null && (
          <Callout title="命中缓存" hideTitle>
            结果来自 <Link href={`/jobs/${j.CachedFrom}`}>job #{j.CachedFrom}</Link>（命中缓存，没有重新执行）
          </Callout>
        )
      )}
      {p.Viewing && (
        <Callout tone="warning" title={`正在查看第 ${p.Viewing.N} 次尝试`}>
          第 {p.Viewing.N} 次尝试（#{p.Viewing.ID}，{outcome(p.Viewing.Outcome)}）不是当前的尝试。job 当前状态：{jobState(j.State)}。{' '}
          <Link href={`/jobs/${j.ID}`}>查看当前尝试</Link>
        </Callout>
      )}
    </div>
  )
}

/** JobActions 是右上角的操作：run 进行中时取消整个 run，结束后重跑这个 job。 */
export function JobActions({ p }: { p: JobPage }) {
  if (p.RunActive) return <CancelRun runID={p.Run.ID} />
  if (!p.Rerun) return null
  if (!p.Rerun.Latest) {
    return (
      <p className={css.actionNote}>
        {rerunLabel(p.Job.Rerun)}不能直接重跑。
        {p.Rerun.LatestID ? <Link href={`/jobs/${p.Rerun.LatestID}`}>打开最新一次运行中的这个 job</Link> : '最新一次运行中没有这个 job。'}
      </p>
    )
  }
  return <RerunJob p={p} rerun={p.Rerun} />
}

function CancelRun({ runID }: { runID: number }) {
  const [open, setOpen] = useState(false)
  useRefreshLock(open)
  return (
    <>
      <Button variant="danger" onClick={() => setOpen(true)}>
        取消整个 run
      </Button>
      {open && (
        <Dialog title={`取消 Run #${runID}？`} onClose={() => setOpen(false)} width="medium">
          <Form action={`/runs/${runID}/cancel`} aria-label="取消整个 run">
            <p className={css.dialogText}>
              取消的是整个 Run #{runID}（不只是这个 job）：正在执行的 job 会被终止，还没有开始的 job 不再执行。已经完成的 job 不受影响，已经发布的 Release 或镜像不会撤回。
            </p>
            <div className={css.dialogActions}>
              <Button onClick={() => setOpen(false)}>不取消</Button>
              <SubmitButton variant="danger">取消整个 run</SubmitButton>
            </div>
          </Form>
        </Dialog>
      )}
    </>
  )
}

/** rerunEffects 返回重跑这个 job 的影响说明（每条一句）。 */
export function rerunEffects(r: Rerun, currentRerun: number): string[] {
  const out = [
    r.Force ? '这个 job 上次成功，会强制重新执行（不使用缓存）。' : '这个 job 重新规划，输入没变时仍可能命中缓存。',
    r.Downstream.length > 0
      ? `下游 ${r.Downstream.length} 个 job 也会重新规划（可命中缓存）：${r.Downstream.join('、')}。`
      : '没有下游 job。',
    `其他 ${r.Kept} 个 job 沿用这一次的结果，不重新执行。`,
  ]
  if (r.Replan > 0) out.push(`这次运行的产物已经按保留策略清理，另有 ${r.Replan} 个成功的 job 无法沿用结果，也会重新规划（可命中缓存）。`)
  out.push(`会新建一次重跑（${rerunLabel(currentRerun)}之后的下一次）。`)
  return out
}

function RerunJob({ p, rerun }: { p: JobPage; rerun: Rerun }) {
  const [open, setOpen] = useState(false)
  useRefreshLock(open)
  return (
    <>
      <Button leadingVisual={SyncIcon} onClick={() => setOpen(true)}>
        重跑这个 job
      </Button>
      {open && (
        <Dialog title="重跑这个 job 及其下游" subtitle={p.Job.DisplayName} onClose={() => setOpen(false)} width="large">
          <Form action={`/jobs/${p.Job.ID}/rerun`} aria-label="重跑这个 job">
            <ul className={css.effects}>
              {rerunEffects(rerun, p.Run.CurrentRerun).map((t) => (
                <li key={t}>{t}</li>
              ))}
              {rerun.Publish && (
                <li>
                  <strong>重新规划的 job 中有发布 job，会再次执行发布</strong>（按 tag 幂等：已存在且内容一致的 asset 会跳过）。
                </li>
              )}
            </ul>
            <FormControl>
              <Checkbox name="keep" />
              <FormControl.Label>失败时保留工作区</FormControl.Label>
              <FormControl.Caption>job 失败后在 Runner 上保留现场，便于登录排查。</FormControl.Caption>
            </FormControl>
            <div className={css.dialogActions}>
              <Button onClick={() => setOpen(false)}>取消</Button>
              <SubmitButton variant="primary" leadingVisual={SyncIcon}>
                重跑这个 job 及其下游
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
  const j = p.Job
  return (
    <p className={css.intro}>
      {j.ReusedFrom !== null ? (
        <>
          沿用 <Link href={`/jobs/${j.ReusedFrom}`}>job #{j.ReusedFrom}</Link> 的结果
        </>
      ) : j.CachedFrom !== null ? (
        <>
          命中缓存，由 <Link href={`/jobs/${j.CachedFrom}`}>job #{j.CachedFrom}</Link> 生成
        </>
      ) : p.Producer ? (
        `由第 ${p.Producer.N} 次尝试（#${p.Producer.ID}）生成`
      ) : (
        '由这个 job 生成'
      )}
      。下载需要登录；下载地址 5 分钟内有效，不要转发。
    </p>
  )
}

function downloadURL(jobID: number, output: string, path: string): string {
  return `/jobs/${jobID}/download?${new URLSearchParams({ output, path }).toString()}`
}

function OutputBlock({ jobID, o }: { jobID: number; o: Output }) {
  const hidden = o.FileCount - o.Files.length
  return (
    <div className={css.output}>
      <div className={css.outputHead}>
        <PackageIcon size={16} />
        <h3 className={css.outputName}>{o.Name}</h3>
        <span className={css.muted}>{o.Missing ? '清单已不在存储中' : `${o.FileCount} 个文件，${size(o.Size)}`}</span>
      </div>
      {o.Missing ? (
        <p className={css.outputNote}>这个输出的清单已不在存储中，无法下载。重跑这个 job（不用缓存）可以重新生成。</p>
      ) : (
        <ul className={css.files}>
          {o.Files.map((f) => (
            <li key={f.path} className={css.file}>
              {f.symlink ? <FileSymlinkFileIcon size={16} className={css.fileIcon} /> : <FileIcon size={16} className={css.fileIcon} />}
              <span className={css.filePath}>
                <code>{f.path}</code>
                {f.exec && <Label variant="secondary">可执行</Label>}
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
                  <span className={css.muted}>符号链接</span>
                ) : f.Missing ? (
                  <span className={css.bad}>已不在存储中</span>
                ) : (
                  <LinkButton href={downloadURL(jobID, o.Name, f.path)} size="small" leadingVisual={DownloadIcon} aria-label={`下载 ${o.Name}/${f.path}`}>
                    下载
                  </LinkButton>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}
      {!o.Missing && hidden > 0 && (
        <p className={css.outputNote}>
          还有 {hidden} 个文件没有列出（只列出按路径排序的前 {o.Files.length} 个）。
        </p>
      )}
    </div>
  )
}

/** OutputsSection 是产物：每个输出的文件和下载。 */
export function OutputsSection({ p }: { p: JobPage }) {
  const o = p.Outputs
  const has = o !== null && o.Manifest !== '' && (o.Missing || o.Outputs.length > 0)
  return (
    <Box as="section" aria-labelledby="outputs-h">
      <BoxHeader title="产物" id="outputs-h" />
      {has ? (
        <>
          <BoxBody className={css.introBody}>
            <OutputsIntro p={p} />
          </BoxBody>
          {o.Missing ? (
            <BoxBody>
              {p.Purged !== null ? (
                <Callout title="产物已清理" hideTitle>
                  产物已在 {clock(p.Purged)} 按保留策略清理（run 结束超过保留期后只保留结果），无法下载。重跑这个 job 可以重新生成。
                </Callout>
              ) : (
                <Callout tone="warning" title="产物清单不在存储中" hideTitle>
                  产物清单已不在存储中，无法列出和下载。重跑这个 job（不用缓存）可以重新生成。
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
              ? `声明了输出 ${p.Declared.join('、')}。${p.Terminal ? 'job 没有成功，没有产物。' : 'job 成功后可以在这里下载。'}`
              : '这个 job 没有声明 outputs，没有产物。'}
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

function TargetResult({ t }: { t: ReleaseTarget }) {
  switch (t.Kind) {
    case 'github': {
      const d = githubDetailOf(t.Detail)
      return (
        <>
          {d.html_url && <ExternalLink href={d.html_url}>{d.html_url}</ExternalLink>}
          {d.draft && <span className={css.muted}>（notes.from: draft，保持 draft，请在 GitHub 上手动发布）</span>}
        </>
      )
    }
    case 'image': {
      const d = imageDetailOf(t.Detail)
      return (
        <>
          {d.digest && <code className={css.wrap}>{d.digest}</code>}
          {d.tags && d.tags.length > 0 && <span> tag：{d.tags.join('、')}</span>}
        </>
      )
    }
    case 'upload': {
      const d = uploadDetailOf(t.Detail)
      return (
        <>
          {d.previous && <span className={css.muted}>之前已发布，内容一致。</span>}
          {t.State === 'committed' && d.latest && <span className={css.muted}>已设为最新版。</span>}
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
          {d.previous && <span className={`${css.muted} ${css.block}`}>已发布过，内容一致，本次没有重新发布。</span>}
        </>
      )
    }
    default:
      return null
  }
}

/** ReleaseSection 是发布 job 的发布进度和各目标的结果。 */
export function ReleaseSection({ release }: { release: Release | null }) {
  if (!release) {
    return (
      <Box as="section" aria-labelledby="release-h">
        <BoxHeader title="发布" id="release-h" />
        <BoxBody>
          <p className={css.emptyText}>还没有开始发布。</p>
        </BoxBody>
      </Box>
    )
  }
  const legacy = release.Targets.length === 0 ? legacyProgressOf(release.Progress) : null
  return (
    <Box as="section" aria-labelledby="release-h">
      <BoxHeader title="发布" id="release-h">
        <Label variant={labelVariant(stateTone(release.State))} title={release.State}>
          {releaseState(release.State)}
        </Label>
      </BoxHeader>
      <BoxBody>
        <dl className={css.kv}>
          <dt>发布目标</dt>
          <dd className={css.wrap}>{release.Target}</dd>
          <dt>tag</dt>
          <dd>
            <code>{release.Tag}</code>
          </dd>
          <dt>阶段</dt>
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
              <dt>镜像</dt>
              <dd className={css.wrap}>
                <code>
                  {i.name}@{i.digest}
                </code>
                {i.tags && i.tags.length > 0 && ` tag：${i.tags.join('、')}`}
              </dd>
            </div>
          ))}
          {release.Error && (
            <>
              <dt>错误</dt>
              <dd className={`${css.wrap} ${css.bad}`}>{release.Error}</dd>
            </>
          )}
        </dl>
      </BoxBody>
      {release.Targets.map((t) => (
        <BoxRow key={`${t.Kind}:${t.Name}`} className={css.target}>
          <StatusIcon state={t.State} tone={targetTone(t.State)} label={targetState(t.State)} />
          <div className={css.targetMain}>
            <div className={css.targetName}>
              <strong>{t.Name}</strong>
              <span className={css.muted}>{targetKind(t.Kind)}</span>
              <Label variant={labelVariant(targetTone(t.State))} title={t.State}>
                {targetState(t.State)}
              </Label>
            </div>
            {t.Kind === 'upload' && t.Name !== t.Target && <code className={`${css.muted} ${css.wrap}`}>{t.Target}</code>}
            {t.Error && <p className={css.targetError}>{t.Error}</p>}
            <div className={css.targetResult}>
              <TargetResult t={t} />
            </div>
          </div>
        </BoxRow>
      ))}
    </Box>
  )
}

function AttemptRow({ a, jobID, viewing }: { a: Attempt; jobID: number; viewing: boolean }) {
  const t = outcomeTone(a.Outcome)
  return (
    <BoxRow className={`${css.attempt} ${viewing ? css.attemptViewing ?? '' : ''}`} aria-current={viewing ? 'true' : undefined}>
      <StatusIcon state={a.Outcome || 'running'} tone={t} label={outcome(a.Outcome)} />
      <div className={css.attemptMain}>
        <div className={css.attemptTitle}>
          <strong>第 {a.N} 次</strong>
          <Label variant={labelVariant(t)} title={a.Outcome}>
            {outcome(a.Outcome)}
          </Label>
          {a.Current && <Label variant="accent">当前</Label>}
        </div>
        <p className={css.attemptMeta}>
          {a.Runner}，<Duration start={a.Started} end={a.Finished} />
          {a.ExitCode !== null && `，退出码 ${a.ExitCode}`}
          {a.ErrorClass && `，${a.ErrorClass}`} <code>#{a.ID} · epoch {a.Epoch}</code>
        </p>
        {a.Message && <p className={css.attemptMsg}>{a.Message}</p>}
      </div>
      <div className={css.attemptAction}>
        {viewing ? <span className={css.muted}>正在查看日志</span> : <Link href={`/jobs/${jobID}?attempt=${a.ID}`}>查看日志</Link>}
      </div>
    </BoxRow>
  )
}

/** AttemptsSection 是每次尝试的结果。不会再有尝试的 job（命中缓存、跳过、发布）没有尝试时不显示。 */
export function AttemptsSection({ p }: { p: JobPage }) {
  if (p.Attempts.length === 0 && (p.Terminal || p.Publish)) return null
  return (
    <Box as="section" aria-labelledby="attempts-h">
      <BoxHeader
        title={
          <>
            尝试 {p.Attempts.length > 0 && <CounterLabel>{p.Attempts.length}</CounterLabel>}
          </>
        }
        id="attempts-h"
      />
      {p.Attempts.length === 0 ? (
        <BoxBody>
          <p className={css.emptyText}>还没有开始执行。开始后这里会列出每次尝试，日志也会自动出现，不用刷新页面。</p>
        </BoxBody>
      ) : (
        <>
          {p.Attempts.map((a) => (
            <AttemptRow key={a.ID} a={a} jobID={p.Job.ID} viewing={a.ID === p.Attempt} />
          ))}
          <BoxFooter className={css.footNote}>遇到基础设施错误（Runner 掉线、准备环境失败等）会自动重试，产生新的尝试；重跑会新建 job，不算在这里。</BoxFooter>
        </>
      )}
    </Box>
  )
}

/** SpecSection 是 job spec 或 AI 任务的执行输入（JSON），默认收起。 */
export function SpecSection({ spec, title = 'Job spec' }: { spec: string; title?: string }) {
  return (
    <details className={css.spec}>
      <summary>{title}</summary>
      <pre>{spec}</pre>
    </details>
  )
}
