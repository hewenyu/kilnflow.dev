import { existsSync } from 'node:fs';
import { chmod, mkdir, mkdtemp, readFile, rm, utimes, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { Code, ConnectError } from '@connectrpc/connect';
import { afterEach, describe, expect, it } from 'vitest';
import { create, fromBinary, toBinary } from '@bufbuild/protobuf';
import { AITaskSchema, Outcome } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { Attempt } from '../src/core/attempt.js';
import { ErrorClass, RevokedError } from '../src/core/errors.js';
import { emptyBinding, type JournalRecord } from '../src/core/journal.js';
import { nopLogger } from '../src/core/log.js';
import { mailboxRepoDir, mailboxTitle } from '../src/core/mailbox.js';
import { Node, type NodeOptions, protocolVersion } from '../src/core/node.js';
import { type Exec, nodeExec } from '../src/core/exec.js';
import { OrcaCli } from '../src/core/orca.js';
import { DockerServices, prepareDockerConfig, projectName } from '../src/core/services.js';
import { devTask, type Env, FakeServices, makeEnv, records, reviewTask, staleError, waitFor } from './harness.js';

// Agent 脚本、钩子中的失败由 test/setup.ts 在测试结束时检查（harness 的 Faults）
let env: Env | undefined;
afterEach(async () => {
  await env?.cleanup();
  env = undefined;
});

const report = JSON.stringify({ version: 1, verdict: 'approve', summary: 'ok', findings: [] });

function makeNode(e: Env, capacity: number, drained = () => false, restopIntervalMs?: number, dockerOk = false, extra: Partial<NodeOptions> = {}): Node {
  const node: Node = new Node(e.deps, {
    version: '0.1.0',
    capacity,
    hostId: 'host-1',
    orcaStatus: () => e.orca.status(),
    dockerStatus: () => Promise.resolve(dockerOk ? { ok: true, reason: '' } : { ok: false, reason: '没有 Docker' }),
    drained,
    restopIntervalMs,
    ...extra,
  });
  e.orca.tool = (id, cmd, args) => node.attempt(id)?.tool(cmd, args) ?? Promise.resolve({ code: 1, output: '不在执行' });
  return node;
}

describe('Node', () => {
  it('并行执行到容量上限，满了只上报不领取', async () => {
    let release = false;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      await waitFor(() => release, 20_000, '放行');
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    env.kiln.tasks = [1, 2, 3].map(() => reviewTask(env!.origin, { sha, baseSha: sha }));
    const node = makeNode(env, 2, undefined, undefined, false, { idleMs: 20 });
    const ac = new AbortController();
    const running = node.run(ac.signal);
    await waitFor(() => env!.orca.starts.length === 2, 10_000, '两个任务启动');
    // 满了之后每轮（idleMs）只上报：等两个任务都启动后的几次上报
    const before = env.kiln.acquires.length;
    await waitFor(() => env!.kiln.acquires.length >= before + 3, 10_000, '继续上报');
    // 满了：上报 standby，不再领取第三个
    expect(env.kiln.acquires.slice(before).every((a) => a.standby)).toBe(true);
    expect(env.kiln.acquires.at(-1)?.state?.running).toBe(2);
    expect(env.kiln.acquires.at(-1)?.state?.capacity).toBe(2);
    expect(env.kiln.acquires.at(-1)?.state?.protocol).toBe(protocolVersion);
    expect(env.orca.starts).toHaveLength(2);
    release = true;
    await waitFor(() => env!.kiln.completes.length === 3, 15_000, '三个任务完成');
    ac.abort();
    await running;
    expect(env.kiln.completes.every((c) => c.outcome === Outcome.SUCCEEDED)).toBe(true);
  });

  it('每次领取用新的 request_id', async () => {
    env = await makeEnv(() => Promise.resolve());
    const node = makeNode(env, 1);
    const ac = new AbortController();
    const running = node.run(ac.signal);
    await waitFor(() => env!.kiln.acquires.length >= 3, 5_000, '领取');
    ac.abort();
    await running;
    const ids = env.kiln.acquires.map((a) => a.requestId);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it('领取的请求没有到达（网络错误）：重试沿用同一个 request_id，成功之后才换新的', async () => {
    env = await makeEnv(() => Promise.resolve());
    env.kiln.acquireFaults = ['error', 'error'];
    const ac = new AbortController();
    const running = makeNode(env, 1, undefined, undefined, false, { idleMs: 20 }).run(ac.signal);
    await waitFor(() => env!.kiln.acquires.length >= 5, 5_000, '领取');
    ac.abort();
    await running;
    const ids = env.kiln.acquires.map((a) => a.requestId);
    // 第 1、2 次失败，第 3 次（同一个 request_id）成功；之后每次都是新的
    expect(ids[1]).toBe(ids[0]);
    expect(ids[2]).toBe(ids[0]);
    expect(new Set(ids.slice(2)).size).toBe(ids.length - 2);
  });

  it('Server 已经分配了任务但响应丢失：用同一个 request_id 重试取回同一个任务，执行且只执行一次', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    const task = reviewTask(env.origin, { sha, baseSha: sha });
    env.kiln.tasks = [task];
    env.kiln.acquireFaults = ['lost'];
    const ac = new AbortController();
    const running = makeNode(env, 1, undefined, undefined, false, { idleMs: 20 }).run(ac.signal);
    await waitFor(() => env!.kiln.completes.length === 1, 10_000, '任务完成');
    const after = env.kiln.acquires.length;
    await waitFor(() => env!.kiln.acquires.length >= after + 2, 5_000, '继续领取');
    ac.abort();
    await running;
    const ids = env.kiln.acquires.map((a) => a.requestId);
    // 第 1 次分配了任务但响应丢失，第 2 次用同一个 request_id 取回；成功之后换新的
    expect(ids[1]).toBe(ids[0]);
    expect(new Set(ids.slice(1)).size).toBe(ids.length - 1);
    expect(env.orca.starts).toHaveLength(1);
    expect(env.kiln.completes).toHaveLength(1);
    expect(env.kiln.completes[0]?.ref?.attemptId).toBe(task.ref?.attemptId);
    expect(env.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
  });

  it('drain 或 Orca 没有就绪时不领取', async () => {
    env = await makeEnv(() => Promise.resolve());
    const sha = await env.origin.branchSha('master');
    env.kiln.tasks = [reviewTask(env.origin, { sha, baseSha: sha })];
    env.orca.ready = false;
    const node = makeNode(env, 1);
    const ac = new AbortController();
    const running = node.run(ac.signal);
    await waitFor(() => env!.kiln.acquires.length >= 1, 5_000, '上报');
    ac.abort();
    await running;
    expect(env.kiln.acquires.every((a) => a.standby)).toBe(true);
    expect(env.kiln.acquires[0]?.state?.orca?.ready).toBe(false);
    expect(env.kiln.tasks).toHaveLength(1);
  });

  // 本机执行不了 Orca 命令行（config.json 的 orca 指向的程序不存在）：真实的 OrcaCli，节点上报未就绪、写明原因，不领取
  it('执行不了 Orca 命令行时不就绪、不领取', async () => {
    env = await makeEnv(() => Promise.resolve());
    const sha = await env.origin.branchSha('master');
    env.kiln.tasks = [reviewTask(env.origin, { sha, baseSha: sha })];
    const empty = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-nopath-'));
    try {
      const orca = new OrcaCli(nodeExec, path.join(empty, 'orca-ide'));
      const node = makeNode(env, 1, undefined, undefined, false, { orcaStatus: () => orca.status() });
      const ac = new AbortController();
      const running = node.run(ac.signal);
      await waitFor(() => env!.kiln.acquires.length >= 1, 5_000, '上报');
      ac.abort();
      await running;
      expect(env.kiln.acquires.every((a) => a.standby)).toBe(true);
      expect(env.kiln.acquires[0]?.state?.orca?.ready).toBe(false);
      expect(env.kiln.acquires[0]?.state?.orca?.reason).toMatch(/exec_failed.*ENOENT/);
      expect(env.kiln.tasks).toHaveLength(1);
      expect(env.orca.starts).toEqual([]);
    } finally {
      await rm(empty, { recursive: true, force: true });
    }
  });

  it('上报的平台用 Go 的写法：x86-64 的 Linux 是 linux/amd64（不是 Node 的 linux/x64），Server 按它匹配任务的平台', async () => {
    env = await makeEnv(() => Promise.resolve());
    const saved = { platform: process.platform, arch: process.arch };
    Object.defineProperty(process, 'platform', { value: 'linux' });
    Object.defineProperty(process, 'arch', { value: 'x64' });
    try {
      const node = makeNode(env, 1);
      const ac = new AbortController();
      const running = node.run(ac.signal);
      await waitFor(() => env!.kiln.acquires.length >= 1, 5_000, '上报');
      ac.abort();
      await running;
    } finally {
      Object.defineProperty(process, 'platform', { value: saved.platform });
      Object.defineProperty(process, 'arch', { value: saved.arch });
    }
    expect(env.kiln.acquires[0]?.state?.platform).toBe('linux/amd64');
  });

  it('退出时不停止 Agent，重启后从 journal 恢复', async () => {
    let finish = false;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      await waitFor(() => finish, 10_000, '放行');
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    env.kiln.tasks = [reviewTask(env.origin, { sha, baseSha: sha })];
    const ac = new AbortController();
    const running = makeNode(env, 1).run(ac.signal);
    await waitFor(() => env!.orca.starts.length === 1, 5_000, '启动');
    await waitFor(async () => (await records(env!.journal))[0]?.stage === 'running', 5_000, '运行');
    ac.abort();
    await running;
    expect(env.orca.stops).toEqual([]);
    expect(env.kiln.completes).toEqual([]);

    finish = true;
    const ac2 = new AbortController();
    const again = makeNode(env, 1).run(ac2.signal);
    await waitFor(() => env!.kiln.completes.length === 1, 10_000, '恢复后完成');
    ac2.abort();
    await again;
    expect(env.orca.starts).toHaveLength(1);
    expect(env.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
  });

  it('被吊销时停止运行', async () => {
    env = await makeEnv(() => Promise.resolve());
    env.kiln.acquire = () => Promise.reject(new RevokedError());
    await expect(makeNode(env, 1).run(new AbortController().signal)).rejects.toBeInstanceOf(RevokedError);
  });

  it('Agent 的提问由邮箱自动回复', async () => {
    env = await makeEnv(() => Promise.resolve());
    const node = makeNode(env, 1);
    const ac = new AbortController();
    const running = node.run(ac.signal);
    env.orca.send({ type: 'question', id: 'q1', body: '要不要改接口？' });
    await waitFor(() => env!.orca.replies.length === 1, 5_000, '回复');
    ac.abort();
    await running;
    expect(env.orca.replies[0]).toMatch(/^q1:.*kiln-orca task/);
  });

  it('回复失败时这一批留在内存中重试，不读下一批；回复成功后才发新的 check', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    e.orca.replyFaults = 2;
    // 每次回复（包括失败的）时已经发出的 check 数
    const checksAtReply: number[] = [];
    const reply = e.orca.reply.bind(e.orca);
    e.orca.reply = (from, id, body) => {
      checksAtReply.push(e.orca.checks.length);
      return reply(from, id, body);
    };
    const node = makeNode(e, 1);
    const ac = new AbortController();
    const running = node.run(ac.signal);
    e.orca.send({ type: 'question', id: 'q1', body: '?' });
    await waitFor(() => e.orca.replies.length === 1, 10_000, '重试后回复');
    await waitFor(() => e.orca.checks.length > (checksAtReply[2] ?? Infinity), 5_000, '回复成功后的下一次 check');
    ac.abort();
    await running;
    expect(checksAtReply).toHaveLength(3);
    expect(new Set(checksAtReply).size).toBe(1);
    expect(e.orca.handled).toEqual(['q1']);
    // 读取邮箱时显式指定邮箱的 Run
    const { runId } = await e.deps.mailbox.ensure();
    expect([...e.orca.checkRuns]).toEqual([runId]);
  });

  it('不能确认停止的 Agent 记在 journal 中：重启后继续占用容量，确认停止后删除', async () => {
    // Agent 写出改动后一直运行，直到真正被停止（重启后确认停止时）；Server 在这之后才取消，保证取消时 Agent 已经启动
    let working = false;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.worktree, 'wip.txt'), '一半\n');
      working = true;
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    const e = env;
    e.orca.stopConfirms = false;
    e.kiln.heartbeatFn = () => ({ cancelRequested: working });
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    e.orca.tool = (_id, cmd, args) => at.tool(cmd, args);
    const ac0 = new AbortController();
    const pump = e.deps.mailbox.pump(ac0.signal);
    await at.run();
    ac0.abort();
    await pump;
    expect(at.unconfirmedStop).toBe(true);
    const [rec] = await records(e.journal);
    expect(rec?.stage).toBe('stopping');
    expect(rec?.binding.dispatchId).toBe(at.binding.dispatchId);
    expect(rec?.stopSince).toBeGreaterThan(0);

    // 重启：还没确认停止的 Agent 占用容量（容量 1 时只上报、不领取）
    const ac = new AbortController();
    const running = makeNode(e, 1, () => false, 50, false, { idleMs: 20 }).run(ac.signal);
    await waitFor(() => e.kiln.acquires.length >= 1, 5_000, '上报');
    expect(e.kiln.acquires[0]?.standby).toBe(true);
    expect(e.kiln.acquires[0]?.state?.running).toBe(1);
    const stopsBefore = e.orca.stops.length;

    // 到了再停的时间且能确认停止：不再占用容量，回收开发的工作区（PLAN #33.10）后删除 journal
    e.orca.stopConfirms = true;
    await waitFor(() => e.kiln.acquires.some((a) => !a.standby), 15_000, '恢复领取');
    await waitFor(async () => (await records(e.journal)).every((r) => r.attemptId !== rec?.attemptId), 15_000, '回收开发的工作区');
    ac.abort();
    await running;
    // 重启后继续停的是同一个 Agent
    expect(e.orca.stops.length).toBeGreaterThan(stopsBefore);
    expect(new Set(e.orca.stops)).toEqual(new Set([at.binding.dispatchId]));
    expect(e.kiln.acquires.find((a) => !a.standby)?.state?.running).toBe(0);
    expect((await records(e.journal)).find((r) => r.attemptId === rec?.attemptId)).toBeUndefined();
  });

  // 运行中 Orca 命令行暂时执行不了：注入真实的 OrcaCli 对不存在的命令行得到的错误。
  // 等待结束时不当作 dispatch 不存在（不以 infra.orca 结束），停止时不当作已经停止（记为 stopping、不释放、不回收）；
  // 命令行回来之后节点再停、释放、回收工作区
  it('运行中 Orca 命令行暂时执行不了：不当作 Agent 已结束或已停止；恢复后停止、释放、回收', async () => {
    let working = false;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.worktree, 'wip.txt'), '一半\n');
      working = true;
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    const e = env;
    const missing = path.join(os.tmpdir(), `kiln-orca-missing-${process.pid}`, 'orca-ide');
    const vanished = await new OrcaCli(nodeExec, missing).workerStop('ctx_x').then(
      () => undefined,
      (err: unknown) => err as Error,
    );
    expect(vanished).toMatchObject({ code: 'exec_failed' });
    let down = false;
    let showsWhileDown = 0;
    const orig = { stop: e.orca.workerStop.bind(e.orca), release: e.orca.workerRelease.bind(e.orca), show: e.orca.workerShow.bind(e.orca), request: e.orca.requestShow.bind(e.orca) };
    e.orca.workerStop = (d) => (down ? Promise.reject(vanished) : orig.stop(d));
    e.orca.workerRelease = (d) => (down ? Promise.reject(vanished) : orig.release(d));
    e.orca.requestShow = (id) => (down ? Promise.reject(vanished) : orig.request(id));
    e.orca.workerShow = (d) => {
      if (!down) return orig.show(d);
      showsWhileDown++;
      return Promise.reject(vanished);
    };
    let cancel = false;
    e.kiln.heartbeatFn = () => ({ cancelRequested: cancel });
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    e.orca.tool = (_id, cmd, args) => at.tool(cmd, args);
    const ac0 = new AbortController();
    const pump = e.deps.mailbox.pump(ac0.signal);
    const running0 = at.run();
    await waitFor(() => working, 5_000, 'Agent 开始工作');
    down = true;
    // 远多于"dispatch 不存在"的宽限次数：仍在等 Agent，没有结束
    await waitFor(() => showsWhileDown >= 10, 10_000, '命令行执行不了期间的查询');
    expect(e.kiln.completes).toEqual([]);
    cancel = true;
    await running0;
    ac0.abort();
    await pump;
    expect(at.unconfirmedStop).toBe(true);
    const [rec] = await records(e.journal);
    expect(rec?.stage).toBe('stopping');
    // 回收的责任已经记下，确认停止、释放之后才做
    expect(rec?.reclaim?.kind).toBe('develop');
    expect(existsSync(at.binding.worktreePath)).toBe(true);
    expect(e.orca.releases).toEqual([]);
    expect(e.orca.worktreeRemoves).toEqual([]);

    // 重启，命令行仍然执行不了：继续占用容量、不回收
    const ac = new AbortController();
    const running = makeNode(e, 1, () => false, 50, false, { idleMs: 20 }).run(ac.signal);
    await waitFor(() => e.kiln.acquires.length >= 2, 5_000, '上报');
    expect(e.kiln.acquires.every((a) => a.standby)).toBe(true);
    expect(e.orca.worktreeRemoves).toEqual([]);

    // 命令行回来：再停、释放、回收工作区，删除 journal
    down = false;
    await waitFor(async () => (await records(e.journal)).length === 0, 15_000, '停止、释放、回收');
    ac.abort();
    await running;
    expect(e.orca.stops.at(-1)).toBe(at.binding.dispatchId);
    expect(e.orca.releases).toContain(at.binding.dispatchId);
    expect(existsSync(at.binding.worktreePath)).toBe(false);
  });

  it('提交结果没成功就退出：journal 保留，重启后重新结束、只启动过一次 Agent', async () => {
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const e = env;
    e.kiln.completeDown = new ConnectError('down', Code.Unavailable);
    const sha = await e.origin.branchSha('master');
    e.kiln.tasks = [reviewTask(e.origin, { sha, baseSha: sha })];
    const ac = new AbortController();
    const running = makeNode(e, 1).run(ac.signal);
    await waitFor(() => e.kiln.completeCalls >= 1, 10_000, '提交');
    ac.abort();
    await running;
    expect(e.kiln.completes).toEqual([]);
    expect((await records(e.journal))[0]?.stage).toBe('finishing');

    e.kiln.completeDown = undefined;
    const ac2 = new AbortController();
    const again = makeNode(e, 1).run(ac2.signal);
    await waitFor(() => e.kiln.completes.length === 1, 10_000, '恢复后提交');
    ac2.abort();
    await again;
    expect(e.orca.starts).toHaveLength(1);
    expect(e.orca.stops).toEqual([]);
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
    expect(await records(e.journal)).toEqual([]);
  });

  it('测试服务删除失败后，重启的节点回收残留的项目；正在执行（恢复中）的任务的项目不回收；Docker 不可用时跳过', async () => {
    const services = new FakeServices();
    let release = false;
    let second = '';
    env = await makeEnv(
      async (ctx) => {
        await writeFile(path.join(ctx.taskDir, 'report.json'), report);
        if (path.basename(ctx.taskDir) === second) await waitFor(() => release, 15_000, '放行');
        ctx.done();
      },
      { services },
    );
    const sha = await env.origin.branchSha('master');
    const [t1, t2] = [1, 2].map(() => reviewTask(env!.origin, { sha, baseSha: sha, services: ['pg'] }));
    second = String(t2!.ref!.attemptId);
    const p1 = projectName(services.owner, t1!.ref!.attemptId);
    const p2 = projectName(services.owner, second);
    env.kiln.tasks = [t1!, t2!];
    // 第一个任务结束时删除测试服务失败
    services.failStops = 1;
    const ac = new AbortController();
    const running = makeNode(env, 2).run(ac.signal);
    await waitFor(() => env!.kiln.completes.length === 1, 10_000, '第一个任务完成');
    await waitFor(async () => (await records(env!.journal)).some((r) => r.attemptId === second && r.stage === 'running'), 10_000, '第二个任务运行');
    ac.abort();
    await running;
    expect(services.stops).toEqual([p1]);
    expect([...services.projects].sort()).toEqual([p1, p2].sort());
    // Docker 不可用的节点不回收
    expect(services.cleanups).toEqual([]);
    expect((await records(env.journal)).map((r) => r.attemptId)).toEqual([second]);

    const ac2 = new AbortController();
    const again = makeNode(env, 2, () => false, undefined, true).run(ac2.signal);
    await waitFor(() => !services.projects.has(p1), 10_000, '回收残留的项目');
    expect(services.cleanups[0]).toEqual([second]);
    expect(services.projects.has(p2)).toBe(true);
    release = true;
    await waitFor(() => env!.kiln.completes.length === 2, 10_000, '第二个任务完成');
    await waitFor(() => services.projects.size === 0, 5_000, '第二个任务的服务删除');
    ac2.abort();
    await again;
    expect(env.orca.starts).toHaveLength(2);
  });
});

