import { z } from 'zod'
import { list } from './envelope'

// 多个页面共用的数据：run、job（服务端 store.Run、store.Job 和 internal/server/web 的 runView、jobView）和合并队列中的一项。
// 字段名与 Go 一致；Go 的指针编码为 null，时间是 RFC 3339 字符串（零值为 0001-01-01T00:00:00Z）。

/** GitHub 事件的上下文（store.GitHubCtx，字段名是 snake_case）。 */
export const githubCtxSchema = z.object({
  event_name: z.string(),
  sha: z.string(),
  ref: z.string(),
  ref_name: z.string(),
  ref_type: z.string(),
  repository: z.string(),
  repository_owner: z.string(),
  head_ref: z.string(),
  base_ref: z.string(),
  actor: z.string(),
})

/** store.Run */
export const runSchema = z.object({
  ID: z.number(),
  RepoID: z.number(),
  /** workflow 文件名（ci.yml）；WorkflowName 是其中的 name，可能为空。 */
  Workflow: z.string(),
  WorkflowName: z.string(),
  Event: z.string(),
  Ref: z.string(),
  SHA: z.string(),
  /** 来源：webhook、dispatch 等。 */
  Source: z.string(),
  PRNumber: z.number().nullable(),
  SenderLogin: z.string(),
  SenderIsBot: z.boolean(),
  CurrentRerun: z.number(),
  ConcurrencyKey: z.string().nullable(),
  CancelInProgress: z.boolean(),
  /** queued、active、succeeded、failed、cancelled、skipped */
  State: z.string(),
  ConfigError: z.string().nullable(),
  Warnings: z.string().nullable(),
  GitHubCtx: githubCtxSchema,
  Inputs: z.record(z.string(), z.string()).nullable(),
  CommitTime: z.number(),
  Priority: z.number(),
  CreatedAt: z.string(),
  FinishedAt: z.string().nullable(),
  /** 最近一次按保留策略清理过程数据的时间（PLAN #10.3）。 */
  PurgedAt: z.string().nullable(),
})
export type Run = z.infer<typeof runSchema>

/** runView：列表中的 run。 */
export const runViewSchema = runSchema.extend({
  /** 仓库全名（owner/name）。 */
  Repo: z.string(),
  /** 最新一次运行中各 job 的状态。 */
  States: list(z.string()),
  /** 详情页地址（从筛选后的列表进入时带上 back）；为空时用 /runs/{ID}。 */
  URL: z.string(),
  /** GitHub 网页地址；为空时不显示指向 GitHub 的链接。 */
  GitHub: z.string(),
  /** 创建和结束时间（Unix 秒），End 为 0 表示还没结束。 */
  Start: z.number(),
  End: z.number(),
})
export type RunView = z.infer<typeof runViewSchema>

/** store.AttemptProgress：Runner 在心跳中上报的执行阶段与进度（时间是 Unix 秒，由服务端按收到的时间换算）。 */
export const progressSchema = z.object({
  /** prepare、step、upload、cleanup、complete；旧版服务端不认识的阶段为空串。 */
  Phase: z.string(),
  Detail: z.string(),
  /** 正在执行的 step 序号（从 0 起，-1 表示还没开始执行 step）。 */
  Step: z.number(),
  PhaseStartedAt: z.number(),
  /** 最近一次进展（发送了数据、对象确认、阶段切换）的时间。 */
  MovedAt: z.number(),
  /** 最近一次上报的时间。 */
  ReportedAt: z.number(),
  /** 上传：需要上传的文件数与字节数（不含 Server 已有的 FilesSkipped 个）、Server 已确认的、正在上传的文件已发送的。 */
  FilesTotal: z.number(),
  FilesDone: z.number(),
  FilesSkipped: z.number(),
  BytesTotal: z.number(),
  BytesDone: z.number(),
  BytesSent: z.number(),
  /** 当前文件的重试次数和最近一次失败的原因。 */
  Retries: z.number(),
  LastError: z.string(),
})
export type Progress = z.infer<typeof progressSchema>

