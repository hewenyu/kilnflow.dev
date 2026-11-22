import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
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

describe('错误页的语言', () => {
  it('服务端给的标题、原因和入口按当前语言显示', () => {
    renderPage(envelope(fixture), '/runs/128', { locale: 'en' })
    expect(screen.getByRole('heading', { level: 1, name: 'Something went wrong on the server' })).toBeInTheDocument()
    expect(screen.getByText('e3f1a09c').parentElement).toHaveTextContent('HTTP 500 · Error ID e3f1a09c (matches the server log)')
    const nav = screen.getByRole('navigation', { name: 'What you can do next' })
    expect(Array.from(nav.querySelectorAll('a')).map((a) => a.textContent)).toEqual(['Retry', 'Back to runs'])
    expect(document.title).toBe('Something went wrong on the server · Kiln')
  })

  it('原样的原因（对象名等）不翻译；不认识的消息显示兜底说明，不白屏', () => {
    renderPage(withData(envelope(fixture), { Heading: { id: 'error.fromTheFuture' }, Reason: '没有编号为 7 的 run' }), '/x', { locale: 'en' })
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent(/can't be displayed \(error\.fromTheFuture\)/)
    expect(screen.getByText('没有编号为 7 的 run')).toBeInTheDocument()
  })

  it('未登录的错误页（403/404）也可以切换语言', async () => {
    renderPage(envelope(fixture, { Session: null, CSRF: '', Status: 404 }), '/nope')
    await userEvent.click(screen.getByRole('button', { name: '切换语言（当前：简体中文）' }))
    await userEvent.click(await screen.findByRole('menuitemradio', { name: 'English' }))
    expect(screen.getByRole('heading', { level: 1, name: 'Something went wrong on the server' })).toBeInTheDocument()
  })
})
