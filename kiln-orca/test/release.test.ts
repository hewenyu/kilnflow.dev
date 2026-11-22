import { writeFile } from 'node:fs/promises';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { Outcome } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { Attempt } from '../src/core/attempt.js';
import { ErrorClass, RevokedError } from '../src/core/errors.js';
import { emptyBinding, type JournalRecord } from '../src/core/journal.js';
import { Node, type NodeOptions, retentionMs } from '../src/core/node.js';
import { checkStart } from '../src/core/dispatch.js';
import { OrcaError, type ReleaseState, type RequestInfo, StartFailedError, type WorkerRef } from '../src/core/orca.js';
import { type AgentCtx, devTask, type Env, makeEnv, recordLog, records, reviewTask, staleError, waitFor } from './harness.js';

// 终端的释放（PLAN #33.4）：Agent 结束、确认停止、启动失败之后都要 worker-release 回收它的终端。
// 释放与业务结果分开：结果未知时业务流程照常，journal 留下 releasing 的记录（不占容量），节点在恢复之后和周期清理中重试，
// 确认后删除；Orca 明确拒绝时记 error、不再重试；超过保留时间放弃。

// Agent 脚本、钩子中的失败由 test/setup.ts 在测试结束时检查（harness 的 Faults）
let env: Env | undefined;
afterEach(async () => {
  await env?.cleanup();
  env = undefined;
});

const report = JSON.stringify({ version: 1, verdict: 'approve', summary: 'ok', findings: [] });

/** reviewer 写出合格的审计报告后发送 worker_done。 */
const reviewer = async (ctx: AgentCtx) => {
  await writeFile(path.join(ctx.taskDir, 'report.json'), report);
  ctx.done();
};

