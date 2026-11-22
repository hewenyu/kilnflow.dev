import { existsSync } from 'node:fs';
import { mkdir, readFile, rm, stat, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { Code, ConnectError } from '@connectrpc/connect';
import { afterEach, describe, expect, it } from 'vitest';
import { Outcome, ReportKind } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { AgentProbe, knownAgents } from '../src/core/agents.js';
import { Attempt, taskTmpDir } from '../src/core/attempt.js';
import { ErrorClass } from '../src/core/errors.js';
import { type Exec, nodeExec } from '../src/core/exec.js';
import { Git } from '../src/core/git.js';
import type { JournalRecord } from '../src/core/journal.js';
import type { CompleteInit } from '../src/core/kiln.js';
import { type AgentCtx, commitFile, devTask, type Env, FakeKiln, FakeServices, fastTiming, git, heartbeatGate, macrotask, makeEnv, recordLog, records, reviewTask, staleError, waitFor } from './harness.js';
import { KeyedMutex } from '../src/core/mutex.js';
import type { OrcaMessage } from '../src/core/orca.js';
import { ciLogTailBytes, maxCIOutputBytes } from '../src/core/verification.js';

// Agent 脚本、钩子中的失败由 test/setup.ts 在测试结束时检查（harness 的 Faults）
let env: Env | undefined;
afterEach(async () => {
  await env?.cleanup();
  env = undefined;
});

/** run 执行一个 attempt，Agent 的工具调用接到这个 attempt 上。 */
async function run(e: Env, at: Attempt, resumed = false): Promise<void> {
  e.orca.tool = (_id, cmd, args) => at.tool(cmd, args);
  const ac = new AbortController();
  const pump = e.deps.mailbox.pump(ac.signal);
  try {
    await at.run(resumed);
  } finally {
    ac.abort();
    await pump;
  }
}

function completed(e: Env) {
  expect(e.kiln.completes).toHaveLength(1);
  return e.kiln.completes[0]!;
}

const report = JSON.stringify({ version: 1, verdict: 'approve', summary: 'ok', findings: [] });

/**
 * wipAgent 是写出未提交的改动后一直运行、直到被停止的假 Agent。working 在改动写出后为 true：测试据此让 Server
 * 取消或失效，保证这时 Agent 已经启动（按心跳次数触发会在准备慢时早于启动，Agent 根本不会启动）。
 */
function wipAgent() {
  const a = {
    working: false,
    script: async (ctx: AgentCtx) => {
      await writeFile(path.join(ctx.worktree, 'wip.txt'), '一半\n');
      a.working = true;
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    },
  };
  return a;
}

describe('审计', () => {
  it('在 base 上建 worktree，校验报告后提交结果并释放 Agent', async () => {
    let seen: AgentCtx | undefined;
    let wtHead = '';
    let hasAgents = true;
    env = await makeEnv(async (ctx) => {
      seen = ctx;
      // 工作区在结束后回收：在 Agent 运行时查看
      wtHead = await git(ctx.worktree, 'rev-parse', 'HEAD');
      hasAgents = existsSync(path.join(ctx.worktree, 'AGENTS.md'));
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      const r = await ctx.tool('check-report');
      expect(r.code).toBe(0);
      ctx.done();
    });
    const base = await env.origin.branchSha('master');
    const head = await env.origin.commit('AGENTS.md', '被审 PR 改的说明\n', 'feature');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha: head, baseSha: base }));
    await run(env, at);

    const c = completed(env);
    expect(c.outcome).toBe(Outcome.SUCCEEDED);
    expect(c.result).toEqual({ case: 'reviewReport', value: new Uint8Array(Buffer.from(report)) });
    expect(c.binding?.dispatchId).toBe(seen?.dispatchId);
    // 审计的 worktree 从 base 开始：Agent 启动时加载的是 base 的说明，不是被审 PR 改过的
    expect(env.orca.starts[0]?.baseBranch).toBe(base);
    expect(wtHead).toBe(base);
    expect(hasAgents).toBe(false);
    expect(env.orca.starts[0]?.agent).toBe('claude');
    expect(env.orca.releases).toEqual([seen?.dispatchId]);
    expect(env.orca.stops).toEqual([]);
    // 结束后回收审计的工作区：Orca 中的工作区、它的分支都删除，journal 删除
    expect(await records(env.journal)).toEqual([]);
    expect(existsSync(seen!.worktree)).toBe(false);
    expect(env.orca.worktreeRemoves).toEqual([{ id: expect.stringMatching(new RegExp(`::.*/kiln-a${at.id}-e1$`)), force: false }]);
    expect(await git(path.join(env.deps.reposDir, '42', 'widgets'), 'for-each-ref', '--format=%(refname)', 'refs/heads/')).not.toContain(`kiln-a${at.id}-e1`);
    // 占位符由连接器替换
    expect(env.orca.starts[0]?.spec).toContain(`tool=kiln-orca task --attempt ${at.id}`);
    expect(env.orca.starts[0]?.spec).toContain('services=这个任务不需要测试服务。');
    expect(env.orca.starts[0]?.spec).not.toContain('{{');
  });

  it('报告不合格时 check-report 给出问题，最终结果为 ai.report', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), '{}');
      const r = await ctx.tool('check-report');
      expect(r.code).toBe(1);
      expect(r.output).toContain('缺少 verdict');
      ctx.done();
    });
    env.kiln.validateFn = () => ({ valid: false, problems: ['缺少 verdict'] });
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.FAILED);
    expect(c.errorClass).toBe(ErrorClass.report);
  });

  it('escalation 不算结束：自动回复后 Agent 继续，以后来的 worker_done 为准', async () => {
    env = await makeEnv(async (ctx) => {
      const e = env!;
      e.orca.send({ type: 'escalation', id: 'esc1', dispatchId: ctx.dispatchId, subject: '需要决定' });
      await waitFor(() => e.orca.replies.length === 1, 5_000, '回复');
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.SUCCEEDED);
    expect(env.orca.replies[0]).toMatch(/^esc1:.*worker_done/);
  });

  it('Agent 没有发送 worker_done 就退出时按 worker-show 结束', async () => {
    env = await makeEnv(async () => {
      // 什么都不写，直接退出
    });
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.FAILED);
    expect(c.errorClass).toBe(ErrorClass.report);
  });
});

describe('开发', () => {
  it('选题、领取、开发、交付：推送到 attempt 分支', async () => {
    env = await makeEnv(async (ctx) => {
      const cands = JSON.parse(await readFile(path.join(ctx.taskDir, 'candidates.json'), 'utf8')) as Array<{ number: number }>;
      await writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected: [cands[0]!.number] }));
      const claim = await ctx.tool('claim');
      expect(claim.code).toBe(0);
      expect(claim.output).toContain(`开始开发，说明在 ${ctx.taskDir}`);
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      expect((await ctx.tool('deliver')).code).toBe(0);
      ctx.done();
    });
    env.kiln.offers = [{ candidates: [{ number: 12, title: '加功能' }] }];
    env.kiln.claimFn = () => ({ claimed: true, workItemId: 9n, branch: 'kiln/dev-9', phase: 'implement', message: '开始开发，说明在 {{KILN_TASK_DIR}}' });
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, devTask(env.origin, { sha, mode: 'select' }));
    await run(env, at);

    expect(env.kiln.claims[0]?.issues).toEqual([12]);
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.SUCCEEDED);
    expect(c.result?.case).toBe('delivery');
    const d = c.result?.value as { branch: string; sha: string; checkpoint: boolean };
    expect(d.branch).toBe(`kiln/attempt-${at.id}`);
    expect(d.checkpoint).toBe(false);
    expect(await env.origin.branchSha(d.branch)).toBe(d.sha);
    expect(env.orca.starts[0]?.agent).toBe('codex');
  });

  it('任务池没有候选时不启动 Agent', async () => {
    env = await makeEnv(() => Promise.reject(new Error('不应启动')));
    env.kiln.offers = [{ candidates: [] }];
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, devTask(env.origin, { sha, mode: 'select' })));
    expect(env.orca.starts).toEqual([]);
    expect(completed(env).outcome).toBe(Outcome.SUCCEEDED);
  });

  it('领取失败且有新候选时让 Agent 重新选题', async () => {
    let second = '';
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected: [12] }));
      const r = await ctx.tool('claim');
      expect(r.code).toBe(3);
      expect(existsSync(path.join(ctx.taskDir, 'selection.json'))).toBe(false);
      second = await readFile(path.join(ctx.taskDir, 'candidates.json'), 'utf8');
      await writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected: [] }));
      expect((await ctx.tool('claim')).code).toBe(0);
      ctx.done();
    });
    env.kiln.offers = [{ candidates: [{ number: 12 }] }];
    let n = 0;
    env.kiln.claimFn = (req) =>
      n++ === 0 ? { claimed: false, reason: '#12 已被领取', candidates: [{ number: 13 }], message: '重新选题' } : { claimed: false, reason: req.issues?.length ? '' : '没有选中', message: '' };
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, devTask(env.origin, { sha, mode: 'select' })));
    expect(JSON.parse(second)).toEqual([expect.objectContaining({ number: 13 })]);
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.SUCCEEDED);
    expect(c.message).toBe('没有选中 Issue');
    expect(c.result?.case).toBeUndefined();
  });

  it('delivered 但没有 commit 时 deliver 拒绝，最终为 ai.delivery', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: 'x' }));
      const r = await ctx.tool('deliver');
      expect(r.code).toBe(1);
      expect(r.output).toContain('没有任何 commit');
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, devTask(env.origin, { sha, mode: 'implement' })));
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.FAILED);
    expect(c.errorClass).toBe(ErrorClass.delivery);
  });

  it('设计阶段改了代码时交付不合格', async () => {
    env = await makeEnv(async (ctx) => {
      await commitFile(ctx.worktree, 'main.go', 'package main\n', 'code');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: 'x' }));
      const r = await ctx.tool('deliver');
      expect(r.output).toContain('只能改 Markdown');
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, devTask(env.origin, { sha, mode: 'implement', phase: 'design' })));
    expect(completed(env).errorClass).toBe(ErrorClass.delivery);
  });

  it('ci 命令返回失败的 job 和日志尾部', async () => {
    let out = '';
    env = await makeEnv(async (ctx) => {
      out = (await ctx.tool('ci', { sha: 'abc' })).output;
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'blocked', summary: '卡住' }));
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, devTask(env.origin, { sha, mode: 'fix' })));
    expect(out).toContain('commit abc：failed');
    expect(out).toContain('FAIL x');
    expect(out).not.toContain('注意');
    expect(env.kiln.verifications[0]?.logTailBytes).toBe(ciLogTailBytes);
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.SUCCEEDED);
    expect((c.result?.value as { sha: string }).sha).toBe('');
  });

  it('ci 命令的输出有总量上限：失败的 job 很多时明确写出没有列出的 job 和日志', async () => {
    let out = '';
    env = await makeEnv(async (ctx) => {
      out = (await ctx.tool('ci', { sha: 'abc' })).output;
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'blocked', summary: '卡住' }));
      ctx.done();
    });
    const jobs = 40;
    env.kiln.verificationFn = (req) => ({
      sha: req.sha,
      state: 'failed',
      failedJobs: Array.from({ length: jobs }, (_, i) => ({ name: `job-${i}`, reason: 'exit 1', errorClass: 'user', logTail: Buffer.from(`job-${i} 的日志\n`.padEnd(ciLogTailBytes, 'x')) })),
    });
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, devTask(env.origin, { sha, mode: 'fix' })));
    expect(out).toContain(`（${jobs} 个失败的 job）`);
    // 前面的 job 带日志，没有全部列出，开头和末尾都写明
    expect(out).toContain('job-0 的日志');
    expect(out).not.toContain(`job-${jobs - 1} 的日志`);
    expect(out.split('\n')[1]).toMatch(/^注意：输出超过 \d+ 字节的上限，\d+ 个 job 的日志没有列出/);
    expect(out.trimEnd().split('\n').at(-1)).toMatch(/^注意：/);
    const note = out.split('\n')[1] ?? '';
    expect(Buffer.byteLength(out)).toBeLessThanOrEqual(maxCIOutputBytes + 2 * Buffer.byteLength(note) + 4);
  });
});

