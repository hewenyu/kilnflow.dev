import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import created from '../../dev/fixtures/settings_item.json'
import edit from '../../dev/fixtures/settings_item.edit.json'
import secret from '../../dev/fixtures/settings_item.secret.json'
import { envelope, renderPage, withData } from '../test/render'
import { hidden } from './settings/testing'

describe('变量和 secret 的新建、编辑页', () => {
  it('新建变量失败：错误摘要，名称标记无效并回填，回传作用域和 mode=new', () => {
    renderPage(envelope(created), '/settings/variables/new?repo=acme%2Fkilnflow.dev')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('新建变量')
    const form = screen.getByRole('form', { name: '新建变量' })
    expect(form).toHaveAttribute('action', '/settings/variables')
    expect(hidden(form, 'repo')).toBe('acme/kilnflow.dev')
    expect(hidden(form, 'account')).toBe('')
    expect(hidden(form, 'mode')).toBe('new')
    expect(screen.getByText('没有保存。')).toBeInTheDocument()
    const name = screen.getByRole('textbox', { name: /名称/ })
    expect(name).toHaveAttribute('aria-invalid', 'true')
    expect(name).toHaveValue('DEPLOY HOST')
    expect(screen.getByRole('textbox', { name: '值' })).toHaveValue('kiln.example.com')
    expect(screen.queryByText(/危险操作/)).not.toBeInTheDocument()
  })

  it('编辑变量：名称只读，预填当前值，可以删除', () => {
    renderPage(envelope(edit), '/settings/variables/edit?repo=acme%2Fkilnflow.dev&name=GO_VERSION')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('编辑变量 GO_VERSION')
    expect(screen.getByRole('textbox', { name: '名称' })).toHaveAttribute('readonly')
    expect(screen.getByRole('textbox', { name: '值' })).toHaveValue('1.27.3')
    expect(hidden(screen.getByRole('form', { name: '编辑变量' }), 'mode')).toBe('edit')
    expect(screen.getByRole('button', { name: '保存修改' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '删除变量 GO_VERSION' })).toBeInTheDocument()
  })

  it('替换 secret：标题和提示，值不回显，留空则保持不变', () => {
    const env = withData(envelope(secret), { Form: { Values: { name: 'NPM_TOKEN', value: 'npm_leak' }, Errors: {}, Failed: false } })
    const { container } = renderPage(env, '/settings/secrets/edit?account=acme&env=release&name=NPM_TOKEN')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('替换 secret NPM_TOKEN')
    expect(screen.getByText(/当前值已设置/)).toHaveTextContent('由 octocat 更新')
    expect(screen.getByText('保存后替换当前值；留空则保持不变。')).toBeInTheDocument()
    const value = container.querySelector('input[name="value"]')
    expect(value).toHaveAttribute('type', 'password')
    expect(value).toHaveValue('')
    expect(value).not.toBeRequired()
    expect(container.innerHTML).not.toContain('npm_leak')
    expect(screen.getByRole('button', { name: '替换' })).toBeInTheDocument()
  })

  it('新建 secret：值必填', () => {
    const env = withData(envelope(secret), { Name: '', Secret: null })
    const { container } = renderPage(env, '/settings/secrets/new?account=acme&env=release')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('新建 secret')
    expect(container.querySelector('input[name="value"]')).toBeRequired()
    expect(screen.getByText('保存后不会再显示。')).toBeInTheDocument()
  })
})
