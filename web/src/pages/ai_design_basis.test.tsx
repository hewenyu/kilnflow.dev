import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import requestFixture from '../../dev/fixtures/ai_request.json'
import taskFixture from '../../dev/fixtures/ai_task.json'
import workItemFixture from '../../dev/fixtures/ai_dev_item.json'
import { json, stubFetch } from '../test/fetch'
import { deferred, envelope, renderPage, withData } from '../test/render'

// 设计依据：冻结元数据、清理状态、按需读取正文、历史来源和关联任务关系。
// 页面数据本身不含正文；只有点“查看正文”才请求那一篇，且关闭再打开不重复读取。

afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

function nth<T>(items: T[], i: number): T {
  const v = items[i]
  if (v === undefined) throw new Error(`缺少第 ${i} 个元素`)
  return v
}

const content = (id: string, text: string) => ({
  doc_id: id,
  title: id,
  revision: 4,
  sha256: 'd7f4274a59730a9a3ebf033cc49fb7c5ed92ddff5d834125e61bd43b4531085c',
  content: text,
})

describe('AI 请求的设计依据', () => {
  it('展示冻结时间、来源、摘要和文档元数据；清理过的文档没有查看按钮', () => {
    renderPage(envelope(requestFixture), '/ai/requests/88')
    const basis = screen.getByRole('region', { name: '设计依据' })
    expect(within(basis).getByText('独立冻结的快照集')).toBeInTheDocument()
    expect(within(basis).getByText(/请求记录的来源：独立冻结的快照集/)).toBeInTheDocument()
    expect(within(basis).getByText('#412')).toBeInTheDocument()
    expect(within(basis).getByText('2', { selector: 'dd' })).toBeInTheDocument()
    expect(within(basis).getByText('d8344d814a904f420f312c0dd6f35d67bbad83af52a0e4b9ce0706c681819820')).toBeInTheDocument()
    expect(within(basis).getByText('Runner 升级协议')).toBeInTheDocument()
    expect(within(basis).getByText('r4')).toBeInTheDocument()
    expect(within(basis).getByTitle('d7f4274a59730a9a3ebf033cc49fb7c5ed92ddff5d834125e61bd43b4531085c')).toHaveTextContent('d7f4274a5973')
    expect(within(basis).getAllByTitle('在 Outline 中打开这篇文档')[0]).toHaveAttribute('href', 'https://outline.example.com/doc/doc-api')
    // 清理过正文的文档仍列出标题和指纹，但不能查看正文
    expect(within(basis).getByText('Runner 详情页')).toBeInTheDocument()
    expect(within(basis).getByText('已按保留策略清理')).toBeInTheDocument()
    expect(within(basis).getAllByRole('button', { name: '查看正文' })).toHaveLength(1)
  })

  it('请求记录的来源与实际快照不一致时明确报错，不展示为正常依据', () => {
    const view = {
      ...requestFixture.Data.DesignBasis,
      Error: '请求记录的摘要与文档快照集不一致',
      Basis: {
        ...requestFixture.Data.DesignBasis.Basis,
        Source: 'none',
        Frozen: false,
        Docs: [],
        DocCount: 0,
      },
    }
    renderPage(withData(envelope(requestFixture), { DesignBasis: view }), '/ai/requests/88')
    const basis = screen.getByRole('region', { name: '设计依据' })
    expect(within(basis).getByText(/请求记录的摘要与文档快照集不一致/)).toBeInTheDocument()
    expect(within(basis).getByText(/页面不会把它当作正常依据/)).toBeInTheDocument()
    expect(within(basis).getByText(/请求记录的独立快照集不存在或读不到/)).toBeInTheDocument()
    expect(within(basis).queryByRole('table', { name: '冻结的设计文档' })).not.toBeInTheDocument()
  })

  it('空集合即使整批正文被清理过，也只说明是空集合', () => {
    const view = {
      ...requestFixture.Data.DesignBasis,
      Basis: {
        ...requestFixture.Data.DesignBasis.Basis,
        DocCount: 0,
        Docs: [],
        ContentPurgedAt: '2026-11-19T00:00:00Z',
      },
    }
    renderPage(withData(envelope(requestFixture), { DesignBasis: view }), '/ai/requests/88')
    const basis = screen.getByRole('region', { name: '设计依据' })
    expect(within(basis).getByText('这个快照是空集合，没有文档。')).toBeInTheDocument()
    expect(within(basis).queryByText(/正文已于/)).not.toBeInTheDocument()
  })

  it('迁移前的请求：明确说明没有记录来源，不按当前绑定补造', () => {
    const view = {
      ...requestFixture.Data.DesignBasis,
      DeclaredSource: 'legacy',
      Declared: false,
      SnapshotSetID: 0,
      Error: '',
      Basis: { ...requestFixture.Data.DesignBasis.Basis, Source: 'none', Frozen: false, Docs: [], DocCount: 0 },
    }
    renderPage(withData(envelope(requestFixture), { DesignBasis: view }), '/ai/requests/88')
    const basis = screen.getByRole('region', { name: '设计依据' })
    expect(within(basis).getByText(/请求记录的来源：迁移前，没有记录/)).toBeInTheDocument()
    expect(within(basis).getByText(/迁移前的请求没有记录设计依据；这里不按当前仓库绑定补造/)).toBeInTheDocument()
  })
})

