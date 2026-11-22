import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/runner.json'
import failed from '../../dev/fixtures/runner.failed.json'
import { chineseLeft, envelope, renderPage, switchLocale, withData } from '../test/render'
import { json, stubFetch } from '../test/fetch'
import { stubSubmit } from './settings/testing'

afterEach(() => {
  vi.unstubAllGlobals()
})

const section = (name: string) => within(screen.getByRole('region', { name }))
const member = { Session: { Login: 'lin-design', Admin: false, ViaToken: false, Bound: false } }

describe('定时刷新（审计）', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  it('有正在执行的 job 时每 5 秒刷新；没有时（也不在升级）不轮询', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const calls = stubFetch(() => json(envelope(fixture)))
    renderPage(envelope(fixture), '/runners/detail/mini-01')
    await act(() => vi.advanceTimersByTimeAsync(5000))
    expect(calls.map((c) => c.url)).toEqual(['/runners/detail/mini-01'])
  })

  it('没有正在执行的 job、没有升级时不轮询', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const idle = withData(envelope(fixture), { Runner: { ...fixture.Data.Runner, Jobs: [], Upgrade: null } })
    const calls = stubFetch(() => json(idle))
    renderPage(idle, '/runners/detail/mini-01')
    await act(() => vi.advanceTimersByTimeAsync(15_000))
    expect(calls).toHaveLength(0)
  })
})

