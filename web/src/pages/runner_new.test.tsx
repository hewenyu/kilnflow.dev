import { act, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/runner_new.json'
import existingFixture from '../../dev/fixtures/runner_new.existing.json'
import expiredFixture from '../../dev/fixtures/runner_new.expired.json'
import invalidFixture from '../../dev/fixtures/runner_new.invalid.json'
import missingFixture from '../../dev/fixtures/runner_new.missing.json'
import revealFixture from '../../dev/fixtures/runner_new.reveal.json'
import shownFixture from '../../dev/fixtures/runner_new.shown.json'
import usedFixture from '../../dev/fixtures/runner_new.used.json'
import aiFixture from '../../dev/fixtures/runner_new.ai.json'
import aiDevFixture from '../../dev/fixtures/runner_new.aidev.json'
import { envelope, renderPage } from '../test/render'
import { expiresText, formKind } from './runner_new'

afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

const token = 'kiln_enroll_7Hq2xV9pLmN4cR8sT1wY6zB3'

describe('注册新的 Runner', () => {
  it('表单可以选择 CI Runner、AI 审计节点或 AI 开发节点，默认 CI', () => {
    renderPage(envelope(fixture), '/runners/new')
    expect(screen.getByRole('radio', { name: 'CI Runner' })).toBeChecked()
    for (const [name, value] of [
      ['AI 审计节点', 'ai'],
      ['AI 开发节点', 'ai-dev'],
    ]) {
      const r = screen.getByRole('radio', { name })
      expect(r).not.toBeChecked()
      expect(r).toHaveAttribute('name', 'kind')
      expect(r).toHaveAttribute('value', value)
    }
  })

  it('AI 开发节点的令牌：展示角色，重新生成时沿用开发角色', () => {
    vi.useFakeTimers({ now: Date.parse('2026-11-05T05:30:00-08:00'), shouldAdvanceTime: true })
    renderPage(envelope(aiDevFixture), '/runners/enroll/3f1c2a9b8e7d6c5b4a39281706f5e4d3')
    expect(screen.getByText(/AI 开发节点 · pool/)).toBeInTheDocument()
    expect(screen.getByText(new RegExp(`kiln-orca enroll --server https://kiln.example.com --token ${token}`))).toBeInTheDocument()
  })

  it('AI 开发节点的令牌已经显示过：重新生成的表单沿用开发角色', () => {
    vi.useFakeTimers({ now: Date.parse('2026-11-05T05:30:00-08:00'), shouldAdvanceTime: true })
    const shown = structuredClone(aiDevFixture)
    shown.Data.Reveal.Token = ''
    shown.Data.Command = ''
    renderPage(envelope(shown), '/runners/enroll/3f1c2a9b8e7d6c5b4a39281706f5e4d3')
    expect(screen.getByRole('button', { name: '重新生成令牌' })).toBeInTheDocument()
    expect(document.querySelector('form[action="/runners/token"] input[name="kind"]')).toHaveValue('ai-dev')
  })

  it('formKind 把类型和角色换回表单的取值', () => {
    expect(formKind('ci', '')).toBe('ci')
    expect(formKind('ai', 'review')).toBe('ai')
    expect(formKind('ai', '')).toBe('ai')
    expect(formKind('ai', 'development')).toBe('ai-dev')
  })

  it('AI 执行节点的令牌给出 kiln-orca 的命令和说明', () => {
    vi.useFakeTimers({ now: Date.parse('2026-11-05T05:30:00-08:00'), shouldAdvanceTime: true })
    renderPage(envelope(aiFixture), '/runners/enroll/3f1c2a9b8e7d6c5b4a39281706f5e4d3')
    expect(screen.getByText(new RegExp(`kiln-orca enroll --server https://kiln.example.com --token ${token}`))).toBeInTheDocument()
    expect(screen.getByText(/AI 审计节点 · pool/)).toBeInTheDocument()
    expect(screen.getByText(/检查注册、与 Server 的连接、Orca、Agent、git 和 Docker/)).toBeInTheDocument()
    expect(screen.getByText('kiln-orca run')).toBeInTheDocument()
    // 第一步：用 npm 安装连接器；同一台机器的第二个节点用 --home
    expect(screen.getByText('npm install -g @yuebanlaosiji/kilnflow-orca')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '复制安装命令' })).toBeInTheDocument()
    expect(screen.getByText('--home ~/kiln-orca-dev')).toBeInTheDocument()
    expect(screen.queryByText(/herdr|kiln-ai-runner/i)).not.toBeInTheDocument()
  })

  it('AI 执行节点已注册但没有连接时，提示用 kiln-orca 启动服务', () => {
    const used = structuredClone(usedFixture)
    const r = used.Data.Reveal.Runner
    Object.assign(used.Data.Reveal, { Kind: 'ai', Role: 'review', Runner: { ...r, Online: false, Accepting: false, ConnLabel: '离线' } })
    renderPage(envelope(used), '/runners/enroll/x')
    expect(screen.getByText('kiln-orca service install')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '在 AI 执行节点列表中查看' })).toHaveAttribute('href', '/ai/runners')
  })

  it('第一步说明准备工作，第二步是生成令牌的表单', () => {
    renderPage(envelope(fixture), '/runners/new')
    expect(screen.getByText('curl -fsS https://kiln.example.com/healthz')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '复制检查命令' })).toBeInTheDocument()
    expect(screen.getByText('sudo loginctl enable-linger "$USER"')).toBeInTheDocument()
    expect(screen.getByText(/已有：default、linux。/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '生成注册令牌' })).toBeInTheDocument()
    expect(document.querySelector('input[name="request_id"]')).toHaveValue('3f1c2a9b8e7d6c5b4a39281706f5e4d3')
  })

  it('字段错误显示在字段旁，并回填输入', () => {
    renderPage(envelope(invalidFixture), '/runners/new')
    expect(screen.getByText('没有生成令牌')).toBeInTheDocument()
    const pool = screen.getByRole('combobox', { name: 'pool' })
    expect(pool).toHaveValue('mac mini')
    expect(pool).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByText(/pool 只能包含字母/)).toBeInTheDocument()
  })

  it('第一次打开展示页时显示命令、复制按钮和有效期', () => {
    // fixture 的过期时间是固定的，把"现在"定在它之前
    vi.useFakeTimers({ now: Date.parse('2026-11-05T05:30:00-08:00'), shouldAdvanceTime: true })
    renderPage(envelope(revealFixture), '/runners/enroll/3f1c2a9b8e7d6c5b4a39281706f5e4d3')
    expect(screen.getByText('令牌只显示这一次')).toBeInTheDocument()
    expect(screen.getByText(new RegExp(`--token ${token} --name mini-03`))).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '复制命令' })).toBeInTheDocument()
    expect(screen.getByText(/^剩余 \d+ 分钟$/)).toBeInTheDocument()
    expect(screen.getByText('等待注册：令牌还没有被使用。在机器上执行第 3 步的命令后，这里会自动更新。')).toBeInTheDocument()
  })

  it('自动更新拿到的数据中没有令牌时，仍显示第一次的命令', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const shown = envelope(shownFixture)
    const fetch = vi.fn(() =>
      Promise.resolve(new Response(JSON.stringify(shown), { headers: { 'Content-Type': 'application/json' } })),
    )
    vi.stubGlobal('fetch', fetch)
    renderPage(envelope(revealFixture), '/runners/enroll/3f1c2a9b8e7d6c5b4a39281706f5e4d3')
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5100)
    })
    await waitFor(() => expect(fetch).toHaveBeenCalled())
    expect(screen.getByText(new RegExp(`--token ${token}`))).toBeInTheDocument()
    expect(screen.getByText('令牌只显示这一次')).toBeInTheDocument()
  })

  it('令牌已经显示过时不再显示，可以重新生成', () => {
    renderPage(envelope(shownFixture), '/runners/enroll/x')
    expect(screen.getByText('令牌已经显示过，不会再次显示')).toBeInTheDocument()
    expect(screen.queryByText(new RegExp(token))).not.toBeInTheDocument()
    expect(screen.getByText('令牌已经显示过，这里不再显示命令。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新生成令牌' })).toBeInTheDocument()
    expect(document.querySelector('input[name="request_id"]')).toHaveValue('9a8b7c6d5e4f30211203a4b5c6d7e8f9')
  })

  it('令牌已使用时显示上线状态', () => {
    renderPage(envelope(usedFixture), '/runners/enroll/x')
    expect(screen.getByText('令牌已被使用，不能再次使用，见第 4 步。')).toBeInTheDocument()
    expect(screen.getByText('已上线，可以接收任务。')).toBeInTheDocument()
  })

  it('令牌过期时提示重新生成', () => {
    renderPage(envelope(expiredFixture), '/runners/enroll/x')
    expect(screen.getByText('令牌已过期，没有被使用')).toBeInTheDocument()
    expect(screen.getByText('令牌已过期，没有 Runner 用它注册。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新生成令牌' })).toBeInTheDocument()
  })

  it('同名 Runner 存在时说明会替换它的身份', () => {
    renderPage(envelope(existingFixture), '/runners/enroll/x')
    expect(screen.getByText('已经有名为 mini-01 的 Runner')).toBeInTheDocument()
    expect(screen.getByText(/原来那台机器之后无法再连接/)).toBeInTheDocument()
  })

  it('找不到令牌时说明原因', () => {
    renderPage(envelope(missingFixture), '/runners/enroll/x')
    expect(screen.getByText('找不到这次生成的令牌')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '生成注册令牌' })).toBeInTheDocument()
  })

  it('剩余有效期精确到分钟', () => {
    const exp = '2026-11-05T12:00:00Z'
    const at = (s: number) => Date.parse(exp) - s * 1000
    expect(expiresText(exp, at(0))).toBe('已过期')
    expect(expiresText(exp, at(-5))).toBe('已过期')
    expect(expiresText(exp, at(59))).toBe('剩余不到 1 分钟')
    expect(expiresText(exp, at(60 * 57 + 30))).toBe('剩余 57 分钟')
  })
})