/** jobView：store.Job（不含 Spec）加上 Runner 名称、上游 job 的名称和开始、结束时间（Unix 秒，0 表示没有）。 */
export const jobViewSchema = z.object({
  ID: z.number(),
  /** build、publish 等（workflow.JobKind）。 */
  Kind: z.string(),
  RunID: z.number(),
  Rerun: z.number(),
  JobKey: z.string(),
  JobName: z.string(),
  DisplayName: z.string(),
  Matrix: z.record(z.string(), z.unknown()).nullable(),
  Platform: z.string(),
  Executor: z.string(),
  ActionKey: z.string().nullable(),
  InputDigest: z.string().nullable(),
  Environment: z.string().nullable(),
  /** waiting、pending、running、succeeded、failed、timed_out、cancelled、skipped */
  State: z.string(),
  Reason: z.string().nullable(),
  /** 排队中的 job 没有被领取的原因（调度器最近一次跳过它的原因，例如资源不够时需要多少、各 Runner 能提供多少）。 */
  WaitReason: z.string().nullish(),
  Epoch: z.number(),
  AttemptCount: z.number(),
  Priority: z.number(),
  CurrentAttemptID: z.number().nullable(),
  CancelRequested: z.boolean(),
  CancelReason: z.string().nullable(),
  ResultManifest: z.string().nullable(),
  /** 命中缓存时，结果来自的 job。 */
  CachedFrom: z.number().nullable(),
  ReusedFrom: z.number().nullable(),
  Follows: z.number().nullable(),
  Effects: z.boolean(),
  ForceExecute: z.boolean(),
  KeepOnFailure: z.boolean(),
  CheckRunID: z.number().nullable(),
  StartedStep: z.boolean(),
  LeaseExpiresAt: z.string().nullable(),
  QueuedAt: z.string().nullable(),
  StartedAt: z.string().nullable(),
  FinishedAt: z.string().nullable(),
  CreatedAt: z.string(),
  Runner: z.string(),
  Needs: list(z.string()),
  Started: z.number(),
  Finished: z.number(),
  /** 进行中的 job 最近上报的进度；不在执行中或 Runner 没有上报（旧版）时为 null。 */
  Progress: progressSchema.nullish(),
  /** 等待中的 job 还没有完成的上游 job（名称）。 */
  WaitingOn: z.array(z.string()).nullish().transform((v) => v ?? []),
})
export type JobView = z.infer<typeof jobViewSchema>

/** mergeQueueEntrySchema 是合并队列中的一项（internal/server/web 的 mergeQueueEntry；合并队列页和 run 详情页共用）。 */
export const mergeQueueEntrySchema = z.object({
  PR: z.number(),
  /** queued | merged | removed */
  State: z.string(),
  /** 队列确认过的 head；入队后还没有处理时为空。 */
  HeadSHA: z.string(),
  /** PR 最近观测到的目标分支（PLAN #28.5）：合并队列按目标分支分别排队；旧记录为空。 */
  BaseRef: z.string(),
  /** 入队的操作者：github:<login>（在 GitHub 上加 label），或 Dashboard、CLI 的操作者。 */
  EnqueuedBy: z.string(),
  /** 排队中时当前在等什么。 */
  Status: z.string(),
  /** 出队原因；合并时可能说明是在队列之外合并的。 */
  Reason: z.string(),
  /** 自动出队的类别：conflict_dev（与目标分支冲突，已交给 AI 开发返修）、conflict_author（冲突，等待提交者解决）；其他为空。 */
  Removal: z.string(),
  MergeSHA: z.string(),
  EnqueuedAt: z.string(),
  FinishedAt: z.string().nullable(),
})
export type MergeQueueEntry = z.infer<typeof mergeQueueEntrySchema>

/** runTitle 返回 run 的标题：workflow 的 name，没有时用文件名。 */
export function runTitle(r: Pick<Run, 'WorkflowName' | 'Workflow'>): string {
  return r.WorkflowName || r.Workflow
}

/** runURL 返回 run 详情页的地址。 */
export function runURL(r: Pick<RunView, 'URL' | 'ID'>): string {
  return r.URL || `/runs/${r.ID}`
}