/** wip 写出改动后一直运行，直到被停止；working 在改动写出后为 true。 */
function wip() {
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

/** Orca 的 worker-release 结果未知（release_unknown）。 */
const releaseUnknown = () => new OrcaError('release_unknown', '无法确认终端已经关闭');

/** orcaFlaky 让 worker-release 在 down 为 true 时结果未知（其它时候按 FakeOrca 的语义回答）。 */
function orcaFlaky(e: Env): { down: boolean } {
  const s = { down: true };
  e.orca.releaseFn = () => (s.down ? releaseUnknown() : undefined);
  return s;
}


function makeNode(e: Env, capacity: number, extra: Partial<NodeOptions> = {}): Node {
  const node: Node = new Node(e.deps, {
    version: '0.1.0',
    capacity,
    hostId: 'host-1',
    orcaStatus: () => e.orca.status(),
    dockerStatus: () => Promise.resolve({ ok: false, reason: '没有 Docker' }),
    drained: () => false,
    restopIntervalMs: 50,
    idleMs: 20,
    ...extra,
  });
  e.orca.tool = (id, cmd, args) => node.attempt(id)?.tool(cmd, args) ?? Promise.resolve({ code: 1, output: '不在执行' });
  return node;
}

/** run 直接执行一个 attempt（邮箱在执行期间运转）。 */
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

/** releasingRecord 是 journal 中只剩释放终端的记录（不需要任务）。 */
function releasingRecord(attemptId: string, dispatchId: string, since: number): JournalRecord {
  return { attemptId, epoch: '1', spec: '', stage: 'releasing', binding: { ...emptyBinding(), dispatchId }, startedAt: since, release: { dispatchId, since } };
}

const worker = (e: Env, d: string) => e.orca.workers.get(d)!;

describe('正常结束后释放终端失败（审计的复现）', () => {
  it('worker_done 之后 release_unknown：结果照常提交一次，journal 留下 releasing，不占容量、照常领取下一个；Orca 恢复后周期任务释放并删除记录', async () => {
    env = await makeEnv(reviewer);
    const e = env;
    const orca = orcaFlaky(e);
    const sha = await e.origin.branchSha('master');
    const first = reviewTask(e.origin, { sha, baseSha: sha });
    const second = reviewTask(e.origin, { sha, baseSha: sha });
    e.kiln.tasks = [first, second];
    const node = makeNode(e, 1);
    const ac = new AbortController();
    const running = node.run(ac.signal);
    // 容量 1：第一个任务的终端没有释放，第二个任务照样领取、执行、提交
    await waitFor(() => e.kiln.completes.length === 2, 10_000, '两个任务都提交');
    const dispatches = [...e.orca.workers.keys()];
    expect(dispatches).toHaveLength(2);
    expect(e.kiln.completes.map((c) => c.outcome)).toEqual([Outcome.SUCCEEDED, Outcome.SUCCEEDED]);
    expect(e.kiln.completes.map((c) => String(c.ref?.attemptId))).toEqual([String(first.ref!.attemptId), String(second.ref!.attemptId)]);
    // 等的就是要断言的状态：两条记录都已经是 releasing（只等数量时，慢机器上第二条可能还在 finishing）
    await waitFor(async () => (await records(e.journal)).filter((r) => r.stage === 'releasing').length === 2, 5_000, '两条待释放的记录');
    const recs = await records(e.journal);
    expect(recs.map((r) => [r.stage, r.release?.dispatchId])).toEqual([
      ['releasing', dispatches[0]],
      ['releasing', dispatches[1]],
    ]);
    expect(recs.every((r) => (r.release?.since ?? 0) > 0)).toBe(true);
    // 待释放不占容量：两个任务都结束后上报的 running 为 0，不是 standby
    await waitFor(() => node.busy === 0, 5_000, '任务结束');
    const n = e.kiln.acquires.length;
    await waitFor(() => e.kiln.acquires.length >= n + 3, 5_000, '继续领取');
    for (const a of e.kiln.acquires.slice(n + 1)) expect(a).toMatchObject({ standby: false, state: { running: 0 } });
    // 周期任务在结果未知期间一直重试（不放弃）
    const before = e.orca.releases.length;
    await waitFor(() => e.orca.releases.length >= before + 4, 5_000, '周期重试');
    expect((await records(e.journal)).length).toBe(2);
    expect(dispatches.every((d) => !worker(e, d).released)).toBe(true);

    orca.down = false;
    await waitFor(async () => (await records(e.journal)).length === 0, 5_000, '释放后删除记录');
    expect(dispatches.every((d) => worker(e, d).released)).toBe(true);
    ac.abort();
    await running;
    // 结果只提交过一次
    expect(e.kiln.completeCalls).toBe(2);
    expect(e.orca.stops).toEqual([]);
  });

  it('连接器重启后恢复待释放的记录：不占容量，重试成功后删除', async () => {
    env = await makeEnv(reviewer);
    const e = env;
    const orca = orcaFlaky(e);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(e.kiln.completes.map((c) => c.outcome)).toEqual([Outcome.SUCCEEDED]);
    expect(at.unconfirmedStop).toBe(false);
    const [rec] = await records(e.journal);
    expect(rec).toMatchObject({ stage: 'releasing', release: { dispatchId: at.binding.dispatchId } });

    // 重启：Orca 仍不可用时照常领取（不占容量），记录保留
    e.kiln.tasks = [];
    const ac = new AbortController();
    const node = makeNode(e, 1);
    const running = node.run(ac.signal);
    await waitFor(() => e.kiln.acquires.length >= 2, 5_000, '上报');
    expect(e.kiln.acquires[0]).toMatchObject({ standby: false, state: { running: 0 } });
    expect(node.busy).toBe(0);
    expect((await records(e.journal)).length).toBe(1);
    const before = e.orca.releases.length;
    orca.down = false;
    await waitFor(async () => (await records(e.journal)).length === 0, 5_000, '重启后释放');
    expect(e.orca.releases.slice(before)).toEqual([at.binding.dispatchId]);
    expect(worker(e, at.binding.dispatchId).released).toBe(true);
    ac.abort();
    await running;
    // 重启后没有再提交
    expect(e.kiln.completeCalls).toBe(1);
  });
});

describe('释放的回答怎么处理', () => {
  /** seed 写一条待释放的记录，启动节点，等它处理完（记录删除）。 */
  async function settleOne(e: Env, dispatchId: string, since = Date.now()): Promise<void> {
    await e.journal.save(releasingRecord('900', dispatchId, since));
    const ac = new AbortController();
    const running = makeNode(e, 1).run(ac.signal);
    await waitFor(async () => (await records(e.journal)).length === 0, 5_000, '记录删除');
    ac.abort();
    await running;
  }

  it.each<[string, ReleaseState]>([
    ['已经释放过', 'already_released'],
    ['Orca 不替连接器关闭（retained）', 'retained'],
  ])('%s：直接完成、删除记录', async (_name, state) => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    e.orca.releaseFn = () => state;
    await settleOne(e, 'ctx_old');
    expect(e.orca.releases).toEqual(['ctx_old']);
  });

  it('dispatch 不存在：直接完成、删除记录', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const log = recordLog(e);
    await settleOne(e, 'ctx_gone');
    expect(e.orca.releases).toEqual(['ctx_gone']);
    expect(log.find((l) => l.msg === '已释放 Orca 中 Agent 的终端')?.fields).toMatchObject({ dispatch: 'ctx_gone', state: 'not_found' });
  });

  it('Orca 明确拒绝（dispatch 还没有结束）：只尝试一次、不再重试，记 error 并写明处理方法', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const log = recordLog(e);
    e.orca.releaseFn = () => new OrcaError('dispatch_inactive', 'only a settled worker can release');
    await settleOne(e, 'ctx_busy');
    expect(e.orca.releases).toEqual(['ctx_busy']);
    const err = log.find((l) => l.level === 'error');
    expect(err?.msg).toMatch(/Orca 拒绝释放/);
    expect(err?.fields).toMatchObject({ dispatch: 'ctx_busy', err: expect.stringContaining('dispatch_inactive'), fix: expect.stringContaining('worker-stop 再 worker-release') });
  });

  it('Orca 不可达（runtime_unavailable）按结果未知重试，不当成拒绝', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    let down = true;
    e.orca.releaseFn = () => (down ? new OrcaError('runtime_unavailable', 'Orca 没有运行') : 'released');
    await e.journal.save(releasingRecord('901', 'ctx_x', Date.now()));
    const ac = new AbortController();
    const running = makeNode(e, 1).run(ac.signal);
    await waitFor(() => e.orca.releases.length >= 3, 5_000, '重试');
    expect((await records(e.journal)).length).toBe(1);
    down = false;
    await waitFor(async () => (await records(e.journal)).length === 0, 5_000, '记录删除');
    ac.abort();
    await running;
  });

  it('超过保留时间（12 小时）仍不能确认：放弃，删除记录并记 warn；没到时一直保留', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const log = recordLog(e);
    e.orca.releaseFn = releaseUnknown;
    await e.journal.save(releasingRecord('902', 'ctx_old', Date.now() - retentionMs - 1_000));
    await e.journal.save(releasingRecord('903', 'ctx_new', Date.now() - retentionMs + 60_000));
    const ac = new AbortController();
    const running = makeNode(e, 1).run(ac.signal);
    await waitFor(async () => (await records(e.journal)).length === 1, 5_000, '超时的记录删除');
    await waitFor(() => e.orca.releases.filter((d) => d === 'ctx_new').length >= 3, 5_000, '没超时的继续重试');
    ac.abort();
    await running;
    expect((await records(e.journal)).map((r) => r.attemptId)).toEqual(['903']);
    expect(e.orca.releases.filter((d) => d === 'ctx_old')).toEqual(['ctx_old']);
    expect(log.find((l) => /超过保留时间/.test(l.msg))?.fields).toMatchObject({ dispatch: 'ctx_old' });
  });
});

