import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import fixture from '../../dev/fixtures/login.json'
import tokenFixture from '../../dev/fixtures/login.token.json'
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
    expect(screen.getByText(/admin token 不对/)).toBeInTheDocument()
  })

  it('既没有 GitHub 登录也关闭了 token 登录时说明如何恢复', () => {
    const env = envelope(tokenFixture, { OAuth: false })
    renderPage(withData(env, { TokenLogin: false, Form: null }), '/login')
    expect(screen.getByText('现在无法登录')).toBeInTheDocument()
    expect(screen.queryByLabelText(/admin token/, { selector: 'input' })).not.toBeInTheDocument()
  })
})
