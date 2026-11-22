import { z } from 'zod'
import { list } from '../lib/envelope'
import { lookup } from '../lib/lookup'

// Runner 的页面数据（internal/server/web 的 runnerView，以及 cistore.RunningJob）：Runner 列表、总览和注册页共用。
// AI 执行节点的页面数据见 pages/ai/schema.ts。

/** store.RunningJob：Runner 正在执行的 job。 */
export const runningJobSchema = z.object({
  RunnerID: z.number(),
  JobID: z.number(),
  Name: z.string(),
  RunID: z.number(),
  /** workflow 的名称（没有时为文件名）。 */
  Workflow: z.string(),
  Repo: z.string(),
  Started: z.string(),
})
export type RunningJob = z.infer<typeof runningJobSchema>

/** capacityView：Runner 上报的规格与容量的文字（例如"2 核 / 1.9 GiB"）。 */
export const capacityViewSchema = z.object({
  /** 机器规格（静态，不是实时占用）。 */
  Machine: z.string(),
  /** config.toml 中为宿主机预留的资源。 */
  Reserved: z.string(),
  /** 可分配：机器规格 - 预留。 */
  Allocatable: z.string(),
  /** 已分配：正在执行的 job 声明的 resources 之和。 */
  Allocated: z.string(),
  /** 各 executor 的固定上限，例如"host 上限 2 核 / 6.0 GiB"。 */
  Limits: list(z.string()),
  MaxJobs: z.number(),
})
export type CapacityView = z.infer<typeof capacityViewSchema>

/** upgradeRequestView：一次按台升级的请求（PLAN #27.13）。 */
export const upgradeRequestSchema = z.object({
  ID: z.number(),
  /** 目标版本和请求时 Runner 的版本（不带 v）。 */
  Version: z.string(),
  From: z.string(),
  /** pending、succeeded、failed 或 cancelled。 */
  State: z.string(),
  Error: z.string(),
  Actor: z.string(),
  Created: z.string(),
  Finished: z.string().nullable(),
})
export type UpgradeRequest = z.infer<typeof upgradeRequestSchema>

/** runnerUpgradeView：Runner 的自升级状态和按台升级（PLAN #27.12–#27.14）。 */
export const runnerUpgradeSchema = z.object({
  /**
   * Runner 上报的状态：idle、downloading、ready、draining、switching、confirmed、failed、rolled_back、manual；
   * 为空表示 Runner 没有上报过（版本不支持按台升级）。
   */
  State: z.string(),
  Target: z.string(),
  Error: z.string(),
  /** 经过启动器运行（可以切换版本）。 */
  Launcher: z.boolean(),
  /** config.toml 中 upgrade = false（本地关闭了自升级）。 */
  Disabled: z.boolean(),
  /** 启动器记录的当前版本和上一个版本（回滚用）。 */
  Current: z.string(),
  Previous: z.string(),
  /** 对应的 npm 包和 Runner 查到的最新正式版本；CheckedAt 是最后一次查询成功的时间。 */
  Package: z.string(),
  Latest: z.string(),
  CheckedAt: z.string().nullable(),
  /** 最后一次查询的错误。 */
  CheckError: z.string(),
  /** 已经请求检查更新、Runner 还没有处理。 */
  CheckPending: z.boolean(),
  /** 现在可以升级到 Latest；不能时 Reason 说明原因。 */
  CanUpgrade: z.boolean(),
  Reason: z.string(),
  /** 最近一次升级请求。 */
  Request: upgradeRequestSchema.nullable(),
})
export type RunnerUpgrade = z.infer<typeof runnerUpgradeSchema>

const upgradeStates: Record<string, string> = {
  downloading: '正在下载',
  ready: '已下载，等待空闲后切换',
  draining: '等待正在执行的任务结束后切换',
  switching: '正在切换',
}

/** upgrading 返回进行中的升级请求（没有时为 null）。 */
export function upgrading(u: RunnerUpgrade): UpgradeRequest | null {
  return u.Request?.State === 'pending' ? u.Request : null
}

/** upToDate 表示 Runner 已是查到的最新版本。 */
function upToDate(version: string, u: RunnerUpgrade): boolean {
  return u.Latest !== '' && version.replace(/^v/, '') === u.Latest
}

