import { existsSync } from 'node:fs';
import { mkdir, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { Code, ConnectError } from '@connectrpc/connect';
import { afterEach, describe, expect, it } from 'vitest';
import { Outcome } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { Attempt } from '../src/core/attempt.js';
import { ErrorClass, RevokedError } from '../src/core/errors.js';
import { type Exec, nodeExec } from '../src/core/exec.js';
import { Git } from '../src/core/git.js';
import { emptyBinding, type JournalRecord } from '../src/core/journal.js';
import { Node, type NodeOptions, retentionMs } from '../src/core/node.js';
import { OrcaError, type WorktreeInfo } from '../src/core/orca.js';
import { attemptComment, blocksReclaim, canon, maxResiduePerSweep, parseAttemptComment, reclaimWorkspace, residueQuietMs, sweepResidue, type ReclaimTarget, worktreeName } from '../src/core/reclaim.js';
import { type AgentCtx, commitFile, devTask, type Env, git, type LogLine, makeEnv, recordLog, records, reviewTask, staleError, waitFor } from './harness.js';
import { fx, orcaWorld, setTerminalState, worker } from './orcaworld.js';

// 工作区的回收（PLAN #33.10）：审计和开发相同，结果了结、Agent 停止、终端释放之后经 Orca 删除这次任务的
// 工作区和临时检出，删除后核对 Orca、git、磁盘；不干净时才带 --force；失败时 journal 记为 reclaiming，节点重试；只删能证明
// 属于这次任务的。下面大多用审计验证共同的规则，开发特有的（推送之后才删、没有推送的改动写进日志）见"开发结束后回收"。

let env: Env | undefined;
afterEach(async () => {
  await env?.cleanup();
  env = undefined;
});

const report = (findings: unknown[] = []) => JSON.stringify({ version: 1, verdict: findings.length > 0 ? 'request_changes' : 'approve', summary: 'ok', findings });

/** auditor 像真实的审计 Agent：在任务目录中检出被审 commit（head），留下构建产物和变异验证的改动，写报告后结束。 */
function auditor(o: { findings?: unknown[]; dirty?: boolean } = {}) {
  const seen: { ctx?: AgentCtx; head?: string } = {};
  const script = async (ctx: AgentCtx) => {
    seen.ctx = ctx;
    seen.head = path.join(ctx.taskDir, 'head');
    await git(ctx.worktree, 'worktree', 'add', '--quiet', '--detach', seen.head, 'HEAD');
    if (o.dirty ?? true) {
      await writeFile(path.join(seen.head, 'build.out'), '构建产物\n');
      await writeFile(path.join(ctx.worktree, 'README.md'), '变异验证的改动\n');
    }
    await writeFile(path.join(ctx.taskDir, 'report.json'), report(o.findings));
    ctx.done();
  };
  return { seen, script };
}


function makeNode(e: Env, extra: Partial<NodeOptions> = {}): Node {
  const node: Node = new Node(e.deps, {
    version: '0.1.0',
    capacity: 1,
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

async function run(e: Env, at: Attempt): Promise<void> {
  e.orca.tool = (_id, cmd, args) => at.tool(cmd, args);
  const ac = new AbortController();
  const pump = e.deps.mailbox.pump(ac.signal);
  try {
    await at.run();
  } finally {
    ac.abort();
    await pump;
  }
}

/** runNode 运行节点直到 until 成立。 */
async function runNode(e: Env, until: () => Promise<boolean>, what: string, node = makeNode(e)): Promise<void> {
  const ac = new AbortController();
  const running = node.run(ac.signal);
  try {
    await waitFor(until, 10_000, what);
  } finally {
    ac.abort();
    await running;
  }
}

const clonePath = (e: Env) => path.join(e.deps.reposDir, '42', 'widgets');

/** gone 核对：Orca 的列表、git worktree list、磁盘上都没有 p。 */
async function gone(e: Env, p: string): Promise<boolean> {
  const c = await canon(p);
  const repoId = e.orca.repos.get(clonePath(e)) ?? '';
  const prev = e.orca.showExternal;
  e.orca.showExternal = true;
  const rows = await e.orca.worktreeList(repoId);
  e.orca.showExternal = prev;
  const inOrca = (await Promise.all(rows.map((r) => canon(r.path)))).includes(c);
  const inGit = (await git(clonePath(e), 'worktree', 'list', '--porcelain')).includes(c);
  return !inOrca && !inGit && !existsSync(p);
}

const branches = (e: Env) => git(clonePath(e), 'for-each-ref', '--format=%(refname)', 'refs/heads/');

describe('备注', () => {
  it('写入任务类型；旧版本的备注没有类型，不认识的类型、别的格式都不认', () => {
    expect(attemptComment({ attemptId: 7n, epoch: 2n, taskId: 9n, kind: 'review' })).toBe('kiln:attempt=7;epoch=2;task=9;kind=review');
    expect(parseAttemptComment('kiln:attempt=7;epoch=2;task=9;kind=review')).toEqual({ attemptId: '7', epoch: '2', taskId: '9', kind: 'review' });
    expect(parseAttemptComment('kiln:attempt=7;epoch=2;task=9;kind=develop')?.kind).toBe('develop');
    expect(parseAttemptComment('kiln:attempt=7;epoch=2;task=9')).toEqual({ attemptId: '7', epoch: '2', taskId: '9', kind: undefined });
    expect(parseAttemptComment('kiln:attempt=7;epoch=2;task=9;kind=other')).toBeUndefined();
    expect(parseAttemptComment('kiln:attempt=07;epoch=2;task=9;kind=review')).toBeUndefined();
    expect(parseAttemptComment('用户的备注 kiln:attempt=7;epoch=2;task=9;kind=review')).toBeUndefined();
    expect(parseAttemptComment('')).toBeUndefined();
  });
});

describe('审计结束后回收', () => {
  it('成功（有 finding）：提交被确认之后才删；工作区和 head 都不干净，Orca 拒绝后带 --force；删除后核对三处；分支删除', async () => {
    const a = auditor({ findings: [{ title: 'x' }] });
    env = await makeEnv(a.script);
    const e = env;
    const log = recordLog(e);
    // 提交时工作区、检出都还在（报告校验、上传、结果确认之前不删）
    let atComplete: boolean[] = [];
    const complete = e.kiln.complete.bind(e.kiln);
    e.kiln.complete = (req) => {
      atComplete = [existsSync(a.seen.ctx!.worktree), existsSync(a.seen.head!), e.orca.worktreeRemoves.length === 0];
      return complete(req);
    };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);

    expect(e.kiln.completes.map((c) => c.outcome)).toEqual([Outcome.SUCCEEDED]);
    expect(atComplete).toEqual([true, true, true]);
    // 先不带 --force，Orca 因为不干净拒绝之后带 --force；先删检出，再删工作区
    const wt = e.orca.worktreeRemoves.map((r) => [r.id.slice(r.id.indexOf('::') + 2).endsWith('head') ? 'head' : 'wt', r.force]);
    expect(wt).toEqual([
      ['head', false],
      ['head', true],
      ['wt', false],
      ['wt', true],
    ]);
    expect(await gone(e, a.seen.ctx!.worktree)).toBe(true);
    expect(await gone(e, a.seen.head!)).toBe(true);
    expect(await branches(e)).not.toContain(`kiln-a${at.id}-e1`);
    expect(await records(e.journal)).toEqual([]);
    // 终端先释放
    expect(e.orca.releases).toEqual([at.binding.dispatchId]);
    // 日志区分终端释放、工作区删除、head 删除，写明 --force
    const msgs = log.map((l) => l.msg);
    expect(msgs).toContain('审计工作区中有未提交的改动或未跟踪的文件（一次性的产物；开发没有推送的已在上面写明），带 --force 删除');
    expect(msgs).toContain('已删除审计的 Orca 工作区（--force）');
    expect(msgs).toContain('已删除审计的临时检出（--force）');
    expect(msgs).toContain('已删除审计工作区的本地分支');
    expect(msgs).toContain('已核对：审计的工作区在 Orca、git 和磁盘上都没有了');
    // clone 本身、邮箱的工作区不动
    expect(existsSync(path.join(clonePath(e), '.git'))).toBe(true);
    expect(existsSync(path.join(e.root, 'home', 'kiln-orca'))).toBe(true);
  });

  it('干净的工作区不带 --force', async () => {
    const a = auditor({ dirty: false });
    env = await makeEnv(a.script);
    const e = env;
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(e.orca.worktreeRemoves.map((r) => r.force)).toEqual([false, false]);
    expect(await gone(e, a.seen.ctx!.worktree)).toBe(true);
  });

  it('失败（报告不合格）也回收', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    e.kiln.validateFn = () => ({ valid: false, problems: ['缺少 verdict'] });
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(e.kiln.completes.map((c) => c.outcome)).toEqual([Outcome.FAILED]);
    expect(await gone(e, a.seen.ctx!.worktree)).toBe(true);
    expect(await gone(e, a.seen.head!)).toBe(true);
  });

  it('取消：确认停止、释放终端之后回收', async () => {
    let started = false;
    let wtPath = '';
    env = await makeEnv(async (ctx) => {
      wtPath = ctx.worktree;
      await writeFile(path.join(ctx.worktree, 'wip.txt'), '一半\n');
      started = true;
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    const e = env;
    e.kiln.heartbeatFn = () => ({ cancelRequested: started });
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(e.kiln.completes.map((c) => c.outcome)).toEqual([Outcome.CANCELLED]);
    expect(e.orca.stops).toEqual([at.binding.dispatchId]);
    expect(e.orca.releases).toEqual([at.binding.dispatchId]);
    expect(await gone(e, wtPath)).toBe(true);
    expect(await records(e.journal)).toEqual([]);
  });

  it('失效（不再提交）：停止 Agent 之后回收', async () => {
    let started = false;
    let wtPath = '';
    env = await makeEnv(async (ctx) => {
      wtPath = ctx.worktree;
      started = true;
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    const e = env;
    e.kiln.heartbeatFn = () => (started ? staleError() : {});
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(e.kiln.completes).toEqual([]);
    expect(await gone(e, wtPath)).toBe(true);
  });

  it('Server 已经有结果（AlreadyExists）：按了结回收', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    e.kiln.completeFault = new ConnectError('已经有结果', Code.AlreadyExists);
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(await gone(e, a.seen.ctx!.worktree)).toBe(true);
  });

  it('结果没有确认提交：不回收（工作区留作诊断）', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    e.kiln.completeDown = new ConnectError('连不上', Code.Unavailable);
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(e.orca.worktreeRemoves).toEqual([]);
    expect(existsSync(a.seen.ctx!.worktree)).toBe(true);
    expect(existsSync(a.seen.head!)).toBe(true);
    expect(await records(e.journal)).toEqual([]);
  });
});

/** developer 像开发 Agent：在工作区中提交一个改动，按 o 留下未提交的文件，写交付报告；untilStopped 时不结束、等被停止。 */
function developer(o: { status?: string; wip?: boolean; untilStopped?: boolean } = {}) {
  const seen: { ctx?: AgentCtx; working: boolean } = { working: false };
  const script = async (ctx: AgentCtx) => {
    seen.ctx = ctx;
    await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
    if (o.wip) await writeFile(path.join(ctx.worktree, 'wip.txt'), '没有提交的改动\n');
    if (o.untilStopped) {
      seen.working = true;
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
      return;
    }
    await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: o.status ?? 'delivered', summary: '完成' }));
    ctx.done();
  };
  return { seen, script };
}

/** unpushedLog 是回收开发的工作区时关于没有推送的改动的那条日志。 */
const unpushedLog = (log: LogLine[]) => log.find((l) => l.msg.startsWith('回收开发的工作区：'));

// 开发的工作区与审计相同，结束后立即回收（PLAN #33.10）：交付和 checkpoint 在回收之前已经推送、ls-remote 确认，Complete
// 被确认；没有推送的改动不特殊处理，照样回收，日志写明
describe('开发结束后回收', () => {
  it('成功交付：推送确认、Complete 被确认之后才删；工作区、分支都回收，日志写明改动都已推送', async () => {
    const d = developer();
    env = await makeEnv(d.script);
    const e = env;
    const log = recordLog(e);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    let atFirstRemove: { completes: number; remote: string } | undefined;
    const remove = e.orca.worktreeRemove.bind(e.orca);
    e.orca.worktreeRemove = async (id, force) => {
      atFirstRemove ??= { completes: e.kiln.completes.length, remote: await e.origin.branchSha(`kiln/attempt-${at.id}`) };
      return remove(id, force);
    };
    await run(e, at);
    const c = e.kiln.completes[0];
    expect(c?.outcome).toBe(Outcome.SUCCEEDED);
    const pushed = (c?.result?.value as { sha: string }).sha;
    // 删除第一个东西的时候：结果已经提交被确认，远端的 attempt 分支已经是交付的 commit
    expect(atFirstRemove).toEqual({ completes: 1, remote: pushed });
    expect(await gone(e, d.seen.ctx!.worktree)).toBe(true);
    expect(await branches(e)).not.toContain(`kiln-a${at.id}-e1`);
    expect(unpushedLog(log)).toMatchObject({ level: 'info', msg: '回收开发的工作区：其中的改动都已经推到远端', fields: { pushed, head: pushed, commits: 0, dirtyFiles: 0 } });
    expect(await records(e.journal)).toEqual([]);
  });

  it('checkpoint（没有交付、留下未提交的改动）：推了 checkpoint 之后回收，改动都已推送', async () => {
    const d = developer({ status: 'blocked', wip: true });
    env = await makeEnv(d.script);
    const e = env;
    const log = recordLog(e);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    const v = e.kiln.completes[0]?.result?.value as { sha: string; checkpoint: boolean };
    expect(v.checkpoint).toBe(true);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe(v.sha);
    expect(await gone(e, d.seen.ctx!.worktree)).toBe(true);
    expect(unpushedLog(log)).toMatchObject({ level: 'info', fields: { pushed: v.sha, commits: 0, dirtyFiles: 0 } });
  });

  it('推送失败：结果照常提交，工作区照样回收，日志写明删掉了没有推送的改动', async () => {
    const d = developer({ wip: true });
    env = await makeEnv(d.script);
    const e = env;
    const log = recordLog(e);
    const exec: Exec = (cmd, args, opts) => (args[0] === 'push' ? Promise.resolve({ code: 1, stdout: '', stderr: 'remote rejected' }) : nodeExec(cmd, args, opts));
    e.deps.git = new Git(exec);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    expect(e.kiln.completes).toHaveLength(1);
    expect(e.kiln.completes[0]?.errorClass).toBe(ErrorClass.delivery);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
    expect(await gone(e, d.seen.ctx!.worktree)).toBe(true);
    // wip.txt 在推 checkpoint 时已经提交（推送失败）：起点之后 2 个 commit
    expect(unpushedLog(log)).toMatchObject({ level: 'warn', fields: { attempt: String(at.id), start: sha, pushed: '', commits: 2, dirtyFiles: 0 } });
    expect(unpushedLog(log)?.msg).toMatch(/^回收开发的工作区：删除了没有推送的改动（起点之后 2 个 commit/);
    expect(await records(e.journal)).toEqual([]);
  });

  it('失败（Agent 没有写交付报告）：推了 checkpoint 之后回收', async () => {
    env = await makeEnv(async (ctx) => {
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      ctx.done();
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.FAILED);
    expect((e.kiln.completes[0]?.result?.value as { checkpoint: boolean }).checkpoint).toBe(true);
    expect(await gone(e, at.binding.worktreePath)).toBe(true);
    expect(e.orca.worktreeRemoves).toHaveLength(1);
  });

  it.each([
    ['取消', 'cancel', 'info'],
    ['超时', 'timeout', 'info'],
    ['租约失效', 'stale', 'warn'],
    ['节点吊销', 'revoked', 'warn'],
  ] as const)('%s：Agent 停止、终端释放之后回收（失效、吊销不推送，日志写明没有推送的改动）', async (_name, how, level) => {
    const d = developer({ wip: true, untilStopped: true });
    env = await makeEnv(d.script);
    const e = env;
    const log = recordLog(e);
    e.kiln.heartbeatFn = () => {
      if (!d.seen.working) return {};
      if (how === 'cancel') return { cancelRequested: true };
      if (how === 'stale') return staleError();
      if (how === 'revoked') return new RevokedError();
      return {};
    };
    const sha = await e.origin.branchSha('master');
    const task = devTask(e.origin, { sha, mode: 'implement' });
    if (how === 'timeout') task.timeoutSeconds = 1;
    const at = new Attempt(e.deps, task);
    await run(e, at);
    expect(e.orca.stops).toEqual([at.binding.dispatchId]);
    expect(await gone(e, d.seen.ctx!.worktree)).toBe(true);
    expect(await branches(e)).not.toContain(`kiln-a${at.id}-e1`);
    const l = unpushedLog(log);
    expect(l?.level).toBe(level);
    if (level === 'warn') {
      // 失效、吊销：不推 checkpoint、不提交；删掉的是 feature.go 的 commit 和没有提交的 wip.txt
      expect(e.kiln.completeCalls).toBe(0);
      expect(l?.fields).toMatchObject({ pushed: '', start: sha, commits: 1, dirtyFiles: 1, dirty: expect.stringContaining('wip.txt') });
    } else {
      expect((e.kiln.completes[0]?.result?.value as { checkpoint: boolean }).checkpoint).toBe(true);
    }
    expect(await records(e.journal)).toEqual([]);
  });

  it('回收失败：journal 记为 reclaiming（带任务类型、起点和推送的 commit），节点重启后回收', async () => {
    const d = developer();
    env = await makeEnv(d.script);
    const e = env;
    let down = true;
    e.orca.worktreeFault = () => (down ? new OrcaError('runtime_unavailable', 'down') : undefined);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    const pushed = (e.kiln.completes[0]?.result?.value as { sha: string }).sha;
    const [rec] = await records(e.journal);
    expect(rec).toMatchObject({ stage: 'reclaiming', reclaim: { repo: '42', clone: clonePath(e), kind: 'develop', start: sha, pushed } });
    expect(existsSync(d.seen.ctx!.worktree)).toBe(true);
    down = false;
    const log = recordLog(e);
    await runNode(e, async () => (await records(e.journal)).length === 0, '重启后回收');
    expect(await gone(e, d.seen.ctx!.worktree)).toBe(true);
    expect(e.kiln.completes).toHaveLength(1);
    // 重启后按 journal 中记下的推送的 commit 判断：改动都已推送
    expect(unpushedLog(log)).toMatchObject({ level: 'info', fields: { start: sha, pushed, commits: 0 } });
  });

  it('finishing 恢复：提交前退出，工作区不回收；重启后重发同一个请求，确认之后回收', async () => {
    const d = developer();
    env = await makeEnv(d.script);
    const e = env;
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    e.kiln.complete = () => {
      at.shutdown();
      return Promise.reject(new ConnectError('down', Code.Unavailable));
    };
    await run(e, at);
    const [rec] = await records(e.journal);
    expect(rec).toMatchObject({ stage: 'finishing', complete: { delivery: { checkpoint: false } } });
    expect(rec?.reclaim).toBeUndefined();
    expect(existsSync(d.seen.ctx!.worktree)).toBe(true);
    expect(e.orca.worktreeRemoves).toEqual([]);
    e.kiln.complete = Object.getPrototypeOf(e.kiln).complete.bind(e.kiln);
    await run(e, Attempt.fromJournal(e.deps, rec!));
    expect(e.kiln.completes).toHaveLength(1);
    expect(e.kiln.completes[0]?.result?.value).toMatchObject({ sha: rec!.complete!.delivery!.sha });
    expect(await gone(e, d.seen.ctx!.worktree)).toBe(true);
    expect(await records(e.journal)).toEqual([]);
  });

  it('finishing 恢复：结论已记下、推送之前退出，重启后从还在的工作区推送，提交确认之后回收', async () => {
    const d = developer();
    env = await makeEnv(d.script);
    const e = env;
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    e.kiln.complete = () => {
      at.shutdown();
      return Promise.reject(new ConnectError('down', Code.Unavailable));
    };
    await run(e, at);
    const [rec] = await records(e.journal);
    // 推送之前退出时磁盘上的样子：有结论、没有要提交的请求，远端还没有 attempt 分支
    const before: JournalRecord = { ...rec!, complete: undefined };
    await git(e.origin.bare, 'update-ref', '-d', `refs/heads/kiln/attempt-${at.id}`);
    e.kiln.complete = Object.getPrototypeOf(e.kiln).complete.bind(e.kiln);
    await e.journal.save(before);
    await run(e, Attempt.fromJournal(e.deps, before));
    const v = e.kiln.completes[0]?.result?.value as { sha: string };
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe(v.sha);
    expect(await gone(e, d.seen.ctx!.worktree)).toBe(true);
    expect(await records(e.journal)).toEqual([]);
  });
});

describe('回收失败：记为 reclaiming，节点重试，重启后继续', () => {
  it('Orca 连不上：结果照常提交一次，journal 留下 reclaiming（不占容量）；节点重启后在 Orca 恢复时回收、删除记录', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const log = recordLog(e);
    let down = true;
    e.orca.worktreeFault = () => (down ? new OrcaError('runtime_unavailable', 'Orca 没有运行') : undefined);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(e.kiln.completes.map((c) => c.outcome)).toEqual([Outcome.SUCCEEDED]);
    const [rec] = await records(e.journal);
    expect(rec).toMatchObject({ stage: 'reclaiming', reclaim: { repo: '42', clone: clonePath(e) } });
    expect(existsSync(a.seen.ctx!.worktree)).toBe(true);
    expect(log.find((l) => l.msg === '回收审计的工作区没有完成，稍后由节点重试')?.fields.reason).toMatch(/runtime_unavailable/);

    // 重启：不占容量（照常领取），Orca 恢复后回收
    const node = makeNode(e);
    const ac = new AbortController();
    const running = node.run(ac.signal);
    await waitFor(() => e.kiln.acquires.length >= 2, 5_000, '上报');
    expect(e.kiln.acquires[0]).toMatchObject({ standby: false, state: { running: 0 } });
    expect((await records(e.journal)).length).toBe(1);
    down = false;
    await waitFor(async () => (await records(e.journal)).length === 0, 5_000, '回收后删除记录');
    ac.abort();
    await running;
    expect(await gone(e, a.seen.ctx!.worktree)).toBe(true);
    expect(await gone(e, a.seen.head!)).toBe(true);
    expect(e.kiln.completeCalls).toBe(1);
    expect(log.some((l) => l.msg === '已回收工作区')).toBe(true);
  });

  it('Orca 以其它原因拒绝（hook、锁定）：不带 --force 绕过，一直重试', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    e.orca.worktreeFault = (op) => (op === 'rm' ? new OrcaError('worktree_archive_hook_failed', 'archive hook failed') : undefined);
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(e.orca.worktreeRemoves.every((r) => !r.force)).toBe(true);
    expect((await records(e.journal))[0]?.stage).toBe('reclaiming');
    const before = e.orca.worktreeRemoves.length;
    await runNode(e, () => Promise.resolve(e.orca.worktreeRemoves.length >= before + 3), '重试');
    expect(e.orca.worktreeRemoves.every((r) => !r.force)).toBe(true);
    expect(existsSync(a.seen.ctx!.worktree)).toBe(true);
  });

  it('Orca 说不干净、连接器看到是干净的：不带 --force', async () => {
    const a = auditor({ dirty: false });
    env = await makeEnv(a.script);
    const e = env;
    e.orca.worktreeFault = (op, id) => (op === 'rm' ? new OrcaError('runtime_error', `Failed to delete worktree at ${id}. ?? x`) : undefined);
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(e.orca.worktreeRemoves.map((r) => r.force)).toEqual([false]);
    expect((await records(e.journal))[0]?.stage).toBe('reclaiming');
  });

  it('列表被截断：什么都不删', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    e.orca.worktreeFault = (op) => (op === 'list' ? new OrcaError('list_truncated', 'truncated') : undefined);
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(e.orca.worktreeRemoves).toEqual([]);
    expect((await records(e.journal))[0]?.stage).toBe('reclaiming');
  });

  it('工作区中还有活着的终端：等它关闭再删，之前什么都不删', async () => {
    const a = auditor();
    env = await makeEnv(async (ctx) => {
      // 有人在 Orca 中打开了这个工作区的终端
      env!.orca.extraTerminals.set(await canon(ctx.worktree), 1);
      await a.script(ctx);
    });
    const e = env;
    const log = recordLog(e);
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(e.orca.worktreeRemoves).toEqual([]);
    expect(existsSync(a.seen.head!)).toBe(true);
    expect(log.find((l) => l.msg === '回收审计的工作区没有完成，稍后由节点重试')?.fields.reason).toMatch(/活着的终端/);
    e.orca.extraTerminals.clear();
    await runNode(e, async () => (await records(e.journal)).length === 0, '终端关闭后回收');
    expect(await gone(e, a.seen.ctx!.worktree)).toBe(true);
  });

  it('只有挂在界面窗格上、进程还在的终端挡住回收：没有界面窗格（orphaned）的后台终端、进程已经结束的不挡', async () => {
    const a = auditor();
    env = await makeEnv(async (ctx) => {
      const wt = await canon(ctx.worktree);
      env!.orca.addTerminal({ handle: 'term_bg', title: 'bg', worktreePath: wt, connected: true, orphaned: true, exitCause: '' });
      env!.orca.addTerminal({ handle: 'term_dead', title: 'dead', worktreePath: wt, connected: false, orphaned: true, exitCause: 'operator_close' });
      env!.orca.addTerminal({ handle: 'term_ui', title: 'zsh', worktreePath: wt, connected: true, orphaned: false, exitCause: '' });
      await a.script(ctx);
    });
    const e = env;
    const log = recordLog(e);
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(e.orca.worktreeRemoves).toEqual([]);
    expect(log.find((l) => l.msg === '回收审计的工作区没有完成，稍后由节点重试')?.fields.reason).toMatch(/1 个活着的终端（zsh）/);
    // 界面上的终端关掉之后回收，后台终端不挡
    await e.orca.terminalClose('term_ui');
    expect(await e.orca.terminalAlive('term_bg')).toBe(true);
    await runNode(e, async () => (await records(e.journal)).length === 0, '界面上的终端关闭后回收');
    expect(await gone(e, a.seen.ctx!.worktree)).toBe(true);
  });

  it('blocksReclaim：进程还在并且有界面窗格才挡回收', () => {
    const t = (connected: boolean, orphaned: boolean) => ({ handle: 'h', title: 't', worktreePath: '/w', connected, orphaned, exitCause: '' });
    expect(blocksReclaim(t(true, false))).toBe(true);
    expect(blocksReclaim(t(true, true))).toBe(false);
    expect(blocksReclaim(t(false, false))).toBe(false);
    expect(blocksReclaim(t(false, true))).toBe(false);
  });

  it('终端释放结果未知：先 releasing（带回收），确认释放之后才回收', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    let down = true;
    e.orca.releaseFn = () => (down ? new OrcaError('release_unknown', '无法确认') : undefined);
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    const [rec] = await records(e.journal);
    expect(rec).toMatchObject({ stage: 'releasing', release: {}, reclaim: { repo: '42' } });
    expect(e.orca.worktreeRemoves).toEqual([]);
    const node = makeNode(e);
    const ac = new AbortController();
    const running = node.run(ac.signal);
    await waitFor(() => e.orca.releases.length >= 3, 5_000, '重试释放');
    expect(e.orca.worktreeRemoves).toEqual([]);
    down = false;
    await waitFor(async () => (await records(e.journal)).length === 0, 5_000, '释放后回收');
    ac.abort();
    await running;
    expect(await gone(e, a.seen.ctx!.worktree)).toBe(true);
  });

  it('没能确认 Agent 停止：stopping 期间不回收；确认停止后回收', async () => {
    let started = false;
    let wtPath = '';
    env = await makeEnv(async (ctx) => {
      wtPath = ctx.worktree;
      started = true;
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    const e = env;
    e.orca.stopConfirms = false;
    e.kiln.heartbeatFn = () => ({ cancelRequested: started });
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(at.unconfirmedStop).toBe(true);
    expect((await records(e.journal))[0]).toMatchObject({ stage: 'stopping', reclaim: { repo: '42' } });
    expect(e.orca.worktreeRemoves).toEqual([]);
    const node = makeNode(e);
    const ac = new AbortController();
    const running = node.run(ac.signal);
    await waitFor(() => e.orca.stops.length >= 3, 5_000, '再停');
    expect(e.orca.worktreeRemoves).toEqual([]);
    e.orca.stopConfirms = true;
    await waitFor(async () => (await records(e.journal)).length === 0, 5_000, '确认停止后回收');
    ac.abort();
    await running;
    expect(await gone(e, wtPath)).toBe(true);
  });

  it('停止一直没能确认、超过保留时间放弃之后：Agent 还在运行（structured，不在 terminal list 中），历史残留清理也不删它的工作区', async () => {
    let started = false;
    let wtPath = '';
    env = await makeEnv(async (ctx) => {
      wtPath = ctx.worktree;
      started = true;
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    const e = env;
    e.orca.stopConfirms = false;
    e.kiln.heartbeatFn = () => ({ cancelRequested: started });
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    const [rec] = await records(e.journal);
    expect(rec).toMatchObject({ stage: 'stopping' });
    await e.journal.save({ ...rec!, stopSince: Date.now() - retentionMs - 1_000 });
    // 工作区 1 小时以上没有活动，terminal list 中也没有它的终端
    e.orca.worktreeMeta.get(await canon(wtPath))!.lastActivityAt = Date.now() - residueQuietMs - 60_000;
    expect(await e.orca.terminalList(`${e.orca.repos.get(clonePath(e))}::${wtPath}`)).toEqual([]);
    const log = recordLog(e);
    await runNode(e, () => Promise.resolve(log.some((l) => l.msg.startsWith('历史残留的工作区暂不回收') && /终端没有释放的 worker/.test(l.msg))), '残留清理看到还在运行的 worker');
    expect(await records(e.journal)).toEqual([]);
    expect(e.orca.worktreeRemoves).toEqual([]);
    expect(existsSync(wtPath)).toBe(true);
  });

  it('超过保留时间仍不能回收：放弃，记 error 写明处理方法，删除记录，工作区留给人处理', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    e.orca.worktreeFault = (op) => (op === 'rm' ? new OrcaError('runtime_error', 'Failed to force delete worktree at x. Worktree is locked by Git.') : undefined);
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    const [rec] = await records(e.journal);
    await e.journal.save({ ...rec!, reclaim: { ...rec!.reclaim!, since: Date.now() - retentionMs - 1_000 } });
    const log = recordLog(e);
    await runNode(e, async () => (await records(e.journal)).length === 0, '放弃后删除记录');
    const err = log.find((l) => l.level === 'error');
    expect(err?.msg).toBe('超过保留时间仍没能回收工作区，放弃（需要人处理）');
    expect(err?.fields).toMatchObject({ reason: expect.stringContaining('locked'), fix: expect.stringContaining('orca worktree rm') });
    expect(existsSync(a.seen.ctx!.worktree)).toBe(true);
  });
});

describe('只删属于这次审计的', () => {
  /** setup 跑完一次审计（Orca 不可用，留下 reclaiming 的记录），返回它的记录和目标。 */
  async function leftover(e: Env): Promise<{ rec: JournalRecord; target: ReclaimTarget }> {
    let down = true;
    e.orca.worktreeFault = () => (down ? new OrcaError('runtime_unavailable', 'down') : undefined);
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    down = false;
    e.orca.worktreeFault = undefined;
    const [rec] = await records(e.journal);
    return { rec: rec!, target: { attemptId: rec!.attemptId, epoch: rec!.epoch, kind: 'review', start: '', pushed: '', worktreePath: rec!.binding.worktreePath, repo: '42', clone: clonePath(e) } };
  }

  it.each([
    ['Agent 超过保留时间仍没能确认停止', 'stopping'],
    ['终端超过保留时间仍没能确认释放', 'releasing'],
  ] as const)('%s：不回收（记 error），删除记录，工作区留给人处理', async (_name, stage) => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const { rec } = await leftover(e);
    const old = Date.now() - retentionMs - 1_000;
    if (stage === 'stopping') {
      e.orca.stopConfirms = false;
      await e.journal.save({ ...rec, stage, stopSince: old });
    } else {
      e.orca.releaseFn = () => new OrcaError('release_unknown', '无法确认');
      await e.journal.save({ ...rec, stage, release: { dispatchId: rec.binding.dispatchId, since: old } });
    }
    const log = recordLog(e);
    await runNode(e, async () => (await records(e.journal)).length === 0, '放弃后删除记录');
    expect(e.orca.worktreeRemoves).toEqual([]);
    expect(existsSync(a.seen.ctx!.worktree)).toBe(true);
    expect(log.some((l) => l.level === 'error' && /不回收工作区/.test(l.msg))).toBe(true);
  });

  it('重复回收是幂等的：第二次没有可删的，照样完成', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const { target } = await leftover(e);
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'done' });
    const n = e.orca.worktreeRemoves.length;
    expect(await reclaimWorkspace(e.deps, target)).toEqual({ kind: 'done', removed: [] });
    expect(e.orca.worktreeRemoves.length).toBe(n);
  });

  it('运行中的任务、clone 本身、别的 attempt、用户的工作区、路径或名字不一致、folder workspace、不在 repos/ 之下的都不删', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const log = recordLog(e);
    const { target } = await leftover(e);
    const clone = clonePath(e);
    const repoId = e.orca.repos.get(clone)!;
    const sha = await e.origin.branchSha('master');
    const add = async (name: string, comment: string) => {
      const p = path.join(e.root, 'other', name);
      await git(clone, 'worktree', 'add', '--quiet', '-b', `orca-user/${name}`, p, sha);
      e.orca.worktreeMeta.set(await canon(p), { repoId, comment, lastActivityAt: Date.now() });
      return p;
    };
    // 运行中的另一个审计（有活着的终端）、用户的工作区（没有备注）、名字不一致的、同一个 attempt 别的 epoch
    const running = await add(`kiln-a99-e1`, 'kiln:attempt=99;epoch=1;task=99;kind=review');
    e.orca.extraTerminals.set(await canon(running), 1);
    const user = await add('my-feature', '');
    const renamed = await add('renamed', `kiln:attempt=${target.attemptId};epoch=1;task=1;kind=review`);
    const epoch2 = await add(`kiln-a${target.attemptId}-e2`, `kiln:attempt=${target.attemptId};epoch=2;task=1;kind=review`);
    // folder workspace 带着这次的备注
    e.orca.extraWorktrees.push({ id: 'folder:1', repoId, path: path.join(e.root, 'folder'), branch: '', comment: `kiln:attempt=${target.attemptId};epoch=1;task=1;kind=review`, git: false, isMainWorktree: false, isBare: false, lastActivityAt: 0 });
    // 任务目录之外 Agent 检出的（不在 tasks/<attempt>/ 之下）
    const outside = path.join(e.deps.tasksDir, `${target.attemptId}0`, 'head');
    await mkdir(path.dirname(outside), { recursive: true });
    await git(clone, 'worktree', 'add', '--quiet', '--detach', outside, sha);

    // 没有被检出的分支：名字与这次审计不绑定的不删（前缀相同、编号更长的也不算）
    await git(clone, 'branch', 'keep-me', sha);
    await git(clone, 'branch', `orca-user/kiln-a${target.attemptId}0-e1`, sha);
    await git(clone, 'branch', `kiln-a${target.attemptId}-e10`, sha);

    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'done' });
    for (const p of [running, user, renamed, epoch2, outside]) expect(existsSync(p)).toBe(true);
    for (const b of ['keep-me', `orca-user/kiln-a${target.attemptId}0-e1`, `kiln-a${target.attemptId}-e10`]) expect(await branches(e)).toContain(`refs/heads/${b}`);
    expect(await branches(e)).not.toContain(`refs/heads/orca-user/kiln-a${target.attemptId}-e1`);
    expect(existsSync(path.join(clone, '.git'))).toBe(true);
    expect([...new Set(e.orca.worktreeRemoves.map((r) => r.id))]).toEqual([expect.stringMatching(/head$/), expect.stringMatching(new RegExp(`kiln-a${target.attemptId}-e1$`))]);
    expect(log.filter((l) => l.msg.includes('不能确认归属')).map((l) => l.fields.reason)).toEqual([expect.stringMatching(/名字不是/), '不是 Git worktree']);
    const refs = await branches(e);
    for (const b of ['kiln-a99-e1', 'my-feature', 'renamed', `kiln-a${target.attemptId}-e2`]) expect(refs).toContain(`refs/heads/orca-user/${b}`);

    // 路径与 Orca 回执中的不一致：不删
    const r2 = await reclaimWorkspace(e.deps, { ...target, epoch: '2', worktreePath: '/elsewhere/kiln-a1-e2' });
    expect(r2).toMatchObject({ kind: 'done' });
    expect(existsSync(epoch2)).toBe(true);
    // clone 不在 repos/ 之下：不回收
    expect(await reclaimWorkspace(e.deps, { ...target, clone: e.origin.work })).toMatchObject({ kind: 'failed', error: expect.stringMatching(/不在连接器的 repos/) });
  });

  it('符合条件的工作区不止一个：不删，报告失败', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const { target, rec } = await leftover(e);
    const clone = clonePath(e);
    const repoId = e.orca.repos.get(clone)!;
    const p = path.join(e.root, 'dup', `kiln-a${rec.attemptId}-e1`);
    await git(clone, 'worktree', 'add', '--quiet', '--detach', p, 'HEAD');
    e.orca.worktreeMeta.set(await canon(p), { repoId, comment: `kiln:attempt=${rec.attemptId};epoch=1;task=1;kind=review`, lastActivityAt: 0 });
    const out = await reclaimWorkspace(e.deps, { ...target, worktreePath: '' });
    expect(out).toMatchObject({ kind: 'failed', error: expect.stringMatching(/2 个工作区/) });
    expect(e.orca.worktreeRemoves).toEqual([]);
  });

  it('上一次删除去掉了 git 的登记、目录还在（权限不够）：重试时不当作完成，也不删这个目录，一直报失败', async () => {
    const a = auditor({ dirty: false });
    env = await makeEnv(a.script);
    const e = env;
    const { target } = await leftover(e);
    const remove = e.orca.worktreeRemove.bind(e.orca);
    // 像 git worktree remove 遇到权限不够：登记已经去掉（.git/worktrees/<名字>），目录留着，命令失败
    e.orca.worktreeRemove = async (id, force) => {
      if (!id.endsWith(`/${path.basename(target.worktreePath)}`)) return remove(id, force);
      e.orca.worktreeRemoves.push({ id, force });
      await rm(path.join(clonePath(e), '.git', 'worktrees', path.basename(target.worktreePath)), { recursive: true, force: true });
      throw new OrcaError('runtime_error', 'Permission denied');
    };
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'failed', error: expect.stringMatching(/Permission denied/) });
    for (let i = 0; i < 2; i++) {
      expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'failed', error: expect.stringMatching(/还在磁盘上.*不能再确认归属/) });
    }
    expect(e.orca.worktreeRemoves.filter((r) => r.id.endsWith(`/${path.basename(target.worktreePath)}`)).length).toBe(1);
    expect(existsSync(target.worktreePath)).toBe(true);
  });

  it('上一次删除去掉了 head 检出的 git 登记、目录还在：重试时不当作完成，工作区也留着，一直报失败，直到目录不在了', async () => {
    const a = auditor({ dirty: false });
    env = await makeEnv(a.script);
    const e = env;
    const { target } = await leftover(e);
    const head = a.seen.head!;
    const remove = e.orca.worktreeRemove.bind(e.orca);
    // 像 git worktree remove 遇到权限不够：检出的登记已经去掉，目录留着，命令失败
    e.orca.worktreeRemove = async (id, force) => {
      if (!id.endsWith('/head')) return remove(id, force);
      e.orca.worktreeRemoves.push({ id, force });
      const admin = await git(head, 'rev-parse', '--absolute-git-dir');
      await rm(admin, { recursive: true, force: true });
      throw new OrcaError('runtime_error', 'Permission denied');
    };
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'failed', error: expect.stringMatching(/Permission denied/) });
    for (let i = 0; i < 2; i++) {
      expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'failed', error: expect.stringMatching(/head 还在磁盘上.*不能再确认归属/) });
    }
    expect(e.orca.worktreeRemoves.filter((r) => r.id.endsWith('/head')).length).toBe(1);
    expect(existsSync(head)).toBe(true);
    expect(existsSync(target.worktreePath)).toBe(true);
    // 任务目录的清理删除了它之后，重试回收工作区并完成
    await rm(head, { recursive: true, force: true });
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'done' });
    expect(await gone(e, target.worktreePath)).toBe(true);
  });

  it('删除后核对：Orca 回答已删除但目录还在时不算完成', async () => {
    const a = auditor({ dirty: false });
    env = await makeEnv(a.script);
    const e = env;
    const { target } = await leftover(e);
    // Orca 回答 removed，但什么都没删
    e.orca.worktreeRemove = (id, force) => {
      e.orca.worktreeRemoves.push({ id, force });
      return Promise.resolve('removed');
    };
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'failed', error: expect.stringMatching(/删除后核对没有通过.*git worktree list.*磁盘/) });
  });

  it('上一次只删掉了 git 的登记和目录、Orca 还登记着：重试时仍认作这次审计的，一直报失败，直到 Orca 中也没有了', async () => {
    const a = auditor({ dirty: false });
    env = await makeEnv(a.script);
    const e = env;
    const { target } = await leftover(e);
    const repoId = e.orca.repos.get(clonePath(e)) ?? '';
    const want = await canon(target.worktreePath);
    let row: WorktreeInfo | undefined;
    for (const r of await e.orca.worktreeList(repoId)) if ((await canon(r.path)) === want) row = r;
    expect(row).toBeDefined();
    // Orca 删掉了 git 的登记和目录，但自己的登记还在
    const remove = e.orca.worktreeRemove.bind(e.orca);
    e.orca.worktreeRemove = async (id, force) => {
      const out = await remove(id, force);
      if (id === row!.id && !e.orca.extraWorktrees.includes(row!)) e.orca.extraWorktrees.push(row!);
      return out;
    };
    for (let i = 0; i < 2; i++) {
      expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'failed', error: expect.stringMatching(/删除后核对没有通过.*Orca 的列表/) });
    }
    // 第二次也经 Orca 删除了它（没有因为 git 中已经没有而跳过）
    expect(e.orca.worktreeRemoves.filter((r) => r.id === row!.id).length).toBe(2);
    e.orca.extraWorktrees.length = 0;
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'done' });
  });

  it('Orca 还登记着、git 中已经没有：目录被别的东西复用了，或者没有回执中的路径可以核对时，都不删', async () => {
    const a = auditor({ dirty: false });
    env = await makeEnv(a.script);
    const e = env;
    const { target } = await leftover(e);
    const repoId = e.orca.repos.get(clonePath(e)) ?? '';
    const want = await canon(target.worktreePath);
    let row: WorktreeInfo | undefined;
    for (const r of await e.orca.worktreeList(repoId)) if ((await canon(r.path)) === want) row = r;
    // 不经 Orca 去掉 git 的登记和目录，Orca 的登记留着
    await git(clonePath(e), 'worktree', 'remove', '--force', target.worktreePath);
    e.orca.extraWorktrees.push(row!);
    await reclaimWorkspace(e.deps, { ...target, worktreePath: '' });
    expect(e.orca.worktreeRemoves.filter((r) => r.id === row!.id)).toEqual([]);
    // 同一个路径上又有了别的目录
    await mkdir(target.worktreePath, { recursive: true });
    await writeFile(path.join(target.worktreePath, 'keep.txt'), 'x');
    await reclaimWorkspace(e.deps, target);
    expect(e.orca.worktreeRemoves.filter((r) => r.id === row!.id)).toEqual([]);
    expect(existsSync(path.join(target.worktreePath, 'keep.txt'))).toBe(true);
  });

  it('仓库正在准备时节点不等锁（这一轮跳过）', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const { target } = await leftover(e);
    let release!: () => void;
    const held = e.deps.repoLock.run('42', () => new Promise<void>((r) => (release = r)));
    expect(await reclaimWorkspace(e.deps, target, { skipIfBusy: true })).toMatchObject({ kind: 'wait' });
    expect(e.orca.worktreeRemoves).toEqual([]);
    release();
    await held;
    expect(await reclaimWorkspace(e.deps, target, { skipIfBusy: true })).toMatchObject({ kind: 'done' });
  });
});

