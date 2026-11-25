import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/ai_dev.json'
import emptyFixture from '../../dev/fixtures/ai_dev.empty.json'
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

/** rows 返回工作项列表的行（受阻的 #14、已合并的 #12、无需改代码的 #9），不含行内的 Issue 列表。 */
function rows() {
  const list = screen.getByRole('list', { name: '工作项' })
  const items = within(list)
    .getAllByRole('listitem')
    .filter((li) => li.parentElement === list)
  const [blocked, merged, noChange] = items
  if (items.length !== 3 || !blocked || !merged || !noChange) throw new Error('应有 3 个工作项')
  return { blocked, merged, noChange }
}

/** posted 等待提交，返回唯一的 POST 请求的地址和字段。 */
async function posted() {
  await waitFor(() => expect(requests.some((r) => r.method === 'POST')).toBe(true))
  const post = requests.find((r) => r.method === 'POST')
  return { url: post?.url, body: new URLSearchParams(post?.body) }
}

describe('AI 开发', () => {
  it('列出工作项：状态、Issue 和完成度、PR、原因、仓库；导航高亮 AI 开发', () => {
    renderPage(envelope(fixture), '/ai-dev')
    expect(screen.getByRole('heading', { level: 1, name: 'AI 开发' })).toBeInTheDocument()
    const nav = screen.getByRole('navigation', { name: '主导航' })
    expect(within(nav).getByRole('link', { name: /AI 开发/ })).toHaveAttribute('aria-current', 'page')

    const { blocked, merged, noChange } = rows()
    expect(within(blocked).getByRole('link', { name: '工作项 #14' })).toHaveAttribute('href', '/ai-dev/14')
    expect(within(blocked).getByText('受阻')).toBeInTheDocument()
    expect(within(blocked).getByText('kiln:hold')).toBeInTheDocument()
    expect(within(blocked).getByText('需求已变')).toBeInTheDocument()
    expect(within(blocked).getByRole('link', { name: 'PR #131' })).toHaveAttribute('href', 'https://github.com/acme/kiln/pull/131')
    expect(within(blocked).getByText(/返修 2 轮后 CI 仍未通过/)).toBeInTheDocument()
    expect(within(blocked).getByText(/返修 2 \/ 2 轮/)).toBeInTheDocument()
    const issues = within(within(blocked).getByRole('list', { name: '工作项 #14 的 Issue' })).getAllByRole('listitem')
    expect(issues.map((li) => li.textContent)).toEqual(['#128 Runner 详情页显示升级历史（部分解决）', '#129 升级失败时保留旧版本的日志（完整解决）'])
    expect(within(blocked).getByRole('link', { name: 'acme/kiln' })).toHaveAttribute('href', '/ai-dev?repo=acme%2Fkiln')

    expect(within(merged).getByText('已合并')).toBeInTheDocument()
    expect(within(merged).getByText(/已关闭/)).toBeInTheDocument()
    expect(within(noChange).getByText('无需改代码')).toBeInTheDocument()
    expect(within(noChange).queryByRole('link', { name: /PR/ })).not.toBeInTheDocument()

    const paused = screen.getByRole('region', { name: '暂停了新领取的仓库' })
    expect(within(paused).getByRole('link', { name: 'acme/notes' })).toBeInTheDocument()
  })

  it('管理员：受阻的工作项可以再返修一轮，已结束的没有操作', async () => {
    renderPage(envelope(fixture), '/ai-dev')
    const { blocked, merged, noChange } = rows()
    expect(within(merged).queryByRole('button')).not.toBeInTheDocument()
    expect(within(noChange).queryByRole('button')).not.toBeInTheDocument()
    await userEvent.click(within(blocked).getByRole('button', { name: '再返修一轮' }))
    const { url, body } = await posted()
    expect(url).toBe('/ai-dev/retry')
    expect(body.get('id')).toBe('14')
    expect(body.get('csrf')).toBe('preview')
  })

  it('管理员确认后终止未结束的工作项', async () => {
    renderPage(envelope(fixture), '/ai-dev')
    await userEvent.click(within(rows().blocked).getByRole('button', { name: '终止' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText(/PR（如果有）保持原样/)).toBeInTheDocument()
    expect(within(dialog).getByText(/重新打上 kiln:ready 后可以再选/)).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('button', { name: '终止工作项' }))
    const { url, body } = await posted()
    expect(url).toBe('/ai-dev/stop')
    expect(body.get('id')).toBe('14')
  })

  it('管理员选择仓库同步任务池，恢复暂停的仓库', async () => {
    renderPage(envelope(fixture), '/ai-dev')
    expect(screen.queryByRole('region', { name: /任务池/ })).not.toBeInTheDocument()
    const form = screen.getByRole('form', { name: '同步任务池' })
    await userEvent.selectOptions(within(form).getByLabelText(/仓库/), 'acme/notes')
    await userEvent.click(within(form).getByRole('button', { name: '同步任务池' }))
    let p = await posted()
    expect(p.url).toBe('/ai-dev/sync')
    expect(p.body.get('repo')).toBe('acme/notes')

    requests = []
    const paused = screen.getByRole('region', { name: '暂停了新领取的仓库' })
    await userEvent.click(within(paused).getByRole('button', { name: '恢复领取' }))
    p = await posted()
    expect(p.url).toBe('/ai-dev/resume')
    expect(p.body.get('repo')).toBe('acme/notes')
  })

  it('只看一个仓库：管理员确认后暂停领取', async () => {
    const kiln = fixture.Data.Items.filter((it) => it.Repo === 'acme/kiln')
    renderPage(withData(envelope(fixture), { Repo: 'acme/kiln', Items: kiln, Paused: [] }), '/ai-dev?repo=acme/kiln')
    expect(screen.getByRole('heading', { level: 1, name: 'acme/kiln 的 AI 开发' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '全部仓库' })).toHaveAttribute('href', '/ai-dev')
    expect(screen.getByRole('form', { name: '同步 acme/kiln 的任务池' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '暂停领取' }))
    const dialog = await screen.findByRole('alertdialog')
    await userEvent.click(within(dialog).getByRole('button', { name: '暂停领取' }))
    const { url, body } = await posted()
    expect(url).toBe('/ai-dev/pause')
    expect(body.get('repo')).toBe('acme/kiln')
  })

  it('只看一个仓库：任务池列出 Issue 和情况，领取的链到工作项', () => {
    const pool = {
      SyncedAt: null,
      Issues: [
        { Number: 128, Title: '升级历史', URL: 'https://github.com/acme/kiln/issues/128', Status: 'claimed', WorkItem: 14, Reason: '', WaitingOn: null, ReadyAt: '2026-11-10T01:00:00Z' },
        { Number: 140, Title: '新需求', URL: '', Status: 'candidate', WorkItem: null, Reason: '', WaitingOn: null, ReadyAt: '2026-11-10T02:00:00Z' },
        { Number: 141, Title: '不清楚', URL: '', Status: 'skipped', WorkItem: null, Reason: '没有被选中：超出授权', WaitingOn: null, ReadyAt: '2026-11-10T03:00:00Z' },
        { Number: 142, Title: '后续', URL: '', Status: 'waiting', WorkItem: null, Reason: '等待 #140 关闭：依赖 #140', WaitingOn: [140], ReadyAt: '2026-11-10T04:00:00Z' },
      ],
    }
    renderPage(withData(envelope(fixture), { Repo: 'acme/kiln', Items: [], Paused: [], Pool: pool }), '/ai-dev?repo=acme/kiln')
    const region = screen.getByRole('region', { name: /任务池/ })
    expect(within(region).getByText(/还没有全量同步过/)).toBeInTheDocument()
    const items = within(within(region).getByRole('list', { name: '任务池' })).getAllByRole('listitem')
    const [claimed, candidate, skipped, waiting] = items
    if (items.length !== 4 || !claimed || !candidate || !skipped || !waiting) throw new Error('任务池应有 4 个 Issue')
    expect(within(claimed).getByRole('link', { name: '#128' })).toHaveAttribute('href', 'https://github.com/acme/kiln/issues/128')
    expect(within(claimed).getByText('已领取')).toBeInTheDocument()
    expect(within(claimed).getByRole('link', { name: '工作项 #14' })).toHaveAttribute('href', '/ai-dev/14')
    expect(within(candidate).getByText('可选')).toBeInTheDocument()
    expect(within(skipped).getByText('已跳过')).toBeInTheDocument()
    expect(within(skipped).getByText(/超出授权/)).toBeInTheDocument()
    expect(within(waiting).getByText('等待依赖')).toBeInTheDocument()
    expect(within(waiting).getByText(/等待 #140 关闭/)).toBeInTheDocument()
  })

  it('只看一个仓库：显示仓库占用（占用者、原因、排在后面的工作项）', () => {
    // 占用是结构化的数据：阶段、状态按语言显示，原因（服务端记下的原文）原样显示
    const occ = { Occupant: 14, Phase: 'implement', State: 'pr_open', Reason: 'PR 已开，等待 CI、AI 审计和合并', Waiting: [15] }
    renderPage(withData(envelope(fixture), { Repo: 'acme/kiln', Items: [], Paused: [], Occupancy: occ }), '/ai-dev?repo=acme/kiln')
    const region = screen.getByRole('region', { name: '仓库占用' })
    expect(within(region).getByRole('link', { name: '工作项 #14' })).toHaveAttribute('href', '/ai-dev/14')
    expect(within(region).getByText(/之后依次是 #15/)).toBeInTheDocument()
    expect(within(region).getByText('进行中（实现 / PR 已开：PR 已开，等待 CI、AI 审计和合并）；同一个仓库按工作项串行，它合并或终止之后才开始下一项')).toBeInTheDocument()
  })

  it('只看一个仓库：没有被占用时说明可以开始下一项；看全部仓库时没有占用', () => {
    renderPage(
      withData(envelope(fixture), { Repo: 'acme/kiln', Items: [], Paused: [], Occupancy: { Occupant: 0, Phase: '', State: '', Reason: '', Waiting: null } }),
      '/ai-dev?repo=acme/kiln',
    )
    expect(within(screen.getByRole('region', { name: '仓库占用' })).getByText(/没有进行中的工作项/)).toBeInTheDocument()
  })

  it('全部仓库时不显示仓库占用；冲突返修与审计返修分开显示轮次', () => {
    const base = fixture.Data.Items[0]
    if (!base) throw new Error('fixture 没有工作项')
    renderPage(withData(envelope(fixture), { Items: [{ ...base, FixRounds: 1, ConflictRounds: 2 }] }), '/ai-dev')
    expect(screen.queryByRole('region', { name: '仓库占用' })).not.toBeInTheDocument()
    expect(screen.getByText(/返修 1 \/ 2 轮 · 冲突返修 2 \/ 2 轮/)).toBeInTheDocument()
  })

  it('先设计的工作项显示阶段；部分完成后重试开始下一轮实现', () => {
    const base = fixture.Data.Items[0]
    if (!base) throw new Error('fixture 没有工作项')
    const design = { ...base, ID: 21, URL: '/ai-dev/21', Phase: 'design', DesignFirst: true, State: 'pr_open', Retryable: false }
    const next = { ...base, ID: 22, URL: '/ai-dev/22', DesignFirst: true, DesignPR: 30, ImplRound: 2, State: 'blocked', Retryable: true, Active: true, NextRound: true }
    renderPage(withData(envelope(fixture), { Items: [design, next] }), '/ai-dev')
    const list = screen.getByRole('list', { name: '工作项' })
    const [d, n] = within(list)
      .getAllByRole('listitem')
      .filter((li) => li.parentElement === list)
    if (!d || !n) throw new Error('应有 2 个工作项')
    expect(within(d).getByText('设计')).toBeInTheDocument()
    expect(within(n).getByText('实现第 2 轮')).toBeInTheDocument()
    expect(within(n).getByRole('button', { name: '开始下一轮实现' })).toBeInTheDocument()
    expect(within(n).queryByRole('button', { name: '再返修一轮' })).not.toBeInTheDocument()
  })

  it('任务池是空的时说明怎样加入', () => {
    renderPage(
      withData(envelope(fixture), { Repo: 'acme/kiln', Items: [], Paused: [], Pool: { SyncedAt: '2026-11-10T01:00:00Z', Issues: null } }),
      '/ai-dev?repo=acme/kiln',
    )
    const region = screen.getByRole('region', { name: /任务池/ })
    expect(within(region).getByText('任务池是空的')).toBeInTheDocument()
    expect(within(region).getByText(/上次全量同步/)).toBeInTheDocument()
  })

  it('暂停中的仓库不显示暂停按钮，显示恢复', () => {
    renderPage(withData(envelope(fixture), { Repo: 'acme/notes', Items: [], Paused: ['acme/notes'] }), '/ai-dev?repo=acme/notes')
    expect(screen.queryByRole('button', { name: '暂停领取' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '恢复领取' })).toBeInTheDocument()
  })

  it('成员只能查看：没有同步、重试、终止和恢复', () => {
    renderPage(withData(envelope(fixture, member), { Repos: null }), '/ai-dev')
    expect(screen.getByRole('list', { name: '工作项' })).toBeInTheDocument()
    expect(screen.queryByRole('form', { name: '同步任务池' })).not.toBeInTheDocument()
    for (const name of ['再返修一轮', '终止', '恢复领取', '同步任务池']) {
      expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
    }
  })

  it('没有工作项时说明怎样开始', () => {
    renderPage(envelope(emptyFixture), '/ai-dev')
    expect(screen.getByText('还没有工作项')).toBeInTheDocument()
    expect(screen.getByText(/webhook 丢失时可以在上面全量同步任务池/)).toHaveTextContent('给 Issue 加上 label kiln:ready')
    expect(screen.queryByRole('region', { name: '暂停了新领取的仓库' })).not.toBeInTheDocument()
  })
})

describe('AI 开发（英文与切换语言）', () => {
  const rawZH = rawStrings(fixture.Data)

  it('英文：工作项的状态、阶段、轮次、表单和说明按英文显示；Issue 标题和原因原样显示', () => {
    const base = fixture.Data.Items[0]
    if (!base) throw new Error('fixture 没有工作项')
    const items = [{ ...base, FixRounds: 1, ConflictRounds: 2, DesignFirst: true, ImplRound: 2 }, ...fixture.Data.Items.slice(1)]
    renderPage(withData(envelope(fixture), { Items: items }), '/ai-dev', { locale: 'en' })
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('AI development')
    const list = screen.getByRole('list', { name: 'Work item' })
    expect(within(list).getByRole('link', { name: 'Work item #14' })).toBeInTheDocument()
    expect(within(list).getByText('Blocked')).toBeInTheDocument()
    expect(within(list).getByText('Implementation round 2')).toBeInTheDocument()
    expect(within(list).getByText(/Fix rounds 1 \/ 2 · Conflict fix rounds 2 \/ 2/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Run another fix round' })).toBeInTheDocument()
    expect(screen.getByRole('form', { name: 'Sync task pool' })).toBeInTheDocument()
    expect(chineseLeft(document.body, rawZH)).toEqual([])
  })

  it('英文：只看一个仓库时任务池、占用和暂停按英文显示，占用的阶段和状态按语言组合', () => {
    const occ = { Occupant: 14, Phase: 'implement', State: 'pr_open', Reason: 'PR 已开，等待 CI、AI 审计和合并', Waiting: [15] }
    const pool = {
      Issues: [{ Number: 3, Title: '新需求', URL: '', Status: 'waiting', WorkItem: null, Reason: '等 #2 关闭', WaitingOn: [2], ReadyAt: '2026-11-10T08:00:00Z' }],
      SyncedAt: null,
    }
    renderPage(withData(envelope(fixture), { Repo: 'acme/kiln', Items: [], Paused: [], Occupancy: occ, Pool: pool }), '/ai-dev?repo=acme/kiln', { locale: 'en' })
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('AI development for acme/kiln')
    const region = screen.getByRole('region', { name: 'Repository occupancy' })
    expect(within(region).getByRole('link', { name: 'Work item #14' })).toHaveAttribute('href', '/ai-dev/14')
    expect(within(region).getByText(/occupies this repository, followed by #15/)).toBeInTheDocument()
    expect(within(region).getByText(/^In progress \(Implementation \/ PR open: PR 已开，等待 CI、AI 审计和合并\)/)).toBeInTheDocument()
    const poolBox = screen.getByRole('region', { name: /Task pool/ })
    expect(within(poolBox).getByText('Waiting on dependencies')).toBeInTheDocument()
    expect(within(poolBox).getByText(/Never fully synced/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Pause pickup' })).toBeInTheDocument()
    expect(chineseLeft(document.body, [...rawZH, occ.Reason, '新需求', '等 #2 关闭'])).toEqual([])
  })

  it('英文：没有工作项时说明怎样开始', () => {
    renderPage(envelope(emptyFixture), '/ai-dev', { locale: 'en' })
    expect(screen.getByText('No work items yet')).toBeInTheDocument()
    expect(chineseLeft(document.body, rawStrings(emptyFixture.Data))).toEqual([])
  })

  it('终止的确认对话框打开时切换语言：对话框保持打开，文字立即切换', async () => {
    renderPage(envelope(fixture), '/ai-dev')
    await userEvent.click(within(rows().blocked).getByRole('button', { name: '终止' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText('终止工作项 #14？')).toBeInTheDocument()
    await switchLocale('English')
    expect(screen.getByRole('alertdialog')).toBe(dialog)
    expect(within(dialog).getByText('Stop work item #14?')).toBeInTheDocument()
    expect(within(dialog).getByText(/label them kiln:ready again/)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: 'Stop work item' })).toBeInTheDocument()
  })
})