describe('AI 任务的设计依据', () => {
  it('请求记录与实际解析不同时分别展示', () => {
    const view = {
      ...taskFixture.Data.DesignBasis,
      DeclaredSource: 'none',
      BasisSource: 'work_item',
    }
    renderPage(withData(envelope(taskFixture), { DesignBasis: view }), '/ai/tasks/520')
    const basis = screen.getByRole('region', { name: '设计依据' })
    expect(within(basis).getByText(/请求记录的来源：没有文档依据/)).toBeInTheDocument()
    expect(within(basis).getByText(/实际可读的依据：工作项冻结快照/)).toBeInTheDocument()
  })

  it('按需读取正文：只请求这一篇，关闭再打开命中缓存', async () => {
    const calls = stubFetch((c) =>
      c.url.includes('doc_id=doc-api') ? json(content('doc-api', '# 升级协议\n\n正文')) : json(content('doc-ui', '详情页正文')),
    )
    const docCalls = () => calls.filter((c) => c.url.includes('/docs/content'))
    renderPage(envelope(taskFixture), '/ai/tasks/520')
    const basis = screen.getByRole('region', { name: '设计依据' })
    await userEvent.click(nth(within(basis).getAllByRole('button', { name: '查看正文' }), 0))
    await waitFor(() => expect(screen.getByText(/# 升级协议/)).toBeInTheDocument())
    expect(docCalls().map((c) => c.url)).toEqual(['/ai/tasks/520/docs/content?doc_id=doc-api'])
    // 正文按纯文本显示，不把 Markdown 当标记执行
    expect(screen.getByText(/# 升级协议/).tagName).toBe('PRE')
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: '关闭' }))

    await userEvent.click(nth(within(basis).getAllByRole('button', { name: '查看正文' }), 1))
    await waitFor(() => expect(screen.getByText('详情页正文')).toBeInTheDocument())
    expect(docCalls()).toHaveLength(2)
    expect(docCalls()[1]?.url).toBe('/ai/tasks/520/docs/content?doc_id=doc-ui')
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: '关闭' }))

    // 再看第一篇：用已经读过的正文，不再请求
    await userEvent.click(nth(within(basis).getAllByRole('button', { name: '查看正文' }), 0))
    expect(screen.getByText(/# 升级协议/)).toBeInTheDocument()
    expect(docCalls()).toHaveLength(2)
  })

  it('正文已清理（410）：明确说明，关闭后这一篇不再显示查看按钮', async () => {
    stubFetch(() => json({ code: 'content_purged', message: '这篇设计文档的正文已按保留策略清理' }, 410))
    renderPage(envelope(taskFixture), '/ai/tasks/520')
    const basis = screen.getByRole('region', { name: '设计依据' })
    await userEvent.click(nth(within(basis).getAllByRole('button', { name: '查看正文' }), 0))
    await waitFor(() => expect(screen.getByText('正文已清理')).toBeInTheDocument())
    expect(screen.getByText(/不会为它恢复旧正文/)).toBeInTheDocument()
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: '关闭' }))
    expect(within(basis).getByText('已按保留策略清理')).toBeInTheDocument()
  })

  it('依据不一致（409）时显示原因，可以重试', async () => {
    stubFetch(() => json({ code: 'basis_conflict', message: '设计依据的记录与实际快照不一致' }, 409))
    renderPage(envelope(taskFixture), '/ai/tasks/520')
    const basis = screen.getByRole('region', { name: '设计依据' })
    await userEvent.click(nth(within(basis).getAllByRole('button', { name: '查看正文' }), 0))
    await waitFor(() => expect(screen.getByText('设计依据的记录与实际快照不一致')).toBeInTheDocument())
    expect(within(screen.getByRole('dialog')).getByRole('button', { name: '重试' })).toBeInTheDocument()
  })

  it('自动刷新替换页面数据时，打开的正文弹窗不关闭、不重复读取；关闭后恢复刷新', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const pending = deferred<Response>()
    let pageCalls = 0
    const calls = stubFetch((c) => {
      if (c.url.includes('/docs/content')) return json(content('doc-api', '# 升级协议'))
      // 日志等其他请求照常返回空文本，不参与页面刷新计数。
      if (c.url !== '/ai/tasks/520') return new Response('', { headers: { 'Content-Type': 'text/plain; charset=utf-8' } })
      pageCalls++
      return pageCalls === 1 ? pending.promise : json(refreshed)
    })
    const freshDigest = 'e'.repeat(64)
    const refreshed = withData(envelope(taskFixture), {
      Terminal: false,
      RequestTerminal: false,
      DesignBasis: { ...taskFixture.Data.DesignBasis, Basis: { ...taskFixture.Data.DesignBasis.Basis, ManifestDigest: freshDigest } },
    })
    renderPage(withData(envelope(taskFixture), { Terminal: false, RequestTerminal: false }), '/ai/tasks/520')
    // 先发起一次真实的定时刷新（响应还没回来）。
    await act(() => vi.advanceTimersByTimeAsync(5000))
    expect(pageCalls).toBe(1)
    const basis = screen.getByRole('region', { name: '设计依据' })
    fireEvent.click(nth(within(basis).getAllByRole('button', { name: '查看正文' }), 0))
    expect(await screen.findByText(/# 升级协议/)).toBeInTheDocument()
    const contentCalls = () => calls.filter((c) => c.url.includes('/docs/content'))
    expect(contentCalls()).toHaveLength(1)
    // 刷新响应带着新的页面数据（新的对象引用）到达：弹窗保持打开，正文不被重新读取。
    await act(async () => {
      pending.resolve(json(refreshed))
      await vi.advanceTimersByTimeAsync(0)
    })
    expect(await screen.findByText(freshDigest)).toBeInTheDocument()
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(screen.getByText(/# 升级协议/)).toBeInTheDocument()
    expect(contentCalls()).toHaveLength(1)
    // 关闭弹窗后释放刷新锁：下一次 5 秒的刷新照常发出。
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: '关闭' }))
    await act(() => vi.advanceTimersByTimeAsync(5000))
    expect(pageCalls).toBe(2)
  })

  it('快速切换文档时，晚到的旧响应不会覆盖后选文档的正文', async () => {
    const slow = deferred<Response>()
    const calls = stubFetch((c) => {
      if (c.url.includes('doc_id=doc-api')) return slow.promise
      return json(content('doc-ui', '详情页正文'))
    })
    renderPage(envelope(taskFixture), '/ai/tasks/520')
    const basis = screen.getByRole('region', { name: '设计依据' })
    const buttons = () => within(basis).getAllByRole('button', { name: '查看正文' })
    await userEvent.click(nth(buttons(), 0))
    await waitFor(() => expect(calls.some((c) => c.url.includes('doc_id=doc-api'))).toBe(true))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: '关闭' }))
    await userEvent.click(nth(buttons(), 1))
    await waitFor(() => expect(screen.getByText('详情页正文')).toBeInTheDocument())
    // 先发的慢响应现在才到：它属于已经关闭的弹窗，不能覆盖当前正文。
    await act(async () => {
      slow.resolve(json(content('doc-api', '# 升级协议')))
      await Promise.resolve()
    })
    expect(screen.getByText('详情页正文')).toBeInTheDocument()
    expect(screen.queryByText('# 升级协议')).not.toBeInTheDocument()
  })

  it('登录已过期（401）时停止读取并提示重新登录', async () => {
    stubFetch(() => json({ code: 'unauthorized', message: '登录已过期' }, 401))
    renderPage(envelope(taskFixture), '/ai/tasks/520')
    const basis = screen.getByRole('region', { name: '设计依据' })
    await userEvent.click(nth(within(basis).getAllByRole('button', { name: '查看正文' }), 0))
    await waitFor(() => expect(screen.getByText('登录已过期，请重新登录后再查看。')).toBeInTheDocument())
    expect(within(screen.getByRole('dialog')).getByRole('link', { name: '重新登录' })).toHaveAttribute(
      'href',
      `/login?next=${encodeURIComponent('/ai/tasks/520')}`,
    )
    expect(within(screen.getByRole('dialog')).queryByRole('button', { name: '重试' })).not.toBeInTheDocument()
  })

  it('首次读取失败后点重试，实际重新请求并显示正文', async () => {
    let contentCalls = 0
    const calls = stubFetch((c) => {
      if (!c.url.includes('/docs/content')) {
        return new Response('', { headers: { 'Content-Type': 'text/plain; charset=utf-8' } })
      }
      contentCalls++
      return contentCalls === 1 ? json({ message: '暂时读不到正文' }, 500) : json(content('doc-api', '# 升级协议'))
    })
    renderPage(envelope(taskFixture), '/ai/tasks/520')
    const basis = screen.getByRole('region', { name: '设计依据' })
    await userEvent.click(nth(within(basis).getAllByRole('button', { name: '查看正文' }), 0))
    await waitFor(() => expect(screen.getByText('暂时读不到正文')).toBeInTheDocument())
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: '重试' }))
    await waitFor(() => expect(screen.getByText(/# 升级协议/)).toBeInTheDocument())
    expect(calls.filter((c) => c.url.includes('/docs/content'))).toHaveLength(2)
  })
})

