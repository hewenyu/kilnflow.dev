import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it } from 'vitest'
import { vi } from 'vitest'
import fixture from '../../dev/fixtures/runs.json'
import emptyFixture from '../../dev/fixtures/runs.empty.json'
import filteredFixture from '../../dev/fixtures/runs.filtered.json'
import { envelope, renderPage, switchLocale, withData } from '../test/render'
import { json, stubFetch } from '../test/fetch'
import { filterSummary } from './runs'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('运行列表', () => {
  it('每个 run 一行：标题链接到详情页，显示仓库、事件、PR 编号和 ref', () => {
    renderPage(envelope(fixture), '/runs')
    const list = screen.getByRole('list', { name: '构建' })
    const rows = within(list).getAllByRole('listitem')
    expect(rows).toHaveLength(10)
    expect(within(list).getByRole('link', { name: /CI #129/ })).toHaveAttribute('href', '/runs/129')
    const pr = rows[2]
    expect(pr).toBeDefined()
    if (!pr) return
    expect(within(pr).getByText('#42')).toBeInTheDocument()
    // PR 的 ref 显示来源分支
    expect(within(pr).getByTitle(/^PR #42 的来源分支/)).toHaveTextContent('feat/notes-sync')
    // 配置了 GitHub 网页地址时，仓库、commit 和分支链接到 GitHub
    expect(within(pr).getByRole('link', { name: 'acme/notes-api' })).toHaveAttribute('href', 'https://github.com/acme/notes-api')
    const ci = rows[0]
    if (!ci) return
    const sha = within(ci).getByTitle(/^commit /)
    expect(within(sha).getByRole('link')).toHaveAttribute('href', expect.stringMatching(/^https:\/\/github\.com\/acme\/[^/]+\/commit\/[0-9a-f]{40}$/))
    expect(within(rows[6] ?? pr).getByText('配置错误')).toBeInTheDocument()
    expect(screen.getByText(/最新的 10 个构建，每 5 秒自动更新。/)).toBeInTheDocument()
  })

  it('筛选表单回填条件；"全部 / 我发起的"标出当前的一个', () => {
    renderPage(envelope(filteredFixture), '/runs?repo=acme%2Fkiln&state=failed')
    expect(screen.getByLabelText('仓库')).toHaveValue('acme/kiln')
    expect(screen.getByLabelText('状态')).toHaveValue('failed')
    expect(screen.getByRole('link', { name: '全部' })).toHaveAttribute('aria-current', 'page')
    expect(screen.getByRole('link', { name: '我发起的' })).toHaveAttribute('href', '/runs?actor=octocat&repo=acme%2Fkiln&state=failed')
    expect(screen.getByText(/已筛选：仓库 acme\/kiln，状态 失败。/)).toBeInTheDocument()
  })

  it('筛选没有结果时说明会查找全部历史，可以清空筛选或为该仓库启动构建', () => {
    renderPage(envelope(filteredFixture), '/runs?repo=acme%2Fkiln&state=failed')
    expect(screen.getByRole('heading', { name: '没有符合筛选条件的构建' })).toBeInTheDocument()
    expect(screen.getByText(/筛选会查找全部历史/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '为 acme/kiln 启动构建' })).toHaveAttribute('href', '/runs/new?from=filter&repo=acme%2Fkiln')
    expect(screen.getAllByRole('link', { name: '清空筛选' })[0]).toHaveAttribute('href', '/runs')
  })

  it('还没有构建时说明怎样开始', () => {
    renderPage(envelope(emptyFixture), '/runs')
    expect(screen.getByRole('heading', { name: '还没有构建' })).toBeInTheDocument()
    // 页头已经有"启动构建"，空状态中不再重复
    expect(screen.getAllByRole('link', { name: '启动构建' })).toHaveLength(1)
    expect(screen.queryByRole('link', { name: '清空筛选' })).not.toBeInTheDocument()
    expect(screen.queryByRole('navigation', { name: '分页' })).not.toBeInTheDocument()
  })

  it('分页：较早一页的链接；翻页后有较新、最新，空页可以回到最新', () => {
    renderPage(envelope(fixture), '/runs')
    expect(screen.getByRole('link', { name: '较早' })).toHaveAttribute('href', '/runs?before=122')
    expect(screen.queryByRole('link', { name: '较新' })).not.toBeInTheDocument()
  })

  it('翻到没有更多的一页时可以回到最新', () => {
    renderPage(withData(envelope(emptyFixture), { Paged: true, NewerURL: '/runs?after=100', LatestURL: '/runs' }), '/runs?before=100')
    expect(screen.getByText('这个方向上没有更多构建了')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '较新' })).toHaveAttribute('href', '/runs?after=100')
    expect(screen.getAllByRole('link', { name: /最新/ }).map((a) => a.getAttribute('href'))).toContain('/runs')
  })

  it('提交筛选时在前端内按条件跳转（GET，带全部字段，由服务端去掉空字段）', async () => {
    const calls = stubFetch(() => json(envelope(filteredFixture)))
    renderPage(envelope(fixture), '/runs')
    await userEvent.type(screen.getByLabelText('仓库'), 'kiln')
    await userEvent.selectOptions(screen.getByLabelText('状态'), 'failed')
    await userEvent.click(screen.getByRole('button', { name: '筛选' }))
    await waitFor(() => expect(calls).toHaveLength(1))
    const u = new URL(calls[0]?.url ?? '', 'http://x')
    expect(u.pathname).toBe('/runs')
    expect(u.searchParams.get('repo')).toBe('kiln')
    expect(u.searchParams.get('state')).toBe('failed')
    expect(calls[0]?.method).toBe('GET')
  })

  it('filterSummary：按子串匹配仓库时写明"仓库名包含"', () => {
    const f = { Repo: 'demo', State: '', Workflow: 'ci.yml', Ref: 'main', Actor: 'bob', Active: true }
    expect(filterSummary(f, '')).toBe('已筛选：仓库名包含"demo"，workflow ci.yml，ref main，发起人 bob。')
    expect(filterSummary({ ...f, Active: false }, '')).toBe('')
  })
})

describe('运行列表的语言', () => {
  it('英文：筛选表单、状态选项（服务端的消息）、筛选说明和空状态', () => {
    renderPage(envelope(filteredFixture), '/runs?repo=acme%2Fkiln&state=failed', { locale: 'en' })
    expect(document.title).toBe('Runs · Kiln')
    expect(screen.getByLabelText('Repository')).toHaveValue('acme/kiln')
    const state = screen.getByLabelText('Status')
    expect(state).toHaveValue('failed')
    expect(within(state).getByRole('option', { name: 'Failed' })).toBeInTheDocument()
    expect(within(state).getByRole('option', { name: 'All statuses' })).toBeInTheDocument()
    expect(screen.getByText(/Filtered by repository acme\/kiln, status Failed\./)).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'No builds match the filters' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Start a build for acme/kiln' })).toBeInTheDocument()
  })

  it('筛选表单填写到一半时切换语言：已填的条件保留，文字立即切换', async () => {
    renderPage(envelope(fixture), '/runs')
    const ref = screen.getByLabelText('ref')
    await userEvent.type(ref, 'release/1.x')
    await switchLocale('English')
    expect(screen.getByLabelText('Ref')).toBe(ref)
    expect(ref).toHaveValue('release/1.x')
    expect(screen.getByRole('button', { name: 'Filter' })).toBeInTheDocument()
    expect(within(screen.getByRole('list', { name: 'Builds' })).getAllByRole('listitem').length).toBeGreaterThan(0)
  })

  it('filterSummary 按语言写说明', () => {
    const f = { Repo: 'demo', State: 'failed', Workflow: '', Ref: '', Actor: 'bob', Active: true }
    expect(filterSummary(f, '', 'en')).toBe('Filtered by repository name contains "demo", status Failed, started by bob. ')
  })
})
