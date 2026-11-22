import { describe, expect, it } from 'vitest'
import { LogBuffer } from './buffer'

const texts = (root: HTMLElement) => Array.from(root.children, (c) => c.textContent)

describe('LogBuffer', () => {
  it('按行显示，没有换行符的最后一行接上下一段', () => {
    const root = document.createElement('div')
    const b = new LogBuffer(root)
    b.append('a\nb')
    b.append('c\nd\n')
    b.append('e')
    expect(texts(root)).toEqual(['a', 'bc', 'd', 'e'])
    expect(b.lines).toEqual(['a', 'bc', 'd', 'e'])
    expect(b.lineCount).toBe(4)
    expect(b.shown).toBe('a\nbc\nd\ne'.length)
  })

  it('去掉 ANSI 转义，包括跨片段的半个转义', () => {
    const root = document.createElement('div')
    const b = new LogBuffer(root)
    expect(b.append('\x1b[3')).toBe(false)
    b.append('1mred\x1b[0m\n')
    expect(texts(root)).toEqual(['red'])
  })

  it('超过上限时丢弃最早的行，行号接着数', () => {
    const root = document.createElement('div')
    const b = new LogBuffer(root, 10)
    b.append('1111\n2222\n3333\n')
    expect(b.truncated).toBe(false)
    expect(b.trim()).toBe(1)
    expect(b.trim()).toBe(0)
    expect(b.truncated).toBe(true)
    expect(texts(root)).toEqual(['2222', '3333'])
    expect(b.lineCount).toBe(3)
    expect(b.shown).toBeLessThanOrEqual(10)
  })

  it('清空后重新开始；搜索结果对应到行中的文字', () => {
    const root = document.createElement('div')
    const b = new LogBuffer(root)
    b.append('--- FAIL: TestA\n')
    expect(b.range({ line: 0, start: 4, length: 4 })?.toString()).toBe('FAIL')
    expect(b.range({ line: 3, start: 0, length: 1 })).toBeNull()
    b.clear()
    expect(root.children).toHaveLength(0)
    expect(b.lineCount).toBe(0)
  })
})