describe('停止', () => {
  it('取消时停止 Agent，未完成的改动推为 checkpoint', async () => {
    const agent = wipAgent();
    env = await makeEnv(agent.script);
    env.kiln.heartbeatFn = () => ({ cancelRequested: agent.working });
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, devTask(env.origin, { sha, mode: 'implement' }));
    await run(env, at);
    expect(env.orca.stops).toHaveLength(1);
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.CANCELLED);
    const d = c.result?.value as { branch: string; sha: string; checkpoint: boolean };
    expect(d.checkpoint).toBe(true);
    expect(await env.origin.branchSha(`kiln/attempt-${at.id}`)).toBe(d.sha);
  });

  it('attempt 失效时停止 Agent，不提交结果、不推 checkpoint', async () => {
    const agent = wipAgent();
    env = await makeEnv(agent.script);
    env.kiln.heartbeatFn = () => (agent.working ? staleError() : {});
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, devTask(env.origin, { sha, mode: 'implement' }));
    await run(env, at);
    expect(env.orca.stops).toHaveLength(1);
    expect(env.kiln.completes).toEqual([]);
    expect(await env.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
    // 失效后 Agent 的工具调用被拒绝
    expect((await at.tool('info', {})).code).toBe(1);
  });

  it('不能确认 Agent 停止时标记 unconfirmedStop，且不推 checkpoint', async () => {
    const agent = wipAgent();
    env = await makeEnv(agent.script);
    env.orca.stopConfirms = false;
    env.kiln.heartbeatFn = () => ({ cancelRequested: agent.working });
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, devTask(env.origin, { sha, mode: 'implement' }));
    await run(env, at);
    expect(env.orca.stops).toEqual([at.binding.dispatchId]);
    expect(at.unconfirmedStop).toBe(true);
    expect(completed(env).result?.case).toBeUndefined();
    expect(await env.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
    // 测试结束时让假 Agent 退出
    env.orca.stopConfirms = true;
    await env.orca.workerStop(at.binding.dispatchId);
  });

  it('连接器退出时不停止 Agent、不提交，journal 保留；重启后恢复并完成', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      await waitFor(() => resume, 5_000, '恢复');
      ctx.done();
    });
    let resume = false;
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    const running = run(env, at);
    await waitFor(() => env!.orca.starts.length === 1 && at.binding.dispatchId !== '', 5_000, '启动');
    at.shutdown();
    await running;
    expect(env.orca.stops).toEqual([]);
    expect(env.kiln.completes).toEqual([]);
    const [rec] = await records(env.journal);
    expect(rec?.stage).toBe('running');
    expect(rec?.binding.dispatchId).toBe(at.binding.dispatchId);

    resume = true;
    const again = Attempt.fromJournal(env.deps, rec!);
    await run(env, again, true);
    expect(env.orca.starts).toHaveLength(1);
    expect(completed(env).outcome).toBe(Outcome.SUCCEEDED);
    expect(await records(env.journal)).toEqual([]);
  });
});

describe('恢复后的日志', () => {
  /**
   * recovered 让一个审计执行到 Agent 启动后连接器退出，再从 journal 恢复并完成。shorten 为 true 时把 Server 上已有的日志换成
   * 很短的内容（比恢复后第一批日志短）。返回恢复前、后 Server 上这个 attempt 的日志。
   */
  async function recovered(shorten: boolean): Promise<{ before: Buffer; after: Buffer }> {
    let resume = false;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      await waitFor(() => resume, 5_000, '恢复');
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    const running = run(env, at);
    await waitFor(() => env!.orca.starts.length === 1 && at.binding.dispatchId !== '', 5_000, '启动');
    at.shutdown();
    await running;
    expect(env.kiln.logStore.get(at.id)?.toString()).toContain('已在 Orca 中启动');
    if (shorten) env.kiln.logStore.set(at.id, Buffer.from('earlier\n'));
    const before = env.kiln.logStore.get(at.id) ?? Buffer.alloc(0);
    const [rec] = await records(env.journal);
    resume = true;
    await run(env, Attempt.fromJournal(env.deps, rec!), true);
    expect(completed(env).outcome).toBe(Outcome.SUCCEEDED);
    return { before, after: env.kiln.logStore.get(at.id) ?? Buffer.alloc(0) };
  }

  function expectAppended({ before, after }: { before: Buffer; after: Buffer }) {
    // 原有的日志不变，恢复后的内容完整接在后面
    expect(after.subarray(0, before.length).equals(before)).toBe(true);
    const added = after.subarray(before.length).toString();
    expect(added.split('\n')[0]).toMatch(/^\d{4}-\d\d-\d\dT\S+ 连接器重启，恢复执行$/);
    expect(added).toContain('已结束（worker_done');
  }

  it('Server 上已有的日志比恢复后第一批长（会被当成重传丢掉的那种）：新日志接在后面', async () => {
    expectAppended(await recovered(false));
  });

  it('Server 上已有的日志比恢复后第一批短：新日志从 Server 的长度接着传，不被截掉开头', async () => {
    expectAppended(await recovered(true));
  });
});

describe('启动查证', () => {
  it('启动响应丢失时用 request-show 沿用已经启动的 Agent', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    env.orca.startFault = 'lost-after';
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    expect(env.orca.starts).toHaveLength(1);
    expect(env.orca.workers.size).toBe(1);
    expect(completed(env).outcome).toBe(Outcome.SUCCEEDED);
  });

  it('查不到启动请求时记为 infra.lost，绝不再启动一份', async () => {
    env = await makeEnv(() => Promise.resolve());
    env.orca.startFault = 'lost-before';
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    await run(env, at);
    expect(env.orca.starts).toHaveLength(1);
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.INFRA_ERROR);
    expect(c.errorClass).toBe(ErrorClass.lost);
    // 不能证明没有启动：继续跟踪这个启动请求（节点在查证窗口后按没有启动结束跟踪）
    expect(at.unconfirmedStop).toBe(true);
    const [rec] = await records(env.journal);
    expect(rec?.stage).toBe('stopping');
    expect(rec?.binding.startRequestId).toBe(env.orca.starts[0]?.requestId);
  });

  it('Orca 拒绝启动时记为 infra.agent，Agent 进入退避', async () => {
    env = await makeEnv(() => Promise.resolve());
    env.orca.startFault = 'refused';
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    await run(env, at);
    expect(completed(env).errorClass).toBe(ErrorClass.agent);
    // Orca 明确拒绝：没有要停的 Agent，不占用容量
    expect(at.unconfirmedStop).toBe(false);
    expect(await records(env.journal)).toEqual([]);
    expect(env.deps.agents.snapshot().find((a) => a.name === 'claude-code')?.backoffUntil).toBeGreaterThan(Date.now());
  });

  it('邮箱的 Run 已经不存在（编排状态被重置）时启动：Orca 拒绝（run_not_found），记为 infra.orca，Agent 不退避，邮箱在现有终端上换新 Run', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const old = await e.deps.mailbox.ensure();
    e.orca.runBindings.delete(old.runId);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    // 不运行邮箱循环：只由启动的拒绝触发换新
    await at.run();
    expect(e.orca.starts.map((s) => s.run)).toEqual([old.runId]);
    const c = completed(e);
    expect(c.outcome).toBe(Outcome.INFRA_ERROR);
    expect(c.errorClass).toBe(ErrorClass.orca);
    expect(c.message).toContain('编排状态被重置');
    expect(e.deps.agents.snapshot().find((a) => a.name === 'claude-code')?.backoffUntil).toBe(0);
    expect(at.unconfirmedStop).toBe(false);
    expect(await records(e.journal)).toEqual([]);
    const now = await e.deps.mailbox.ensure();
    expect(now.handle).toBe(old.handle);
    expect(now.runId).not.toBe(old.runId);
    expect(e.orca.runBindings.get(now.runId)).toBe(old.handle);
  });

  it('Agent 运行中 Orca 中的 dispatch 没有了（编排状态被重置）：worker-show 连续几次 dispatch_not_found 后以 infra.orca 结束，停止、释放按不存在完成，推 checkpoint', async () => {
    const agent = wipAgent();
    env = await makeEnv(agent.script);
    const e = env;
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    const running = run(e, at);
    await waitFor(() => agent.working && at.binding.dispatchId !== '', 5_000, 'Agent 运行');
    const d = at.binding.dispatchId;
    const began = Date.now();
    e.orca.resetOrchestration();
    await running;
    // 宽限：连续 settledGraceChecks + 1 次 worker-show（showMs 间隔）
    expect(Date.now() - began).toBeLessThan(fastTiming.showMs * 50);
    const c = completed(e);
    expect(c.outcome).toBe(Outcome.INFRA_ERROR);
    expect(c.errorClass).toBe(ErrorClass.orca);
    expect(c.message).toContain(`dispatch ${d}`);
    expect(c.message).toContain('编排状态被重置');
    expect(e.orca.stops).toEqual([d]);
    expect(e.orca.releases).toEqual([d]);
    expect(at.unconfirmedStop).toBe(false);
    // 未完成的改动推到 checkpoint
    const cp = c.result?.value as { branch: string; sha: string; checkpoint: boolean };
    expect(cp.checkpoint).toBe(true);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe(cp.sha);
    expect(await records(e.journal)).toEqual([]);
  });

  it('journal 中有启动幂等键时重启后先查证，不重复启动', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    // 模拟：启动请求已经发出、Orca 已经启动，但连接器在写回执之前崩溃
    env.orca.startFault = 'lost-after';
    const running = run(env, at);
    await waitFor(() => env!.orca.starts.length === 1, 5_000, '启动');
    at.shutdown();
    await running;
    const [rec] = await records(env.journal);
    expect(rec?.binding.startRequestId).toBeTruthy();
    rec!.binding.dispatchId = '';
    rec!.stage = 'start';
    await run(env, Attempt.fromJournal(env.deps, rec!), true);
    expect(env.orca.starts).toHaveLength(1);
    expect(completed(env).outcome).toBe(Outcome.SUCCEEDED);
  });
});

