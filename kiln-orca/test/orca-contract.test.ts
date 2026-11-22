import { chmod, mkdtemp, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import { nodeExec } from '../src/core/exec.js';
import { compareVersions, dirtyRefusal, dispatchNotFound, isDefinite, minOrcaVersion, type Orca, OrcaCli, OrcaError, releaseWorker, StartFailedError, terminalUsable, versionProblem } from '../src/core/orca.js';
import { runNotFound } from '../src/core/mailbox.js';
import { cli, firstRunCursor, fx, type Json, orcaWorld, patched, setTerminalState, worker } from './orcaworld.js';

// OrcaCli 与 Orca CLI 输出的契约（PLAN #33.4）：夹具是 Orca 1.4.258 的真实输出（test/fixtures/orca/README.md）。

const fixed = (name: string) => cli(() => name);

/** withPayload 返回一个 OrcaCli：把夹具 name 中第 i 条消息的 payload（完整记录中是 JSON 字符串）按 f 改过之后再交给解析。 */
function withPayload(name: string, i: number, f: (p: Json) => void) {
  return cli(() =>
    patched(name, (r) => {
      const m = (r.messages as Json[])[i]!;
      const p = (typeof m.payload === 'string' ? JSON.parse(m.payload) : m.payload) as Json;
      f(p);
      m.payload = JSON.stringify(p);
    }),
  );
}

describe('Orca 的版本要求', () => {
  it('按数字逐段比较版本号', () => {
    expect(compareVersions('1.4.258', '1.4.99')).toBeGreaterThan(0);
    expect(compareVersions('1.4.99', '1.4.258')).toBeLessThan(0);
    expect(compareVersions('1.10.0', '1.4.258')).toBeGreaterThan(0);
    expect(compareVersions('2.0', '1.99.999')).toBeGreaterThan(0);
    expect(compareVersions('1.4.258', '1.4.258')).toBe(0);
    expect(compareVersions('1.4', '1.4.0')).toBe(0);
    expect(compareVersions('1.4.258-beta.1', '1.4.258')).toBe(0);
    expect(compareVersions('v1.4.259', '1.4.258')).toBeGreaterThan(0);
    expect(() => compareVersions('dev', '1.4.258')).toThrow();
  });

  it('低于最低版本、没有版本、无法识别时说明原因', () => {
    expect(minOrcaVersion).toBe('1.4.258');
    expect(versionProblem('1.4.258')).toBeUndefined();
    expect(versionProblem('1.5.0')).toBeUndefined();
    expect(versionProblem('1.4.245')).toBe('Orca 版本 1.4.245 低于连接器支持的最低版本 1.4.258，请升级 Orca');
    expect(versionProblem('')).toMatch(/无法识别 Orca 的版本（没有返回版本）/);
    expect(versionProblem('dev')).toMatch(/无法识别 Orca 的版本（dev）/);
  });
});

describe('OrcaCli 对照 Orca 1.4.258 的输出', () => {
  it('status：就绪，带版本和 runtime id；版本过低时不就绪', async () => {
    expect(await fixed('status').orca.status()).toEqual({ ready: true, reason: '', version: '1.4.258', runtimeId: '00000000-0000-4000-8000-000000000001' });
    const old = cli(() => patched('status', (r) => ((r.runtime as Json).appVersion = '1.4.245')));
    expect(await old.orca.status()).toEqual({
      ready: false,
      reason: 'Orca 版本 1.4.245 低于连接器支持的最低版本 1.4.258，请升级 Orca',
      version: '1.4.245',
      runtimeId: '00000000-0000-4000-8000-000000000001',
    });
    const noVersion = cli(() => patched('status', (r) => delete (r.runtime as Json).appVersion));
    expect((await noVersion.orca.status()).ready).toBe(false);
    const down = cli(() => patched('status', (r) => ((r.runtime as Json).state = 'starting')));
    expect(await down.orca.status()).toMatchObject({ ready: false, reason: 'Orca runtime 状态为 starting' });
    // status 不是 orchestration 命令：不带 --full
    const { orca, calls } = fixed('status');
    await orca.status();
    expect(calls[0]?.args).toEqual(['status', '--json']);
  });

  it('repoAdd：已注册时用 repo list 中的 id，否则 repo add', async () => {
    const listed = fixed('repo-list');
    expect(await listed.orca.repoAdd('/Users/kiln/exp-repo')).toBe('00000000-0000-4000-8000-000000000003');
    expect(listed.calls).toHaveLength(1);
    const added = cli((a) => (a[1] === 'list' ? 'repo-list' : 'repo-add'));
    expect(await added.orca.repoAdd('/Users/kiln/Library/Application Support/kiln-orca/kiln-orca')).toBe('00000000-0000-4000-8000-000000000005');
    expect(added.calls[1]?.args).toEqual(['repo', 'add', '--path', '/Users/kiln/Library/Application Support/kiln-orca/kiln-orca', '--json']);
  });

  it('terminal：create 返回句柄；show 区分能用（含没有界面窗格的）、进程已经结束（仍查得到）和不存在', async () => {
    const usable = async (name: string) => terminalUsable(await fixed(name).orca.terminalShow('h'));
    expect(await fixed('terminal-create').orca.terminalCreate('/x', 't')).toBe('term_00000000-0000-4000-8000-000000000007');
    const { orca, calls } = fixed('terminal-show');
    expect(await orca.terminalShow('h')).toEqual({
      handle: 'term_00000000-0000-4000-8000-000000000007',
      title: 'kiln-orca 邮箱',
      worktreePath: '/Users/kiln/Library/Application Support/kiln-orca/kiln-orca',
      connected: true,
      orphaned: false,
      exitCause: '',
    });
    expect(calls[0]?.args).toEqual(['terminal', 'show', '--terminal', 'h', '--json']);
    expect(await usable('terminal-show')).toBe(true);
    // 没有挂在界面窗格上（orphaned）、进程还在：能用（生产节点上把它当作不在，每次检查都新建一个邮箱终端）
    expect(await fixed('terminal-show-orphaned').orca.terminalShow('h')).toMatchObject({ orphaned: true, connected: true, exitCause: '' });
    expect(await usable('terminal-show-orphaned')).toBe(true);
    // 被关闭、shell 退出：仍查得到，进程已经结束
    expect(await fixed('terminal-show-closed').orca.terminalShow('h')).toMatchObject({ orphaned: true, connected: false, exitCause: 'operator_close' });
    expect(await usable('terminal-show-closed')).toBe(false);
    expect(await fixed('terminal-show-exited').orca.terminalShow('h')).toMatchObject({ connected: false, exitCause: 'unknown' });
    expect(await usable('terminal-show-exited')).toBe(false);
    // 不存在（句柄失效）
    expect(await cli(() => ({ stdout: fx('terminal-show-stale'), code: 1 })).orca.terminalShow('h')).toBeUndefined();
    expect(terminalUsable(undefined)).toBe(false);
    // 缺少 connected：输出无法解析，不当作不在
    await expect(cli(() => patched('terminal-show', (r) => delete (r.terminal as Json).connected)).orca.terminalShow('h')).rejects.toMatchObject({ code: 'bad_output' });
    // Orca 不可达、结果未知、其它回答时不能断定终端不在
    const down = cli(() => ({ stdout: '{"ok":false,"error":{"code":"runtime_unavailable","message":"x"}}', code: 1 }));
    await expect(down.orca.terminalShow('h')).rejects.toMatchObject({ code: 'runtime_unavailable' });
    await expect(cli(() => ({ stdout: '', code: 1 })).orca.terminalShow('h')).rejects.toMatchObject({ code: 'exec_failed' });
    const other = cli(() => ({ stdout: '{"ok":false,"error":{"code":"invalid_argument","message":"x"}}', code: 1 }));
    await expect(other.orca.terminalShow('h')).rejects.toMatchObject({ code: 'invalid_argument' });
  });

  it('terminal close：关闭（已经关闭的再关也成功）；句柄失效时返回 false；其它失败抛出', async () => {
    const { orca, calls } = fixed('terminal-close');
    expect(await orca.terminalClose('h')).toBe(true);
    expect(calls[0]?.args).toEqual(['terminal', 'close', '--terminal', 'h', '--json']);
    expect(await cli(() => ({ stdout: fx('terminal-close-stale'), code: 1 })).orca.terminalClose('h')).toBe(false);
    await expect(cli(() => patched('terminal-close', (r) => delete r.close)).orca.terminalClose('h')).rejects.toMatchObject({ code: 'bad_output' });
    await expect(cli(() => ({ stdout: '', code: 1 })).orca.terminalClose('h')).rejects.toMatchObject({ code: 'exec_failed' });
  });

  it('run-create / run-use：带 --full 解析 run.id；精简视图缺字段时报 bad_output', async () => {
    const created = fixed('run-create');
    expect(await created.orca.runCreate('h', 'o')).toBe('run_000000000001');
    expect(created.calls[0]?.args).toEqual(['orchestration', 'run-create', '--from', 'h', '--objective', 'o', '--json', '--full']);
    await expect(fixed('run-create-concise').orca.runCreate('h', 'o')).rejects.toMatchObject({
      code: 'bad_output',
      message: expect.stringContaining('orchestration run-create 的输出缺少 run（'),
    });
    const used = fixed('run-use');
    await used.orca.runUse('h', 'run_000000000001');
    expect(used.calls[0]?.args).toEqual(['orchestration', 'run-use', '--id', 'run_000000000001', '--from', 'h', '--json', '--full']);
    await expect(fixed('run-use').orca.runUse('h', 'run_000000000009')).rejects.toMatchObject({ code: 'bad_output' });
    await expect(cli(() => ({ stdout: fx('run-use-not-found'), code: 1 })).orca.runUse('h', 'run_x')).rejects.toMatchObject({ code: 'run_not_found' });
  });

  it('worker-start：回执中的 run、task、dispatch 和新建的 worktree；Orca 报告启动失败时是 start_failed', async () => {
    const { orca, calls } = fixed('worker-start');
    const ref = await orca.workerStart({ from: 'h', run: 'r', requestId: 'u-1', spec: 's', title: 't', repoId: 'repo', name: 'n', baseBranch: 'abc', comment: 'c', agent: 'claude' });
    expect(ref).toEqual({ runId: 'run_000000000001', taskId: 'task_000000000001', dispatchId: 'ctx_000000000001', worktreePath: '/Users/kiln/orca/workspaces/target/kiln-smoke-1' });
    const args = calls[0]?.args ?? [];
    expect(args.slice(-2)).toEqual(['--json', '--full']);
    expect(args).toEqual(expect.arrayContaining(['--from', 'h', '--run', 'r', '--retry-request', 'u-1', '--worktree', 'new-top-level', '--repo', 'id:repo', '--agent', 'claude', '--base-branch', 'abc']));
    // worker-start 本身不带会话身份（从 Agent 会话中启动连接器时 Orca 会拒绝 --from）
    expect(calls[0]?.opts?.env).toEqual({ ORCA_AGENT_SESSION_ID: undefined });
    // 启动失败：orca 以 1 退出，输出仍是 ok:true 的回执
    const failed = cli(() => ({
      stdout: patched('worker-start', (r) => {
        r.state = 'failed';
        r.failedStage = 'agent_readiness';
        r.lastError = 'claude 没有就绪';
      }),
      code: 1,
    }));
    await expect(failed.orca.workerStart({ from: 'h', run: 'r', requestId: 'u', spec: 's', title: 't', repoId: 'r', name: 'n', baseBranch: 'b', comment: 'c', agent: 'claude' })).rejects.toMatchObject({
      code: 'start_failed',
      message: expect.stringContaining('agent_readiness：claude 没有就绪'),
    });
    // Orca 自己也不确定 Agent 是否启动（outcome_unknown）：有 dispatch，按已启动跟踪
    // 真实的失败回执（建 worktree 失败）：Orca 已经建了 dispatch，带出来（结束时 worker-release 回收它可能留下的终端）
    const real = cli(() => ({ stdout: fx('worker-start-failed'), code: 1 }));
    const err = await real.orca.workerStart({ from: 'h', run: 'r', requestId: 'u', spec: 's', title: 't', repoId: 'r', name: 'n', baseBranch: 'b', comment: 'c', agent: 'claude' }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(StartFailedError);
    expect(err).toMatchObject({ code: 'start_failed', dispatchId: 'ctx_000000000102', message: expect.stringContaining('worktree_create：Command failed') });
    expect(isDefinite(err)).toBe(true);
    const unknown = cli(() => ({ stdout: patched('worker-start', (r) => (r.state = 'outcome_unknown')), code: 1 }));
    expect((await unknown.orca.workerStart({ from: 'h', run: 'r', requestId: 'u', spec: 's', title: 't', repoId: 'r', name: 'n', baseBranch: 'b', comment: 'c', agent: 'claude' })).dispatchId).toBe('ctx_000000000001');
    const noDispatch = cli(() => patched('worker-start', (r) => delete r.dispatchId));
    await expect(noDispatch.orca.workerStart({ from: 'h', run: 'r', requestId: 'u', spec: 's', title: 't', repoId: 'r', name: 'n', baseBranch: 'b', comment: 'c', agent: 'claude' })).rejects.toMatchObject({
      code: 'bad_output',
      message: expect.stringContaining('dispatchId'),
    });
  });

  it('request-show：completed 时取回执中的 dispatch；absent；回执是启动失败时为 failed', async () => {
    const { orca, calls } = fixed('request-show-completed');
    expect(await orca.requestShow('u-1')).toEqual({
      state: 'completed',
      worker: { runId: 'run_000000000001', taskId: 'task_000000000001', dispatchId: 'ctx_000000000001', worktreePath: '/Users/kiln/orca/workspaces/target/kiln-smoke-1' },
    });
    expect(calls[0]?.args).toEqual(['orchestration', 'request-show', '--request', 'u-1', '--json', '--full']);
    expect(await fixed('request-show-absent').orca.requestShow('u')).toEqual({ state: 'absent' });
    const failed = cli(() => patched('request-show-completed', (r) => Object.assign(r.receipt as Json, { state: 'failed', lastError: '没有就绪' })));
    expect(await failed.orca.requestShow('u')).toEqual({ state: 'failed', error: '没有就绪', dispatchId: 'ctx_000000000001' });
    const strange = cli(() => patched('request-show-absent', (r) => (r.state = 'weird')));
    await expect(strange.orca.requestShow('u')).rejects.toMatchObject({ code: 'bad_output' });
  });

  it('worker-show：运行中、已结束、已释放、已停止；精简视图（没有 projection）报 bad_output', async () => {
    expect(await fixed('worker-show-running').orca.workerShow('d')).toEqual({ dispatch: 'dispatched', outcome: 'in_progress', terminal: 'live', worktreePath: '/Users/kiln/orca/workspaces/target/kiln-smoke-1' });
    expect(await fixed('worker-show-completed').orca.workerShow('d')).toMatchObject({ dispatch: 'completed', outcome: 'succeeded', terminal: 'live' });
    expect(await fixed('worker-show-released').orca.workerShow('d')).toMatchObject({ dispatch: 'completed', terminal: 'exited' });
    expect(await fixed('worker-show-stopped').orca.workerShow('d')).toMatchObject({ dispatch: 'failed', outcome: 'failed', terminal: 'exited', worktreePath: '/Users/kiln/orca/workspaces/target/kiln-smoke-3' });
    await expect(fixed('worker-show-concise').orca.workerShow('d')).rejects.toMatchObject({
      code: 'bad_output',
      message: expect.stringContaining('orchestration worker-show 的输出缺少 projection（'),
    });
    const noStage = cli(() => patched('worker-show-running', (r) => delete ((r.projection as Json).stage as Json).dispatch));
    await expect(noStage.orca.workerShow('d')).rejects.toMatchObject({ message: expect.stringContaining('projection.stage.dispatch') });
  });

  it('check：带 --retry-request 与全部参数；解析 worker_done、提问、被拒绝的消息', async () => {
    const req = { requestId: 'u-1', terminal: 'h', run: 'run_1', waitMs: 300_000 };
    const { orca, calls } = fixed('check-worker-done');
    const r = await orca.check(req);
    expect(calls[0]?.args).toEqual(['orchestration', 'check', '--terminal', 'h', '--run', 'run_1', '--wait', '--types', 'worker_done,escalation,question', '--timeout-ms', '300000', '--retry-request', 'u-1', '--json', '--full']);
    expect(calls[0]?.opts?.timeoutMs).toBe(360_000);
    expect(r).toEqual({
      interrupted: false,
      messages: [
        {
          id: 'msg_000000000001',
          type: 'worker_done',
          subject: 'Smoke task done',
          body: 'I sent worker_done immediately as instructed, doing no other work. Nothing was found or changed in the worktree. Nothing is left to do.',
          dispatchId: 'ctx_000000000001',
          outcome: 'succeeded',
          reportPath: '',
          rejected: '',
          turnOutcome: '',
          stopReason: '',
          failureKind: '',
          error: '',
          detail: '',
        },
      ],
    });
    const q = await fixed('check-question').orca.check(req);
    expect(q.messages).toEqual([expect.objectContaining({ id: 'msg_000000000002', type: 'question', dispatchId: 'ctx_000000000004', rejected: '' })]);
    const rej = await fixed('check-rejected').orca.check(req);
    expect(rej.messages.map((m) => [m.type, m.dispatchId, m.rejected])).toEqual([
      ['question', 'ctx_fake1', ''],
      ['worker_done', 'ctx_fake1', 'worker_done references unknown task task_fake.'],
      ['status', 'ctx_fake1', 'No active Dispatch belongs to this message sender.'],
    ]);
    // 精简视图中 payload 是对象：同样解析（连接器总是带 --full，这里只验证解析不依赖 payload 的形状）
    const concise = await fixed('check-concise').orca.check(req);
    expect(concise.messages.map((m) => [m.id, m.dispatchId, m.outcome, m.reportPath, m.rejected])).toEqual(rej.messages.map((m) => [m.id, m.dispatchId, m.outcome, m.reportPath, m.rejected]));
  });

  it('check：Agent 运行中断（1.4.263 的 turnOutcome、failureKind、error、detail）；对象形式的 payload、未知值与错误类型', async () => {
    const req = { requestId: 'u-1', terminal: 'h', run: 'run_1', waitMs: 300_000 };
    const r = await fixed('check-escalation-chaterr').orca.check(req);
    expect(r.messages).toEqual([
      {
        id: 'msg_chaterr_000000000071',
        type: 'escalation',
        subject: 'Agent hit an error: Safety check blocked this request. Please rephrase your prompt.',
        body: expect.stringContaining('Safety check blocked this request'),
        dispatchId: 'ctx_000000000074',
        outcome: '',
        reportPath: '',
        rejected: '',
        turnOutcome: 'failure',
        stopReason: '',
        failureKind: 'safetyCheck',
        error: 'Safety check blocked this request. Please rephrase your prompt.',
        detail: 'Safety check: blocked by policy.',
      },
    ]);
    // 精简视图中 payload 是对象：解析结果相同
    const asObject = cli(() =>
      patched('check-escalation-chaterr', (res) => {
        const m = (res.messages as Json[])[0]!;
        m.payload = JSON.parse(m.payload as string);
      }),
    );
    expect(await asObject.orca.check(req)).toEqual(r);
    // 不认识的 failureKind、stopReason 原样保留：连接器不按 Agent 私有的枚举判断
    const unknown = withPayload('check-escalation-chaterr', 0, (p) => Object.assign(p, { failureKind: 'someFutureKind', stopReason: 'someFutureStop' }));
    expect((await unknown.orca.check(req)).messages[0]).toMatchObject({ failureKind: 'someFutureKind', stopReason: 'someFutureStop', turnOutcome: 'failure' });
    // 不是字符串的字段按缺失处理：不把数字、对象、数组当成原因
    const wrong = withPayload('check-escalation-chaterr', 0, (p) => Object.assign(p, { turnOutcome: 1, failureKind: {}, error: ['x'], detail: null, stopReason: true }));
    expect((await wrong.orca.check(req)).messages[0]).toMatchObject({ turnOutcome: '', failureKind: '', error: '', detail: '', stopReason: '' });
  });

  it('check：系统停止通知的 stopReason（finished 这一轮结束、needs-input 需要人）', async () => {
    const r = await fixed('check-escalation-stop').orca.check({ requestId: 'u-1', terminal: 'h', run: 'run_1', waitMs: 300_000 });
    expect(r.messages.map((m) => [m.id, m.type, m.dispatchId, m.stopReason, m.turnOutcome, m.error])).toEqual([
      ['msg_stop_000000000080', 'escalation', 'ctx_000000000083', 'finished', '', ''],
      ['msg_stop_000000000084', 'escalation', 'ctx_000000000087', 'needs-input', '', ''],
    ]);
  });

  it('check：_orcaLifecycleRejection 的各种形状——非标准形状同样算拒绝', async () => {
    const req = { requestId: 'u-1', terminal: 'h', run: 'run_1', waitMs: 1 };
    const rej = async (v: unknown) => {
      const c = withPayload('check-worker-done', 0, (p) => {
        if (v === undefined) delete p._orcaLifecycleRejection;
        else p._orcaLifecycleRejection = v;
      });
      return (await c.orca.check(req)).messages[0]!;
    };
    // 没有拒绝
    for (const v of [undefined, null, false, '']) expect((await rej(v)).rejected).toBe('');
    // 原因字符串、带 reason / code 的对象
    expect((await rej('sender_not_assignee')).rejected).toBe('sender_not_assignee');
    expect((await rej({ reason: '未知的 task', code: 'unknown_task' })).rejected).toBe('未知的 task');
    expect((await rej({ code: 'unknown_task' })).rejected).toBe('unknown_task');
    // 空对象和非标准形状同样是拒绝，不能当成没有拒绝
    for (const v of [{}, true, 1, ['x']]) expect((await rej(v)).rejected).toBe('被 Orca 拒绝');
  });

  it('check：超时没有消息；等待被打断；Run 绑定在别的终端上；缺少 messages', async () => {
    const req = { requestId: 'u', terminal: 'h', run: 'r', waitMs: 1 };
    expect(await fixed('check-timeout').orca.check(req)).toEqual({ messages: [], interrupted: false });
    expect(await fixed('check-interrupted').orca.check(req)).toEqual({ messages: [], interrupted: true });
    await expect(cli(() => ({ stdout: fx('check-fenced'), code: 1 })).orca.check(req)).rejects.toMatchObject({ code: 'consumer_fenced' });
    // Run 不存在（编排状态被重置）：明确的拒绝，邮箱据此换新 Run
    const gone = await cli(() => ({ stdout: fx('check-run-not-found'), code: 1 })).orca.check(req).catch((e: unknown) => e);
    expect(gone).toMatchObject({ code: 'run_not_found' });
    expect(runNotFound(gone)).toBe(true);
    const noMessages = cli(() => patched('check-timeout', (r) => delete r.messages));
    await expect(noMessages.orca.check(req)).rejects.toMatchObject({ code: 'bad_output', message: expect.stringContaining('messages') });
    const noId = cli(() => patched('check-worker-done', (r) => delete ((r.messages as Json[])[0] as Json).id));
    await expect(noId.orca.check(req)).rejects.toMatchObject({ code: 'bad_output', message: expect.stringContaining('messages[].id') });
  });

  it('worker-stop：停止、已经结束、dispatch 不存在都算确认；stop_unknown（以 1 退出）不算', async () => {
    expect(await fixed('worker-stop').orca.workerStop('d')).toBe(true);
    expect(await fixed('worker-stop-settled').orca.workerStop('d')).toBe(true);
    // 编排状态被重置后 Orca 中没有这个 dispatch：没有可停的
    expect(await cli(() => ({ stdout: fx('worker-stop-not-found'), code: 1 })).orca.workerStop('d')).toBe(true);
    // 其它明确的拒绝照常抛出
    await expect(cli(() => ({ stdout: fx('run-use-not-found'), code: 1 })).orca.workerStop('d')).rejects.toMatchObject({ code: 'run_not_found' });
    const unknown = cli(() => ({
      stdout: patched('worker-stop', (r) => {
        r.state = 'stop_unknown';
        r.alreadySettled = false;
      }),
      code: 1,
    }));
    expect(await unknown.orca.workerStop('d')).toBe(false);
    const { orca, calls } = fixed('worker-stop');
    await orca.workerStop('ctx_1');
    expect(calls[0]?.args).toEqual(['orchestration', 'worker-stop', '--dispatch', 'ctx_1', '--json', '--full']);
  });

  it('worker-show：dispatch 不存在（编排状态被重置）是 dispatchNotFound 的明确错误；其它错误不是', async () => {
    const e = await cli(() => ({ stdout: fx('worker-show-not-found'), code: 1 })).orca.workerShow('ctx_000000000999').catch((err: unknown) => err);
    expect(e).toMatchObject({ code: 'dispatch_not_found' });
    expect(dispatchNotFound(e)).toBe(true);
    expect(isDefinite(e)).toBe(true);
    expect(dispatchNotFound(await cli(() => ({ stdout: '', code: 1 })).orca.workerShow('d').catch((err: unknown) => err))).toBe(false);
    expect(dispatchNotFound(await cli(() => ({ stdout: fx('run-use-not-found'), code: 1 })).orca.workerShow('d').catch((err: unknown) => err))).toBe(false);
  });

  it('worker-release：released、already_released、retained、dispatch 不存在都是确认；release_unknown、release_pending 是结果未知；还没有结束是明确拒绝', async () => {
    const { orca, calls } = fixed('worker-release');
    expect(await orca.workerRelease('ctx_1')).toBe('released');
    expect(calls[0]?.args).toEqual(['orchestration', 'worker-release', '--dispatch', 'ctx_1', '--json', '--full']);
    expect(await fixed('worker-release-already').orca.workerRelease('d')).toBe('already_released');
    expect(await fixed('worker-release-retained').orca.workerRelease('d')).toBe('retained');
    expect(await cli(() => ({ stdout: fx('worker-release-not-found'), code: 1 })).orca.workerRelease('d')).toBe('not_found');
    // release_unknown 以 1 退出、输出仍是 ok:true 的记录
    const unknown = cli(() => ({ stdout: patched('worker-release', (r) => Object.assign(r, { state: 'release_unknown', lastError: '终端没有响应' })), code: 1 }));
    const ue = await unknown.orca.workerRelease('d').catch((e: unknown) => e);
    expect(ue).toMatchObject({ code: 'release_unknown', message: expect.stringContaining('终端没有响应') });
    expect(isDefinite(ue)).toBe(false);
    const pe = await cli(() => patched('worker-release', (r) => (r.state = 'release_pending'))).orca.workerRelease('d').catch((e: unknown) => e);
    expect(pe).toMatchObject({ code: 'release_pending' });
    expect(isDefinite(pe)).toBe(false);
    // 不认识的 state 不能确认终端已经关闭：按结果未知
    const weird = await cli(() => patched('worker-release', (r) => (r.state = 'weird'))).orca.workerRelease('d').catch((e: unknown) => e);
    expect(weird).toMatchObject({ code: 'release_unknown' });
    const ie = await cli(() => ({ stdout: fx('worker-release-inactive'), code: 1 })).orca.workerRelease('d').catch((e: unknown) => e);
    expect(ie).toMatchObject({ code: 'dispatch_inactive' });
    expect(isDefinite(ie)).toBe(true);
  });

  it('releaseWorker 归类释放的结果：确认、明确拒绝（不再重试）、结果未知或 Orca 不可达（稍后重试）', async () => {
    const via = (stdout: string, code = 0) => releaseWorker(cli(() => ({ stdout, code })).orca, 'd');
    expect(await via(fx('worker-release'))).toEqual({ kind: 'done', state: 'released' });
    expect(await via(fx('worker-release-already'))).toEqual({ kind: 'done', state: 'already_released' });
    expect(await via(fx('worker-release-retained'))).toEqual({ kind: 'done', state: 'retained' });
    expect(await via(fx('worker-release-not-found'), 1)).toEqual({ kind: 'done', state: 'not_found' });
    expect(await via(fx('worker-release-inactive'), 1)).toMatchObject({ kind: 'refused', error: expect.stringContaining('dispatch_inactive') });
    expect(await via(patched('worker-release', (r) => (r.state = 'release_unknown')), 1)).toMatchObject({ kind: 'unknown' });
    expect(await via('', 1)).toMatchObject({ kind: 'unknown', error: expect.stringContaining('exec_failed') });
    // Orca 不可达（带错误码，但不是对这个请求的拒绝）：稍后重试
    const down = JSON.stringify({ ok: false, error: { code: 'runtime_unavailable', message: 'Orca 没有运行' } });
    expect(await via(down, 1)).toMatchObject({ kind: 'unknown', error: expect.stringContaining('runtime_unavailable') });
    // 非 OrcaError 的异常同样按结果未知
    const thrower = { workerRelease: () => Promise.reject(new Error('boom')) } as unknown as Orca;
    expect(await releaseWorker(thrower, 'd')).toEqual({ kind: 'unknown', error: 'boom' });
  });

  it('reply：第一次与重复的相同回答都成功', async () => {
    const { orca, calls } = fixed('reply');
    await orca.reply('h', 'msg_1', 'b');
    expect(calls[0]?.args).toEqual(['orchestration', 'reply', '--from', 'h', '--id', 'msg_1', '--body', 'b', '--json', '--full']);
    await fixed('reply-duplicate').orca.reply('h', 'msg_1', 'b');
  });

  it('repo list：id、路径和类型', async () => {
    const { orca } = fixed('repo-list');
    const repos = await orca.repoList();
    expect(repos.length).toBeGreaterThan(0);
    for (const r of repos) expect(r).toMatchObject({ id: expect.any(String), path: expect.stringMatching(/^\//), kind: 'git' });
  });

  it('worktree list：按仓库列出，带 --limit；区分仓库本身、Agent 的检出和连接器建的工作区；截断、缺字段时报错', async () => {
    const { orca, calls } = fixed('worktree-list');
    const rows = await orca.worktreeList('r1');
    expect(calls[0]?.args).toEqual(['worktree', 'list', '--repo', 'id:r1', '--limit', '10000', '--json']);
    expect(rows.map((r) => [path.basename(r.path), r.isMainWorktree, r.git, r.comment])).toEqual([
      ['target', true, true, ''],
      ['head', false, true, ''],
      ['kiln-a9155-e1', false, true, 'kiln:attempt=9155;epoch=1;task=1;kind=review'],
    ]);
    const wt = rows[2]!;
    expect(wt.id).toBe(`${wt.repoId}::${wt.path}`);
    expect(wt.branch).toBe('refs/heads/kiln/kiln-a9155-e1');
    expect(wt.lastActivityAt).toBeGreaterThan(0);
    expect(rows[1]?.lastActivityAt).toBe(0);
    await expect(cli(() => patched('worktree-list', (r) => (r.truncated = true))).orca.worktreeList('r1')).rejects.toMatchObject({ code: 'list_truncated' });
    await expect(cli(() => patched('worktree-list', (r) => delete r.truncated)).orca.worktreeList('r1')).rejects.toMatchObject({ code: 'list_truncated' });
    await expect(cli(() => patched('worktree-list', (r) => delete r.worktrees)).orca.worktreeList('r1')).rejects.toMatchObject({ code: 'bad_output' });
  });

  it('worktree rm：不带 --run-hooks、--delete-branch，只在要求时带 --force；不干净时是 dirtyRefusal；不存在时是 not_found', async () => {
    const { orca, calls } = fixed('worktree-rm');
    expect(await orca.worktreeRemove('r1::/p', false)).toBe('removed');
    expect(await orca.worktreeRemove('r1::/p', true)).toBe('removed');
    expect(calls.map((c) => c.args)).toEqual([
      ['worktree', 'rm', '--worktree', 'id:r1::/p', '--json'],
      ['worktree', 'rm', '--worktree', 'id:r1::/p', '--force', '--json'],
    ]);
    const dirty = await cli(() => ({ stdout: fx('worktree-rm-dirty'), code: 1 })).orca.worktreeRemove('r1::/p', false).catch((e: unknown) => e);
    expect(dirtyRefusal(dirty)).toBe(true);
    expect(await cli(() => ({ stdout: fx('worktree-rm-not-found'), code: 1 })).orca.worktreeRemove('r1::/p', false)).toBe('not_found');
    // 带 --force 仍失败（git 锁定）、hook 失败、Orca 不可达都不是"因为不干净"
    expect(dirtyRefusal(new OrcaError('runtime_error', 'Failed to force delete worktree at /p. Worktree is locked by Git.'))).toBe(false);
    expect(dirtyRefusal(new OrcaError('worktree_archive_hook_failed', 'Failed to delete worktree at /p.'))).toBe(false);
    expect(dirtyRefusal(new OrcaError('runtime_unavailable', 'down'))).toBe(false);
    await expect(cli(() => patched('worktree-rm', (r) => delete r.removed)).orca.worktreeRemove('r1::/p', false)).rejects.toMatchObject({ code: 'remove_unknown' });
  });

  it('terminal list：工作区中的终端（没有界面窗格的也列出、也算活着）；截断时报错', async () => {
    const { orca, calls } = fixed('terminal-list');
    const t = await orca.terminalList('id:r1::/p');
    expect(calls[0]?.args).toEqual(['terminal', 'list', '--worktree', 'id:r1::/p', '--limit', '10000', '--json']);
    expect(t.map((x) => x.title)).toEqual(['Terminal 1', 'verify']);
    expect(t.every(terminalUsable)).toBe(true);
    expect(await cli(() => patched('terminal-list', (r) => (r.terminals = []))).orca.terminalList('id:r1::/p')).toEqual([]);
    const orphan = await cli(() => patched('terminal-list', (r) => ((r.terminals as Json[])[0]!.orphaned = true))).orca.terminalList('id:r1::/p');
    expect(orphan.filter(terminalUsable)).toHaveLength(2);
    expect(orphan[0]).toMatchObject({ orphaned: true, connected: true });
    await expect(cli(() => patched('terminal-list', (r) => (r.truncated = true))).orca.terminalList('id:r1::/p')).rejects.toMatchObject({ code: 'list_truncated' });
    // 只有明确回答 truncated === false 才算完整（审计）
    await expect(cli(() => patched('terminal-list', (r) => delete r.truncated)).orca.terminalList('id:r1::/p')).rejects.toMatchObject({ code: 'list_truncated' });
    await expect(cli(() => patched('terminal-list', (r) => (r.truncated = 'false'))).orca.terminalList('id:r1::/p')).rejects.toMatchObject({ code: 'list_truncated' });
    await expect(cli(() => patched('terminal-list', (r) => delete (r.terminals as Json[])[1]!.connected)).orca.terminalList('id:r1::/p')).rejects.toMatchObject({ code: 'bad_output' });
    // 邮箱仓库中的终端（两个邮箱、一个别的标题），按 path: 选择
    const mbox = fixed('terminal-list-mailbox');
    const m = await mbox.orca.terminalList('path:/Users/kiln/Library/Application Support/kiln-orca/kiln-orca');
    expect(mbox.calls[0]?.args).toEqual(['terminal', 'list', '--worktree', 'path:/Users/kiln/Library/Application Support/kiln-orca/kiln-orca', '--limit', '10000', '--json']);
    expect(m.map((x) => x.title)).toEqual(['kiln-orca 邮箱', 'kiln-orca 邮箱', 'zsh']);
    expect(m.every((x) => x.worktreePath === '/Users/kiln/Library/Application Support/kiln-orca/kiln-orca')).toBe(true);
  });

  it('unreleasedWorkers：真实的翻页回执（三页）按游标原样翻完，排除终端已经释放、没有终端且已经结束的', async () => {
    // 夹具 worker-list-all-p1..p3.json 是同一个 Run 的七个 dispatch（采集时 --limit 3）：三个终端保留，四个没有终端、已经结束；
    // worker-list-all-released.json 是另一个 Run 的十个：终端已经释放，或者没有终端、已经结束
    const cursorOf = (name: string) => (JSON.parse(fx(name)) as { result: { page: { nextCursor: string } } }).result.page.nextCursor;
    const pages: Record<string, string> = { '': 'worker-list-all-p1', [cursorOf('worker-list-all-p1')]: 'worker-list-all-p2', [cursorOf('worker-list-all-p2')]: 'worker-list-all-p3' };
    const oneRun = (run: string) => patched('run-list-last', (r) => (r.runs = [{ ...(r.runs as Json[])[0], id: run }]));
    const flag = (args: string[], name: string) => (args.includes(name) ? args[args.indexOf(name) + 1]! : '');
    const { orca, calls } = cli((args) => (args[1] === 'run-list' ? oneRun('run_000000000601') : pages[flag(args, '--cursor')]!));
    const ws = await orca.unreleasedWorkers();
    expect(calls.map((c) => c.args.join(' '))).toEqual([
      'orchestration run-list --limit 100 --json --full',
      'orchestration worker-list --run run_000000000601 --limit 100 --json --full',
      `orchestration worker-list --run run_000000000601 --limit 100 --cursor ${cursorOf('worker-list-all-p1')} --json --full`,
      `orchestration worker-list --run run_000000000601 --limit 100 --cursor ${cursorOf('worker-list-all-p2')} --json --full`,
      'orchestration run-list --limit 100 --json --full',
    ]);
    expect(ws.map((x) => [x.dispatchId, x.terminalState, x.liveness])).toEqual([
      ['ctx_000000000702', 'retained', 'live'],
      ['ctx_000000000705', 'retained', 'live'],
      ['ctx_000000000719', 'retained', 'exited'],
    ]);
    expect(ws[0]!.workspace).toBe('00000000-0000-4000-8000-000000000712::/Users/kiln/orca/workspaces/target/kiln-a9157-e1');
    const released = cli((args) => (args[1] === 'run-list' ? oneRun('run_000000000602') : 'worker-list-all-released'));
    expect(await released.orca.unreleasedWorkers()).toEqual([]);
  });

  it('unreleasedWorkers：run-list 翻完全部 Run，逐个 Run 显式 --run、不按状态过滤翻完所有页；别的 Run 的 worker 都列出', async () => {
    const w = orcaWorld();
    // 邮箱 Run（run-list 第二页）有两页：最旧的（第二页上）是状态不明的 worker，第一页上的都已经释放
    w.add('run_000000000600', worker('ctx_000000000800', 'release_unknown', 'unverifiable', '/Users/kiln/orca/workspaces/target/kiln-a9170-e1'));
    for (let i = 0; i < 100; i++) w.add('run_000000000600', worker(`ctx_${String(900 + i).padStart(12, '0')}`, 'released', 'exited', '/Users/kiln/orca/workspaces/target/kiln-a9171-e1'));
    const ws = await w.orca.unreleasedWorkers();
    // 每次 worker-list 都显式指定 Run、不带 --terminal-state：不依赖 Orca 对调用方的推断，也不分状态查询
    expect(w.calls.filter((c) => c[1] === 'worker-list').every((c) => c[2] === '--run' && !c.includes('--terminal-state'))).toBe(true);
    const runList = [
      ['orchestration', 'run-list', '--limit', '100', '--json', '--full'],
      ['orchestration', 'run-list', '--limit', '100', '--cursor', firstRunCursor, '--json', '--full'],
    ];
    const list = (run: string, cursor?: string) => ['orchestration', 'worker-list', '--run', run, '--limit', '100', ...(cursor ? ['--cursor', cursor] : []), '--json', '--full'];
    // 先列全部 Run（两页），再逐个 Run 翻完，最后再列一次 Run
    expect(w.calls).toEqual([
      ...runList,
      list('run_000000000601'),
      list('run_000000000602'),
      list('run_000000000603'),
      list('run_000000000600'),
      list('run_000000000600', 'c101-2'),
      ...runList,
    ]);
    // run_000000000601（不是邮箱的 Run）中终端保留的三个（其中一个是还在运行的 structured Agent）和邮箱 Run 第二页上的一个
    expect(ws.map((x) => x.dispatchId)).toEqual(['ctx_000000000702', 'ctx_000000000705', 'ctx_000000000719', 'ctx_000000000800']);
    expect(ws[3]).toEqual({
      dispatchId: 'ctx_000000000800',
      terminalState: 'release_unknown',
      liveness: 'unverifiable',
      workspace: '00000000-0000-4000-8000-000000000623::/Users/kiln/orca/workspaces/target/kiln-a9170-e1',
    });
  });

  it('unreleasedWorkers：worker 的状态在查询期间变化也不漏（分状态查询不是同一时刻的）', async () => {
    const at = '/Users/kiln/orca/workspaces/target/kiln-a9170-e1';
    // 一个 Orca：每次 worker-list 之前，worker 都变到这次没有查询的那个状态（active 与 retained 之间来回，例如 worker-keep-open
    // 与 worker-allow-auto-close）。按状态分开查询时两次都查不到它；一次查询全部 dispatch 时它总在
    const w = orcaWorld();
    const flip = worker('ctx_000000000800', 'active', 'live', at);
    w.add('run_000000000603', flip);
    w.beforeList = (args) => {
      if (args[3] !== 'run_000000000603') return;
      const asked = args.includes('--terminal-state') ? args[args.indexOf('--terminal-state') + 1] : flip.terminalState;
      setTerminalState(flip, asked === 'active' ? 'retained' : 'active');
    };
    expect((await w.orca.unreleasedWorkers()).map((x) => x.dispatchId)).toContain('ctx_000000000800');

    // 翻页期间状态变化：第一页读过的 worker 之后释放了、第二页上的 worker 在读到之前从 active 变成 retained、读到之前
    // 释放了的就不在；翻页期间新建的 dispatch 不在这次的快照中（它不改变 page.total）
    const p = orcaWorld();
    const older = worker('ctx_000000000801', 'active', 'live', at);
    const gone = worker('ctx_000000000802', 'retained', 'exited', at);
    p.add('run_000000000600', older);
    p.add('run_000000000600', gone);
    for (let i = 0; i < 100; i++) p.add('run_000000000600', worker(`ctx_${String(900 + i).padStart(12, '0')}`, i === 99 ? 'reclaimable' : 'released', 'exited', at));
    const newest = p.runs.get('run_000000000600')!.at(-1)!;
    p.beforeList = (args) => {
      if (args[3] !== 'run_000000000600' || !args.includes('--cursor')) return;
      setTerminalState(newest, 'released');
      setTerminalState(older, 'retained');
      setTerminalState(gone, 'released');
      p.add('run_000000000600', worker('ctx_000000000803', 'active', 'live', at));
    };
    const ws = await p.orca.unreleasedWorkers();
    expect(ws.filter((x) => x.workspace.endsWith(at)).map((x) => [x.dispatchId, x.terminalState])).toEqual([
      ['ctx_000000000999', 'reclaimable'],
      ['ctx_000000000801', 'retained'],
    ]);
  });

  it('unreleasedWorkers：没有终端的 dispatch 还没有了结时算没有释放（之后可能建终端），不认识的终端状态也算', async () => {
    const at = '/Users/kiln/orca/workspaces/target/kiln-a9170-e1';
    const w = orcaWorld();
    const noTerminal = (id: string, status: string) => {
      const x = worker(id, 'active', 'unknown', at);
      setTerminalState(x, null);
      x.dispatchStatus = status;
      delete x.resource;
      w.add('run_000000000603', x);
    };
    noTerminal('ctx_000000000810', 'pending');
    noTerminal('ctx_000000000811', 'dispatched');
    noTerminal('ctx_000000000812', 'completed');
    noTerminal('ctx_000000000813', 'failed');
    noTerminal('ctx_000000000814', 'circuit_broken');
    w.add('run_000000000603', worker('ctx_000000000815', 'paused', 'live', at));
    const ws = (await w.orca.unreleasedWorkers()).filter((x) => x.workspace.endsWith(at));
    expect(ws.map((x) => [x.dispatchId, x.terminalState])).toEqual([
      ['ctx_000000000815', 'paused'],
      ['ctx_000000000811', ''],
      ['ctx_000000000810', ''],
    ]);
    // 没有终端时工作区取自 projection.workspace
    expect(ws[1]!.workspace).toBe(`00000000-0000-4000-8000-000000000623::${at}`);
  });

  it('unreleasedWorkers：不带 --run 时 Orca 按推断的调用方列出（bound），连接器不用它', async () => {
    // 夹具 worker-list-bound.json：调用方被推断为邮箱终端时，不带 --run 只列出邮箱 Run 的 worker
    expect((JSON.parse(fx('worker-list-bound')) as { result: Json }).result.scope).toEqual({ run: 'run_000000000600', source: 'bound' });
    const w = orcaWorld();
    await w.orca.unreleasedWorkers();
    expect(w.calls.some((c) => c[1] === 'worker-list' && !c.includes('--run'))).toBe(false);
  });

  it('unreleasedWorkers：不能确认列表完整时报错（作用域不对、某个 Run 查询失败、翻页不完整、输出结构不认识、查询期间新建了 Run）', async () => {
    type World = ReturnType<typeof orcaWorld>;
    const reject = (patch: (w: World) => void) => {
      const w = orcaWorld();
      patch(w);
      return w.orca.unreleasedWorkers();
    };
    /** mine 是 Run run 的回执（作用域正确），再按 f 改动。 */
    const mine = (run: string, f: (r: Json) => void) => patched('worker-list-all-p3', (r) => ((r.scope = { run, source: 'flag' }), f(r)));
    const onRun = (run: string, f: (args: string[]) => string | undefined) => (w: World) => (w.override = (args) => (args[1] === 'worker-list' && args[3] === run ? f(args) : undefined));
    // 回执的作用域不是请求的 Run：bound（Orca 忽略了 --run）、all、别的 Run、没有 scope
    await expect(reject(onRun('run_000000000603', () => fx('worker-list-bound')))).rejects.toMatchObject({ code: 'list_scoped' });
    await expect(reject(onRun('run_000000000603', () => fx('worker-list')))).rejects.toMatchObject({ code: 'list_scoped' });
    await expect(reject(onRun('run_000000000603', () => fx('worker-list-all-p3')))).rejects.toMatchObject({ code: 'list_scoped' });
    await expect(reject(onRun('run_000000000601', () => patched('worker-list-all-p3', (r) => delete r.scope)))).rejects.toMatchObject({ code: 'list_scoped' });
    // 某个 Run 的查询失败：别的 Run 都正常也不返回部分结果
    await expect(reject(onRun('run_000000000602', () => fx('check-run-not-found')))).rejects.toMatchObject({ code: 'run_not_found' });
    await expect(reject((w) => (w.override = (args) => (args[3] === 'run_000000000602' ? { stdout: '', code: 1 } : undefined)))).rejects.toMatchObject({ code: 'exec_failed' });
    // 翻页期间游标失效（Orca 只在 orchestration reset 时删除 dispatch；夹具是真实回执，CLI 报的是 runtime_error）
    await expect(reject(onRun('run_000000000601', (args) => (args.includes('--cursor') ? fx('worker-list-cursor-expired') : fx('worker-list-all-p1'))))).rejects.toMatchObject({
      code: 'runtime_error',
      message: expect.stringContaining('changed destructively while paging'),
    });
    // worker-list 的翻页：hasMore 却没有 nextCursor；缺少 workers、page.total、dispatchId；terminalState 不是字符串或 null
    await expect(reject(onRun('run_000000000600', () => mine('run_000000000600', (r) => (r.page = { total: 1, hasMore: true, nextCursor: null }))))).rejects.toMatchObject({ code: 'bad_output' });
    await expect(reject(onRun('run_000000000600', () => mine('run_000000000600', (r) => delete r.workers)))).rejects.toMatchObject({ code: 'bad_output' });
    await expect(reject(onRun('run_000000000600', () => mine('run_000000000600', (r) => delete (r.page as Json).total)))).rejects.toMatchObject({ code: 'bad_output' });
    await expect(reject(onRun('run_000000000600', () => mine('run_000000000600', (r) => delete (r.workers as Json[])[0]!.dispatchId)))).rejects.toMatchObject({ code: 'bad_output' });
    await expect(reject(onRun('run_000000000600', () => mine('run_000000000600', (r) => ((r.workers as Json[])[0]!.terminalState = 1))))).rejects.toMatchObject({ code: 'bad_output' });
    // 列出的与 page.total 对不上：翻页期间 total 变了（第二页的 total 不同，翻完的条数仍等于第一页的）、同一个 dispatch
    // 出现两次（去重之后条数与 total 相等）、翻完时条数与 total 不等
    const cursorOf = (name: string) => (JSON.parse(fx(name)) as { result: { page: { nextCursor: string } } }).result.page.nextCursor;
    const realPages = (p2: string) => (args: string[]) =>
      !args.includes('--cursor') ? fx('worker-list-all-p1') : args.includes(cursorOf('worker-list-all-p1')) ? p2 : fx('worker-list-all-p3');
    await expect(reject(onRun('run_000000000601', realPages(fx('worker-list-all-p2'))))).resolves.toHaveLength(3);
    await expect(reject(onRun('run_000000000601', realPages(patched('worker-list-all-p2', (r) => ((r.page as Json).total = 8)))))).rejects.toMatchObject({ code: 'list_changed' });
    await expect(reject(onRun('run_000000000600', () => mine('run_000000000600', (r) => ((r.workers as Json[]).push((r.workers as Json[])[0]!), ((r.page as Json).total = 1)))))).rejects.toMatchObject({ code: 'list_changed' });
    await expect(reject(onRun('run_000000000600', () => mine('run_000000000600', (r) => ((r.page as Json).total = 2))))).rejects.toMatchObject({ code: 'list_changed' });
    await expect(reject(onRun('run_000000000600', () => mine('run_000000000600', (r) => ((r.page as Json).total = 0))))).rejects.toMatchObject({ code: 'list_changed' });
    // 页数上限：每个 Run 的 worker-list 最多 200 页（2 万个 dispatch），恰好 200 页时能翻完；run-list 最多 50 页
    const many = (n: number) => {
      const w = orcaWorld();
      w.runs.set('run_000000000600', Array.from({ length: n }, (_, i) => ({ dispatchId: `ctx_${String(i).padStart(12, '0')}`, terminalState: 'released', dispatchStatus: 'completed' })));
      return w;
    };
    const full = many(20_000);
    expect(await full.orca.unreleasedWorkers()).toHaveLength(3);
    expect(full.calls.filter((c) => c[3] === 'run_000000000600').length).toBe(200);
    const over = many(20_001);
    await expect(over.orca.unreleasedWorkers()).rejects.toMatchObject({ code: 'list_truncated', message: expect.stringContaining('20000') });
    expect(over.calls.filter((c) => c[3] === 'run_000000000600').length).toBe(200);
    const runList = (f: (r: Json) => void) => (w: World) => (w.override = (args) => (args[1] === 'run-list' ? patched('run-list', f) : undefined));
    const endlessRuns = orcaWorld();
    runList(() => undefined)(endlessRuns);
    await expect(endlessRuns.orca.unreleasedWorkers()).rejects.toMatchObject({ code: 'list_truncated' });
    expect(endlessRuns.calls.length).toBe(50);
    // run-list：没有 nextCursor 字段、nextCursor 不是字符串、缺少 runs、Run 没有 id
    await expect(reject(runList((r) => delete r.nextCursor))).rejects.toMatchObject({ code: 'bad_output' });
    await expect(reject(runList((r) => (r.nextCursor = 2)))).rejects.toMatchObject({ code: 'bad_output' });
    await expect(reject(runList((r) => delete r.runs))).rejects.toMatchObject({ code: 'bad_output' });
    await expect(reject(runList((r) => delete (r.runs as Json[])[0]!.id))).rejects.toMatchObject({ code: 'bad_output' });
    // 查询期间新建了 Run：它的 worker 没有查过
    await expect(
      reject((w) => {
        let lists = 0;
        w.override = (args) => {
          if (args[1] !== 'run-list' || args.includes('--cursor')) return undefined;
          return ++lists === 1 ? undefined : patched('run-list', (r) => (r.runs as Json[]).push({ ...(r.runs as Json[])[0], id: 'run_000000000604' }));
        };
      }),
    ).rejects.toMatchObject({ code: 'list_changed' });
    // 已有的 Run 在查询期间消失（编排状态被重置）不影响：它的 worker 也随之没有了
    const w = orcaWorld();
    let lists = 0;
    w.override = (args) => (args[1] === 'run-list' && args.includes('--cursor') && ++lists === 2 ? patched('run-list-last', (r) => (r.runs as Json[]).pop()) : undefined);
    expect(await w.orca.unreleasedWorkers()).toHaveLength(3);
  });

  it('ok:false 转为带 Orca 错误码的 OrcaError；没有输出时是 exec_failed', async () => {
    await expect(cli(() => ({ stdout: fx('run-use-not-found'), code: 1 })).orca.runUse('h', 'r')).rejects.toBeInstanceOf(OrcaError);
    await expect(cli(() => ({ stdout: '', code: 1 })).orca.workerRelease('d')).rejects.toMatchObject({ code: 'exec_failed' });
  });
});

// 本机执行不了 Orca 命令行：命令行在运行中消失、没有执行权限、脚本的解释器不在时，
// 真实的 nodeExec 得到的都是 exec_failed——结果未知，不是 Orca 的回答。停止、释放、查询、启动查证、删除工作区、终端存活
// 都不能因此当成 dispatch 不存在、已经停止、已经释放、没有这个请求或者工作区不在；命令行回来之后照常。
describe('Orca 命令行暂时执行不了', () => {
  const fixtures = path.join(import.meta.dirname, 'fixtures', 'orca');
  /** script 是按子命令回答夹具的 Orca 命令行替身（真实的可执行文件）。 */
  const script = (interpreter: string) => `#!${interpreter}
const fs = require('fs');
const a = process.argv.slice(2);
const sub = a[0] === 'orchestration' ? a[1] : a[0] === 'status' ? 'status' : a[0] + '-' + a[1];
const name = { 'worker-stop': 'worker-stop', 'worker-release': 'worker-release', 'worker-show': 'worker-show-running', 'request-show': 'request-show-absent', 'worktree-rm': 'worktree-rm', 'terminal-show': 'terminal-show', status: 'status' }[sub];
process.stdout.write(fs.readFileSync(${JSON.stringify(fixtures)} + '/' + name + '.json', 'utf8'));
`;

  it.each([
    ['消失', async (bin: string) => rm(bin)],
    ['没有执行权限', async (bin: string) => chmod(bin, 0o644)],
    ['脚本的解释器不在', async (bin: string) => writeFile(bin, script('/nonexistent/node'), { mode: 0o755 })],
  ] as const)('命令行%s：都是结果未知（exec_failed），恢复后照常', async (_what, breakIt) => {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-vanish-'));
    try {
      const bin = path.join(dir, 'orca-ide');
      await writeFile(bin, script(process.execPath), { mode: 0o755 });
      const orca = new OrcaCli(nodeExec, bin);
      expect(await orca.workerStop('ctx_1')).toBe(true);
      expect((await orca.status()).ready).toBe(true);

      await breakIt(bin);
      const errs = await Promise.all([
        orca.workerStop('ctx_1').then(() => undefined, (e: unknown) => e),
        orca.workerShow('ctx_1').then(() => undefined, (e: unknown) => e),
        orca.requestShow('0e3c3f5e-8a3b-4c7d-9f1e-2b6a1c0d9e8f').then(() => undefined, (e: unknown) => e),
        orca.worktreeRemove('repo::/x', false).then(() => undefined, (e: unknown) => e),
        orca.terminalShow('term_1').then(() => undefined, (e: unknown) => e),
        orca.terminalClose('term_1').then(() => undefined, (e: unknown) => e),
      ]);
      for (const err of errs) {
        // 每一个都抛出（没有返回"已停止 / 不存在 / 已删除 / 没有存活"），并且是结果未知
        expect(err).toBeInstanceOf(OrcaError);
        expect(err).toMatchObject({ code: 'exec_failed' });
        expect(dispatchNotFound(err)).toBe(false);
        expect(isDefinite(err)).toBe(false);
      }
      expect(await releaseWorker(orca, 'ctx_1')).toMatchObject({ kind: 'unknown', error: expect.stringContaining('exec_failed') });
      expect(await orca.status()).toMatchObject({ ready: false, reason: expect.stringContaining('exec_failed') });

      // 恢复（writeFile 的 mode 只在新建时生效）
      await writeFile(bin, script(process.execPath), { mode: 0o755 });
      await chmod(bin, 0o755);
      expect(await orca.workerStop('ctx_1')).toBe(true);
      expect(await releaseWorker(orca, 'ctx_1')).toMatchObject({ kind: 'done' });
      expect(await orca.requestShow('0e3c3f5e-8a3b-4c7d-9f1e-2b6a1c0d9e8f')).toEqual({ state: 'absent' });
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });
});
