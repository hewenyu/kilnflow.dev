import { z } from 'zod'
import { list } from '../../lib/envelope'
import { jobViewSchema, runSchema } from '../../lib/schemas'

// job 详情页的数据（internal/server/web/job_detail.go 的 jobPage，以及 json.go 中加上的 Publish、NoLog、AttemptKey）。

/** attemptView：一次尝试。 */
export const attemptSchema = z.object({
  ID: z.number(),
  Epoch: z.number(),
  Runner: z.string(),
  /** 空串表示还在执行。 */
  Outcome: z.string(),
  ErrorClass: z.string(),
  Message: z.string(),
  ExitCode: z.number().nullable(),
  Started: z.number(),
  Finished: z.number(),
  /** 第 N 次尝试。 */
  N: z.number(),
  Current: z.boolean(),
})
export type Attempt = z.infer<typeof attemptSchema>

const record = z.record(z.string(), z.unknown()).nullable()

/** releaseTargetView：一个发布目标的结果。Detail 的内容按 Kind 不同，见 targetDetail。 */
export const releaseTargetSchema = z.object({
  Kind: z.string(),
  Name: z.string(),
  Target: z.string(),
  State: z.string(),
  Detail: record,
  Error: z.string(),
  /** 发布结果的页面（发布到 npm 官方 registry 的包在 www.npmjs.com 上的版本页），没有时为空。 */
  URL: z.string(),
})
export type ReleaseTarget = z.infer<typeof releaseTargetSchema>

/** releaseView：发布 job 的发布进度。 */
export const releaseSchema = z.object({
  Target: z.string(),
  Tag: z.string(),
  State: z.string(),
  Stage: z.string(),
  Progress: record,
  Error: z.string(),
  /** v0.25 之前的发布没有，只显示 Progress。 */
  Targets: list(releaseTargetSchema),
})
export type Release = z.infer<typeof releaseSchema>

/** admin.OutputFile：cas.FileEntry（json 标签是小写）加上 Missing。 */
export const outputFileSchema = z.object({
  path: z.string(),
  digest: z.string().optional(),
  size: z.number(),
  exec: z.boolean().optional(),
  symlink: z.string().optional(),
  Missing: z.boolean(),
})
export type OutputFile = z.infer<typeof outputFileSchema>

/** admin.JobOutput：一个命名输出。Files 只是按路径排序的前若干个，FileCount、Size 是全部文件的。 */
export const outputSchema = z.object({
  Name: z.string(),
  Digest: z.string(),
  Missing: z.boolean(),
  Files: list(outputFileSchema),
  FileCount: z.number(),
  Size: z.number(),
})
export type Output = z.infer<typeof outputSchema>

/** admin.JobOutputs：Manifest 为空表示 job 没有结果。 */
export const outputsSchema = z.object({
  Manifest: z.string(),
  Missing: z.boolean(),
  Outputs: list(outputSchema),
})
export type Outputs = z.infer<typeof outputsSchema>

/** jobRerun：重跑这个 job 的影响。 */
export const rerunSchema = z.object({
  /** 为 false 表示 job 不属于最新一次运行；LatestID 是最新一次运行中同一个 job（没有时为 0）。 */
  Latest: z.boolean(),
  LatestID: z.number(),
  /** job 上次成功，重跑时强制重新执行。 */
  Force: z.boolean(),
  Downstream: list(z.string()),
  Kept: z.number(),
  Publish: z.boolean(),
  /** 产物已清理、不能沿用而重新规划的其他 job 数。 */
  Replan: z.number(),
})
export type Rerun = z.infer<typeof rerunSchema>

/** cistore.QueuePosition：排队中的 job 前面还有多少个待领取的 job（调度解释）。 */
export const queueSchema = z.object({
  /** 排在它前面、与它 platform 和 executor 相同的待领取 job 数。 */
  Ahead: z.number(),
  /** 其中排在最前面的 job 所属的 run；Ahead 为 0 时为 0。 */
  FirstRunID: z.number(),
})
export type Queue = z.infer<typeof queueSchema>

export const jobPageSchema = z.object({
  Job: jobViewSchema,
  Run: runSchema,
  Repo: z.string(),
  Attempts: list(attemptSchema),
  /** 日志读取的 attempt（地址中指定的或当前的）；还没有 attempt 时为 0。 */
  Attempt: z.number(),
  /** 地址中指定了 attempt。 */
  Explicit: z.boolean(),
  /** 地址中指定的、不是当前的那次尝试。 */
  Viewing: attemptSchema.nullable(),
  /** 地址中的 attempt 无效（页面状态码 404，不加载日志）。 */
  AttemptErr: z.string(),
  /** job spec（JSON，缩进过）。 */
  Spec: z.string(),
  Terminal: z.boolean(),
  RunActive: z.boolean(),
  Release: releaseSchema.nullable(),
  Outputs: outputsSchema.nullable(),
  Declared: list(z.string()),
  /** 生成产物的那次尝试（命中缓存、沿用结果时为 null）。 */
  Producer: attemptSchema.nullable(),
  /** run 没有结束时为 null。 */
  Rerun: rerunSchema.nullable(),
  LogEmpty: z.string(),
  /** 日志和产物按保留策略清理的时间。 */
  Purged: z.string().nullable(),
  /** 同一次运行中的全部 job（左侧列表）。 */
  Jobs: list(jobViewSchema),
  /** 排队中、没有跳过原因时它前面的待领取 job；其他情况为 null。 */
  Queue: queueSchema.nullish(),
  /** 发布 job（由 Server 执行，没有 attempt）。 */
  Publish: z.boolean(),
  /** 没有执行过、也不会再有日志。 */
  NoLog: z.boolean(),
  AttemptKey: z.string(),
})
export type JobPage = z.infer<typeof jobPageSchema>

// 发布目标和旧版发布进度中用到的字段（其余字段忽略；格式不对时当作没有）

const githubDetail = z.object({ html_url: z.string().optional(), draft: z.boolean().optional() })
const imageDetail = z.object({ digest: z.string().optional(), tags: z.array(z.string()).optional() })
const uploadDetail = z.object({
  previous: z.boolean().optional(),
  latest: z.boolean().optional(),
  // url 是外部上传服务返回的下载地址；不安全（非 http/https）的地址由 Server 删除，此时没有 url。
  // assets 为 null 是旧数据（没有安全地址时曾经序列化为 null），与空数组一样忽略。
  assets: z.array(z.object({ name: z.string(), url: z.string().optional() })).nullish(),
})
const npmDetail = z.object({
  package: z.string().optional(),
  version: z.string().optional(),
  dist_tag: z.string().optional(),
  access: z.string().optional(),
  /** registry 中已经有内容相同的这个版本，本次没有再发布。 */
  previous: z.boolean().optional(),
})
const legacyProgress = z.object({
  html_url: z.string().optional(),
  images: z.array(z.object({ name: z.string(), digest: z.string(), tags: z.array(z.string()).nullable().optional() })).optional(),
})

export const githubDetailOf = (d: unknown) => githubDetail.safeParse(d ?? {}).data ?? {}
export const imageDetailOf = (d: unknown) => imageDetail.safeParse(d ?? {}).data ?? {}
export const uploadDetailOf = (d: unknown) => uploadDetail.safeParse(d ?? {}).data ?? {}
export const npmDetailOf = (d: unknown) => npmDetail.safeParse(d ?? {}).data ?? {}
export const legacyProgressOf = (d: unknown) => legacyProgress.safeParse(d ?? {}).data ?? {}