/** finished 跑完一次审计（Orca 不可用，留下 reclaiming 的记录），返回回收的目标。 */
async function finished(e: Env): Promise<ReclaimTarget> {
  let down = true;
  e.orca.worktreeFault = () => (down ? new OrcaError('runtime_unavailable', 'down') : undefined);
  const sha = await e.origin.branchSha('master');
  await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
  down = false;
  e.orca.worktreeFault = undefined;
  const [rec] = await records(e.journal);
  return { attemptId: rec!.attemptId, epoch: rec!.epoch, kind: 'review', start: '', pushed: '', worktreePath: rec!.binding.worktreePath, repo: '42', clone: clonePath(e) };
}

/** auditBranch 是这次审计的工作区的本地分支（替身 Orca 按 git 用户名加前缀 orca-user/）。 */
const auditBranch = (t: ReclaimTarget) => `refs/heads/orca-user/${worktreeName(t.attemptId, t.epoch)}`;

describe('全部 Run 中的 worker：worker-list 不带 --run 时只列出调用方绑定的 Run', () => {
  /**
   * production 让回收经真实的 OrcaCli.unreleasedWorkers 查询 worker，Orca 按生产环境的上下文回答（test/orcaworld.ts）：
   * 不带 --run 时调用方被推断为邮箱，只列出邮箱 Run 的（scope.source=bound）；别的 Run（夹具 worker-list-all-p*.json）中有
   * 三个终端没有释放的 worker，都在别的工作区。
   */
  function production(e: Env) {
    const w = orcaWorld();
    e.orca.unreleasedWorkers = () => w.orca.unreleasedWorkers();
    return w;
  }

  it('调用方被绑定到邮箱 Run 时照样回收：逐个 Run 显式查询，别的工作区中的 worker 不挡', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const target = await finished(e);
    const w = production(e);
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'done' });
    expect(w.calls.filter((c) => c[1] === 'worker-list').every((c) => c[2] === '--run')).toBe(true);
    expect(await gone(e, a.seen.ctx!.worktree)).toBe(true);
    expect(await gone(e, a.seen.head!)).toBe(true);
    expect(await branches(e)).not.toContain(auditBranch(target));
  });

  it.each([
    ['别的 Run 在这个工作区中有状态不明（release_unknown、unverifiable）的 worker', 'run_000000000602', 'release_unknown', 'unverifiable', 'worktree'],
    ['run-list 第二页上的 Run 在 head 检出中有还在运行的 worker', 'run_000000000600', 'active', 'live', 'head'],
    ['别的 Run 在这个工作区中有已经退出、终端保留的 worker', 'run_000000000603', 'retained', 'exited', 'worktree'],
  ] as const)('%s：保留，什么都不删；释放之后回收', async (_name, run, state, verdict, where) => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const target = await finished(e);
    const w = production(e);
    const busy = worker('ctx_000000000800', state, verdict, where === 'head' ? a.seen.head! : a.seen.ctx!.worktree);
    w.add(run, busy);
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'wait', reason: expect.stringContaining(`dispatch ctx_000000000800，${state}，${verdict}`) });
    expect(e.orca.worktreeRemoves).toEqual([]);
    expect(existsSync(a.seen.ctx!.worktree)).toBe(true);
    expect(existsSync(a.seen.head!)).toBe(true);
    expect(await branches(e)).toContain(auditBranch(target));
    setTerminalState(busy, 'released');
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'done' });
  });

  it('工作区中的 worker 在查询期间换了没有释放的状态：照样看到它，不删', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const target = await finished(e);
    const w = production(e);
    // 每次 worker-list 之前它都变到这次没有查询的状态（active 与 retained 之间来回）：分状态查询时永远查不到它
    const busy = worker('ctx_000000000800', 'active', 'live', a.seen.ctx!.worktree);
    w.add('run_000000000603', busy);
    w.beforeList = (args) => {
      const asked = args.includes('--terminal-state') ? args[args.indexOf('--terminal-state') + 1] : busy.terminalState;
      setTerminalState(busy, asked === 'active' ? 'retained' : 'active');
    };
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'wait', reason: expect.stringContaining('dispatch ctx_000000000800') });
    expect(e.orca.worktreeRemoves).toEqual([]);
    expect(await branches(e)).toContain(auditBranch(target));
  });

  it('某个 Run 查询失败、回执的作用域不对：不删，报失败', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const target = await finished(e);
    const w = production(e);
    w.override = (args) => (args[1] === 'worker-list' && args[3] === 'run_000000000602' ? { stdout: '', code: 1 } : undefined);
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'failed', error: expect.stringMatching(/exec_failed/) });
    w.override = (args) => (args[1] === 'worker-list' && args[3] === 'run_000000000602' ? fx('worker-list-bound') : undefined);
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'failed', error: expect.stringMatching(/list_scoped/) });
    expect(e.orca.worktreeRemoves).toEqual([]);
    expect(existsSync(a.seen.ctx!.worktree)).toBe(true);
  });

  it('因为 list_scoped 超过保留时间放弃过的审计：之后的周期清理按历史残留回收', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const target = await finished(e);
    // 修复前的查询：不带 --run，回执是 bound
    e.orca.unreleasedWorkers = () => Promise.reject(new OrcaError('list_scoped', 'orca orchestration worker-list 只列出了部分 worker（scope.source=bound），不能确认工作区中没有运行的 Agent'));
    const [rec] = await records(e.journal);
    await e.journal.save({ ...rec!, reclaim: { ...rec!.reclaim!, since: Date.now() - retentionMs - 1_000 } });
    const log = recordLog(e);
    await runNode(e, async () => (await records(e.journal)).length === 0, '放弃后删除记录');
    expect(log.find((l) => l.level === 'error')?.fields.reason).toMatch(/list_scoped/);
    expect(existsSync(a.seen.ctx!.worktree)).toBe(true);
    // 修复后：工作区 1 小时以上没有活动，周期清理把它当作历史残留回收（工作区、head 和分支）
    production(e);
    e.orca.worktreeMeta.get(await canon(a.seen.ctx!.worktree))!.lastActivityAt = Date.now() - residueQuietMs - 60_000;
    await runNode(e, () => Promise.resolve(log.some((l) => l.msg === '回收了已经结束的审计留下的工作区（历史残留）')), '周期清理回收');
    expect(await gone(e, a.seen.ctx!.worktree)).toBe(true);
    expect(await gone(e, a.seen.head!)).toBe(true);
    expect(await branches(e)).not.toContain(auditBranch(target));
  });
});

