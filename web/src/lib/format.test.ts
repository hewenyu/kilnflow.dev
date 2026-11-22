import { describe, expect, it } from 'vitest'
import { ago, avatarHue, avatarURL, clock, dur, eventLabel, jobState, jobStatesSummary, outcome, refKind, refName, rerunLabel, runState, shortDigest, shortVersion, size, stateTone, timeOfDay } from './format'

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

  // 测试固定在 Asia/Shanghai 时区（vite.config.ts 的 test.env.TZ）：结果不随运行测试的机器变化
  describe('按语言', () => {
    const now = Date.UTC(2026, 10, 5, 12, 0, 0)
    const s = (sec: number) => (now - sec * 1000) / 1000

    it('ago：en 区分单复数，48 小时以前显示本地日期', () => {
      expect(ago(s(1), now, 'en')).toBe('1 second ago')
      expect(ago(s(0), now, 'en')).toBe('0 seconds ago')
      expect(ago(s(2 * 60), now, 'en')).toBe('2 minutes ago')
      expect(ago(s(3600), now, 'en')).toBe('1 hour ago')
      expect(ago(s(49 * 3600), now, 'en')).toBe('Nov 3, 2026, 7:00 PM')
      expect(ago(s(49 * 3600), now, 'zh-CN')).toBe('2026-11-03 19:00')
    })

    it('clock 按浏览器时区和语言格式化', () => {
      const t = Date.UTC(2026, 0, 2, 3, 4, 0) / 1000
      expect(clock(t, 'zh-CN')).toBe('2026-01-02 11:04')
      expect(clock(t, 'en')).toBe('Jan 2, 2026, 11:04 AM')
      expect(clock(0, 'en')).toBe('')
    })

    it('dur 的英文写法', () => {
      expect(dur(100, 145, 0, 'en')).toBe('45s')
      expect(dur(100, 306, 0, 'en')).toBe('3m 26s')
      expect(dur(100, 100 + 3900, 0, 'en')).toBe('1h 5m')
    })

    it('状态、事件和重跑的名称；未知的值原样返回', () => {
      expect(runState('active', 'en')).toBe('In progress')
      expect(jobState('timed_out', 'en')).toBe('Timed out')
      expect(outcome('', 'en')).toBe('In progress')
      expect(outcome('', 'zh-CN')).toBe('执行中')
      expect(eventLabel('workflow_dispatch', 'en')).toBe('Manual')
      expect(runState('mystery', 'en')).toBe('mystery')
      expect(rerunLabel(0, 'en')).toBe('First run')
      expect(rerunLabel(2, 'en')).toBe('Re-run 2')
    })

    it.each([
      [[], '0 jobs: '],
      [['succeeded'], '1 job: Succeeded 1'],
      [['succeeded', 'running'], '2 jobs: Succeeded 1, In progress 1'],
    ])('job 状态汇总的复数：%j', (states, want) => {
      expect(jobStatesSummary(states, 'en')).toBe(want)
    })
  })
})
