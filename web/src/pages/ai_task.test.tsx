import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import fixture from '../../dev/fixtures/ai_task.json'
import { envelope, renderPage, withData } from '../test/render'
import { aiReviewSchema } from './ai/schema'

// AI 任务详情页：页面数据用 dev/fixtures/ai_task.json，日志接口和表单提交用假的 fetch。

interface Call {
  url: string
  method: string
  body: string
}

let calls: Call[] = []

beforeEach(() => {
  calls = []
  sessionStorage.clear()
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
      const body = init?.body instanceof URLSearchParams ? init.body.toString() : ''
      calls.push({ url, method: init?.method ?? 'GET', body })
      const u = new URL(url, 'http://kiln.test')
      if (u.pathname.endsWith('/log')) {
        const first = u.searchParams.get('offset') === '0'
        return Promise.resolve(
          new Response(first ? 'codex 开始审计\n' : '', {
            headers: {
              'Content-Type': 'text/plain; charset=utf-8',
              'X-Next-Offset': '20',
              'X-Finished': 'true',
              'X-Job-State': 'failed',
              'X-Job-Terminal': 'true',
              'X-Attempt': '31',
              'X-Current-Attempt': '31',
            },
          }),
        )
      }
      return Promise.resolve(new Response(JSON.stringify({ Redirect: '/ai/requests/95?flash=ok' }), { headers: { 'Content-Type': 'application/json' } }))
    }),
  )
})

afterEach(() => {
  vi.unstubAllGlobals()
})

/** closestEl 返回 el 最近的匹配 sel 的祖先元素（用于在其中查找）。 */
function closestEl(el: HTMLElement, sel: string): HTMLElement {
  const found = el.closest(sel)
  if (!(found instanceof HTMLElement)) throw new Error(`找不到 ${sel}`)
  return found
}