describe('其它结束路径的终端', () => {
  it('取消：确认停止后释放终端（停止不释放）；释放结果未知时 journal 记为 releasing，不记 stopping', async () => {
    const agent = wip();
    env = await makeEnv(agent.script);
    const e = env;
    e.kiln.heartbeatFn = () => ({ cancelRequested: agent.working });
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    const d = at.binding.dispatchId;
    expect(e.orca.stops).toEqual([d]);
    expect(e.orca.releases).toEqual([d]);
    expect(worker(e, d)).toMatchObject({ state: 'stopped', released: true });
    expect(e.kiln.completes.map((c) => c.outcome)).toEqual([Outcome.CANCELLED]);
    expect(await records(e.journal)).toEqual([]);

    // 同样的取消，释放结果未知
    const agent2 = wip();
    e.orca.agent = agent2.script;
    e.orca.releaseFn = releaseUnknown;
    e.kiln.heartbeatFn = () => ({ cancelRequested: agent2.working });
    const at2 = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at2);
    expect(at2.unconfirmedStop).toBe(false);
    expect(e.kiln.completes.map((c) => c.outcome)).toEqual([Outcome.CANCELLED, Outcome.CANCELLED]);
    const [rec] = await records(e.journal);
    expect(rec).toMatchObject({ attemptId: String(at2.id), stage: 'releasing', release: { dispatchId: at2.binding.dispatchId } });
  });

  it('失效：停止 Agent 后同样释放终端，不提交', async () => {
    const agent = wip();
    env = await makeEnv(agent.script);
    const e = env;
    e.kiln.heartbeatFn = () => (agent.working ? staleError() : {});
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    const d = at.binding.dispatchId;
    expect(e.orca.stops).toEqual([d]);
    expect(e.orca.releases).toEqual([d]);
    expect(worker(e, d).released).toBe(true);
    expect(e.kiln.completeCalls).toBe(0);
    expect(await records(e.journal)).toEqual([]);
  });

  it('Orca 报告启动失败（建了 dispatch）：回收它可能留下的终端；结果未知时记为 releasing', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    e.orca.startFault = 'failed';
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    const leftover = [...e.orca.failedStarts.values()][0];
    expect(leftover).toBeTruthy();
    expect(e.kiln.completes.map((c) => c.errorClass)).toEqual([ErrorClass.agent]);
    expect(e.orca.releases).toEqual([leftover]);
    expect(at.unconfirmedStop).toBe(false);
    expect(await records(e.journal)).toEqual([]);

    e.orca.startFault = 'failed';
    e.orca.releaseFn = releaseUnknown;
    const at2 = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at2);
    const leftover2 = [...e.orca.failedStarts.values()][1];
    const [rec] = await records(e.journal);
    expect(rec).toMatchObject({ attemptId: String(at2.id), stage: 'releasing', release: { dispatchId: leftover2 } });
  });

  it('Agent 结束后记 finishing 时写 journal 失败：终端已经先释放了（结果按出错提交）', async () => {
    env = await makeEnv(reviewer);
    const e = env;
    const save = e.journal.save.bind(e.journal);
    let failed = false;
    e.journal.save = (r) => {
      // waitAndFinish 记 finishing（还没有结论）时失败
      if (!failed && r.stage === 'finishing' && !r.verdict) {
        failed = true;
        return Promise.reject(new Error('ENOSPC: no space left on device'));
      }
      return save(r);
    };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(failed).toBe(true);
    const d = at.binding.dispatchId;
    expect(e.orca.stops).toEqual([]);
    expect(e.orca.releases).toEqual([d]);
    expect(worker(e, d).released).toBe(true);
    expect(e.kiln.completes.map((c) => c.message)).toEqual([expect.stringContaining('ENOSPC')]);
    expect(await records(e.journal)).toEqual([]);
  });

  it('意料之外的错误（记结论时写 journal 失败）且释放结果未知：放弃这次执行，待释放的记录只在内存中，节点照样重试释放', async () => {
    env = await makeEnv(reviewer);
    const e = env;
    const orca = orcaFlaky(e);
    const save = e.journal.save.bind(e.journal);
    let broken = false;
    e.journal.save = (r) => {
      // 记下结论起 journal 写不进去（磁盘满）
      if (r.verdict) broken = true;
      return broken ? Promise.reject(new Error('ENOSPC: no space left on device')) : save(r);
    };
    const sha = await e.origin.branchSha('master');
    e.kiln.tasks = [reviewTask(e.origin, { sha, baseSha: sha })];
    const ac = new AbortController();
    const node = makeNode(e, 1);
    const running = node.run(ac.signal);
    await waitFor(() => broken && node.busy === 0 && e.orca.releases.length >= 3, 5_000, '放弃执行后重试释放');
    const d = [...e.orca.workers.keys()][0]!;
    expect(new Set(e.orca.releases)).toEqual(new Set([d]));
    expect(e.orca.stops).toEqual([]);
    expect(e.kiln.completeCalls).toBe(0);
    orca.down = false;
    await waitFor(() => worker(e, d).released, 5_000, '释放');
    const after = e.orca.releases.length;
    await new Promise((r) => setTimeout(r, 200));
    // 确认释放后不再重试
    expect(e.orca.releases.length).toBe(after);
    ac.abort();
    await running;
  });

  it('节点再停确认停止后释放终端；释放结果未知时转为 releasing，不再占用容量', async () => {
    const agent = wip();
    env = await makeEnv(agent.script);
    const e = env;
    e.orca.stopConfirms = false;
    e.kiln.heartbeatFn = () => ({ cancelRequested: agent.working });
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    const d = at.binding.dispatchId;
    expect(at.unconfirmedStop).toBe(true);
    expect(e.orca.releases).toEqual([]);
    expect((await records(e.journal))[0]?.stage).toBe('stopping');

    // 重启：stopping 占容量；确认停止后释放，释放结果未知 → releasing、不占容量
    const orca = orcaFlaky(e);
    const ac = new AbortController();
    const node = makeNode(e, 1);
    const running = node.run(ac.signal);
    await waitFor(() => e.kiln.acquires.length >= 1, 5_000, '上报');
    expect(e.kiln.acquires[0]).toMatchObject({ standby: true, state: { running: 1 } });
    e.orca.stopConfirms = true;
    await waitFor(async () => (await records(e.journal))[0]?.stage === 'releasing', 5_000, '转为 releasing');
    expect((await records(e.journal))[0]?.release?.dispatchId).toBe(d);
    await waitFor(() => e.kiln.acquires.some((a) => !a.standby), 5_000, '恢复领取');
    expect(node.busy).toBe(0);
    expect(worker(e, d)).toMatchObject({ state: 'stopped', released: false });
    orca.down = false;
    await waitFor(async () => (await records(e.journal)).length === 0, 5_000, '释放后删除');
    expect(worker(e, d).released).toBe(true);
    ac.abort();
    await running;
  });

  it('节点再停确认停止、释放成功：直接删除 journal', async () => {
    const agent = wip();
    env = await makeEnv(agent.script);
    const e = env;
    e.orca.stopConfirms = false;
    e.kiln.heartbeatFn = () => ({ cancelRequested: agent.working });
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    e.orca.stopConfirms = true;
    const ac = new AbortController();
    const running = makeNode(e, 1).run(ac.signal);
    await waitFor(async () => (await records(e.journal)).length === 0, 5_000, '删除');
    ac.abort();
    await running;
    expect(e.orca.releases).toEqual([at.binding.dispatchId]);
    expect(worker(e, at.binding.dispatchId).released).toBe(true);
  });
});

