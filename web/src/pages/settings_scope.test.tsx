import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/settings_scope.json'
import account from '../../dev/fixtures/settings_scope.account.json'
import docker from '../../dev/fixtures/settings_scope.docker.json'
import empty from '../../dev/fixtures/settings_scope.empty.json'
import repo from '../../dev/fixtures/settings_scope.repo.json'
import { envelope, renderPage } from '../test/render'
import { hidden, stubSubmit } from './settings/testing'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('作用域配置页', () => {
  it('environment：保护规则、覆盖关系、变量和 secret（secret 不显示值），以及删除 environment', () => {
    const { container } = renderPage(envelope(fixture), '/settings/scope?account=acme&env=release')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('环境/acme/release')
    expect(screen.getByText('账号级 environment（账号下的所有仓库共用）')).toBeInTheDocument()
    expect(screen.getByText('被仓库级 environment 整体覆盖')).toBeInTheDocument()
    expect(container.textContent).toMatch(/会整体覆盖这里的配置：acme\/legacy-api/)
    const rules = screen.getByRole('region', { name: '保护规则' })
    expect(within(rules).getByText('acme/kiln')).toBeInTheDocument()
    expect(screen.getByText('编辑保护规则').closest('details')).not.toHaveAttribute('open')

    expect(screen.getByRole('link', { name: '编辑变量 NPM_REGISTRY' })).toHaveAttribute(
      'href',
      '/settings/variables/edit?account=acme&env=release&name=NPM_REGISTRY',
    )
    expect(screen.getByRole('link', { name: '新建 secret' })).toHaveAttribute('href', '/settings/secrets/new?account=acme&env=release')
    expect(screen.getByRole('link', { name: '替换 secret NPM_TOKEN' })).toBeInTheDocument()
    expect(container.querySelectorAll('input[type="password"]')).toHaveLength(0)

    expect(screen.getByRole('heading', { name: '删除 environment release' })).toBeInTheDocument()
    expect(screen.getByText(/同时删除其中的 2 个变量和 2 个 secret/)).toBeInTheDocument()
  })

  it('保护规则保存失败时展开表单，字段标记无效，回传作用域', () => {
    renderPage(envelope(docker), '/settings/scope?account=acme&env=docker-release')
    expect(screen.getByText(/这是 Docker Hub 连接使用的 environment/)).toBeInTheDocument()
    expect(screen.getByText('编辑保护规则').closest('details')).toHaveAttribute('open')
    const form = screen.getByRole('form', { name: '编辑保护规则' })
    expect(within(form).getByText('没有保存。')).toBeInTheDocument()
    expect(within(form).getByRole('textbox', { name: '允许发布到的仓库' })).toHaveAttribute('aria-invalid', 'true')
    expect(hidden(form, 'account')).toBe('acme')
    expect(hidden(form, 'env')).toBe('docker-release')
    expect(hidden(form, 'name')).toBe('docker-release')
  })

  it('仓库级：没有保护规则和删除 environment，列表为空时有说明', () => {
    renderPage(envelope(empty), '/settings/scope?repo=acme%2Fkiln-site')
    expect(screen.queryByRole('region', { name: '保护规则' })).not.toBeInTheDocument()
    expect(screen.queryByText(/删除 environment/)).not.toBeInTheDocument()
    expect(screen.getByText('没有变量')).toBeInTheDocument()
    expect(screen.getByText('没有 secret')).toBeInTheDocument()
  })

  it('删除变量要先确认，确认后提交作用域和名称', async () => {
    const calls = stubSubmit('/settings/scope?repo=acme%2Fkilnflow.dev')
    renderPage(envelope(repo), '/settings/scope?repo=acme%2Fkilnflow.dev')
    await userEvent.click(screen.getByRole('button', { name: '删除变量 GO_VERSION' }))
    const dialog = await screen.findByRole('alertdialog')
    await userEvent.click(within(dialog).getByRole('button', { name: '删除变量' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    const post = calls.find((c) => c.method === 'POST')
    expect(post?.url).toBe('/settings/variables/delete')
    expect(post?.body.get('repo')).toBe('acme/kilnflow.dev')
    expect(post?.body.get('name')).toBe('GO_VERSION')
  })

  it('账号级也不显示 npm 配置（npm 在“连接与发布凭据”中，与 Docker Hub 平级）', () => {
    renderPage(envelope(account), '/settings/scope?account=acme')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('acme')
    expect(screen.queryByRole('region', { name: 'npm' })).not.toBeInTheDocument()
    expect(screen.queryByRole('form', { name: 'npm 配置' })).not.toBeInTheDocument()
    expect(screen.queryByText(/kiln npm set/)).not.toBeInTheDocument()
  })
})
