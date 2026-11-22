import { describe, expect, it } from 'vitest'
import { awaitingConfirm, phaseLabel, progressSummary, uploadDetail, uploadPercent } from './progress'
import type { Progress } from './schemas'

const base: Progress = {
  Phase: 'upload',
  Detail: '上传产物到 Kiln Server',
  Step: 3,
  PhaseStartedAt: 100,
  MovedAt: 200,
  ReportedAt: 200,
  FilesTotal: 1,
  FilesDone: 0,
  FilesSkipped: 0,
  BytesTotal: 135682737,
  BytesDone: 0,
  BytesSent: 0,
  Retries: 0,
  LastError: '',
}

describe('执行进度', () => {
  it('上传：百分比按已确认 + 正在发送的字节计算，向下取整', () => {
    const p = { ...base, BytesSent: 67841368 }
    expect(uploadPercent(p)).toBe(49)
    expect(progressSummary(p)).toBe('上传产物 49%')
    expect(uploadDetail(p)).toBe('已确认 0/1 个文件，已发送 64.7 MiB / 129.4 MiB（49%）')
    expect(uploadDetail({ ...p, FilesSkipped: 2 })).toContain('另有 2 个文件 Server 已有，跳过')
  })

  it('全部发送但 Server 还没确认：不算完成', () => {
    const p = { ...base, BytesSent: base.BytesTotal }
    expect(awaitingConfirm(p)).toBe(true)
    expect(progressSummary(p)).toBe('上传产物：等待 Server 确认')
    const done = { ...p, FilesDone: 1, BytesDone: base.BytesTotal, BytesSent: 0 }
    expect(awaitingConfirm(done)).toBe(false)
    expect(progressSummary(done)).toBe('上传产物 100%')
  })

  it('还不知道要上传多少（收集产物）时不显示百分比', () => {
    const p = { ...base, Detail: '收集产物', FilesTotal: 0, BytesTotal: 0 }
    expect(uploadPercent(p)).toBeNull()
    expect(progressSummary(p)).toBe('上传产物到 Kiln Server：收集产物')
  })

  it('其他阶段：step 显示名称，其余显示阶段和说明', () => {
    expect(progressSummary({ ...base, Phase: 'step', Detail: '[2/5] build' })).toBe('[2/5] build')
    expect(progressSummary({ ...base, Phase: 'prepare', Detail: '拉取源码' })).toBe('准备执行环境：拉取源码')
    expect(progressSummary({ ...base, Phase: 'complete', Detail: '' })).toBe('提交结果')
    expect(phaseLabel('future')).toBe('执行中')
  })
})