// 拿到一次启动请求的查证结果之后做什么只在 dispatch.ts 定义（StartCheck、settleStart）；三个调用点各自只决定接下来继续等、
// 停止还是结束。下面对每个调用点 × 每种结果断言最后的状态。

/** StartAnswer 是 request-show 的一种回答（failed 带 dispatch 时再分 worker-release 确认、结果未知）。 */
type StartAnswer = 'started' | 'failed+dispatch' | 'failed+dispatch，释放未知' | 'failed' | 'pending' | 'absent' | '查询失败';
const startAnswers: StartAnswer[] = ['started', 'failed+dispatch', 'failed+dispatch，释放未知', 'failed', 'pending', 'absent', '查询失败'];
/** leftover 是启动失败的回执中 Orca 已经建了的 dispatch。 */
const leftover = 'ctx_leftover';
/** startedRef 是 request-show 查到的已启动的 Agent（停止、释放都只记下调用）。 */
const startedRef: WorkerRef = { runId: 'run_s', taskId: 'task_s', dispatchId: 'ctx_started', worktreePath: '' };

/** answerStart 让 request-show 按 a 回答；started 为 undefined 时按 FakeOrca 实际启动过的请求回答。 */
function answerStart(e: Env, a: StartAnswer, started: WorkerRef | undefined): void {
  e.orca.requestShowFn = (): RequestInfo | Error | undefined => {
    switch (a) {
      case 'started':
        return started && { state: 'completed', worker: started };
      case 'failed+dispatch':
      case 'failed+dispatch，释放未知':
        return { state: 'failed', error: 'worktree_create', dispatchId: leftover };
      case 'failed':
        return { state: 'failed', error: 'agent_unavailable' };
      case 'pending':
        return { state: 'pending' };
      case 'absent':
        return { state: 'absent' };
      case '查询失败':
        return new OrcaError('exec_failed', '连不上 Orca');
    }
  };
  e.orca.releaseFn = (d) => (d === leftover ? (a === 'failed+dispatch，释放未知' ? releaseUnknown() : 'retained') : undefined);
}

