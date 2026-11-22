import { describe, expect, it } from 'vitest'
import { backoff, findMatches, logDownloadURL, parseLogMeta, parsePosition, stepIndex, storageKey, stripAnsi } from './log'

const ESC = '\x1b'

describe('stripAnsi', () => {
  it('去掉颜色等 CSI 序列', () => {
    expect(stripAnsi('', `${ESC}[31m--- FAIL${ESC}[0m ok`)).toEqual({ text: '--- FAIL ok', pending: '' })
    expect(stripAnsi('', `${ESC}[1;32mPASS${ESC}[m`).text).toBe('PASS')
  })

  it('片段末尾不完整的转义留到下一段，拼起来后去掉', () => {
    const a = stripAnsi('', `ok ${ESC}[3`)
    expect(a).toEqual({ text: 'ok ', pending: `${ESC}[3` })
    const b = stripAnsi(a.pending, `2mgreen${ESC}`)
    expect(b).toEqual({ text: 'green', pending: ESC })
    expect(stripAnsi(b.pending, '[0m done')).toEqual({ text: ' done', pending: '' })
  })
})

describe('parseLogMeta', () => {
  const headers = (extra: Record<string, string> = {}) =>
    new Headers({
      'Content-Type': 'text/plain; charset=utf-8',
      'X-Next-Offset': '1024',
      'X-Finished': 'true',
      'X-Job-State': 'failed',
      'X-Job-Terminal': 'true',
      'X-Attempt': '31',
      'X-Current-Attempt': '32',
      ...extra,
    })

  it('解析服务端的响应头（布尔值是 true / false）', () => {
    expect(parseLogMeta(headers())).toEqual({ next: 1024, finished: true, state: 'failed', terminal: true, attempt: 31, current: 32 })
    expect(parseLogMeta(headers({ 'X-Finished': 'false', 'X-Job-Terminal': 'false' }))).toMatchObject({ finished: false, terminal: false })
  })

  it('不是日志时返回 null：HTML、缺少 X-Next-Offset、offset 无效', () => {
    expect(parseLogMeta(headers({ 'Content-Type': 'text/html; charset=utf-8' }))).toBeNull()
    expect(parseLogMeta(new Headers({ 'Content-Type': 'text/plain' }))).toBeNull()
    expect(parseLogMeta(headers({ 'X-Next-Offset': 'abc' }))).toBeNull()
  })
})

describe('搜索', () => {
  it('不区分大小写，一行中可以有多处，最多 limit 处', () => {
    const lines = ['--- FAIL: TestA', 'ok pkg', 'fail fail', 'FAIL']
    expect(findMatches(lines, 'fail')).toEqual([
      { line: 0, start: 4, length: 4 },
      { line: 2, start: 0, length: 4 },
      { line: 2, start: 5, length: 4 },
      { line: 3, start: 0, length: 4 },
    ])
    expect(findMatches(lines, 'fail', 2)).toHaveLength(2)
    expect(findMatches(lines, '')).toEqual([])
    expect(findMatches(lines, 'nothing')).toEqual([])
  })

  it('上一个、下一个循环；还没有选中时下一个是第一处、上一个是最后一处', () => {
    expect(stepIndex(-1, 1, 3)).toBe(0)
    expect(stepIndex(-1, -1, 3)).toBe(2)
    expect(stepIndex(2, 1, 3)).toBe(0)
    expect(stepIndex(0, -1, 3)).toBe(2)
    expect(stepIndex(0, 1, 0)).toBe(-1)
  })
})

describe('其他', () => {
  it('退避：每次加倍，最多 max', () => {
    expect([0, 1, 2, 5].map((f) => backoff(1500, f, 30000))).toEqual([1500, 3000, 6000, 30000])
  })

  it('下载地址：指定 attempt 时带上', () => {
    expect(logDownloadURL('/jobs/412', 0)).toBe('/jobs/412/log/download')
    expect(logDownloadURL('/jobs/412', 31)).toBe('/jobs/412/log/download?attempt=31')
    expect(logDownloadURL('/ai/tasks/9', 4)).toBe('/ai/tasks/9/log/download?attempt=4')
  })

  it('阅读位置：键和格式校验', () => {
    expect(storageKey('/jobs/412', 'current')).toBe('kiln-log:/jobs/412:current')
    expect(storageKey('/ai/tasks/412', 'current')).not.toBe(storageKey('/jobs/412', 'current'))
    expect(parsePosition('{"follow":false,"top":320}')).toEqual({ follow: false, top: 320 })
    expect(parsePosition('{"follow":"no","top":1}')).toBeNull()
    expect(parsePosition('not json')).toBeNull()
    expect(parsePosition(null)).toBeNull()
  })
})
