import { existsSync } from 'node:fs';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { create } from '@bufbuild/protobuf';
import { Code, ConnectError } from '@connectrpc/connect';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { type Credential, CredentialAccess, CredentialSchema, Outcome } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { Attempt, credentialAttempts } from '../src/core/attempt.js';
import type { KeyedMutex } from '../src/core/mutex.js';
import {
  credentialAllowed,
  execGitEnv,
  execGitOf,
  GitOps,
  goPrivatePattern,
  maxCredentialQueryBytes,
  OpClosedError,
  parseCredentialQuery,
  readBounded,
  repoOfPath,
  tokenFingerprint,
  transportRepo,
} from '../src/core/credentials.js';
import { ErrorClass, StaleError } from '../src/core/errors.js';
import { authQueryTimeoutMs, realExecPath } from '../src/core/gitexec.js';
import { type Exec, nodeExec } from '../src/core/exec.js';
import { Git, gitAuthor, gitEmail, localGitEnv, networkEnv, quitHelper } from '../src/core/git.js';
import type { Logger } from '../src/core/log.js';
import { basicAuth, circularMark, Redactor, redactedMark, redactingLogger } from '../src/core/redact.js';
import { type GitHTTP, gitHTTP } from './githttp.js';
import { type AgentCtx, devTask, type Env, git, heartbeatGate, makeEnv, reviewTask, waitFor, wireError } from './harness.js';

// git 凭据（PLAN #33.13）：按操作签发、用后吊销。连接器的网络 git 只用 Server 签发的短期凭据、与机器上的 git 配置隔离；
// Agent 的 git 传输经 kiln-orca task exec 的传输包装各开一个读操作（见 transport.test.ts）；写凭据只在推送前签发、推完吊销；
// 凭据不出现在日志和错误说明中。

let env: Env | undefined;
let srv: GitHTTP | undefined;
afterEach(async () => {
  vi.unstubAllEnvs();
  await srv?.close();
  srv = undefined;
  await env?.cleanup();
  env = undefined;
});

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

const report = JSON.stringify({ version: 1, verdict: 'approve', summary: 'ok', findings: [] });

/**
 * httpEnv 建测试环境，origin 经 HTTP 提供（acme/widgets，另有依赖仓库 acme/lib）：只接受 FakeKiln 签发、没有吊销的
 * 凭据，推送要求写凭据。机器上的全局 git 配置换成"恶意"的：把仓库地址改写到别处（insteadOf），凭据 helper 留下痕迹
 * 并给出假密码，另有一个本地命令能读到的标记。
 */
async function httpEnv(agent: (ctx: AgentCtx) => Promise<void>) {
  const e = await makeEnv(agent);
  env = e;
  const s = await gitHTTP(e.root, { 'acme/widgets': e.origin.bare, 'acme/lib': e.origin.bare }, (r) => {
    const c = e.kiln.credentials.find((x) => x.secret === r.password && !x.revoked);
    return r.user === 'x-access-token' && !!c && (!r.write || c.access === CredentialAccess.WRITE);
  });
  srv = s;
  const trace = path.join(e.root, 'global-helper-called');
  const global = path.join(e.root, 'evil.gitconfig');
  await writeFile(
    global,
    [
      `[url "http://127.0.0.1:1/evil/"]`,
      `\tinsteadOf = http://${s.host}/`,
      `[credential]`,
      `\thelper = "!f() { echo called >> '${trace}'; echo username=evil; echo password=evil; }; f"`,
      `[kiln]`,
      `\tmarker = from-global`,
      '',
    ].join('\n'),
  );
  vi.stubEnv('GIT_CONFIG_GLOBAL', global);
  return { e, s, trace };
}

