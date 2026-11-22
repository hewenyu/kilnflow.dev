import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/ai_runners.json'
import detailFixture from '../../dev/fixtures/ai_runner.json'
import { clock } from '../lib/format'
import { envelope, renderPage, withData } from '../test/render'
import { shortAgentVersion } from './ai/runner'

// AI 执行节点列表和详情页。

let posts: { url: string; body: string }[] = []

beforeEach(() => {
  posts = []
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input)
      if (init?.method === 'POST') posts.push({ url, body: init.body instanceof URLSearchParams ? init.body.toString() : '' })
      return Promise.resolve(new Response(JSON.stringify({ Redirect: '/ai/runners?flash=ok' }), { headers: { 'Content-Type': 'application/json' } }))
    }),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
})

/** closestLi 返回 el 所在的列表项。 */
const closestLi = (el: HTMLElement) => {
  const li = el.closest('li')
  if (!li) throw new Error('不在列表项中')
  return li
}

const rowEl = (name: string) => closestLi(screen.getByRole('heading', { level: 3, name }))
const row = (name: string) => within(rowEl(name))

/** dd 返回 container 中 dt 为 term 的那一项的内容。 */
const dd = (container: HTMLElement, term: string) => {
  const el = within(container).getByText(term, { selector: 'dt' }).nextElementSibling
  if (!(el instanceof HTMLElement)) throw new Error(`没有 ${term} 的内容`)
  return el
}