describe('Node：专用 Docker 配置生成失败', () => {
  it('恢复带测试服务的任务不中断（删除服务失败留给之后回收）；不需要服务的任务照常执行，需要服务的按"不可用"执行；修好后恢复可用并回收；没有专用配置时不执行 docker', async () => {
    const fake = new FakeServices();
    let release = false;
    let blocked = '';
    const prompts = new Map<string, string>();
    env = await makeEnv(
      async (ctx) => {
        const id = path.basename(ctx.taskDir);
        prompts.set(id, ctx.opts.spec);
        await writeFile(path.join(ctx.taskDir, 'report.json'), report);
        if (id === blocked) await waitFor(() => release, 15_000, '放行');
        ctx.done();
      },
      { services: fake },
    );
    const e = env;
    const sha = await e.origin.branchSha('master');
    const t1 = reviewTask(e.origin, { sha, baseSha: sha, services: ['pg'] });
    blocked = String(t1.ref!.attemptId);
    const p1 = projectName(fake.owner, blocked);
    e.kiln.tasks = [t1];
    // 第一次运行：Docker 可用，拉起了测试服务；任务还在执行时连接器退出
    const ac = new AbortController();
    const first = makeNode(e, 3, () => false, undefined, true).run(ac.signal);
    await waitFor(async () => (await records(e.journal)).some((r) => r.attemptId === blocked && r.stage === 'running'), 10_000, '任务运行');
    ac.abort();
    await first;
    expect((await records(e.journal))[0]?.services?.project).toBe(p1);

    // 重启时用户的 Docker 配置坏了。假的 docker：容器就是 fake 中剩下的项目，记录每次调用的 DOCKER_CONFIG
    const userDir = path.join(e.root, 'user-docker');
    await mkdir(userDir);
    await writeFile(path.join(userDir, 'config.json'), '{');
    const dockerDir = path.join(e.root, 'home', 'docker');
    const dockerCalls: { args: string[]; config: string | undefined }[] = [];
    const exec: Exec = (_cmd, args, opts) => {
      dockerCalls.push({ args: [...args], config: opts?.env?.DOCKER_CONFIG });
      let stdout = '';
      if (args[0] === 'ps' && args.includes('label=kiln.orca.home=' + fake.owner)) stdout = [...fake.projects].join('\n');
      if (args[0] === 'compose' && args.includes('down')) fake.projects.delete(args[args.indexOf('-p') + 1] ?? '');
      return Promise.resolve({ code: 0, stdout, stderr: '' });
    };
    const settings = { docker: 'docker', postgresImage: 'postgres:17', redisImage: 'redis:7', postgresPort: 55432, redisPort: 56379 };
    const docker = new DockerServices(exec, settings, () => prepareDockerConfig(dockerDir, userDir), fake.owner);
    const restart = () => {
      const node: Node = new Node(
        { ...e.deps, services: docker, servicesReady: () => node.servicesReady },
        { version: '0.1.0', capacity: 3, hostId: 'host-1', orcaStatus: () => e.orca.status(), dockerStatus: () => docker.available(), drained: () => false, idleMs: 20, statusIntervalMs: 50 },
      );
      e.orca.tool = (id, cmd, args) => node.attempt(id)?.tool(cmd, args) ?? Promise.resolve({ code: 1, output: '不在执行' });
      return node;
    };
    const t2 = reviewTask(e.origin, { sha, baseSha: sha });
    const t3 = reviewTask(e.origin, { sha, baseSha: sha, services: ['pg'] });
    e.kiln.tasks = [t2, t3];
    release = true;
    const ac2 = new AbortController();
    const again = restart().run(ac2.signal);
    await waitFor(() => e.kiln.completes.length === 3, 15_000, '三个任务完成');
    // 恢复的任务正常提交；它的测试服务删除失败（没有专用配置），留给之后回收
    expect(e.kiln.completes.map((c) => c.outcome)).toEqual([Outcome.SUCCEEDED, Outcome.SUCCEEDED, Outcome.SUCCEEDED]);
    expect(fake.projects.has(p1)).toBe(true);
    // 需要测试服务的任务按"不可用"执行，说明中写明原因
    expect(prompts.get(String(t3.ref!.attemptId))).toContain('测试服务（pg）不可用：无法生成专用的 Docker 客户端配置');
    const st = e.kiln.acquires.at(-1)?.state;
    expect(st?.docker?.ready).toBe(false);
    expect(st?.docker?.reason).toContain(`${path.join(userDir, 'config.json')} 不是有效的 JSON`);
    expect(dockerCalls).toEqual([]);

    // 修好配置：不用重启，下一次检测恢复可用
    await writeFile(path.join(userDir, 'config.json'), '{}');
    await waitFor(() => e.kiln.acquires.at(-1)?.state?.docker?.ready === true, 5_000, 'Docker 恢复可用');
    ac2.abort();
    await again;
    // 重启后恢复之后回收残留的项目（周期清理同样）
    const ac3 = new AbortController();
    const third = restart().run(ac3.signal);
    await waitFor(() => !fake.projects.has(p1), 5_000, '回收残留的项目');
    ac3.abort();
    await third;
    expect(dockerCalls.length).toBeGreaterThan(0);
    for (const c of dockerCalls) expect(c.config).toBe(dockerDir);
  });
});

