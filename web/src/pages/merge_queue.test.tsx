import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/merge_queue.json'
import emptyFixture from '../../dev/fixtures/merge_queue.empty.json'
import { chineseLeft, envelope, rawStrings, renderPage, switchLocale, withData } from '../test/render'
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
    expect(items).toHaveLength(4)
    const [tagging, failed, planned, skipped] = items
    if (!tagging || !failed || !planned || !skipped) throw new Error('应有 4 条记录')
    expect(within(tagging).getByText('正在打 tag')).toBeInTheDocument()
    expect(within(tagging).getByText('v0.4.4')).toBeInTheDocument()
    expect(within(tagging).queryByRole('button', { name: '重试' })).not.toBeInTheDocument()
    expect(within(failed).getByText('失败')).toBeInTheDocument()
    // 有原因码的原因按消息显示，失败的突出显示
    expect(within(failed).getByText(/^403 Resource not accessible by integration（GitHub App 需要 Contents 的读写权限）/)).toHaveClass('reason')
    expect(within(failed).getByText(/在队列之外/)).toHaveTextContent('，尝试 3 次')
    // 没有原因码的旧记录原样显示中文说明；跳过的不突出显示，可以重试
    expect(within(skipped).getByText('已跳过')).toBeInTheDocument()
    expect(within(skipped).getByText('版本号未定')).toBeInTheDocument()
    expect(within(skipped).getByText(/^PR 有 release:skip/)).not.toHaveClass('reason')
    expect(within(skipped).getByRole('button', { name: '重试' })).toBeInTheDocument()
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
    expect(done).toHaveLength(6)
    const [merged, removed, outside, handedBack, author, gaveUp] = done
    if (!merged || !removed || !outside || !handedBack || !author || !gaveUp) throw new Error('应有 6 个最近结束的项')
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
    expect(within(merged).getByText(/已关闭关联的 issue #101/)).not.toHaveClass('reason')
    // 关闭关联 issue 失败：按服务端给出的语义级别突出显示
    expect(within(gaveUp).getByText(/关闭关联的 issue 失败 5 次，已放弃：403/)).toHaveClass('reason')
    expect(within(gaveUp).getByText(/关闭关联的 issue 失败 5 次，已放弃：403/)).toHaveTextContent('（GitHub App 需要 Issues 的读写权限，见 DEPLOY.md #3）')
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
    expect(enqueuedBy('github:alice', 'en')).toBe('alice (added the label)')
    expect(enqueuedBy('admin', 'en')).toBe('admin')
    expect(enqueuedBy('auto', 'en')).toBe('Kiln (queued automatically after checks passed)')
    expect(enqueuedBy('', 'en')).toBe('unknown')
  })
})