describe('claim 的回复丢失后重试', () => {
  const selection = (ctx: AgentCtx, selected: number[]) => writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected }));

  it('领取成功后重试 claim：原样重放完整的说明（同样的退出码），Server 只领取一次；连接器重启后重试仍然相同', async () => {
    const outs: Array<{ code: number; output: string }> = [];
    let resume = false;
    env = await makeEnv(async (ctx) => {
      await selection(ctx, [12]);
      outs.push(await ctx.tool('claim'));
      // 第一次的回复没有送到：Agent 重试
      outs.push(await ctx.tool('claim'));
      await waitFor(() => resume, 5_000, '恢复');
      outs.push(await ctx.tool('claim'));
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      ctx.done();
    });
    const e = env;
    e.kiln.offers = [{ candidates: [{ number: 12, title: '加功能' }] }];
    e.kiln.claimFn = () => ({
      claimed: true,
      workItemId: 9n,
      branch: 'kiln/dev-9',
      phase: 'implement',
      message: '实现阶段的要求……delivery.json 的 Schema 见 {{KILN_TASK_DIR}}/delivery-schema.json，完成后运行 {{KILN_TOOL}} deliver，再运行 {{KILN_TOOL}} done --outcome succeeded',
    });
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'select' }));
    const running = run(e, at);
    await waitFor(() => outs.length === 2, 5_000, '两次 claim');
    at.shutdown();
    await running;
    const [rec] = await records(e.journal);
    resume = true;
    await run(e, Attempt.fromJournal(e.deps, rec!), true);

    expect(outs[0]).toEqual({ code: 0, output: `实现阶段的要求……delivery.json 的 Schema 见 ${at.taskDir}/delivery-schema.json，完成后运行 kiln-orca task --attempt ${at.id} deliver，再运行 kiln-orca task --attempt ${at.id} done --outcome succeeded` });
    expect(outs[1]).toEqual(outs[0]);
    expect(outs[2]).toEqual(outs[0]);
    expect(e.kiln.claims).toHaveLength(1);
    expect(completed(e).outcome).toBe(Outcome.SUCCEEDED);
  });

  it('没有领取（结束说明）后重试：重放同一个回复，不再调用 Server', async () => {
    const outs: Array<{ code: number; output: string }> = [];
    env = await makeEnv(async (ctx) => {
      await selection(ctx, [12]);
      outs.push(await ctx.tool('claim'));
      outs.push(await ctx.tool('claim'));
      ctx.done();
    });
    const e = env;
    e.kiln.offers = [{ candidates: [{ number: 12 }] }];
    let n = 0;
    // 第二次调用 Server 会领取成功：重试不能改变第一次的结果
    e.kiln.claimFn = () => (n++ === 0 ? { claimed: false, reason: '#12 已被领取', message: '没有领取，发送 worker_done 结束' } : { claimed: true, workItemId: 9n, branch: 'kiln/dev-9', phase: 'implement', message: '领取成功' });
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, devTask(e.origin, { sha, mode: 'select' })));
    expect(outs[0]).toEqual({ code: 0, output: '没有领取，发送 worker_done 结束' });
    expect(outs[1]).toEqual(outs[0]);
    expect(e.kiln.claims).toHaveLength(1);
    expect(completed(e).message).toBe('没有领取到 Issue：#12 已被领取');
  });

  it('领取冲突（已换新候选）后、写新选题之前重试：重放"重新选题"的回复（退出码 3）；写了新选题后照常领取', async () => {
    const outs: Array<{ code: number; output: string }> = [];
    let saved: unknown;
    env = await makeEnv(async (ctx) => {
      await selection(ctx, [12]);
      outs.push(await ctx.tool('claim'));
      // 回复先写入 journal（连接器重启后重试同样重放）
      saved = (await records(env!.journal))[0]?.claimReply;
      outs.push(await ctx.tool('claim'));
      await selection(ctx, [13]);
      outs.push(await ctx.tool('claim'));
      outs.push(await ctx.tool('claim'));
      ctx.done();
    });
    const e = env;
    e.kiln.offers = [{ candidates: [{ number: 12 }] }];
    let n = 0;
    e.kiln.claimFn = () =>
      n++ === 0 ? { claimed: false, reason: '#12 已被领取', candidates: [{ number: 13 }], message: '重新选题：候选在 {{KILN_TASK_DIR}}' } : { claimed: true, workItemId: 9n, branch: 'kiln/dev-9', phase: 'implement', message: '领取成功' };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'select' }));
    await run(e, at);
    expect(outs[0]).toEqual({ code: 3, output: `重新选题：候选在 ${at.taskDir}` });
    expect(saved).toEqual(outs[0]);
    expect(outs[1]).toEqual(outs[0]);
    expect(outs[2]).toEqual({ code: 0, output: '领取成功' });
    expect(outs[3]).toEqual(outs[2]);
    expect(e.kiln.claims.map((c) => c.issues)).toEqual([[12], [13]]);
  });
});

describe('写盘失败时回滚内存（审计）', () => {
  /** failSave 让第一次满足 pred 的 journal 写入失败（之后照常写）。 */
  function failSave(e: Env, pred: (r: JournalRecord) => boolean): void {
    const real = e.journal.save.bind(e.journal);
    let failed = false;
    e.journal.save = (r) => {
      if (!failed && pred(r)) {
        failed = true;
        return Promise.reject(new Error('磁盘满'));
      }
      return real(r);
    };
  }

  it('领取成功后写 journal 失败：回复是失败；重试重新领取（不会命中没有持久化的结果），journal 里最终是领取结果', async () => {
    const outs: Array<{ code: number; output: string }> = [];
    let saved: JournalRecord['claim'];
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected: [12] }));
      outs.push(await ctx.tool('claim'));
      outs.push(await ctx.tool('claim'));
      saved = (await records(env!.journal))[0]?.claim;
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      ctx.done();
    });
    const e = env;
    e.kiln.offers = [{ candidates: [{ number: 12 }] }];
    failSave(e, (r) => !!r.claim);
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, devTask(e.origin, { sha, mode: 'select' })));
    expect(outs[0]).toMatchObject({ code: 1, output: expect.stringContaining('磁盘满') });
    expect(outs[1]?.code).toBe(0);
    // 第一次领取没有持久化：重试重新走一遍领取
    expect(e.kiln.claims.map((c) => c.issues)).toEqual([[12], [12]]);
    expect(saved).toMatchObject({ workItemId: '9' });
    expect(completed(e).outcome).toBe(Outcome.SUCCEEDED);
  });

  it('候选文件写失败不消耗重新选题的轮次：重试仍是同一轮，写成功后照常重新选题（不会因轮次用尽变成"没有领取"）', async () => {
    const outs: Array<{ code: number; output: string }> = [];
    let saved: JournalRecord | undefined;
    env = await makeEnv(async (ctx) => {
      const selection = (selected: number[]) => writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected }));
      await selection([12]);
      // candidates.json 是目录：writeCandidates 写不进去（磁盘、权限问题的替身）
      const blocker = path.join(ctx.taskDir, 'candidates.json');
      await rm(blocker, { force: true });
      await mkdir(blocker);
      outs.push(await ctx.tool('claim'));
      outs.push(await ctx.tool('claim'));
      await rm(blocker, { recursive: true, force: true });
      outs.push(await ctx.tool('claim'));
      // 结束时会删掉记录，在结束之前读
      saved = (await records(env!.journal))[0];
      ctx.done();
    });
    const e = env;
    e.kiln.offers = [{ candidates: [{ number: 12 }] }];
    e.kiln.claimFn = () => ({ claimed: false, reason: '#12 已被领取', candidates: [{ number: 13 }], message: '重新选题' });
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, devTask(e.origin, { sha, mode: 'select' })));
    expect(outs[0]).toMatchObject({ code: 1 });
    expect(outs[1]).toMatchObject({ code: 1 });
    // 前两次写盘失败没有算轮次：第三次写成功时还是第 1 轮（否则会因为到达上限变成"没有领取"的 code 0）
    expect(outs[2]).toEqual({ code: 3, output: '重新选题' });
    expect(saved?.selectRounds).toBe(1);
    expect(saved?.claimReply).toEqual(outs[2]);
  });

  it('task done 写 journal 失败：回复是失败；重试生成新的请求 id 并写入 journal', async () => {
    const outs: Array<{ code: number; output: string }> = [];
    let saved: JournalRecord['done'];
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      outs.push(await ctx.tool('done', { outcome: 'succeeded' }));
      outs.push(await ctx.tool('done', { outcome: 'succeeded' }));
      saved = (await records(env!.journal))[0]?.done;
      ctx.done();
    });
    const e = env;
    failSave(e, (r) => !!r.done);
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(outs[0]).toMatchObject({ code: 1, output: expect.stringContaining('磁盘满') });
    expect(outs[1]?.code).toBe(0);
    const send = JSON.parse(outs[1]!.output) as { retryRequest: string };
    // 失败的请求 id 没有留下：journal 中的就是这次回复的
    expect(saved?.retryRequest).toBe(send.retryRequest);
    expect(completed(e).outcome).toBe(Outcome.SUCCEEDED);
  });
});

describe('任务日志的上传带停止信号（审计）', () => {
  it('心跳上传日志时带上 attempt 的停止信号；结束时这些信号都已中止', async () => {
    let flushed = false;
    let claimed: { code: number; output: string } | undefined;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected: [12] }));
      claimed = await ctx.tool('claim');
      // 等测试看到一次带信号的上传（心跳周期在 fastTiming 中是 20 ms）
      while (!flushed) await new Promise((r) => setTimeout(r, 5));
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      ctx.done();
    });
    const e = env;
    e.kiln.offers = [{ candidates: [{ number: 12 }] }];
    const sha = await e.origin.branchSha('master');
    const running = run(e, new Attempt(e.deps, devTask(e.origin, { sha, mode: 'select' })));
    await waitFor(() => e.kiln.logSignals.some((s) => s !== undefined), 10_000, '带信号的上传');
    flushed = true;
    await running;
    expect(claimed?.code).toBe(0);
    const signals = e.kiln.logSignals.filter((s): s is AbortSignal => s !== undefined);
    expect(signals.length).toBeGreaterThan(0);
    expect(signals.every((s) => s.aborted)).toBe(true);
  });
});

describe('工具', () => {
  it('只接受与任务类型相符的命令', async () => {
    env = await makeEnv(async (ctx) => {
      expect((await ctx.tool('claim')).code).toBe(2);
      expect((await ctx.tool('deliver')).code).toBe(2);
      expect((await ctx.tool('nope')).code).toBe(2);
      const info = JSON.parse((await ctx.tool('info')).output) as { kind: string; worktree: string };
      expect(info.kind).toBe('review');
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    expect(completed(env).outcome).toBe(Outcome.SUCCEEDED);
  });

  it('报告种类正确地交给 Server 校验', async () => {
    const kinds: ReportKind[] = [];
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      await ctx.tool('check-report');
      ctx.done();
    });
    env.kiln.validateFn = (r) => {
      kinds.push(r.kind!);
      return { valid: true };
    };
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    expect(kinds).toEqual([ReportKind.REVIEW, ReportKind.REVIEW]);
  });
});

describe('提交结果', () => {
  const agent = async (ctx: AgentCtx) => {
    await writeFile(path.join(ctx.taskDir, 'report.json'), report);
    ctx.done();
  };

  it('网络错误时重试提交', async () => {
    env = await makeEnv(agent);
    env.kiln.completeFault = new ConnectError('down', Code.Unavailable);
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    expect(completed(env).outcome).toBe(Outcome.SUCCEEDED);
  });

  it('已经有结果（ALREADY_EXISTS）时不重试，照常清理', async () => {
    env = await makeEnv(agent);
    env.kiln.completeFault = new ConnectError('ALREADY_EXISTS', Code.AlreadyExists);
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    expect(env.kiln.completes).toEqual([]);
    expect(await records(env.journal)).toEqual([]);
  });
});