describe('AI 工作项的设计依据', () => {
  it('冻结快照与正文清理状态', () => {
    renderPage(envelope(workItemFixture), '/ai-dev/14')
    const basis = screen.getByRole('region', { name: '设计依据' })
    expect(within(basis).getByText('工作项冻结快照')).toBeInTheDocument()
    expect(within(basis).getByText(/只展示冻结时保存的元数据/)).toBeInTheDocument()
    expect(within(basis).getByText('Runner 升级协议')).toBeInTheDocument()
    expect(within(basis).getByText('已按保留策略清理')).toBeInTheDocument()
  })

  it('关联任务：开发、返修、审计与冻结依据的关系如实展示', () => {
    renderPage(envelope(workItemFixture), '/ai-dev/14')
    const list = screen.getByRole('table', { name: '关联任务与依据' })
    const rows = within(list).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(4)
    expect(within(nth(rows, 0)).getByRole('link', { name: '#210' })).toHaveAttribute('href', '/ai/tasks/210')
    expect(within(nth(rows, 0)).getByText('领取后关联')).toBeInTheDocument()
    expect(within(nth(rows, 0)).getByText('选题')).toBeInTheDocument()
    expect(within(nth(rows, 1)).getByText('文档引用一致')).toBeInTheDocument()
    expect(within(nth(rows, 1)).getByText('实现')).toBeInTheDocument()
    expect(within(nth(rows, 2)).getByText('依据不一致')).toBeInTheDocument()
    expect(within(nth(rows, 2)).getByText('返修')).toBeInTheDocument()
    expect(within(nth(rows, 3)).getByText('迁移前核对一致')).toBeInTheDocument()
    expect(within(nth(rows, 3)).getByText('审计')).toBeInTheDocument()
    expect(within(nth(rows, 3)).getByRole('link', { name: '请求 #142' })).toHaveAttribute('href', '/ai/requests/142')
  })

  it.each(['toString', 'constructor', '__proto__'])('关联任务的关系、类型和方式是 %s 时原样显示，页面照常渲染', (key) => {
    const item = workItemFixture.Data.RelatedTasks[0]
    if (!item) throw new Error('fixture 应有相关任务')
    renderPage(withData(envelope(workItemFixture), { RelatedTasks: [{ ...item, Type: key, Mode: key, Relation: key }] }), '/ai-dev/14')
    const list = screen.getByRole('table', { name: '关联任务与依据' })
    const row = within(list).getAllByRole('row')[1]
    if (!row) throw new Error('应有任务行')
    // 关系 Label 的文字、类型和方式三处都按未知值原样显示
    expect(within(row).getAllByText(key)).toHaveLength(3)
  })
})