describe('Node：执行出错与 journal 不可用', () => {
  it('结束流程中 journal 写入失败、不能确认停止：仍占用容量、继续再停，确认停止后释放', async () => {
    let working = false;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.worktree, 'wip.txt'), '一半\n');
      working = true;
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    const e = env;
    e.orca.stopConfirms = false;
    e.kiln.heartbeatFn = () => ({ cancelRequested: working });
    const save = e.journal.save.bind(e.journal);
    let failed = false;
    let probesAtFailure = 0;
    e.journal.save = (r) => {
      if (!failed && r.verdict) {
        failed = true;
        probesAtFailure = probes;
        return Promise.reject(new Error('ENOSPC'));
      }
      return save(r);
    };
    let probes = 0;
    const probe = e.journal.probe.bind(e.journal);
    e.journal.probe = () => {
      probes++;
      return probe();
    };
    const sha = await e.origin.branchSha('master');
    const task = devTask(e.origin, { sha, mode: 'implement' });
    e.kiln.tasks = [task];
    const ac = new AbortController();
    const node = makeNode(e, 1, () => false, 50, false, { idleMs: 20 });
    const running = node.run(ac.signal);
    await waitFor(() => failed && e.orca.stops.length >= 1, 10_000, '出错并停止');
    // run 出错后这个任务不在执行，但 Agent 没能确认停止：仍计入容量，不领取
    await waitFor(() => node.attempt(String(task.ref!.attemptId)) === undefined, 5_000, '任务结束执行');
    // 执行出错后立即重新检测 journal 能否写入（不等 60 秒的状态周期）
    await waitFor(() => probes > probesAtFailure, 2_000, '重新检测 journal');
    const dispatch = e.orca.stops[0]!;
    const before = e.kiln.acquires.length;
    await waitFor(() => e.kiln.acquires.length >= before + 2, 5_000, '继续上报');
    for (const a of e.kiln.acquires.slice(before)) {
      expect(a.standby).toBe(true);
      expect(a.state?.running).toBe(1);
    }
    expect(node.busy).toBe(1);
    // 继续再停同一个 Agent
    await waitFor(() => e.orca.stops.length >= 2, 5_000, '再停');
    expect(new Set(e.orca.stops)).toEqual(new Set([dispatch]));
    e.orca.stopConfirms = true;
    await waitFor(() => node.busy === 0, 5_000, '确认停止后释放');
    await waitFor(() => e.kiln.acquires.some((a, i) => i >= before && !a.standby), 5_000, '恢复领取');
    ac.abort();
    await running;
    expect(e.kiln.completeCalls).toBe(0);
  });

  it('journal 写不进去时不领取新任务（standby），恢复后继续领取', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    let broken = true;
    const probe = e.journal.probe.bind(e.journal);
    e.journal.probe = () => (broken ? Promise.reject(new Error('EROFS: read-only file system')) : probe());
    const sha = await e.origin.branchSha('master');
    e.kiln.tasks = [reviewTask(e.origin, { sha, baseSha: sha })];
    const ac = new AbortController();
    // 状态每 60 秒检测一次：用可控的时钟让下一轮立即重新检测
    let clock = Date.now();
    e.deps.now = () => clock;
    const running = makeNode(e, 1, () => false, undefined, false, { idleMs: 20 }).run(ac.signal);
    await waitFor(() => e.kiln.acquires.length >= 3, 5_000, '上报');
    expect(e.kiln.acquires.every((a) => a.standby)).toBe(true);
    expect(e.kiln.tasks).toHaveLength(1);
    broken = false;
    clock += 61_000;
    await waitFor(() => e.kiln.acquires.some((a) => !a.standby), 5_000, '恢复领取');
    ac.abort();
    await running;
  });
});