describe('恢复：目录或登记已经被别的东西删掉', () => {
  it('任务目录的 12 小时清理删除了 head、git 和 Orca 的登记还在：照样回收（不带 --force），登记一起去掉', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const target = await finished(e);
    await rm(path.join(e.deps.tasksDir, target.attemptId), { recursive: true, force: true });
    expect(await git(clonePath(e), 'worktree', 'list', '--porcelain')).toContain(await canon(a.seen.head!));
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'done' });
    expect(e.orca.worktreeRemoves.filter((r) => r.id.endsWith('/head'))).toEqual([{ id: expect.any(String), force: false }]);
    expect(await gone(e, a.seen.head!)).toBe(true);
    expect(await gone(e, a.seen.ctx!.worktree)).toBe(true);
  });

  it('有人手工删除了工作区的目录（还检出着分支）、登记还在：先删登记再删分支，完成', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const target = await finished(e);
    await rm(a.seen.ctx!.worktree, { recursive: true, force: true });
    expect(await branches(e)).toContain(auditBranch(target));
    expect(await reclaimWorkspace(e.deps, target)).toMatchObject({ kind: 'done' });
    expect(await gone(e, a.seen.ctx!.worktree)).toBe(true);
    expect(await gone(e, a.seen.head!)).toBe(true);
    expect(await branches(e)).not.toContain(auditBranch(target));
  });

  it('人已经按 reclaimFix 手工清理（经 Orca 删除工作区和 head、删除分支）：journal 中的 reclaiming 照常收尾', async () => {
    const a = auditor();
    env = await makeEnv(a.script);
    const e = env;
    const target = await finished(e);
    const repoId = e.orca.repos.get(clonePath(e))!;
    await e.orca.worktreeRemove(`${repoId}::${a.seen.head!}`, true);
    await e.orca.worktreeRemove(`${repoId}::${a.seen.ctx!.worktree}`, true);
    await git(clonePath(e), 'branch', '-D', auditBranch(target).replace('refs/heads/', ''));
    const before = e.orca.worktreeRemoves.length;
    const log = recordLog(e);
    await runNode(e, async () => (await records(e.journal)).length === 0, '收尾');
    expect(e.orca.worktreeRemoves.length).toBe(before);
    expect(log.some((l) => l.msg === '已回收工作区')).toBe(true);
  });
});

