import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import blank from '../../dev/fixtures/settings_env_new.blank.json'
import failed from '../../dev/fixtures/settings_env_new.json'
import { envelope, renderPage } from '../test/render'
import { hidden } from './settings/testing'

describe('新建 environment', () => {
  it('作用域可选账号级和选中的仓库，失败时回填并标记无效', () => {
    renderPage(envelope(failed), '/settings/environments/new?repo=acme%2Fkilnflow.dev')
    const form = screen.getByRole('form', { name: '新建 environment' })
    expect(form).toHaveAttribute('action', '/settings/environments')
    expect(hidden(form, 'create')).toBe('1')
    const scope = screen.getByRole('combobox', { name: '作用域' })
    expect(scope).toHaveValue('repo:acme/kilnflow.dev')
    expect(screen.getByRole('option', { name: 'acme（账号级，账号下所有仓库共用）' })).toHaveValue('account:acme')
    expect(screen.getByRole('option', { name: 'acme/kilnflow.dev（仓库级）' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '换一个仓库' })).toHaveAttribute('href', '/settings/repos?pick=env')
    expect(screen.getByText('没有保存。')).toBeInTheDocument()
    const name = screen.getByRole('textbox', { name: /名称/ })
    expect(name).toHaveAttribute('aria-invalid', 'true')
    expect(name).toHaveValue('release')
    expect(screen.getByRole('textbox', { name: '允许的 ref' })).toHaveValue('refs/tags/v*')
  })

  it('没有选中仓库时只能选账号级', () => {
    renderPage(envelope(blank), '/settings/environments/new')
    expect(screen.getAllByRole('option')).toHaveLength(2)
    expect(screen.getByRole('link', { name: '搜索并选择仓库' })).toBeInTheDocument()
    expect(screen.queryByText('没有保存。')).not.toBeInTheDocument()
  })
})