describe('连接器的网络 git', () => {
  it('审计：准备仓库是一个读操作，准备结束即吊销（V1），之后本地 git 照常；clone 的 origin 是真实地址，不经 task exec 的网络 git 不会用到个人凭据', async () => {
    let during: Array<[CredentialAccess, boolean]> = [];
    let local = '';
    let seen: { env: string; noOp: string } | undefined;
    const { e, s, trace } = await httpEnv(async (ctx) => {
      during = e.kiln.credentials.map((c) => [c.access, c.revoked]);
      // 本地命令照常（凭据已经吊销）
      local = await git(ctx.worktree, 'log', '--format=%H', '-1', 'origin/feature');
      await git(ctx.worktree, 'diff', '--stat', 'origin/master...origin/feature');
      seen = {
        env: (await ctx.tool('exec-env')).output,
        // 不属于任何传输（没有操作）的 helper 请求不回答
        noOp: (await ctx.tool('git-credential', { protocol: 'http', host: s.host, path: 'acme/widgets.git' })).output,
      };
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const base = await e.origin.branchSha('master');
    const head = await e.origin.commit('a.txt', 'x\n', 'feature');
    const task = reviewTask(e.origin, { sha: head, baseSha: base });
    task.cloneUrl = s.url('acme/widgets');
    task.repositories = ['acme/lib'];
    const at = new Attempt(e.deps, task);
    await run(e, at);

    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
    // 准备仓库：一个读操作、一枚任务仓库的凭据，Agent 开始之前已经吊销（带 op_id 和凭据）
    expect(during).toEqual([[CredentialAccess.READ, true]]);
    const c = e.kiln.credentials[0]!;
    expect(c).toMatchObject({ access: CredentialAccess.READ, repository: 'acme/widgets', revoked: true });
    expect(e.kiln.revokes.map((r) => [r.opId, r.secret])).toEqual([[c.opId, c.secret]]);
    expect(local).toBe(head);
    // 请求都到了真正的地址（没有按全局配置改写），用的是签发的凭据，全局的凭据 helper 没有被调用
    expect(s.requests.length).toBeGreaterThan(0);
    expect(s.requests.every((r) => r.repo === 'acme/widgets.git' && r.password === c.secret && !r.write)).toBe(true);
    expect(existsSync(trace)).toBe(false);
    expect(seen?.noOp).toBe('');
    expect(JSON.parse(seen?.env ?? '')).toEqual({
      env: null,
      git: { helperURL: `http://${s.host}`, goprivate: [`${s.host}/[aA][cC][mM][eE]/[lL][iI][bB]`], execRoot: path.join(at.taskDir, 'git-exec') },
    });
    // clone：origin 是真实地址、没有推送地址，refspec 照常，身份是连接器的；任务主机上只有回答 quit 的 helper；配置中没有凭据
    const clone = path.join(e.deps.reposDir, '42', 'widgets');
    // （机器上的 insteadOf 照常作用于 get-url，这里读原始配置）
    expect(await git(clone, 'config', '--get', 'remote.origin.url')).toBe(s.url('acme/widgets'));
    expect((await nodeExec('git', ['config', '--get', 'remote.origin.pushurl'], { cwd: clone, env: localGitEnv(process.env) })).code).toBe(1);
    expect(await git(clone, 'config', '--get-all', 'remote.origin.fetch')).toBe('+refs/heads/*:refs/remotes/origin/*');
    expect((await nodeExec('git', ['config', '--local', '--get-all', `credential.http://${s.host}.helper`], { cwd: clone, env: localGitEnv(process.env) })).stdout).toBe(`\n${quitHelper}\n`);
    expect(await git(clone, 'rev-parse', 'refs/remotes/origin/feature')).toBe(head);
    expect(await git(clone, 'config', '--local', '--get', 'user.name')).toBe(gitAuthor);
    expect(await git(clone, 'config', '--local', '--get', 'user.email')).toBe(gitEmail);
    expect(await readFile(path.join(clone, '.git', 'config'), 'utf8')).not.toContain(c.secret);
    // 不经 task exec 的网络 git：任务主机上 helper 回答 quit，全局的 helper（insteadOf 之前的地址也一样）不被调用，直接失败
    // （另一份全局配置：只有个人 helper，没有 insteadOf，请求真的到达任务主机）
    const helperOnly = path.join(e.root, 'helper-only.gitconfig');
    await writeFile(helperOnly, `[credential]\n\thelper = "!f() { echo called >> '${trace}'; echo username=evil; echo password=evil; }; f"\n`);
    s.requests.length = 0;
    // 测试自己运行 git 时经 localGitEnv 清掉外层 kiln-orca task exec / CI Runner 的包装、配置注入和 askpass：
    // 继承 GIT_EXEC_PATH 会走外层的传输包装，请求到不了模拟服务。这里测的是不经 task exec 的本地网络 git，它应该按
    // clone 的本地配置以 quit 失败。
    const direct = await nodeExec('git', ['ls-remote', 'origin'], {
      cwd: clone,
      env: { ...localGitEnv(process.env), GIT_CONFIG_GLOBAL: helperOnly },
    });
    expect(direct.code).not.toBe(0);
    expect(direct.stderr).toContain('told us to quit');
    // 请求确实到了模拟服务（匿名；401 之后才走到本地 quitHelper），不是被外层或环境错误挡下造成的假通过
    expect(s.requests.length).toBeGreaterThan(0);
    expect(s.requests.every((x) => x.password === '')).toBe(true);
    expect(existsSync(trace)).toBe(false);
    // 本地命令照常读机器上的配置
    expect(await e.deps.git.run(clone, 'config', '--get', 'kiln.marker')).toBe('from-global');
    expect(e.kiln.log).not.toContain(c.secret);
  });

  it('开发：写凭据只在推送前签发、只用于推送，推送确认后吊销；Agent 的 commit 用 clone 中的身份', async () => {
    let during: Array<[CredentialAccess, boolean]> = [];
    const { e, s, trace } = await httpEnv(async (ctx) => {
      await writeFile(path.join(ctx.worktree, 'feature.go'), 'package x\n');
      await git(ctx.worktree, 'add', '-A');
      // 不给身份：用连接器写在 clone 中的
      await git(ctx.worktree, 'commit', '--quiet', '-m', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      during = e.kiln.credentials.map((c) => [c.access, c.revoked]);
      ctx.done();
    });
    const sha = await e.origin.branchSha('master');
    const task = devTask(e.origin, { sha, mode: 'implement' });
    task.cloneUrl = s.url('acme/widgets');
    const at = new Attempt(e.deps, task);
    await run(e, at);

    const c = e.kiln.completes[0];
    expect(c?.outcome).toBe(Outcome.SUCCEEDED);
    const d = c?.result?.value as { branch: string; sha: string };
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe(d.sha);
    expect(await git(e.origin.bare, 'log', '-1', '--format=%an <%ae>', d.sha)).toBe(`${gitAuthor} <${gitEmail}>`);
    // Agent 运行时没有任何有效的凭据（准备仓库的读凭据已经吊销）
    expect(during).toEqual([[CredentialAccess.READ, true]]);
    // 写凭据推送后吊销；两个操作各自一枚
    expect(e.kiln.credentials.map((x) => [x.access, x.revoked])).toEqual([
      [CredentialAccess.READ, true],
      [CredentialAccess.WRITE, true],
    ]);
    const write = e.kiln.credentials[1]!;
    expect(write.opId).not.toBe(e.kiln.credentials[0]!.opId);
    const pushes = s.requests.filter((r) => r.write);
    expect(pushes.length).toBeGreaterThan(0);
    expect(pushes.every((r) => r.password === write.secret)).toBe(true);
    // 推送之后的 ls-remote 也用写凭据（在吊销之前）
    expect(s.requests.at(-1)?.password).toBe(write.secret);
    expect(e.kiln.revokes.at(-1)).toMatchObject({ opId: write.opId, secret: write.secret });
    expect(existsSync(trace)).toBe(false);
  });

  it('准备仓库：等仓库锁期间不签发；取得锁之后才签发，prepareClone 返回即吊销，Orca 注册仓库（repoAdd）开始之前凭据已经吊销', async () => {
    const e = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    env = e;
    // 记下每次进入仓库锁（第一次是测试自己占住锁）
    const entered: string[] = [];
    const mutex = e.deps.repoLock;
    e.deps.repoLock = Object.assign(Object.create(Object.getPrototypeOf(mutex) as object) as KeyedMutex, mutex, {
      run: <T>(key: string, f: () => Promise<T>) => {
        entered.push(key);
        return mutex.run(key, f);
      },
    });
    const sha = await e.origin.branchSha('master');
    const task = reviewTask(e.origin, { sha, baseSha: sha });
    let releaseLock!: () => void;
    const held = e.deps.repoLock.run(String(task.repoId), () => new Promise<void>((r) => (releaseLock = r)));
    let atRepoAdd: boolean[] | undefined;
    let releaseAdd!: () => void;
    e.orca.repoAddHold = (p) => {
      // 只管任务仓库的 clone（邮箱仓库也经 repoAdd 注册）
      if (!p.startsWith(e.deps.reposDir)) return undefined;
      atRepoAdd = e.kiln.credentials.map((c) => c.revoked);
      return new Promise<void>((r) => (releaseAdd = r));
    };
    const at = new Attempt(e.deps, task);
    const running = run(e, at);
    // attempt 已经在排队等锁：这期间没有签发
    await waitFor(() => entered.length === 2, 10_000, 'attempt 排队等仓库锁');
    expect(e.kiln.issues).toHaveLength(0);
    releaseLock();
    await held;
    await waitFor(() => atRepoAdd !== undefined, 10_000, 'Orca 注册仓库');
    expect(e.kiln.issues).toHaveLength(1);
    expect(atRepoAdd).toEqual([true]);
    releaseAdd();
    await running;
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
  });

  it('写：推送用的签发回复丢失（Server 已签出）：同一 op_id 不出现第二次签发，原操作关闭，用新的操作推送成功', async () => {
    const { e, s } = await httpEnv(async (ctx) => {
      await writeFile(path.join(ctx.worktree, 'feature.go'), 'package x\n');
      await git(ctx.worktree, 'add', '-A');
      await git(ctx.worktree, 'commit', '--quiet', '-m', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      ctx.done();
    });
    let lost = 1;
    e.kiln.credentialLost = (req) => req.access === CredentialAccess.WRITE && lost-- > 0;
    const sha = await e.origin.branchSha('master');
    const task = devTask(e.origin, { sha, mode: 'implement' });
    task.cloneUrl = s.url('acme/widgets');
    const at = new Attempt(e.deps, task);
    await run(e, at);
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
    const writes = e.kiln.credentials.filter((c) => c.access === CredentialAccess.WRITE);
    expect(writes).toHaveLength(2);
    const [first, second] = writes;
    expect(second?.opId).not.toBe(first?.opId);
    expect(e.kiln.issues.filter((r) => r.opId === first?.opId)).toHaveLength(1);
    expect(first?.revoked).toBe(true);
    expect(second?.revoked).toBe(true);
    // 推送只用第二枚，丢失的那枚没有被用到
    const pushes = s.requests.filter((r) => r.write);
    expect(pushes.length).toBeGreaterThan(0);
    expect(pushes.every((r) => r.password === second?.secret)).toBe(true);
  });

  it('Agent 的传输：签发回复丢失时这次 git 访问失败，不用同一 op_id 重放、不换新的操作；传输结束时按原 op_id 关闭', async () => {
    let opened: ReturnType<Attempt['openTransport']> | undefined;
    let reply: { code: number; output: string } | undefined;
    let issuesForOp = -1;
    const { e, s } = await httpEnv(async (ctx) => {
      e.kiln.credentialLost = () => true;
      opened = at.openTransport(s.url('acme/widgets'), '1');
      reply = await ctx.tool('git-credential', { op: opened.op, protocol: 'http', host: s.host, path: 'acme/widgets.git' });
      e.kiln.credentialLost = () => false;
      issuesForOp = e.kiln.issues.length;
      await opened.onClose?.();
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const sha = await e.origin.branchSha('master');
    const task = reviewTask(e.origin, { sha, baseSha: sha });
    task.cloneUrl = s.url('acme/widgets');
    const at = new Attempt(e.deps, task);
    await run(e, at);
    expect(reply?.output).not.toContain('password=');
    const mine = e.kiln.credentials.filter((c) => c.opId === opened?.op);
    // 准备仓库 1 次 + 这次传输 1 次
    expect(issuesForOp).toBe(2);
    expect(e.kiln.issues.filter((r) => r.opId === opened?.op)).toHaveLength(1);
    expect(mine).toHaveLength(1);
    expect(mine[0]?.revoked).toBe(true);
    expect(e.kiln.revokes.filter((r) => r.opId === opened?.op).map((r) => r.secret)).toEqual(['']);
  });

  it('失效后不再签发写凭据、不推 checkpoint（不再调用 Server）', async () => {
    let working = false;
    const { e, s } = await httpEnv(async (ctx) => {
      await writeFile(path.join(ctx.worktree, 'wip.txt'), '一半\n');
      working = true;
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    e.kiln.heartbeatFn = () => (working ? new StaleError() : {});
    const sha = await e.origin.branchSha('master');
    const task = devTask(e.origin, { sha, mode: 'implement' });
    task.cloneUrl = s.url('acme/widgets');
    const at = new Attempt(e.deps, task);
    await run(e, at);
    expect(e.kiln.completes).toEqual([]);
    expect(e.kiln.credentials.map((x) => x.access)).toEqual([CredentialAccess.READ]);
    // 只有准备仓库那一个操作的吊销
    expect(e.kiln.revokes).toHaveLength(1);
    expect(s.requests.some((r) => r.write)).toBe(false);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toBe('');
  });

  it('helper 要凭据的过程中任务停止：签发回来的读凭据不交给 Agent，随后吊销', async () => {
    let reply: { code: number; output: string } | undefined;
    let releaseCI!: () => void;
    let opened: ReturnType<Attempt['openTransport']> | undefined;
    const { e, s } = await httpEnv(async (ctx) => {
      // 排队中的 ci 调用挡住结束流程的 closeTools：停止已经生效、传输的操作还没有关闭的窗口
      void ctx.tool('ci', { sha: '' });
      opened = at.openTransport(s.url('acme/widgets'), '1');
      reply = await ctx.tool('git-credential', { op: opened.op, protocol: 'http', host: s.host, path: 'acme/widgets.git' });
      await new Promise<void>((r) => ctx.stopped.addEventListener('abort', () => r()));
    });
    e.kiln.verificationHold = new Promise((r) => (releaseCI = r));
    let releaseIssue!: () => void;
    let held = false;
    const sha = await e.origin.branchSha('master');
    const task = reviewTask(e.origin, { sha, baseSha: sha });
    task.cloneUrl = s.url('acme/widgets');
    const at = new Attempt(e.deps, task);
    const running = run(e, at);
    // 准备仓库的签发照常；helper 的签发挂起
    await waitFor(() => e.kiln.credentials.length === 1, 10_000, '准备仓库的读凭据');
    e.kiln.credentialHold = () => {
      held = true;
      return new Promise((r) => (releaseIssue = r));
    };
    await waitFor(() => held && e.kiln.verifications.length === 1, 10_000, 'helper 的签发挂起、ci 排队');
    // 签发返回之前取消：工具关闭，结束流程等在 closeTools（ci 还没有返回）
    await heartbeatGate(e, () => ({ cancelRequested: true })).fire();
    e.kiln.credentialHold = undefined;
    releaseIssue();
    await waitFor(() => reply !== undefined, 10_000, 'helper 的回复');
    const fresh = e.kiln.credentials[1]!;
    expect(fresh.access).toBe(CredentialAccess.READ);
    expect(reply?.code).toBe(1);
    expect(reply?.output).not.toContain(fresh.secret);
    expect(reply?.output).not.toContain('password=');
    // 停止之后的请求同样拿不到，也不再开新的传输
    expect((await at.tool('git-credential', { op: opened!.op, protocol: 'http', host: s.host, path: 'acme/widgets.git' })).output).not.toContain('password=');
    expect(at.openTransport(s.url('acme/widgets'), '2')).toMatchObject({ code: 1, op: '' });
    releaseCI();
    await running;
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.CANCELLED);
    // 结束时关闭 Agent 还没有结束的传输：这个读凭据被吊销（包装的连接之后才关闭也不会再吊销一次）
    expect(e.kiln.revokes.filter((r) => r.opId === fresh.opId).map((r) => r.secret)).toEqual([fresh.secret]);
    opened?.onClose?.();
    await Promise.resolve();
    expect(e.kiln.revokes.filter((r) => r.opId === fresh.opId)).toHaveLength(1);
    expect(fresh.revoked).toBe(true);
    expect(e.kiln.credentials).toHaveLength(2);
  });

  it('Agent 结束、关闭工具时就关闭它还没有结束的 git 传输并等吊销完成（在校验报告之前，不等到提交结果）', async () => {
    let opened: ReturnType<Attempt['openTransport']> | undefined;
    const { e, s } = await httpEnv(async (ctx) => {
      opened = at.openTransport(s.url('acme/widgets'), '1');
      const r = await ctx.tool('git-credential', { op: opened.op, protocol: 'http', host: s.host, path: 'acme/widgets.git' });
      expect(r.output).toContain('password=');
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      // 传输还没有结束（包装的连接还开着）时 Agent 就结束了
      ctx.done();
    });
    let revokedAtValidate: boolean | undefined;
    e.kiln.validateFn = () => {
      revokedAtValidate = e.kiln.credentials.find((c) => c.opId === opened?.op)?.revoked;
      return { valid: true };
    };
    const sha = await e.origin.branchSha('master');
    const task = reviewTask(e.origin, { sha, baseSha: sha });
    task.cloneUrl = s.url('acme/widgets');
    const at = new Attempt(e.deps, task);
    await run(e, at);
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
    expect(revokedAtValidate).toBe(true);
    // 之后包装的连接才关闭：不再吊销第二次
    await opened?.onClose?.();
    expect(e.kiln.revokes.filter((r) => r.opId === opened?.op)).toHaveLength(1);
  });

  it('要不到凭据时准备失败（infra.credential），错误说明中的密钥被替换', async () => {
    const e = await makeEnv(async (ctx) => ctx.done());
    env = e;
    const leaked = 'ghs_leakedSecretValue123';
    e.deps.redactor.add(leaked, Date.now() + 3_600_000);
    e.kiln.credentialFn = () => new Error(`Server 出错（${leaked}）`);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    const c = e.kiln.completes[0];
    expect(c?.outcome).toBe(Outcome.INFRA_ERROR);
    expect(c?.errorClass).toBe(ErrorClass.credential);
    expect(c?.message).toContain(redactedMark);
    expect(c?.message).not.toContain(leaked);
    expect(e.orca.starts).toEqual([]);
    // 结果不明的失败：每次都是新的操作（同一 op_id 不重放），最多 credentialAttempts 次；每个发出过签发请求的操作都按原
    // op_id 关闭一次（只带 op_id）
    const ops = e.kiln.issues.map((r) => r.opId);
    expect(ops).toHaveLength(credentialAttempts);
    expect(new Set(ops).size).toBe(credentialAttempts);
    expect(e.kiln.revokes.map((r) => [r.opId, r.secret])).toEqual(ops.map((op) => [op, '']));
  });
  it('签发结果不明（ABORTED，Server 已关闭这个操作）：同一操作不重试，准备失败；关闭时只带 op_id', async () => {
    const e = await makeEnv(async (ctx) => ctx.done());
    env = e;
    e.kiln.credentialFn = () => new ConnectError('GitHub 签发 token 的结果不明，这个操作已经关闭', Code.Aborted);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    const c = e.kiln.completes[0];
    expect(c?.outcome).toBe(Outcome.INFRA_ERROR);
    expect(c?.errorClass).toBe(ErrorClass.credential);
    expect(e.kiln.issues).toHaveLength(1);
    expect(e.kiln.revokes.map((r) => [r.opId, r.secret])).toEqual([[e.kiln.issues[0]?.opId, '']]);
  });
  it('签发 INTERNAL（Server 已关闭这个操作）：这个操作结束、按原 op_id 关闭，不重放；准备仓库换新的操作重来成功', async () => {
    const e = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    env = e;
    const replies: Array<Error | undefined> = [wireError('记录签发 git 凭据的审计日志失败', Code.Internal)];
    e.kiln.credentialFn = () => replies.shift();
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
    const [first, second] = e.kiln.issues.map((r) => r.opId);
    expect(e.kiln.issues).toHaveLength(2);
    expect(second).not.toBe(first);
    expect(e.kiln.revokes.find((r) => r.opId === first)).toMatchObject({ secret: '' });
  });

  it('代理的普通 401（客户端合成的 UNAUTHENTICATED，签发可能已经执行）：不重放，原操作按 op_id 关闭，准备仓库换新的操作重来', async () => {
    const e = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    env = e;
    let lost = 1;
    // Server 已经签出、代理回复普通的 401：ConnectKiln.call 不重放，错误原样交回
    e.kiln.credentialFn = () => (lost-- > 0 ? new ConnectError('HTTP 401', Code.Unauthenticated) : undefined);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
    const [first, second] = e.kiln.issues.map((r) => r.opId);
    expect(e.kiln.issues).toHaveLength(2);
    expect(second).not.toBe(first);
    expect(e.kiln.revokes.filter((r) => r.opId === first).map((r) => r.secret)).toEqual(['']);
  });

  it('Server 明确回答没有签发（Server 发来的 UNAVAILABLE）：同一 op_id 重试，拿到之后照常', async () => {
    const e = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    env = e;
    const replies: Array<Error | undefined> = [wireError('GitHub 确定没有签发', Code.Unavailable)];
    e.kiln.credentialFn = () => replies.shift();
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
    const op = e.kiln.issues[0]?.opId;
    expect(e.kiln.issues.map((r) => r.opId)).toEqual([op, op]);
    expect(e.kiln.credentials).toHaveLength(1);
  });

  it('客户端合成的 UNAVAILABLE（网关 502 等，没有到达 Server）同样不重放：换新的操作', async () => {
    const e = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    env = e;
    const replies: Array<Error | undefined> = [new ConnectError('HTTP 502', Code.Unavailable)];
    e.kiln.credentialFn = () => replies.shift();
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
    const ops = e.kiln.issues.map((r) => r.opId);
    expect(ops).toHaveLength(2);
    expect(ops[1]).not.toBe(ops[0]);
  });

  it('读：准备仓库的签发回复丢失（Server 已签出）：同一 op_id 不出现第二次签发，原操作按 op_id 关闭（Server 吊销已签出的），用新的操作重来成功', async () => {
    const e = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    env = e;
    let lost = 1;
    e.kiln.credentialLost = () => lost-- > 0;
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
    const [first, second] = e.kiln.credentials;
    expect(e.kiln.issues.filter((r) => r.opId === first?.opId)).toHaveLength(1);
    expect(second?.opId).not.toBe(first?.opId);
    expect(first?.revoked).toBe(true);
    expect(e.kiln.revokes.filter((r) => r.opId === first?.opId).map((r) => r.secret)).toEqual(['']);
    expect(second?.revoked).toBe(true);
  });
});

describe('准备仓库的签发随准备阶段的停止结束', () => {
  /** spyPrepare 记下 prepareClone 的调用次数（网络 git）。 */
  function spyPrepare(e: Env): () => number {
    let n = 0;
    const orig = e.deps.git.prepareClone.bind(e.deps.git);
    e.deps.git.prepareClone = (...args) => {
      n++;
      return orig(...args);
    };
    return () => n;
  }

  it('签发在途时连接器退出、Server 回复 INTERNAL：不再开新的操作（只有 1 个 op_id），原操作按 op_id 关闭，不启动 Agent', async () => {
    const e = await makeEnv(() => Promise.resolve());
    env = e;
    const prepared = spyPrepare(e);
    // 回调在 at 建好之后才会被调用
    // eslint-disable-next-line prefer-const
    let at!: Attempt;
    let first = true;
    e.kiln.credentialFn = () => {
      if (first) {
        first = false;
        at.shutdown();
      }
      return wireError('临时登记失败', Code.Internal);
    };
    const sha = await e.origin.branchSha('master');
    at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    const ops = new Set(e.kiln.issues.map((r) => r.opId));
    expect(ops.size).toBe(1);
    expect(e.kiln.revokes.map((r) => r.opId)).toEqual([...ops]);
    expect(prepared()).toBe(0);
    expect(e.orca.starts).toEqual([]);
  });

  it('退避等待期间连接器退出：立即返回（不等退避结束）、不再签发，仓库锁随之释放', async () => {
    const e = await makeEnv(() => Promise.resolve());
    env = e;
    // 退避很长：没有被打断时测试会超时
    e.deps.timing = { ...e.deps.timing, credentialRetryMs: 600_000 };
    e.kiln.credentialFn = () => wireError('临时登记失败', Code.Internal);
    const sha = await e.origin.branchSha('master');
    const task = reviewTask(e.origin, { sha, baseSha: sha });
    const at = new Attempt(e.deps, task);
    const running = run(e, at);
    // 第一个操作已经关闭（之后进入退避）
    await waitFor(() => e.kiln.revokes.length === 1, 10_000, '第一个操作关闭');
    expect(e.deps.repoLock.busy(String(task.repoId))).toBe(true);
    const t0 = Date.now();
    at.shutdown();
    await running;
    expect(Date.now() - t0).toBeLessThan(5_000);
    expect(e.kiln.issues).toHaveLength(1);
    expect(e.deps.repoLock.busy(String(task.repoId))).toBe(false);
  });

  it('Server 已经签出（凭据在返回途中）时连接器退出：不执行网络 git，凭据被关闭', async () => {
    const e = await makeEnv(() => Promise.resolve());
    env = e;
    const prepared = spyPrepare(e);
    // 回调在 at 建好之后才会被调用
    // eslint-disable-next-line prefer-const
    let at!: Attempt;
    e.kiln.credentialFn = () => {
      at.shutdown();
      return undefined;
    };
    const sha = await e.origin.branchSha('master');
    at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(e.kiln.credentials).toHaveLength(1);
    expect(e.kiln.credentials[0]?.revoked).toBe(true);
    expect(e.kiln.issues).toHaveLength(1);
    expect(prepared()).toBe(0);
  });

  it('交付不接收准备阶段的停止：推送用的签发进行中连接器退出，照常推完 attempt 分支、吊销写凭据', async () => {
    // 回调在 at 建好之后才会被调用
    // eslint-disable-next-line prefer-const
    let at!: Attempt;
    const { e, s } = await httpEnv(async (ctx) => {
      await writeFile(path.join(ctx.worktree, 'feature.go'), 'package x\n');
      await git(ctx.worktree, 'add', '-A');
      await git(ctx.worktree, 'commit', '--quiet', '-m', 'feat');
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      ctx.done();
    });
    e.kiln.credentialHold = (req) => {
      if (req.access === CredentialAccess.WRITE) at.shutdown();
      return Promise.resolve();
    };
    const sha = await e.origin.branchSha('master');
    const task = devTask(e.origin, { sha, mode: 'implement' });
    task.cloneUrl = s.url('acme/widgets');
    at = new Attempt(e.deps, task);
    await run(e, at);
    const write = e.kiln.credentials.find((c) => c.access === CredentialAccess.WRITE);
    expect(write?.revoked).toBe(true);
    expect(s.requests.some((r) => r.write && r.password === write?.secret)).toBe(true);
    expect(await e.origin.branchSha(`kiln/attempt-${at.id}`)).toMatch(/^[0-9a-f]{40}$/);
  });
});

describe('准备仓库时 clone 本地配置的读写有期限', () => {
  it('git config --get-all 卡住：到期准备失败，读操作关闭（凭据吊销），仓库锁释放', async () => {
    const e = await makeEnv(() => Promise.resolve());
    env = e;
    // 配置查询一直不返回（例如配置文件在卡住的文件系统上）；git 的期限缩短到 300 毫秒
    const hang: Exec = (cmd, args, o) =>
      args.includes('--get-all') ? nodeExec('sleep', ['60'], { timeoutMs: o?.timeoutMs, killGraceMs: 100, pipeDrainMs: 100 }) : nodeExec(cmd, args, o);
    e.deps.git = new Git(hang, 300);
    const sha = await e.origin.branchSha('master');
    const task = reviewTask(e.origin, { sha, baseSha: sha });
    const at = new Attempt(e.deps, task);
    const t0 = Date.now();
    await run(e, at);
    expect(Date.now() - t0).toBeLessThan(20_000);
    const c = e.kiln.completes[0];
    expect(c?.outcome).toBe(Outcome.INFRA_ERROR);
    expect(e.kiln.credentials).toHaveLength(1);
    expect(e.kiln.credentials[0]?.revoked).toBe(true);
    expect(e.deps.repoLock.busy(String(task.repoId))).toBe(false);
    expect(e.orca.starts).toEqual([]);
  });
});

describe('辅助路径的上限与期限', () => {
  it('readBounded：helper 的 stdin 超过上限时停止读取、不给凭据；正常的请求照常', async () => {
    async function* chunks(...xs: string[]) {
      for (const x of xs) yield Buffer.from(x);
    }
    expect(await readBounded(chunks('protocol=https\n', 'host=github.com\n\n'), maxCredentialQueryBytes)).toBe('protocol=https\nhost=github.com\n\n');
    expect(await readBounded(chunks('x'.repeat(maxCredentialQueryBytes), 'y'), maxCredentialQueryBytes)).toBeUndefined();
  });

  it('realExecPath：git --exec-path 带期限', async () => {
    const seen: Array<number | undefined> = [];
    await realExecPath((_c, _a, o) => (seen.push(o?.timeoutMs), Promise.resolve({ code: 0, stdout: '/usr/lib/git-core\n', stderr: '' })), {});
    expect(seen).toEqual([authQueryTimeoutMs]);
  });
});

describe('GitOps', () => {
  function fake() {
    const issued: Array<{ access: CredentialAccess; opId: string; repository: string; secret: string }> = [];
    const revoked: Array<{ opId: string; secret: string }> = [];
    const lines: Array<[string, string, Record<string, unknown> | undefined]> = [];
    const printed: string[] = [];
    let n = 0;
    let fail: Error | undefined;
    let issueFail: Error | undefined;
    let hold: Promise<void> | undefined;
    let revokeHold: Promise<void> | undefined;
    const log: Logger = { info: (m, f) => lines.push(['info', m, f]), warn: (m, f) => lines.push(['warn', m, f]), error: (m, f) => lines.push(['error', m, f]) };
    const ops = new GitOps({
      issue: async ({ access, opId, repository }) => {
        await hold;
        await Promise.resolve();
        if (issueFail) throw issueFail;
        const secret = `ghs_secret_${++n}_abcdef`;
        issued.push({ access, opId, repository, secret });
        return create(CredentialSchema, { access, username: 'x-access-token', secret, expiresAt: BigInt(Math.floor(Date.now() / 1000) + 3600), scopes: [repository] }) as Credential;
      },
      revoke: async (r) => {
        revoked.push(r);
        await revokeHold;
        if (fail) throw fail;
      },
      redactor: new Redactor(),
      log,
      print: (l) => printed.push(l),
      attemptId: '7',
    });
    return {
      ops,
      issued,
      revoked,
      lines,
      printed,
      setFail: (e: Error | undefined) => (fail = e),
      setIssueFail: (e: Error | undefined) => (issueFail = e),
      setHold: (p: Promise<void> | undefined) => (hold = p),
      setRevokeHold: (p: Promise<void> | undefined) => (revokeHold = p),
    };
  }

  it('一个操作只签一枚（并发的请求只签发一次）；关闭时带 op_id 和凭据吊销，之后不再给出；签发、吊销成对记日志（指纹，不含凭据）', async () => {
    const f = fake();
    const op = f.ops.open('acme/lib', CredentialAccess.READ);
    expect(f.ops.get(op.id)).toBe(op);
    const [a, b] = await Promise.all([op.token(), op.token()]);
    expect(a).toBe(b);
    expect(await op.token()).toBe(a);
    expect(f.issued).toEqual([{ access: CredentialAccess.READ, opId: op.id, repository: 'acme/lib', secret: a.secret }]);
    await op.close();
    expect(f.revoked).toEqual([{ opId: op.id, secret: a.secret }]);
    expect(op.open).toBe(false);
    expect(f.ops.get(op.id)).toBeUndefined();
    await expect(op.token()).rejects.toBeInstanceOf(OpClosedError);
    await op.close();
    expect(f.revoked).toHaveLength(1);
    const fp = tokenFingerprint(a.secret);
    expect(fp).toMatch(/^sha256:[0-9a-f]{12}$/);
    const pair = f.lines.map(([, m, x]) => [m, x]);
    expect(pair).toEqual([
      ['签发 git 凭据', { attempt: '7', op: op.id, repo: 'acme/lib', access: '读', token: fp }],
      ['已请求 Server 关闭 git 操作、回收凭据', { attempt: '7', op: op.id, repo: 'acme/lib', access: '读', token: fp }],
    ]);
    expect(f.printed.every((l) => l.includes(op.id) && l.includes(fp))).toBe(true);
    // 关闭的 RPC 成功只说明 Server 关闭了操作（GitHub 侧的吊销可能失败，只在 Server 日志中有 warn）：任务日志不写成已吊销
    expect(f.printed.join('\n')).not.toMatch(/(^|[：，\s])吊销 git|已吊销/);
    expect(JSON.stringify([f.lines, f.printed])).not.toContain(a.secret);
  });

  it('已经开始的关闭（传输连接先断开）：立即查不到这个操作，closeAll、settle 等它的吊销结束才返回；重复关闭不重复吊销', async () => {
    const f = fake();
    const op = f.ops.open('acme/widgets', CredentialAccess.READ);
    const c = await op.token();
    let release!: () => void;
    f.setRevokeHold(new Promise((r) => (release = r)));
    // 与 socket 的 onClose 相同：不等待
    void op.close();
    expect(f.ops.get(op.id)).toBeUndefined();
    let done = false;
    const all = f.ops.closeAll().then(() => (done = true));
    const settled = f.ops.settle();
    await new Promise((r) => setImmediate(r));
    expect(f.revoked).toEqual([{ opId: op.id, secret: c.secret }]);
    expect(done).toBe(false);
    release();
    await all;
    await settled;
    expect(done).toBe(true);
    await op.close();
    await f.ops.closeAll();
    expect(f.revoked).toHaveLength(1);
  });

  it('closeAll(READ) 只关读操作（写操作照常），并等已经开始的关闭', async () => {
    const f = fake();
    const r = f.ops.open('acme/widgets', CredentialAccess.READ);
    const w = f.ops.open('acme/widgets', CredentialAccess.WRITE);
    await Promise.all([r.token(), w.token()]);
    await f.ops.closeAll(CredentialAccess.READ);
    expect(r.open).toBe(false);
    expect(w.open).toBe(true);
    expect(f.revoked.map((x) => x.opId)).toEqual([r.id]);
    await w.close();
  });

  it('没有要过凭据的操作关闭时不调用 Server；不同操作各自签发、各自吊销', async () => {
    const f = fake();
    const quiet = f.ops.open('acme/widgets', CredentialAccess.READ);
    await quiet.close();
    expect(f.revoked).toEqual([]);
    const x = f.ops.open('acme/widgets', CredentialAccess.READ);
    const y = f.ops.open('acme/widgets', CredentialAccess.READ);
    const [cx, cy] = await Promise.all([x.token(), y.token()]);
    expect(cx.secret).not.toBe(cy.secret);
    await x.close();
    expect(f.revoked).toEqual([{ opId: x.id, secret: cx.secret }]);
    expect(await y.token()).toBe(cy);
    await f.ops.closeAll();
    expect(f.revoked).toEqual([
      { opId: x.id, secret: cx.secret },
      { opId: y.id, secret: cy.secret },
    ]);
  });

  it('签发在途时关闭：等待中的请求拿不到，签发回来的凭据立即吊销（带 op_id 和凭据）', async () => {
    const f = fake();
    let release!: () => void;
    f.setHold(new Promise((r) => (release = r)));
    const op = f.ops.open('acme/widgets', CredentialAccess.READ);
    const pending = op.token();
    const closing = op.close();
    release();
    await expect(pending).rejects.toBeInstanceOf(OpClosedError);
    await closing;
    expect(f.issued).toHaveLength(1);
    expect(f.revoked).toEqual([{ opId: op.id, secret: f.issued[0]!.secret }]);
  });

  it('签发失败：同一操作不再重试；关闭时只带 op_id 请 Server 关闭（它可能已经签发）', async () => {
    const f = fake();
    f.setIssueFail(new Error('结果不明'));
    const op = f.ops.open('acme/widgets', CredentialAccess.READ);
    await expect(op.token()).rejects.toThrow('结果不明');
    f.setIssueFail(undefined);
    await expect(op.token()).rejects.toThrow('结果不明');
    await op.close();
    expect(f.issued).toEqual([]);
    expect(f.revoked).toEqual([{ opId: op.id, secret: '' }]);
  });

  it('run：结束后（包括失败）关闭；吊销失败记 warn（带过期时间），失效时不记 warn', async () => {
    const f = fake();
    expect(await f.ops.run('acme/widgets', CredentialAccess.WRITE, async (op) => (await op.token()).secret)).toBe(f.issued[0]?.secret);
    await expect(
      f.ops.run('acme/widgets', CredentialAccess.WRITE, async (op) => {
        await op.token();
        throw new Error('推送失败');
      }),
    ).rejects.toThrow('推送失败');
    expect(f.issued.map((x) => x.access)).toEqual([CredentialAccess.WRITE, CredentialAccess.WRITE]);
    expect(f.revoked.map((x) => x.secret)).toEqual(f.issued.map((x) => x.secret));
    f.setFail(new Error('网络错误'));
    await f.ops.run('acme/widgets', CredentialAccess.WRITE, (op) => op.token());
    const warns = f.lines.filter(([l]) => l === 'warn');
    expect(warns).toHaveLength(1);
    expect(warns[0]?.[2]?.expiresAt).toMatch(/^\d{4}-\d\d-\d\dT/);
    expect(warns[0]?.[2]?.access).toBe('写');
    f.setFail(new StaleError());
    await f.ops.run('acme/widgets', CredentialAccess.WRITE, (op) => op.token());
    expect(f.lines.filter(([l]) => l === 'warn')).toHaveLength(1);
    expect(f.lines.at(-1)?.[1]).toContain('attempt 已经失效');
  });
});

describe('凭据相关的纯函数', () => {
  it('networkEnv：不读全局、系统配置，清空 helper，凭据作为限定在仓库地址上的请求头（不在参数中）', () => {
    const e = networkEnv('https://github.com/acme/widgets.git', { username: 'x-access-token', secret: 'ghs_abc12345' });
    expect(e).toMatchObject({ GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1', GIT_TERMINAL_PROMPT: '0', HOME: '/dev/null', GIT_CONFIG_PARAMETERS: undefined, GIT_ASKPASS: undefined });
    const cfg = Object.fromEntries(Array.from({ length: Number(e.GIT_CONFIG_COUNT) }, (_, i) => [e[`GIT_CONFIG_KEY_${i}`], e[`GIT_CONFIG_VALUE_${i}`]]));
    expect(cfg['credential.helper']).toBe('');
    expect(cfg['protocol.ssh.allow']).toBe('never');
    expect(cfg['http.https://github.com/acme/widgets.git.extraHeader']).toBe(`Authorization: Basic ${basicAuth('x-access-token', 'ghs_abc12345')}`);
    // 本地路径（测试）不带凭据
    expect(Object.values(networkEnv('/tmp/x.git', { username: 'u', secret: 'ghs_abc12345' })).join('\n')).not.toContain(basicAuth('u', 'ghs_abc12345'));
  });

  it('networkEnv / localGitEnv：清理外层 task exec 的私有变量、环境变量配置注入与仓库上下文，网络 git 另加隔离项', () => {
    const base: Record<string, string | undefined> = {
      PATH: '/usr/bin',
      HOME: '/home/u',
      GIT_CONFIG_GLOBAL: '/home/u/.gitconfig',
      GIT_EXEC_PATH: '/outer/mirror',
      KILN_ORCA_GIT_OP: 'op',
      KILN_GIT_OP: 'op2',
      KILN_GIT_OP_ERROR: 'err',
      KILN_GIT_SOCKET: '/sock',
      KILN_GIT_EXEC_REAL: '/real',
      KILN_GIT_CONFIG_N: '4',
      KILN_GIT_CONFIG_KEY_0: 'a.b',
      KILN_GIT_CONFIG_VALUE_0: 'v',
      GIT_CONFIG: '/f',
      GIT_CONFIG_PARAMETERS: "'a.b'='v'",
      GIT_CONFIG_COUNT: '9',
      GIT_CONFIG_KEY_9: 'x.y',
      GIT_CONFIG_VALUE_9: 'z',
      GIT_DIR: '/g',
      GIT_WORK_TREE: '/w',
      GIT_COMMON_DIR: '/c',
      GIT_INDEX_FILE: '/i',
      GIT_OBJECT_DIRECTORY: '/o',
      GIT_ALTERNATE_OBJECT_DIRECTORIES: '/ao',
      GIT_SSL_NO_VERIFY: '1',
      NETRC: '/n',
      GIT_ASKPASS: '/ap',
      SSH_ASKPASS: '/sap',
    };
    // base 的快照：networkEnv / localGitEnv 是纯函数，不能修改入参。
    const snapshot: Record<string, string | undefined> = { ...base };
    // 必须删除的静态键：cleanupOverlay 不论 base 中有没有都给出 undefined，nodeExec 合并时才会把它从继承的环境里删掉。
    const staticPolluted = [
      'GIT_EXEC_PATH', 'KILN_ORCA_GIT_OP', 'KILN_GIT_OP', 'KILN_GIT_OP_ERROR', 'KILN_GIT_SOCKET', 'KILN_GIT_EXEC_REAL', 'KILN_GIT_CONFIG_N',
      'GIT_CONFIG', 'GIT_CONFIG_PARAMETERS',
      'GIT_DIR', 'GIT_WORK_TREE', 'GIT_COMMON_DIR', 'GIT_INDEX_FILE', 'GIT_OBJECT_DIRECTORY', 'GIT_ALTERNATE_OBJECT_DIRECTORIES',
      'GIT_ASKPASS', 'SSH_ASKPASS',
    ];
    // 按前缀匹配、base 中实际出现的高编号键（编号不定，不能列举）。
    const dynamicPolluted = ['KILN_GIT_CONFIG_KEY_0', 'KILN_GIT_CONFIG_VALUE_0', 'GIT_CONFIG_KEY_9', 'GIT_CONFIG_VALUE_9'];
    // 只断言 overlay[k] === undefined 是不够的：键整个缺失（nodeExec 不会删父变量）时同样通过；必须同时断言键存在。
    const mustDelete = (overlay: Record<string, string | undefined>, keys: readonly string[], label: string) => {
      for (const k of keys) {
        expect(Object.hasOwn(overlay, k), `${label}：${k} 必须显式给出 undefined`).toBe(true);
        expect(overlay[k], `${label}：${k}`).toBeUndefined();
      }
    };
    // 模拟 nodeExec 的合并语义：以 base 为基础，overlay 中值为 undefined 的键删除、其余覆盖。
    const merged = (overlay: Record<string, string | undefined>): Record<string, string | undefined> => {
      const out: Record<string, string | undefined> = { ...base };
      for (const [k, v] of Object.entries(overlay)) {
        if (v === undefined) delete out[k];
        else out[k] = v;
      }
      return out;
    };

    const n = networkEnv('https://github.com/acme/widgets.git', undefined, base);
    mustDelete(n, [...staticPolluted, ...dynamicPolluted, 'GIT_SSL_NO_VERIFY', 'NETRC'], 'networkEnv');
    // 纯函数：不修改入参 base
    expect(base).toEqual(snapshot);
    // 合并之后，污染键在真正交给子进程的环境里都已不存在
    const nm = merged(n);
    for (const k of [...staticPolluted, ...dynamicPolluted, 'GIT_SSL_NO_VERIFY', 'NETRC']) expect(Object.hasOwn(nm, k), `networkEnv 合并后 ${k}`).toBe(false);
    // 本次的配置从 0 开始重写覆盖低编号，高编号（base 中的 KEY_9）仍然被清掉
    expect(nm.GIT_CONFIG_COUNT).toBe('6');
    expect(nm.GIT_CONFIG_KEY_0).toBe('credential.helper');
    expect(nm.GIT_CONFIG_KEY_9).toBeUndefined();
    expect(nm.GIT_CONFIG_VALUE_9).toBeUndefined();
    // 网络 git 的隔离项照旧
    expect(n).toMatchObject({ GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_SYSTEM: undefined, HOME: '/dev/null', GIT_TERMINAL_PROMPT: '0', XDG_CONFIG_HOME: undefined });

    // 本地 git：清掉同样的污染键（以及被重设为 0 的 GIT_TERMINAL_PROMPT），但保留 PATH / HOME / 用户的全局配置（照常读机器上的配置；不设 GIT_CONFIG_GLOBAL=/dev/null）
    const l = localGitEnv(base);
    mustDelete(l, [...staticPolluted, ...dynamicPolluted, 'GIT_CONFIG_COUNT'], 'localGitEnv');
    // 刻意保留的键不在 overlay 中（继承机器的配置），nodeExec 合并后仍来自 base
    for (const k of ['GIT_CONFIG_GLOBAL', 'HOME', 'PATH', 'GIT_SSL_NO_VERIFY', 'NETRC']) {
      expect(Object.hasOwn(l, k), `localGitEnv 刻意保留 ${k}`).toBe(false);
    }
    expect(l.GIT_TERMINAL_PROMPT).toBe('0');
    expect(base).toEqual(snapshot);
    const lm = merged(l);
    for (const k of [...staticPolluted, ...dynamicPolluted, 'GIT_CONFIG_COUNT']) expect(Object.hasOwn(lm, k), `localGitEnv 合并后 ${k}`).toBe(false);
    expect(lm).toMatchObject({ GIT_CONFIG_GLOBAL: '/home/u/.gitconfig', HOME: '/home/u', PATH: '/usr/bin', GIT_TERMINAL_PROMPT: '0' });
    // 纯函数：不给 base 时只产出固定项，不隐式读取 process.env
    expect(networkEnv('https://github.com/a/b.git').GIT_EXEC_PATH).toBeUndefined();
  });

  it('execGitEnv：接在已有的环境变量配置之后，先清空再加 helper，askpass 关掉，GIT_EXEC_PATH 指向镜像，GOPRIVATE 保留原值', () => {
    const g = execGitOf('https://GitHub.com/acme/widgets.git', ['Acme/Lib'], '/t/git-exec');
    expect(g).toEqual({ helperURL: 'https://github.com', goprivate: ['github.com/[aA][cC][mM][eE]/[lL][iI][bB]'], execRoot: '/t/git-exec' });
    const e = execGitEnv({ GIT_CONFIG_COUNT: '1', GIT_CONFIG_KEY_0: 'a.b', GIT_CONFIG_VALUE_0: 'c', GOPRIVATE: 'corp.example' }, g!, "'kiln-orca' task --attempt 5", '/t/git-exec/abc');
    expect(e).toEqual({
      GIT_CONFIG_COUNT: '5',
      GIT_CONFIG_KEY_1: 'credential.https://github.com.helper',
      GIT_CONFIG_VALUE_1: '',
      GIT_CONFIG_KEY_2: 'credential.https://github.com.helper',
      GIT_CONFIG_VALUE_2: "!'kiln-orca' task --attempt 5 git-credential",
      GIT_CONFIG_KEY_3: 'credential.https://github.com.useHttpPath',
      GIT_CONFIG_VALUE_3: 'true',
      GIT_CONFIG_KEY_4: 'core.askPass',
      GIT_CONFIG_VALUE_4: '',
      GIT_EXEC_PATH: '/t/git-exec/abc',
      GIT_TERMINAL_PROMPT: '0',
      GIT_ASKPASS: undefined,
      SSH_ASKPASS: undefined,
      KILN_ORCA_GIT_OP: undefined,
      GOPRIVATE: 'corp.example,github.com/[aA][cC][mM][eE]/[lL][iI][bB]',
    });
    expect(execGitEnv({}, { helperURL: 'https://github.com', goprivate: [], execRoot: '/t' }, 'k', '/t/x').GOPRIVATE).toBeUndefined();
    expect(execGitOf('/tmp/origin.git', [], '/t')).toBeNull();
  });

  it('goPrivatePattern：owner/name 的字母不区分大小写，其它字符按字面，主机名小写，glob 的特殊字符转义', () => {
    expect(goPrivatePattern('GitHub.com', 'Acme-1/lib.go_x')).toBe('github.com/[aA][cC][mM][eE]-1/[lL][iI][bB].[gG][oO]_[xX]');
    expect(goPrivatePattern('h:8080', 'a/b')).toBe('h:8080/[aA]/[bB]');
    expect(goPrivatePattern('h', 'o*/n?[x]\\')).toBe('h/[oO]\\*/[nN]\\?\\[[xX]\\]\\\\');
  });

  it('helper、传输只认任务仓库和声明的依赖仓库（同一个协议和主机），返回声明的写法', () => {
    const repos = ['acme/widgets', 'Acme/Lib'];
    const q = (p: string, host = 'github.com', protocol = 'https') => credentialAllowed({ protocol, host, path: p }, 'https://github.com/acme/widgets.git', repos);
    expect(q('acme/widgets.git')).toBe('acme/widgets');
    expect(q('acme/widgets')).toBe('acme/widgets');
    expect(q('acme/lib.git')).toBe('Acme/Lib');
    expect(q('acme/other.git')).toBe('');
    expect(q('acme/widgets.git/extra')).toBe('');
    expect(q('')).toBe('');
    expect(q('acme/widgets.git', 'evil.example')).toBe('');
    expect(q('acme/widgets.git', 'github.com', 'http')).toBe('');
    const t = (url: string) => transportRepo(url, 'https://github.com/acme/widgets.git', repos);
    expect(t('https://github.com/acme/widgets.git')).toBe('acme/widgets');
    expect(t('https://GITHUB.com/ACME/lib')).toBe('Acme/Lib');
    expect(t('https://github.com/acme/other.git')).toBe('');
    expect(t('http://github.com/acme/widgets.git')).toBe('');
    expect(t('https://u:p@github.com/acme/widgets.git')).toBe('');
    expect(t('https://github.com/acme/%E0%A4%A.git')).toBe('');
    expect(t('git@github.com:acme/widgets.git')).toBe('');
    // 路径穿越、百分号编码：WHATWG URL 会归一化掉，git、curl 的处理不同，一律不给凭据
    expect(t('https://github.com/x/../acme/widgets.git')).toBe('');
    expect(t('https://github.com/acme/./widgets.git')).toBe('');
    expect(t('https://github.com/acme/%77idgets.git')).toBe('');
    expect(t('https://github.com/acme\\widgets.git')).toBe('');
    // 显式的默认端口与不写端口相同
    expect(t('https://github.com:443/acme/widgets.git')).toBe('acme/widgets');
    expect(t('https://github.com:8443/acme/widgets.git')).toBe('');
    expect(repoOfPath('/acme/widgets.git/')).toBe('acme/widgets');
    expect(parseCredentialQuery('protocol=https\nhost=github.com\npath=acme/lib.git\n\nignored=1\n')).toEqual({ protocol: 'https', host: 'github.com', path: 'acme/lib.git' });
  });

  it('Redactor 替换密钥和它的 Basic 认证形式；过期很久的不再记着；redactingLogger 替换消息和字段', () => {
    const now = { t: 0 };
    const r = new Redactor(() => now.t);
    r.add('ghs_tokenvalue1', 1_000, 'x-access-token');
    const b = basicAuth('x-access-token', 'ghs_tokenvalue1');
    expect(r.redact(`a ghs_tokenvalue1 b Authorization: Basic ${b}`)).toBe(`a ${redactedMark} b Authorization: Basic ${redactedMark}`);
    const lines: Array<[string, Record<string, unknown> | undefined]> = [];
    const log = redactingLogger({ info: (m, f) => lines.push([m, f]), warn: () => undefined, error: () => undefined }, r);
    log.info('x ghs_tokenvalue1', { err: 'y ghs_tokenvalue1', e: new Error('z ghs_tokenvalue1'), n: 1 });
    expect(lines).toEqual([[`x ${redactedMark}`, { err: `y ${redactedMark}`, e: `z ${redactedMark}`, n: 1 }]]);
    now.t = 10 * 60 * 60_000;
    r.add('ghs_other_token', now.t + 1_000);
    expect(r.redact('ghs_tokenvalue1')).toBe('ghs_tokenvalue1');
    // 字段中的数组、嵌套对象逐层替换；类实例（Map、Buffer）不按字段遍历，原样保留（审计）
    const fields: Array<Record<string, unknown> | undefined> = [];
    const deep = redactingLogger({ info: (_m, f) => fields.push(f), warn: () => undefined, error: () => undefined }, r);
    const map = new Map([['k', 'ghs_other_token']]);
    deep.info('m', { list: ['a ghs_other_token', { deep: 'ghs_other_token' }], map, n: 1 });
    expect(fields[0]).toEqual({ list: [`a ${redactedMark}`, { deep: redactedMark }], map, n: 1 });
    expect((fields[0]?.map as Map<string, string>).get('k')).toBe('ghs_other_token');
  });

  it('redactingLogger 在循环引用处结束（不会无限递归），同一个对象在兄弟位置出现时照常各展开一次', () => {
    const r = new Redactor(() => 0);
    r.add('ghs_circular_token', 1_000);
    const fields: Array<Record<string, unknown> | undefined> = [];
    const log = redactingLogger({ info: (_m, f) => fields.push(f), warn: () => undefined, error: () => undefined }, r);
    const shared = { s: 'ghs_circular_token' };
    const cyclic: Record<string, unknown> = { s: 'ghs_circular_token', list: [] };
    (cyclic.list as unknown[]).push(cyclic);
    cyclic.self = cyclic;
    log.info('c', { cyclic, a: shared, b: shared });
    // 循环引用（自己、经由数组绕回来）替换为标记；兄弟位置的同一个对象不是循环，各展开一次
    expect(fields[0]).toEqual({
      cyclic: { s: redactedMark, list: [circularMark], self: circularMark },
      a: { s: redactedMark },
      b: { s: redactedMark },
    });
  });
});