describe('kiln-task skill', () => {
  it('同步失败时不领取新任务（standby）、上报原因，解决后恢复领取', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    e.skill.failAll = 'codex：EACCES，~/.agents 不可写';
    const sha = await e.origin.branchSha('master');
    e.kiln.tasks = [reviewTask(e.origin, { sha, baseSha: sha })];
    // 状态每 60 秒检测一次：用可控的时钟让下一轮立即重新检测
    let clock = Date.now();
    e.deps.now = () => clock;
    const ac = new AbortController();
    const running = makeNode(e, 1, () => false, undefined, false, { idleMs: 20 }).run(ac.signal);
    await waitFor(() => e.kiln.acquires.length >= 1, 5_000, '上报');
    const first = e.kiln.acquires[0];
    expect(first?.standby).toBe(true);
    expect(first?.state?.orca?.ready).toBe(false);
    expect(first?.state?.orca?.reason).toContain('kiln-task skill：codex：EACCES');
    expect(e.skill.all[0]).toEqual(['codex', 'claude-code']);
    expect(e.orca.starts).toEqual([]);
    expect(e.kiln.tasks).toHaveLength(1);

    e.skill.failAll = '';
    clock += 61_000;
    await waitFor(() => e.kiln.acquires.some((a) => !a.standby), 5_000, '恢复领取');
    const ready = e.kiln.acquires.find((a) => !a.standby);
    expect(ready?.state?.orca?.reason ?? '').not.toContain('kiln-task skill');
    ac.abort();
    await running;
  });

  it('同步失败不影响已经在执行的任务：它照常完成', async () => {
    let release = false;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      await waitFor(() => release, 20_000, '放行');
      ctx.done();
    });
    const e = env;
    let clock = Date.now();
    e.deps.now = () => clock;
    const sha = await e.origin.branchSha('master');
    e.kiln.tasks = [reviewTask(e.origin, { sha, baseSha: sha })];
    const ac = new AbortController();
    const running = makeNode(e, 1, () => false, undefined, false, { idleMs: 20 }).run(ac.signal);
    await waitFor(() => e.orca.starts.length === 1, 10_000, '任务启动');

    e.skill.failAll = '磁盘满了';
    const before = e.kiln.acquires.length;
    clock += 61_000;
    await waitFor(
      () => e.kiln.acquires.some((a, i) => i >= before && a.standby && a.state?.running === 1),
      5_000,
      'skill 失败后 standby'
    );
    expect(e.kiln.acquires.at(-1)?.state?.orca?.reason).toContain('kiln-task skill：磁盘满了');

    release = true;
    await waitFor(() => e.kiln.completes.length === 1, 10_000, '任务完成');
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
    ac.abort();
    await running;
  });

  it('相同原因多轮检测只记一次 error（不重复刷日志），恢复时记一次 info', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const errors: string[] = [];
    const infos: string[] = [];
    e.deps.log = {
      ...nopLogger,
      error: (m) => void errors.push(m),
      info: (m) => void infos.push(m),
    };
    e.skill.failAll = 'codex：EACCES，~/.agents 不可写';
    let clock = Date.now();
    e.deps.now = () => clock;
    const ac = new AbortController();
    const running = makeNode(e, 1, () => false, undefined, false, { idleMs: 20 }).run(ac.signal);
    await waitFor(() => e.kiln.acquires.length >= 1, 5_000, '首次上报');
    // 同一个错误连续多轮检测（每轮 60 秒）：只有第一次是 error。等 skill 同步真正跑过这一轮再断言，不靠上报的时序
    clock += 61_000;
    await waitFor(() => e.skill.all.length >= 2, 5_000, '再次检测');
    clock += 61_000;
    await waitFor(() => e.skill.all.length >= 3, 5_000, '第三次检测');
    expect(errors.filter((m) => m.includes('kiln-task skill 不能同步'))).toHaveLength(1);
    expect(infos.some((m) => m.includes('恢复领取'))).toBe(false);

    // 解决后只记一次 info
    e.skill.failAll = '';
    clock += 61_000;
    await waitFor(() => infos.some((m) => m.includes('恢复领取')), 5_000, '恢复领取');
    ac.abort();
    await running;
    expect(infos.filter((m) => m.includes('恢复领取'))).toHaveLength(1);
  });
});

describe('Mailbox（自查）', () => {
  it('邮箱终端丢失后同时 ensure（邮箱循环、状态检测、启动 Agent）：只重建一个终端，Run 不变', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const first = await e.deps.mailbox.ensure();
    e.orca.terminals.delete(first.handle);
    const before = e.orca.terminals.size;
    const got = await Promise.all([e.deps.mailbox.ensure(), e.deps.mailbox.ensure(), e.deps.mailbox.ensure()]);
    expect(e.orca.terminals.size).toBe(before + 1);
    expect(new Set(got.map((g) => g.handle)).size).toBe(1);
    expect(got[0]?.handle).not.toBe(first.handle);
    expect(got.every((g) => g.runId === first.runId)).toBe(true);
    // 之后照常：终端存在时不再重建
    expect(await e.deps.mailbox.ensure()).toEqual(got[0]);
    expect(e.orca.terminals.size).toBe(before + 1);
  });
});

describe('多余的邮箱终端', () => {
  it('连接器启动后（第一次周期清理）关掉邮箱仓库中以前留下的邮箱终端，当前的、别的标题的不动', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const first = await e.deps.mailbox.ensure();
    const repo = mailboxRepoDir(path.join(e.root, 'home'));
    for (const h of ['term_l1', 'term_l2']) e.orca.addTerminal({ handle: h, title: mailboxTitle, worktreePath: repo, connected: true, orphaned: true, exitCause: '' });
    e.orca.addTerminal({ handle: 'term_user', title: 'zsh', worktreePath: repo, connected: true, orphaned: false, exitCause: '' });
    const ac = new AbortController();
    const running = makeNode(e, 1, () => false, undefined, false, { idleMs: 20 }).run(ac.signal);
    await waitFor(() => e.orca.terminalCloses.length === 2, 5_000, '清理');
    ac.abort();
    await running;
    expect(e.orca.terminalCloses).toEqual(['term_l1', 'term_l2']);
    expect(await e.orca.terminalAlive(first.handle)).toBe(true);
    expect(await e.orca.terminalAlive('term_user')).toBe(true);
  });

  it('Orca 没有就绪时不清理', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    await e.deps.mailbox.ensure();
    e.orca.addTerminal({ handle: 'term_l1', title: mailboxTitle, worktreePath: mailboxRepoDir(path.join(e.root, 'home')), connected: true, orphaned: true, exitCause: '' });
    e.orca.ready = false;
    const ac = new AbortController();
    const running = makeNode(e, 1, () => false, undefined, false, { idleMs: 20 }).run(ac.signal);
    await waitFor(() => e.kiln.acquires.length >= 3, 5_000, '上报');
    ac.abort();
    await running;
    expect(e.orca.terminalCloses).toEqual([]);
  });
});

describe('Orca 的编排状态被重置', () => {
  it('Run、dispatch 都没有了、邮箱终端还在：在途任务在限定时间内以 infra.orca 结束，邮箱在现有终端上换新 Run，节点恢复领取，新任务在新 Run 中执行', async () => {
    let stuck = true;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      // 第一个任务的 Agent 一直运行（它发往旧 Run 的 worker_done 再也收不到）；之后的任务正常结束
      if (stuck) {
        stuck = false;
        await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
        return;
      }
      ctx.done();
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    const first = reviewTask(e.origin, { sha, baseSha: sha });
    e.kiln.tasks = [first];
    const node = makeNode(e, 1, undefined, undefined, false, { idleMs: 20 });
    const ac = new AbortController();
    const running = node.run(ac.signal);
    await waitFor(async () => (await records(e.journal))[0]?.stage === 'running', 10_000, 'Agent 运行');
    const old = await e.deps.mailbox.ensure();
    const dispatch = [...e.orca.workers.keys()][0]!;
    const began = Date.now();
    e.orca.resetOrchestration();
    // 邮箱的 check 被拒绝（run_not_found）后在现有终端上换新 Run
    await waitFor(() => e.orca.runCreates.length === 2, 5_000, '换新 Run');
    const now = await e.deps.mailbox.ensure();
    expect(now.handle).toBe(old.handle);
    expect(now.runId).not.toBe(old.runId);
    await waitFor(() => e.kiln.completes.length === 1 && node.busy === 0, 10_000, '在途任务结束');
    expect(Date.now() - began).toBeLessThan(10_000);
    expect(e.kiln.completes[0]).toMatchObject({ outcome: Outcome.INFRA_ERROR, errorClass: ErrorClass.orca });
    expect(e.kiln.completes[0]?.message).toContain('编排状态被重置');
    // 停止、释放都按"Orca 中没有这个 dispatch"完成，不占容量
    expect(e.orca.stops).toEqual([dispatch]);
    expect(e.orca.releases).toEqual([dispatch]);
    // 恢复领取：新任务在新 Run 中启动、正常完成
    e.kiln.tasks = [reviewTask(e.origin, { sha, baseSha: sha })];
    await waitFor(() => e.kiln.completes.length === 2 && node.busy === 0, 10_000, '新任务完成');
    expect(e.kiln.completes[1]?.outcome).toBe(Outcome.SUCCEEDED);
    expect(e.orca.starts.map((s) => s.run)).toEqual([old.runId, now.runId]);
    await waitFor(async () => (await records(e.journal)).length === 0, 5_000, 'journal 删除');
    ac.abort();
    await running;
  });
});