/** StartOutcome 是一种回答之后的最后状态：journal 中留下的记录（没有时为 undefined）、停止和释放过的 dispatch。 */
interface StartOutcome {
  stage: 'stopping' | 'releasing' | undefined;
  stops: string[];
  releases: string[];
}
/** startOutcome：查到的 Agent 是 started 时停止它、释放它（停止不释放）。 */
function startOutcome(a: StartAnswer, started: string): StartOutcome {
  switch (a) {
    case 'started':
      return { stage: undefined, stops: [started], releases: [started] };
    case 'failed+dispatch':
      return { stage: undefined, stops: [], releases: [leftover] };
    case 'failed+dispatch，释放未知':
      return { stage: 'releasing', stops: [], releases: [leftover] };
    case 'failed':
      return { stage: undefined, stops: [], releases: [] };
    case 'pending':
    case 'absent':
    case '查询失败':
      return { stage: 'stopping', stops: [], releases: [] };
  }
}

/** expectRecord 断言 journal 中留下的记录：releasing 记着回执中的 dispatch，stopping 只有启动幂等键。 */
async function expectRecord(e: Env, stage: StartOutcome['stage']): Promise<void> {
  const recs = await records(e.journal);
  if (stage === undefined) {
    expect(recs).toEqual([]);
    return;
  }
  expect(recs).toHaveLength(1);
  const rec = recs[0]!;
  expect(rec.stage).toBe(stage);
  if (stage === 'releasing') {
    expect(rec).toMatchObject({ startFailed: true, release: { dispatchId: leftover }, binding: { dispatchId: '' } });
  } else {
    expect(rec.binding).toMatchObject({ dispatchId: '', startRequestId: expect.any(String) as string });
    expect(rec.binding.startRequestId).not.toBe('');
  }
}