describe('AI 任务详情页', () => {
  it('标题是请求，左侧是同一个请求中的任务，日志从 /ai/tasks/{id}/log 读取，下载地址在任务下', async () => {
    renderPage(envelope(fixture), '/ai/tasks/520')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('AI 审计请求 #88')
    const nav = within(screen.getByRole('navigation', { name: '同一个请求中的 AI 任务' }))
    expect(nav.getByRole('link', { name: '概要' })).toHaveAttribute('href', '/ai/requests/88')
    expect(nav.getByRole('link', { name: 'claude · linux-amd64' })).toHaveAttribute('href', '/ai/tasks/521')
    expect(nav.getByRole('link', { name: 'codex · macos-arm64' }).closest('li')).toHaveAttribute('aria-current', 'page')
    expect(await screen.findByText('codex 开始审计')).toBeInTheDocument()
    expect(calls[0]?.url).toBe('/ai/tasks/520/log?attempt=31&offset=0')
    expect(screen.getByRole('link', { name: /下载/ })).toHaveAttribute('href', '/ai/tasks/520/log/download?attempt=31')
    expect(screen.getByRole('link', { name: /下载/ })).toHaveAttribute('download')
    // attempt：结果、执行时的版本和模型
    const att = within(screen.getByRole('region', { name: /attempt/ }))
    expect(att.getByText('kiln-orca 0.3.0，Agent codex-cli 0.58.0，模型 gpt-5.1-codex')).toBeInTheDocument()
    // attempt 和任务的标题都显示实际执行的平台
    expect(att.getByText('darwin/arm64')).toBeInTheDocument()
    expect(screen.getByTitle('执行的平台')).toHaveTextContent('darwin/arm64')
    expect(att.getByText('正在查看日志')).toBeInTheDocument()
    // 在 Orca 中的关联：dispatch、worktree（完整路径在 title 中）、启动请求；已结束的 attempt 不显示进度
    expect(att.getByText('dsp-31-1')).toBeInTheDocument()
    expect(att.getByText('kiln-a31-e1')).toHaveAttribute('title', '/Users/me/orca/workspaces/kiln/kiln-a31-e1')
    expect(att.getByText('5b2e1c7a-3d4f-4a8b-9c0d-1e2f3a4b5c6d')).toBeInTheDocument()
    expect(att.queryByText(/提交审计报告/)).not.toBeInTheDocument()
  })

  it('执行中的 attempt 显示心跳上报的进度；启动请求取自 Orca 的关联，只有路径时显示路径', () => {
    const [a] = fixture.Data.Attempts
    const running = {
      ...a,
      Outcome: '',
      Finished: 0,
      Stage: 'start',
      Text: '',
      Orca: { start_request_id: 'req-9', worktree_path: '/tmp/wt/kiln-a31-e1' },
    }
    renderPage(withData(envelope(fixture), { Attempts: [running] }), '/ai/tasks/520')
    const att = within(screen.getByRole('region', { name: /attempt/ }))
    expect(att.getByText('启动 Agent')).toBeInTheDocument()
    expect(att.getByText('req-9')).toBeInTheDocument()
    expect(att.getByText('/tmp/wt/kiln-a31-e1')).toBeInTheDocument()
    expect(att.queryByText(/dispatch/)).not.toBeInTheDocument()
  })

  it('可以只重跑这个任务：提交到 /ai/tasks/{id}/rerun', async () => {
    const user = userEvent.setup()
    renderPage(envelope(fixture), '/ai/tasks/520')
    expect(screen.queryByRole('button', { name: '取消请求…' })).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '重跑这个任务…' }))
    const dialog = within(await screen.findByRole('alertdialog'))
    expect(dialog.getByText(/其余任务沿用AI 审计请求 #88的结果/)).toBeInTheDocument()
    await user.click(dialog.getByRole('button', { name: '重跑' }))
    const post = calls.find((c) => c.method === 'POST')
    expect(post?.url).toBe('/ai/tasks/520/rerun')
    expect(post?.body).toContain('csrf=csrf-preview')
  })

  it('请求没有结束时可以取消请求，不能重跑；成员（非管理员）也可以取消、重跑', () => {
    const running = withData(envelope(fixture), {
      Terminal: false,
      RequestTerminal: false,
      CanRerun: false,
      Task: { ...fixture.Data.Task, State: 'running', StateTitle: '执行中', Finished: null },
    })
    const view = renderPage(running, '/ai/tasks/520')
    expect(screen.getByRole('button', { name: '取消请求…' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '重跑这个任务…' })).not.toBeInTheDocument()
    view.unmount()
    const member = { Session: { Login: 'bob', Admin: false, ViaToken: false, Bound: true } }
    const view2 = renderPage(envelope(fixture, member), '/ai/tasks/520')
    expect(screen.getByRole('button', { name: '重跑这个任务…' })).toBeInTheDocument()
    view2.unmount()
    renderPage(withData(envelope(fixture, member), { RequestTerminal: false, CanRerun: false }), '/ai/tasks/520')
    expect(screen.getByRole('button', { name: '取消请求…' })).toBeInTheDocument()
  })

  it('地址中的 attempt 无效：说明原因，不读取日志；没有执行过的任务显示原因', () => {
    const view = renderPage(withData(envelope(fixture), { AttemptErr: '这个任务没有 attempt #9。' }), '/ai/tasks/520?attempt=9')
    expect(screen.getByText(/这个任务没有 attempt #9/)).toBeInTheDocument()
    expect(screen.getByText('选择一次有效的 attempt 后显示日志。')).toBeInTheDocument()
    expect(calls.filter((c) => c.url.includes('/log'))).toHaveLength(0)
    view.unmount()
    renderPage(withData(envelope(fixture), { NoLog: true, Attempts: [], Attempt: 0, LogEmpty: '这个任务沿用上一次的结果，没有执行；日志见 AI 任务 #500。' }), '/ai/tasks/520')
    expect(screen.getByText(/日志见 AI 任务 #500/)).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: /attempt/ })).not.toBeInTheDocument()
  })

  it('审计任务显示审计报告和 PR review 的发布状态', () => {
    renderPage(envelope(fixture), '/ai/tasks/520')
    const sec = within(screen.getByRole('region', { name: 'AI 审计' }))
    expect(sec.getByText('发现问题')).toBeInTheDocument()
    expect(sec.getByText('PR #74 · df16426a1b2c…8d74cb0e1f2a')).toBeInTheDocument()
    expect(sec.getByRole('link', { name: '已发布为 PR review' })).toHaveAttribute('href', 'https://github.com/acme/kiln/pull/74#pullrequestreview-3123456789')
    expect(sec.getByText('8 个变更文件及相关调用链')).toBeInTheDocument()
    // 问题按编号列出，写清影响、触发条件、实际与预期行为和证据
    const first = closestEl(sec.getByText('safeBoundary 在没有换行的长输出下缓冲区无限增长'), 'div')
    expect(within(first).getByText('1.')).toBeInTheDocument()
    expect(within(first).getByText('internal/runner/logs.go:157')).toBeInTheDocument()
    expect(within(first).getByText('已复现')).toBeInTheDocument()
    expect(within(first).getByText('长时间运行的 job 会耗尽 Runner 的内存。')).toBeInTheDocument()
    expect(within(first).getByText('注册 secret "abcd"，job 持续写入以 "abc" 结尾、没有换行的数据。')).toBeInTheDocument()
    expect(within(first).getByText('超过上限后按安全边界发出，缓冲区有界。')).toBeInTheDocument()
    expect(within(first).getByText(/--- FAIL: TestLogShipper\/no_newline/)).toBeInTheDocument()
    // 没有修复方向的问题不显示这一项
    const second = closestEl(sec.getByText('fetch.matrix 使用了未求值的 environment 名称'), 'div')
    expect(within(second).getByText('2.')).toBeInTheDocument()
    expect(within(second).queryByText('修复方向')).not.toBeInTheDocument()
    // 验证结果、未验证、环境备注、已有问题分开显示
    expect(sec.getByText(/多行密钥跨日志分片泄露/).closest('li')).toHaveTextContent('通过 本次改动 多行密钥跨日志分片泄露')
    expect(sec.getByText(/撤掉密钥分片的修复/).closest('li')).toHaveTextContent('通过 变异验证 撤掉密钥分片的修复：TestScrubSplit 按预期失败')
    expect(sec.getByText(/前端测试：需要 pnpm install/).closest('li')).toHaveTextContent('未运行 整体验证 前端测试：需要 pnpm install')
    expect(sec.getByText('前端测试需要 pnpm install，未运行')).toBeInTheDocument()
    expect(sec.getByText('golangci-lint 版本过旧，临时升级后通过')).toBeInTheDocument()
    expect(sec.getByText('internal/server/retention/logs.go:40').closest('li')).toHaveTextContent('retention 删除日志时没有分批：大仓库一次删除大量行')
    // 完整的命令记录按 outcome 标注，不只看退出码
    expect(sec.getByText('完整执行记录（5 条命令）')).toBeInTheDocument()
    expect(sec.getByText('make lint').closest('li')).toHaveTextContent('环境问题，已解决 make lint → 2：golangci-lint 版本过旧')
    expect(sec.getByText('go test -run TestScrubSplit ./internal/runner/').closest('li')).toHaveTextContent('预期失败')
    expect(sec.getByText('go test ./internal/runner/ -run TestLogShipper').closest('li')).toHaveTextContent('最终失败')
    // AI 任务没有产物，也没有开发报告
    expect(screen.queryByRole('region', { name: '产物' })).not.toBeInTheDocument()
    expect(screen.queryByRole('region', { name: 'AI 开发' })).not.toBeInTheDocument()
  })

  it('开发任务显示保存的选题报告：dry_run 没有工作项，计划只在这里看；有工作项时链接过去', () => {
    const dev = (develop: unknown) => withData(envelope(fixture), { Request: { ...fixture.Data.Request, Type: 'develop' }, Review: null, Develop: develop })
    const report = {
      Kind: 'selection',
      TaskID: 520,
      TaskURL: '/ai/tasks/520',
      Selection: {
        selected: [3],
        batch_reason: '同一个模块',
        plan: '先改 a.go 再补测试',
        skipped: [
          { number: 5, reason: '与这一批无关', reconsider: '', later: true },
          { number: 7, reason: '需求不清', reconsider: '补齐验收标准后', later: false },
        ],
      },
      Delivery: null,
      Raw: '',
      RunnerVersion: 'v0.2.0',
      AgentVersion: '',
      Model: '',
      Created: '2026-11-10T08:00:00Z',
    }
    const view = renderPage(
      dev({ Reports: [report], WorkItemURL: '' }),
      '/ai/tasks/520',
    )
    expect(screen.getByRole('region', { name: 'AI 开发' })).toHaveTextContent('没有领取工作项')
    const rep = within(screen.getByRole('region', { name: /选题报告/ }))
    expect(rep.getByText('先改 a.go 再补测试')).toBeInTheDocument()
    expect(rep.getByText(/#5：与这一批无关/)).toHaveTextContent('留到之后的选题')
    expect(rep.getByText(/#7：需求不清/)).not.toHaveTextContent('留到之后的选题')
    expect(screen.queryByRole('region', { name: '产物' })).not.toBeInTheDocument()
    view.unmount()

    renderPage(dev({ Reports: [], WorkItemURL: '/ai-dev/4' }), '/ai/tasks/520')
    const sec = within(screen.getByRole('region', { name: 'AI 开发' }))
    expect(sec.getByRole('link', { name: '工作项' })).toHaveAttribute('href', '/ai-dev/4')
    expect(sec.getByText(/还没有报告/)).toBeInTheDocument()
  })

  it('AI 审计显示 v1 格式的报告', () => {
    const ai = aiReviewSchema.parse(fixture.Data.Review)
    const report = {
      version: 0,
      verdict: 'issues_found',
      summary: '旧报告',
      scope: '',
      findings: [
        { title: '旧问题', severity: 'high', evidence: 'code_evidence', path: 'a.go', line: 0, body: '旧格式的说明', scenario: '', actual: '', expected: '', impact: '', proof: '', fix: '' },
      ],
      checks: null,
      unverified: null,
      env_notes: null,
      preexisting: null,
      commands_run: [{ command: 'go test ./...', exit_code: 1, outcome: '', note: '' }],
    }
    renderPage(withData(envelope(fixture), { Review: { ...ai, Report: report } }), '/ai/tasks/520')
    const sec = within(screen.getByRole('region', { name: 'AI 审计' }))
    expect(sec.getByText('旧格式的说明')).toBeInTheDocument()
    expect(sec.getByText('a.go')).toBeInTheDocument()
    expect(sec.queryByText('审计范围')).not.toBeInTheDocument()
    // v1 的命令没有 outcome：只显示退出码，非 0 标红
    expect(sec.getByText('go test ./...').closest('li')).toHaveTextContent(/^go test \.\/\.\.\. → 1$/)
  })

  // 报告由 Agent 写：结论、严重程度、证据、验证结果、命令的 outcome 与对象原型上的属性同名时原样显示，页面照常渲染
  it.each(['__proto__', 'constructor', 'toString'])('报告中的枚举值是 %s 时原样显示', (value) => {
    const ai = aiReviewSchema.parse(fixture.Data.Review)
    const report = {
      ...ai.Report,
      verdict: value,
      findings: [{ title: '奇怪的问题', severity: value, evidence: value, path: '', line: 0, body: '', scenario: '', actual: '', expected: '', impact: '', proof: '', fix: '' }],
      checks: [{ kind: 'change', title: '奇怪的验证', result: value, detail: '' }],
      commands_run: [{ command: 'go vet ./...', exit_code: 0, outcome: value, note: '' }],
    }
    renderPage(withData(envelope(fixture), { Review: { ...ai, Report: report } }), '/ai/tasks/520')
    const sec = within(screen.getByRole('region', { name: 'AI 审计' }))
    // 结论、严重程度、证据、验证结果各一个 Label
    expect(sec.getAllByText(value, { exact: true })).toHaveLength(4)
    expect(sec.getByText('奇怪的问题').closest('p')).toHaveTextContent(`1. ${value} ${value} 奇怪的问题`)
    expect(sec.getByText(/奇怪的验证/).closest('li')).toHaveTextContent(`${value} 本次改动 奇怪的验证`)
    // 不认识的 outcome 不标注，只显示退出码
    expect(sec.getByText('go vet ./...').closest('li')).toHaveTextContent(/^go vet \.\/\.\.\. → 0$/)
  })

  it('PR review 发布结果不明时标红并说明没有自动重发（C19）', () => {
    const ai = aiReviewSchema.parse(fixture.Data.Review)
    renderPage(withData(envelope(fixture), { Review: { ...ai, ReviewState: 'unknown', ReviewURL: '', ReviewError: '在 PR 中没有找到' } }), '/ai/tasks/520')
    const sec = within(screen.getByRole('region', { name: 'AI 审计' }))
    const dd = sec.getByText(/发布 PR review 的结果不明（没有自动重发）/)
    expect(dd).toHaveTextContent('发布 PR review 的结果不明（没有自动重发）：在 PR 中没有找到')
  })

  it('AI 审计还没有报告时说明原因', () => {
    renderPage(withData(envelope(fixture), { Review: null }), '/ai/tasks/520')
    expect(screen.getByText(/还没有审计报告/)).toBeInTheDocument()
  })
})
