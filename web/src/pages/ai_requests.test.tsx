import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import fixture from '../../dev/fixtures/ai_requests.json'
import requestFixture from '../../dev/fixtures/ai_request.json'
import { envelope, renderPage, withData } from '../test/render'
import { aiRequestsURL } from './ai_requests'

// AI 请求列表和 AI 请求详情页。

describe('AI 请求列表', () => {
  it('列出请求：类型、状态、对象（PR、工作项）和重跑来源；导航高亮 AI 请求', () => {
    renderPage(envelope(fixture), '/ai/requests')
    const nav = screen.getByRole('navigation', { name: '主导航' })
    expect(within(nav).getByRole('link', { name: /AI 请求/ })).toHaveAttribute('aria-current', 'page')
    const table = within(screen.getByRole('table', { name: 'AI 请求' }))
    const review = table.getByRole('link', { name: 'AI 审计请求 #88' })
    expect(review).toHaveAttribute('href', '/ai/requests/88')
    expect(review.closest('tr')).toHaveTextContent('失败')
    expect(review.closest('tr')).toHaveTextContent('PR #74')
    const dev = table.getByRole('link', { name: 'AI 开发请求 #90' }).closest('tr')
    if (!(dev instanceof HTMLElement)) throw new Error('没有开发请求的行')
    expect(within(dev).getByRole('link', { name: '工作项 #4' })).toHaveAttribute('href', '/ai-dev/4')
    expect(within(dev).getByText('进行中')).toBeInTheDocument()
    expect(table.getByRole('link', { name: 'AI 审计请求 #91' }).closest('td')).toHaveTextContent('（重跑 #88）')
  })

  it('筛选地址与服务端一致；筛选后可以清空', () => {
    expect(aiRequestsURL({ Repo: '', Type: '', State: '', PR: 0 })).toBe('/ai/requests')
    expect(aiRequestsURL({ Repo: 'acme/kiln', Type: 'review', State: 'failed', PR: 74 })).toBe('/ai/requests?repo=acme%2Fkiln&type=review&state=failed&pr=74')
    renderPage(withData(envelope(fixture), { Filter: { Repo: '', Type: 'develop', State: '', PR: 0 }, Requests: [] }), '/ai/requests?type=develop')
    expect(screen.getByText('没有符合筛选条件的 AI 请求')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /类型：AI 开发/ })).toBeInTheDocument()
  })

  it('达到条数上限时提示还有更早的请求；没有请求时说明怎样触发', () => {
    const view = renderPage(withData(envelope(fixture), { Limit: 3 }), '/ai/requests')
    expect(screen.getByText('只列出最近的 3 个，更早的请用筛选条件查找。')).toBeInTheDocument()
    view.unmount()
    renderPage(withData(envelope(fixture), { Requests: [] }), '/ai/requests')
    expect(screen.getByText('还没有 AI 请求')).toBeInTheDocument()
    expect(screen.queryByText(/只列出最近的/)).not.toBeInTheDocument()
  })
})

describe('AI 请求详情页', () => {
  it('信息、各平台的任务和重跑记录', () => {
    renderPage(envelope(requestFixture), '/ai/requests/88')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('AI 审计请求 #88')
    expect(screen.getByRole('link', { name: '#74' })).toHaveAttribute('href', 'https://github.com/acme/kiln/pull/74')
    const tasks = within(screen.getByRole('table', { name: 'AI 任务' }))
    const codex = tasks.getByRole('link', { name: 'codex · macos-arm64' })
    expect(codex).toHaveAttribute('href', '/ai/tasks/520')
    expect(codex.closest('td')).toHaveTextContent('审计发现问题：2 个')
    expect(codex.closest('tr')).toHaveTextContent('ai-node-01')
    expect(codex.closest('tr')).toHaveTextContent('darwin/arm64')
    // 一组平台的任务：显示实际执行的平台和节点
    const group = tasks.getByRole('link', { name: 'codex · any' }).closest('tr')
    expect(group).toHaveTextContent('linux/amd64')
    expect(group).toHaveTextContent('linux-ai')
    expect(group).not.toHaveTextContent('darwin/arm64')
    const family = within(screen.getByRole('region', { name: '同一个请求的重跑' }))
    expect(family.getByRole('link', { name: '#91' })).toHaveAttribute('href', '/ai/requests/91')
    expect(family.queryByRole('link', { name: '#88' })).not.toBeInTheDocument()
  })

  it('还没有领取的一组平台的任务显示可以执行的平台', () => {
    const data = structuredClone(requestFixture)
    const t = data.Data.Tasks[1]
    if (!t) throw new Error('fixture 中没有第二个任务')
    Object.assign(t, { State: 'pending', StateTitle: '待领取', Platform: '', Runner: '', Started: null, Finished: null })
    renderPage(envelope(data), '/ai/requests/88')
    const row = within(screen.getByRole('table', { name: 'AI 任务' })).getByRole('link', { name: 'codex · any' }).closest('tr')
    expect(row).toHaveTextContent('darwin/arm64 或 linux/amd64')
  })

  it('已结束的审计请求：可以重跑失败的或全部；全部成功时只能全部重跑', () => {
    const view = renderPage(envelope(requestFixture), '/ai/requests/88')
    expect(screen.getByRole('button', { name: '重跑失败的任务…' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '全部重跑…' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '取消请求…' })).not.toBeInTheDocument()
    view.unmount()
    renderPage(withData(envelope(requestFixture), { HasFailed: false }), '/ai/requests/88')
    expect(screen.queryByRole('button', { name: '重跑失败的任务…' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '全部重跑…' })).toBeInTheDocument()
  })

  it('进行中的请求可以取消，不能重跑；配置错误单独显示；成员（非管理员）也可以取消、重跑', () => {
    const req = requestFixture.Data.Request
    const view = renderPage(
      withData(envelope(requestFixture), { Terminal: false, CanRerun: false, Request: { ...req, State: 'active', ConfigError: 'agent 写错了' } }),
      '/ai/requests/88',
    )
    expect(screen.getByRole('button', { name: '取消请求…' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '全部重跑…' })).not.toBeInTheDocument()
    expect(screen.getByText('agent 写错了')).toBeInTheDocument()
    view.unmount()
    const member = { Session: { Login: 'bob', Admin: false, ViaToken: false, Bound: true } }
    const view2 = renderPage(envelope(requestFixture, member), '/ai/requests/88')
    expect(screen.getByRole('button', { name: '全部重跑…' })).toBeInTheDocument()
    view2.unmount()
    renderPage(withData(envelope(requestFixture, member), { Terminal: false, CanRerun: false }), '/ai/requests/88')
    expect(screen.getByRole('button', { name: '取消请求…' })).toBeInTheDocument()
  })
})