describe('查证启动请求的结果：每个调用点 × 每种结果', () => {
  it.each(startAnswers)('Attempt.verifyStart（worker-start 的响应丢失）：%s', async (a) => {
    env = await makeEnv(reviewer);
    const e = env;
    // started：Orca 真的启动了（响应丢失），request-show 按实际的回执回答；其它：Orca 没有启动任何东西
    e.orca.startFault = a === 'started' ? 'lost-after' : 'lost-before';
    answerStart(e, a, undefined);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(e.orca.starts).toHaveLength(1);
    const [c] = e.kiln.completes;
    const want = startOutcome(a, at.binding.dispatchId);
    if (a === 'started') {
      // Agent 正常结束：不停止，只释放
      expect(c).toMatchObject({ outcome: Outcome.SUCCEEDED });
      expect(e.orca.stops).toEqual([]);
      expect(e.orca.releases).toEqual(want.releases);
      expect(await records(e.journal)).toEqual([]);
      return;
    }
    expect(c?.errorClass).toBe(want.stage === 'stopping' ? ErrorClass.lost : ErrorClass.agent);
    expect(at.unconfirmedStop).toBe(want.stage === 'stopping');
    expect(e.orca.stops).toEqual(want.stops);
    expect(e.orca.releases).toEqual(want.releases);
    await expectRecord(e, want.stage);
  });

  it.each(startAnswers)('Attempt.stopAgent（吊销后恢复，journal 只有启动幂等键）：%s', async (a) => {
    env = await makeEnv(reviewer);
    const e = env;
    answerStart(e, a, startedRef);
    const sha = await e.origin.branchSha('master');
    const fresh = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    const rec: JournalRecord = { ...fresh.record, stage: 'start', binding: { ...emptyBinding(), startRequestId: 'lost-start-request' } };
    await e.journal.save(rec);
    const at = Attempt.fromJournal(e.deps, rec);
    at.revoke();
    await run(e, at, true);
    const want = startOutcome(a, startedRef.dispatchId);
    expect(e.orca.requestShows).toEqual(['lost-start-request']);
    expect(e.orca.starts).toEqual([]);
    expect(e.kiln.completeCalls).toBe(0);
    expect(at.unconfirmedStop).toBe(want.stage === 'stopping');
    expect(e.orca.stops).toEqual(want.stops);
    expect(e.orca.releases).toEqual(want.releases);
    await expectRecord(e, want.stage);
  });

  it.each(startAnswers)('Node.restop（stopping 只有启动幂等键）：%s', async (a) => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    answerStart(e, a, startedRef);
    const now = Date.now();
    await e.journal.save({ attemptId: '920', epoch: '1', spec: '', stage: 'stopping', binding: { ...emptyBinding(), startRequestId: 'req-920' }, startedAt: now, stopSince: now });
    const ac = new AbortController();
    const node = makeNode(e, 1);
    const running = node.run(ac.signal);
    const want = startOutcome(a, startedRef.dispatchId);
    if (want.stage === 'stopping') {
      // 不能断定：一直跟踪（absent 到查证窗口为止），继续占用容量
      if (a === 'absent') {
        await waitFor(async () => (await records(e.journal)).length === 0, 5_000, 'absent 超过查证窗口后结束跟踪');
      } else {
        await waitFor(() => e.orca.requestShows.length >= 4, 5_000, '继续查证');
        expect(node.busy).toBe(1);
        await expectRecord(e, 'stopping');
      }
    } else {
      // 结束跟踪：journal 中不再是 stopping，并且已经释放容量（先写 journal 再释放容量）
      await waitFor(async () => (await records(e.journal)).every((r) => r.stage !== 'stopping') && node.busy === 0, 5_000, '结束跟踪');
      if (want.stage === 'releasing') await waitFor(() => e.orca.releases.length >= 2, 5_000, '待释放的终端由节点重试');
      expect(node.busy).toBe(0);
      await expectRecord(e, want.stage);
    }
    ac.abort();
    await running;
    expect(e.orca.requestShows.every((r) => r === 'req-920')).toBe(true);
    expect(e.orca.stops).toEqual(want.stops);
    expect(new Set(e.orca.releases)).toEqual(new Set(want.releases));
  });
});