describe('Node：领取到不完整的任务（自查）', () => {
  it('任务缺少 attempt 时记日志、跳过，主循环继续领取', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    e.kiln.tasks = [create(AITaskSchema, { taskId: 5n })];
    const ac = new AbortController();
    const running = makeNode(e, 1, undefined, undefined, false, { idleMs: 20 }).run(ac.signal);
    await waitFor(() => e.kiln.acquires.length >= 3, 5_000, '继续领取');
    ac.abort();
    await running;
    expect(e.kiln.tasks).toEqual([]);
  });
});

describe('启动结果未知时停止', () => {
  /** 一直运行到被停止的假 Agent。 */
  const untilStopped = (ctx: { stopped: AbortSignal }) => new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));

  /** worker-start 实际启动了 Agent 但响应丢失，request-show 一直 pending。 */
  async function lostStart(): Promise<Env> {
    const e = await makeEnv(untilStopped);
    e.orca.startFault = 'lost-after';
    let pending = true;
    e.orca.requestShowFn = () => (pending ? { state: 'pending' } : undefined);
    (e as Env & { settle(): void }).settle = () => (pending = false);
    return e;
  }
  const settle = (e: Env) => (e as Env & { settle(): void }).settle();
  const worker = (e: Env) => [...e.orca.workers.values()][0]!;
  /** journalCleared 等 journal 清空：确认停止之后先回收开发的工作区（reclaiming，不占容量），回收完删除记录（PLAN #33.10）。 */
  const journalCleared = (e: Env, advance?: () => void) =>
    waitFor(
      async () => {
        advance?.();
        return (await records(e.journal)).length === 0;
      },
      5_000,
      '回收开发的工作区后删除 journal',
    );

  type Trigger = 'cancel' | 'stale' | 'timeout';
  /** stopDuring 让 Agent 已经启动、启动结果还在查证时停止（取消、失效，或者查证超时），直接执行 Attempt。 */
  async function stopDuring(e: Env, trigger: Trigger): Promise<Attempt> {
    if (trigger === 'cancel') e.kiln.heartbeatFn = () => ({ cancelRequested: e.orca.workers.size > 0 });
    if (trigger === 'stale') e.kiln.heartbeatFn = () => (e.orca.workers.size > 0 ? staleError() : {});
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    e.orca.tool = (_id, cmd, args) => at.tool(cmd, args);
    const ac = new AbortController();
    const pump = e.deps.mailbox.pump(ac.signal);
    await at.run();
    ac.abort();
    await pump;
    return at;
  }

  /** stoppingRecord 检查 journal 记为 stopping，带启动幂等键、没有 dispatch。 */
  async function stoppingRecord(e: Env, at: Attempt): Promise<JournalRecord> {
    const [rec] = await records(e.journal);
    expect(rec?.attemptId).toBe(String(at.id));
    expect(rec?.stage).toBe('stopping');
    expect(rec?.binding.startRequestId).toBeTruthy();
    expect(rec?.binding.dispatchId).toBe('');
    return rec!;
  }

  it('取消：结果 CANCELLED，Agent 可能在运行——占用容量、journal 带启动幂等键；查到 dispatch 后停止它并释放', async () => {
    env = await lostStart();
    const e = env;
    e.kiln.heartbeatFn = () => ({ cancelRequested: e.orca.workers.size > 0 });
    const sha = await e.origin.branchSha('master');
    const task = devTask(e.origin, { sha, mode: 'implement' });
    e.kiln.tasks = [task];
    const key = String(task.ref!.attemptId);
    const node = makeNode(e, 1, () => false, 50, false, { idleMs: 20 });
    const ac = new AbortController();
    const running = node.run(ac.signal);
    await waitFor(() => e.kiln.completes.length === 1 && node.attempt(key) === undefined, 10_000, '取消并结束执行');
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.CANCELLED);
    expect(e.kiln.completes[0]?.binding?.startRequestId).toBeTruthy();
    // 不知道 worktree、不能确认停止：不推 checkpoint
    expect(e.kiln.completes[0]?.result?.case).toBeUndefined();
    const rec = (await records(e.journal))[0];
    expect(rec?.stage).toBe('stopping');
    expect(rec?.binding.startRequestId).toBe(worker(e).opts.requestId);
    expect(node.busy).toBe(1);
    const before = e.kiln.acquires.length;
    await waitFor(() => e.kiln.acquires.length >= before + 2, 5_000, '继续上报');
    expect(e.kiln.acquires.slice(before).every((a) => a.standby && a.state?.running === 1)).toBe(true);
    expect(worker(e).state).toBe('running');
    expect(e.orca.stops).toEqual([]);

    // Orca 查到了启动的 dispatch：停止它，删除 journal，释放容量
    settle(e);
    await waitFor(() => node.busy === 0, 5_000, '停止后释放');
    expect(e.orca.stops).toEqual([worker(e).ref.dispatchId]);
    expect(worker(e).state).toBe('stopped');
    await journalCleared(e);
    await waitFor(() => e.kiln.acquires.some((a, i) => i >= before && !a.standby), 5_000, '恢复领取');
    ac.abort();
    await running;
    expect(e.orca.starts).toHaveLength(1);
  });

  for (const c of [
    { trigger: 'cancel' as const, outcome: Outcome.CANCELLED, errorClass: '' },
    { trigger: 'stale' as const, outcome: undefined, errorClass: '' },
    { trigger: 'timeout' as const, outcome: Outcome.INFRA_ERROR, errorClass: ErrorClass.lost },
  ]) {
    it(`${c.trigger}：业务结果照常，journal 记为 stopping 带启动幂等键；连接器重启后恢复跟踪，查到 dispatch 后停止`, async () => {
      env = await lostStart();
      const e = env;
      const at = await stopDuring(e, c.trigger);
      if (c.outcome === undefined) {
        expect(e.kiln.completeCalls).toBe(0);
      } else {
        expect(e.kiln.completes).toHaveLength(1);
        expect(e.kiln.completes[0]?.outcome).toBe(c.outcome);
        expect(e.kiln.completes[0]?.errorClass).toBe(c.errorClass);
      }
      expect(at.unconfirmedStop).toBe(true);
      await stoppingRecord(e, at);
      expect(worker(e).state).toBe('running');
      // 查证被打断不让 Agent 退避
      expect(e.deps.agents.snapshot().find((a) => a.name === 'codex')?.backoffUntil ?? 0).toBeLessThanOrEqual(Date.now());

      // 重启：只有启动幂等键的 stopping 记录照样占用容量
      const node = makeNode(e, 1, () => false, 50, false, { idleMs: 20 });
      const ac = new AbortController();
      const running = node.run(ac.signal);
      await waitFor(() => e.kiln.acquires.length >= 2, 5_000, '上报');
      expect(e.kiln.acquires.every((a) => a.standby && a.state?.running === 1)).toBe(true);
      settle(e);
      await waitFor(() => node.busy === 0, 5_000, '停止后释放');
      expect(e.orca.stops).toEqual([worker(e).ref.dispatchId]);
      expect(worker(e).state).toBe('stopped');
      await journalCleared(e);
      ac.abort();
      await running;
    });
  }

  it('结束时 request-show 已经查到 dispatch：当场停止，不进入 stopping', async () => {
    env = await lostStart();
    const e = env;
    // 取消之后 Orca 才查到：verifyStart 已经因停止退出，记下结论之后（停止时）的查证拿到 dispatch
    e.kiln.heartbeatFn = () => ({ cancelRequested: e.orca.workers.size > 0 });
    const save = e.journal.save.bind(e.journal);
    e.journal.save = (r) => {
      if (r.verdict) settle(e);
      return save(r);
    };
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    e.orca.tool = (_id, cmd, args) => at.tool(cmd, args);
    const ac = new AbortController();
    const pump = e.deps.mailbox.pump(ac.signal);
    await at.run();
    ac.abort();
    await pump;
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.CANCELLED);
    expect(e.orca.stops).toEqual([worker(e).ref.dispatchId]);
    expect(at.unconfirmedStop).toBe(false);
    expect(await records(e.journal)).toEqual([]);
  });

  it('跟踪中 request-show 报告 failed：Orca 没有启动，结束跟踪、不停止任何东西', async () => {
    env = await lostStart();
    const e = env;
    const at = await stopDuring(e, 'cancel');
    await stoppingRecord(e, at);
    e.orca.requestShowFn = () => ({ state: 'failed', error: 'agent 不可用' });
    const node = makeNode(e, 1, () => false, 50, false, { idleMs: 20 });
    const ac = new AbortController();
    const running = node.run(ac.signal);
    await waitFor(async () => (await records(e.journal)).every((r) => r.stage !== 'stopping'), 5_000, '结束跟踪');
    await waitFor(() => e.kiln.acquires.some((a) => !a.standby), 5_000, '恢复领取');
    ac.abort();
    await running;
    expect(e.orca.stops).toEqual([]);
    // 只剩回收工作区（不占容量）：替身 Orca 中这个工作区里实际还有没有释放的 worker，回收一直等它，不删除
    expect((await records(e.journal)).map((r) => r.stage)).toEqual(['reclaiming']);
    expect(e.orca.worktreeRemoves).toEqual([]);
  });

  /** clockNode 用可控的时钟从 journal 恢复；tick 推进时钟、等一次再停（request-show）和之后的一次上报。 */
  async function clockNode(e: Env) {
    let clock = Date.now();
    e.deps.now = () => clock;
    const node = makeNode(e, 1, () => false, 50, false, { idleMs: 20 });
    const ac = new AbortController();
    const running = node.run(ac.signal);
    await waitFor(() => e.orca.requestShows.length > 0 && e.kiln.acquires.length > 0, 5_000, '恢复');
    const restops = () => e.orca.requestShows.length + e.orca.stops.length;
    const tick = async (ms: number) => {
      const before = restops();
      clock += ms;
      await waitFor(() => restops() > before, 5_000, '再查证或再停');
      const acquires = e.kiln.acquires.length;
      await waitFor(() => e.kiln.acquires.length > acquires, 5_000, '上报');
    };
    return { node, tick, advance: (ms: number) => (clock += ms), stop: async () => (ac.abort(), await running) };
  }

  it('跟踪中 absent：查证窗口内仍占用容量，持续超过窗口后结束跟踪；查询失败不计入窗口', async () => {
    env = await lostStart();
    const e = env;
    const at = await stopDuring(e, 'cancel');
    await stoppingRecord(e, at);
    const verify = e.deps.timing.startVerifyMs;
    let mode: 'error' | 'absent' = 'error';
    e.orca.requestShowFn = () => (mode === 'error' ? new Error('连不上 Orca') : { state: 'absent' });
    const n = await clockNode(e);
    // 查询失败持续超过窗口：不能断定，继续占用
    await n.tick(verify);
    await n.tick(verify);
    expect(n.node.busy).toBe(1);
    // absent 从第一次查到时算起；窗口内仍占用
    mode = 'absent';
    await n.tick(100);
    await n.tick(verify - 200);
    expect(n.node.busy).toBe(1);
    expect((await records(e.journal))[0]?.stage).toBe('stopping');
    // 中间一次查询失败：窗口重新算起
    mode = 'error';
    await n.tick(150);
    mode = 'absent';
    await n.tick(100);
    await n.tick(verify - 200);
    expect(n.node.busy).toBe(1);
    await n.tick(200);
    await waitFor(() => n.node.busy === 0, 5_000, '超过窗口后释放');
    expect(e.orca.stops).toEqual([]);
    // 只剩回收工作区（不占容量）：替身 Orca 中这个工作区里实际还有没有释放的 worker，回收一直等它，不删除
    expect((await records(e.journal)).map((r) => r.stage)).toEqual(['reclaiming']);
    expect(e.orca.worktreeRemoves).toEqual([]);
    await n.stop();
  });

  it('跟踪中先 absent、后来查到 dispatch：照常停止；查到的 dispatch 写回 journal', async () => {
    env = await lostStart();
    const e = env;
    const at = await stopDuring(e, 'cancel');
    await stoppingRecord(e, at);
    let absent = true;
    e.orca.requestShowFn = () => (absent ? { state: 'absent' } : undefined);
    e.orca.stopConfirms = false;
    const n = await clockNode(e);
    await n.tick(100);
    absent = false;
    await n.tick(100);
    await waitFor(() => e.orca.stops.length > 0, 5_000, '停止');
    expect(n.node.busy).toBe(1);
    // 不能确认停止：dispatch 已经写回 journal（重启后直接停它）
    await waitFor(async () => (await records(e.journal))[0]?.binding.dispatchId === worker(e).ref.dispatchId, 5_000, '写回');
    e.orca.stopConfirms = true;
    await n.tick(100);
    await waitFor(() => n.node.busy === 0, 5_000, '停止后释放');
    expect(new Set(e.orca.stops)).toEqual(new Set([worker(e).ref.dispatchId]));
    await journalCleared(e, () => n.advance(100));
    await n.stop();
  });
});

