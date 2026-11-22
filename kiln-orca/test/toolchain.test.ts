import { chmod, mkdir, mkdtemp, realpath, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { afterEach, describe, expect, it } from 'vitest';
import { Outcome } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { Attempt } from '../src/core/attempt.js';
import { ErrorClass } from '../src/core/errors.js';
import { parseRecord, type JournalRecord } from '../src/core/journal.js';
import { Node } from '../src/core/node.js';
import { applyToolchainEnv, parseEnsure, Toolchain } from '../src/core/toolchain.js';
import { kilnRunnerLocator, runnerPackage } from '../src/host/toolchain.js';
import { type AgentCtx, devTask, type Env, makeEnv, records, reviewTask, tool, waitFor } from './harness.js';

// 受管理的工具链（PLAN #33.12）：连接器调用 kiln-runner toolchain（这里是 harness 的 FakeRunner）准备任务声明的工具，
// 把环境变量记进 journal、交给 task exec；节点上报能否准备，周期回收时不删进行中的任务用到的。

let env: Env | undefined;
let tmp = '';
afterEach(async () => {
  await env?.cleanup();
  env = undefined;
  if (tmp) await rm(tmp, { recursive: true, force: true });
  tmp = '';
});

const report = JSON.stringify({ version: 1, verdict: 'approve', summary: 'ok', findings: [] });
const goTool = tool('go', '1.27.3', 'a'.repeat(64));
const nodeTool = tool('node', '24.11.0', 'b'.repeat(64));

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

describe('applyToolchainEnv', () => {
  it('set 覆盖原值，prepend 的路径按顺序放在原值之前；原值为空时只有这些路径', () => {
    const out = applyToolchainEnv({ PATH: '/usr/bin', GOTOOLCHAIN: 'auto', HOME: '/h', GONE: undefined }, { set: { GOTOOLCHAIN: 'local', GOROOT: '/t/go' }, prepend: { PATH: ['/t/go/bin', '/t/node/bin'], NODE_PATH: ['/t/n'] } });
    expect(out).toEqual({ PATH: '/t/go/bin:/t/node/bin:/usr/bin', GOTOOLCHAIN: 'local', GOROOT: '/t/go', HOME: '/h', NODE_PATH: '/t/n' });
  });
});

describe('parseEnsure', () => {
  const ok = { format: 1, tools: [{ name: 'go', version: '1.27.3', platform: 'darwin/arm64', digest: goTool.digest, dir: '/t/a', reused: true, millis: 0 }], env: { set: {}, prepend: { PATH: ['/t/a/bin'] } } };
  it('与请求逐个对应时返回目录和环境变量', () => {
    expect(parseEnsure(JSON.stringify(ok), [goTool])).toEqual({ tools: [{ name: 'go', version: '1.27.3', digest: goTool.digest, dir: '/t/a', reused: true }], env: { set: {}, prepend: { PATH: ['/t/a/bin'] } } });
  });
  it.each([
    ['格式版本不同', { ...ok, format: 2 }, '格式版本'],
    ['digest 不同', { ...ok, tools: [{ ...ok.tools[0], digest: nodeTool.digest }] }, '不一致'],
    ['数量不同', { ...ok, tools: [] }, '不一致'],
    ['目录不是绝对路径', { ...ok, tools: [{ ...ok.tools[0], dir: 't/a' }] }, '不一致'],
    ['环境变量格式不对', { ...ok, env: { set: {}, prepend: { PATH: '/t/a/bin' } } }, '环境变量'],
  ])('%s时抛出', (_n, out, msg) => {
    expect(() => parseEnsure(JSON.stringify(out), [goTool])).toThrow(msg);
  });
});

describe('Toolchain', () => {
  it('status：找不到 kiln-runner、旧版本（没有 toolchain 子命令）、格式版本不同时不就绪，并说明原因', async () => {
    env = await makeEnv(() => Promise.resolve());
    const r = env.runner;
    expect(await new Toolchain(r.exec, () => ({ reason: '没有找到 kiln-runner' }), r.root).status()).toMatchObject({ ok: false, reason: '没有找到 kiln-runner' });
    const old = new Toolchain(() => Promise.resolve({ code: 2, stdout: '', stderr: 'kiln-runner：不认识的命令 toolchain\n用法…' }), () => ({ bin: '/x/kiln-runner', reason: '' }), r.root);
    expect((await old.status()).reason).toContain('不支持 toolchain 子命令');
    r.infoFormat = 2;
    const tc = new Toolchain(r.exec, () => ({ bin: '/x/kiln-runner', reason: '' }), r.root);
    expect(await tc.status()).toMatchObject({ ok: false });
    r.infoFormat = 1;
    expect(await tc.status()).toEqual({ ok: true, reason: '', version: 'kiln-runner v0.0.0-test', bin: '/x/kiln-runner' });
  });

  it('gc：有 ensure 在进行时跳过这一轮；之后按 keep 传 --keep', async () => {
    env = await makeEnv(() => Promise.resolve());
    const r = env.runner;
    const tc = env.deps.toolchain;
    r.hang = true;
    const ac = new AbortController();
    const ensuring = tc.ensure([goTool], { signal: ac.signal }).catch((err: unknown) => err);
    await waitFor(() => r.calls.length === 1, 2_000, 'ensure 开始');
    expect(await tc.gc([goTool.digest])).toBeUndefined();
    ac.abort();
    expect(await ensuring).toBeInstanceOf(Error);
    r.hang = false;
    r.installed.add(nodeTool.digest);
    r.installed.add(goTool.digest);
    expect(await tc.gc([goTool.digest, goTool.digest])).toEqual({ removed: 1, removedBytes: 1, failures: [] });
    expect(r.calls.at(-1)?.args).toEqual(['toolchain', 'gc', '--root', r.root, '--keep', goTool.digest]);
    expect([...r.installed]).toEqual([goTool.digest]);
  });
});

describe('任务的工具链', () => {
  it('准备阶段装好任务声明的工具，环境变量记进 journal、经 exec-env 交给 Agent；第二个任务复用', async () => {
    const seen: { env?: string; info?: string; rec?: JournalRecord }[] = [];
    env = await makeEnv(async (ctx: AgentCtx) => {
      const e = env!;
      const rec = (await records(e.journal)).find((x) => x.attemptId === path.basename(ctx.taskDir));
      seen.push({ env: (await ctx.tool('exec-env')).output, info: (await ctx.tool('info')).output, rec });
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    for (let i = 0; i < 2; i++) await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha, tools: [goTool, nodeTool] })));

    expect(env.kiln.completes.map((c) => c.outcome)).toEqual([Outcome.SUCCEEDED, Outcome.SUCCEEDED]);
    const ensures = env.runner.calls.filter((c) => c.args[1] === 'ensure');
    expect(ensures).toHaveLength(2);
    expect(ensures[0]?.args).toEqual(['toolchain', 'ensure', '--root', env.runner.root]);
    expect(JSON.parse(ensures[0]?.input ?? '')).toEqual({ tools: [goTool, nodeTool] });
    const dirs = [goTool, nodeTool].map((t) => path.join(env!.runner.dir(t.digest), 'bin'));
    expect(JSON.parse(seen[0]?.env ?? '')).toEqual({ git: null, env: { set: { GOTOOLCHAIN: 'local' }, prepend: { PATH: dirs } } });
    expect(JSON.parse(seen[0]?.info ?? '').tools).toEqual(['go 1.27.3', 'node 24.11.0']);
    // 启动 Agent 之前已经写进 journal（连接器重启后 exec 仍能用）
    expect(seen[0]?.rec?.toolchain?.env.prepend.PATH).toEqual(dirs);
    expect(seen[0]?.rec?.toolchain?.tools.map((t) => t.reused)).toEqual([false, false]);
    expect(seen[1]?.rec?.toolchain?.tools.map((t) => t.reused)).toEqual([true, true]);
    // 日志中有每个工具是下载还是复用
    expect(env.kiln.log).toContain('go 1.27.3（darwin/arm64）：已安装，直接使用');
  });

  it('没有声明工具的任务不调用 kiln-runner，exec-env 回复 null（按原环境运行）', async () => {
    let reply = '';
    env = await makeEnv(async (ctx) => {
      reply = (await ctx.tool('exec-env')).output;
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, reviewTask(env.origin, { sha, baseSha: sha })));
    expect(env.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
    expect(env.runner.calls).toEqual([]);
    expect(JSON.parse(reply)).toEqual({ env: null, git: null });
  });

  it('准备失败时记为 infra.toolchain，不启动 Agent', async () => {
    env = await makeEnv(() => Promise.reject(new Error('不应启动 Agent')));
    env.runner.fail = { code: 1, stdout: '', stderr: 'go 1.27.3（darwin/arm64）：下载失败：connection refused' };
    const sha = await env.origin.branchSha('master');
    await run(env, new Attempt(env.deps, devTask(env.origin, { sha, mode: 'implement', tools: [goTool] })));
    const c = env.kiln.completes[0];
    expect(c?.outcome).toBe(Outcome.INFRA_ERROR);
    expect(c?.errorClass).toBe(ErrorClass.toolchain);
    expect(c?.message).toContain('connection refused');
    expect(env.orca.starts).toEqual([]);
  });

  it('journal：带工具链的记录能读回；环境变量格式不对的记录读不了', () => {
    const rec = { attemptId: '7', epoch: '1', spec: '', stage: 'running', binding: { runtimeId: '', startRequestId: '', runId: '', taskId: '', dispatchId: '', worktreePath: '', worktreeName: '' }, startedAt: 1 };
    const tc = { tools: [{ name: 'go', version: '1.27.3', digest: goTool.digest, dir: '/t/a', reused: false }], env: { set: { GOTOOLCHAIN: 'local' }, prepend: { PATH: ['/t/a/bin'] } } };
    expect(parseRecord(JSON.stringify({ ...rec, toolchain: tc }), '7').toolchain).toEqual(tc);
    expect(() => parseRecord(JSON.stringify({ ...rec, toolchain: { ...tc, env: { set: {}, prepend: { PATH: '/t/a/bin' } } } }), '7')).toThrow('toolchain.env');
  });
});