/** within 在 ms 内等到 p，否则失败（用来证明不会一直卡住）。 */
async function within<T>(p: Promise<T>, ms: number, what: string): Promise<T> {
  let t: NodeJS.Timeout | undefined;
  const timeout = new Promise<never>((_, reject) => {
    t = setTimeout(() => reject(new Error(`${what}超过 ${ms}ms`)), ms);
  });
  try {
    return await Promise.race([p, timeout]);
  } finally {
    clearTimeout(t);
  }
}

/**
 * watchWrites 记下 attempt 结束时的写入：带结论的 journal 写入（verdicts）、开始推送（Git.push，pushes）、提交
 * checkpoint（commitCheckpoint，checkpoints）。失效之后这些都不应再开始。
 */
function watchWrites(e: Env) {
  const w = { verdicts: 0, pushes: 0, checkpoints: 0 };
  const save = e.journal.save.bind(e.journal);
  e.journal.save = (r) => {
    if (r.verdict) w.verdicts++;
    return save(r);
  };
  const push = e.deps.git.push.bind(e.deps.git);
  e.deps.git.push = (...a) => {
    w.pushes++;
    return push(...a);
  };
  const commit = e.deps.git.commitCheckpoint.bind(e.deps.git);
  e.deps.git.commitCheckpoint = (...a) => {
    w.checkpoints++;
    return commit(...a);
  };
  return w;
}

describe('失效优先（F3）', () => {
  it('结束时校验交付报告返回失效：不推送交付、不提交、不再停止 Agent，journal 删除', async () => {
    env = await makeEnv(async (ctx) => {
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      ctx.done();
    });
    env.kiln.validateFn = () => staleError();
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, devTask(env.origin, { sha, mode: 'implement' }));
    await run(env, at);
    expect(await env.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
    expect(env.kiln.completes).toEqual([]);
    expect(env.kiln.completeCalls).toBe(0);
    // Agent 已经结束这一轮（已释放），不再停止
    expect(env.orca.stops).toEqual([]);
    expect(env.orca.releases).toEqual([at.binding.dispatchId]);
    expect(await records(env.journal)).toEqual([]);
  });

  it('结束时校验交付期间心跳返回失效：不记结论、不推送交付、不提交', async () => {
    env = await makeEnv(async (ctx) => {
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      ctx.done();
    });
    const e = env;
    const gate = heartbeatGate(e, staleError);
    const w = watchWrites(e);
    // 校验完成（交付合格：没有失效就会推送）之后、返回之前，心跳返回失效并被处理
    const check = e.deps.git.checkDelivered.bind(e.deps.git);
    let problems: string[] | undefined;
    let returned = false;
    e.deps.git.checkDelivered = async (wt, o) => {
      problems = await check(wt, o);
      await gate.fire();
      returned = true;
      return problems;
    };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    // 同步点之后的代码执行到了：校验结果原样交回，由产品代码自己决定不推送
    expect(returned).toBe(true);
    expect(problems).toEqual([]);
    // 失效之后 rpc 在入口拒绝：心跳不再到达 Server
    expect(e.kiln.heartbeats).toHaveLength(gate.answered);
    expect(w.verdicts).toBe(0);
    expect(w.pushes).toBe(0);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
    expect(e.kiln.completeCalls).toBe(0);
    expect(e.orca.stops).toEqual([]);
    expect(await records(e.journal)).toEqual([]);
  });

  it('记下结论（交付合格）期间失效：不开始推送、不提交', async () => {
    env = await makeEnv(async (ctx) => {
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      ctx.done();
    });
    const e = env;
    const gate = heartbeatGate(e, staleError);
    const w = watchWrites(e);
    // 结论（要推送交付）已经写进 journal、save 返回之前失效
    const save = e.journal.save.bind(e.journal);
    let saved: JournalRecord | undefined;
    e.journal.save = async (r) => {
      await save(r);
      if (saved || !r.verdict) return;
      saved = structuredClone(r);
      await gate.fire();
    };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    expect(saved?.verdict?.push).toBe(true);
    expect(gate.passed).toBe(true);
    expect(e.kiln.heartbeats).toHaveLength(gate.answered);
    expect(w.pushes).toBe(0);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
    expect(e.kiln.completeCalls).toBe(0);
    expect(await records(e.journal)).toEqual([]);
  });

  it('推送交付失败期间失效：不再尝试 checkpoint（不动 worktree）、不提交', async () => {
    env = await makeEnv(async (ctx) => {
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      ctx.done();
    });
    const e = env;
    const gate = heartbeatGate(e, staleError);
    // git push 已经开始（读完 HEAD、过了 guard）时失效，push 本身失败（例如远端拒绝）：之后本该尝试 checkpoint
    let pushFailed = false;
    const exec: Exec = async (cmd, args, opts) => {
      if (args[0] !== 'push') return nodeExec(cmd, args, opts);
      await gate.fire();
      pushFailed = true;
      return { code: 1, stdout: '', stderr: 'remote rejected' };
    };
    e.deps.git = new Git(exec);
    const w = watchWrites(e);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    expect(pushFailed).toBe(true);
    expect(e.kiln.heartbeats).toHaveLength(gate.answered);
    expect(w.pushes).toBe(1);
    expect(w.checkpoints).toBe(0);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
    expect(e.kiln.completeCalls).toBe(0);
    expect(await records(e.journal)).toEqual([]);
  });

  it('取消后停止 Agent 期间失效：不提交 checkpoint、不推送、不提交', async () => {
    const agent = wipAgent();
    env = await makeEnv(agent.script);
    const e = env;
    const log = recordLog(e);
    const gate = heartbeatGate(e, staleError, () => ({ cancelRequested: agent.working }));
    const w = watchWrites(e);
    // Agent 确认停止之后、返回之前失效（之后本该提交 checkpoint、推送）
    const stop = e.orca.workerStop.bind(e.orca);
    e.orca.workerStop = async (d) => {
      const ok = await stop(d);
      await gate.fire();
      return ok;
    };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    expect(gate.passed).toBe(true);
    expect(e.orca.stops).toEqual([at.binding.dispatchId]);
    expect(e.kiln.heartbeats).toHaveLength(gate.answered);
    // 未完成的改动没有被提交为 checkpoint；工作区照样回收，日志写明删掉了没有推送的改动（PLAN #33.10）
    expect(w.checkpoints).toBe(0);
    expect(existsSync(at.binding.worktreePath)).toBe(false);
    expect(log.find((l) => l.msg.startsWith('回收开发的工作区：删除了没有推送的改动'))?.fields).toMatchObject({ attempt: String(at.id), pushed: '', dirtyFiles: 1, dirty: expect.stringContaining('wip.txt') });
    expect(w.pushes).toBe(0);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
    expect(e.kiln.completeCalls).toBe(0);
  });

  it('取消后、推 checkpoint 之前失效：不推 checkpoint、不提交', async () => {
    const agent = wipAgent();
    env = await makeEnv(agent.script);
    const e = env;
    const gate = heartbeatGate(e, staleError, () => ({ cancelRequested: agent.working }));
    // checkpoint 已经提交到 worktree、还没推送时失效
    const commit = e.deps.git.commitCheckpoint.bind(e.deps.git);
    let changed: boolean | undefined;
    let returned = false;
    e.deps.git.commitCheckpoint = async (wt, start, id) => {
      changed = await commit(wt, start, id);
      await gate.fire();
      returned = true;
      return changed;
    };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    // 同步点之后的代码执行到了：checkpoint commit 已经建好（没有失效就会推送），由产品代码自己决定不推送
    expect(returned).toBe(true);
    expect(changed).toBe(true);
    expect(e.kiln.heartbeats).toHaveLength(gate.answered);
    expect(e.orca.stops).toHaveLength(1);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
    expect(e.kiln.completeCalls).toBe(0);
  });
});

describe('RPC 期限（F4）', () => {
  it('心跳不响应时按断连结束，不会一直等', async () => {
    env = await makeEnv(async (ctx) => {
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    env.kiln.heartbeatHang = true;
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    await within(run(env, at), env.deps.timing.maxDisconnectedMs + 5_000, '断连后结束');
    expect(env.orca.stops).toEqual([at.binding.dispatchId]);
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.INFRA_ERROR);
    expect(c.errorClass).toBe(ErrorClass.disconnected);
  });

  it('心跳卡住时连接器退出：run 很快返回，journal 保留，Agent 不停止', async () => {
    env = await makeEnv(async (ctx) => {
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    // RPC 期限很长：退出只能靠打断进行中的心跳
    env.deps.timing = { ...env.deps.timing, rpcMs: 60_000 };
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    const running = run(env, at);
    await waitFor(async () => (await records(env!.journal))[0]?.stage === 'running', 5_000, '运行');
    env.kiln.heartbeatHang = true;
    const n = env.kiln.heartbeats.length;
    await waitFor(() => env!.kiln.heartbeats.length > n, 5_000, '卡住的心跳');
    at.shutdown();
    await within(running, 2_000, '退出');
    expect(env.orca.stops).toEqual([]);
    expect(env.kiln.completeCalls).toBe(0);
    expect((await records(env.journal))[0]?.stage).toBe('running');
    // 测试结束时让假 Agent 退出
    await env.orca.workerStop(at.binding.dispatchId);
  });
});

describe('审计的 base（F5）', () => {
  it('拿不到 base 时记为 infra.fetch，不启动 Agent', async () => {
    env = await makeEnv(() => Promise.reject(new Error('不应启动')));
    const head = await env.origin.commit('AGENTS.md', '被审 PR 改的说明\n', 'feature');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha: head, baseSha: 'deadbeef'.repeat(5) })));
    expect(env.orca.starts).toEqual([]);
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.INFRA_ERROR);
    expect(c.errorClass).toBe(ErrorClass.fetch);
  });

  it('没有 base（手动审计）时从被审的 commit 开始', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const head = await env.origin.commit('x.txt', 'x\n', 'feature');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha: head, baseSha: '' })));
    expect(env.orca.starts[0]?.baseBranch).toBe(head);
    expect(completed(env).outcome).toBe(Outcome.SUCCEEDED);
  });
});