describe('节点被吊销', () => {
  /** 写出改动后一直运行到被停止的假 Agent（被取消时会有 checkpoint 可推）。 */
  const working = async (ctx: { worktree: string; stopped: AbortSignal }) => {
    await writeFile(path.join(ctx.worktree, 'wip.txt'), '一半\n');
    await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
  };
  const worker = (e: Env) => [...e.orca.workers.values()][0]!;
  const revoked = () => new RevokedError();
  const runningStage = async (e: Env) => (await records(e.journal))[0]?.stage === 'running';

  /** offline 让 FakeKiln 的每个 RPC 都记下调用并失败：以吊销状态启动的节点不应联系 Server。 */
  function offline(e: Env): string[] {
    const calls: string[] = [];
    const fail = (m: string) => () => {
      calls.push(m);
      return Promise.reject(new Error('不应联系 Server'));
    };
    Object.assign(e.kiln, Object.fromEntries(['acquire', 'heartbeat', 'appendLog', 'complete', 'offerDevCandidates', 'claimDevIssues', 'validateReport', 'getVerification'].map((m) => [m, fail(m)])));
    return calls;
  }

  /** startWorking 领取一个开发任务（带测试服务）并等到 Agent 运行、写出改动。 */
  async function startWorking(e: Env, services: FakeServices, extra: Partial<NodeOptions> = {}) {
    const sha = await e.origin.branchSha('master');
    const task = devTask(e.origin, { sha, mode: 'implement', services: ['pg'] });
    e.kiln.tasks = [task];
    const node = makeNode(e, 1, () => false, 50, true, { idleMs: 20, ...extra });
    const ac = new AbortController();
    const running = node.run(ac.signal);
    const result = running.then(
      () => undefined,
      (err: unknown) => err,
    );
    await waitFor(() => runningStage(e), 10_000, 'Agent 运行');
    await waitFor(() => services.projects.size === 1, 5_000, '测试服务');
    return { task, node, ac, result, id: String(task.ref!.attemptId) };
  }

  /** expectRevokedClean 检查吊销后的结局：Agent 被停止、没有推送和提交、journal 与测试服务清理。 */
  async function expectRevokedClean(e: Env, services: FakeServices, id: string) {
    expect(e.orca.stops).toContain(worker(e).ref.dispatchId);
    expect(worker(e).state).toBe('stopped');
    // 停止之后释放了终端
    expect(worker(e).released).toBe(true);
    expect(e.kiln.completeCalls).toBe(0);
    expect(await e.origin.branchSha(`kiln/attempt-${id}`)).toBe('');
    expect(await records(e.journal)).toEqual([]);
    expect(services.projects.size).toBe(0);
  }

  it('Agent 运行中 Acquire 返回吊销：停止 Agent，不推 checkpoint、不提交，清理 journal 和测试服务，run 以 RevokedError 结束', async () => {
    const services = new FakeServices();
    env = await makeEnv(working, { services });
    const e = env;
    let persisted = 0;
    const w = await startWorking(e, services, { onRevoked: () => Promise.resolve(void persisted++) });
    e.kiln.acquire = () => Promise.reject(revoked());
    expect(await w.result).toBeInstanceOf(RevokedError);
    await expectRevokedClean(e, services, w.id);
    expect(persisted).toBe(1);
    expect(w.node.busy).toBe(0);
  });

  it('心跳先收到吊销：不等下一次 Acquire，立即停止领取和 Agent；之后的工具调用被拒绝', async () => {
    const services = new FakeServices();
    let toolAfter: { code: number; output: string } | undefined;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.worktree, 'wip.txt'), '一半\n');
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
      toolAfter = await ctx.tool('info');
    }, { services });
    const e = env;
    const w = await startWorking(e, services);
    let acquiresAtRevoke = -1;
    e.kiln.heartbeatFn = () => {
      if (acquiresAtRevoke < 0) acquiresAtRevoke = e.kiln.acquires.length;
      return revoked();
    };
    expect(await w.result).toBeInstanceOf(RevokedError);
    await expectRevokedClean(e, services, w.id);
    // 得知吊销之后没有再领取
    expect(e.kiln.acquires.length).toBe(acquiresAtRevoke);
    await waitFor(() => toolAfter !== undefined, 5_000, 'Agent 的工具调用');
    expect(toolAfter?.code).toBe(1);
  });

  it('不能确认停止：进程不退出、不再联系 Server，journal 保留 stopping 并继续再停；确认后才以 RevokedError 结束', async () => {
    const services = new FakeServices();
    env = await makeEnv(working, { services });
    const e = env;
    e.orca.stopConfirms = false;
    const w = await startWorking(e, services);
    e.kiln.acquire = () => Promise.reject(revoked());
    await waitFor(async () => (await records(e.journal))[0]?.stage === 'stopping', 10_000, '记为 stopping');
    const contacts = offline(e);
    await waitFor(() => e.orca.stops.length >= 3, 5_000, '继续再停');
    let settled = false;
    void w.result.then(() => (settled = true));
    await new Promise((r) => setTimeout(r, 100));
    expect(settled).toBe(false);
    expect(new Set(e.orca.stops)).toEqual(new Set([worker(e).ref.dispatchId]));
    e.orca.stopConfirms = true;
    expect(await w.result).toBeInstanceOf(RevokedError);
    expect(contacts).toEqual([]);
    await expectRevokedClean(e, services, w.id);
  });

  it('清理中收到退出信号：stopping 留在 journal；以吊销状态重启后不联系 Server，继续停止，清完以 RevokedError 结束', async () => {
    env = await makeEnv(working);
    const e = env;
    e.orca.stopConfirms = false;
    const sha = await e.origin.branchSha('master');
    e.kiln.tasks = [devTask(e.origin, { sha, mode: 'implement' })];
    const ac = new AbortController();
    const result = makeNode(e, 1, () => false, 50, false, { idleMs: 20 }).run(ac.signal).catch((err: unknown) => err);
    await waitFor(() => runningStage(e), 10_000, 'Agent 运行');
    e.kiln.acquire = () => Promise.reject(revoked());
    await waitFor(async () => (await records(e.journal))[0]?.stage === 'stopping', 10_000, '记为 stopping');
    ac.abort();
    expect(await result).toBeInstanceOf(RevokedError);
    expect((await records(e.journal))[0]?.stage).toBe('stopping');
    expect(worker(e).state).toBe('running');

    const contacts = offline(e);
    const stopsBefore = e.orca.stops.length;
    const again = makeNode(e, 1, () => false, 50, false, { revoked: true }).run(new AbortController().signal).catch((err: unknown) => err);
    await waitFor(() => e.orca.stops.length >= stopsBefore + 2, 5_000, '重启后继续再停');
    e.orca.stopConfirms = true;
    expect(await again).toBeInstanceOf(RevokedError);
    expect(contacts).toEqual([]);
    expect(worker(e).state).toBe('stopped');
    expect(await records(e.journal)).toEqual([]);
  });

  it('启动结果未知时被吊销：按启动查证的规则继续查证，查到 dispatch 后停止它，然后才结束', async () => {
    env = await makeEnv(working);
    const e = env;
    e.orca.startFault = 'lost-after';
    let pending = true;
    e.orca.requestShowFn = () => (pending ? { state: 'pending' } : undefined);
    const sha = await e.origin.branchSha('master');
    e.kiln.tasks = [devTask(e.origin, { sha, mode: 'implement' })];
    const result = makeNode(e, 1, () => false, 50, false, { idleMs: 20 }).run(new AbortController().signal).catch((err: unknown) => err);
    await waitFor(() => e.orca.workers.size === 1, 10_000, 'Agent 启动（响应丢失）');
    e.kiln.acquire = () => Promise.reject(revoked());
    await waitFor(async () => (await records(e.journal))[0]?.stage === 'stopping', 10_000, '记为 stopping');
    const rec = (await records(e.journal))[0];
    expect(rec?.binding.startRequestId).toBe(worker(e).opts.requestId);
    expect(rec?.binding.dispatchId).toBe('');
    const shows = e.orca.requestShows.length;
    await waitFor(() => e.orca.requestShows.length >= shows + 2, 5_000, '继续查证');
    expect(e.orca.stops).toEqual([]);
    pending = false;
    expect(await result).toBeInstanceOf(RevokedError);
    expect(e.orca.stops).toEqual([worker(e).ref.dispatchId]);
    expect(worker(e).state).toBe('stopped');
    expect(e.orca.starts).toHaveLength(1);
    expect(e.kiln.completeCalls).toBe(0);
    expect(await records(e.journal)).toEqual([]);
  });

  it('重启恢复时从 Server 得知被吊销：停止恢复的 Agent，不提交', async () => {
    const services = new FakeServices();
    env = await makeEnv(working, { services });
    const e = env;
    const w = await startWorking(e, services);
    w.ac.abort();
    expect(await w.result).toBeUndefined();
    expect(e.orca.stops).toEqual([]);
    expect(await runningStage(e)).toBe(true);

    e.kiln.acquire = () => Promise.reject(revoked());
    e.kiln.heartbeatFn = revoked;
    let persisted = 0;
    const again = makeNode(e, 1, () => false, 50, true, { onRevoked: () => Promise.resolve(void persisted++) }).run(new AbortController().signal);
    await expect(again).rejects.toBeInstanceOf(RevokedError);
    await expectRevokedClean(e, services, w.id);
    expect(persisted).toBe(1);
  });

  it('以吊销状态启动：journal 中运行中、结束中（已记下请求）的任务都按失效停止，不联系 Server', async () => {
    const services = new FakeServices();
    env = await makeEnv(working, { services });
    const e = env;
    const w = await startWorking(e, services);
    w.ac.abort();
    await w.result;
    // 另一条结束中、已经记下要提交的请求、Agent 还要停止的记录（取消后提交没成功）
    const running = (await records(e.journal))[0]!;
    const finishing: JournalRecord = {
      ...running,
      attemptId: String(BigInt(running.attemptId) + 1000n),
      stage: 'finishing',
      services: undefined,
      binding: { ...running.binding, dispatchId: 'ctx_finishing' },
      verdict: { outcome: Outcome.CANCELLED, errorClass: '', message: '已取消', push: false, checkpoint: false, stopAgent: true },
      complete: { outcome: Outcome.CANCELLED, errorClass: '', message: '已取消', agentVersion: '', binding: { ...running.binding, dispatchId: 'ctx_finishing' } },
    };
    const spec = fromBinary(AITaskSchema, Buffer.from(running.spec, 'base64'));
    spec.ref!.attemptId = BigInt(finishing.attemptId);
    finishing.spec = Buffer.from(toBinary(AITaskSchema, spec)).toString('base64');
    await e.journal.save(finishing);
    // 还在准备的记录：不再准备仓库
    const preparing: JournalRecord = { ...running, attemptId: String(BigInt(running.attemptId) + 2000n), stage: 'prepare', services: undefined, binding: emptyBinding() };
    spec.ref!.attemptId = BigInt(preparing.attemptId);
    preparing.spec = Buffer.from(toBinary(AITaskSchema, spec)).toString('base64');
    await e.journal.save(preparing);
    let prepares = 0;
    const prepareClone = e.deps.git.prepareClone.bind(e.deps.git);
    e.deps.git.prepareClone = (...a) => {
      prepares++;
      return prepareClone(...a);
    };

    const contacts = offline(e);
    const again = makeNode(e, 3, () => false, 50, true, { revoked: true }).run(new AbortController().signal);
    await expect(again).rejects.toBeInstanceOf(RevokedError);
    expect(contacts).toEqual([]);
    expect(new Set(e.orca.stops)).toEqual(new Set([worker(e).ref.dispatchId, 'ctx_finishing']));
    expect(prepares).toBe(0);
    expect(worker(e).state).toBe('stopped');
    expect(await e.origin.branchSha(`kiln/attempt-${w.id}`)).toBe('');
    expect(await records(e.journal)).toEqual([]);
    expect(services.projects.size).toBe(0);
  });
});


