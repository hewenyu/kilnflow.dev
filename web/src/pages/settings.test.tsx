import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import fixture from '../../dev/fixtures/settings.json'
import empty from '../../dev/fixtures/settings.empty.json'
import { envelope, renderPage } from '../test/render'

describe('设置 · 连接与发布凭据', () => {
  it('每个账号一张 Docker Hub 卡片，带状态标签和对应的操作', () => {
    renderPage(envelope(fixture), '/settings')
    expect(screen.getByRole('heading', { level: 1, name: '连接与发布凭据' })).toBeInTheDocument()
    const verified = screen.getByRole('region', { name: /^Docker Hub · acme$/ })
    expect(within(verified).getByText('已验证')).toBeInTheDocument()
    expect(within(verified).getByRole('link', { name: '编辑连接' })).toHaveAttribute('href', '/settings/docker?account=acme')
    expect(within(verified).getByText(/定义了自己的 environment/)).toHaveTextContent('acme/legacy-api')

    const incomplete = screen.getByRole('region', { name: /^Docker Hub · acme-lab$/ })
    expect(within(incomplete).getByText('不完整')).toBeInTheDocument()
    expect(within(incomplete).getByText('还缺少：镜像命名空间、访问令牌。')).toBeInTheDocument()

    const unconf = screen.getByRole('region', { name: /^Docker Hub · acme-sandbox$/ })
    expect(within(unconf).getByText('未配置')).toBeInTheDocument()
    expect(within(unconf).getByRole('link', { name: '配置 Docker Hub' })).toBeInTheDocument()
    expect(within(unconf).getByText('首次保存时创建：只允许 v* tag')).toBeInTheDocument()
  })

  it('npm 与 Docker Hub 平级：每个账号一张 npm 卡片，显示 scope 和 registry，不显示 token', () => {
    renderPage(envelope(fixture), '/settings')
    const docker = screen.getByRole('region', { name: 'Docker Hub' })
    const npm = screen.getByRole('region', { name: 'npm' })
    expect(within(docker).getAllByRole('region', { name: /^Docker Hub · / })).toHaveLength(3)
    expect(within(npm).getAllByRole('region', { name: /^npm · / })).toHaveLength(3)
    expect(within(npm).queryByRole('region', { name: /^Docker Hub/ })).not.toBeInTheDocument()

    const configured = within(npm).getByRole('region', { name: 'npm · acme' })
    expect(within(configured).getByText('已配置')).toBeInTheDocument()
    expect(within(configured).getByText('@acme')).toBeInTheDocument()
    expect(within(configured).getByText('https://registry.npmjs.org/')).toBeInTheDocument()
    expect(within(configured).getByText('已设置')).toBeInTheDocument()
    expect(within(configured).getByRole('link', { name: '编辑配置' })).toHaveAttribute('href', '/settings/npm?account=acme')

    const unconf = within(npm).getByRole('region', { name: 'npm · acme-lab' })
    expect(within(unconf).getByText('未配置')).toBeInTheDocument()
    expect(within(unconf).getByRole('link', { name: '配置 npm' })).toHaveAttribute('href', '/settings/npm?account=acme-lab')
  })

  it('没有账号时说明怎么让账号出现', () => {
    renderPage(envelope(empty), '/settings')
    expect(screen.getByText('还没有已安装 App 的账号')).toBeInTheDocument()
    const hint = screen.getByText(/确认 App 已安装/)
    expect(within(hint).getByRole('link', { name: '仓库' })).toHaveAttribute('href', '/settings/repos')
  })
})