describe('kiln-task skill', () => {
  it('启动 Agent 之前装好 skill，prompt 中的 {{KILN_SKILL}} 换成入口文件', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const head = await env.origin.commit('x.txt', 'x\n', 'feature');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha: head, baseSha: '' })));
    expect(env.skill.ensured).toEqual(['claude-code']);
    const prompt = env.orca.starts[0]?.spec ?? '';
    expect(prompt).toContain('skill=/fake/skills/claude-code/kiln-task/SKILL.md');
    expect(prompt).not.toContain('{{KILN_SKILL}}');
    expect(completed(env).outcome).toBe(Outcome.SUCCEEDED);
  });

  it('装不上 skill 时不启动 Agent，记为 infra.prepare', async () => {
    env = await makeEnv(() => Promise.reject(new Error('不应启动')));
    env.skill.failFor = (agent) => `不能写 ${agent} 的目录：磁盘满了`;
    const head = await env.origin.commit('x.txt', 'x\n', 'feature');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha: head, baseSha: '' })));
    expect(env.orca.starts).toEqual([]);
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.INFRA_ERROR);
    expect(c.errorClass).toBe(ErrorClass.prepare);
    expect(c.message).toContain('kiln-task skill');
    expect(c.message).toContain('磁盘满了');
  });

  it('装不上 skill 时 journal 里没有写入启动幂等键（不变量：有 startRequestId ⇔ 可能已经发出）', async () => {
    env = await makeEnv(() => Promise.reject(new Error('不应启动')));
    const e = env;
    const saves: JournalRecord[] = [];
    const save = e.journal.save.bind(e.journal);
    e.journal.save = async (r) => {
      saves.push(structuredClone(r));
      return save(r);
    };
    e.skill.failFor = () => '不能写目录';
    const head = await e.origin.commit('x.txt', 'x\n', 'feature');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha: head, baseSha: '' })));
    expect(e.orca.starts).toEqual([]);
    expect(saves.length).toBeGreaterThan(0);
    expect(saves.every((r) => r.binding.startRequestId === '')).toBe(true);
  });

  it('恢复已经启动的任务时不再装 skill：即使 skill 同步现在报错也照常完成', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      await waitFor(() => resume, 5_000, '恢复');
      ctx.done();
    });
    let resume = false;
    const e = env;
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    const running = run(e, at);
    await waitFor(() => e.orca.starts.length === 1 && at.binding.dispatchId !== '', 5_000, '启动');
    at.shutdown();
    await running;
    const [rec] = await records(e.journal);
    expect(rec?.binding.dispatchId).toBe(at.binding.dispatchId);
    expect(e.skill.ensured).toEqual(['claude-code']);

    // 恢复时 skill 目录变得不可写：已有启动幂等键，不需要也不应该再装 skill
    e.skill.failFor = () => '恢复期间 skill 目录不可写';
    resume = true;
    const again = Attempt.fromJournal(e.deps, rec!);
    await run(e, again, true);
    expect(completed(e).outcome).toBe(Outcome.SUCCEEDED);
    expect(e.skill.ensured).toEqual(['claude-code']);
  });
});

describe('Agent 结束后（F8）', () => {
  it('正在结束时连接器退出：结果照常提交一次，journal 删除', async () => {
    let validating = false;
    let release = false;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    env.kiln.validateFn = async () => {
      validating = true;
      await waitFor(() => release, 5_000, '放行校验');
      return { valid: true };
    };
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    const running = run(env, at);
    await waitFor(() => validating, 5_000, '开始校验');
    at.shutdown();
    release = true;
    await running;
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.SUCCEEDED);
    expect(env.orca.stops).toEqual([]);
    expect(await records(env.journal)).toEqual([]);
  });

  it('提交结果一直失败时连接器退出：不再重试，journal 保留在 finishing', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    env.kiln.completeDown = new ConnectError('down', Code.Unavailable);
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    const running = run(env, at);
    await waitFor(() => env!.kiln.completeCalls >= 1, 5_000, '提交');
    at.shutdown();
    const calls = env.kiln.completeCalls;
    await within(running, 2_000, '退出');
    expect(env.kiln.completeCalls).toBe(calls);
    expect(env.kiln.completes).toEqual([]);
    expect(env.orca.stops).toEqual([]);
    const [rec] = await records(env.journal);
    expect(rec?.stage).toBe('finishing');
    // 要提交的请求已经记下：重启后重发它
    expect(rec?.complete?.outcome).toBe(Outcome.SUCCEEDED);
  });
});

/**
 * exitAtComplete 让第一次提交失败并让连接器退出（提交没有送达），返回记下的请求。afterPush 时还记下推送之后、
 * 请求记下之前的 journal（模拟在这时崩溃）。
 */
function exitAtComplete(e: Env, at: Attempt) {
  const seen: { req?: CompleteInit; afterPush?: JournalRecord; pushes: number; validates: number } = { pushes: 0, validates: 0 };
  e.kiln.complete = (req) => {
    seen.req = req;
    at.shutdown();
    return Promise.reject(new ConnectError('down', Code.Unavailable));
  };
  const validate = e.kiln.validateFn;
  e.kiln.validateFn = (req) => {
    seen.validates++;
    return validate(req);
  };
  const push = e.deps.git.push.bind(e.deps.git);
  e.deps.git.push = async (wt, url, id) => {
    const r = await push(wt, url, id);
    seen.pushes++;
    seen.afterPush ??= structuredClone((await e.faults.guard(() => records(e.journal)))[0]);
    return r;
  };
  return seen;
}

/** restart 恢复 Server 的 Complete，从 rec 重启 attempt。 */
async function restart(e: Env, rec: JournalRecord): Promise<void> {
  e.kiln.complete = Object.getPrototypeOf(e.kiln).complete.bind(e.kiln);
  await e.journal.save(rec);
  await run(e, Attempt.fromJournal(e.deps, rec), true);
}

const delivered = JSON.stringify({ status: 'delivered', summary: '完成' });

describe('结束的恢复（R1）', () => {
  it('交付不合格、推了 checkpoint 后、提交前退出：恢复后不重新校验，提交与第一次完全相同', async () => {
    env = await makeEnv(async (ctx) => {
      // delivered，但改动没有 commit：交付不合格，改动推为 checkpoint
      await writeFile(path.join(ctx.worktree, 'uncommitted.go'), 'package x\n');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), delivered);
      ctx.done();
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    const seen = exitAtComplete(e, at);
    await run(e, at);
    const first = seen.req!;
    expect(first.outcome).toBe(Outcome.FAILED);
    expect(first.errorClass).toBe(ErrorClass.delivery);
    expect(first.message).toContain('未提交');
    const d = first.result?.value as { sha: string; checkpoint: boolean };
    expect(d.checkpoint).toBe(true);
    expect(seen.afterPush?.verdict).toBeDefined();
    expect(seen.afterPush?.complete).toBeUndefined();
    const validates = seen.validates;

    // checkpoint 已经把 worktree 改干净：重新校验会得出"交付成功"
    await restart(e, seen.afterPush!);
    expect(seen.validates).toBe(validates);
    expect(completed(e)).toEqual(first);
    expect(e.orca.starts).toHaveLength(1);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe(d.sha);
    expect(await records(e.journal)).toEqual([]);
  });

  it('成功交付推送后、提交前退出：恢复后不再校验，重推同一个 commit 并提交相同的请求', async () => {
    env = await makeEnv(async (ctx) => {
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), delivered);
      ctx.done();
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    const seen = exitAtComplete(e, at);
    await run(e, at);
    const first = seen.req!;
    expect(first.outcome).toBe(Outcome.SUCCEEDED);
    const d = first.result?.value as { sha: string; checkpoint: boolean };
    expect(d.checkpoint).toBe(false);
    const validates = seen.validates;
    expect(validates).toBeGreaterThan(0);

    await restart(e, seen.afterPush!);
    expect(seen.validates).toBe(validates);
    expect(seen.pushes).toBe(2);
    expect(completed(e)).toEqual(first);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe(d.sha);
  });

  it('请求已经记下（提交没有送达）：恢复后不推送、不校验，重发同一个请求（Agent 版本取自 journal）', async () => {
    env = await makeEnv(async (ctx) => {
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), delivered);
      ctx.done();
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    const seen = exitAtComplete(e, at);
    await run(e, at);
    const first = seen.req!;
    const [rec] = await records(e.journal);
    expect(rec?.complete).toBeDefined();
    const { pushes, validates } = seen;

    // 重启前 Agent 升级了：提交的仍是执行时的版本
    e.deps.agents = new AgentProbe((cmd) => Promise.resolve({ code: 0, stdout: `${cmd} 9.9.9\n`, stderr: '' }), knownAgents);
    await e.deps.agents.refresh(true);
    await restart(e, rec!);
    expect(seen.pushes).toBe(pushes);
    expect(seen.validates).toBe(validates);
    expect(e.kiln.completeCalls).toBe(1);
    expect(completed(e)).toEqual(first);
    expect(first.agentVersion).not.toBe(e.deps.agents.version(at.spec.agent));
    expect(await records(e.journal)).toEqual([]);
  });
});

describe('日志发现失效（F3-R）', () => {
  const cases = [
    { name: '交付通过校验时不推送交付', status: 'delivered', wip: false },
    { name: '有未完成的改动时不推 checkpoint', status: 'blocked', wip: true },
  ];
  for (const c of cases) {
    it(`日志上传先返回失效（心跳没有发现）：${c.name}、不提交，之后不再上传日志`, async () => {
      let stale = false;
      let agentDone = false;
      env = await makeEnv(async (ctx) => {
        await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
        if (c.wip) await writeFile(path.join(ctx.worktree, 'wip.txt'), '一半\n');
        await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: c.status, summary: '结束' }));
        await waitFor(() => stale, 5_000, '日志失效');
        ctx.done();
        agentDone = true;
      });
      const e = env;
      const append = e.kiln.appendLog.bind(e.kiln);
      let staleCalls = 0;
      // Agent 启动之后的第一次日志上传返回失效；心跳一直正常
      e.kiln.appendLog = (ref, offset, data) => {
        if (stale || Buffer.from(data).toString('utf8').includes('已在 Orca 中启动')) {
          stale = true;
          staleCalls++;
          return Promise.reject(staleError());
        }
        return append(ref, offset, data);
      };
      // 停止 Agent 期间心跳循环跑过多轮（每轮心跳后 flush 日志）：失效后它不再调用 Server（心跳、日志都不发）
      const stop = e.orca.workerStop.bind(e.orca);
      let beatsAfterStale = -1;
      e.orca.workerStop = async (d) => {
        const n = e.kiln.heartbeats.length;
        await new Promise((r) => setTimeout(r, fastTiming.heartbeatMs * 10));
        beatsAfterStale = e.kiln.heartbeats.length - n;
        return stop(d);
      };
      const sha = await e.origin.branchSha('master');
      const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
      await run(e, at);
      // 假 Agent 在被停止后照常跑完（测试结束前等它，避免它在临时目录删除后还在写）
      await waitFor(() => agentDone, 5_000, 'Agent 结束');
      expect(stale).toBe(true);
      expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
      expect(e.kiln.completeCalls).toBe(0);
      expect(staleCalls).toBe(1);
      expect(beatsAfterStale).toBe(0);
      expect(await records(e.journal)).toEqual([]);
    });
  }
});

describe('失效后不再启动写入（F3-R）', () => {
  it('Git.push 读 HEAD 期间得知失效：不再执行 git push，远端没有 attempt 分支、不提交', async () => {
    let inPush = false;
    let headRead = '';
    const pushes: string[][] = [];
    env = await makeEnv(async (ctx) => {
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      ctx.done();
    });
    const e = env;
    const gate = heartbeatGate(e, staleError);
    // 交付推送读完 HEAD 之后、真正 git push 之前，心跳返回失效并被处理；读到的 HEAD 原样交回
    const exec: Exec = async (cmd, args, opts) => {
      if (args[0] === 'push') pushes.push([...args]);
      const r = await nodeExec(cmd, args, opts);
      if (inPush && args[0] === 'rev-parse' && !gate.passed) {
        await gate.fire();
        headRead = r.stdout.trim();
      }
      return r;
    };
    e.deps.git = new Git(exec);
    const push = e.deps.git.push.bind(e.deps.git);
    e.deps.git.push = (...args) => {
      inPush = true;
      return push(...args);
    };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    // 同步点之后的代码执行到了（读到了要推送的 commit）
    expect(headRead).toMatch(/^[0-9a-f]{40}$/);
    expect(e.kiln.heartbeats).toHaveLength(gate.answered);
    expect(pushes).toEqual([]);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
    expect(e.kiln.completeCalls).toBe(0);
  });

  it('Complete 网络错误后退避期间得知失效：退避立即结束，不再发送 Complete', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const e = env;
    let staleAt = 0;
    e.kiln.completeFault = new ConnectError('暂时连不上', Code.Unavailable);
    e.kiln.heartbeatFn = () => {
      if (e.kiln.completeCalls === 0) return {};
      staleAt ||= Date.now();
      return staleError();
    };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    const elapsed = Date.now() - staleAt;
    expect(staleAt).toBeGreaterThan(0);
    expect(e.kiln.completeCalls).toBe(1);
    expect(e.kiln.completes).toEqual([]);
    // Complete 的第一次退避是 1 秒：失效后不等它结束
    expect(elapsed).toBeLessThan(800);
    expect(await records(e.journal)).toEqual([]);
  });
});

