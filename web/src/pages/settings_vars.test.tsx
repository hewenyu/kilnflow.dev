import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import fixture from '../../dev/fixtures/settings_vars.json'
import { envelope, renderPage, withData } from '../test/render'

describe('设置 · 变量与密钥', () => {
  it('按账号级、仓库级、environment 级列出作用域', () => {
    renderPage(envelope(fixture), '/settings/variables')
    const acc = screen.getByRole('region', { name: '账号级' })
    expect(within(acc).getByRole('link', { name: '管理 acme 的账号级变量与 secret' })).toHaveAttribute('href', '/settings/scope?account=acme')
    expect(within(acc).getByText('3 个变量 · 2 个 secret')).toBeInTheDocument()

    const repo = screen.getByRole('region', { name: '仓库级' })
    const search = within(repo).getByRole('form', { name: '查找仓库' })
    expect(search).toHaveAttribute('action', '/settings/repos')
    expect(search).toHaveAttribute('method', 'get')
    expect(within(repo).getByRole('link', { name: 'acme/kilnflow.dev' })).toHaveAttribute('href', '/settings/scope?repo=acme%2Fkilnflow.dev')

    const envs = screen.getByRole('region', { name: 'environment 级' })
    expect(within(envs).getByRole('link', { name: '新建 environment' })).toHaveAttribute('href', '/settings/environments/new')
    expect(within(envs).getByText(/Docker Hub 连接，建议在/)).toBeInTheDocument()
  })

  it('空列表有说明', () => {
    renderPage(withData(envelope(fixture), { Accounts: null, Repos: null, Envs: null }), '/settings/variables')
    expect(screen.getByText('没有已安装的账号')).toBeInTheDocument()
    expect(screen.getByText('还没有仓库级变量或 secret')).toBeInTheDocument()
    expect(screen.getByText('没有 environment')).toBeInTheDocument()
  })
})