describe('journal 读不了', () => {
  /** failingList 让 journal.list 从第 from 次调用起失败（EACCES）。 */
  function failingList(e: Env, from: number): () => number {
    let calls = 0;
    const list = e.journal.list.bind(e.journal);
    e.journal.list = () => {
      calls++;
      if (calls >= from) return Promise.reject(Object.assign(new Error('EACCES: permission denied, scandir journal'), { code: 'EACCES' }));
      return list();
    };
    return () => calls;
  }

  it('启动恢复时读不了 journal：节点退出（不当成没有在途任务、不领取）', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    failingList(e, 1);
    await expect(makeNode(e, 1).run(new AbortController().signal)).rejects.toThrow(/EACCES/);
    expect(e.kiln.acquires).toEqual([]);
  });

  it('周期回收测试服务时读不了 journal：记日志、不回收，主循环继续', async () => {
    const services = new FakeServices();
    env = await makeEnv(() => Promise.resolve(), { services });
    const e = env;
    const warns: string[] = [];
    e.deps.log = { ...e.deps.log, warn: (m: string) => void warns.push(m) };
    // 恢复时第一次读取成功，之后（恢复后的回收、周期回收）都失败
    const calls = failingList(e, 2);
    const ac = new AbortController();
    const running = makeNode(e, 1, () => false, undefined, true, { idleMs: 20 }).run(ac.signal);
    await waitFor(() => calls() >= 3 && e.kiln.acquires.length >= 3, 5_000, '周期回收之后继续领取');
    ac.abort();
    await running;
    expect(services.cleanups).toEqual([]);
    expect(warns.filter((w) => w.includes('回收残留的测试服务失败')).length).toBeGreaterThanOrEqual(2);
  });
});

