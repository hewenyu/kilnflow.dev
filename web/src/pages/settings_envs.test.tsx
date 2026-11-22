import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import fixture from '../../dev/fixtures/settings_envs.json'
import empty from '../../dev/fixtures/settings_envs.empty.json'
import { envelope, renderPage } from '../test/render'

describe('设置 · 环境', () => {
  it('列出 environment 的作用域、保护规则和覆盖关系', () => {
    const { container } = renderPage(envelope(fixture), '/settings/environments')
    expect(screen.getByRole('link', { name: 'release' })).toHaveAttribute('href', '/settings/scope?account=acme&env=release')
    expect(screen.getAllByText('账号级')).toHaveLength(2)
    expect(screen.getAllByText('仓库级')).toHaveLength(2)
    expect(screen.getByText('Docker Hub')).toBeInTheDocument()
    expect(container.textContent).toContain('可发布到 acme/kiln')
    expect(screen.getAllByText(/在 acme\/legacy-api 中被同名仓库级 environment 整体覆盖/)).toHaveLength(2)
    expect(screen.getByText('整体覆盖账号级同名 environment（不继承其变量和 secret）')).toBeInTheDocument()
    // 没有保护规则的 environment
    expect(container.textContent).toContain('acme-lab/console · 不限制 ref')
    expect(screen.getByRole('link', { name: '新建 environment' })).toHaveAttribute('href', '/settings/environments/new')
  })

  it('没有 environment 时引导新建', () => {
    renderPage(envelope(empty), '/settings/environments')
    expect(screen.getByText('没有 environment')).toBeInTheDocument()
    expect(screen.getAllByRole('link', { name: '新建 environment' })).toHaveLength(2)
  })
})
