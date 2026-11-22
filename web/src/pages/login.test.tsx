import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import fixture from '../../dev/fixtures/login.json'
import tokenFixture from '../../dev/fixtures/login.token.json'
import { storageKey } from '../i18n/locale'
import { envelope, renderPage, withData } from '../test/render'

describe('登录页', () => {
  it('配置了 GitHub 登录时以 GitHub 登录为主，admin token 收在备用入口中', () => {
    renderPage(envelope(fixture), '/login?next=%2Fruns')
    expect(screen.getByRole('heading', { level: 1, name: '登录 Kiln' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '使用 GitHub 登录' })).toHaveAttribute('href', '/auth/github?next=%2Fruns')
    expect(screen.getByText('管理员备用入口：用 admin token 登录')).toBeInTheDocument()
    expect(screen.getByText('登录已过期，请重新登录。登录后会回到刚才的页面。')).toBeInTheDocument()
  })

  it('admin token 错误时显示在字段旁，并标记字段无效', () => {
    renderPage(envelope(tokenFixture), '/login')
    const input = screen.getByLabelText(/admin token/, { selector: 'input' })
    expect(input).toHaveAttribute('type', 'password')
    expect(input).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByText(/admin token 不正确/)).toBeInTheDocument()
  })

  it('既没有 GitHub 登录也关闭了 token 登录时说明如何恢复', () => {
    const env = envelope(tokenFixture, { OAuth: false })
    renderPage(withData(env, { TokenLogin: false, Form: null }), '/login')
    expect(screen.getByText('现在无法登录')).toBeInTheDocument()
    expect(screen.queryByLabelText(/admin token/, { selector: 'input' })).not.toBeInTheDocument()
  })
})

describe('登录页的语言', () => {
  it('未登录时可以切换语言：文案、标题和 <html lang> 立即切换，已填的输入不丢、页面不重新挂载，选择被保存', async () => {
    renderPage(envelope(fixture), '/login?next=%2Fruns')
    expect(document.title).toBe('登录 · Kiln')
    const input = screen.getByLabelText(/admin token/, { selector: 'input' })
    await userEvent.type(input, 'partial-token')
    await userEvent.click(screen.getByRole('button', { name: '切换语言（当前：简体中文）' }))
    await userEvent.click(await screen.findByRole('menuitemradio', { name: 'English' }))

    expect(screen.getByRole('heading', { level: 1, name: 'Sign in to Kiln' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Sign in with GitHub' })).toBeInTheDocument()
    expect(screen.getByText('Your session has expired. Sign in again to return to the page you were on.')).toBeInTheDocument()
    expect(document.title).toBe('Sign in · Kiln')
    expect(document.documentElement.lang).toBe('en')
    expect(window.localStorage.getItem(storageKey)).toBe('en')
    // 同一个输入框（没有重新挂载），输入还在
    expect(screen.getByLabelText(/admin token/, { selector: 'input' })).toBe(input)
    expect(input).toHaveValue('partial-token')

    // 再切回中文
    await userEvent.click(screen.getByRole('button', { name: 'Change language (current: English)' }))
    await userEvent.click(await screen.findByRole('menuitemradio', { name: '简体中文' }))
    expect(screen.getByRole('heading', { level: 1, name: '登录 Kiln' })).toBeInTheDocument()
    expect(document.documentElement.lang).toBe('zh-CN')
  })

  it('表单校验失败的提示按当前语言显示，切换后立即更新', async () => {
    renderPage(envelope(tokenFixture), '/login', { locale: 'en' })
    expect(screen.getByText(/The admin token is incorrect/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Change language (current: English)' }))
    await userEvent.click(await screen.findByRole('menuitemradio', { name: '简体中文' }))
    expect(screen.getByText(/admin token 不正确/)).toBeInTheDocument()
    expect(screen.getByLabelText(/admin token/, { selector: 'input' })).toHaveAttribute('aria-invalid', 'true')
  })

  it('富文本中的命令用 <code> 显示，参数不会变成元素', () => {
    const env = envelope(tokenFixture, { OAuth: false })
    renderPage(withData(env, { TokenLogin: false, Form: null }), '/login', { locale: 'en' })
    expect(screen.getByText('kiln admin token-login on').tagName).toBe('CODE')
    expect(screen.getByText('Sign-in is unavailable')).toBeInTheDocument()
  })
})