describe('节点', () => {
  it('上报工具链是否就绪；周期回收时保留执行中的任务用到的工具链', async () => {
    let release = false;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      await waitFor(() => release, 15_000, '放行');
      ctx.done();
    });
    const e = env;
    const old = tool('go', '1.20.0', 'c'.repeat(64));
    e.runner.installed.add(old.digest);
    const sha = await e.origin.branchSha('master');
    e.kiln.tasks = [reviewTask(e.origin, { sha, baseSha: sha, tools: [goTool] })];
    const node = new Node(e.deps, {
      version: '0.1.0',
      capacity: 1,
      hostId: 'host-1',
      orcaStatus: () => e.orca.status(),
      dockerStatus: () => Promise.resolve({ ok: false, reason: '没有 Docker' }),
      drained: () => false,
      idleMs: 20,
      statusIntervalMs: 20,
    });
    e.orca.tool = (id, cmd, args) => node.attempt(id)?.tool(cmd, args) ?? Promise.resolve({ code: 1, output: '不在执行' });
    const ac = new AbortController();
    const running = node.run(ac.signal);
    await waitFor(() => e.kiln.acquires.length > 0, 5_000, '领取');
    expect(e.kiln.acquires[0]?.state?.toolchain).toEqual({ ready: true, reason: '' });
    // 启动时的周期清理在任务装好工具链之前：之后由下一轮回收（这里直接触发一次与节点相同的 gc）
    await waitFor(() => e.orca.starts.length === 1, 10_000, '启动 Agent');
    expect(e.runner.calls.filter((c) => c.args[1] === 'gc').length).toBeGreaterThan(0);
    const keep = (node as unknown as { reclaimToolchains(): Promise<void> }).reclaimToolchains();
    await keep;
    const gc = e.runner.calls.filter((c) => c.args[1] === 'gc').at(-1);
    expect(gc?.args).toContain(goTool.digest);
    expect(e.runner.installed.has(goTool.digest)).toBe(true);
    expect(e.runner.installed.has(old.digest)).toBe(false);
    release = true;
    await waitFor(() => e.kiln.completes.length === 1, 10_000, '完成');
    ac.abort();
    await running;
  });

  it('kiln-runner 不可用时上报不就绪和原因', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    e.deps.toolchain = new Toolchain(e.runner.exec, () => ({ reason: '没有找到 kiln-runner' }), e.runner.root);
    const node = new Node(e.deps, { version: '0.1.0', capacity: 1, hostId: 'h', orcaStatus: () => e.orca.status(), dockerStatus: () => Promise.resolve({ ok: false, reason: '' }), drained: () => false, idleMs: 20 });
    const ac = new AbortController();
    const running = node.run(ac.signal);
    await waitFor(() => e.kiln.acquires.length > 0, 5_000, '领取');
    expect(e.kiln.acquires[0]?.state?.toolchain).toEqual({ ready: false, reason: '没有找到 kiln-runner' });
    ac.abort();
    await running;
  });
});

