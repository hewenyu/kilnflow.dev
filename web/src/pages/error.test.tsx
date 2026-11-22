import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import fixture from '../../dev/fixtures/error.json'
import { envelope, renderPage, withData } from '../test/render'

describe('错误页', () => {
  it('显示状态码、原因、错误编号和恢复入口（主要操作在前）', () => {
    renderPage(envelope(fixture), '/runs/128')
    expect(screen.getByText('e3f1a09c').parentElement).toHaveTextContent('HTTP 500 · 错误编号 e3f1a09c')
    expect(screen.getByRole('heading', { level: 1, name: '服务器出错了' })).toBeInTheDocument()
    expect(screen.getByText('e3f1a09c')).toBeInTheDocument()
    const nav = screen.getByRole('navigation', { name: '接下来可以' })
    const links = Array.from(nav.querySelectorAll('a'))
    expect(links.map((a) => [a.textContent, a.getAttribute('href')])).toEqual([
      ['重试', '/runs/128'],
      ['返回运行列表', '/runs'],
    ])
    expect(links[0]).toHaveAttribute('data-variant', 'primary')
  })

  it('没有错误编号和恢复入口时不显示这两部分', () => {
    renderPage(withData(envelope(fixture), { Ref: '', Links: null }), '/x')
    expect(screen.queryByText('e3f1a09c')).not.toBeInTheDocument()
    expect(screen.getByText('HTTP 500')).toBeInTheDocument()
    expect(screen.queryByRole('navigation', { name: '接下来可以' })).not.toBeInTheDocument()
  })
})