describe('journal 中有读不了的记录', () => {
  /** asRoot：root 下 chmod 000 挡不住读取（CI 的 Linux 镜像以 root 运行），只测损坏的内容。 */
  const asRoot = process.getuid?.() === 0;
  type Breakage = 'chmod' | 'corrupt';
  const breakages: Breakage[] = asRoot ? ['corrupt'] : ['chmod', 'corrupt'];

  /** breakRecord 让 attempt 的记录读不了，返回恢复它的函数。 */
  async function breakRecord(e: Env, id: string, how: Breakage): Promise<() => Promise<void>> {
    const file = e.journal.fileOf(id);
    const original = await readFile(file, 'utf8');
    if (how === 'chmod') {
      await chmod(file, 0o000);
      return () => chmod(file, 0o600);
    }
    await writeFile(file, original.slice(0, original.length >> 1));
    return () => writeFile(file, original);
  }

  /** capture 记下 error 级的日志（消息和字段）。 */
  function capture(e: Env): string[] {
    const errors: string[] = [];
    e.deps.log = { ...nopLogger, error: (m, f) => void errors.push(`${m} ${JSON.stringify(f ?? {})}`) };
    return errors;
  }

  const report = JSON.stringify({ version: 1, verdict: 'approve', summary: 'ok', findings: [] });

  /** gated 是等放行后才写报告、结束的审计 Agent。 */
  function gated(open: () => boolean) {
    return async (ctx: { taskDir: string; done(): void }) => {
      await waitFor(open, 30_000, '放行');
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    };
  }

  /** startAndExit 领取任务、等它们都运行起来，然后连接器退出（Agent 继续运行）。 */
  async function startAndExit(e: Env, services: FakeServices | undefined, tasks: ReturnType<typeof reviewTask>[]): Promise<void> {
    e.kiln.tasks = [...tasks];
    const ac = new AbortController();
    const running = makeNode(e, tasks.length, () => false, 50, true, { idleMs: 20 }).run(ac.signal);
    await waitFor(async () => (await records(e.journal)).filter((r) => r.stage === 'running').length === tasks.length, 10_000, 'Agent 运行');
    ac.abort();
    await running;
    // 只看之后的节点的上报和回收
    e.kiln.acquires.length = 0;
    services?.cleanups.splice(0);
  }

  it.each(breakages)('Agent 运行中连接器退出、记录读不了（%s）：不回收测试服务、占用容量、不领取，原因在日志和上报中；恢复后接着跟踪到结束', async (how) => {
    const services = new FakeServices();
    let finish = false;
    env = await makeEnv(gated(() => finish), { services });
    const e = env;
    const sha = await e.origin.branchSha('master');
    const task = reviewTask(e.origin, { sha, baseSha: sha, services: ['pg'] });
    const id = String(task.ref!.attemptId);
    await startAndExit(e, services, [task]);
    expect(services.projects.size).toBe(1);
    const restore = await breakRecord(e, id, how);

    const errors = capture(e);
    e.kiln.tasks = [reviewTask(e.origin, { sha, baseSha: sha })];
    const ac = new AbortController();
    const node = makeNode(e, 2, () => false, 50, true, { idleMs: 20, statusIntervalMs: 50 });
    const running = node.run(ac.signal);
    await waitFor(() => e.kiln.acquires.length >= 5, 5_000, '上报');
    // 测试服务没有被回收、没有停止 Agent、按占用容量计、只上报不领取
    expect(services.projects.size).toBe(1);
    expect(services.cleanups).toEqual([]);
    expect(e.orca.stops).toEqual([]);
    expect([...e.orca.workers.values()][0]!.state).toBe('running');
    expect(node.busy).toBe(1);
    expect(node.attempt(id)).toBeUndefined();
    for (const a of e.kiln.acquires) {
      expect(a.standby).toBe(true);
      expect(a.state?.running).toBe(1);
      expect(a.state?.orca?.ready).toBe(false);
      expect(a.state?.orca?.reason).toContain(e.journal.fileOf(id));
    }
    expect(e.kiln.tasks).toHaveLength(1);
    // 每条读不了的记录各记一条 error：文件、原因、处理方法
    expect(errors.some((m) => m.startsWith('journal 记录读不了') && m.includes(e.journal.fileOf(id)) && m.includes('dispatchId'))).toBe(true);
    // 文件没有被删除、改名
    expect(existsSync(e.journal.fileOf(id))).toBe(true);

    await restore();
    await waitFor(() => node.attempt(id) !== undefined, 5_000, '恢复这个 attempt');
    finish = true;
    await waitFor(() => e.kiln.completes.some((c) => String(c.ref?.attemptId) === id), 10_000, '跟踪到结束');
    // 解除后恢复领取，另一个任务照常执行
    await waitFor(() => e.kiln.completes.length === 2, 10_000, '领取新任务');
    expect(e.orca.starts).toHaveLength(2);
    await waitFor(() => services.projects.size === 0, 5_000, '测试服务删除');
    ac.abort();
    await running;
  });

  it('一条可读、一条损坏：可读的照常恢复并结束；损坏的一直阻止领取和回收；人删掉文件后解除，回收它的测试服务', async () => {
    const services = new FakeServices();
    let finish = false;
    env = await makeEnv(gated(() => finish), { services });
    const e = env;
    const sha = await e.origin.branchSha('master');
    const [good, bad] = [1, 2].map(() => reviewTask(e.origin, { sha, baseSha: sha, services: ['pg'] }));
    const goodId = String(good!.ref!.attemptId);
    const badId = String(bad!.ref!.attemptId);
    await startAndExit(e, services, [good!, bad!]);
    await breakRecord(e, badId, 'corrupt');

    e.kiln.tasks = [reviewTask(e.origin, { sha, baseSha: sha })];
    const ac = new AbortController();
    const node = makeNode(e, 3, () => false, 50, true, { idleMs: 20, statusIntervalMs: 50 });
    const running = node.run(ac.signal);
    await waitFor(() => node.attempt(goodId) !== undefined, 5_000, '恢复可读的记录');
    finish = true;
    await waitFor(() => e.kiln.completes.some((c) => String(c.ref?.attemptId) === goodId), 10_000, '可读的记录结束');
    await waitFor(() => !services.projects.has(projectName(services.owner, goodId)), 5_000, '可读的记录的测试服务删除');
    // 删除测试服务之后还要回收审计的工作区、删除 journal：等它真正结束，下面的容量只剩损坏的记录
    await waitFor(() => node.attempt(goodId) === undefined, 5_000, '可读的记录结束执行');
    const before = e.kiln.acquires.length;
    await waitFor(() => e.kiln.acquires.length >= before + 5, 5_000, '上报');
    // 损坏的记录：仍占用容量（有 3 份容量也不领取）、不回收它的测试服务、Agent 不停止
    expect(e.kiln.acquires.every((a) => a.standby)).toBe(true);
    expect(node.busy).toBe(1);
    expect(services.cleanups).toEqual([]);
    expect(services.projects).toEqual(new Set([projectName(services.owner, badId)]));
    expect(e.kiln.completes).toHaveLength(1);
    expect(e.orca.stops).toEqual([]);

    // 人在 Orca 中停止了 Agent，删除文件：解除，恢复领取，回收残留的测试服务
    await rm(e.journal.fileOf(badId));
    await waitFor(() => e.kiln.acquires.some((a) => !a.standby), 5_000, '恢复领取');
    await waitFor(() => services.projects.size === 0 && e.kiln.completes.length === 2, 10_000, '回收残留的测试服务、执行新任务');
    await waitFor(() => node.busy === 0, 5_000, '释放容量');
    ac.abort();
    await running;
  });

  it('任务还原不出来（spec 与记录不一致）：不删除记录，阻止领取；修好后按恢复流程接着执行', async () => {
    let finish = false;
    env = await makeEnv(gated(() => finish));
    const e = env;
    const sha = await e.origin.branchSha('master');
    const task = reviewTask(e.origin, { sha, baseSha: sha });
    const id = String(task.ref!.attemptId);
    await startAndExit(e, undefined, [task]);
    const rec = (await records(e.journal))[0]!;
    const wrong = fromBinary(AITaskSchema, Buffer.from(rec.spec, 'base64'));
    wrong.ref!.attemptId += 1n;
    await e.journal.save({ ...rec, spec: Buffer.from(toBinary(AITaskSchema, wrong)).toString('base64') });

    const ac = new AbortController();
    const node = makeNode(e, 1, () => false, 50, false, { idleMs: 20, statusIntervalMs: 50 });
    const running = node.run(ac.signal);
    await waitFor(() => e.kiln.acquires.length >= 3, 5_000, '上报');
    expect(e.kiln.acquires.every((a) => a.standby)).toBe(true);
    expect(e.kiln.acquires.at(-1)?.state?.orca?.reason).toContain('与记录不一致');
    expect(node.busy).toBe(1);
    expect(existsSync(e.journal.fileOf(id))).toBe(true);

    await e.journal.save(rec);
    await waitFor(() => node.attempt(id) !== undefined, 5_000, '恢复');
    finish = true;
    await waitFor(() => e.kiln.completes.length === 1, 10_000, '结束');
    expect(e.orca.starts).toHaveLength(1);
    ac.abort();
    await running;
  });

  it('吊销后的清理中有读不了的记录：不当成清理完（不结束 run），修好后按失效停止它的 Agent 才以 RevokedError 结束', async () => {
    env = await makeEnv(async (ctx) => {
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    const task = devTask(e.origin, { sha, mode: 'implement' });
    const id = String(task.ref!.attemptId);
    e.kiln.tasks = [task];
    const ac = new AbortController();
    const first = makeNode(e, 1, () => false, 50, false, { idleMs: 20 }).run(ac.signal);
    await waitFor(async () => (await records(e.journal))[0]?.stage === 'running', 10_000, 'Agent 运行');
    ac.abort();
    await first;
    const restore = await breakRecord(e, id, 'corrupt');

    const errors = capture(e);
    const result = makeNode(e, 1, () => false, 50, false, { revoked: true }).run(new AbortController().signal).catch((err: unknown) => err);
    let settled = false;
    void result.then(() => (settled = true));
    await new Promise((r) => setTimeout(r, 400));
    expect(settled).toBe(false);
    expect(errors.some((m) => m.includes('处理之前不结束清理'))).toBe(true);
    expect([...e.orca.workers.values()][0]!.state).toBe('running');

    await restore();
    expect(await result).toBeInstanceOf(RevokedError);
    expect([...e.orca.workers.values()][0]!.state).toBe('stopped');
    expect(e.kiln.completeCalls).toBe(0);
    expect(await records(e.journal)).toEqual([]);
  });

  it('周期清理：读不了的记录的任务目录保留、邮箱落盘的消息不清理；删掉文件后立即补做清理', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const home = path.dirname(e.journal.dir);
    const old = new Date(Date.now() - 13 * 60 * 60_000);
    const aged = async (p: string, content?: string) => {
      if (content === undefined) await mkdir(p, { recursive: true });
      else {
        await mkdir(path.dirname(p), { recursive: true });
        await writeFile(p, content);
      }
      await utimes(p, old, old);
    };
    await aged(e.journal.fileOf('125'), '{');
    await aged(path.join(e.deps.tasksDir, '125'));
    await aged(path.join(e.deps.tasksDir, '999'));
    await aged(path.join(home, 'inbox', 'ctx_9.json'), '{}');
    const ac = new AbortController();
    const node = makeNode(e, 1, () => false, 50, false, { idleMs: 20, statusIntervalMs: 50 });
    const running = node.run(ac.signal);
    await waitFor(() => !existsSync(path.join(e.deps.tasksDir, '999')), 5_000, '周期清理');
    await waitFor(() => e.kiln.acquires.length >= 3, 5_000, '上报');
    expect(existsSync(path.join(e.deps.tasksDir, '125'))).toBe(true);
    expect(existsSync(path.join(home, 'inbox', 'ctx_9.json'))).toBe(true);
    await rm(e.journal.fileOf('125'));
    await waitFor(() => !existsSync(path.join(e.deps.tasksDir, '125')) && !existsSync(path.join(home, 'inbox', 'ctx_9.json')), 5_000, '解除后补做清理');
    ac.abort();
    await running;
  });

  it('吊销后的清理中，人删掉了读不了的记录：解除后以 RevokedError 结束', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    await mkdir(e.journal.dir, { recursive: true });
    await writeFile(e.journal.fileOf('123'), '{');
    const result = makeNode(e, 1, () => false, 50, false, { revoked: true }).run(new AbortController().signal).catch((err: unknown) => err);
    let settled = false;
    void result.then(() => (settled = true));
    await new Promise((r) => setTimeout(r, 300));
    expect(settled).toBe(false);
    await rm(e.journal.fileOf('123'));
    expect(await result).toBeInstanceOf(RevokedError);
  });

  it('吊销后的清理中收到退出信号：照常退出（下次以吊销状态启动时继续），读不了的文件保留', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    await mkdir(e.journal.dir, { recursive: true });
    await writeFile(e.journal.fileOf('124'), '{');
    const ac = new AbortController();
    const result = makeNode(e, 1, () => false, 50, false, { revoked: true }).run(ac.signal).catch((err: unknown) => err);
    await new Promise((r) => setTimeout(r, 200));
    ac.abort();
    expect(await result).toBeInstanceOf(RevokedError);
    expect(existsSync(e.journal.fileOf('124'))).toBe(true);
  });
});