describe('kilnRunnerLocator', () => {
  it('从 kiln-orca 的位置解析本平台的 kiln-runner 包；config.json 指定路径时只用它', async () => {
    tmp = await realpath(await mkdtemp(path.join(os.tmpdir(), 'ko-locate-')));
    const pkg = runnerPackage('darwin', 'x64');
    expect(pkg).toBe('@yuebanlaosiji/kilnflow-runner-darwin-amd64');
    const from = pathToFileURL(path.join(tmp, 'kiln-orca', 'dist', 'host', 'toolchain.js')).href;
    // 没有安装这个包
    expect(kilnRunnerLocator('', from, pkg)().reason).toContain('没有找到 kiln-runner');
    const dir = path.join(tmp, 'node_modules', pkg);
    await mkdir(dir, { recursive: true });
    await writeFile(path.join(dir, 'package.json'), JSON.stringify({ name: pkg, version: '0.3.4' }));
    // 包在、二进制不能执行
    await writeFile(path.join(dir, 'kiln-runner'), '#!/bin/sh\n');
    expect(kilnRunnerLocator('', from, pkg)().bin).toBeUndefined();
    await chmod(path.join(dir, 'kiln-runner'), 0o755);
    expect(kilnRunnerLocator('', from, pkg)()).toEqual({ bin: path.join(dir, 'kiln-runner'), reason: '' });
    // 指定了路径：只用它，不存在时不退回到包
    expect(kilnRunnerLocator(path.join(tmp, 'nope'), from, pkg)()).toEqual({ reason: expect.stringContaining('kiln_runner') });
  });
});