describe('checkStart 合并 worker-start 的错误与 request-show 的回答', () => {
  const ask = async (show: RequestInfo | Error, startErr?: unknown) => {
    env = await makeEnv(() => Promise.resolve());
    env.orca.requestShowFn = () => show;
    return checkStart(env.orca, 'req-1', startErr);
  };
  it('回执带出的 dispatch 优先（request-show 的 failed 没有 dispatch 时也回收它）', async () => {
    expect(await ask({ state: 'failed', error: 'x' }, new StartFailedError('worktree_create', 'ctx_receipt'))).toMatchObject({ kind: 'failed', error: expect.stringContaining('worktree_create') as string, dispatchId: 'ctx_receipt' });
  });
  it('request-show 的 failed 带出 dispatch', async () => {
    expect(await ask({ state: 'failed', error: 'x', dispatchId: 'ctx_show' })).toEqual({ kind: 'failed', error: 'x', dispatchId: 'ctx_show' });
  });
  it('worker-start 被明确拒绝且 Orca 没有这条请求：没有启动（failed）；结果未知的错误时只是 absent', async () => {
    expect(await ask({ state: 'absent' }, new OrcaError('agent_unavailable', 'claude 不可用'))).toMatchObject({ kind: 'failed', error: expect.stringContaining('claude 不可用') as string, dispatchId: '' });
    expect(await ask({ state: 'absent' }, new OrcaError('exec_failed', '连接断开'))).toEqual({ kind: 'absent' });
    expect(await ask({ state: 'absent' })).toEqual({ kind: 'absent' });
  });
  it('completed 没有回执、查询失败：unknown', async () => {
    expect((await ask({ state: 'completed' })).kind).toBe('unknown');
    expect(await ask(new OrcaError('runtime_unavailable', 'Orca 没有运行'))).toMatchObject({ kind: 'unknown', error: expect.stringContaining('Orca 没有运行') as string });
  });
});