describe('合并队列（英文与切换语言）', () => {
  // 原样显示的内容：没有原因码的旧记录记下的中文说明（以及消息参数中的原文）
  const rawZH = rawStrings(fixture.Data)
  // 状态标签（"Merged" 也是"Merged 5 minutes ago"的开头）
  const label = '[data-component="Label"]'

  it('英文：标题、排队中的 PR、最近结束的项和自动打 tag 按英文显示；有原因码的按消息，旧记录原样显示', () => {
    vi.useFakeTimers({ now: Date.parse('2026-11-08T10:00:00-08:00'), shouldAdvanceTime: true })
    renderPage(envelope(fixture), '/merge-queue', { locale: 'en' })
    expect(screen.getByRole('heading', { level: 1, name: 'Merge queue' })).toBeInTheDocument()
    expect(document.title).toMatch(/^Merge queue/)
    expect(screen.getByText(/PRs queue separately per target branch/)).toHaveTextContent('Add the kiln:merge label to a PR')

    const queued = within(screen.getByRole('list', { name: 'Queued PRs in acme/kiln' }))
    const [first, second, third] = queued.getAllByRole('listitem')
    if (!first || !second || !third) throw new Error('应有 3 个排队中的 PR')
    expect(within(first).getByText('Processing')).toBeInTheDocument()
    expect(within(first).getByLabelText('Position 1')).toBeInTheDocument()
    expect(within(first).getByText('Waiting for checks: 2/3 runs finished')).toBeInTheDocument()
    expect(within(first).getByText(/Queued by octocat \(added the label\)/)).toHaveTextContent(/ago$/)
    // 旧记录（没有原因码）原样显示中文说明，不按中文反推
    expect(within(second).getByText('等待验证：1/3 个 run 已结束')).toBeInTheDocument()
    expect(within(third).getByText('Target dev · position 2 in the queue')).toBeInTheDocument()
    expect(within(first).getByRole('button', { name: 'Remove' })).toBeInTheDocument()

    const recent = within(screen.getByRole('list', { name: 'Recently finished PRs in acme/kiln' }))
    const [merged, removed, outside, handedBack, author, gaveUp] = recent.getAllByRole('listitem')
    if (!merged || !removed || !outside || !handedBack || !author || !gaveUp) throw new Error('应有 6 个最近结束的项')
    expect(within(merged).getByText('Merged', { selector: label })).toBeInTheDocument()
    expect(within(merged).getByText(/Closed linked issues #101/)).not.toHaveClass('reason')
    expect(within(removed).getByText('Removed', { selector: label })).toBeInTheDocument()
    expect(within(removed).getByText(/^验证失败：ci\.yml（run #131）失败/)).toHaveClass('reason')
    expect(within(outside).getByText(/^The PR was merged outside the merge queue/)).toBeInTheDocument()
    expect(within(handedBack).getByText('Conflict: handed to AI development')).toBeInTheDocument()
    expect(within(handedBack).getByText(/^Conflicts with the target branch main \(5e6f7a8b9c0d\); handed back to AI development to fix \(work item #12\)/)).toHaveClass('reason')
    expect(within(author).getByText('Conflict: waiting for the author')).toBeInTheDocument()
    // 关闭关联 issue 失败：英文界面同样按语义级别突出显示，与译文无关
    expect(within(gaveUp).getByText(/Couldn't close linked issues after 5 attempts; gave up: 403/)).toHaveClass('reason')

    const tags = within(screen.getByRole('list', { name: 'Auto-tag records for acme/kiln' }))
    const [tagging, failed, planned, skipped] = tags.getAllByRole('listitem')
    if (!tagging || !failed || !planned || !skipped) throw new Error('应有 4 条记录')
    expect(within(tagging).getByText('Tagging')).toBeInTheDocument()
    expect(within(failed).getByText('Failed')).toBeInTheDocument()
    expect(within(failed).getByText(/^403 Resource not accessible by integration \(the GitHub App needs read and write access to Contents\)/)).toHaveClass('reason')
    expect(within(failed).getByText(/merged outside the queue/)).toHaveTextContent(', labels minor')
    expect(within(failed).getByText(/merged outside the queue/)).toHaveTextContent(', 3 attempts')
    expect(within(failed).getByRole('button', { name: 'Retry' })).toBeInTheDocument()
    expect(within(failed).getByRole('form', { name: 'Retry auto-tag for 1a2b3c4' })).toBeInTheDocument()
    expect(within(planned).getByText(/merged through the merge queue/)).toHaveTextContent('(kiln[bot], bot)')
    expect(within(planned).getByRole('link', { name: 'Work item #12' })).toHaveAttribute('href', '/ai-dev/12')
    expect(within(skipped).getByText('Skipped')).toBeInTheDocument()
    expect(within(skipped).getByText('Version not decided yet')).toBeInTheDocument()
    expect(within(skipped).getByText(/^PR 有 release:skip/)).toBeInTheDocument()

    const notes = screen.getByRole('region', { name: /acme\/notes/ })
    expect(within(notes).getByText('No PRs in the queue.')).toBeInTheDocument()
    expect(within(notes).getByText(/^The PR was closed/)).toBeInTheDocument()
    const form = screen.getByRole('form', { name: 'Add to merge queue' })
    expect(within(form).getByLabelText(/Repository/)).toBeInTheDocument()
    expect(within(form).getByPlaceholderText('e.g. 42')).toBeInTheDocument()
    expect(chineseLeft(document.body, rawZH)).toEqual([])
  })

  it('英文：只看一个仓库和没有记录时', () => {
    const kiln = fixture.Data.Queues[0]
    const { unmount } = renderPage(
      withData(envelope(fixture, { Title: { id: 'mergeQueue.pageTitleRepo', params: { repo: 'acme/kiln' } } }), { Repo: 'acme/kiln', Queues: [kiln] }),
      '/merge-queue?repo=acme/kiln',
      { locale: 'en' },
    )
    expect(screen.getByRole('heading', { level: 1, name: 'Merge queue for acme/kiln' })).toBeInTheDocument()
    expect(document.title).toMatch(/^Merge queue · acme\/kiln/)
    expect(screen.getByRole('link', { name: 'All repositories' })).toHaveAttribute('href', '/merge-queue')
    unmount()
    renderPage(envelope(emptyFixture), '/merge-queue', { locale: 'en' })
    expect(screen.getByText('The merge queue is empty')).toBeInTheDocument()
    expect(screen.getByText(/or enter a repository and PR number above/)).toHaveTextContent('Add the kiln:merge label to a PR')
    expect(chineseLeft(document.body, rawStrings(emptyFixture.Data))).toEqual([])
  })

  it('移出的确认对话框打开时切换语言：对话框保持打开，加入表单中填写的内容不丢', async () => {
    renderPage(envelope(fixture), '/merge-queue')
    const form = screen.getByRole('form', { name: '加入合并队列' })
    await userEvent.type(within(form).getByLabelText(/PR 号/), '42')
    const queued = screen.getByRole('list', { name: 'acme/kiln 排队中的 PR' })
    const second = within(queued).getAllByRole('listitem')[1]
    if (!second) throw new Error('没有第 2 个排队中的 PR')
    await userEvent.click(within(second).getByRole('button', { name: '移出' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText('把 #107 移出合并队列？')).toBeInTheDocument()
    await switchLocale('English')
    expect(screen.getByRole('alertdialog')).toBe(dialog)
    expect(within(dialog).getByText('Remove #107 from the merge queue?')).toBeInTheDocument()
    expect(within(dialog).getByText(/remove the kiln:merge label from the PR/)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: 'Remove from merge queue' })).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: 'Back' })).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: /PR number/, hidden: true })).toHaveValue('42')
  })
})