describe('AI 执行节点列表', () => {
  it('角色、连接状态、并发、Orca / Docker、Agent 和正在执行的 AI 任务', () => {
    renderPage(envelope(fixture), '/ai/runners')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('AI 执行节点')
    const r = row('ai-node-01')
    expect(r.getByRole('link', { name: 'ai-node-01' })).toHaveAttribute('href', '/ai/runners/ai-node-01')
    expect(r.getByText('AI 审计节点')).toBeInTheDocument()
    expect(r.getByText('在线')).toBeInTheDocument()
    // 并发是正在执行的任务数 / 上限；Orca 就绪时显示版本，运行实例在 title 中
    expect(dd(rowEl('ai-node-01'), '并发')).toHaveTextContent(/^1 \/ 2$/)
    expect(dd(rowEl('ai-node-01'), 'Orca')).toHaveTextContent(/^就绪 1\.4\.2$/)
    expect(r.getByText('1.4.2')).toHaveAttribute('title', 'runtime rt-7f3a9c')
    expect(dd(rowEl('ai-node-01'), 'kiln-orca')).toHaveTextContent('0.3.0')
    expect(r.queryByText('Herdr')).not.toBeInTheDocument()
    expect(r.getByText('不可用（docker 没有运行）')).toBeInTheDocument()
    expect(r.getByText('0.58.0')).toHaveAttribute('title', 'codex-cli 0.58.0')
    expect(r.getByText('不可用（没有安装）')).toBeInTheDocument()
    expect(r.getByRole('link', { name: 'codex · macos-arm64' })).toHaveAttribute('href', '/ai/tasks/530')
    expect(r.getByRole('link', { name: 'AI 审计请求 #92' })).toHaveAttribute('href', '/ai/requests/92')
    // 任务的进度和在 Orca 中的关联：dispatch、worktree（名称，完整路径在 title 中）
    const task = within(closestLi(r.getByRole('link', { name: 'codex · macos-arm64' })))
    expect(task.getByText('执行中：运行 go test ./...')).toBeInTheDocument()
    expect(task.getByText('dsp-530-1')).toBeInTheDocument()
    expect(task.getByText('kiln-a41-e1')).toHaveAttribute('title', '/Users/me/orca/workspaces/kiln/kiln-a41-e1')
    const d = row('mini-dev')
    expect(d.getByText('AI 开发节点')).toBeInTheDocument()
    expect(dd(rowEl('mini-dev'), '协议')).toHaveTextContent(/协议版本过旧.*npm install -g @yuebanlaosiji\/kilnflow-orca@latest/)
    expect(dd(rowEl('mini-dev'), '并发')).toHaveTextContent('未上报')
    expect(dd(rowEl('mini-dev'), 'Orca')).toHaveTextContent(/^未上报$/)
    expect(d.getByText('离线，重新连接后才会领取任务。')).toBeInTheDocument()
    expect(screen.getByText(/共 2 个：1 在线/)).toBeInTheDocument()
  })

  it('Orca 不可用时显示原因；没有上报进度和关联时不显示那一行', () => {
    const [a, b] = fixture.Data.Runners
    const task = { ...a?.Tasks[0], Orca: {}, Stage: '', Text: '' }
    renderPage(
      withData(envelope(fixture), { Runners: [{ ...a, OrcaState: 'unavailable', OrcaReason: 'orca status 不可达', OrcaVersion: '', Tasks: [task] }, b] }),
      '/ai/runners',
    )
    const r = row('ai-node-01')
    expect(dd(rowEl('ai-node-01'), 'Orca')).toHaveTextContent(/^不可用（orca status 不可达）$/)
    expect(r.queryByText(/Orca：/)).not.toBeInTheDocument()
    expect(r.queryByText(/执行中/)).not.toBeInTheDocument()
  })

  it('Agent 因为失败暂停使用时显示什么时候恢复，不把未来的时间显示成“0 秒前”', () => {
    const [a, b] = fixture.Data.Runners
    const until = '2099-01-02T03:04:05Z'
    const agents = [{ ...a?.Agents[0] }, { ...a?.Agents[1], State: 'backoff', Reason: '连续失败', BackoffUntil: until }]
    renderPage(withData(envelope(fixture), { Runners: [{ ...a, Agents: agents }, b] }), '/ai/runners')
    const text = dd(rowEl('ai-node-01'), 'Agent').textContent
    expect(text).toContain(`暂停使用（连续失败），预计 ${clock(until)} 恢复`)
    expect(text).not.toContain('秒前')
  })

  it('只有暂停使用（backoff）且上报了恢复时间时才显示预计恢复：其它状态残留的时间、零值时间都不显示', () => {
    const [a, b] = fixture.Data.Runners
    const until = '2099-01-02T03:04:05Z'
    const agents = [
      { ...a?.Agents[0], State: 'unavailable', Reason: '没有安装', BackoffUntil: until },
      { ...a?.Agents[1], State: 'backoff', Reason: '连续失败', BackoffUntil: '0001-01-01T00:00:00Z' },
    ]
    renderPage(withData(envelope(fixture), { Runners: [{ ...a, Agents: agents }, b] }), '/ai/runners')
    const text = dd(rowEl('ai-node-01'), 'Agent').textContent
    expect(text).toContain('不可用（没有安装）')
    expect(text).toContain('暂停使用（连续失败）')
    expect(text).not.toContain('恢复')
  })

  // 状态由节点上报，可以是任意字符串：与对象原型上的属性同名时也只是不认识的状态，原样显示，页面照常渲染
  it.each(['__proto__', 'constructor', 'toString'])('上报的状态是 %s 时原样显示', (state) => {
    const [a, b] = fixture.Data.Runners
    const agents = [{ ...a?.Agents[1], State: state, Reason: '' }]
    renderPage(
      withData(envelope(fixture), { Runners: [{ ...a, OrcaState: state, OrcaReason: '', OrcaVersion: '', DockerState: state, DockerReason: '', Agents: agents }, b] }),
      '/ai/runners',
    )
    expect(dd(rowEl('ai-node-01'), 'Orca')).toHaveTextContent(new RegExp(`^${state}$`))
    expect(dd(rowEl('ai-node-01'), 'Docker')).toHaveTextContent(new RegExp(`^${state}$`))
    expect(dd(rowEl('ai-node-01'), 'Agent')).toHaveTextContent(new RegExp(`^Claude Code.*${state}$`))
  })

  it('暂停接单先确认，提交到与 CI Runner 相同的地址', async () => {
    const user = userEvent.setup()
    renderPage(envelope(fixture), '/ai/runners')
    await user.click(row('ai-node-01').getByRole('button', { name: '暂停接单…' }))
    const dialog = within(await screen.findByRole('alertdialog'))
    expect(dialog.getByText(/正在执行的 1 个 AI 任务继续执行到结束/)).toBeInTheDocument()
    await user.click(dialog.getByRole('button', { name: '确认暂停接单' }))
    expect(posts[0]?.url).toBe('/runners/ai-node-01/drain')
  })

  it('已吊销的 AI 执行节点重新注册时带上角色；有任务时不能删除', () => {
    const [a, b] = fixture.Data.Runners
    renderPage(withData(envelope(fixture), { Runners: [{ ...a, Revoked: true, ConnLabel: '已吊销' }, { ...b, Revoked: true, ConnLabel: '已吊销' }] }), '/ai/runners')
    expect(row('mini-dev').getByRole('link', { name: '重新注册…' })).toHaveAttribute('href', '/runners/new?name=mini-dev&kind=ai-dev')
    expect(row('ai-node-01').getByRole('link', { name: '重新注册…' })).toHaveAttribute('href', '/runners/new?name=ai-node-01&kind=ai')
    expect(row('ai-node-01').getByText('1 个 AI 任务结束后可以删除')).toBeInTheDocument()
    expect(row('mini-dev').getByRole('button', { name: '删除…' })).toBeInTheDocument()
    expect(row('ai-node-01').queryByRole('button', { name: '暂停接单…' })).not.toBeInTheDocument()
  })

  it('吊销前说明 kiln-orca 会退出，恢复要重新执行 kiln-orca enroll', async () => {
    const user = userEvent.setup()
    renderPage(envelope(fixture), '/ai/runners')
    await user.click(row('ai-node-01').getByRole('button', { name: '吊销…' }))
    const dialog = within(await screen.findByRole('alertdialog'))
    expect(dialog.getByText(/kiln-orca 会退出/)).toBeInTheDocument()
    expect(dialog.getByText('kiln-orca enroll')).toBeInTheDocument()
    expect(dialog.queryByText(/kiln-ai-runner/)).not.toBeInTheDocument()
  })

  it('成员看不到操作；没有 AI 执行节点时说明怎样注册；没有接单的提醒任务会排队', () => {
    const view = renderPage(envelope(fixture, { Session: { Login: 'bob', Admin: false, ViaToken: false, Bound: true } }), '/ai/runners')
    expect(screen.queryByRole('button', { name: '暂停接单…' })).not.toBeInTheDocument()
    view.unmount()
    const v2 = renderPage(withData(envelope(fixture), { Runners: [], Counts: { ...fixture.Data.Counts, Total: 0 } }), '/ai/runners')
    expect(screen.getByText('还没有 AI 执行节点')).toBeInTheDocument()
    v2.unmount()
    renderPage(withData(envelope(fixture), { Counts: { ...fixture.Data.Counts, Accepting: 0 } }), '/ai/runners')
    expect(screen.getByText('没有在线并接单的 AI 执行节点')).toBeInTheDocument()
  })

  it('Agent 版本取 --version 输出中的版本号', () => {
    expect(shortAgentVersion('2.1.32 (Claude Code)')).toBe('2.1.32')
    expect(shortAgentVersion('nightly')).toBe('nightly')
  })
})

