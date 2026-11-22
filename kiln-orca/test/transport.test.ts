import { type ChildProcess, spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { lstat, mkdir, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import type { Server } from 'node:http';
import path from 'node:path';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import { CredentialAccess, Outcome } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { Attempt } from '../src/core/attempt.js';
import { repoKey } from '../src/core/credentials.js';
import { basicAuth } from '../src/core/redact.js';
import { Home } from '../src/host/home.js';
import { serveTools } from '../src/host/socket.js';
import { buildCli, type CliBuild } from './clibuild.js';
import { type GitHTTP, gitHTTP, type GitRequest } from './githttp.js';
import { type AgentCtx, devTask, type Env, git, type LogLine, makeEnv, recordLog, reviewTask, shortTmp, waitFor } from './harness.js';

// Agent 的 git 传输（PLAN #33.13）：真实的 git、编译后的 kiln-orca 命令行（task exec、传输包装、credential helper）和
// 本机 socket 上的连接器（Attempt），git HTTP 服务是 githttp.ts 的替身，Server 是 FakeKiln（按 op_id 记录签发、吊销）。
// 不联网。每个场景结束后检查凭据没有留在 clone 的配置、任务目录、journal 和日志中，个人的凭据 helper 和 askpass 没有被调用。

/**
 * 每个场景启动多个真实进程（git、编译后的命令行、传输包装、credential helper 都是 Node 进程），慢机器或有负载时一步就可能
 * 要几秒。等待都按事件（waitFor 条件），这里只放宽上限：eventMs 是等一个事件的上限，scenarioMs 是一个场景（以及编译命令行）的上限。
 */
const eventMs = 60_000;
const scenarioMs = 300_000;

let build: CliBuild | undefined;
let cli = '';
beforeAll(async () => {
  build = await buildCli();
  cli = build.cli;
}, scenarioMs);
afterAll(async () => {
  await build?.cleanup();
});

const cleanups: Array<() => Promise<void> | void> = [];
afterEach(async () => {
  for (const c of cleanups.splice(0).reverse()) await c();
});

interface Result {
  code: number | null;
  signal: NodeJS.Signals | null;
  stdout: string;
  stderr: string;
}

interface Run {
  child: ChildProcess;
  /** result 在输出读完后返回（stdout、stderr 被孙进程继承时等它们也结束）。 */
  result: Promise<Result>;
  /** exited 在 task exec 进程本身退出时返回。 */
  exited: Promise<void>;
}

/** World 是一个场景：attempt、git HTTP 服务、数据目录（socket）、命令行的环境和记下的日志。 */
interface World {
  e: Env;
  s: GitHTTP;
  at: Attempt;
  /** home 是命令行的数据目录（嵌套的 task exec 用同一个 attempt）。 */
  home: Home;
  lines: LogLine[];
  /** trace 是个人 helper、askpass 被调用时写的文件（不应出现）。 */
  trace: string;
  /** userHome 是命令行的 HOME（netrc 在这里）。 */
  userHome: string;
  /** outputs 是各命令的输出（检查其中没有凭据）。 */
  outputs: string[];
  /** hold 返回 true 的请求被挂起，直到 release 放行（按凭据）。 */
  hold: (r: GitRequest) => boolean;
  held: Array<{ r: GitRequest; go: () => void }>;
  release(pred: (r: GitRequest) => boolean): void;
  /** exec 在 cwd 中运行 kiln-orca task exec -- argv。 */
  exec(cwd: string, argv: string[], o?: { env?: Record<string, string>; input?: string }): Run;
  run(): Promise<void>;
}

/**
 * world 建场景：任务仓库 acme/widgets 和依赖仓库 acme/lib 经 HTTP 提供，服务只接受 FakeKiln 签发、没有吊销、仓库一致的
 * 凭据（写要写凭据）。命令行的全局 git 配置中有个人的凭据 helper，GIT_ASKPASS、SSH_ASKPASS 是替身，都会留下痕迹。
 */
async function world(kind: 'review' | 'develop', agent: (w: World, ctx: AgentCtx) => Promise<void>, o: { preClone?: (clone: string, url: string, bare: string) => Promise<void> } = {}): Promise<World> {
  // Agent、socket 的处理在 w 建好之后才会被调用
  // eslint-disable-next-line prefer-const
  let w!: World;
  const e = await makeEnv((ctx) => agent(w, ctx));
  cleanups.push(() => e.cleanup());
  const lines = recordLog(e);
  const held: World['held'] = [];
  const s = await gitHTTP(
    e.root,
    { 'acme/widgets': e.origin.bare, 'acme/lib': e.origin.bare },
    (r) => {
      const c = e.kiln.credentials.find((x) => x.secret === r.password && !x.revoked);
      return r.user === 'x-access-token' && !!c && `${repoKey(c.repository)}.git` === r.repo.toLowerCase() && (!r.write || c.access === CredentialAccess.WRITE);
    },
    {
      gate: (r) => {
        if (!w.hold(r)) return undefined;
        return new Promise<void>((go) => held.push({ r, go }));
      },
    },
  );
  cleanups.push(() => {
    for (const h of held.splice(0)) h.go();
    return s.close();
  });
  const short = await shortTmp();
  cleanups.push(() => rm(short, { recursive: true, force: true }));
  const home = new Home(path.join(short, 'h'));
  await home.prepareRunDirs({ clone_root: '' });
  const srv: Server = await serveTools(
    home.socketPath,
    (req) => (req.attempt === String(w.at.id) ? w.at.tool(req.cmd, req.args) : Promise.resolve({ code: 1, output: '不是这个 attempt' })),
    (req) => Promise.resolve(req.attempt === String(w.at.id) ? w.at.openTransport(req.url, req.pid) : { code: 1, output: '不是这个 attempt', op: '' }),
  );
  cleanups.push(
    () =>
      new Promise<void>((r) => {
        srv.closeAllConnections();
        srv.close(() => r());
      }),
  );
  const userHome = path.join(e.root, 'user-home');
  await mkdir(userHome, { recursive: true });
  const trace = path.join(e.root, 'personal-called');
  const personal = path.join(e.root, 'personal.gitconfig');
  await writeFile(personal, `[credential]\n\thelper = "!f() { echo helper >> '${trace}'; echo username=evil; echo password=evil; }; f"\n`);
  const askpass = path.join(e.root, 'askpass.sh');
  await writeFile(askpass, `#!/bin/sh\necho askpass >> '${trace}'\necho evil\n`, { mode: 0o755 });
  const baseEnv: Record<string, string> = {
    PATH: process.env.PATH ?? '/usr/bin:/bin',
    HOME: userHome,
    GIT_CONFIG_GLOBAL: personal,
    GIT_CONFIG_NOSYSTEM: '1',
    GIT_ASKPASS: askpass,
    SSH_ASKPASS: askpass,
    LC_ALL: 'C',
  };
  if (process.env.TMPDIR) baseEnv.TMPDIR = process.env.TMPDIR;

  const sha = await e.origin.branchSha('master');
  const task = kind === 'review' ? reviewTask(e.origin, { sha, baseSha: sha }) : devTask(e.origin, { sha, mode: 'implement' });
  task.cloneUrl = s.url('acme/widgets');
  task.repositories = ['acme/lib'];
  if (o.preClone) await o.preClone(path.join(e.deps.reposDir, '42', 'widgets'), task.cloneUrl, e.origin.bare);
  const outputs: string[] = [];
  w = {
    e,
    s,
    at: new Attempt(e.deps, task),
    home,
    lines,
    trace,
    userHome,
    outputs,
    hold: () => false,
    held,
    release(pred) {
      for (const h of held.filter((x) => pred(x.r))) {
        held.splice(held.indexOf(h), 1);
        h.go();
      }
    },
    exec(cwd, argv, x = {}) {
      const child = spawn(process.execPath, [cli, 'task', '--home', home.dir, '--attempt', String(w.at.id), 'exec', '--', ...argv], {
        cwd,
        env: { ...baseEnv, ...x.env },
        stdio: [x.input === undefined ? 'ignore' : 'pipe', 'pipe', 'pipe'],
      });
      if (x.input !== undefined) child.stdin?.end(x.input);
      let stdout = '';
      let stderr = '';
      child.stdout?.setEncoding('utf8').on('data', (d: string) => (stdout += d));
      child.stderr?.setEncoding('utf8').on('data', (d: string) => (stderr += d));
      const result = new Promise<Result>((resolve) =>
        child.once('close', (code, signal) => {
          outputs.push(stdout, stderr);
          resolve({ code, signal, stdout, stderr });
        }),
      );
      const exited = new Promise<void>((resolve) => child.once('exit', () => resolve()));
      return { child, result, exited };
    },
    async run() {
      e.orca.tool = (_id, cmd, args) => w.at.tool(cmd, args);
      const ac = new AbortController();
      const pump = e.deps.mailbox.pump(ac.signal);
      try {
        await w.at.run();
      } finally {
        ac.abort();
        await pump;
      }
    },
  };
  return w;
}

/** transportOps 是 Agent 的传输开的操作（按开始的顺序；准备仓库、推送不经过传输包装，不在其中）。 */
function transportOps(w: World): string[] {
  return w.lines.filter((l) => l.msg === '开始 git 传输').map((l) => String(l.fields.op));
}

/** credOf 返回操作 op 签发的凭据。 */
function credOf(w: World, op: string) {
  return w.e.kiln.credentials.find((c) => c.opId === op);
}

/** pidOf 返回操作 op 的传输包装的进程号（连接器的日志中记下的）。 */
function pidOf(w: World, op: string): number {
  return Number(w.lines.find((l) => l.msg === '开始 git 传输' && l.fields.op === op)?.fields.pid);
}

/** filesUnder 列出 dir 下的普通文件（不跟随符号链接：git exec 镜像中的链接指向 git 自己的目录）。 */
async function filesUnder(dir: string): Promise<string[]> {
  if (!existsSync(dir)) return [];
  const out: string[] = [];
  for (const name of await readdir(dir)) {
    const p = path.join(dir, name);
    const st = await lstat(p);
    if (st.isDirectory()) out.push(...(await filesUnder(p)));
    else if (st.isFile() && st.size < 4 << 20) out.push(p);
  }
  return out;
}

/**
 * noTokens 检查签发过的凭据（和它们的 Basic 认证形式）不在 clone 的配置（包括 worktree 的配置）、任务目录、journal、
 * 连接器日志、任务日志和命令的输出中；个人的 helper 和 askpass 没有被调用。
 */
async function noTokens(w: World): Promise<void> {
  const clone = path.join(w.e.deps.reposDir, '42', 'widgets');
  const files = [path.join(clone, '.git', 'config'), ...(await filesUnder(path.join(clone, '.git', 'worktrees'))), ...(await filesUnder(w.at.taskDir)), ...(await filesUnder(path.join(w.e.root, 'home', 'journal')))];
  const texts = await Promise.all(files.filter((f) => existsSync(f)).map((f) => readFile(f, 'utf8')));
  const all = [...texts, w.e.kiln.log, JSON.stringify(w.lines), ...w.outputs].join('\n');
  expect(w.e.kiln.credentials.length).toBeGreaterThan(0);
  for (const c of w.e.kiln.credentials) {
    expect(all).not.toContain(c.secret);
    expect(all).not.toContain(basicAuth('x-access-token', c.secret));
  }
  expect(existsSync(w.trace)).toBe(false);
}

const report = JSON.stringify({ version: 1, verdict: 'approve', summary: 'ok', findings: [] });
const delivered = JSON.stringify({ status: 'delivered', summary: '完成' });

describe('Agent 的 git 传输（task exec + 传输包装）', () => {
  it('V3/V4：从带旧配置的 clone 起，Agent 在原工作区 git fetch origin、merge 上游（含冲突）后交付；本地命令零签发，fetch 只签一枚读凭据、传输结束即吊销', { timeout: scenarioMs }, async () => {
    const checks: Record<string, unknown> = {};
    const w = await world(
      'develop',
      async (w, ctx) => {
        const wt = ctx.worktree;
        const url = w.s.url('acme/widgets');
        checks.fetchURL = await git(wt, 'remote', 'get-url', 'origin');
        checks.pushURL = await git(wt, 'remote', 'get-url', '--push', 'origin');
        // V4：本地命令不启动远程助手，不产生操作、不签发
        const before = w.e.kiln.issues.length;
        const local = await w.exec(wt, [
          '/bin/sh',
          '-c',
          'git status --short && git diff && git log -1 --format=%H && git checkout -q -b side && echo s > side.txt && git add -A && git commit -qm side && git checkout -q - && git merge -q --no-edit side && git log -1 --format=%s',
        ]).result;
        checks.local = local.code;
        checks.localIssues = w.e.kiln.issues.length - before;
        checks.localOps = transportOps(w).length;
        // 本地改动与上游的改动冲突
        await writeFile(path.join(wt, 'conflict.txt'), 'mine\n');
        await git(wt, 'add', '-A');
        await git(wt, 'commit', '-qm', 'mine');
        const upstream = await w.e.origin.commit('conflict.txt', 'upstream\n', 'master');
        const merged = await w.exec(wt, ['/bin/sh', '-c', 'git fetch origin && git merge --no-edit origin/master']).result;
        checks.mergeCode = merged.code;
        checks.mergeOut = merged.stdout + merged.stderr;
        checks.fetched = (await git(wt, 'rev-parse', 'refs/remotes/origin/master')) === upstream;
        // 传输结束（包装退出、连接关闭）后吊销
        const [op] = transportOps(w);
        await waitFor(() => !!op && !!credOf(w, op)?.revoked, eventMs, 'fetch 的凭据吊销');
        checks.fetchCred = credOf(w, op!);
        checks.fetchIssues = w.e.kiln.issues.length - before;
        // 解决冲突后提交
        await writeFile(path.join(wt, 'conflict.txt'), 'merged\n');
        await git(wt, 'add', '-A');
        await git(wt, 'commit', '-q', '--no-edit');
        checks.afterURL = await git(wt, 'config', '--get', 'remote.origin.url');
        checks.url = url;
        await writeFile(path.join(ctx.taskDir, 'delivery.json'), delivered);
        ctx.done();
      },
      {
        // 旧版本连接器留下的 clone：拉取、推送地址都是无效的
        preClone: async (clone, url, bare) => {
          await mkdir(path.dirname(clone), { recursive: true });
          await git(path.dirname(clone), 'clone', '--quiet', '--no-checkout', bare, clone);
          await git(clone, 'remote', 'set-url', 'origin', 'kiln-disabled://use-kiln-orca-task-exec-git-fetch-with-explicit-url');
          await git(clone, 'config', 'remote.origin.pushurl', 'kiln-disabled://agent-must-not-push');
          checks.oldPush = await git(clone, 'remote', 'get-url', '--push', 'origin');
          checks.url0 = url;
        },
      },
    );
    await w.run();

    expect(checks.oldPush).toBe('kiln-disabled://agent-must-not-push');
    expect(checks.fetchURL).toBe(checks.url);
    expect(checks.pushURL).toBe(checks.url);
    expect(checks.local).toBe(0);
    expect(checks.localIssues).toBe(0);
    expect(checks.localOps).toBe(0);
    expect(checks.mergeCode).not.toBe(0);
    expect(checks.mergeOut).toContain('CONFLICT');
    expect(checks.fetched).toBe(true);
    expect(checks.fetchIssues).toBe(1);
    expect(checks.fetchCred).toMatchObject({ access: CredentialAccess.READ, repository: 'acme/widgets', revoked: true });
    expect(checks.afterURL).toBe(checks.url);
    // 交付：checkDelivered 以 spec.sha 为起点通过，推送用单独的写操作
    const c = w.e.kiln.completes[0];
    expect(c?.outcome).toBe(Outcome.SUCCEEDED);
    const d = c?.result?.value as { sha: string };
    expect(await w.e.origin.branchSha(`kiln/attempt-${w.at.id}`)).toBe(d.sha);
    expect(w.e.kiln.credentials.map((x) => [x.access, x.repository, x.revoked])).toEqual([
      [CredentialAccess.READ, 'acme/widgets', true],
      [CredentialAccess.READ, 'acme/widgets', true],
      [CredentialAccess.WRITE, 'acme/widgets', true],
    ]);
    // 写请求只用写凭据（写只在 push 中出现）
    const write = w.e.kiln.credentials[2]!.secret;
    expect(w.s.requests.filter((r) => r.write).every((r) => r.password === write || r.password === '')).toBe(true);
    // 签发与吊销成对出现在连接器日志中（指纹相同）
    const issued = w.lines.filter((l) => l.msg === '签发 git 凭据').map((l) => l.fields.token);
    const revoked = w.lines.filter((l) => l.msg === '已请求 Server 关闭 git 操作、回收凭据').map((l) => l.fields.token);
    expect(new Set(revoked)).toEqual(new Set(issued));
    expect(issued).toHaveLength(3);
    await noTokens(w);
  });

  it('V6：两个并发的传输各自签发，先结束的只吊销自己的；正常退出后吊销', { timeout: scenarioMs }, async () => {
    const out: Record<string, unknown> = {};
    const w = await world('review', async (w, ctx) => {
      w.hold = (r) => r.password !== '';
      const a = w.exec(ctx.worktree, ['git', 'ls-remote', 'origin']);
      const b = w.exec(ctx.worktree, ['git', 'ls-remote', 'origin']);
      await waitFor(() => transportOps(w).length === 2 && new Set(w.held.map((h) => h.r.password)).size === 2, eventMs, '两个传输都签发并挂起');
      const [x, y] = transportOps(w).map((op) => credOf(w, op)!);
      expect(x!.secret).not.toBe(y!.secret);
      // 放行 x 的请求：x 的传输结束、吊销，y 不受影响（仍可用）
      w.hold = (r) => r.password === y!.secret;
      w.release((r) => r.password === x!.secret);
      await waitFor(() => x!.revoked, eventMs, 'x 吊销');
      out.yAfterX = y!.revoked;
      w.hold = () => false;
      w.release(() => true);
      const [ra, rb] = await Promise.all([a.result, b.result]);
      out.codes = [ra.code, rb.code];
      await waitFor(() => y!.revoked, eventMs, 'y 吊销');
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    await w.run();
    expect(out.yAfterX).toBe(false);
    expect(out.codes).toEqual([0, 0]);
    expect(w.e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
    // 准备仓库一枚 + 两个传输各一枚，都已吊销
    expect(w.e.kiln.credentials.map((c) => c.revoked)).toEqual([true, true, true]);
    await noTokens(w);
  });

  it('V6：传输包装被 SIGKILL：操作关闭、凭据吊销，真实远程助手之后的认证失败（不再签发、不用个人凭据）', { timeout: scenarioMs }, async () => {
    const out: Record<string, unknown> = {};
    const w = await world('review', async (w, ctx) => {
      w.hold = (r) => r.password !== '';
      const t = w.exec(ctx.worktree, ['git', 'ls-remote', 'origin']);
      await waitFor(() => w.held.length > 0, eventMs, '带凭据的请求挂起');
      const [op] = transportOps(w);
      const c = credOf(w, op!)!;
      process.kill(pidOf(w, op!), 'SIGKILL');
      await waitFor(() => c.revoked, eventMs, '包装被杀后吊销');
      const issues = w.e.kiln.issues.length;
      w.hold = () => false;
      w.release(() => true);
      const r = await t.result;
      out.code = r.code;
      out.newIssues = w.e.kiln.issues.length - issues;
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    await w.run();
    expect(out.code).not.toBe(0);
    expect(out.newIssues).toBe(0);
    await noTokens(w);
  });

  it('V6：只杀掉 task exec 时传输继续，操作在传输结束后才关闭', { timeout: scenarioMs }, async () => {
    const out: Record<string, unknown> = {};
    const w = await world('review', async (w, ctx) => {
      const upstream = await w.e.origin.commit('late.txt', 'late\n', 'master');
      w.hold = (r) => r.password !== '';
      const t = w.exec(ctx.worktree, ['git', 'fetch', 'origin']);
      await waitFor(() => w.held.length > 0, eventMs, '带凭据的请求挂起');
      const [op] = transportOps(w);
      const c = credOf(w, op!)!;
      t.child.kill('SIGKILL');
      // git 继承了 task exec 的 stdout、stderr：只等 task exec 本身退出
      await t.exited;
      out.revokedAfterKill = c.revoked;
      w.hold = () => false;
      w.release(() => true);
      await waitFor(() => c.revoked, eventMs, '传输结束后吊销');
      // git 在传输结束之后才更新 ref（孙进程的输出由 result 读完）
      await t.result;
      out.fetched = (await git(ctx.worktree, 'rev-parse', 'refs/remotes/origin/master')) === upstream;
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    await w.run();
    expect(out.revokedAfterKill).toBe(false);
    expect(out.fetched).toBe(true);
    await noTokens(w);
  });

  it('V6：签发在途时连接断开：迟到的凭据立即吊销，不交给 git', { timeout: scenarioMs }, async () => {
    const out: Record<string, unknown> = {};
    const w = await world('review', async (w, ctx) => {
      let release!: () => void;
      let held = false;
      w.e.kiln.credentialHold = () => {
        held = true;
        return new Promise((r) => (release = r));
      };
      const t = w.exec(ctx.worktree, ['git', 'ls-remote', 'origin']);
      await waitFor(() => held, eventMs, '传输的签发挂起');
      const [op] = transportOps(w);
      process.kill(pidOf(w, op!), 'SIGKILL');
      // 连接器看到连接关闭；签发还在途，关闭等它回来
      await waitFor(() => w.lines.some((l) => l.msg === 'git 传输结束' && l.fields.op === op), eventMs, '连接关闭');
      w.e.kiln.credentialHold = undefined;
      release();
      await waitFor(() => w.e.kiln.revokes.some((r) => r.opId === op), eventMs, '迟到的凭据吊销');
      const c = credOf(w, op!)!;
      out.revoke = w.e.kiln.revokes.find((r) => r.opId === op);
      out.secret = c.secret;
      out.revoked = c.revoked;
      out.used = w.s.requests.some((r) => r.password === c.secret);
      out.code = (await t.result).code;
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    await w.run();
    expect(out.revoke).toMatchObject({ secret: out.secret });
    expect(out.revoked).toBe(true);
    expect(out.used).toBe(false);
    expect(out.code).not.toBe(0);
    await noTokens(w);
  });

  it('V7：认证冲突（地址带凭据、本地 / 全局 / 命令行的 extraHeader、netrc）各自只拒绝那一次传输、不签发；日志有键和来源，没有值', { timeout: scenarioMs }, async () => {
    const results: Array<{ name: string; code: number | null; stderr: string }> = [];
    const secrets = ['Sekr3tUrlPw', 'Sekr3tLocalHdr', 'Sekr3tGlobalHdr', 'Sekr3tCliHdr', 'Sekr3tNetrc'];
    let issuesDuring = -1;
    let ok: Result | undefined;
    const w = await world('review', async (w, ctx) => {
      const wt = ctx.worktree;
      const host = w.s.host;
      const before = w.e.kiln.issues.length;
      const run = async (name: string, argv: string[], env?: Record<string, string>) => {
        const r = await w.exec(wt, argv, { env }).result;
        results.push({ name, code: r.code, stderr: r.stderr });
      };
      await run('url', ['git', '-c', `url.http://u:${secrets[0]}@${host}/.insteadOf=http://${host}/`, 'ls-remote', 'origin']);
      // 本地、全局、命令行的按主机 / 不限地址的请求头：对依赖仓库生效（clone 的本地置空只覆盖任务仓库这个地址，见下一个
      // 用例：比它不具体的请求头对 origin 都不生效，git 也不发出）
      const dep = `http://${host}/acme/lib.git`;
      await git(wt, 'config', '--local', `http.http://${host}/.extraheader`, `X-Token: ${secrets[1]}`);
      await run('local', ['git', 'ls-remote', dep]);
      await git(wt, 'config', '--local', '--unset-all', `http.http://${host}/.extraheader`);
      const global = path.join(w.e.root, 'global-header.gitconfig');
      await writeFile(global, `[http]\n\textraHeader = X-Token: ${secrets[2]}\n`);
      await run('global', ['git', 'ls-remote', dep], { GIT_CONFIG_GLOBAL: global });
      await run('cli', ['git', '-c', `http.extraHeader=X-Token: ${secrets[3]}`, 'ls-remote', dep]);
      const netrc = path.join(w.userHome, '.netrc');
      await writeFile(netrc, `machine 127.0.0.1\n  login x\n  password ${secrets[4]}\n`, { mode: 0o600 });
      await run('netrc', ['git', 'ls-remote', 'origin']);
      await rm(netrc);
      issuesDuring = w.e.kiln.issues.length - before;
      // 节点（这个 attempt）照常：冲突去掉之后同样的传输成功
      ok = await w.exec(wt, ['git', 'ls-remote', 'origin']).result;
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    await w.run();
    expect(results.map((r) => r.name)).toEqual(['url', 'local', 'global', 'cli', 'netrc']);
    for (const r of results) {
      expect(r.code, r.name).not.toBe(0);
      expect(r.stderr, r.name).toContain('拒绝这次 git 传输');
    }
    expect(results[0]?.stderr).toContain('地址中的用户名或密码');
    expect(results[1]?.stderr).toContain('http.extraheader');
    expect(results[2]?.stderr).toContain('http.extraheader');
    expect(results[3]?.stderr).toContain('http.extraheader');
    expect(results[4]?.stderr).toContain('netrc 中的主机 127.0.0.1');
    expect(results[4]?.stderr).toContain(path.join(w.userHome, '.netrc'));
    expect(issuesDuring).toBe(0);
    expect(ok?.code).toBe(0);
    // warn 日志：每一次拒绝一条，有键和来源，没有值；任务日志同样
    const warns = w.lines.filter((l) => l.level === 'warn' && l.msg.includes('拒绝了一次 git 传输'));
    expect(warns).toHaveLength(5);
    expect(String(warns[4]?.fields.conflicts)).toContain('.netrc');
    expect(w.e.kiln.log).toContain('拒绝了一次 git 传输');
    const all = [JSON.stringify(w.lines), w.e.kiln.log, ...results.map((r) => r.stderr)].join('\n');
    for (const s of secrets) expect(all).not.toContain(s);
    await noTokens(w);
  });

  it('全局配置、本地配置中按主机的个人请求头：clone 中任务仓库地址上的空值使它们对 origin 不生效，包装的冲突判定与实际发出的请求头一致（不拒绝）、只带签发的凭据', { timeout: scenarioMs }, async () => {
    const personalSecret = 'Sekr3tPersonal';
    const header = `Authorization: Basic ${Buffer.from(`me:${personalSecret}`).toString('base64')}`;
    let r: Result | undefined;
    let deps: Result | undefined;
    const w = await world('review', async (w, ctx) => {
      const wt = ctx.worktree;
      const global = path.join(w.e.root, 'personal-header.gitconfig');
      await writeFile(global, `[http]\n\textraHeader = ${header}\n`);
      await git(wt, 'config', '--local', `http.http://${w.s.host}/.extraheader`, header);
      r = await w.exec(wt, ['git', 'ls-remote', 'origin'], { env: { GIT_CONFIG_GLOBAL: global } }).result;
      // 对照：依赖仓库的地址不在本地置空的范围内，同样的请求头对它生效，包装拒绝
      deps = await w.exec(wt, ['git', 'ls-remote', `http://${w.s.host}/acme/lib.git`], { env: { GIT_CONFIG_GLOBAL: global } }).result;
      await git(wt, 'config', '--local', '--unset-all', `http.http://${w.s.host}/.extraheader`);
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    await w.run();
    expect(r?.code, r?.stderr).toBe(0);
    expect(r?.stderr).not.toContain('拒绝这次 git 传输');
    expect(deps?.code).not.toBe(0);
    expect(deps?.stderr).toContain('http.extraheader');
    // 任务仓库的请求只带签发的凭据，个人的请求头没有发出
    expect(w.s.requests.some((x) => x.password === personalSecret)).toBe(false);
    expect(w.s.requests.filter((x) => x.repo === 'acme/widgets.git' && x.password !== '').every((x) => x.user === 'x-access-token')).toBe(true);
    await noTokens(w);
  });

  it('V7：未授权的仓库、请求与操作的仓库不符、不经传输包装的请求：都不签发，也不调用个人 helper 和 askpass', { timeout: scenarioMs }, async () => {
    const out: Record<string, unknown> = {};
    const w = await world('review', async (w, ctx) => {
      const wt = ctx.worktree;
      const before = w.e.kiln.issues.length;
      // 没有声明的仓库：这次传输没有操作
      const secret = await w.exec(wt, ['git', 'ls-remote', w.s.url('acme/secret')]).result;
      out.secret = secret.code;
      // 操作批准的是 acme/widgets，helper 请求的是 acme/lib（与操作不符）
      const opened = w.at.openTransport(w.s.url('acme/widgets'), 'test');
      const fill = (p: string) =>
        w.exec(wt, ['/bin/sh', '-c', `KILN_ORCA_GIT_OP=${opened.op} git credential fill`], { input: `protocol=http\nhost=${w.s.host}\npath=${p}\n\n` }).result;
      const mismatch = await fill('acme/lib.git');
      out.mismatch = [mismatch.code, mismatch.stdout.includes('password=')];
      // 不在任何传输中（没有 KILN_ORCA_GIT_OP）
      const noOp = await w.exec(wt, ['git', 'credential', 'fill'], { input: `protocol=http\nhost=${w.s.host}\npath=acme/widgets.git\n\n` }).result;
      out.noOp = [noOp.code, noOp.stdout.includes('password=')];
      out.issues = w.e.kiln.issues.length - before;
      // 同一个操作、仓库一致时才给出
      const match = await fill('acme/widgets.git');
      out.match = match.stdout.includes('password=');
      opened.onClose?.();
      await waitFor(() => credOf(w, opened.op)?.revoked === true, eventMs, '测试打开的操作吊销');
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    await w.run();
    expect(out.secret).not.toBe(0);
    const [mismatchCode, mismatchPassword] = out.mismatch as [number, boolean];
    expect(mismatchCode).not.toBe(0);
    expect(mismatchPassword).toBe(false);
    const [noOpCode, noOpPassword] = out.noOp as [number, boolean];
    expect(noOpCode).not.toBe(0);
    expect(noOpPassword).toBe(false);
    expect(out.issues).toBe(0);
    expect(out.match).toBe(true);
    // 测试自己取出的那一枚出现在输出中：从 outputs 中去掉再检查其它地方
    w.outputs.splice(0);
    await noTokens(w);
  });

  it('传输地址的归一化（真实 git）：仓库名大小写、不带 .git、经 insteadOf 的地址、子模块各自按批准的仓库签发；路径穿越、百分号编码不给凭据', { timeout: scenarioMs }, async () => {
    const out: Record<string, unknown> = {};
    const w = await world('review', async (w, ctx) => {
      const wt = ctx.worktree;
      const host = `http://${w.s.host}`;
      const ok = async (name: string, argv: string[]) => {
        const before = transportOps(w).length;
        const r = await w.exec(wt, argv).result;
        const ops = transportOps(w).slice(before);
        const creds = ops.map((op) => credOf(w, op));
        out[name] = [r.code, creds.map((c) => c?.repository)];
        await waitFor(() => creds.every((c) => c?.revoked), eventMs, `${name} 的凭据吊销`);
      };
      await ok('case', ['git', 'ls-remote', `${host}/ACME/Widgets.git`]);
      await ok('noGit', ['git', 'ls-remote', `${host}/acme/widgets`]);
      await ok('insteadOf', ['git', '-c', `url.${host}/acme/.insteadOf=team:`, 'ls-remote', 'team:widgets.git']);
      // 子模块：声明的依赖仓库 acme/lib（指向同一个仓库的一个 commit）
      const sub = await git(wt, 'rev-parse', 'HEAD');
      await writeFile(path.join(wt, '.gitmodules'), `[submodule "lib"]\n\tpath = lib\n\turl = ${host}/acme/lib.git\n`);
      await git(wt, 'update-index', '--add', '--cacheinfo', `160000,${sub},lib`);
      await git(wt, 'add', '.gitmodules');
      await git(wt, 'commit', '-qm', 'submodule');
      await ok('submodule', ['git', 'submodule', 'update', '--init', '--quiet']);
      out.subHead = await git(path.join(wt, 'lib'), 'rev-parse', 'HEAD');
      out.sub = sub;
      const before = w.e.kiln.issues.length;
      const traversal = await w.exec(wt, ['git', 'ls-remote', `${host}/x/../acme/widgets.git`]).result;
      const encoded = await w.exec(wt, ['git', 'ls-remote', `${host}/acme/%77idgets.git`]).result;
      out.rejected = [traversal.code !== 0, encoded.code !== 0, w.e.kiln.issues.length - before];
      await git(wt, 'reset', '-q', '--hard', 'HEAD~1');
      await rm(path.join(wt, 'lib'), { recursive: true, force: true });
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    await w.run();
    expect(out.case).toEqual([0, ['acme/widgets']]);
    expect(out.noGit).toEqual([0, ['acme/widgets']]);
    expect(out.insteadOf).toEqual([0, ['acme/widgets']]);
    expect(out.submodule).toEqual([0, ['acme/lib']]);
    expect(out.subHead).toBe(out.sub);
    expect(out.rejected).toEqual([true, true, 0]);
    await noTokens(w);
  });

  it('嵌套 task exec：内层 task exec 与连接器自己的网络 git 不继承外层的传输包装，请求照常按操作签发；裸冲突传输仍由外层拒绝', { timeout: scenarioMs }, async () => {
    const out: Record<string, unknown> = {};
    const w = await world('review', async (w, ctx) => {
      const warns = () => w.lines.filter((l) => l.level === 'warn' && l.msg.includes('拒绝了一次 git 传输')).length;
      const warnsBefore = warns();
      // 连接器自己的网络 git（prepareClone、push）走一个只接受 tok 的服务：外层环境中的 GIT_EXEC_PATH 若被继承，git 会
      // 走外层的传输包装，按外层看到的请求头 / netrc 拒绝（生产现象）。
      const s2 = await gitHTTP(path.join(w.e.root, 'nested-http'), { 'acme/widgets': w.e.origin.bare }, (r) => r.password === 'tok');
      cleanups.push(() => s2.close());
      const sha = await w.e.origin.branchSha('master');
      const coreGit = path.join(path.dirname(cli), '..', 'core', 'git.js');
      const coreExec = path.join(path.dirname(cli), '..', 'core', 'exec.js');
      const script = path.join(w.e.root, 'nested.mjs');
      await writeFile(
        script,
        [
          `import { Git } from ${JSON.stringify(coreGit)};`,
          `import { nodeExec } from ${JSON.stringify(coreExec)};`,
          'const [dir, url, sha, secret] = process.argv.slice(2);',
          'const g = new Git(nodeExec);',
          `await g.prepareClone(dir, url, [sha], 0, { username: 'x-access-token', secret });`,
          `await g.run(dir, 'checkout', '--quiet', '-B', 'master', 'origin/master');`,
          `const { writeFileSync } = await import('node:fs');`,
          'writeFileSync(`${dir}/nested.txt`, "nested\\n");',
          `await g.run(dir, 'add', '-A');`,
          `await g.run(dir, '-c', 'user.name=t', '-c', 'user.email=t@t', 'commit', '--quiet', '-m', 'nested');`,
          `await g.push(dir, url, 42n, undefined, { username: 'x-access-token', secret });`,
          `console.log('NESTED_OK');`,
        ].join('\n'),
      );
      const clone = path.join(w.e.root, 'nested-clone');
      // 外层 task exec：它设置 GIT_EXEC_PATH、GIT_CONFIG_COUNT、KILN_ORCA_GIT_OP 等，子进程（连接器的网络 git）不能继承
      const child = await w.exec(ctx.worktree, [process.execPath, script, clone, s2.url('acme/widgets'), sha, 'tok']).result;
      out.child = [child.code, child.stdout.includes('NESTED_OK'), child.stderr];
      out.s2 = {
        read: s2.requests.some((r) => !r.write && r.password === 'tok'),
        write: s2.requests.some((r) => r.write && r.password === 'tok'),
        other: s2.requests.some((r) => r.password !== 'tok'),
      };
      // 内层真实 task exec：内层自己的传输包装工作，按操作签发并关闭
      const before = w.e.kiln.credentials.length;
      const inner = await w.exec(ctx.worktree, [process.execPath, cli, 'task', '--home', w.home.dir, '--attempt', String(w.at.id), 'exec', '--', 'git', 'ls-remote', 'origin']).result;
      out.inner = [inner.code, inner.stderr];
      const added = w.e.kiln.credentials.slice(before);
      out.added = added.length;
      await waitFor(() => added.every((c) => c.revoked), eventMs, '内层传输的凭据吊销');
      out.revoked = added.every((c) => c.revoked);
      // 同一外层环境中的裸冲突传输（不经内层 task exec）：外层仍然严格拒绝，并记拒绝日志
      const conflict = await w.exec(ctx.worktree, ['git', '-c', `http.extraHeader=X-Evil: 1`, 'ls-remote', s2.url('acme/widgets')]).result;
      out.conflict = [conflict.code, conflict.stderr.includes('拒绝这次 git 传输')];
      out.warns = warns() - warnsBefore;
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    await w.run();
    expect(out.child).toEqual([0, true, '']);
    expect(out.s2).toEqual({ read: true, write: true, other: false });
    expect(out.inner).toEqual([0, '']);
    expect(out.added).toBeGreaterThan(0);
    expect(out.revoked).toBe(true);
    expect(out.conflict).toEqual([expect.any(Number), true]);
    expect((out.conflict as [number, boolean])[0]).not.toBe(0);
    // 只有裸冲突传输那一次拒绝；连接器自己的网络 git 与内层 task exec 都没有惊动外层
    expect(out.warns).toBe(1);
    await noTokens(w);
  });
});
