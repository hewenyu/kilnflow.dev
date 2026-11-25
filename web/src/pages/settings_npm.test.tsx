import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/settings_npm.json'
import failed from '../../dev/fixtures/settings_npm.new.json'
import { envelope, renderPage, switchLocale, tFor, withData } from '../test/render'
import { hidden, stubSubmit } from './settings/testing'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('设置 · npm', () => {
  it('已配置：在连接与发布凭据分区，显示 scope、registry 和 token 的更新记录，不显示 token，表单预填 scope 和 registry', () => {
    const { container } = renderPage(envelope(fixture), '/settings/npm?account=acme')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('连接与发布凭据/npm · acme')
    expect(within(screen.getByRole('heading', { level: 1 })).getByRole('link', { name: '连接与发布凭据' })).toHaveAttribute('href', '/settings')
    expect(screen.getByText('已配置')).toBeInTheDocument()
    const summary = container.querySelector('dl')
    if (!(summary instanceof HTMLElement)) throw new Error('应有 npm 配置摘要')
    expect(within(summary).getByText('@acme')).toBeInTheDocument()
    expect(within(summary).getByText('https://registry.npmjs.org/')).toBeInTheDocument()
    expect(within(summary).getByText('已设置')).toBeInTheDocument()
    expect(within(summary).getByText(/由 octocat 更新/)).toBeInTheDocument()
    const form = screen.getByRole('form', { name: 'npm 配置' })
    expect(form).toHaveAttribute('action', '/settings/npm')
    expect(hidden(form, 'account')).toBe('acme')
    expect(within(form).getByRole('textbox', { name: /scope/ })).toHaveValue('@acme')
    expect(within(form).getByRole('textbox', { name: /registry/ })).toHaveValue('https://registry.npmjs.org/')
    const token = container.querySelector('input[name="token"]')
    expect(token).toHaveAttribute('type', 'password')
    expect(token).toHaveValue('')
    expect(token).toBeRequired()
    expect(within(form).getByText(/每次保存都整体替换/)).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '删除 npm 配置' })).toBeInTheDocument()
  })

  it('没有配置且保存失败：说明步骤和 CLI 命令，字段标记无效，token 不回填，没有删除', () => {
    const { container } = renderPage(envelope(failed), '/settings/npm?account=acme-sandbox')
    expect(screen.getByText('未配置')).toBeInTheDocument()
    expect(screen.getByText(/创建一个对这个 scope 有读写权限/)).toBeInTheDocument()
    expect(screen.getByText('kiln npm set --account acme-sandbox --scope @<scope>')).toBeInTheDocument()
    const form = screen.getByRole('form', { name: 'npm 配置' })
    expect(within(form).getByText('没有保存。')).toBeInTheDocument()
    expect(within(form).getByRole('textbox', { name: /scope/ })).toHaveAttribute('aria-invalid', 'true')
    expect(within(form).getByText('scope "@Acme" 无效：不能有大写字母')).toBeInTheDocument()
    expect(container.querySelector('input[name="token"]')).toHaveValue('')
    expect(within(form).getByText(/刚才输入的 token 没有保留/)).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '删除 npm 配置' })).not.toBeInTheDocument()
  })

  it('保存时提交账号、scope、registry 和 token', async () => {
    const calls = stubSubmit('/settings/npm?account=acme')
    const { container } = renderPage(envelope(fixture), '/settings/npm?account=acme')
    const token = container.querySelector('input[name="token"]')
    if (!(token instanceof HTMLInputElement)) throw new Error('应有 token 输入框')
    await userEvent.type(token, 'npm_secret')
    await userEvent.click(screen.getByRole('button', { name: '保存' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    const post = calls.find((c) => c.method === 'POST')
    expect(post?.url).toBe('/settings/npm')
    expect(post?.body.get('account')).toBe('acme')
    expect(post?.body.get('scope')).toBe('@acme')
    expect(post?.body.get('registry')).toBe('https://registry.npmjs.org/')
    expect(post?.body.get('token')).toBe('npm_secret')
  })

  it('删除要先确认，确认后提交账号和 confirm=1', async () => {
    const calls = stubSubmit('/settings')
    renderPage(envelope(fixture), '/settings/npm?account=acme')
    await userEvent.click(screen.getByRole('button', { name: '删除 npm 配置' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(calls).toHaveLength(0)
    expect(within(dialog).getByText(/无法发布 release.npm/)).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('button', { name: '删除 npm 配置' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    const post = calls.find((c) => c.method === 'POST')
    expect(post?.url).toBe('/settings/npm/delete')
    expect(post?.body.get('account')).toBe('acme')
    expect(post?.body.get('confirm')).toBe('1')
  })
  it('英文界面：registry 带用户信息时说明原因，不回显；中途切换语言保留 scope，token 为空', async () => {
    const en = tFor('en')
    const env = withData(envelope(failed), {
      Form: {
        Values: { scope: '@acme', registry: '', token: 'npm_t0ken_leak' },
        Errors: { registry: { id: 'settingsErr.urlUserInfo' } },
        Failed: true,
      },
    })
    const { container } = renderPage(env, '/settings/npm?account=acme-sandbox', { locale: 'en' })
    expect(container.innerHTML).not.toContain('npm_t0ken_leak')
    expect(container.querySelector('input[name="token"]')).toHaveValue('')
    expect(container.querySelector('input[name="registry"]')).toHaveValue('')
    expect(screen.getByText(en('settingsErr.urlUserInfo'))).toBeInTheDocument()
    const scope = container.querySelector('input[name="scope"]')
    if (!(scope instanceof HTMLInputElement)) throw new Error('应有 scope 字段')
    await userEvent.clear(scope)
    await userEvent.type(scope, '@kiln')
    await switchLocale('简体中文')
    expect(container.querySelector('input[name="scope"]')).toHaveValue('@kiln')
    expect(container.querySelector('input[name="token"]')).toHaveValue('')
    await switchLocale('English')
    expect(screen.getByText(en('settingsForm.notSaved'))).toBeInTheDocument()
  })
})