describe('AI 执行节点详情页', () => {
  it('概况、Orca 和任务的关联，没有按台升级（连接器用 npm 升级），导航高亮 AI 执行节点', () => {
    renderPage(envelope(detailFixture), '/ai/runners/ai-node-01')
    const nav = screen.getByRole('navigation', { name: '主导航' })
    expect(within(nav).getByRole('link', { name: /AI 执行节点/ })).toHaveAttribute('aria-current', 'page')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('ai-node-01')
    const region = screen.getByRole('region', { name: '概况' })
    const summary = within(region)
    expect(summary.getByText('AI 审计节点')).toBeInTheDocument()
    expect(dd(region, '并发')).toHaveTextContent(/^1 \/ 2$/)
    expect(dd(region, 'Orca')).toHaveTextContent('就绪')
    expect(summary.getByRole('link', { name: 'codex · macos-arm64' })).toHaveAttribute('href', '/ai/tasks/530')
    expect(summary.getByText('dsp-530-1')).toBeInTheDocument()
    expect(summary.getByText(/npm install -g @yuebanlaosiji\/kilnflow-orca@latest/)).toBeInTheDocument()
    // 没有升级部分、升级记录和升级表单
    expect(screen.queryByRole('region', { name: '升级' })).not.toBeInTheDocument()
    expect(screen.queryByRole('region', { name: '升级记录' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /升级到|检查更新|取消升级/ })).not.toBeInTheDocument()
    expect(document.querySelector('form[action^="/runners/upgrade/"]')).toBeNull()
  })
})
