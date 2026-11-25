import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import fixture from '../../dev/fixtures/settings_repos.json'
import empty from '../../dev/fixtures/settings_repos.empty.json'
import pick from '../../dev/fixtures/settings_repos.pick.json'
import { envelope, renderPage, tFor, withData } from '../test/render'
import { hidden } from './settings/testing'

describe('设置 · 仓库', () => {
  it('仓库的可见性按语言显示', () => {
    const { unmount } = renderPage(envelope(fixture), '/settings/repos')
    const row = (name: string): HTMLElement => {
      const el = screen.getByText(name).parentElement
      if (!el) throw new Error(`找不到 ${name} 所在的行`)
      return el
    }
    expect(within(row('acme/kilnflow.dev')).getByText('私有')).toBeInTheDocument()
    expect(within(row('acme/kiln')).getByText('公开')).toBeInTheDocument()
    unmount()
    renderPage(envelope(fixture), '/settings/repos', { locale: 'en' })
    expect(within(row('acme/kilnflow.dev')).getByText('Private')).toBeInTheDocument()
    expect(within(row('acme/kiln')).getByText('Public')).toBeInTheDocument()
  })

  it('统计、最近一次刷新、仓库操作和分页', () => {
    renderPage(envelope(fixture), '/settings/repos')
    expect(screen.getByText('共 112 个仓库，第 1 / 3 页')).toBeInTheDocument()
    expect(screen.getByText(/2 个 installation、11 个启用中的仓库/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '刷新仓库' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'acme/kiln 的变量与密钥' })).toHaveAttribute('href', '/settings/scope?repo=acme%2Fkiln')
    expect(screen.getByRole('link', { name: 'acme/kiln 的运行记录' })).toHaveAttribute('href', '/runs?repo=acme%2Fkiln')
    const reconcile = screen.getByRole('button', { name: '对 acme/kiln 补对账' }).closest('form')
    expect(reconcile).toHaveAttribute('action', '/settings/repos/reconcile')
    expect(reconcile && hidden(reconcile, 'repo')).toBe('acme/kiln')
    expect(screen.getByText('已停用')).toBeInTheDocument()
    expect(screen.getByText('已停用的仓库不能配置')).toBeInTheDocument()
    // 停用的仓库也有外部文档入口（只读查看历史配置）
    expect(screen.getByRole('link', { name: 'acme/legacy-api 的外部文档' })).toHaveAttribute(
      'href',
      '/settings/repos/docs?repo=acme%2Flegacy-api',
    )
    // 外部文档的状态：保存配置不等于读取正常
    expect(screen.getByRole('link', { name: 'acme/kilnflow.dev 的外部文档' })).toHaveAttribute(
      'href',
      '/settings/repos/docs?repo=acme%2Fkilnflow.dev',
    )
    // 状态徽标按 DESIGN.md 取色：已验证 success、失败 danger、已保存未验证 accent。
    expect(screen.getByText('已验证可读取')).toHaveAttribute('data-variant', 'success')
    expect(screen.getByText('读取失败')).toHaveAttribute('data-variant', 'danger')
    expect(screen.getByText('已保存 · 未验证')).toHaveAttribute('data-variant', 'accent')
    expect(screen.getByText('待决策清单')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '对 acme/legacy-api 补对账' })).not.toBeInTheDocument()
    const pager = screen.getByRole('navigation', { name: '分页' })
    expect(within(pager).getByRole('link', { name: '下一页' })).toHaveAttribute('href', '/settings/repos?page=2')
    expect(within(pager).queryByRole('link', { name: '上一页' })).not.toBeInTheDocument()
  })

  it('空集合用成功色表示连接正常，旁边用中性徽标写明 0 篇', () => {
    const repos = fixture.Data.Repos.map((r) =>
      r.FullName === 'acme/kiln-site' ? { ...r, DocBound: true, DocCheckStatus: 'empty' } : r,
    )
    renderPage(withData(envelope(fixture), { Repos: repos }), '/settings/repos')
    expect(screen.getByText('已验证 · 空集合')).toHaveAttribute('data-variant', 'success')
    expect(screen.getByText('0 篇')).toHaveAttribute('data-variant', 'secondary')
  })

  it('还没有手动刷新过', () => {
    renderPage(withData(envelope(fixture), { Sync: null }), '/settings/repos')
    expect(screen.getByText(/还没有手动刷新过/)).toBeInTheDocument()
  })

  it('刷新失败时显示原因；没有仓库时说明仓库何时出现', () => {
    renderPage(envelope(empty), '/settings/repos')
    expect(screen.getByText('失败：GitHub 返回 401：App 的私钥不对')).toBeInTheDocument()
    expect(screen.getByText(/还没有仓库。仓库在第一次收到它的事件/)).toBeInTheDocument()
  })

  it('选择器模式：没有刷新按钮，只能选启用中的仓库，搜索保留 pick', () => {
    renderPage(envelope(pick), '/settings/repos?pick=env&q=acme')
    expect(screen.queryByRole('button', { name: '刷新仓库' })).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('选择仓库')
    expect(screen.getByText('与“acme”匹配的 4 个仓库')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '选择 acme/kiln' })).toHaveAttribute('href', '/settings/environments/new?repo=acme%2Fkiln')
    expect(screen.queryByRole('link', { name: '选择 acme/legacy-api' })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /的变量与密钥$/ })).not.toBeInTheDocument()
    const search = screen.getByRole('form', { name: '搜索仓库' })
    expect(hidden(search, 'pick')).toBe('env')
    expect(screen.getByRole('link', { name: '清除' })).toHaveAttribute('href', '/settings/repos?pick=env')
  })
  it('补对账被限流：时间按语言格式化为本地时间，不显示 Unix 秒', () => {
    const at = 1_795_000_000
    const flash = { id: 'settingsRepos.reconcileRateLimited', params: { repo: 'acme/kiln', at } }
    for (const locale of ['zh-CN', 'en'] as const) {
      const { unmount } = renderPage(envelope(fixture, { Flash: flash }), '/settings/repos', { locale })
      const text = tFor(locale)('settingsRepos.reconcileRateLimited', { repo: 'acme/kiln', at })
      expect(screen.getByText(text)).toBeInTheDocument()
      expect(text).not.toContain(String(at))
      expect(text).toContain('acme/kiln')
      unmount()
    }
  })

  it('英文界面：统计、复数和操作的无障碍名称', () => {
    const en = tFor('en')
    renderPage(envelope(fixture), '/settings/repos', { locale: 'en' })
    expect(screen.getByText(/^112 repositories/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: en('repos.varsLabel', { repo: 'acme/kiln' }) })).toHaveAttribute('href', '/settings/scope?repo=acme%2Fkiln')
    expect(screen.getByText(en('settingsRepos.synced', { installations: 2, repos: 11 }))).toBeInTheDocument()
  })
})
