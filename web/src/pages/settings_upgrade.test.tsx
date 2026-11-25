import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/settings_upgrade.json'
import invalid from '../../dev/fixtures/settings_upgrade.invalid.json'
import { envelope, renderPage, switchLocale, tFor } from '../test/render'
import { stubSubmit } from './settings/testing'

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('设置 · Runner 升级', () => {
  it('在设置导航中，显示 registry 和各 Runner 的版本、最新版本、升级状态', () => {
    renderPage(envelope(fixture), '/settings/runner-upgrade')
    expect(screen.getByRole('heading', { level: 1, name: 'Runner 升级' })).toBeInTheDocument()
    expect(screen.getAllByRole('link', { name: 'Runner 升级' }).some((a) => a.getAttribute('aria-current') === 'page')).toBe(true)

    const form = screen.getByRole('form', { name: 'npm registry' })
    expect(within(form).getByRole('textbox', { name: /registry 地址/ })).toHaveValue('https://npm.example.com/')
    expect(screen.getByText(/当前使用/)).toHaveTextContent('当前使用：https://npm.example.com/')

    const runners = within(screen.getByRole('table', { name: '各 Runner' }))
    const row = (name: string) => {
      const tr = runners.getByRole('link', { name }).closest('tr')
      if (!tr) throw new Error(`没有 ${name}`)
      return within(tr)
    }
    expect(runners.getByRole('link', { name: 'mini-01' })).toHaveAttribute('href', '/runners/detail/mini-01')
    // 只列 CI Runner，不再标注类型；AI 执行节点用 npm 升级，不在这里
    expect(runners.queryByText('CI Runner')).not.toBeInTheDocument()
    expect(screen.getByText(/AI 执行节点不在这里，连接器 kiln-orca 在节点上用 npm 升级/)).toBeInTheDocument()
    expect(row('mini-01').getByText('正在下载：0.4.3')).toBeInTheDocument()
    expect(row('mini-02').getByText('升级到 0.4.3 失败')).toBeInTheDocument()
    expect(row('linux-01').getByText('不能按台升级')).toBeInTheDocument()
    expect(row('linux-01').getByText(/没有经过启动器运行/)).toBeInTheDocument()
    expect(row('linux-01').getByText('0.4.3')).toBeInTheDocument()
    expect(runners.queryByText('mac-02')).not.toBeInTheDocument()
  })

  it('保存 registry 提交表单', async () => {
    const calls = stubSubmit('/settings/runner-upgrade')
    renderPage(envelope(fixture), '/settings/runner-upgrade')
    const form = screen.getByRole('form', { name: 'npm registry' })
    const input = within(form).getByRole('textbox', { name: /registry 地址/ })
    await userEvent.clear(input)
    await userEvent.click(within(form).getByRole('button', { name: '保存' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    const post = calls.find((c) => c.method === 'POST')
    expect(post?.url).toBe('/settings/runner-upgrade/registry')
    expect(post?.body.get('registry')).toBe('')
  })

  it('全部检查更新', async () => {
    const calls = stubSubmit('/settings/runner-upgrade')
    renderPage(envelope(fixture), '/settings/runner-upgrade')
    await userEvent.click(screen.getByRole('button', { name: '全部检查更新' }))
    await waitFor(() => expect(calls.some((c) => c.method === 'POST')).toBe(true))
    expect(calls.find((c) => c.method === 'POST')?.url).toBe('/settings/runner-upgrade/check')
  })

  it('校验失败时在字段旁显示错误；没有设置时使用官方 registry', () => {
    renderPage(envelope(invalid), '/settings/runner-upgrade')
    const form = screen.getByRole('form', { name: 'npm registry' })
    expect(within(form).getByText('没有保存。')).toBeInTheDocument()
    const input = within(form).getByRole('textbox', { name: /registry 地址/ })
    expect(input).toHaveValue('http://npm.example.com/')
    expect(input).toHaveAttribute('aria-invalid', 'true')
    expect(within(form).getByText('地址 http://npm.example.com/ 必须是 https（只有本机地址可以用 http）')).toBeInTheDocument()
    expect(screen.getByText(/当前使用/)).toHaveTextContent('当前使用：https://registry.npmjs.org/（官方）')
  })

  it('字段错误按语言显示：英文界面是英文，中途切换语言随之切换', async () => {
    const zh = '地址 http://npm.example.com/ 必须是 https（只有本机地址可以用 http）'
    const en = tFor('en')('settingsErr.urlScheme', { value: 'http://npm.example.com/' })
    renderPage(envelope(invalid), '/settings/runner-upgrade', { locale: 'en' })
    expect(screen.getByText(en)).toBeInTheDocument()
    expect(screen.queryByText(zh)).not.toBeInTheDocument()
    await switchLocale('简体中文')
    expect(screen.getByText(zh)).toBeInTheDocument()
    await switchLocale('English')
    expect(screen.getByText(en)).toBeInTheDocument()
    expect(screen.queryByText(zh)).not.toBeInTheDocument()
  })
  it('全部检查更新的结果：等待时长按语言格式化', async () => {
    const flash = { id: 'settingsUpgrade.checkRequested', params: { count: 2, poll: 20 } }
    renderPage(envelope(fixture, { Flash: flash }), '/settings/runner-upgrade')
    expect(screen.getByText('已请求 2 台 Runner 查询最新版本：在线的 Runner 下一次连接 Server 时查询（最长约 20 秒）。')).toBeInTheDocument()
    await switchLocale('English')
    const text = tFor('en')('settingsUpgrade.checkRequested', { count: 2, poll: 20 })
    expect(text).toContain('20')
    expect(screen.getByText(text)).toBeInTheDocument()
  })
})