describe('启动前得知停止', () => {
  /**
   * stopWhileSavingStart 在 start() 把启动幂等键写入 journal（journal.save(stage=start) 已经写盘）之后、返回之前让 attempt
   * 停止：stale、cancel 由下一次心跳返回，等它经过 rpc / halt 生效后才放行；shutdown 直接让连接器退出。
   */
  function stopWhileSavingStart(e: Env, how: 'stale' | 'cancel' | 'shutdown', at: () => Attempt) {
    let fired = false;
    let passed = false;
    const gate = heartbeatGate(e, () => (how === 'stale' ? staleError() : { cancelRequested: true }));
    const save = e.journal.save.bind(e.journal);
    e.journal.save = async (r) => {
      await save(r);
      if (fired || r.stage !== 'start' || !r.binding.startRequestId) return;
      fired = true;
      if (how === 'shutdown') at().shutdown();
      else await gate.fire();
      passed = true;
    };
    /** fired：在写入幂等键时停止了；passed：同步点之后的代码执行到了（save 正常返回）。 */
    return { fired: () => fired, passed: () => passed };
  }

  it('写入启动幂等键期间失效：不启动 Agent、不提交，没有 infra.lost，journal 删除', async () => {
    let ran = false;
    env = await makeEnv(() => {
      ran = true;
      return Promise.resolve();
    });
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    const h = stopWhileSavingStart(env, 'stale', () => at);
    await run(env, at);
    expect(h.fired()).toBe(true);
    expect(h.passed()).toBe(true);
    expect(env.orca.starts).toHaveLength(0);
    expect(ran).toBe(false);
    expect(env.kiln.completeCalls).toBe(0);
    expect(env.orca.stops).toEqual([]);
    expect(await records(env.journal)).toEqual([]);
  });

  it('写入启动幂等键期间取消：结果为 CANCELLED（不是 infra.lost），不启动 Agent，提交的绑定中没有启动幂等键', async () => {
    env = await makeEnv(() => Promise.resolve());
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    const h = stopWhileSavingStart(env, 'cancel', () => at);
    await run(env, at);
    expect(h.fired()).toBe(true);
    expect(h.passed()).toBe(true);
    expect(env.orca.starts).toHaveLength(0);
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.CANCELLED);
    expect(c.errorClass).toBe('');
    expect(c.binding?.startRequestId).toBe('');
    expect(await records(env.journal)).toEqual([]);
  });

  it('写入启动幂等键期间连接器退出：journal 保留且没有启动幂等键，重启后正常启动一次并完成', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    const h = stopWhileSavingStart(env, 'shutdown', () => at);
    await run(env, at);
    expect(h.fired()).toBe(true);
    expect(h.passed()).toBe(true);
    expect(env.orca.starts).toHaveLength(0);
    expect(env.kiln.completeCalls).toBe(0);
    const [rec] = await records(env.journal);
    expect(rec?.stage).toBe('start');
    expect(rec?.binding.startRequestId).toBe('');

    await run(env, Attempt.fromJournal(env.deps, rec!), true);
    expect(env.orca.starts).toHaveLength(1);
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.SUCCEEDED);
    expect(c.binding?.startRequestId).toBe(env.orca.starts[0]?.requestId);
    expect(await records(env.journal)).toEqual([]);
  });

  it('启动时、写入启动幂等键之前取消：不写入幂等键（没有"可能已经发出"的窗口），不启动 Agent，结果为 CANCELLED', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const gate = heartbeatGate(e, () => ({ cancelRequested: true }));
    const keys: string[] = [];
    const save = e.journal.save.bind(e.journal);
    e.journal.save = (r) => {
      if (r.binding.startRequestId) keys.push(r.binding.startRequestId);
      return save(r);
    };
    // 让 start() 自己在 Orca 中注册仓库（准备时已经注册过一次），注册期间取消；注册的结果原样交回
    e.deps.orcaRepoIds.get = () => undefined;
    const repoAdd = e.orca.repoAdd.bind(e.orca);
    let returned = false;
    e.orca.repoAdd = async (p) => {
      const id = await repoAdd(p);
      if (e.deps.orcaRepoIds.size > 0) {
        await gate.fire();
        returned = true;
      }
      return id;
    };
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(returned).toBe(true);
    expect(keys).toEqual([]);
    expect(e.orca.starts).toHaveLength(0);
    expect(completed(e).outcome).toBe(Outcome.CANCELLED);
  });

  it('准备仓库期间取消：不拉起测试服务、不启动 Agent，结果为 CANCELLED', async () => {
    const services = new FakeServices();
    env = await makeEnv(() => Promise.resolve(), { services });
    const e = env;
    const gate = heartbeatGate(e, () => ({ cancelRequested: true }));
    // 仓库准备好之后、返回之前取消（之后本该拉起测试服务、启动 Agent）；准备的结果原样交回
    const prepareClone = e.deps.git.prepareClone.bind(e.deps.git);
    let returned = false;
    e.deps.git.prepareClone = async (...args) => {
      const r = await prepareClone(...args);
      await gate.fire();
      returned = true;
      return r;
    };
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha, services: ['pg'] })));
    expect(returned).toBe(true);
    expect(services.started).toEqual([]);
    expect(e.orca.starts).toHaveLength(0);
    expect(completed(e).outcome).toBe(Outcome.CANCELLED);
  });
});

describe('共用 clone 的互斥', () => {
  /** RecordingMutex 记下每次请求锁（run 被调用）的 key。 */
  class RecordingMutex extends KeyedMutex {
    readonly calls: string[] = [];
    override run<T>(key: string, f: () => Promise<T>): Promise<T> {
      this.calls.push(key);
      return super.run(key, f);
    }
  }

  it('任务 B 的准备仓库不与任务 A 的 worker-start（在共用的 clone 中建 worktree）重叠', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const e = env;
    const lock = new RecordingMutex();
    e.deps.repoLock = lock;
    const events: string[] = [];
    const prepareClone = e.deps.git.prepareClone.bind(e.deps.git);
    e.deps.git.prepareClone = async (...a) => {
      events.push('prepare:enter');
      try {
        return await prepareClone(...a);
      } finally {
        events.push('prepare:leave');
      }
    };
    const sha = await e.origin.branchSha('master');
    const [a, b] = [1, 2].map(() => new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    let runB: Promise<void> | undefined;
    const workerStart = e.orca.workerStart.bind(e.orca);
    e.orca.workerStart = async (o) => {
      events.push(`start:enter:${o.name}`);
      try {
        if (o.name === a!.binding.worktreeName) {
          // A 在 Orca 中启动（建 worktree）期间 B 开始准备：等 B 请求仓库的锁，再给它一轮宏任务（没有互斥时它这时已经在 fetch）
          const n = lock.calls.length;
          runB = b!.run();
          await waitFor(() => lock.calls.length > n, 5_000, 'B 请求仓库的锁');
          await macrotask();
        }
        return await workerStart(o);
      } finally {
        events.push(`start:leave:${o.name}`);
      }
    };
    const ac = new AbortController();
    const pump = e.deps.mailbox.pump(ac.signal);
    try {
      await a!.run();
      await waitFor(() => runB !== undefined, 5_000, 'B 开始');
      await runB;
    } finally {
      ac.abort();
      await pump;
    }
    const [na, nb] = [a!.binding.worktreeName, b!.binding.worktreeName];
    expect(events).toEqual(['prepare:enter', 'prepare:leave', `start:enter:${na}`, `start:leave:${na}`, 'prepare:enter', 'prepare:leave', `start:enter:${nb}`, `start:leave:${nb}`]);
    expect(e.kiln.completes.map((c) => c.outcome)).toEqual([Outcome.SUCCEEDED, Outcome.SUCCEEDED]);
  });

  it('任务 A 的启动响应丢失、正在查证时不占着仓库的锁：任务 B 照常准备', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    const [a, b] = [1, 2].map(() => new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    // A 的 worker-start 实际启动了 Agent 但响应丢失；request-show 在 B 开始准备仓库之前一直是 pending。查证窗口放长到 5 秒：
    // 查证在锁外时 B 几十毫秒内就开始准备，在锁内时 B 要等到 A 查证超时（A 记为 infra.lost）
    e.deps.timing = { ...e.deps.timing, startVerifyMs: 5_000 };
    e.orca.startFault = 'lost-after';
    let bPreparing = false;
    e.orca.requestShowFn = () => (bPreparing ? undefined : { state: 'pending' });
    const prepareClone = e.deps.git.prepareClone.bind(e.deps.git);
    let prepares = 0;
    e.deps.git.prepareClone = (...args) => {
      if (++prepares === 2) bPreparing = true;
      return prepareClone(...args);
    };
    let runB: Promise<void> | undefined;
    const requestShow = e.orca.requestShow.bind(e.orca);
    e.orca.requestShow = (id) => {
      // A 开始查证时 B 开始执行
      runB ??= b!.run();
      return requestShow(id);
    };
    const ac = new AbortController();
    const pump = e.deps.mailbox.pump(ac.signal);
    try {
      await a!.run();
      await runB;
    } finally {
      ac.abort();
      await pump;
    }
    expect(bPreparing).toBe(true);
    // A 查证到了自己的 Agent（没有因为 B 等锁而查证超时）
    expect(e.orca.starts).toHaveLength(2);
    expect(e.kiln.completes.map((c) => [String(c.ref?.attemptId), c.outcome])).toEqual(
      expect.arrayContaining([
        [String(a!.id), Outcome.SUCCEEDED],
        [String(b!.id), Outcome.SUCCEEDED],
      ]),
    );
  });

  it('等仓库锁期间取消：不写入启动幂等键，不启动 Agent，结果为 CANCELLED', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const lock = new RecordingMutex();
    e.deps.repoLock = lock;
    const gate = heartbeatGate(e, () => ({ cancelRequested: true }));
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    // 准备完之后（最后一次写 prepare 阶段），别的任务占住这个仓库的锁
    let release!: () => void;
    const held = new Promise<void>((r) => (release = r));
    let holder: Promise<void> | undefined;
    const keys: string[] = [];
    const save = e.journal.save.bind(e.journal);
    e.journal.save = async (r) => {
      if (r.binding.startRequestId) keys.push(r.binding.startRequestId);
      await save(r);
      if (!holder && r.stage === 'prepare' && r.binding.worktreeName) holder = lock.run(String(at.spec.repoId), () => held);
    };
    const running = run(e, at);
    // 准备、占锁、启动各请求一次：启动正在等锁
    await waitFor(() => lock.calls.length >= 3, 5_000, '启动等仓库的锁');
    await gate.fire();
    release();
    await running;
    await holder;
    expect(gate.passed).toBe(true);
    expect(keys).toEqual([]);
    expect(e.orca.starts).toHaveLength(0);
    expect(completed(e).outcome).toBe(Outcome.CANCELLED);
  });
});

