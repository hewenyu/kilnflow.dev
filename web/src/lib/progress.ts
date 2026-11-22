import { defaultLocale, isMsgKey, msg, translate, type Locale, type MsgKey, type Params } from '../i18n/messages'
import { size } from './format'
import type { Progress } from './schemas'

// 执行阶段与进度的说明：Runner 在心跳中上报，run 图和 job 页据此说明 job 正在做什么。
// 只显示有依据的数字：没有总字节数时不显示百分比，不估算剩余时间。

const tr = (locale: Locale, id: MsgKey, params?: Params) => translate(locale, msg(id, params))

/** phaseLabel 返回阶段的名称（phase.<阶段>）；不认识的阶段返回"执行中"。 */
export function phaseLabel(phase: string, locale: Locale = defaultLocale): string {
  const key = `phase.${phase}`
  return isMsgKey(key) ? tr(locale, key) : tr(locale, 'phase.unknown')
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
export function progressSummary(p: Progress, locale: Locale = defaultLocale): string {
  if (uploading(p)) {
    if (awaitingConfirm(p)) return tr(locale, 'progress.awaitingConfirm')
    const pct = uploadPercent(p)
    return pct === null ? tr(locale, 'progress.uploading') : tr(locale, 'progress.uploadingPercent', { pct })
  }
  if (p.Phase === 'step' && p.Detail) return p.Detail
  return p.Detail ? tr(locale, 'progress.phaseDetail', { phase: phaseLabel(p.Phase, locale), detail: p.Detail }) : phaseLabel(p.Phase, locale)
}

/** uploadDetail 是上传的详细计数，例如"已确认 0/1 个文件，已发送 64.7 MiB / 129.4 MiB（50%）"。 */
export function uploadDetail(p: Progress, locale: Locale = defaultLocale): string {
  const pct = uploadPercent(p)
  let s = tr(locale, 'progress.uploadCounts', {
    done: p.FilesDone,
    total: p.FilesTotal,
    sent: size(p.BytesDone + p.BytesSent),
    bytes: size(p.BytesTotal),
  })
  if (pct !== null) s += tr(locale, 'progress.uploadPercent', { pct })
  if (p.FilesSkipped > 0) s += tr(locale, 'progress.uploadSkipped', { count: p.FilesSkipped })
  return s
}
