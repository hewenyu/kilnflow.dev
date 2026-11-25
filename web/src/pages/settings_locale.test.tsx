import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { envelope, chineseLeft, rawStrings, renderPage, switchLocale } from '../test/render'

// 设置页的全部 fixtures 都在英文界面中渲染：界面文字全部来自词典，剩下的中文只能是服务端原样给出的内容。
// 按语言切换（不重新取数据）后，标题和正文都随之切换。

const fixtures = import.meta.glob<{ default: unknown }>('../../dev/fixtures/settings*.json', { eager: true })

const cases = Object.entries(fixtures).map(([path, mod]) => ({ name: path.replace(/^.*\//, ''), json: mod.default }))

describe('设置页的英文界面', () => {
  it('覆盖全部设置页 fixtures', () => {
    expect(cases.length).toBeGreaterThanOrEqual(38)
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