describe('任务目录的临时目录', () => {
  it('Agent 启动时任务目录中已有 tmp/（0700）；恢复时被删掉的 tmp/ 重新建好', async () => {
    let resume = false;
    let mode = -1;
    env = await makeEnv(async (ctx) => {
      mode = (await stat(path.join(ctx.taskDir, taskTmpDir))).mode & 0o777;
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      await waitFor(() => resume, 5_000, '恢复');
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    const running = run(env, at);
    await waitFor(() => at.binding.dispatchId !== '' && mode !== -1, 5_000, '启动');
    expect(taskTmpDir).toBe('tmp');
    expect(mode).toBe(0o700);
    at.shutdown();
    await running;

    const tmp = path.join(at.taskDir, taskTmpDir);
    await rm(tmp, { recursive: true });
    const [rec] = await records(env.journal);
    resume = true;
    await run(env, Attempt.fromJournal(env.deps, rec!), true);
    expect(completed(env).outcome).toBe(Outcome.SUCCEEDED);
    expect((await stat(tmp)).mode & 0o777).toBe(0o700);
  });
});

describe('工具调用、结束流程与 journal 的并发', () => {
  /**
   * claimRace 复现审计的场景：Agent 先写出改动，再领取；心跳在领取进行中收到取消。where 是取消到达的位置：save 是领取
   * 成功、正在把 claim 写入 journal 时（审计的复现，journal 用真实的 Journal.save），rpc 是领取的 RPC 还没有返回时。
   * 都在那个位置等到取消生效后再放行。
   */
  async function claimRace(e: Env, where: 'save' | 'rpc'): Promise<{ at: Attempt; claimed: () => { code: number; output: string } | undefined; passed: () => boolean }> {
    let fired = false;
    let claimed: { code: number; output: string } | undefined;
    e.orca.agent = async (ctx) => {
      await writeFile(path.join(ctx.worktree, 'wip.txt'), '一半\n');
      await writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected: [12] }));
      claimed = await ctx.tool('claim');
      await new Promise<void>((r) => (ctx.stopped.aborted ? r() : ctx.stopped.addEventListener('abort', () => r())));
    };
    const gate = heartbeatGate(e, () => ({ cancelRequested: true }));
    const hold = async () => {
      fired = true;
      await gate.fire();
    };
    const save = e.journal.save.bind(e.journal);
    e.journal.save = async (r) => {
      if (where === 'save' && !fired && r.claim && r.stage === 'running') await hold();
      return save(r);
    };
    const claimDevIssues = FakeKiln.prototype.claimDevIssues.bind(e.kiln);
    e.kiln.claimDevIssues = async (req) => {
      if (where === 'rpc' && !fired) await hold();
      return claimDevIssues(req);
    };
    e.kiln.offers.push({ candidates: [{ number: 12, title: '加功能' }] });
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'select' }));
    return { at, claimed: () => claimed, passed: () => gate.passed };
  }

  it.each(['save', 'rpc'] as const)('领取进行中（%s）取消：没有 ENOENT，按"已领取"推 checkpoint，停止 Agent，Complete 一次，journal 清理（多轮）', async (where) => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const save = e.journal.save.bind(e.journal);
    for (let round = 0; round < 5; round++) {
      e.journal.save = save;
      const { at, claimed, passed } = await claimRace(e, where);
      const completesBefore = e.kiln.completes.length;
      const stopsBefore = e.orca.stops.length;
      await run(e, at);
      // 同步点之后的代码执行到了：领取照常写入 journal / 返回
      expect(passed()).toBe(true);
      expect(e.orca.stops.slice(stopsBefore)).toEqual([at.binding.dispatchId]);
      expect(e.kiln.completes).toHaveLength(completesBefore + 1);
      const c = e.kiln.completes.at(-1)!;
      expect(c.outcome).toBe(Outcome.CANCELLED);
      // 正在进行的领取没有被截断：Agent 收到领取成功，结论按已领取处理（推了 checkpoint）
      expect(claimed()?.code).toBe(0);
      const d = c.result?.value as { branch: string; sha: string; checkpoint: boolean };
      expect(c.result?.case).toBe('delivery');
      expect(d.checkpoint).toBe(true);
      expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe(d.sha);
      expect(await records(e.journal)).toEqual([]);
      expect(at.unconfirmedStop).toBe(false);
    }
  });

  it('结束流程开始后新的工具调用被拒绝；正在进行的领取照常完成，结论按已领取处理', async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let inflight: Promise<{ code: number; output: string }> | undefined;
    let queued: Promise<{ code: number; output: string }> | undefined;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected: [12] }));
      inflight = ctx.tool('claim');
      // 排在领取之后的调用：轮到它时结束流程已经开始，不再执行
      queued = ctx.tool('info');
      // 领取还在进行时 Agent 就结束了这一轮
      ctx.done();
      await inflight;
    });
    const e = env;
    e.kiln.offers = [{ candidates: [{ number: 12, title: '加功能' }] }];
    const claimDevIssues = e.kiln.claimDevIssues.bind(e.kiln);
    let claimStarted = false;
    e.kiln.claimDevIssues = async (req) => {
      claimStarted = true;
      await gate;
      return claimDevIssues(req);
    };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'select' }));
    const running = run(e, at);
    await waitFor(() => claimStarted, 5_000, '领取发出');
    // 等主流程看到 worker_done、开始结束（finishing）：之后的工具调用被拒绝
    await waitFor(() => e.kiln.heartbeats.some((h) => h.stage === 'finishing'), 5_000, '开始结束');
    const late = await at.tool('info', {});
    expect(late.code).toBe(1);
    expect(late.output).toContain('已经停止');
    // 主流程在等正在进行的领取：还没有提交。这时到达的取消不再改变结果（Agent 已经结束），也不再停止 Agent
    await heartbeatGate(e, () => ({ cancelRequested: true })).fire();
    expect(e.kiln.completeCalls).toBe(0);
    release();
    await running;
    expect((await inflight!).code).toBe(0);
    expect((await queued!).output).toContain('已经停止');
    const c = completed(e);
    // 领取算数：开发任务要求交付报告（没有领取时会以"没有领取 Issue"成功结束）
    expect(c.outcome).toBe(Outcome.FAILED);
    expect(c.errorClass).toBe(ErrorClass.delivery);
    expect(e.orca.stops).toEqual([]);
    expect(await records(e.journal)).toEqual([]);
  });

  it('领取的重试等待被取消打断：结束流程不等完重试，结论按没有领取处理', async () => {
    let started = false;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected: [12] }));
      started = true;
      expect((await ctx.tool('claim')).code).toBe(1);
      await new Promise<void>((r) => (ctx.stopped.aborted ? r() : ctx.stopped.addEventListener('abort', () => r())));
    });
    const e = env;
    e.kiln.offers = [{ candidates: [{ number: 12, title: '加功能' }] }];
    let calls = 0;
    e.kiln.claimDevIssues = () => {
      calls++;
      return Promise.reject(new ConnectError('连接断开', Code.Unavailable));
    };
    e.kiln.heartbeatFn = () => ({ cancelRequested: calls > 0 });
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'select' }));
    const t0 = Date.now();
    await run(e, at);
    expect(started).toBe(true);
    // 5 次重试的退避合计 15 秒：被取消打断时远小于它
    expect(Date.now() - t0).toBeLessThan(5_000);
    expect(calls).toBeLessThan(5);
    const c = completed(e);
    expect(c.outcome).toBe(Outcome.CANCELLED);
    expect(c.result?.case).toBeUndefined();
  });

  it('结束流程中 journal 写入失败：Agent 被停止，不提交，journal 删除，run 报错', async () => {
    const agent = wipAgent();
    env = await makeEnv(agent.script);
    const e = env;
    e.kiln.heartbeatFn = () => ({ cancelRequested: agent.working });
    const save = e.journal.save.bind(e.journal);
    let failed = false;
    e.journal.save = (r) => {
      if (!failed && r.verdict) {
        failed = true;
        return Promise.reject(new Error('ENOSPC: no space left on device'));
      }
      return save(r);
    };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await expect(run(e, at)).rejects.toThrow(/ENOSPC/);
    expect(failed).toBe(true);
    expect(e.orca.stops).toEqual([at.binding.dispatchId]);
    expect(e.orca.workers.get(at.binding.dispatchId)?.state).toBe('stopped');
    expect(at.unconfirmedStop).toBe(false);
    expect(e.kiln.completeCalls).toBe(0);
    expect(await records(e.journal)).toEqual([]);
  });

  it('结束流程中 journal 写入失败且不能确认停止：记为 unconfirmedStop，journal 记为 stopping', async () => {
    const agent = wipAgent();
    env = await makeEnv(agent.script);
    const e = env;
    e.orca.stopConfirms = false;
    e.kiln.heartbeatFn = () => ({ cancelRequested: agent.working });
    const save = e.journal.save.bind(e.journal);
    let failed = false;
    e.journal.save = (r) => {
      if (!failed && r.verdict) {
        failed = true;
        return Promise.reject(new Error('EIO'));
      }
      return save(r);
    };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await expect(run(e, at)).rejects.toThrow(/EIO/);
    expect(e.orca.stops).toEqual([at.binding.dispatchId]);
    expect(at.unconfirmedStop).toBe(true);
    expect(e.kiln.completeCalls).toBe(0);
    const [rec] = await records(e.journal);
    expect(rec?.stage).toBe('stopping');
    expect(rec?.binding.dispatchId).toBe(at.binding.dispatchId);
    e.orca.stopConfirms = true;
    await e.orca.workerStop(at.binding.dispatchId);
  });
});

describe('task doc（外部设计文档快照）', () => {
  it('通过连接器读取工作项冻结的文档：list / get / 不存在', async () => {
    env = await makeEnv(async () => {});
    env.kiln.taskDocs = [{ id: 'd1', title: '需求文档', revision: 2, sha256: 'sha', url: 'https://o/d1', content: '# 正文' }];
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, devTask(env.origin, { sha, mode: 'implement' }));

    const list = await at.tool('doc', {});
    expect(list.code).toBe(0);
    expect(list.output).toContain('d1');
    expect(list.output).toContain('需求文档');
    // 请求带的是当前 attempt 的 ref
    expect(env.kiln.taskDocRefs.at(-1)?.attemptId).toBe(at.ref.attemptId);

    const get = await at.tool('doc', { id: 'd1' });
    expect(get.code).toBe(0);
    expect(get.output).toContain('# 正文');
    expect(env.kiln.taskDocGets.at(-1)).toBe('d1');

    const missing = await at.tool('doc', { id: 'nope' });
    expect(missing.code).toBe(1);
  });
});

