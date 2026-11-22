import { describe, expect, it } from 'vitest'
import { ago, avatarHue, avatarURL, dur, jobStatesSummary, refKind, refName, shortDigest, shortVersion, size, stateTone, timeOfDay } from './format'

describe('format', () => {
  it('timeOfDay 显示本地的时:分:秒，没有时间时为空', () => {
    expect(timeOfDay(new Date(2026, 10, 5, 4, 3, 9).toISOString())).toBe('04:03:09')
    expect(timeOfDay('0001-01-01T00:00:00Z')).toBe('')
    expect(timeOfDay(null)).toBe('')
  })

  it('ago：秒、分钟、小时，48 小时以前显示日期', () => {
    const now = Date.UTC(2026, 10, 5, 12, 0, 0)
    const s = (sec: number) => (now - sec * 1000) / 1000
    expect(ago(s(5), now)).toBe('5 秒前')
    expect(ago(s(125), now)).toBe('2 分钟前')
    expect(ago(s(47 * 3600), now)).toBe('47 小时前')
    expect(ago(s(49 * 3600), now)).toMatch(/^2026-11-03 \d\d:00$/)
    expect(ago(0, now)).toBe('')
    expect(ago('0001-01-01T00:00:00Z', now)).toBe('')
  })

  it('dur：秒、分秒、小时分；结束为 0 表示到现在', () => {
    expect(dur(100, 145)).toBe('45 秒')
    expect(dur(100, 306)).toBe('3 分 26 秒')
    expect(dur(0, 306)).toBe('')
    expect(dur(100, 100 + 3900)).toBe('1 小时 5 分')
    expect(dur(100, 0, 160_000)).toBe('1 分 0 秒')
  })

  it('摘要和版本号的缩写', () => {
    expect(shortDigest('sha256:0123456789abcdef')).toBe('0123456789ab')
    expect(shortVersion('0123456789abcdef0123')).toBe('0123456789ab')
    expect(shortVersion('v1.2.3')).toBe('v1.2.3')
    expect(shortVersion('a-very-long-version-name-that-goes-on')).toHaveLength(24)
  })

  it('ref 的名称和种类', () => {
    expect(refName('refs/heads/main')).toBe('main')
    expect(refName('refs/tags/v1.2.0')).toBe('v1.2.0')
    expect(refKind('refs/tags/v1.2.0')).toBe('tag')
    expect(refKind('refs/pull/1/head')).toBeNull()
  })

  it('大小与服务端 bytesize.Format 一致', () => {
    expect(size(12)).toBe('12 B')
    expect(size(3145728)).toBe('3.0 MiB')
    expect(size(1536)).toBe('1.5 KiB')
  })

  it('状态大类', () => {
    expect(stateTone('active')).toBe('run')
    expect(stateTone('timed_out')).toBe('bad')
    expect(stateTone('waiting')).toBe('wait')
    expect(stateTone('cancelled')).toBe('muted')
  })

  it('job 状态汇总', () => {
    expect(jobStatesSummary(['succeeded', 'running', 'running', 'waiting'])).toBe('4 个 job：成功 1，执行中 2，等待上游 1')
  })

  it('头像：GitHub 登录名才有头像地址，色相稳定', () => {
    expect(avatarURL('octocat')).toBe('https://avatars.githubusercontent.com/octocat?s=96')
    expect(avatarURL('admin')).toBeNull()
    expect(avatarURL('dependabot[bot]')).toBeNull()
    expect(avatarHue('Alice')).toBe(avatarHue('alice'))
  })
})
