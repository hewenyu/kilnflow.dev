import { describe, expect, it } from 'vitest'
import { lookup } from './lookup'

describe('lookup', () => {
  const table = { ready: '就绪', unknown: '未上报' }

  it('返回表中的值，不在表中时返回 undefined', () => {
    expect(lookup(table, 'ready')).toBe('就绪')
    expect(lookup(table, 'gone')).toBeUndefined()
  })

  // 与对象原型上的属性同名的 key 不在表中
  it.each(['__proto__', 'constructor', 'toString', 'hasOwnProperty', 'valueOf', 'isPrototypeOf'])('%s 返回 undefined', (key) => {
    expect(lookup(table, key)).toBeUndefined()
  })

  it('表自己有这个 key 时照常返回（包括 __proto__ 这样的名字）', () => {
    const own = Object.fromEntries([
      ['__proto__', '自己的'],
      ['constructor', '也是自己的'],
    ])
    expect(lookup(own, '__proto__')).toBe('自己的')
    expect(lookup(own, 'constructor')).toBe('也是自己的')
  })
})