describe('历史残留', () => {
  /** tracked 是 sweepResidue 交给节点接着重试的回收记录（track）。 */
  let tracked: JournalRecord[] = [];
  /** own 是 sweepResidue 的 journal 和 track（直接调用 sweepResidue 的测试用；节点的周期清理用节点自己的）。 */
  const own = (e: Env) => ({ journal: e.journal, track: (rec: JournalRecord) => void tracked.push(rec) });
  afterEach(() => {
    tracked = [];
  });

  /** residue 在连接器的 clone 上造一个升级前留下的工作区（和它的 head 检出），没有 journal 记录。 */
  async function residue(e: Env, attempt: number, comment: string, o: { quietMs?: number; head?: boolean; name?: string } = {}): Promise<{ wt: string; head: string }> {
    const clone = clonePath(e);
    const repoId = await e.orca.repoAdd(clone);
    const sha = await e.origin.branchSha('master');
    const name = o.name ?? `kiln-a${attempt}-e1`;
    const wt = path.join(e.root, 'orca-worktrees', name);
    await git(clone, 'worktree', 'add', '--quiet', '-b', `orca-user/${name}`, wt, sha);
    e.orca.worktreeMeta.set(await canon(wt), { repoId, comment, lastActivityAt: Date.now() - (o.quietMs ?? residueQuietMs + 60_000) });
    const head = path.join(e.deps.tasksDir, String(attempt), 'head');
    if (o.head ?? true) {
      await mkdir(path.dirname(head), { recursive: true });
      await git(wt, 'worktree', 'add', '--quiet', '--detach', head, 'HEAD');
    }
    return { wt, head };
  }

  /** withClone 准备连接器的 clone（与准备仓库时相同）。 */
  async function withClone(e: Env): Promise<void> {
    await e.deps.git.prepareClone(clonePath(e), e.origin.bare, [await e.origin.branchSha('master')]);
  }

  it('证据齐全的已结束审计：回收工作区、head 和分支', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    await withClone(e);
    const r = await residue(e, 501, 'kiln:attempt=501;epoch=1;task=7;kind=review');
    await sweepResidue(e.deps, { ...own(e), keep: new Set(), now: Date.now(), logged: new Set(), tried: new Map() });
    expect(await gone(e, r.wt)).toBe(true);
    expect(await gone(e, r.head)).toBe(true);
    expect(await branches(e)).not.toContain('kiln-a501-e1');
  });

  it('开发的历史残留（升级前保留的开发工作区）：同样回收，日志写明不能确定有没有没推送的改动', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const log = recordLog(e);
    await withClone(e);
    const r = await residue(e, 503, 'kiln:attempt=503;epoch=1;task=7;kind=develop', { head: false });
    await commitFile(r.wt, 'feature.go', 'package x\n', 'feat');
    await sweepResidue(e.deps, { ...own(e), keep: new Set(), now: Date.now(), logged: new Set(), tried: new Map() });
    expect(await gone(e, r.wt)).toBe(true);
    expect(await branches(e)).not.toContain('kiln-a503-e1');
    expect(log.find((l) => l.msg.startsWith('回收开发的工作区：不知道起点和推送的 commit'))?.fields).toMatchObject({ attempt: '503', start: '', pushed: '' });
    expect(log.some((l) => l.msg === '回收了已经结束的开发留下的工作区（历史残留）')).toBe(true);
  });

  it('分支删不掉（git 锁定）：工作区和检出都不删，下一轮还能找到它，锁解除后连同分支一起回收', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const log = recordLog(e);
    await withClone(e);
    const r = await residue(e, 502, 'kiln:attempt=502;epoch=1;task=7;kind=review');
    const lock = path.join(clonePath(e), '.git', 'refs', 'heads', 'orca-user', 'kiln-a502-e1.lock');
    await writeFile(lock, '');
    await sweepResidue(e.deps, { ...own(e), keep: new Set(), now: Date.now(), logged: new Set(), tried: new Map() });
    expect(log.some((l) => l.msg.startsWith('回收历史残留的工作区失败'))).toBe(true);
    expect(existsSync(r.wt)).toBe(true);
    expect(existsSync(r.head)).toBe(true);
    expect(await branches(e)).toContain('kiln-a502-e1');
    await rm(lock);
    await sweepResidue(e.deps, { ...own(e), keep: new Set(), now: Date.now(), logged: new Set(), tried: new Map() });
    expect(await gone(e, r.wt)).toBe(true);
    expect(await gone(e, r.head)).toBe(true);
    expect(await branches(e)).not.toContain('kiln-a502-e1');
  });

  it('各个跳过条件：旧备注、在途、最近有活动、有活着的终端、名字不一致、不在 repos/ 之下；旧备注的原因只记一次', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const log = recordLog(e);
    await withClone(e);
    const old = await residue(e, 601, 'kiln:attempt=601;epoch=1;task=7');
    const inflight = await residue(e, 603, 'kiln:attempt=603;epoch=1;task=7;kind=review');
    const recent = await residue(e, 604, 'kiln:attempt=604;epoch=1;task=7;kind=review', { quietMs: 60_000 });
    const live = await residue(e, 605, 'kiln:attempt=605;epoch=1;task=7;kind=review');
    e.orca.extraTerminals.set(await canon(live.wt), 1);
    const renamed = await residue(e, 606, 'kiln:attempt=606;epoch=1;task=7;kind=review', { name: 'kiln-a666-e1', head: false });
    // 用户自己的项目（不在 repos/ 之下）中带着同样格式备注的工作区
    const userRepoId = await e.orca.repoAdd(e.origin.work);
    const userWt = path.join(e.root, 'user-wt', 'kiln-a607-e1');
    await git(e.origin.work, 'worktree', 'add', '--quiet', '--detach', userWt, 'HEAD');
    e.orca.worktreeMeta.set(await canon(userWt), { repoId: userRepoId, comment: 'kiln:attempt=607;epoch=1;task=7;kind=review', lastActivityAt: 1 });
    const logged = new Set<string>();
    for (let i = 0; i < 2; i++) await sweepResidue(e.deps, { ...own(e), keep: new Set(['603']), now: Date.now(), logged, tried: new Map() });
    for (const p of [old.wt, old.head, inflight.wt, inflight.head, recent.wt, live.wt, live.head, renamed.wt, userWt]) expect(existsSync(p)).toBe(true);
    expect(log.filter((l) => l.msg.startsWith('跳过旧版本建的工作区')).length).toBe(1);
    expect(log.some((l) => l.msg.startsWith('历史残留的工作区暂不回收') && /活着的终端/.test(l.msg))).toBe(true);
    expect(log.some((l) => l.msg === '跳过历史残留的工作区：名字与备注不一致')).toBe(true);
    expect(e.orca.worktreeRemoves).toEqual([]);
  });

  it('每一轮最多回收 maxResiduePerSweep 个', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    await withClone(e);
    for (let i = 0; i < maxResiduePerSweep + 2; i++) await residue(e, 700 + i, `kiln:attempt=${700 + i};epoch=1;task=7;kind=review`, { head: false });
    await sweepResidue(e.deps, { ...own(e), keep: new Set(), now: Date.now(), logged: new Set(), tried: new Map() });
    expect(e.orca.worktreeRemoves.length).toBe(maxResiduePerSweep);
    await sweepResidue(e.deps, { ...own(e), keep: new Set(), now: Date.now(), logged: new Set(), tried: new Map() });
    expect(e.orca.worktreeRemoves.length).toBe(maxResiduePerSweep + 2);
  });

  it('一直受阻的残留不挡住后面的：每轮名额不变，各轮轮换到没试过的、试得最早的', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    await withClone(e);
    const blocked: string[] = [];
    for (let i = 0; i < maxResiduePerSweep; i++) {
      const r = await residue(e, 900 + i, `kiln:attempt=${900 + i};epoch=1;task=7;kind=review`, { head: false });
      e.orca.extraTerminals.set(await canon(r.wt), 1);
      blocked.push(r.wt);
    }
    const free = await residue(e, 999, 'kiln:attempt=999;epoch=1;task=7;kind=review', { head: false });
    const o = { keep: new Set<string>(), logged: new Set<string>(), tried: new Map<string, number>() };
    const t0 = Date.now();
    await sweepResidue(e.deps, { ...own(e), ...o, now: t0 });
    // 第一轮的名额被排在前面的受阻项用完（列表按路径排序，999 在最后）
    expect(existsSync(free.wt)).toBe(true);
    await sweepResidue(e.deps, { ...own(e), ...o, now: t0 + residueQuietMs });
    expect(await gone(e, free.wt)).toBe(true);
    for (const p of blocked) expect(existsSync(p)).toBe(true);
    // 回收了的不再记着
    expect(o.tried.size).toBe(maxResiduePerSweep);
  });

  it('回收之前先写 journal 中的回收记录，回收完删除；还不能回收（wait）时也删除', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    await withClone(e);
    const r = await residue(e, 504, 'kiln:attempt=504;epoch=1;task=7;kind=review');
    const busy = await residue(e, 505, 'kiln:attempt=505;epoch=1;task=7;kind=review', { head: false });
    e.orca.extraTerminals.set(await canon(busy.wt), 1);
    const saved: JournalRecord[] = [];
    const journal = { save: (rec: JournalRecord) => (saved.push(structuredClone(rec)), e.journal.save(rec)), remove: (id: string) => e.journal.remove(id) };
    // 删除第一个东西的时候，记录已经在 journal 中
    let atFirstRemove: string[] | undefined;
    const remove = e.orca.worktreeRemove.bind(e.orca);
    e.orca.worktreeRemove = async (id, force) => {
      atFirstRemove ??= (await records(e.journal)).map((x) => x.attemptId);
      return remove(id, force);
    };
    const now = Date.now();
    await sweepResidue(e.deps, { ...own(e), journal, keep: new Set(), now, logged: new Set(), tried: new Map() });
    expect(await gone(e, r.wt)).toBe(true);
    expect(saved.map((x) => x.attemptId).sort()).toEqual(['504', '505']);
    expect(saved.find((x) => x.attemptId === '504')).toEqual({
      attemptId: '504',
      epoch: '1',
      spec: '',
      stage: 'reclaiming',
      binding: { ...emptyBinding(), worktreePath: await canon(r.wt), worktreeName: 'kiln-a504-e1' },
      startedAt: now,
      reclaim: { since: now, repo: '42', clone: clonePath(e), kind: 'review', start: '', pushed: '' },
    });
    expect(atFirstRemove).toContain('504');
    expect(await records(e.journal)).toEqual([]);
    expect(tracked).toEqual([]);
    expect(existsSync(busy.wt)).toBe(true);
  });

  it('写不进 journal 时不回收', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const log = recordLog(e);
    await withClone(e);
    const r = await residue(e, 506, 'kiln:attempt=506;epoch=1;task=7;kind=review');
    const journal = { save: () => Promise.reject(new Error('ENOSPC')), remove: (id: string) => e.journal.remove(id) };
    await sweepResidue(e.deps, { ...own(e), journal, keep: new Set(), now: Date.now(), logged: new Set(), tried: new Map() });
    expect(e.orca.worktreeRemoves).toEqual([]);
    expect(existsSync(r.wt)).toBe(true);
    expect(await branches(e)).toContain('refs/heads/orca-user/kiln-a506-e1');
    expect(log.some((l) => l.msg.startsWith('写不进 journal，不回收'))).toBe(true);
  });

  it('目录已经不在、只剩登记的残留，删了登记、分支被锁住删不掉：按 journal 中的回收记录重试，锁解除后删除分支', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const log = recordLog(e);
    await withClone(e);
    const r = await residue(e, 503, 'kiln:attempt=503;epoch=1;task=7;kind=review', { head: false });
    await rm(r.wt, { recursive: true, force: true });
    const lock = path.join(clonePath(e), '.git', 'refs', 'heads', 'orca-user', 'kiln-a503-e1.lock');
    await writeFile(lock, '');
    // 节点的周期清理：登记删掉了，分支删不掉，回收失败；回收记录留在 journal 中、交给节点重试
    await runNode(e, () => Promise.resolve(log.some((l) => l.msg.startsWith('回收历史残留的工作区失败'))), '第一轮回收失败');
    expect(await gone(e, r.wt)).toBe(true);
    expect(await branches(e)).toContain('refs/heads/orca-user/kiln-a503-e1');
    expect((await records(e.journal)).map((x) => [x.attemptId, x.stage])).toEqual([['503', 'reclaiming']]);
    // 锁解除后（包括节点重启之后）：按记录重试，删除分支、删除记录
    await rm(lock);
    await runNode(e, async () => (await records(e.journal)).length === 0, '按记录重试');
    expect(await branches(e)).not.toContain('refs/heads/orca-user/kiln-a503-e1');
    expect(log.some((l) => l.msg === '已回收工作区')).toBe(true);
  });

  it('同上，回收失败后重试的是同一个节点（没有重启）', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const log = recordLog(e);
    await withClone(e);
    const r = await residue(e, 507, 'kiln:attempt=507;epoch=1;task=7;kind=review', { head: false });
    await rm(r.wt, { recursive: true, force: true });
    const lock = path.join(clonePath(e), '.git', 'refs', 'heads', 'orca-user', 'kiln-a507-e1.lock');
    await writeFile(lock, '');
    const node = makeNode(e);
    const ac = new AbortController();
    const running = node.run(ac.signal);
    try {
      await waitFor(() => Promise.resolve(log.some((l) => l.msg.startsWith('回收历史残留的工作区失败'))), 10_000, '第一轮回收失败');
      await rm(lock);
      await waitFor(async () => (await records(e.journal)).length === 0, 10_000, '按记录重试');
    } finally {
      ac.abort();
      await running;
    }
    expect(await branches(e)).not.toContain('refs/heads/orca-user/kiln-a507-e1');
  });

  it('节点的周期清理回收残留，journal 中有记录的不碰', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    await withClone(e);
    const done = await residue(e, 801, 'kiln:attempt=801;epoch=1;task=7;kind=review');
    const kept = await residue(e, 802, 'kiln:attempt=802;epoch=1;task=7;kind=review');
    // 802 还在 journal 中（例如没能确认停止）
    const rec: JournalRecord = { attemptId: '802', epoch: '1', spec: '', stage: 'stopping', binding: { ...emptyBinding(), dispatchId: 'ctx_802' }, startedAt: Date.now(), stopSince: Date.now() };
    await e.journal.save(rec);
    e.orca.stopConfirms = false;
    await runNode(e, () => Promise.resolve(!existsSync(done.wt)), '周期清理回收残留');
    expect(existsSync(kept.wt)).toBe(true);
  });
});