/** upgradeStateText 返回升级的一句话说明：进行中的请求的进度、上一次失败、可以升级或已是最新。 */
export function upgradeStateText(r: Pick<RunnerView, 'Version' | 'Online'>, u: RunnerUpgrade): string {
  const req = upgrading(u)
  if (req) {
    const step = u.Target === req.Version ? lookup(upgradeStates, u.State) : undefined
    if (step) return `${step}：${req.Version}`
    return r.Online ? `等待开始升级到 ${req.Version}` : `离线，重新连接后升级到 ${req.Version}`
  }
  if (u.Request?.State === 'failed' && !upToDate(r.Version, u)) return `升级到 ${u.Request.Version} 失败`
  if (u.CanUpgrade) return `可以升级到 ${u.Latest}`
  if (upToDate(r.Version, u)) return '已是最新版本'
  return '不能按台升级'
}

/** upgradeTone 返回升级状态的提示级别：上一次升级失败为 bad，可以升级为 warn，正在升级为 run，已是最新为 ok。 */
export function upgradeTone(r: Pick<RunnerView, 'Version'>, u: RunnerUpgrade): 'ok' | 'bad' | 'warn' | 'run' | 'muted' {
  if (upgrading(u)) return 'run'
  if (u.Request?.State === 'failed' && !upToDate(r.Version, u)) return 'bad'
  if (u.CanUpgrade) return 'warn'
  if (upToDate(r.Version, u)) return 'ok'
  return 'muted'
}

const requestStates: Record<string, string> = {
  pending: '进行中',
  succeeded: '已完成',
  failed: '失败',
  cancelled: '已取消',
}

/** requestStateText 返回升级请求的状态文字。 */
export function requestStateText(state: string): string {
  return lookup(requestStates, state) ?? state
}

/** runnerView：CI Runner；注册页上也用它显示 AI 执行节点的摘要（没有 pool、executor、容量和升级）。 */
export const runnerViewSchema = z.object({
  ID: z.number(),
  Name: z.string(),
  Pool: z.string(),
  /** 类型的显示名称：CI Runner；注册页上的 AI 执行节点为 AI 审计节点或 AI 开发节点。 */
  KindLabel: z.string(),
  /** 详情页（CI Runner 在 /runners/{name}，AI 执行节点在 /ai/runners/{name}）。 */
  URL: z.string(),
  Platforms: list(z.string()),
  Executors: list(z.string()),
  Version: z.string(),
  Revoked: z.boolean(),
  /** 已暂停接单（Server 上的 drain 标记）。 */
  Drain: z.boolean(),
  Online: z.boolean(),
  LastSeen: z.string().nullable(),
  Jobs: list(runningJobSchema),
  /** 上报的规格与容量；旧版 kiln-runner 没有上报时为 null。 */
  Capacity: capacityViewSchema.nullish(),
  /** 最近一次上报的本地缓存总大小（字节）和上报时间；没有上报过时为 0 和 null。 */
  CacheBytes: z.number().default(0),
  CacheReportedAt: z.string().nullish(),
  /** 自升级状态和按台升级。 */
  Upgrade: runnerUpgradeSchema.nullish(),
  /** 在线并接单。 */
  Accepting: z.boolean(),
  /** 连接状态的文字（在线、离线、已吊销）和样式（ok、warn、bad）。 */
  ConnLabel: z.string(),
  ConnClass: z.string(),
})
export type RunnerView = z.infer<typeof runnerViewSchema>

/** runnerCounts：全部 Runner（不受筛选影响）的统计。 */
export const runnerCountsSchema = z.object({
  Total: z.number(),
  Online: z.number(),
  Accepting: z.number(),
  Paused: z.number(),
  Offline: z.number(),
  Revoked: z.number(),
  Jobs: z.number(),
})
export type RunnerCounts = z.infer<typeof runnerCountsSchema>

/** RunnerConn 是 CI Runner 和 AI 执行节点共有的连接和接单状态。 */
export type RunnerConn = Pick<RunnerView, 'Revoked' | 'Online' | 'Drain' | 'ConnLabel'>

/** runnerIdle 返回 Runner 当前在做什么的一句话说明（没有正在执行的任务时）。 */
export function runnerIdle(r: RunnerConn): string {
  if (r.Revoked) return '已吊销，不能再连接。'
  if (!r.Online) return r.Drain ? '离线。' : '离线，重新连接后才会领取任务。'
  if (r.Drain) return '空闲，不领取新任务。'
  return '空闲，等待任务。'
}