describe('Agent 运行中断（Orca 1.4.263）', () => {
  const abortPath = (e: Env, d: string) => path.join(e.root, 'home', 'inbox', `${d}.abort.json`);
  /** fatal 是一条致命的 escalation：Orca 报告 Agent 运行中断（一轮无法继续）。 */
  const fatal = (dispatchId: string, o: Partial<OrcaMessage> = {}): Partial<OrcaMessage> & { type: string } => ({
    type: 'escalation',
    dispatchId,
    subject: 'Agent hit an error: Safety check blocked this request.',
    body: '原始输出：Safety check blocked this request.',
    turnOutcome: 'failure',
    failureKind: 'safetyCheck',
    error: 'Safety check blocked this request. Please rephrase your prompt.',
    detail: 'Safety check: blocked by policy.',
    ...o,
  });
  /** waiting 是收到停止信号才退出的 Agent：运行中断时它可能还在运行（Orca 只是报告这一轮无法继续）。 */
  const waiting = (send: (ctx: AgentCtx) => void) => async (ctx: AgentCtx) => {
    send(ctx);
    await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
  };

  it('审计：按 ai.runtime 失败、保留原始原因，Agent 不进入退避，停止并释放终端，工作区回收', async () => {
    let dispatch = '';
    env = await makeEnv(waiting((ctx) => { dispatch = ctx.dispatchId; env!.orca.send(fatal(ctx.dispatchId)); }));
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    await run(env, at);

    const c = completed(env);
    expect(c.outcome).toBe(Outcome.FAILED);
    expect(c.errorClass).toBe(ErrorClass.runtime);
    expect(c.message).toContain('Safety check blocked this request. Please rephrase your prompt.');
    // 不是普通求助：不自动回复
    expect(env.orca.replies).toEqual([]);
    // Agent 的这一轮无法继续：停止、释放终端，结果了结后回收工作区
    expect(env.orca.stops).toEqual([dispatch]);
    expect(env.orca.releases).toEqual([dispatch]);
    expect(env.orca.worktreeRemoves).toHaveLength(1);
    expect(await records(env.journal)).toEqual([]);
    // 一次任务的安全检查不代表这台机器上的 Agent 不能用：不退避
    expect(env.deps.agents.usable('claude-code')).toBe(true);
    // 落盘的中止记录随 attempt 的清理删除
    expect(existsSync(abortPath(env, dispatch))).toBe(false);
  });

  it.each(['needs-input', 'exited', 'lost'])('系统停止通知 %s：同样按 ai.runtime 结束（不是成功，也不是报告不合格）', async (stopReason) => {
    let dispatch = '';
    env = await makeEnv(waiting((ctx) => { dispatch = ctx.dispatchId; env!.orca.send(fatal(ctx.dispatchId, { turnOutcome: '', failureKind: '', error: '', detail: '', stopReason })); }));
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.FAILED);
    expect(c.errorClass).toBe(ErrorClass.runtime);
    expect(c.message).toContain('原始输出');
    expect(env.orca.replies).toEqual([]);
    expect(env.orca.stops).toEqual([dispatch]);
  });

  it('stopReason 为 finished（这一轮结束）：不是中断，不写中止记录，零回复，之后照常以 worker_done 结束', async () => {
    env = await makeEnv(async (ctx) => {
      const handled = env!.orca.handled.length;
      env!.orca.send(fatal(ctx.dispatchId, { turnOutcome: '', failureKind: '', error: '', detail: '', stopReason: 'finished' }));
      // 先等邮箱把这条通知单独处理完（这一批处理完之后才发出下一次 check），再发 worker_done：否则同批的 worker_done
      // 优先规则会掩盖"把 finished 误判成运行中断"的缺陷
      await waitFor(() => env!.orca.handled.length > handled, 5_000, '邮箱取走 finished 通知');
      const requests = env!.orca.checks.length;
      await waitFor(() => env!.orca.checks.length > requests, 5_000, '处理完这一批');
      expect(env!.orca.replies).toEqual([]);
      expect(existsSync(abortPath(env!, ctx.dispatchId))).toBe(false);
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    await run(env, at);
    expect(completed(env).outcome).toBe(Outcome.SUCCEEDED);
    expect(env.orca.replies).toEqual([]);
    expect(existsSync(abortPath(env, at.binding.dispatchId))).toBe(false);
  });

  it('审计报告已经写好但 Agent 运行中断：不校验报告，按 ai.runtime 失败（不把半截结果当成审计完成）', async () => {
    env = await makeEnv(waiting(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      env!.orca.send(fatal(ctx.dispatchId));
    }));
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.FAILED);
    expect(c.errorClass).toBe(ErrorClass.runtime);
    // 合格的报告没有提交：运行中断时它可能只是半截
    expect(c.result?.case).toBeUndefined();
  });

  it('开发已领取、已经提交并交付：不推送正式交付，改动推为 checkpoint，结果为 ai.runtime', async () => {
    env = await makeEnv(waiting(async (ctx) => {
      env!.kiln.offers = [{ candidates: [{ number: 12, title: '加功能' }] }];
      await writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected: [12] }));
      expect((await ctx.tool('claim')).code).toBe(0);
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      expect((await ctx.tool('deliver')).code).toBe(0);
      env!.orca.send(fatal(ctx.dispatchId));
    }));
    env.kiln.offers = [{ candidates: [{ number: 12, title: '加功能' }] }];
    env.kiln.claimFn = () => ({ claimed: true, workItemId: 9n, branch: 'kiln/dev-9', phase: 'implement', message: '开始开发' });
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, devTask(env.origin, { sha, mode: 'select' }));
    await run(env, at);

    const c = completed(env);
    expect(c.outcome).toBe(Outcome.FAILED);
    expect(c.errorClass).toBe(ErrorClass.runtime);
    const d = c.result?.value as { branch: string; sha: string; checkpoint: boolean; report: Uint8Array };
    // 交付不合格：没有推送正式交付（branch 是 attempt 分支的 checkpoint，不是交付分支）
    expect(d.checkpoint).toBe(true);
    expect(d.branch).toBe(`kiln/attempt-${at.id}`);
    expect(await env.origin.branchSha(d.branch)).toBe(d.sha);
    expect(d.sha).not.toBe(await env.origin.branchSha('kiln/dev-9'));
  });

  it('选题还没有领取时运行中断：按 ai.runtime 失败，不是"没有领取 Issue"的成功', async () => {
    env = await makeEnv(waiting((ctx) => env!.orca.send(fatal(ctx.dispatchId))));
    env.kiln.offers = [{ candidates: [{ number: 12, title: '加功能' }] }];
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, devTask(env.origin, { sha, mode: 'select' })));
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.FAILED);
    expect(c.errorClass).toBe(ErrorClass.runtime);
    expect(c.message).not.toContain('没有领取');
    expect(env.kiln.claims).toEqual([]);
    // 没有领取的工作没有 checkpoint
    expect(c.result?.case).toBeUndefined();
  });

  it('不能确认 Agent 停止时：不推 checkpoint，journal 记为 stopping（节点继续停止），结果照常提交', async () => {
    let dispatch = '';
    env = await makeEnv(async (ctx) => {
      dispatch = ctx.dispatchId;
      // 已经领取、已经提交：本应有 checkpoint；只是停止不可确认
      env!.kiln.offers = [{ candidates: [{ number: 12, title: '加功能' }] }];
      await writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected: [12] }));
      expect((await ctx.tool('claim')).code).toBe(0);
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      env!.orca.send(fatal(ctx.dispatchId));
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    env.orca.stopConfirms = false;
    env.kiln.offers = [{ candidates: [{ number: 12, title: '加功能' }] }];
    env.kiln.claimFn = () => ({ claimed: true, workItemId: 9n, branch: 'kiln/dev-9', phase: 'implement', message: '开始开发' });
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, devTask(env.origin, { sha, mode: 'select' }));
    await run(env, at);

    expect(at.unconfirmedStop).toBe(true);
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.FAILED);
    expect(c.errorClass).toBe(ErrorClass.runtime);
    // 领取过的工作本应有 checkpoint，但不能确认停止时不推：没有交付
    expect(c.result?.case).toBeUndefined();
    expect(env.orca.stops).toContain(dispatch);
    // 没有确认停止：不释放终端
    expect(env.orca.releases).toEqual([]);
    // 远端没有 attempt 分支：checkpoint 没有推送
    expect(await env.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
    const [rec] = await records(env.journal);
    expect(rec?.stage).toBe('stopping');
    expect(rec?.binding.dispatchId).toBe(at.binding.dispatchId);
    // 测试结束时让假 Agent 退出
    env.orca.stopConfirms = true;
    await env.orca.workerStop(at.binding.dispatchId);
  });

  it('崩溃后重启：journal 为 running 时读到中止记录，按 ai.runtime 结束，不再启动 Agent', async () => {
    env = await makeEnv(waiting(async () => {}));
    const sha = await env.origin.branchSha('master');
    const at = new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha }));
    const running = run(env, at);
    await waitFor(() => env!.orca.starts.length === 1 && at.binding.dispatchId !== '', 5_000, '启动');
    // 先等 journal 保存成 running：否则 shutdown 可能落在 preparing，恢复时看到的是另一个阶段
    await waitFor(async () => (await records(env!.journal)).some((r) => r.attemptId === String(at.id) && r.stage === 'running'), 5_000, 'journal 记为 running');
    at.shutdown();
    await running;
    expect(env.kiln.completes).toEqual([]);
    const [rec] = await records(env.journal);
    // 停在 running（连接器退出时不提交、不改写阶段）
    expect(rec?.stage).toBe('running');
    expect(rec?.binding.dispatchId).toBe(at.binding.dispatchId);

    // 进程在这个阶段退出：崩溃前落盘的中止记录由重启的 attempt 读到
    await mkdir(path.join(env.root, 'home', 'inbox'), { recursive: true });
    await writeFile(abortPath(env, at.binding.dispatchId), JSON.stringify({ id: 'msg_x', dispatchId: at.binding.dispatchId, turnOutcome: 'failure', stopReason: '', failureKind: 'safetyCheck', reason: '崩溃前落盘的原因', logDetail: '崩溃前落盘的原因' }));
    const again = Attempt.fromJournal(env.deps, rec!);
    await run(env, again, true);
    expect(env.orca.starts).toHaveLength(1);
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.FAILED);
    expect(c.errorClass).toBe(ErrorClass.runtime);
    expect(c.message).toContain('崩溃前落盘的原因');
    // 恢复用的是落盘的记录：不再重启 Agent（结果只提交一次）
    expect(env.kiln.completes).toHaveLength(1);
  });

  it('worker-show 兜底决定之前再看一次邮箱：兜底瞬间落盘的中止记录按 ai.runtime 结束', async () => {
    env = await makeEnv(async () => {
      // 不发送 worker_done，直接退出：等 worker-show 兜底
    });
    let settledShows = 0;
    const show = env.orca.workerShow.bind(env.orca);
    env.orca.workerShow = async (d) => {
      const w = await show(d);
      if (w.terminal === 'exited' && ++settledShows === 3) {
        await mkdir(path.join(env!.root, 'home', 'inbox'), { recursive: true });
        await writeFile(path.join(env!.root, 'home', 'inbox', `${d}.abort.json`), JSON.stringify({ id: 'msg_late', dispatchId: d, turnOutcome: 'failure', stopReason: '', failureKind: 'safetyCheck', reason: '兜底瞬间到达的原因', logDetail: '兜底瞬间到达的原因' }));
      }
      return w;
    };
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.FAILED);
    expect(c.errorClass).toBe(ErrorClass.runtime);
    expect(c.message).toContain('兜底瞬间到达的原因');
  });

  it('中止记录损坏时明确失败，不会一直等到超时', async () => {
    env = await makeEnv(async (ctx) => {
      await mkdir(path.join(env!.root, 'home', 'inbox'), { recursive: true });
      await writeFile(abortPath(env!, ctx.dispatchId), '{"reason":');
    });
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    const c = completed(env);
    expect(c.outcome).toBe(Outcome.INFRA_ERROR);
    expect(c.errorClass).toBe(ErrorClass.prepare);
    // 不是成功、不是 ai.runtime 的推断，也不是等到超时的 TIMED_OUT
    expect(c.outcome).not.toBe(Outcome.TIMED_OUT);
  });
});
