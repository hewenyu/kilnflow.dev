import { size } from './format'
import type { Progress } from './schemas'
import { lookup } from './lookup'

// 执行阶段与进度的说明：Runner 在心跳中上报，run 图和 job 页据此说明 job 正在做什么。
// 只显示有依据的数字：没有总字节数时不显示百分比，不估算剩余时间。

const phaseLabels: Record<string, string> = {
  prepare: '准备执行环境',
  step: '执行 step',
  upload: '上传产物到 Kiln Server',
  cleanup: '清理执行环境',
  complete: '提交结果',
}

/** phaseLabel 返回阶段的中文名称；不认识的阶段返回"执行中"。 */
export function phaseLabel(phase: string): string {
  return lookup(phaseLabels, phase) ?? '执行中'
}

/** uploading 表示正在上传产物（已经知道要上传多少）。 */
export function uploading(p: Progress): boolean {
  return p.Phase === 'upload' && p.FilesTotal > 0
}

/** uploadPercent 返回上传的百分比（已确认 + 正在发送的字节，向下取整）；没有总字节数时返回 null。 */
export function uploadPercent(p: Progress): number | null {
  if (!uploading(p) || p.BytesTotal <= 0) return null
  return Math.min(100, Math.floor(((p.BytesDone + p.BytesSent) * 100) / p.BytesTotal))
}

/** awaitingConfirm 表示当前文件已经全部发送，还在等 Server 确认校验（这时不能算上传完成）。 */
export function awaitingConfirm(p: Progress): boolean {
  return uploading(p) && p.FilesDone < p.FilesTotal && p.BytesDone + p.BytesSent >= p.BytesTotal
}

/** progressSummary 是 run 图节点上的一行说明，例如"上传产物 34%""[2/5] build""准备执行环境：拉取源码"。 */
export function progressSummary(p: Progress): string {
  if (uploading(p)) {
    if (awaitingConfirm(p)) return '上传产物：等待 Server 确认'
    const pct = uploadPercent(p)
    return pct === null ? '上传产物' : `上传产物 ${pct}%`
  }
  if (p.Phase === 'step' && p.Detail) return p.Detail
  return p.Detail ? `${phaseLabel(p.Phase)}：${p.Detail}` : phaseLabel(p.Phase)
}

/** uploadDetail 是上传的详细计数，例如"已确认 0/1 个文件，已发送 64.7 MiB / 129.4 MiB（50%）"。 */
export function uploadDetail(p: Progress): string {
  const pct = uploadPercent(p)
  let s = `已确认 ${p.FilesDone}/${p.FilesTotal} 个文件，已发送 ${size(p.BytesDone + p.BytesSent)} / ${size(p.BytesTotal)}`
  if (pct !== null) s += `（${pct}%）`
  if (p.FilesSkipped > 0) s += `；另有 ${p.FilesSkipped} 个文件 Server 已有，跳过`
  return s
}