describe('吊销后的清理与待释放的终端', () => {
  it('吊销清理等待释放完成才结束；以吊销状态启动时同样先释放（不联系 Server）', async () => {
    env = await makeEnv(reviewer);
    const e = env;
    const orca = orcaFlaky(e);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect((await records(e.journal))[0]?.stage).toBe('releasing');

    // 以吊销状态启动：不联系 Server；释放没有确认时不结束
    let contacted = 0;
    e.kiln.acquire = () => {
      contacted++;
      return Promise.reject(new Error('不应联系 Server'));
    };
    let settled = false;
    const result = makeNode(e, 1, { revoked: true })
      .run(new AbortController().signal)
      .then(
        () => undefined,
        (err: unknown) => err,
      )
      .finally(() => (settled = true));
    const before = e.orca.releases.length;
    await waitFor(() => e.orca.releases.length >= before + 3, 5_000, '吊销清理中重试释放');
    expect(settled).toBe(false);
    orca.down = false;
    expect(await result).toBeInstanceOf(RevokedError);
    expect(contacted).toBe(0);
    expect(await records(e.journal)).toEqual([]);
    expect(worker(e, at.binding.dispatchId).released).toBe(true);
  });

  it('吊销清理中收到退出信号：待释放的记录留在 journal（下次启动继续）', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    e.orca.releaseFn = releaseUnknown;
    await e.journal.save(releasingRecord('910', 'ctx_r', Date.now()));
    const ac = new AbortController();
    const result = makeNode(e, 1, { revoked: true })
      .run(ac.signal)
      .catch((err: unknown) => err);
    await waitFor(() => e.orca.releases.length >= 2, 5_000, '重试释放');
    ac.abort();
    expect(await result).toBeInstanceOf(RevokedError);
    expect((await records(e.journal)).map((r) => r.stage)).toEqual(['releasing']);
  });
});
