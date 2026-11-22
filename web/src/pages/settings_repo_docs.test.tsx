import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/settings_repo_docs.json'
import empty from '../../dev/fixtures/settings_repo_docs.empty.json'
import failed from '../../dev/fixtures/settings_repo_docs.failed.json'
import { envelope, renderPage, withData } from '../test/render'
import { hidden, stubSubmit } from './settings/testing'

// 设置 · 仓库 · 外部文档：只读展示绑定与验证状态，管理员显式触发验证，页面不含 token。

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('设置 · 仓库 · 外部文档', () => {
  it('绑定：文档集链接、验证结果、更新者和待决策清单', () => {
    renderPage(envelope(fixture), '/settings/repos/docs?repo=acme%2Fkiln')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('acme/kiln · 外部文档')
    const binding = screen.getByRole('region', { name: '文档集绑定' })
    expect(binding).toHaveTextContent('已验证可读取')
    expect(screen.getByText('已验证可读取')).toHaveAttribute('data-variant', 'success')
    expect(binding).toHaveTextContent('Outline')
    expect(binding).toHaveTextContent('Kiln 设计')
    expect(binding).toHaveTextContent('共 7 篇文档')
    expect(binding).toHaveTextContent('0b8f7f04-6a4a-4d0e-9c1b-9a6f0f6a1c21')
    expect(screen.getByRole('link', { name: /在 Outline 中打开/ })).toHaveAttribute(
      'href',
      'https://outline.example.com/collection/3f6f2c1e-6a1a-4c6f-9f43-1f0a2d8b7c11',
    )
    expect(binding).toHaveTextContent('由 octocat')
    expect(binding).toHaveTextContent('第 3 代')

    const decision = screen.getByRole('region', { name: '待决策清单' })
    expect(decision).toHaveTextContent('已配置')
    expect(decision).toHaveTextContent('11111111-1111-4111-8111-111111111111')
    expect(decision).toHaveTextContent('18200 个字符')
    expect(decision).toHaveTextContent('接近推荐篇幅')
    // 只读页面不显示 token，也不声称验证过写权限
    expect(screen.getByText(/这里不显示写 token/)).toBeInTheDocument()
    expect(screen.getByText(/读取验证只检查只读绑定，不测试这篇文档的写权限/)).toBeInTheDocument()
  })

  it('管理员点“验证读取”只提交仓库名，不访问 Outline', async () => {
    const calls = stubSubmit('/settings/repos/docs?repo=acme%2Fkiln&flash=ok')
    renderPage(envelope(fixture), '/settings/repos/docs?repo=acme%2Fkiln')
    const form = screen.getByRole('form', { name: '验证文档读取' })
    expect(form).toHaveAttribute('action', '/settings/repos/docs/verify')
    expect(hidden(form, 'repo')).toBe('acme/kiln')
    await userEvent.click(screen.getByRole('button', { name: '验证读取' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    const posts = calls.filter((c) => c.method === 'POST')
    expect(posts).toHaveLength(1)
    expect(posts[0]?.url).toBe('/settings/repos/docs/verify')
    expect(posts[0]?.body.get('repo')).toBe('acme/kiln')
  })

  it('未绑定：给出空状态和 CLI 配置方式，没有验证按钮', () => {
    renderPage(envelope(empty), '/settings/repos/docs?repo=acme%2Fkiln-site')
    expect(screen.getByText('未绑定')).toBeInTheDocument()
    expect(screen.getByText(/kiln doc bind acme\/kiln-site/)).toBeInTheDocument()
    expect(screen.getByText(/绑定只把配置保存到 Kiln，不访问 Outline/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '验证读取' })).not.toBeInTheDocument()
    expect(screen.getByText('没有配置待决策清单。AI 开发在 PR 中记录的有疑问决定不会写进 Outline。')).toBeInTheDocument()
  })

  it('停用的仓库：仍可查看和验证，失败的原因与代码都显示，不泄露 token', () => {
    renderPage(envelope(failed), '/settings/repos/docs?repo=acme%2Flegacy-api')
    expect(screen.getByText(/已停用的仓库/)).toBeInTheDocument()
    const binding = screen.getByRole('region', { name: '文档集绑定' })
    expect(binding).toHaveTextContent('读取失败')
    expect(screen.getByText('读取失败')).toHaveAttribute('data-variant', 'danger')
    expect(binding).toHaveTextContent('文档系统拒绝了这组凭据')
    expect(binding).toHaveTextContent('auth_failed')
    expect(binding).toHaveTextContent('历史记录没有更新者')
    expect(screen.getByRole('button', { name: '验证读取' })).toBeInTheDocument()
    // 未验证与读取失败不能混为一谈
    expect(screen.queryByText(/连接正常/)).not.toBeInTheDocument()
  })

  it('未验证：明确区分“配置已保存”与“可以读取”', () => {
    const b = {
      ...fixture.Data.Binding,
      CheckStatus: '',
      CheckStartedAt: null,
      CheckedAt: null,
      CheckedBy: '',
      CheckErrorCode: '',
      CheckErrorText: '',
      CheckDocCount: null,
      CheckSampleDocID: '',
    }
    renderPage(withData(envelope(fixture), { Binding: b }), '/settings/repos/docs?repo=acme%2Fkiln')
    const binding = screen.getByRole('region', { name: '文档集绑定' })
    expect(binding).toHaveTextContent('已保存 · 未验证')
    expect(screen.getByText('已保存 · 未验证')).toHaveAttribute('data-variant', 'accent')
    expect(binding).toHaveTextContent('还没有验证。绑定只保存了配置，不会访问 Outline')
    expect(binding).not.toHaveTextContent('可以读取')
  })

  it('空集合：验证通过用成功色，另用中性徽标写明 0 篇', () => {
    const b = {
      ...fixture.Data.Binding,
      CheckStatus: 'empty',
      CheckErrorCode: '',
      CheckErrorText: '',
      CheckDocCount: 0,
      CheckSampleDocID: '',
    }
    renderPage(withData(envelope(fixture), { Binding: b }), '/settings/repos/docs?repo=acme%2Fkiln')
    const binding = screen.getByRole('region', { name: '文档集绑定' })
    expect(screen.getByText('已验证 · 空集合')).toHaveAttribute('data-variant', 'success')
    expect(screen.getByText('0 篇')).toHaveAttribute('data-variant', 'secondary')
    expect(binding).toHaveTextContent('当前没有文档（冻结时会记录一个空集合，不是读取失败）')
  })
})
