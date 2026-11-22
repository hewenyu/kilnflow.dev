import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/merge_queue.json'
import emptyFixture from '../../dev/fixtures/merge_queue.empty.json'
import { envelope, renderPage, withData } from '../test/render'
import { enqueuedBy } from './merge_queue'

let requests: { url: string; method: string; body: string }[] = []

beforeEach(() => {
  requests = []
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input)
      requests.push({ url, method: init?.method ?? 'GET', body: init?.body instanceof URLSearchParams ? init.body.toString() : '' })
      const res = init?.method === 'POST' ? { Redirect: '/merge-queue?repo=acme%2Fkiln&flash=ok' } : fixture
      return Promise.resolve(new Response(JSON.stringify(res), { headers: { 'Content-Type': 'application/json' } }))
    }),
  )
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const member = { Session: { Login: 'lin-design', Admin: false, ViaToken: false, Bound: false } }

describe('合并队列', () => {
  it('自动打 tag 的记录：状态、tag、合入方式和原因；失败的可以重试', async () => {
    renderPage(envelope(fixture), '/merge-queue')
    const tags = within(screen.getByRole('list', { name: 'acme/kiln 自动打 tag 的记录' }))
    const items = tags.getAllByRole('listitem')
    expect(items).toHaveLength(3)
    const [tagging, failed, planned] = items
    if (!tagging || !failed || !planned) throw new Error('应有 3 条记录')
    expect(within(tagging).getByText('正在打 tag')).toBeInTheDocument()
    expect(within(tagging).getByText('v0.4.4')).toBeInTheDocument()
    expect(within(tagging).queryByRole('button', { name: '重试' })).not.toBeInTheDocument()
    expect(within(failed).getByText('失败')).toBeInTheDocument()
    expect(within(failed).getByText(/Contents 读写权限/)).toBeInTheDocument()
    expect(within(failed).getByText(/在队列之外/)).toHaveTextContent('→ main')
    expect(within(failed).getByText(/在队列之外/)).toHaveTextContent('，label minor')
    expect(within(planned).getByRole('link', { name: 'v0.4.2' })).toHaveAttribute('href', 'https://github.com/acme/kiln/releases/tag/v0.4.2')
    expect(within(planned).getByText(/经合并队列/)).toBeInTheDocument()
    // 机器人合并的可信 AI 开发实现 PR：显示机器人和认定的工作项
    expect(within(planned).getByText(/（kiln\[bot\]，机器人）/)).toBeInTheDocument()
    expect(within(planned).getByRole('link', { name: '工作项 #12' })).toHaveAttribute('href', '/ai-dev/12')
    expect(within(failed).queryByText(/机器人/)).not.toBeInTheDocument()
    expect(screen.queryByRole('list', { name: 'acme/notes 自动打 tag 的记录' })).not.toBeInTheDocument()

    await userEvent.click(within(failed).getByRole('button', { name: '重试' }))
    await waitFor(() => expect(requests.some((r) => r.method === 'POST')).toBe(true))
    const post = requests.find((r) => r.method === 'POST')
    expect(post?.url).toBe('/merge-queue/autotag/retry')
    const body = new URLSearchParams(post?.body)
    expect(body.get('repo')).toBe('acme/kiln')
    expect(body.get('sha')).toBe('1a2b3c4d5e6f708192a3b4c5d6e7f80912345678')
  })

  it.each(['toString', 'constructor', '__proto__'])('移出原因类别是 %s 时按未知处理，页面照常渲染', (removal) => {
    const q = fixture.Data.Queues[0]
    if (!q) throw new Error('fixture 应有队列')
    const first = q.Recent[0]
    if (!first) throw new Error('fixture 应有最近的记录')
    const recent = [{ ...first, State: 'removed', Removal: removal, MergeSHA: '' }]
    renderPage(withData(envelope(fixture), { Queues: [{ ...q, Recent: recent }] }), '/merge-queue')
    expect(screen.getByRole('heading', { level: 1, name: '合并队列' })).toBeInTheDocument()
    // 不认识的类别不显示 Label，也不把值渲染出来
    expect(screen.queryByText(removal)).not.toBeInTheDocument()
    expect(screen.getByText('已移出')).toBeInTheDocument()
  })

  it('成员看不到自动打 tag 的重试按钮', () => {
    renderPage(envelope(fixture, member), '/merge-queue')
    const tags = within(screen.getByRole('list', { name: 'acme/kiln 自动打 tag 的记录' }))
    expect(tags.queryByRole('button', { name: '重试' })).not.toBeInTheDocument()
  })

  it('按仓库列出排队中的 PR（顺序、状态、head、入队者）和最近结束的项', () => {
    // 入队时间显示为"多久之前"：固定当前时间，不随测试运行的日期变成具体日期
    vi.useFakeTimers({ now: Date.parse('2026-11-08T10:00:00-08:00'), shouldAdvanceTime: true })
    renderPage(envelope(fixture), '/merge-queue')
    expect(screen.getByRole('heading', { level: 1, name: '合并队列' })).toBeInTheDocument()
    const nav = screen.getByRole('navigation', { name: '主导航' })
    expect(within(nav).getByRole('link', { name: /合并队列/ })).toHaveAttribute('aria-current', 'page')

    const kiln = screen.getByRole('region', { name: /acme\/kiln/ })
    expect(within(kiln).getByRole('link', { name: 'acme/kiln' })).toHaveAttribute('href', '/merge-queue?repo=acme%2Fkiln')
    const queued = within(kiln).getByRole('list', { name: 'acme/kiln 排队中的 PR' })
    const rows = within(queued).getAllByRole('listitem')
    expect(rows).toHaveLength(3)
    const [first, second, third] = rows
    if (!first || !second || !third) throw new Error('应有 3 个排队中的 PR')
    expect(within(first).getByRole('link', { name: '#104' })).toHaveAttribute('href', 'https://github.com/acme/kiln/pull/104')
    expect(within(first).getByText('→ main')).toBeInTheDocument()
    expect(within(first).getByText('正在处理')).toBeInTheDocument()
    expect(within(first).getByText('等待验证：2/3 个 run 已结束')).toBeInTheDocument()
    expect(within(first).getByRole('link', { name: '9f2c4e1a7b3d' })).toHaveAttribute('href', 'https://github.com/acme/kiln/commit/9f2c4e1a7b3d5c6e8f0a1b2c3d4e5f6a7b8c9d0e')
    expect(within(first).getByText(/由 octocat（加 label）/)).toHaveTextContent(/由 octocat（加 label） 于 .+前 入队/)
    // 目标 dev 的分支各自排队（PLAN #28.5）：它的第一个排队项也是正在处理的队首，互不阻塞；排位按同一目标分支计算，与
    // 状态文案"目标 dev · 排在第 2 个"一致
    expect(within(second).getByText('→ dev')).toBeInTheDocument()
    expect(within(second).getByText('正在处理')).toBeInTheDocument()
    expect(within(second).getByLabelText('第 1 个')).toBeInTheDocument()
    expect(within(second).getByText('等待验证：1/3 个 run 已结束')).toBeInTheDocument()
    expect(within(third).getByText('→ dev')).toBeInTheDocument()
    expect(within(third).queryByText('正在处理')).not.toBeInTheDocument()
    expect(within(third).getByLabelText('第 2 个')).toBeInTheDocument()
    expect(within(third).getByText('目标 dev · 排在第 2 个')).toBeInTheDocument()
    // 还没有处理的项没有 head
    expect(within(third).queryByText(/^head/)).not.toBeInTheDocument()

    const recent = within(kiln).getByRole('list', { name: 'acme/kiln 最近结束的 PR' })
    const done = within(recent).getAllByRole('listitem')
    expect(done).toHaveLength(5)
    const [merged, removed, outside, handedBack, author] = done
    if (!merged || !removed || !outside || !handedBack || !author) throw new Error('应有 5 个最近结束的项')
    // 冲突出队按 PR 的归属：AI 开发的交回返修，第三方的等提交者
    expect(within(handedBack).getByText('冲突：已交给 AI 开发')).toBeInTheDocument()
    expect(within(handedBack).getByText(/已交给 AI 开发返修（工作项 #12）/)).toBeInTheDocument()
    expect(within(author).getByText('冲突：等待提交者解决')).toBeInTheDocument()
    expect(within(removed).queryByText(/^冲突：/)).not.toBeInTheDocument()
    expect(within(merged).getByText('已合并')).toBeInTheDocument()
    // 最近结束的项也显示目标分支（PLAN #28.5）
    expect(within(merged).getByText('→ main')).toBeInTheDocument()
    expect(within(merged).getByRole('link', { name: '5e6f7a8b9c0d' })).toBeInTheDocument()
    // 队列合并后关闭关联 issue 的进度
    expect(within(merged).getByText(/已关闭关联的 issue #101/)).toBeInTheDocument()
    expect(within(outside).queryByText(/关联的 issue/)).not.toBeInTheDocument()
    expect(within(removed).getByText('已移出')).toBeInTheDocument()
    expect(within(removed).getByText(/验证失败：ci\.yml/)).toBeInTheDocument()
    expect(within(outside).getByText(/PR 在合并队列之外被合并/)).toBeInTheDocument()

    // 另一个仓库没有排队中的 PR
    const notes = screen.getByRole('region', { name: /acme\/notes/ })
    expect(within(notes).getByText('没有排队中的 PR。')).toBeInTheDocument()
    expect(within(notes).getByText(/PR 已关闭/)).toBeInTheDocument()
  })

  it('管理员填写仓库和 PR 号加入合并队列', async () => {
    renderPage(envelope(fixture), '/merge-queue')
    const form = screen.getByRole('form', { name: '加入合并队列' })
    await userEvent.selectOptions(within(form).getByLabelText(/仓库/), 'acme/notes')
    await userEvent.type(within(form).getByLabelText(/PR 号/), '42')
    await userEvent.click(within(form).getByRole('button', { name: '加入合并队列' }))
    await waitFor(() => expect(requests.some((r) => r.method === 'POST')).toBe(true))
    const post = requests.find((r) => r.method === 'POST')
    expect(post?.url).toBe('/merge-queue/add')
    const body = new URLSearchParams(post?.body)
    expect(body.get('repo')).toBe('acme/notes')
    expect(body.get('pr')).toBe('42')
    expect(body.get('csrf')).toBe('preview')
  })

  it('管理员确认后把 PR 移出合并队列', async () => {
    renderPage(envelope(fixture), '/merge-queue')
    const queued = screen.getByRole('list', { name: 'acme/kiln 排队中的 PR' })
    const second = within(queued).getAllByRole('listitem')[1]
    if (!second) throw new Error('没有第 2 个排队中的 PR')
    await userEvent.click(within(second).getByRole('button', { name: '移出' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText(/去掉 PR 上的 label kiln:merge/)).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('button', { name: '移出合并队列' }))
    await waitFor(() => expect(requests.some((r) => r.method === 'POST')).toBe(true))
    const post = requests.find((r) => r.method === 'POST')
    expect(post?.url).toBe('/merge-queue/remove')
    const body = new URLSearchParams(post?.body)
    expect(body.get('repo')).toBe('acme/kiln')
    expect(body.get('pr')).toBe('107')
  })

  it('成员只能查看：没有加入表单和移出按钮', () => {
    renderPage(withData(envelope(fixture, member), { Repos: null }), '/merge-queue')
    expect(screen.getByRole('list', { name: 'acme/kiln 排队中的 PR' })).toBeInTheDocument()
    expect(screen.queryByRole('form', { name: '加入合并队列' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '移出' })).not.toBeInTheDocument()
  })

  it('没有任何记录时说明怎样加入合并队列', () => {
    renderPage(envelope(emptyFixture), '/merge-queue')
    expect(screen.getByText('合并队列是空的')).toBeInTheDocument()
    expect(screen.getByText(/也可以在上面填写仓库和 PR 号加入/)).toHaveTextContent('给 PR 加上 label kiln:merge 即加入合并队列')
    expect(screen.getByRole('form', { name: '加入合并队列' })).toBeInTheDocument()
  })

  it('只看一个仓库时标题是仓库名，可以回到全部仓库', () => {
    const kiln = fixture.Data.Queues[0]
    renderPage(withData(envelope(fixture), { Repo: 'acme/kiln', Queues: [kiln] }), '/merge-queue?repo=acme/kiln')
    expect(screen.getByRole('heading', { level: 1, name: 'acme/kiln 的合并队列' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '全部仓库' })).toHaveAttribute('href', '/merge-queue')
    expect(within(screen.getByRole('form', { name: '加入合并队列' })).getByLabelText(/仓库/)).toHaveValue('acme/kiln')
  })

  it('入队者：github: 前缀表示在 GitHub 上加了 label', () => {
    expect(enqueuedBy('github:alice')).toBe('alice（加 label）')
    expect(enqueuedBy('admin')).toBe('admin')
    expect(enqueuedBy('auto')).toBe('Kiln（验证通过后自动入队）')
    expect(enqueuedBy('')).toBe('未知')
  })
})
