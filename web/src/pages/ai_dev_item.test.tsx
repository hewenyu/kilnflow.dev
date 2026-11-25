import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/ai_dev_item.json'
import { chineseLeft, envelope, rawStrings, renderPage, switchLocale, withData } from '../test/render'

let requests: { url: string; method: string; body: string }[] = []

beforeEach(() => {
  requests = []
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input)
      requests.push({ url, method: init?.method ?? 'GET', body: init?.body instanceof URLSearchParams ? init.body.toString() : '' })
      const res = init?.method === 'POST' ? { Redirect: '/ai-dev/14?flash=ok' } : fixture
      return Promise.resolve(new Response(JSON.stringify(res), { headers: { 'Content-Type': 'application/json' } }))
    }),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
})

const member = { Session: { Login: 'lin-design', Admin: false, ViaToken: false, Bound: false } }

describe('工作项详情', () => {
  it('概要：状态、原因、分支、PR、返修轮次；导航高亮 AI 开发', () => {
    renderPage(envelope(fixture), '/ai-dev/14')
    expect(screen.getByRole('heading', { level: 1, name: '工作项 #14' })).toBeInTheDocument()
    const nav = screen.getByRole('navigation', { name: '主导航' })
    expect(within(nav).getByRole('link', { name: /AI 开发/ })).toHaveAttribute('aria-current', 'page')
    const summary = screen.getByRole('region', { name: '概要' })
    expect(within(summary).getByText('受阻')).toBeInTheDocument()
    expect(within(summary).getByText(/返修 2 轮后 CI 仍未通过/)).toBeInTheDocument()
    expect(within(summary).getByText('kiln/dev-14')).toBeInTheDocument()
    expect(within(summary).getByRole('link', { name: 'PR #131' })).toHaveAttribute('href', 'https://github.com/acme/kiln/pull/131')
    expect(within(summary).getByText('返修 2 / 2 轮')).toBeInTheDocument()
    expect(within(summary).getByRole('link', { name: '#215' })).toHaveAttribute('href', '/ai/tasks/215')
    expect(screen.getByRole('link', { name: 'acme/kiln 的工作项' })).toHaveAttribute('href', '/ai-dev?repo=acme%2Fkiln')
  })

  it('报告标明 Agent 自报，适用范围拿不到的显示"未知"', () => {
    renderPage(envelope(fixture), '/ai-dev/14')
    const selection = screen.getByRole('region', { name: /选题报告/ })
    expect(within(selection).getByText('Agent 自报')).toBeInTheDocument()
    // 选题报告没有模型
    expect(within(selection).getByText(/Runner 0\.1\.45 · Agent 版本 2\.1\.3 · 模型 未知/)).toBeInTheDocument()
    expect(within(selection).getByText(/选中：#128、#129/)).toBeInTheDocument()
    expect(within(selection).getByText(/#130：需求不清/)).toBeInTheDocument()

    const delivery = screen.getByRole('region', { name: /交付报告/ })
    expect(within(delivery).getByText('Agent 自报')).toBeInTheDocument()
    expect(within(delivery).getByText(/模型 claude-opus-5-5/)).toBeInTheDocument()
    expect(within(delivery).queryByText(/未知/)).not.toBeInTheDocument()
    expect(within(delivery).getByText(/PR 标题：Runner 详情页显示升级历史/)).toBeInTheDocument()
    expect(within(delivery).getByText(/未完成：按状态筛选/)).toBeInTheDocument()
    expect(within(delivery).getByText('F212-2')).toBeInTheDocument()
    expect(within(delivery).getByText('还剩什么')).toBeInTheDocument()

    // 计划也是 Agent 写的
    expect(within(screen.getByRole('region', { name: /计划/ })).getByText('Agent 自报')).toBeInTheDocument()
  })

  it('新格式的报告：交付方式、等待的依赖、设计文档、决策记录、需要人提供的条件、与设计的出入', () => {
    const [sel, del] = fixture.Data.Reports
    if (!sel?.Selection || !del?.Delivery) throw new Error('fixture 应有选题和交付报告')
    const selection = {
      ...sel,
      Selection: {
        ...sel.Selection,
        version: 2,
        design_first: true,
        design_reason: '要改协议和迁移，一个 PR 评审不过来',
        skipped: [{ number: 130, reason: '依赖 #128 的历史表', reconsider: '', later: false, wait_for: [128, 127] }],
      },
    }
    const delivery = {
      ...del,
      Delivery: {
        ...del.Delivery,
        version: 2,
        design: {
          documents: ['docs/PLAN.md'],
          decisions: [
            { category: 'policy', ref: '', context: '历史表会变大', options: '全量保留；只留 12 小时', choice: '只留 12 小时', impact: '看不到更早的升级', review: '有没有人要查更早的记录' },
            { category: 'confirmed_decision', ref: 'C19', context: '', options: '', choice: '改成异步发评论', impact: '', review: '' },
          ],
          external_inputs: [{ what: '一台 Linux Runner', blocks: '真实升级验证', resume: '注册后重跑验证' }],
        },
        design_deviations: [{ item: '筛选改用查询参数', reason: '路由更简单' }],
      },
    }
    renderPage(withData(envelope(fixture), { Reports: [selection, delivery] }), '/ai-dev/14')

    const s = within(screen.getByRole('region', { name: /选题报告/ }))
    expect(s.getByText('方式：先交付设计 PR，合并后再实现')).toBeInTheDocument()
    expect(s.getByText('要改协议和迁移，一个 PR 评审不过来')).toBeInTheDocument()
    expect(s.getByText(/#130：依赖 #128 的历史表/)).toHaveTextContent('等 #128、#127 关闭后再选')

    const d = within(screen.getByRole('region', { name: /交付报告/ }))
    expect(d.getByText('docs/PLAN.md')).toBeInTheDocument()
    expect(d.getByText(/预算、范围等取舍：只留 12 小时/)).toHaveTextContent('复盘时看：有没有人要查更早的记录')
    expect(d.getByText(/改已确认的决定（需要人确认）（C19）：改成异步发评论/)).not.toHaveTextContent('依据')
    expect(d.getByText(/一台 Linux Runner/)).toHaveTextContent('阻塞：真实升级验证')
    expect(d.getByText(/筛选改用查询参数/)).toHaveTextContent('理由：路由更简单')
  })

  it('旧格式的报告没有新段落', () => {
    renderPage(envelope(fixture), '/ai-dev/14')
    const s = within(screen.getByRole('region', { name: /选题报告/ }))
    expect(s.queryByText(/方式：/)).not.toBeInTheDocument()
    expect(s.getByText(/#130：需求不清/)).not.toHaveTextContent('关闭后再选')
    const d = within(screen.getByRole('region', { name: /交付报告/ }))
    expect(d.queryByText('设计文档')).not.toBeInTheDocument()
    expect(d.queryByText('决策记录')).not.toBeInTheDocument()
    expect(d.queryByText('与已合并设计的出入')).not.toBeInTheDocument()
  })

  it('适用范围全部为空时都显示"未知"；格式无法识别的报告显示原文', () => {
    const env = envelope(fixture)
    const raw = {
      Kind: 'delivery', TaskID: 300, TaskURL: '/ai/tasks/300', Selection: null, Delivery: null, Raw: '["不是对象"]',
      RunnerVersion: '', AgentVersion: '', Model: '', Created: '2026-11-10T09:00:00-08:00',
    }
    renderPage(withData(env, { Reports: [raw] }), '/ai-dev/14')
    const report = screen.getByRole('region', { name: /交付报告/ })
    expect(within(report).getByText(/Runner 未知 · Agent 版本 未知 · 模型 未知/)).toBeInTheDocument()
    expect(within(report).getByText('["不是对象"]')).toBeInTheDocument()
  })

  it('评论意图：结果不明的说明不自动重发，失败的显示原因', () => {
    renderPage(envelope(fixture), '/ai-dev/14')
    const items = within(screen.getByRole('list', { name: '评论意图' })).getAllByRole('listitem')
    const [done, unknown, failed] = items
    if (items.length !== 3 || !done || !unknown || !failed) throw new Error('应有 3 条评论')
    expect(within(done).getByText('已发送')).toBeInTheDocument()
    expect(within(unknown).getByText('结果不明，不自动重发')).toBeInTheDocument()
    expect(within(unknown).getByText(/#129 上的领取说明/)).toBeInTheDocument()
    expect(within(failed).getByText('发送失败')).toBeInTheDocument()
    expect(within(failed).getByText(/没有 Issues 写权限/)).toBeInTheDocument()
  })

  it('attempt 分支：已删除的显示删除时间，保留中的显示何时删除', () => {
    renderPage(envelope(fixture), '/ai-dev/14')
    const items = within(screen.getByRole('list', { name: 'attempt 分支' })).getAllByRole('listitem')
    const [deleted, live] = items
    if (items.length !== 2 || !deleted || !live) throw new Error('应有 2 个分支')
    expect(deleted).toHaveTextContent(/已于 .+ 删除/)
    expect(deleted).not.toHaveTextContent('保留期满')
    expect(live).toHaveTextContent(/推进后或 \d{4}-\d{2}-\d{2} \d{2}:\d{2} 保留期满后删除/)
  })

  it('管理员：受阻的工作项可以再返修一轮，也可以终止', async () => {
    renderPage(envelope(fixture), '/ai-dev/14')
    await userEvent.click(screen.getByRole('button', { name: '再返修一轮' }))
    await waitFor(() => expect(requests.some((r) => r.method === 'POST')).toBe(true))
    const post = requests.find((r) => r.method === 'POST')
    expect(post?.url).toBe('/ai-dev/retry')
    expect(new URLSearchParams(post?.body).get('id')).toBe('14')
    expect(screen.getByRole('button', { name: '终止' })).toBeInTheDocument()
  })

  it('已结束的工作项没有操作', () => {
    const item = { ...fixture.Data.Item, State: 'merged', Active: false, Retryable: false }
    renderPage(withData(envelope(fixture), { Item: item }), '/ai-dev/14')
    expect(screen.getByText('已合并')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '再返修一轮' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '终止' })).not.toBeInTheDocument()
  })

  it('成员只能查看', () => {
    renderPage(envelope(fixture, member), '/ai-dev/14')
    expect(screen.getByRole('region', { name: '概要' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '再返修一轮' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '终止' })).not.toBeInTheDocument()
  })
})

describe('工作项详情（英文与切换语言）', () => {
  const rawZH = rawStrings(fixture.Data)

  it('英文：概要、报告、评论、分支、设计依据和关联任务的界面文字按英文显示；Agent 写的内容原样显示', () => {
    renderPage(envelope(fixture), '/ai-dev/14', { locale: 'en' })
    expect(screen.getByRole('heading', { level: 1, name: 'Work item #14' })).toBeInTheDocument()
    const summary = screen.getByRole('region', { name: 'Summary' })
    expect(within(summary).getByText('Blocked')).toBeInTheDocument()
    expect(within(summary).getByText('Fix rounds 2 / 2')).toBeInTheDocument()
    expect(within(summary).getByText(/返修 2 轮后 CI 仍未通过/)).toBeInTheDocument()
    const selection = screen.getByRole('region', { name: /Selection report/ })
    expect(within(selection).getByText('Agent-reported')).toBeInTheDocument()
    expect(within(selection).getByText(/Model Unknown/)).toBeInTheDocument()
    expect(within(selection).getByText('Selected: #128, #129')).toBeInTheDocument()
    const delivery = screen.getByRole('region', { name: /Delivery report/ })
    expect(within(delivery).getByText(/PR title: Runner 详情页显示升级历史/)).toBeInTheDocument()
    expect(within(delivery).getByText(/Not done: 按状态筛选/)).toBeInTheDocument()
    const comments = within(screen.getByRole('list', { name: 'Comment intents' }))
    expect(comments.getByText('Outcome unknown; not resent automatically')).toBeInTheDocument()
    expect(comments.getByText('Claim note on #129')).toBeInTheDocument()
    expect(screen.getByRole('list', { name: 'Attempt branches' })).toHaveTextContent(/deleted once it's pushed or when retention ends at/)
    expect(screen.getByRole('region', { name: 'Design basis' })).toHaveTextContent('Work item frozen snapshot')
    const related = within(screen.getByRole('table', { name: 'Related tasks and basis' }))
    expect(related.getByText('Linked after claim')).toBeInTheDocument()
    // 依据关系的说明是服务端按原因码给的消息
    expect(related.getByText("The task didn't pin documents in its spec; once claimed, they resolve to the work item's frozen snapshot")).toBeInTheDocument()
    expect(related.getByRole('link', { name: 'Request #142' })).toHaveAttribute('href', '/ai/requests/142')
    expect(chineseLeft(document.body, rawZH)).toEqual([])
  })

  it('关联任务的依据无法解析时，关系说明和具体原因都按语言显示', () => {
    const item = fixture.Data.RelatedTasks[2]
    if (!item) throw new Error('fixture 应有第三个关联任务')
    const conflict = {
      ...item,
      Relation: 'mismatch',
      Note: { id: 'docsNote.basisConflict' },
      NoteDetail: { id: 'docsIssue.taskDevRequestMismatch', params: { task: 212, request: 141 } },
    }
    const view = renderPage(withData(envelope(fixture), { RelatedTasks: [conflict] }), '/ai-dev/14')
    expect(screen.getByText('任务的文档依据记录与工作项不一致：AI 任务 #212 的工作项与开发请求 #141 的记录不一致')).toBeInTheDocument()
    view.unmount()
    renderPage(withData(envelope(fixture), { RelatedTasks: [conflict] }), '/ai-dev/14', { locale: 'en' })
    expect(
      screen.getByText("The task's design basis record doesn't match the work item: The work item of AI task #212 doesn't match the record of development request #141"),
    ).toBeInTheDocument()
  })

  it('切换语言：页面不重新加载，标题和状态立即切换', async () => {
    renderPage(envelope(fixture), '/ai-dev/14')
    const summary = screen.getByRole('region', { name: '概要' })
    expect(within(summary).getByText('受阻')).toBeInTheDocument()
    await switchLocale('English')
    expect(screen.getByRole('region', { name: 'Summary' })).toBe(summary)
    expect(within(summary).getByText('Blocked')).toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Work item #14')
    expect(requests.filter((r) => r.url.startsWith('/ai-dev/14'))).toHaveLength(0)
  })
})
