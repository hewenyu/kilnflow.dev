import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it } from 'vitest'
import { vi } from 'vitest'
import fixture from '../../dev/fixtures/settings_docker.json'
import fresh from '../../dev/fixtures/settings_docker.new.json'
import failed from '../../dev/fixtures/settings_docker.saved.json'
import { envelope, renderPage, withData } from '../test/render'
import { hidden, stubSubmit } from './settings/testing'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('设置 · Docker Hub 连接', () => {
  it('已验证的连接：预填用户名和命名空间，令牌只显示状态，替换要展开', () => {
    const { container } = renderPage(envelope(fixture), '/settings/docker?account=acme')
    expect(screen.getByText('已验证')).toBeInTheDocument()
    const form = screen.getByRole('form', { name: 'Docker Hub 连接' })
    expect(form).toHaveAttribute('action', '/settings/docker')
    expect(hidden(form, 'account')).toBe('acme')
    expect(within(form).getByRole('textbox', { name: /Docker Hub 用户名/ })).toHaveValue('octocat')
    expect(within(form).getByRole('textbox', { name: /镜像命名空间/ })).toHaveValue('acme')
    expect(screen.getByText('替换访问令牌').closest('details')).not.toHaveAttribute('open')
    expect(container.querySelector('input[name="token"]')).toHaveAttribute('type', 'password')
    // 已有 environment 时不再提供 ref 策略字段
    expect(container.querySelector('[name="ref_policy"]')).toBeNull()
  })

  it('校验失败：显示"没有保存。"，字段标记无效，令牌不回填并展开替换', () => {
    const env = envelope(failed)
    const data = withData(env, {
      Form: { Values: { username: 'octocat', namespace: 'KilnBuild', token: 'dckr_pat_leak' }, Errors: { namespace: '命名空间只能包含小写字母', token: '请重新输入' }, Failed: true },
    })
    const { container } = renderPage(data, '/settings/docker?account=acme')
    expect(screen.getByText('已保存 · 未验证')).toBeInTheDocument()
    expect(screen.getByText('没有保存。')).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: /镜像命名空间/ })).toHaveAttribute('aria-invalid', 'true')
    const token = container.querySelector('input[name="token"]')
    expect(token).toHaveAttribute('aria-invalid', 'true')
    expect(token).toHaveValue('')
    expect(container.innerHTML).not.toContain('dckr_pat_leak')
    expect(screen.getByText('替换访问令牌').closest('details')).toHaveAttribute('open')
  })

  it('首次配置：令牌必填，可以填 ref 策略，没有删除操作', () => {
    const { container } = renderPage(envelope(fresh), '/settings/docker?account=acme-sandbox')
    expect(screen.getByText('未配置')).toBeInTheDocument()
    expect(container.querySelector('input[name="token"]')).toBeRequired()
    expect(container.querySelector('input[name="ref_policy"]')).toHaveValue('refs/tags/v*')
    expect(screen.queryByRole('button', { name: '删除凭据' })).not.toBeInTheDocument()
  })

  it('删除凭据要先确认，确认后提交账号和 confirm=1', async () => {
    const calls = stubSubmit('/settings')
    renderPage(envelope(fixture), '/settings/docker?account=acme')
    await userEvent.click(screen.getByRole('button', { name: '删除凭据' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(calls).toHaveLength(0)
    await userEvent.click(within(dialog).getByRole('button', { name: '删除凭据' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    const post = calls.find((c) => c.method === 'POST')
    expect(post?.url).toBe('/settings/docker/delete')
    expect(post?.body.get('account')).toBe('acme')
    expect(post?.body.get('confirm')).toBe('1')
    expect(post?.body.get('csrf')).toBe('csrf')
  })
})