describe('Runner 详情', () => {
  it('显示概况、升级信息和进行中的升级，可以取消', async () => {
    const calls = stubSubmit('/runners/detail/mini-01')
    renderPage(envelope(fixture), '/runners/detail/mini-01')
    expect(screen.getByRole('heading', { level: 1, name: 'mini-01' })).toBeInTheDocument()
    expect(section('概况').getByRole('link', { name: 'build (darwin/arm64)' })).toHaveAttribute('href', '/jobs/9812')

    const up = section('升级')
    expect(up.getByText('@yuebanlaosiji/kilnflow-runner-darwin-arm64')).toBeInTheDocument()
    expect(up.getByText('https://registry.npmjs.org/')).toBeInTheDocument()
    expect(up.getByText('（官方）')).toBeInTheDocument()
    expect(up.getByText('正在下载：0.4.3')).toBeInTheDocument()
    expect(up.getByText(/请求从 0.4.2 升级到 0.4.3/)).toBeInTheDocument()
    expect(up.queryByRole('button', { name: /^升级到/ })).not.toBeInTheDocument()

    await userEvent.click(up.getByRole('button', { name: '取消升级…' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText('取消 mini-01 升级到 0.4.3？')).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('button', { name: '取消升级' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    expect(calls.find((c) => c.method === 'POST')?.url).toBe('/runners/upgrade/mini-01/cancel')

    const history = within(screen.getByRole('table', { name: '升级记录' }))
    expect(history.getByText('0.4.2 → 0.4.3')).toBeInTheDocument()
    expect(history.getByText('进行中')).toBeInTheDocument()
  })

  it('上一次升级失败：显示原因，确认后重新升级到最新版本', async () => {
    const calls = stubSubmit('/runners/detail/mini-02')
    renderPage(envelope(failed), '/runners/detail/mini-02')
    const up = section('升级')
    expect(up.getByText('升级到 0.4.3 失败')).toBeInTheDocument()
    expect(up.getAllByText('0.4.3 连续 3 次启动后没有连上 Server，已回滚到 0.4.2').length).toBeGreaterThan(0)

    await userEvent.click(up.getByRole('button', { name: '升级到 0.4.3…' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText(/等正在执行的 1 个任务结束后切换/)).toBeInTheDocument()
    expect(within(dialog).getByText(/自动回滚到/)).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('button', { name: '升级' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    const post = calls.find((c) => c.method === 'POST')
    expect(post?.url).toBe('/runners/upgrade/mini-02/start')
    // 带上确认的版本：期间 Runner 查到了更新的版本时服务端拒绝，不升级到没有确认过的版本
    expect(post?.body.get('version')).toBe('0.4.3')

    const history = within(screen.getByRole('table', { name: '升级记录' }))
    expect(history.getByText('失败')).toBeInTheDocument()
    expect(history.getByText('已完成')).toBeInTheDocument()
  })

  it('检查更新直接提交', async () => {
    const calls = stubSubmit('/runners/detail/mini-02')
    renderPage(envelope(failed), '/runners/detail/mini-02')
    await userEvent.click(section('升级').getByRole('button', { name: '检查更新' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    expect(calls.find((c) => c.method === 'POST')?.url).toBe('/runners/upgrade/mini-02/check')
  })

  it('等待检查时按钮不可用', () => {
    const r = failed.Data.Runner
    renderPage(withData(envelope(failed), { Runner: { ...r, Upgrade: { ...r.Upgrade, CheckPending: true } } }), '/runners/detail/mini-02')
    expect(screen.getByRole('button', { name: '检查更新' })).toBeDisabled()
    expect(screen.getByText(/等待 Runner 检查/)).toBeInTheDocument()
  })

  it('不能升级时说明原因；成员没有操作按钮', () => {
    const r = failed.Data.Runner
    const reason = '没有经过启动器运行（前台 run 或旧的服务入口），不能自动切换：在机器上执行一次 kiln-runner service install'
    const data = { Runner: { ...r, Upgrade: { ...r.Upgrade, Launcher: false, CanUpgrade: false, Reason: reason, Request: null } }, History: [] }
    renderPage(withData(envelope(failed, member), data), '/runners/detail/mini-02')
    const up = section('升级')
    expect(up.getByText(reason)).toBeInTheDocument()
    expect(up.getByText('升级需要管理员。')).toBeInTheDocument()
    expect(up.queryByRole('button')).not.toBeInTheDocument()
    expect(screen.getByText('还没有升级过。')).toBeInTheDocument()
  })
})

describe('Runner 详情页（英文与切换语言）', () => {
  const rawZH = ['0.4.3 连续 3 次启动后没有连上 Server，已回滚到 0.4.2']

  it('英文：概况、升级、升级记录都按英文显示，没有残留的中文界面文字', () => {
    renderPage(envelope(failed), '/runners/detail/mini-02', { locale: 'en' })
    const up = section('Upgrade')
    expect(up.getByText('Upgrade to 0.4.3 failed')).toBeInTheDocument()
    expect(up.getByRole('button', { name: 'Upgrade to 0.4.3…' })).toBeInTheDocument()
    const history = within(screen.getByRole('table', { name: 'Upgrade history' }))
    expect(history.getByRole('columnheader', { name: 'Requested by' })).toBeInTheDocument()
    expect(history.getByText('Failed')).toBeInTheDocument()
    expect(history.getAllByText('Failed')[0]?.closest('td')).toHaveAttribute('data-label', 'State')
    expect(chineseLeft(document.body, rawZH)).toEqual([])
  })

  it('不能升级的原因来自服务端的原因码，按当前语言显示', async () => {
    const r = failed.Data.Runner
    const reason = { id: 'upgradeReason.disabled' }
    const data = { Runner: { ...r, Upgrade: { ...r.Upgrade, Disabled: true, CanUpgrade: false, Reason: reason, Request: null } }, History: [] }
    renderPage(withData(envelope(failed), data), '/runners/detail/mini-02', { locale: 'en' })
    expect(section('Upgrade').getByText("Self-upgrade is turned off locally: upgrade = false in this machine's config.toml")).toBeInTheDocument()
    await switchLocale('简体中文')
    expect(section('升级').getByText('这台机器的 config.toml 中 upgrade = false，本地关闭了自升级')).toBeInTheDocument()
  })

  it('升级确认对话框打开时切换语言：对话框保持打开，文字立即切换', async () => {
    renderPage(envelope(failed), '/runners/detail/mini-02')
    await userEvent.click(section('升级').getByRole('button', { name: '升级到 0.4.3…' }))
    const dialog = await screen.findByRole('alertdialog')
    expect(within(dialog).getByText(/等正在执行的 1 个任务结束后切换/)).toBeInTheDocument()
    await switchLocale('English')
    expect(screen.getByRole('alertdialog')).toBe(dialog)
    expect(within(dialog).getByText(/It switches after its 1 running job finishes/)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: 'Upgrade' })).toBeInTheDocument()
  })
})

