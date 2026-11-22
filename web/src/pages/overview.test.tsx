import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import fixture from '../../dev/fixtures/overview.json'
import emptyFixture from '../../dev/fixtures/overview.empty.json'
import setupFixture from '../../dev/fixtures/overview.setup.json'
import { envelope, renderPage, withData } from '../test/render'

describe('总览', () => {
  it('按区块列出进行中、我发起的和失败的 run', () => {
    renderPage(envelope(fixture))
    expect(screen.getByText('octocat，现在有 2 个构建在运行，1 个在排队。')).toBeInTheDocument()
    for (const name of ['正在进行', '我最近发起的', '最近失败', '过去 24 小时']) {
      expect(screen.getByRole('heading', { name })).toBeInTheDocument()
    }
    expect(within(screen.getByRole('list', { name: '正在进行' })).getAllByRole('listitem')).toHaveLength(3)
    expect(screen.getByRole('link', { name: '查看全部' })).toHaveAttribute('href', '/runs?state=active')
    expect(screen.getByRole('link', { name: '全部失败' })).toHaveAttribute('href', '/runs?state=failed')
    expect(screen.queryByText('完成初始设置')).not.toBeInTheDocument()
  })

  it('右侧栏显示 24 小时统计和 Runner 摘要', () => {
    renderPage(envelope(fixture))
    expect(screen.getByText('80%')).toBeInTheDocument()
    expect(screen.getByText('80%').parentElement).toHaveTextContent('成功率 80% · 共 23 个构建')
    expect(screen.getByRole('link', { name: /失败\s*3/ })).toHaveAttribute('href', '/runs?state=failed')
    expect(screen.getByText('2 在线，1 离线，3 个任务')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'mini-01' })).toHaveAttribute('href', expect.stringMatching(/^\/runners#r-\d+$/))
    // AI 执行节点单独一行统计，链接到 AI 执行节点页面
    const ai = screen.getByText(/：1 在线，1 离线，1 个 AI 任务/)
    expect(within(ai).getByRole('link', { name: 'AI 执行节点' })).toHaveAttribute('href', '/ai/runners')
  })

  it('没有 AI 执行节点时不显示 AI 执行节点的统计', () => {
    renderPage(envelope(emptyFixture))
    expect(screen.queryByText(/个 AI 任务|AI 执行节点：/)).not.toBeInTheDocument()
  })

  it('管理员看到初始设置清单和进度', () => {
    renderPage(envelope(setupFixture))
    expect(screen.getByRole('heading', { name: '完成初始设置' })).toBeInTheDocument()
    expect(screen.getAllByText('已完成 2 / 4').length).toBeGreaterThan(0)
    expect(screen.getAllByLabelText('已完成')).toHaveLength(2)
    expect(screen.getAllByLabelText('未完成')).toHaveLength(2)
  })

  it('没有数据时显示空状态', () => {
    renderPage(envelope(emptyFixture))
    expect(screen.getByText('现在没有正在运行的构建。')).toBeInTheDocument()
    expect(screen.getByText('现在没有构建在跑')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '我最近发起的' })).not.toBeInTheDocument()
    expect(screen.getByText('过去 24 小时没有构建。')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '注册一台' })).toHaveAttribute('href', '/runners/new')
  })

  it('非管理员没有 Runner 时请管理员注册', () => {
    const env = envelope(emptyFixture)
    renderPage(withData({ ...env, Session: env.Session && { ...env.Session, Admin: false } }, { Setup: [] }))
    expect(screen.getByText(/请管理员注册一台/)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: '注册一台' })).not.toBeInTheDocument()
  })
})
