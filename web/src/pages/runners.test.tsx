import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/runners.json'
import emptyFixture from '../../dev/fixtures/runners.empty.json'
import noAcceptFixture from '../../dev/fixtures/runners.noaccept.json'
import offlineFixture from '../../dev/fixtures/runners.offline.json'
import { envelope, renderPage, withData } from '../test/render'
import { runnersURL } from './runners'

let posts: { url: string; body: string }[] = []

beforeEach(() => {
  posts = []
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input)
      if (init?.method === 'POST') posts.push({ url, body: init.body instanceof URLSearchParams ? init.body.toString() : '' })
      return Promise.resolve(new Response(JSON.stringify({ Redirect: '/runners?flash=ok' }), { headers: { 'Content-Type': 'application/json' } }))
    }),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
})

const row = (name: string) => {
  const h = screen.getByRole('heading', { level: 3, name })
  const li = h.closest('li')
  if (!li) throw new Error(`没有 ${name} 的行`)
  return within(li)
}

describe('Runner 列表', () => {
  it('只列出 CI Runner，说明中链接到 AI 执行节点页面', () => {
    renderPage(envelope(fixture), '/runners')
    const header = screen.getByText(/执行 job 的机器（kiln-runner）/)
    expect(within(header).getByRole('link', { name: 'AI 执行节点' })).toHaveAttribute('href', '/ai/runners')
    expect(row('mini-01').queryByText('Agent')).not.toBeInTheDocument()
  })

  it('显示统计、连接和接单状态、正在执行的任务', () => {
    renderPage(envelope(fixture), '/runners')
    const counts = screen.getByRole('list', { name: '统计（点击按状态筛选）' })
    expect(within(counts).getByRole('link', { name: '2 在线' })).toHaveAttribute('href', '/runners?state=online')
    expect(within(counts).getByRole('link', { name: '3 个任务在执行' })).toHaveAttribute('href', '/runners?state=busy')
    const mini = row('mini-01')
    expect(mini.getByText('在线')).toBeInTheDocument()
    expect(mini.getByText('接单中')).toBeInTheDocument()
    expect(mini.getByRole('link', { name: 'build (darwin/arm64)' })).toHaveAttribute('href', '/jobs/9812')
    expect(row('mini-02').getByText('已暂停接单，等 1 个任务执行完。')).toBeInTheDocument()
    expect(row('linux-01').getByText('离线，重新连接后才会领取任务。')).toBeInTheDocument()
    expect(screen.getByText(/共 4 台，更新于/)).toHaveTextContent(/共 4 台，更新于 \d\d:\d\d:\d\d，每 5 秒自动更新。/)
  })

  it('版本号只在缩短时显示复制按钮', () => {
    renderPage(envelope(fixture), '/runners')
    expect(row('mini-02').getByText('0f3c9a7d21e8')).toHaveAttribute('title', '0f3c9a7d21e84b6c55aa90d1e3f7b2c4a8d6e0f1')
    expect(row('mini-02').getByRole('button', { name: '复制 mini-02 的完整版本号' })).toBeInTheDocument()
    expect(row('mini-01').queryByRole('button', { name: /完整版本号/ })).not.toBeInTheDocument()
  })

  it('已吊销的 Runner 没有暂停和吊销按钮，只能重新注册或删除', () => {
    renderPage(envelope(fixture), '/runners')
    const old = row('mac-02')
    expect(old.queryByRole('button', { name: /暂停接单|吊销/ })).not.toBeInTheDocument()
    expect(old.getByRole('link', { name: '重新注册…' })).toHaveAttribute('href', '/runners/new?name=mac-02&pool=default')
    expect(old.getByRole('button', { name: '删除…' })).toBeInTheDocument()
  })

  it('删除已吊销的 Runner 前说明影响，确认后带上筛选条件提交', async () => {
    renderPage(envelope(fixture), '/runners?state=revoked')
    await userEvent.click(row('mac-02').getByRole('button', { name: '删除…' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText('删除 mac-02？')).toBeInTheDocument()
    expect(within(dialog).getByText(/job 页面仍显示 Runner 名称 mac-02/)).toBeInTheDocument()
    expect(within(dialog).getByText(/可以重新注册/)).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('button', { name: '确认删除 mac-02' }))
    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0]?.url).toBe('/runners/mac-02/delete')
  })

  it('已吊销但还有未结束任务的 Runner 暂时不能删除', () => {
    const runners = fixture.Data.Runners.map((r) => (r.Name === 'mac-02' ? { ...r, Jobs: fixture.Data.Runners[0]?.Jobs ?? [] } : r))
    renderPage(withData(envelope(fixture), { Runners: runners }), '/runners')
    const old = row('mac-02')
    expect(old.queryByRole('button', { name: '删除…' })).not.toBeInTheDocument()
    expect(old.getByText('2 个任务结束后可以删除')).toBeInTheDocument()
  })

  it('成员看不到注册入口和操作按钮', () => {
    const env = envelope(fixture, { Session: { Login: 'lin-design', Admin: false, ViaToken: false, Bound: false } })
    renderPage(env, '/runners')
    expect(screen.queryByRole('link', { name: /注册新的 Runner/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Runner 升级' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /暂停接单|吊销|恢复接单/ })).not.toBeInTheDocument()
  })

  it('吊销前在对话框中说明后果，确认后带上筛选条件提交', async () => {
    renderPage(envelope(fixture), '/runners')
    await userEvent.click(row('mini-01').getByRole('button', { name: '吊销…' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText('吊销 mini-01？')).toBeInTheDocument()
    expect(within(dialog).getByText(/正在执行的 2 个任务（build \(darwin\/arm64\)、test）会被中断/)).toBeInTheDocument()
    expect(within(dialog).getByText(/吊销不能撤销/)).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('button', { name: '确认吊销 mini-01' }))
    await waitFor(() => expect(posts).toHaveLength(1))
    expect(posts[0]?.url).toBe('/runners/mini-01/revoke')
    const body = new URLSearchParams(posts[0]?.body)
    expect(body.get('state')).toBe('')
    expect(body.get('pool')).toBe('')
    expect(body.get('csrf')).toBe('preview')
  })

  it('暂停接单先确认；恢复接单直接提交', async () => {
    renderPage(envelope(fixture), '/runners')
    await userEvent.click(row('mini-01').getByRole('button', { name: '暂停接单…' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText('正在执行的 2 个任务继续执行到结束，不会被中断。')).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(posts).toHaveLength(0)
    await userEvent.click(row('mini-02').getByRole('button', { name: '恢复接单' }))
    await waitFor(() => expect(posts[0]?.url).toBe('/runners/mini-02/undrain'))
  })

  it('显示规格、已分配 / 可分配和 executor 上限，旧版 Runner 显示未上报', () => {
    renderPage(envelope(fixture), '/runners')
    const mini = row('mini-01')
    expect(mini.getByText('10 核 / 16.0 GiB')).toBeInTheDocument()
    expect(mini.getByText('2 核 / 2.0 GiB / 8 核 / 12.0 GiB（oci 上限 8 核 / 7.7 GiB）')).toHaveAttribute(
      'title',
      '预留 2 核 / 4.0 GiB，最多同时执行 4 个 job',
    )
    expect(row('linux-01').getByText('2 核 / 1.9 GiB')).toBeInTheDocument()
    const old = row('mac-02')
    expect(old.getByText('未上报（旧版 kiln-runner）')).toBeInTheDocument()
    expect(old.queryByText('已分配 / 可分配')).not.toBeInTheDocument()
  })

  it('名称链接到详情页；升级状态：进行中、上一次失败的原因、不能按台升级的原因', () => {
    renderPage(envelope(fixture), '/runners')
    expect(row('mini-01').getByRole('link', { name: 'mini-01' })).toHaveAttribute('href', '/runners/detail/mini-01')
    expect(row('mini-01').getByRole('link', { name: 'mini-01 的升级：正在下载：0.4.3' })).toHaveAttribute('href', '/runners/detail/mini-01')
    const m2 = row('mini-02')
    expect(m2.getByText('升级到 0.4.3 失败')).toBeInTheDocument()
    expect(m2.getByText('0.4.3 连续 3 次启动后没有连上 Server，已回滚到 0.4.2')).toBeInTheDocument()
    const linux = row('linux-01')
    expect(linux.getByText('不能按台升级')).toBeInTheDocument()
    expect(linux.getByText(/没有经过启动器运行（前台 run/)).toBeInTheDocument()
    // 已吊销的不显示升级状态
    expect(row('mac-02').queryByText('升级')).not.toBeInTheDocument()
    expect(row('mac-02').queryByText('Runner 已吊销')).not.toBeInTheDocument()
  })

  it('可以升级、已是最新版本', () => {
    const runners = fixture.Data.Runners.map((r) => {
      if (r.Name === 'mini-01') return { ...r, Upgrade: { ...r.Upgrade, State: 'idle', Request: null, CanUpgrade: true, Reason: '' } }
      if (r.Name === 'mini-02') return { ...r, Version: 'v0.4.3', Upgrade: { ...r.Upgrade, CanUpgrade: false, Reason: '已是最新版本' } }
      return r
    })
    renderPage(withData(envelope(fixture), { Runners: runners }), '/runners')
    expect(row('mini-01').getByText('可以升级到 0.4.3')).toBeInTheDocument()
    expect(row('mini-02').getByText('已是最新版本')).toBeInTheDocument()
    // 已经升级成功后不再显示上一次失败的原因
    expect(row('mini-02').queryByText(/连续 3 次启动/)).not.toBeInTheDocument()
  })

  it('管理员有 Runner 升级设置的入口，成员没有', () => {
    renderPage(envelope(fixture), '/runners')
    expect(screen.getByRole('link', { name: 'Runner 升级' })).toHaveAttribute('href', '/settings/runner-upgrade')
  })

  it('本地缓存显示总大小并链接到缓存页', () => {
    renderPage(envelope(fixture), '/runners')
    expect(row('mini-01').getByRole('link', { name: 'mini-01 的本地缓存' })).toHaveTextContent('3.2 GiB')
    expect(row('mini-01').getByRole('link', { name: 'mini-01 的本地缓存' })).toHaveAttribute('href', '/runners/cache/mini-01')
    expect(row('mac-02').getByRole('link', { name: 'mac-02 的本地缓存' })).toHaveTextContent('未上报')
  })

  it('说明中解释规格是静态的、调度按声明的资源记账', async () => {
    renderPage(envelope(fixture), '/runners')
    const summary = screen.getByText('状态、各列和操作是什么意思')
    await userEvent.click(summary)
    const help = summary.closest('details')
    expect(help).toHaveAttribute('open')
    expect(help).toHaveTextContent('这是静态规格，不是实时占用')
    expect(help).toHaveTextContent('调度按声明的资源记账，已分配加上新 job 的声明不会超过这个容量')
    expect(help).toHaveTextContent('点击查看清单、上一次清理的结果')
  })

  it('没有接单的 Runner 时提醒任务会排队', () => {
    renderPage(envelope(noAcceptFixture), '/runners')
    expect(screen.getByText('没有在线并接单的 Runner')).toBeInTheDocument()
    expect(screen.getByText('新的任务会一直排队，直到有 Runner 上线或恢复接单。')).toBeInTheDocument()
  })

  it('筛选后说明条件和数量', () => {
    renderPage(envelope(offlineFixture), '/runners?state=offline')
    expect(screen.getByText('已筛选，状态：离线；显示 1 台。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '状态：离线' })).toBeInTheDocument()
  })

  it('还没有 Runner 时引导注册', () => {
    renderPage(envelope(emptyFixture), '/runners')
    expect(screen.getByText('还没有 Runner')).toBeInTheDocument()
    expect(screen.getAllByRole('link', { name: '注册新的 Runner' }).length).toBeGreaterThan(0)
  })

  it('筛选地址与服务端一致', () => {
    expect(runnersURL('', '')).toBe('/runners')
    expect(runnersURL('offline', 'linux')).toBe('/runners?state=offline&pool=linux')
  })
})
