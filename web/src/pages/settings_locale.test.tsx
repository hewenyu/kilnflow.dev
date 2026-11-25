import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { envelope, chineseLeft, rawStrings, renderPage, switchLocale } from '../test/render'

// 设置页的全部 fixtures 都在英文界面中渲染：界面文字全部来自词典，剩下的中文只能是服务端原样给出的内容。
// 按语言切换（不重新取数据）后，标题和正文都随之切换。

const fixtures = import.meta.glob<{ default: unknown }>('../../dev/fixtures/settings*.json', { eager: true })

const cases = Object.entries(fixtures).map(([path, mod]) => ({ name: path.replace(/^.*\//, ''), json: mod.default }))

/**
 * formErrorStrings 返回页面数据中表单（Form、EnvForm 等带 Errors 的对象）字段错误里的字符串：字段错误都是服务端给的界面文字，
 * 应是消息（{id, params}）。rawStrings 把含中文的字符串都当作原样内容从检查中去掉，没迁移的中文错误会因此漏检，所以单独检查。
 */
function formErrorStrings(data: unknown): string[] {
  const out: string[] = []
  const walk = (v: unknown): void => {
    if (Array.isArray(v)) {
      v.forEach(walk)
    } else if (typeof v === 'object' && v !== null) {
      const entries: [string, unknown][] = Object.entries(v)
      for (const [k, x] of entries) {
        if (k === 'Errors' && typeof x === 'object' && x !== null) {
          for (const e of Object.values(x)) if (typeof e === 'string') out.push(e)
        }
        walk(x)
      }
    }
  }
  walk(data)
  return out
}

describe('设置页的英文界面', () => {
  it('覆盖全部设置页 fixtures', () => {
    expect(cases.length).toBeGreaterThanOrEqual(38)
  })

  it('formErrorStrings 找出字段错误中的字符串', () => {
    expect(formErrorStrings({ Form: { Errors: { a: '中文', b: { id: 'x' } } }, List: [{ EnvForm: { Errors: { c: 'raw' } } }] })).toEqual(['中文', 'raw'])
  })

  it.each(cases)('$name：表单的字段错误都是消息，不是原样的文字', ({ json }) => {
    expect(formErrorStrings(envelope(json).Data)).toEqual([])
  })

  it.each(cases)('$name：英文界面没有残留的中文', ({ json }) => {
    const env = envelope(json)
    const { container } = renderPage(env, '/settings', { locale: 'en' })
    expect(screen.getByRole('heading', { level: 1 })).not.toHaveTextContent(/[一-鿿]/)
    expect(chineseLeft(container, rawStrings(env.Data))).toEqual([])
    expect(document.title).not.toMatch(/[一-鿿]/)
  })

  it.each(cases)('$name：中文界面切换到英文后不残留中文，再切回中文', async ({ json }) => {
    const env = envelope(json)
    const { container } = renderPage(env, '/settings')
    const zhHeading = screen.getByRole('heading', { level: 1 }).textContent
    await switchLocale('English')
    const enHeading = screen.getByRole('heading', { level: 1 }).textContent
    // 标题含界面文字时随语言变化；作用域页的标题是对象名（例如 acme/release），原样显示
    if (/[一-鿿]/.test(zhHeading)) expect(enHeading).not.toBe(zhHeading)
    expect(chineseLeft(container, rawStrings(env.Data))).toEqual([])
    await switchLocale('简体中文')
    expect(screen.getByRole('heading', { level: 1 }).textContent).toBe(zhHeading)
  })
})
