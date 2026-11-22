import { existsSync } from 'node:fs';
import { mkdir, readFile, mkdtemp, realpath, rm, stat, symlink, writeFile } from 'node:fs/promises';
import http from 'node:http';
import { createServer as createNetServer } from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { create } from '@bufbuild/protobuf';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { type Credential, CredentialAccess, CredentialSchema } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { credentialAllowed, execGitEnv, GitOps, parseCredentialQuery } from '../src/core/credentials.js';
import { ErrorClass, InfraError } from '../src/core/errors.js';
import { type Exec, ExecError, type ExecResult, exitedNormally, nodeExec, outputLimitCode, timeoutCode } from '../src/core/exec.js';
import { Git, gitAuthor, gitConfigCount, gitEmail, localGitEnv, lsRemoteRef, porcelainRejection, PushUnknownError, quitHelper } from '../src/core/git.js';
import { authConflicts, authQueryTimeoutMs, ensureExecMirror, gitEnvTrue, realExecPath, globalConfigFiles, machineAuthConflicts, maxNetrcBytes, netrcFiles, netrcMention, readNetrc, safeURL } from '../src/core/gitexec.js';
import { nopLogger } from '../src/core/log.js';
import { Redactor } from '../src/core/redact.js';
import { checkSocketPath, maxOpReplyBytes, maxSocketPathBytes, openOp, serveTools } from '../src/host/socket.js';
import { type GitHTTP, gitHTTP, type GitRequest } from './githttp.js';
import { commitFile, git, Origin, waitFor } from './harness.js';

// 连接器自己的推送（PLAN #33.13 V5）、认证冲突的检查、传输包装与连接器之间的长连接。

const cleanups: Array<() => Promise<void> | void> = [];
afterEach(async () => {
  for (const c of cleanups.splice(0).reverse()) await c();
  vi.unstubAllEnvs();
});

async function tmp(): Promise<string> {
  const d = await realpath(await mkdtemp(path.join(existsSync('/tmp') ? '/tmp' : os.tmpdir(), 'ko-')));
  cleanups.push(() => rm(d, { recursive: true, force: true }));
  return d;
}

/**
 * outerCiGitEnv 模拟真实 Kiln CI Runner（Linux OCI 容器）为 https://github.com 注入的外层环境：更具体的
 * credential.https://github.com.helper（CI 中是 `!'/kiln/bin/kiln-runner' git-credential`，回答 quit=1；这里内联同一个
 * 行为，不依赖测试机上有那个路径）会压过用例自己给的 -c / GIT_CONFIG_COUNT 配置，GIT_EXEC_PATH 指向传输包装的镜像。
 * 需要它生效的用例先经 stubOuterCiGitEnv 把它写进 process.env，测试自己运行 git 时再经 localGitEnv 清掉（与连接器的
 * 本地 git 同一组清理），否则会出现依赖外层环境的假失败（helper 没被调用，dump 从未写入）或假通过（请求根本没到模拟服务）。
 */
const outerCiGitEnv: Record<string, string> = {
  GIT_CONFIG_COUNT: '4',
  GIT_CONFIG_KEY_0: 'credential.https://github.com.helper',
  GIT_CONFIG_VALUE_0: '',
  GIT_CONFIG_KEY_1: 'credential.https://github.com.helper',
  GIT_CONFIG_VALUE_1: '!f() { echo quit=1; }; f',
  GIT_CONFIG_KEY_2: 'credential.https://github.com.useHttpPath',
  GIT_CONFIG_VALUE_2: 'true',
  GIT_CONFIG_KEY_3: 'core.askPass',
  GIT_CONFIG_VALUE_3: '',
  // 超出 GIT_CONFIG_COUNT（4）的高编号项：git 不读，但清理要按键名前缀整体删掉，不能只看 0..count-1
  GIT_CONFIG_KEY_9: 'img.stale',
  GIT_CONFIG_VALUE_9: '9',
  GIT_EXEC_PATH: '/kiln/git-exec-mirror',
};

/**
 * stubOuterCiGitEnv 把 outerCiGitEnv 真正注入 process.env（afterEach 中 unstubAllEnvs 恢复）：nodeExec 从 process.env
 * 出发、再应用用例给的 env，只把 outerCiGitEnv 交给 localGitEnv 生成删除 overlay 不会污染子进程继承的环境，清理逻辑被
 * 删掉时测试也就不会失败。注入之后，用例运行 git 时要么经 localGitEnv 清掉这些键，要么它们真的生效。
 */
function stubOuterCiGitEnv(): void {
  for (const [k, v] of Object.entries(outerCiGitEnv)) vi.stubEnv(k, v);
}

/**
 * machineEnv 是运行机器检查相关的 git 命令时的受控环境：先经 localGitEnv 去掉外层注入，再显式清掉从测试机继承的 HOME
 * 与全局 / 系统配置位置（用例按需覆盖），避免运行机的 GIT_CONFIG_GLOBAL、XDG_CONFIG_HOME、GIT_CONFIG_NOSYSTEM 等改变
 * git 实际读的配置。
 */
function machineEnv(overrides: Record<string, string | undefined>): Record<string, string | undefined> {
  return {
    ...localGitEnv(process.env),
    HOME: undefined,
    XDG_CONFIG_HOME: undefined,
    GIT_CONFIG_GLOBAL: undefined,
    GIT_CONFIG_SYSTEM: undefined,
    GIT_CONFIG_NOSYSTEM: undefined,
    LC_ALL: 'C',
    ...overrides,
  };
}

/** fakeOps 是一个 attempt 的 GitOps，凭据由测试签发、记下吊销。 */
function fakeOps() {
  const issued: Array<{ secret: string; access: CredentialAccess; revoked: boolean }> = [];
  let n = 0;
  const ops = new GitOps({
    issue: ({ access }) => {
      const secret = `ghs_push_${++n}_${Math.random().toString(36).slice(2, 10)}`;
      issued.push({ secret, access, revoked: false });
      return Promise.resolve(create(CredentialSchema, { access, username: 'x-access-token', secret, expiresAt: BigInt(Math.floor(Date.now() / 1000) + 3600) }) as Credential);
    },
    revoke: ({ secret }) => {
      const c = issued.find((x) => x.secret === secret);
      if (c) c.revoked = true;
      return Promise.resolve();
    },
    redactor: new Redactor(),
    log: nopLogger,
    print: () => undefined,
    attemptId: '9',
  });
  return { ops, issued };
}

describe('push：推送并以 ls-remote 确认远端事实，结束后写凭据都已吊销（V5）', () => {
  async function setup(o: { deny?: (r: GitRequest) => boolean; gate?: (r: GitRequest) => Promise<void> | undefined; drop?: (r: GitRequest) => boolean } = {}) {
    const root = await tmp();
    const origin = await Origin.create(root);
    const f = fakeOps();
    const s: GitHTTP = await gitHTTP(
      root,
      { 'acme/widgets': origin.bare },
      (r) => {
        const c = f.issued.find((x) => x.secret === r.password && !x.revoked);
        return !!c && (!r.write || c.access === CredentialAccess.WRITE) && !(o.deny?.(r) ?? false);
      },
      { gate: o.gate, dropResponse: o.drop },
    );
    cleanups.push(() => s.close());
    const wt = path.join(root, 'wt');
    await git(root, 'clone', '--quiet', origin.bare, wt);
    const sha = await commitFile(wt, 'x.txt', 'x\n', 'work');
    const g = new Git(nodeExec);
    const url = s.url('acme/widgets');
    const push = (signal?: AbortSignal, guard?: () => void) => f.ops.run('acme/widgets', CredentialAccess.WRITE, async (op) => g.push(wt, url, 7n, guard, await op.token(), signal));
    return { root, origin, f, s, sha, wt, push };
  }

  /** phases 把带凭据的请求按读 / 写归并成段（连续的同类请求算一段）。 */
  function phases(rs: GitRequest[]): string[] {
    const out: string[] = [];
    for (const r of rs.filter((x) => x.password !== '')) {
      const k = r.write ? '写' : '读';
      if (out.at(-1) !== k) out.push(k);
    }
    return out;
  }

  it('成功：查询 → 写入 → 再查询，全程同一枚写凭据，结束后吊销；远端已经是这个 commit 时不再推送', async () => {
    const t = await setup();
    expect(await t.push()).toEqual({ branch: 'kiln/attempt-7', sha: t.sha });
    expect(await t.origin.branchSha('kiln/attempt-7')).toBe(t.sha);
    expect(t.f.issued.map((c) => [c.access, c.revoked])).toEqual([[CredentialAccess.WRITE, true]]);
    expect(phases(t.s.requests)).toEqual(['读', '写', '读']);
    const secret = t.f.issued[0]!.secret;
    expect(t.s.requests.filter((r) => r.password !== '').every((r) => r.password === secret)).toBe(true);
    expect(t.s.requests.at(-1)).toMatchObject({ write: false, password: secret });
    const n = t.s.requests.length;
    expect(await t.push()).toEqual({ branch: 'kiln/attempt-7', sha: t.sha });
    expect(t.s.requests.slice(n).some((r) => r.write)).toBe(false);
    expect(t.f.issued.every((c) => c.revoked)).toBe(true);
  });

  it('推送成功后确认失败：结果未知（推送其实已经生效）；写凭据吊销', async () => {
    let pushed = false;
    const t = await setup({
      drop: (r) => {
        if (r.write && r.method === 'POST') pushed = true;
        return pushed && !r.write;
      },
    });
    const err = await t.push().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(PushUnknownError);
    expect((err as Error).message).toContain('结果未知');
    expect(await t.origin.branchSha('kiln/attempt-7')).toBe(t.sha);
    expect(t.f.issued.map((c) => c.revoked)).toEqual([true]);
  });

  it('推送返回成功、确认时远端却不是这个 commit：明确失败（不当作成功）', async () => {
    const t = await setup();
    // 远端在接受推送之后删掉了这个分支（post-receive 钩子）
    const hook = path.join(t.origin.bare, 'hooks', 'post-receive');
    await writeFile(hook, '#!/bin/sh\ngit update-ref -d refs/heads/kiln/attempt-7\n', { mode: 0o755 });
    const err = await t.push().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(InfraError);
    expect(err).not.toBeInstanceOf(PushUnknownError);
    expect((err as Error).message).toContain('应为');
    expect(phases(t.s.requests)).toEqual(['读', '写', '读']);
    expect(t.f.issued.map((c) => c.revoked)).toEqual([true]);
  });

  it('推送的认证被拒（403，--porcelain 中没有这个 ref 的明确拒绝）：区分不了是否被接受，按结果未知；写凭据吊销', async () => {
    const t = await setup({ deny: (r) => r.write });
    const err = await t.push().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(PushUnknownError);
    expect((err as InfraError).errorClass).toBe(ErrorClass.push);
    expect(await t.origin.branchSha('kiln/attempt-7')).toBe('');
    expect(t.f.issued.map((c) => c.revoked)).toEqual([true]);
  });

  it('远端的钩子拒绝了这个 ref（--porcelain：! … [remote rejected]）：明确失败（不是结果未知）；写凭据吊销', async () => {
    const t = await setup();
    await writeFile(path.join(t.origin.bare, 'hooks', 'pre-receive'), '#!/bin/sh\necho denied >&2\nexit 1\n', { mode: 0o755 });
    const err = await t.push().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(InfraError);
    expect(err).not.toBeInstanceOf(PushUnknownError);
    expect((err as Error).message).toContain('[remote rejected]');
    expect(await t.origin.branchSha('kiln/attempt-7')).toBe('');
    expect(t.f.issued.map((c) => c.revoked)).toEqual([true]);
  });

  /** pushReply 让 git push 以给定的结果正常退出（applied 时先真的推送）。 */
  function pushReply(applied: boolean, reply: (ref: string) => Partial<ExecResult>): Exec {
    return async (cmd, args, o) => {
      if (cmd === 'git' && args[0] === 'push') {
        const r = applied ? await nodeExec(cmd, args, o) : { code: 0, stdout: '', stderr: '' };
        const ref = String(args.at(-1)).split(':')[1] ?? '';
        return { ...r, end: 'exit' as const, ...reply(ref) };
      }
      return nodeExec(cmd, args, o);
    };
  }

  it('git push 正常退出且非零但没有明确拒绝（连接在给出最终状态之前断开：RPC failed、128）：后查为旧值时结果未知，已到位时成功', async () => {
    const rpcFailed = () => ({ code: 128, stdout: '', stderr: 'error: RPC failed; HTTP 502\nfatal: the remote end hung up unexpectedly' });
    for (const applied of [false, true]) {
      const t = await setup();
      const g = new Git(pushReply(applied, rpcFailed));
      const r = t.f.ops.run('acme/widgets', CredentialAccess.WRITE, async (op) => g.push(t.wt, t.s.url('acme/widgets'), 7n, undefined, await op.token()));
      if (applied) await expect(r).resolves.toEqual({ branch: 'kiln/attempt-7', sha: t.sha });
      else await expect(r).rejects.toBeInstanceOf(PushUnknownError);
      expect(t.f.issued.every((c) => c.revoked)).toBe(true);
    }
  });

  it('--porcelain 报这个 ref 非快进被拒（! … [rejected]）：明确失败', async () => {
    const t = await setup();
    const g = new Git(pushReply(false, (ref) => ({ code: 1, stdout: `To x\n!\t${t.sha}:${ref}\t[rejected] (non-fast-forward)\nDone\n` })));
    const err = await t.f.ops.run('acme/widgets', CredentialAccess.WRITE, async (op) => g.push(t.wt, t.s.url('acme/widgets'), 7n, undefined, await op.token())).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(InfraError);
    expect(err).not.toBeInstanceOf(PushUnknownError);
    expect((err as Error).message).toContain('[rejected] (non-fast-forward)');
  });

  it('porcelainRejection：只认这个 ref 的 ! 行且状态是 [rejected] / [remote rejected]', () => {
    const ref = 'refs/heads/kiln/attempt-7';
    expect(porcelainRejection(`To x\n!\tabc:${ref}\t[remote rejected] (pre-receive hook declined)\nDone\n`, ref)).toBe('[remote rejected] (pre-receive hook declined)');
    expect(porcelainRejection(`To x\n!\tabc:${ref}\t[remote failure] (remote failed to report status)\nDone\n`, ref)).toBe('');
    expect(porcelainRejection(`To x\n!\tabc:refs/heads/other\t[rejected] (non-fast-forward)\nDone\n`, ref)).toBe('');
    expect(porcelainRejection(`To x\n*\tabc:${ref}\t[new branch]\nDone\n`, ref)).toBe('');
    expect(porcelainRejection('', ref)).toBe('');
  });

  it('推送的响应丢失：推送已经生效，用同一个凭据 ls-remote 判为成功；写凭据吊销', async () => {
    const t = await setup({ drop: (r) => r.write && r.method === 'POST' });
    expect(await t.push()).toEqual({ branch: 'kiln/attempt-7', sha: t.sha });
    expect(await t.origin.branchSha('kiln/attempt-7')).toBe(t.sha);
    const secret = t.f.issued[0]!.secret;
    expect(t.s.requests.at(-1)).toMatchObject({ write: false, password: secret });
    expect(t.f.issued.map((c) => c.revoked)).toEqual([true]);
  });

  it('推送发出后被取消：确认只读到旧值也不能证明没有写入，结果未知；写凭据吊销；下一次推送之前先查远端', async () => {
    const ac = new AbortController();
    let armed = true;
    const t = await setup({
      // 第一次推送的数据到达时取消（这个请求一直不处理：远端可能还在写）
      gate: (r) => {
        if (armed && r.write && r.method === 'POST') {
          armed = false;
          ac.abort();
          return new Promise<void>(() => undefined);
        }
        return undefined;
      },
    });
    const err = await t.push(ac.signal).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(PushUnknownError);
    expect((err as Error).message).toContain('结果未知');
    expect(t.f.issued.map((c) => c.revoked)).toEqual([true]);
    const n = t.s.requests.length;
    // 第二次（新的写操作）：先查远端，再推送
    expect(await t.push()).toEqual({ branch: 'kiln/attempt-7', sha: t.sha });
    expect(phases(t.s.requests.slice(n))).toEqual(['读', '写', '读']);
    expect(t.f.issued.map((c) => [c.access, c.revoked])).toEqual([
      [CredentialAccess.WRITE, true],
      [CredentialAccess.WRITE, true],
    ]);
  });

  // 只有 git push 自己正常退出且非零才是明确被拒：被外部信号终止、输出超过上限被终止、超时都按结果不明处理
  const endings: Array<[string, Partial<ExecResult>]> = [
    ['被外部信号终止', { code: 137, signal: 'SIGKILL', end: 'signal' }],
    ['输出超过上限被终止', { code: outputLimitCode, end: 'overflow' }],
    ['超时被终止', { code: timeoutCode, end: 'timeout' }],
  ];
  it.each(endings)('git push %s：远端已是目标 SHA 时成功，否则结果未知（不是明确失败）', async (_name, ending) => {
    for (const applied of [true, false]) {
      const t = await setup();
      // 推送本身照常执行（applied）或不执行，之后都报告这种结束方式
      const killed: Exec = async (cmd, args, o) => {
        if (cmd === 'git' && args[0] === 'push') {
          const r = applied ? await nodeExec(cmd, args, o) : { code: 0, stdout: '', stderr: '' };
          return { ...r, ...ending, stderr: r.stderr + '\n本地终止' };
        }
        return nodeExec(cmd, args, o);
      };
      const g = new Git(killed);
      const url = t.s.url('acme/widgets');
      const r = t.f.ops.run('acme/widgets', CredentialAccess.WRITE, async (op) => g.push(t.wt, url, 7n, undefined, await op.token()));
      if (applied) await expect(r).resolves.toEqual({ branch: 'kiln/attempt-7', sha: t.sha });
      else await expect(r).rejects.toBeInstanceOf(PushUnknownError);
      expect(t.f.issued.every((c) => c.revoked)).toBe(true);
    }
  });

  it('git push 未能启动（end=notrun）：远端已到位时成功，否则明确本地启动失败（不是结果未知，即使后查也失败）', async () => {
    for (const mode of ['unchanged', 'applied', 'confirmFailed'] as const) {
      const t = await setup();
      let pushed = false;
      const missing = path.join(t.root, 'nonexistent-git-for-test');
      const exec: Exec = async (cmd, args, o) => {
        if (cmd === 'git' && args[0] === 'push') {
          pushed = true;
          if (mode === 'applied') await nodeExec(cmd, args, o);
          return nodeExec(missing, [], {});
        }
        if (mode === 'confirmFailed' && pushed && cmd === 'git' && args[0] === 'ls-remote') {
          return nodeExec(missing, [], {});
        }
        return nodeExec(cmd, args, o);
      };
      const g = new Git(exec);
      const url = t.s.url('acme/widgets');
      const p = t.f.ops.run('acme/widgets', CredentialAccess.WRITE, async (op) => g.push(t.wt, url, 7n, undefined, await op.token()));
      if (mode === 'applied') {
        await expect(p).resolves.toEqual({ branch: 'kiln/attempt-7', sha: t.sha });
      } else {
        const err = await p.catch((e: unknown) => e);
        expect(err).toBeInstanceOf(InfraError);
        expect(err).not.toBeInstanceOf(PushUnknownError);
        expect((err as InfraError).errorClass).toBe(ErrorClass.push);
        expect((err as Error).message).toContain('未能启动 git push');
      }
      expect(t.f.issued.every((c) => c.revoked)).toBe(true);
    }
  });

  it('只认完整的 ref 名：远端只有后缀相同的 tag（同一个 commit）时不算成功；git push 会写到那个 tag 上，所以不推送、明确失败', async () => {
    const t = await setup();
    // refs/tags/refs/heads/kiln/attempt-7 按后缀匹配 ls-remote 和 git push 的 refs/heads/kiln/attempt-7
    const tag = 'refs/tags/refs/heads/kiln/attempt-7';
    await git(t.wt, 'push', '--quiet', t.origin.bare, `${t.sha}:${tag}`);
    const writes = t.s.requests.filter((r) => r.write).length;
    const err = await t.push().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(InfraError);
    expect(err).not.toBeInstanceOf(PushUnknownError);
    expect((err as Error).message).toContain(tag);
    expect(t.s.requests.filter((r) => r.write).length).toBe(writes);
    expect((await nodeExec('git', ['show-ref', '--verify', 'refs/heads/kiln/attempt-7'], { cwd: t.origin.bare, env: localGitEnv(process.env) })).code).not.toBe(0);
    expect(t.f.issued.map((c) => c.revoked)).toEqual([true]);
    expect(lsRemoteRef(`${t.sha}\trefs/tags/refs/heads/x\nabc\trefs/heads/x\n`, 'refs/heads/x')).toBe('abc');
    expect(lsRemoteRef(`${t.sha}\trefs/tags/refs/heads/x\n`, 'refs/heads/x')).toBe('');
  });

  it('推送的源固定为开始时的 HEAD（查远端之后 HEAD 变了也一样），不递归推送子模块（本地配置要求检查子模块时照样推送）', async () => {
    const t = await setup();
    // 子模块有没有推送到任何远端的 commit；push.recurseSubmodules=check 时不加 --recurse-submodules=no 的推送会失败
    const subBare = path.join(t.root, 'sub.git');
    const subWork = path.join(t.root, 'sub-work');
    await git(t.root, 'init', '--quiet', '--bare', '-b', 'main', subBare);
    await git(t.root, 'init', '--quiet', subWork);
    await commitFile(subWork, 's.txt', 's\n', 'sub');
    await git(subWork, 'push', '--quiet', subBare, 'HEAD:refs/heads/main');
    await git(t.wt, '-c', 'protocol.file.allow=always', 'submodule', 'add', '--quiet', subBare, 'sub');
    await commitFile(path.join(t.wt, 'sub'), 'local.txt', 'unpushed\n', 'unpushed');
    await git(t.wt, 'add', 'sub');
    const sha = await commitFile(t.wt, 'y.txt', 'y\n', 'with submodule');
    await git(t.wt, 'config', 'push.recurseSubmodules', 'check');
    // guard 在查远端之后、推送之前调用：这时 HEAD 前进了一个 commit
    const later = async () => commitFile(t.wt, 'z.txt', 'z\n', 'later');
    let moved: Promise<string> | undefined;
    expect(await t.push(undefined, () => void (moved = later()))).toEqual({ branch: 'kiln/attempt-7', sha });
    await moved;
    expect(await t.origin.branchSha('kiln/attempt-7')).toBe(sha);
  });
});

describe('prepareClone 的本地配置：origin 是真实地址时，个人的凭据 helper 和全局配置中的个人请求头都不生效', () => {
  /** personalSecret 是机器全局配置中个人请求头的密码（测试的 git 服务接受它：请求头被带上时就能读到仓库）。 */
  const personalSecret = 'personal-secret';

  /**
   * setup 建一个经 HTTP 提供的私有任务仓库（只接受连接器的凭据 tok 和个人的密码），和一份个人的全局配置：记录型的凭据
   * helper，以及不限地址的 http.extraHeader（个人的 Basic 认证头）。Git 的 exec 在每一条改配置的命令（clone、git config
   * 的写入、remote set-url）之后检查：origin 已经是真实地址时，任务仓库主机上的个人 helper 不被调用（git credential
   * fill），访问 origin 时生效的请求头为空（git config --get-urlmatch，与 git 发请求时读的相同）。origin 还是旧的无效地址
   * 时普通的 git 访问不到任务仓库，保护是在改 origin 之前装好的。
   */
  async function setup() {
    const root = await tmp();
    const origin = await Origin.create(root);
    const s: GitHTTP = await gitHTTP(root, { 'acme/widgets': origin.bare }, (r) => r.password === 'tok' || r.password === personalSecret);
    cleanups.push(() => s.close());
    const trace = path.join(root, 'personal-called');
    const personal = path.join(root, 'personal.gitconfig');
    const header = `Authorization: Basic ${Buffer.from(`me:${personalSecret}`).toString('base64')}`;
    await writeFile(personal, `[credential]\n\thelper = "!f() { echo helper >> '${trace}'; echo username=evil; echo password=evil; }; f"\n[http]\n\textraHeader = ${header}\n`);
    // 测试自己运行 git 时经 localGitEnv 清掉外层 kiln-orca task exec / CI Runner 的包装、配置注入和 askpass：否则 git
    // 会走外层传输包装，请求到不了模拟服务，下面的断言会假通过或假失败。
    const personalEnv = {
      ...localGitEnv(process.env),
      GIT_CONFIG_GLOBAL: personal, GIT_CONFIG_NOSYSTEM: '1', LC_ALL: 'C',
    };
    const fill = (dir: string) =>
      nodeExec('git', ['credential', 'fill'], {
        cwd: dir,
        env: personalEnv,
        input: `protocol=http\nhost=${s.host}\npath=acme/widgets.git\n\n`,
      });
    /** effectiveHeader 返回在 dir 中访问 url 时生效的 http.extraHeader（个人的全局配置）。 */
    const effectiveHeader = async (dir: string, url: string) => (await nodeExec('git', ['config', '--get-urlmatch', 'http.extraheader', url], { cwd: dir, env: personalEnv })).stdout.trim();
    const writes: string[][] = [];
    const leaks: string[] = [];
    const watched: Exec = async (cmd, args, o) => {
      const r = await nodeExec(cmd, args, o);
      const write =
        cmd === 'git' &&
        (args[0] === 'clone' || (args[0] === 'remote' && args[1] === 'set-url') || (args[0] === 'config' && !args.includes('--get-all') && !args.includes('--get')));
      if (write) {
        writes.push([...args]);
        const dir = args[0] === 'clone' ? args.at(-1)! : o?.cwd ?? '';
        const originURL = (await nodeExec('git', ['config', '--get', 'remote.origin.url'], { cwd: dir, env: personalEnv })).stdout.trim();
        if (originURL === url) {
          await fill(dir);
          if (existsSync(trace)) {
            leaks.push(`helper：${args.join(' ')}`);
            await rm(trace, { force: true });
          }
          if ((await effectiveHeader(dir, url)) !== '') leaks.push(`请求头：${args.join(' ')}`);
        }
      }
      return r;
    };
    const clone = path.join(root, 'repos', '42', 'widgets');
    const url = s.url('acme/widgets');
    const g = new Git(watched);
    const prepare = () => g.prepareClone(clone, url, [], 0, { username: 'x-access-token', secret: 'tok' });
    const helperKey = `credential.http://${s.host}.helper`;
    const headerKey = `http.${url}.extraHeader`;
    return { root, origin, s, g, trace, fill, effectiveHeader, personalEnv, writes, leaks, clone, url, prepare, helperKey, headerKey };
  }

  async function expectConfigured(t: Awaited<ReturnType<typeof setup>>) {
    expect((await nodeExec('git', ['config', '--local', '--get-all', t.helperKey], { cwd: t.clone, env: t.personalEnv })).stdout).toBe(`\n${quitHelper}\n`);
    expect((await nodeExec('git', ['config', '--local', '--get-all', t.headerKey], { cwd: t.clone, env: t.personalEnv })).stdout).toBe('\n');
    expect(await t.effectiveHeader(t.clone, t.url)).toBe('');
    expect(await git(t.clone, 'config', '--get', 'remote.origin.url')).toBe(t.url);
    expect((await nodeExec('git', ['config', '--get', 'remote.origin.pushurl'], { cwd: t.clone, env: t.personalEnv })).code).toBe(1);
    expect(await git(t.clone, 'config', '--get-all', 'remote.origin.fetch')).toBe('+refs/heads/*:refs/remotes/origin/*');
    expect(await git(t.clone, 'config', '--local', '--get', 'user.name')).toBe(gitAuthor);
    expect(await git(t.clone, 'config', '--local', '--get', 'user.email')).toBe(gitEmail);
    // 最终状态：个人 helper 不被调用
    await t.fill(t.clone);
    expect(existsSync(t.trace)).toBe(false);
  }

  it('新建 clone：保护随 clone 写入（clone -c），之后每一步个人 helper 都不生效', async () => {
    const t = await setup();
    await t.prepare();
    expect(t.writes[0]?.[0]).toBe('clone');
    expect(t.writes[0]).toContain(`${t.helperKey}=${quitHelper}`);
    expect(t.writes[0]).toContain(`${t.headerKey}=`);
    expect(t.leaks).toEqual([]);
    await expectConfigured(t);
  });

  it('从旧配置迁移（无效的拉取、推送地址，没有保护）：先装保护再改 origin，每一步之后个人 helper 都不生效', async () => {
    const t = await setup();
    await mkdir(path.dirname(t.clone), { recursive: true });
    await git(path.dirname(t.clone), 'clone', '--quiet', '--no-checkout', t.origin.bare, t.clone);
    await git(t.clone, 'remote', 'set-url', 'origin', 'kiln-disabled://use-kiln-orca-task-exec-git-fetch-with-explicit-url');
    await git(t.clone, 'config', 'remote.origin.pushurl', 'kiln-disabled://agent-must-not-push');
    // 旧配置中个人 helper、个人请求头都生效（测试替身确实会被调用、请求头确实会被带上）
    await t.fill(t.clone);
    expect(existsSync(t.trace)).toBe(true);
    await rm(t.trace);
    expect(await t.effectiveHeader(t.clone, t.url)).not.toBe('');

    await t.prepare();
    // 先装保护（请求头置空，helper 原子地替换成一个空值再追加 quit），之后才改 origin
    expect(t.writes.slice(0, 3)).toEqual([
      ['config', '--local', '--replace-all', t.headerKey, ''],
      ['config', '--local', '--replace-all', t.helperKey, ''],
      ['config', '--local', '--add', t.helperKey, quitHelper],
    ]);
    expect(t.writes.findIndex((w) => w[0] === 'remote')).toBe(3);
    expect(t.leaks).toEqual([]);
    await expectConfigured(t);
  });

  it('保护被改坏（helper 列表不对、请求头的空值被删）时同样先原子替换再追加；已经是要求的配置时零写入（配置文件不被重写）', async () => {
    const t = await setup();
    await t.prepare();
    // 被改坏（每次坏一项，另一项仍在；修复的每一步之后两项都生效）：helper 只剩 quit 之外的一个值
    await git(t.clone, 'config', '--local', '--replace-all', t.helperKey, 'store');
    t.writes.length = 0;
    await t.prepare();
    expect(t.writes).toEqual([
      ['config', '--local', '--replace-all', t.helperKey, ''],
      ['config', '--local', '--add', t.helperKey, quitHelper],
    ]);
    expect(t.leaks).toEqual([]);
    await expectConfigured(t);
    // 请求头的空值被删掉
    await git(t.clone, 'config', '--local', '--unset-all', t.headerKey);
    t.writes.length = 0;
    await t.prepare();
    expect(t.writes).toEqual([['config', '--local', '--replace-all', t.headerKey, '']]);
    expect(t.leaks).toEqual([]);
    await expectConfigured(t);

    t.writes.length = 0;
    const cfg = path.join(t.clone, '.git', 'config');
    const before = await stat(cfg, { bigint: true });
    await t.prepare();
    const after = await stat(cfg, { bigint: true });
    expect(t.writes).toEqual([]);
    expect([after.ino, after.mtimeNs]).toEqual([before.ino, before.mtimeNs]);
  });

  it('不经 task exec 的 git fetch origin：不带全局配置中的个人请求头、不用个人 helper，以 quit 失败；连接器自己的准备仓库、推送（环境变量注入请求头）不受本地置空影响', async () => {
    const t = await setup();
    await t.prepare();
    // 连接器自己的准备仓库用的是签发的凭据
    expect(t.s.requests.length).toBeGreaterThan(0);
    expect(t.s.requests.every((r) => r.password === 'tok')).toBe(true);
    // 对照：没有本地保护的普通 clone 中，个人请求头确实会被带上并被接受
    const plain = path.join(t.root, 'plain');
    await git(t.root, 'clone', '--quiet', '--no-checkout', t.origin.bare, plain);
    await git(plain, 'remote', 'set-url', 'origin', t.url);
    t.s.requests.length = 0;
    const control = await nodeExec('git', ['fetch', 'origin'], { cwd: plain, env: t.personalEnv });
    expect(control.code, control.stderr).toBe(0);
    expect(t.s.requests.some((r) => r.password === personalSecret)).toBe(true);
    // 受保护的 clone：请求不带个人请求头，git 要凭据时 helper 回答 quit
    t.s.requests.length = 0;
    const r = await nodeExec('git', ['fetch', 'origin'], { cwd: t.clone, env: t.personalEnv });
    expect(r.code).not.toBe(0);
    expect(r.stderr).toContain('quit');
    expect(t.s.requests.length).toBeGreaterThan(0);
    expect(t.s.requests.some((x) => x.password !== '')).toBe(false);
    expect(existsSync(t.trace)).toBe(false);
    // 连接器的推送：环境变量中的请求头在本地配置之后读取，照常带上签发的凭据
    t.s.requests.length = 0;
    await t.g.push(t.clone, t.url, 7n, undefined, { username: 'x-access-token', secret: 'tok' });
    expect(t.s.requests.some((x) => x.write && x.password === 'tok')).toBe(true);
    expect(t.s.requests.some((x) => x.password === personalSecret)).toBe(false);
  });
});

describe('prepareClone 的停止信号（审计）', () => {
  it('停止信号交给 git：clone 挂着时停止立即结束，不再继续后面的步骤', async () => {
    const calls: Array<{ args: readonly string[]; signal?: AbortSignal }> = [];
    const exec: Exec = (_cmd, args, opts) => {
      calls.push({ args, signal: opts?.signal });
      const s = opts?.signal;
      // 没有信号时直接失败（测试据此发现信号没有传下来）；有信号时模拟网络 clone 挂着，等停止
      if (!s) return Promise.resolve({ code: 1, stdout: '', stderr: '没有信号', end: 'exit' });
      return new Promise((resolve) => s.addEventListener('abort', () => resolve({ code: 127, stdout: '', stderr: '已中止', end: 'abort' }), { once: true }));
    };
    const g = new Git(exec);
    const ac = new AbortController();
    const p = g.prepareClone(path.join(await tmp(), 'clone'), 'https://github.com/acme/widgets.git', ['abc'], 0, undefined, ac.signal).then(
      () => undefined,
      (err: unknown) => err,
    );
    await waitFor(() => calls.length === 1, 5_000, '开始 clone');
    expect(calls[0]?.args[0]).toBe('clone');
    expect(calls[0]?.signal).toBe(ac.signal);
    ac.abort();
    const err = await p;
    // prepareClone 把 git 的失败归到准备仓库（infra.fetch），原因里是停止
    expect(err).toBeInstanceOf(InfraError);
    expect((err as InfraError).message).toContain('已中止');
    expect((err as InfraError).errorClass).toBe(ErrorClass.fetch);
    // 没有继续 fetch、配置等步骤
    expect(calls).toHaveLength(1);
  });
});

describe('用 git 的结果判断本地状态：只有明确的结果才算"没有"，git 出错时抛出', () => {
  it('hasCommit 与交付核对：对象有 / 没有照常判断；仓库配置损坏（cat-file、merge-base 以 128 退出）时抛出，不当作"没有"或"不是后代"', async () => {
    const root = await tmp();
    const origin = await Origin.create(root);
    const wt = path.join(root, 'wt');
    await git(root, 'clone', '--quiet', origin.bare, wt);
    const start = await git(wt, 'rev-parse', 'HEAD');
    await commitFile(wt, 'docs/d.md', '# d\n', 'doc');
    const g = new Git(nodeExec);
    expect(await g.hasCommit(wt, start)).toBe(true);
    expect(await g.hasCommit(wt, '0123456789012345678901234567890123456789')).toBe(false);
    expect(await g.checkDelivered(wt, { start, design: true, baseRef: '', documents: ['docs/d.md'] })).toEqual([]);
    expect(await g.checkDelivered(wt, { start, design: true, baseRef: '', documents: ['docs/none.md'] })).toEqual(['design.documents 中的 docs/none.md 不在 HEAD 中（请 commit 它，或者改正路径）']);
    // 不是后代：merge-base 正常退出、返回 1
    await git(wt, 'checkout', '--quiet', '--orphan', 'other');
    await commitFile(wt, 'x.md', 'x\n', 'orphan');
    expect((await g.checkDelivered(wt, { start, design: false, baseRef: '', documents: [] }))[0]).toContain('不是起点');
    // merge-base 以 128 退出（起点在本地不存在）：抛出，不当作"不是后代"告诉 Agent（git status 照常，前面的检查都能通过）
    await expect(g.checkDelivered(wt, { start: '0123456789012345678901234567890123456789', design: false, baseRef: '', documents: [] })).rejects.toBeInstanceOf(ExecError);
    // 配置损坏：对象其实在，但 git 以 128 退出
    await writeFile(path.join(wt, '.git', 'config'), '[[[ broken\n');
    await expect(g.hasCommit(wt, start)).rejects.toBeInstanceOf(ExecError);
    await expect(g.checkDelivered(wt, { start, design: false, baseRef: '', documents: [] })).rejects.toBeInstanceOf(ExecError);
  });
});

// GIT_CONFIG_COUNT 的各种写法（与 CI Runner 的 gitconfig_test.go 相同的一组；undefined 表示没有设置）。
const gitConfigCountCases: Array<string | undefined> = [
  undefined, '', ' ', '0', '1', '2', '3', ' 2', '\t2', '\n2', '\v2', '\f2', '\r2', '+2', '-0', '-2', '2 ', '02', '008',
  '00000000000002', 'abc', '+', '-', '0x2', '2147483647', '2147483648', '99999999999', '00000000002147483648',
];

describe('task exec 的 git 配置接续在已有的环境变量配置之后（GIT_CONFIG_COUNT 的判定与 git 相同）', () => {
  const imageConfig = (count: string | undefined) => ({
    GIT_CONFIG_KEY_0: 'img.a', GIT_CONFIG_VALUE_0: '1', GIT_CONFIG_KEY_1: 'img.b', GIT_CONFIG_VALUE_1: '2',
    GIT_CONFIG_KEY_2: 'credential.https://github.com.helper', GIT_CONFIG_VALUE_2: 'agent-helper',
    ...(count === undefined ? {} : { GIT_CONFIG_COUNT: count }),
  });
  it('gitConfigCount 与真实的 git 一致；git 接受时 execGitEnv 从它开始追加（原有的项仍可读，Kiln 的 helper 最后生效），拒绝时不改', async () => {
    const dir = await tmp();
    // 把真实 CI Runner 的外层环境真正注入 process.env，再经 localGitEnv 清理：清理漏掉外层编号不定的配置注入时，
    // c === undefined 的用例会读到外层的 GIT_CONFIG_COUNT，下面的断言立刻失败；不依赖测试机当前的环境变量。
    stubOuterCiGitEnv();
    const base = { ...localGitEnv(process.env), PATH: process.env.PATH, HOME: dir, GIT_CONFIG_NOSYSTEM: '1', LC_ALL: 'C' };
    // 外层注入的高编号项（GIT_CONFIG_KEY_9 / GIT_CONFIG_VALUE_9）也要删掉：localGitEnv 只按名字前缀清理时子进程才继承不到它们，
    // 改成按 GIT_CONFIG_COUNT 逐个删就会漏掉这一项，下面的断言立刻失败（子进程真的继承，不是看对象里有没有 undefined）。
    const stale = await nodeExec('/bin/sh', ['-c', 'printf "%s|%s" "${GIT_CONFIG_KEY_9-}" "${GIT_CONFIG_VALUE_9-}"'], { cwd: dir, env: base });
    expect(stale.stdout, 'localGitEnv 没有删掉外层的高编号 GIT_CONFIG_KEY_9 / GIT_CONFIG_VALUE_9').toBe('|');
    for (const c of gitConfigCountCases) {
      const probe = await nodeExec('git', ['config', '--get-regexp', String.raw`^img\.`], { cwd: dir, env: { ...base, ...imageConfig(c) } });
      const rejects = /bogus count|too many entries/.test(probe.stderr);
      const n = gitConfigCount(c);
      expect(n === undefined, `GIT_CONFIG_COUNT=${JSON.stringify(c)}`).toBe(rejects);
      const env = { ...base, ...imageConfig(c) };
      const out: Record<string, string | undefined> = { ...env, ...execGitEnv(env, { helperURL: 'https://github.com', goprivate: [], execRoot: dir }, 'kiln-tool', '/x') };
      if (n === undefined) {
        expect(out.GIT_CONFIG_COUNT, `GIT_CONFIG_COUNT=${JSON.stringify(c)}`).toBe(c);
        expect(out.GIT_CONFIG_KEY_3).toBeUndefined();
        continue;
      }
      expect(out.GIT_CONFIG_COUNT, `GIT_CONFIG_COUNT=${JSON.stringify(c)}`).toBe(String(n + 4));
      expect(out[`GIT_CONFIG_KEY_${n}`]).toBe('credential.https://github.com.helper');
      if (n !== 3) continue; // 原有的三项都生效时核对读到的结果
      const env2 = { ...out, GIT_EXEC_PATH: undefined };
      const img = await nodeExec('git', ['config', '--get-regexp', String.raw`^img\.`], { cwd: dir, env: env2 });
      expect(img.stdout).toBe('img.a 1\nimg.b 2\n');
      const helpers = await nodeExec('git', ['config', '--get-all', 'credential.https://github.com.helper'], { cwd: dir, env: env2 });
      expect(helpers.stdout).toBe('agent-helper\n\n!kiln-tool git-credential\n');
    }
  });
});

// 要与另一个进程（libcurl、git）打开同一个文件的路径按原始分量拼接，由文件系统解析符号链接和 ..（joinRaw）。布局：root/w 是
// 传输的工作目录，root/w/link 指向 root/far/inner，所以 link/../x 实际是 root/far/x；按词法规整会变成 root/w/x（诱饵）。
// 每个文件写一个无敏感信息的标记，比较检查器选中的文件与按原始路径打开的文件是不是同一个。netrc 的情形与 CI Runner 的
// rawpath_test.go 相同。
describe('路径按原始分量拼接（符号链接 + ..）', () => {
  async function layout() {
    const root = await tmp();
    const w = path.join(root, 'w');
    const far = path.join(root, 'far');
    await mkdir(w, { recursive: true });
    await mkdir(path.join(far, 'inner'), { recursive: true });
    await symlink(path.join(far, 'inner'), path.join(w, 'link'));
    return { root, w, far };
  }
  const marker = async (p: string) => (await readFile(p, 'utf8')).trim();

  it('netrc：相对 / 绝对的 NETRC、HOME 下的 .netrc，检查的是 libcurl 读的那个文件；只有它里面有 github.com 时报告', async () => {
    const { root, w, far } = await layout();
    await writeFile(path.join(far, 'n'), 'machine github.com login real');
    await writeFile(path.join(w, 'n'), 'machine example.com login decoy');
    await writeFile(path.join(far, '.netrc'), 'machine github.com login real-home');
    await writeFile(path.join(w, '.netrc'), 'machine example.com login decoy-home');
    const cases: Array<[string, string, Record<string, string>]> = [
      ['相对的 NETRC', `${w}/link/../n`, { NETRC: 'link/../n' }],
      ['绝对的 NETRC', `${w}/link/../n`, { NETRC: `${w}/link/../n` }],
      ['HOME', `${w}/link/../.netrc`, { HOME: `${w}/link/..` }],
    ];
    for (const [name, want, env] of cases) {
      const c = { exec: nodeExec, env, cwd: w, platform: 'linux' as const };
      const got = netrcFiles(c).filter((f) => f.startsWith(root));
      expect(got, name).toHaveLength(1);
      expect(await marker(got[0]!), name).toBe(await marker(want));
      const conflicts = await authConflicts('https://github.com/acme/widgets.git', { ...c, env: { ...env, PATH: process.env.PATH, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null' } });
      expect(conflicts.some((x) => x.source.startsWith(root)), name).toBe(true);
    }
  });

  it('全局配置文件（XDG_CONFIG_HOME、HOME）：查询的是 git 读的那个文件', async () => {
    const { w, far } = await layout();
    for (const d of [far, w]) await mkdir(path.join(d, 'git'), { recursive: true });
    for (const d of [far, w]) await mkdir(path.join(d, '.config', 'git'), { recursive: true });
    await writeFile(path.join(far, 'git', 'config'), 'real-xdg');
    await writeFile(path.join(w, 'git', 'config'), 'decoy-xdg');
    await writeFile(path.join(far, '.config', 'git', 'config'), 'real-default');
    await writeFile(path.join(w, '.config', 'git', 'config'), 'decoy-default');
    await writeFile(path.join(far, '.gitconfig'), 'real-home');
    await writeFile(path.join(w, '.gitconfig'), 'decoy-home');
    const [xdg] = globalConfigFiles({ XDG_CONFIG_HOME: `${w}/link/..` });
    expect(await marker(xdg!)).toBe('real-xdg');
    const [def, home] = globalConfigFiles({ HOME: `${w}/link/..` });
    expect(await marker(def!)).toBe('real-default');
    expect(await marker(home!)).toBe('real-home');
  });

  it('git --exec-path 的输出只去掉末尾的那一个换行：以空格结尾的 exec 目录原样返回，能据此建出镜像', async () => {
    const root = await tmp();
    const core = `${root}/core `; // 以空格结尾
    await mkdir(core);
    await writeFile(`${core}/git-upload-pack`, 'real');
    const bin = path.join(root, 'bin');
    await mkdir(bin);
    await writeFile(path.join(bin, 'git'), `#!/bin/sh\nprintf '%s\\n' '${core}'\n`, { mode: 0o755 });
    const got = await realExecPath(nodeExec, { PATH: `${bin}:${process.env.PATH ?? ''}` });
    expect(got).toBe(core);
    const dir = await ensureExecMirror(path.join(root, 'mirrors'), got, () => '#!/bin/sh\n');
    expect(await readFile(path.join(dir, 'git-upload-pack'), 'utf8')).toBe('real');
  });

  it('git exec 镜像：符号链接和包装指向 git --exec-path 原样给出的目录中的文件', async () => {
    const { root, w, far } = await layout();
    for (const d of [far, w]) await mkdir(path.join(d, 'core'), { recursive: true });
    await writeFile(path.join(far, 'core', 'git-upload-pack'), 'real-pack');
    await writeFile(path.join(w, 'core', 'git-upload-pack'), 'decoy-pack');
    await writeFile(path.join(far, 'core', 'git-remote-https'), 'real');
    const real = `${w}/link/../core`;
    const helpers: string[] = [];
    const dir = await ensureExecMirror(path.join(root, 'mirrors'), real, (h) => {
      if (h) helpers.push(h);
      return '#!/bin/sh\n';
    });
    expect(await marker(path.join(dir, 'git-upload-pack'))).toBe('real-pack');
    expect(helpers).toHaveLength(1);
    expect(await marker(helpers[0]!)).toBe('real');
  });
});

describe('authConflicts：只返回键和来源，不含值', () => {
  /** fakeGit 按 git config 的子命令回复（其它命令不应出现），记下环境。 */
  /** fakeGit 是 git 的替身：不限范围的 --get-urlmatch、git config get（git 2.46 起），以及按范围的查询（旧 git 的来源）。 */
  function fakeGit(
    urlmatch: { code: number; stdout: string },
    origin = { code: 0, stdout: 'file:/home/u/.gitconfig\tX-Token: v' },
    seen: Array<Record<string, string | undefined>> = [],
    scopes: Record<string, { code: number; stdout: string }> = {},
  ): Exec {
    return (_cmd, args, o) => {
      seen.push(o?.env ?? {});
      if (args[1] === '-z' && args[2] === '--get-urlmatch') return Promise.resolve({ ...urlmatch, stderr: '' });
      if (args[1] === 'get') return Promise.resolve({ ...origin, stderr: '' });
      // 按范围的查询：--local、--system，或者用 --file 指定的一份全局配置（scopes 的键是文件路径）
      const file = args[1] === '--file';
      const scope = file ? args[2] : args[1];
      const rest = args.slice(file ? 3 : 2);
      if ((file || ['--local', '--system'].includes(args[1] ?? '')) && rest[0] === '--includes' && rest[1] === '-z' && rest[2] === '--get-urlmatch') {
        return Promise.resolve({ ...(scopes[scope!] ?? { code: 1, stdout: '' }), stderr: '' });
      }
      return Promise.reject(new Error(`意外的命令 ${args.join(' ')}`));
    };
  }
  const check = (exec: Exec, env: Record<string, string | undefined> = { HOME: '/nonexistent-home' }) => authConflicts('https://github.com/acme/widgets.git', { exec, env, platform: 'linux' });

  it('extraHeader：退出码 0 且值非空是冲突（来源取自 git config get，丢掉值）；0 且为空、1 不是；其它退出码是查询失败；查询不带 GIT_CONFIG', async () => {
    const seen: Array<Record<string, string | undefined>> = [];
    expect(await check(fakeGit({ code: 0, stdout: 'X-Token: v\n' }, undefined, seen), { HOME: '/nonexistent-home', GIT_CONFIG: '/x' })).toEqual([{ key: 'http.extraheader', source: 'git 配置 file:/home/u/.gitconfig' }]);
    expect(seen.length).toBe(2);
    for (const e of seen) {
      expect('GIT_CONFIG' in e).toBe(true);
      expect(e.GIT_CONFIG).toBeUndefined();
    }
    // git 2.46 以下没有 git config get：逐个查询本地、git 实际读的每一份全局配置（两份并存时都查）、系统配置，列出有值的
    const noGet = { code: 129, stdout: '' };
    const old = await check(fakeGit({ code: 0, stdout: 'X-Token: v\n' }, noGet, [], { '/home/u/.gitconfig': { code: 0, stdout: 'X-Token: v\n' } }), { HOME: '/home/u' });
    expect(old).toEqual([{ key: 'http.extraheader', source: 'git 配置：全局配置 /home/u/.gitconfig（含其中 include 的文件）' }]);
    const xdg = await check(fakeGit({ code: 0, stdout: 'X-Token: v\n' }, noGet, [], { '/home/u/.config/git/config': { code: 0, stdout: 'X-Token: v\n' } }), { HOME: '/home/u' });
    expect(xdg[0]?.source).toBe('git 配置：全局配置 /home/u/.config/git/config（含其中 include 的文件）');
    const named = await check(fakeGit({ code: 0, stdout: 'X-Token: v\n' }, noGet, [], { '--system': { code: 0, stdout: 'X-Token: v\n' } }), { HOME: '/home/u', GIT_CONFIG_SYSTEM: '/etc/x' });
    expect(named[0]?.source).toBe('git 配置：系统配置 /etc/x（含其中 include 的文件）');
    // 哪个范围都没有：来自环境变量配置或 git -c
    const env = await check(fakeGit({ code: 0, stdout: 'X-Token: v\n' }, noGet));
    expect(env[0]?.source).toContain('GIT_CONFIG_COUNT');
    // -z：零长度的值（清空了列表）只有 NUL；只有空白的值不是空值
    expect(await check(fakeGit({ code: 0, stdout: '\0' }))).toEqual([]);
    expect((await check(fakeGit({ code: 0, stdout: '  \0' })))[0]?.key).toBe('http.extraheader');
    expect(await check(fakeGit({ code: 1, stdout: '' }))).toEqual([]);
    for (const code of [128, 129, 127]) {
      const r = await check(fakeGit({ code, stdout: '' }));
      expect(r).toHaveLength(1);
      expect(r[0]?.key).toBe('http.extraheader');
      expect(r[0]?.source).toContain(`查询失败（git config --get-urlmatch 退出码 ${code}`);
    }
    expect(JSON.stringify(await check(fakeGit({ code: 0, stdout: 'X-Token: v\n' })))).not.toContain('X-Token');
  });

  it('nodeExec 的 end：正常退出、被信号终止、超时、输出超限、调用方取消、没能启动各自可区分；只有正常退出才 exitedNormally', async () => {
    const exit1 = await nodeExec('sh', ['-c', 'exit 1']);
    const killed = await nodeExec('sh', ['-c', 'kill -TERM $$']);
    const slow = await nodeExec('sleep', ['30'], { timeoutMs: 100, killGraceMs: 100, pipeDrainMs: 100 });
    const big = await nodeExec('sh', ['-c', 'head -c 4096 /dev/zero'], { maxOutputBytes: 100, killGraceMs: 100, pipeDrainMs: 100 });
    const ac = new AbortController();
    const aborted = nodeExec('sleep', ['30'], { signal: ac.signal, killGraceMs: 100, pipeDrainMs: 100 });
    ac.abort();
    const missing = await nodeExec('/nonexistent/command', []);
    expect([exit1.end, killed.end, slow.end, big.end, (await aborted).end, missing.end]).toEqual(['exit', 'signal', 'timeout', 'overflow', 'abort', 'notrun']);
    expect([exit1, killed, slow, big, missing].map(exitedNormally)).toEqual([true, false, false, false, false]);
  });

  it('git config 被信号终止（没有退出码）：不能当成"没有匹配"的退出码 1，按查询失败处理；正常退出 1 才是没有匹配', async () => {
    // nodeExec 本身：被信号终止时 code 是 128 + 信号编号并带上信号，正常退出 1 不带
    const killed = await nodeExec('sh', ['-c', 'kill -TERM $$']);
    expect([killed.code, killed.signal]).toEqual([143, 'SIGTERM']);
    const one = await nodeExec('sh', ['-c', 'exit 1']);
    expect([one.code, one.signal]).toEqual([1, undefined]);
    // 真实的 git 被 SIGKILL：冲突（查询失败）
    const root = await tmp();
    const bin = path.join(root, 'bin');
    await mkdir(bin);
    await writeFile(path.join(bin, 'git'), '#!/bin/sh\nkill -KILL $$\n', { mode: 0o755 });
    const exec: Exec = (cmd, args, o) => nodeExec(cmd, args, { ...o, env: { ...o?.env, PATH: `${bin}:${process.env.PATH ?? ''}` } });
    const got = await authConflicts('https://github.com/acme/widgets.git', { exec, env: { HOME: '/nonexistent-home' }, platform: 'linux' });
    expect(got).toHaveLength(1);
    expect(got[0]?.source).toContain('查询失败');
    // 替身：退出码 1 但带信号同样不是没有匹配
    const fake: Exec = () => Promise.resolve({ code: 1, stdout: '', stderr: '', signal: 'SIGHUP' });
    expect((await authConflicts('https://github.com/acme/widgets.git', { exec: fake, env: { HOME: '/nonexistent-home' }, platform: 'linux' }))[0]?.source).toContain('查询失败');
  });

  it('netrc 不是普通文件（没有写端的 FIFO）或超过大小上限：不卡住，按无法检查（冲突）处理', async () => {
    const home = await tmp();
    const netrc = path.join(home, '.netrc');
    const mk = await nodeExec('mkfifo', [netrc]);
    expect(mk.code, mk.stderr).toBe(0);
    const none = fakeGit({ code: 1, stdout: '' });
    const t0 = Date.now();
    const fifo = await check(none, { HOME: home });
    expect(Date.now() - t0).toBeLessThan(5_000);
    expect(fifo).toEqual([{ key: 'netrc', source: `查询失败：${netrc} 不是普通文件` }]);
    await rm(netrc);
    await writeFile(netrc, 'x'.repeat(maxNetrcBytes + 1));
    expect((await check(none, { HOME: home }))[0]?.source).toContain('超过');
    // 不存在的文件不算冲突
    expect(await readNetrc(path.join(home, 'missing'), 1_000)).toEqual({ missing: true });
  });

  it('netrc：ENOTDIR（HOME=/dev/null、HOME 是普通文件、NETRC 在普通文件下）按没有这个文件处理；其它读不了的情况仍然拒绝', async () => {
    const root = await tmp();
    const none = fakeGit({ code: 1, stdout: '' });
    // 路径中有一段不是目录（/dev/null/.netrc）：与不存在相同，不是"无法检查"
    expect(await check(none, { HOME: '/dev/null' })).toEqual([]);
    const fileHome = path.join(root, 'file-home');
    await writeFile(fileHome, 'x');
    expect(await check(none, { HOME: fileHome })).toEqual([]);
    expect(await check(none, { HOME: path.join(root, 'nohome'), NETRC: path.join(fileHome, '.netrc') })).toEqual([]);
    // HOME=/dev/null 时，账户主目录中有一份提到目标主机的 netrc：仍然拒绝（注入临时候选，不读真实的用户主目录）
    const account = path.join(root, 'account');
    await mkdir(account);
    await writeFile(path.join(account, '.netrc'), 'machine github.com\n');
    expect(await authConflicts('https://github.com/acme/widgets.git', { exec: none, env: { HOME: '/dev/null' }, platform: 'linux', accountHome: account })).toEqual([
      { key: 'netrc 中的主机 github.com', source: path.join(account, '.netrc') },
    ]);
    const netrc = path.join(root, 'valid-netrc');
    await writeFile(netrc, 'machine github.com\n');
    expect(await check(none, { HOME: '/dev/null', NETRC: netrc })).toEqual([{ key: 'netrc 中的主机 github.com', source: netrc }]);
    // 符号链接循环（ELOOP）不是"没有这个文件"，仍然按无法检查拒绝
    const loop = path.join(root, 'loop');
    await symlink(loop, loop);
    expect((await check(none, { HOME: path.join(root, 'nohome'), NETRC: loop }))[0]?.source).toContain('查询失败');
    // 没有权限：以 root 运行时权限位不起作用，跳过这一项
    if (process.getuid?.() !== 0) {
      const denied = path.join(root, 'denied-netrc');
      await writeFile(denied, 'machine github.com\n', { mode: 0o000 });
      expect((await check(none, { HOME: path.join(root, 'nohome'), NETRC: denied }))[0]?.source).toContain('查询失败');
    }
  });

  it('git config 的查询卡住：到期按查询失败处理（冲突，拒绝这次传输），不会一直挂着；每次查询都带期限', async () => {
    const root = await tmp();
    const bin = path.join(root, 'bin');
    await mkdir(bin);
    await writeFile(path.join(bin, 'git'), '#!/bin/sh\nexec sleep 60\n', { mode: 0o755 });
    const opts: Array<number | undefined> = [];
    const exec: Exec = (cmd, args, o) => {
      opts.push(o?.timeoutMs);
      return nodeExec(cmd, args, { ...o, env: { ...o?.env, PATH: `${bin}:${process.env.PATH ?? ''}` }, killGraceMs: 100, pipeDrainMs: 100 });
    };
    const t0 = Date.now();
    const got = await authConflicts('https://github.com/acme/widgets.git', { exec, env: { HOME: '/nonexistent-home' }, platform: 'linux', timeoutMs: 200 });
    expect(Date.now() - t0).toBeLessThan(10_000);
    expect(got).toHaveLength(1);
    expect(got[0]?.source).toContain('查询失败');
    expect(opts).toEqual([200]);
    // 默认的期限
    const seen: Array<number | undefined> = [];
    await authConflicts('https://github.com/acme/widgets.git', {
      exec: (_c, _a, o) => (seen.push(o?.timeoutMs), Promise.resolve({ code: 1, stdout: '', stderr: '' })),
      env: { HOME: '/nonexistent-home' },
      platform: 'linux',
    });
    expect(seen).toEqual([authQueryTimeoutMs]);
  });

  it('地址带用户名或密码：来源只写净化后的地址，密码不进诊断子进程的参数', async () => {
    const argv: string[][] = [];
    const none: Exec = (_c, args) => {
      argv.push([...args]);
      return Promise.resolve({ code: 1, stdout: '', stderr: '' });
    };
    const r = await authConflicts('https://u:Sekr3t@github.com/acme/widgets.git?token=Sekr3tQ#frag', { exec: none, env: {}, platform: 'linux' });
    expect(r).toEqual([{ key: '地址中的用户名或密码', source: '地址 https://github.com/acme/widgets（可能来自 url.<地址>.insteadOf）' }]);
    expect(JSON.stringify(argv)).not.toContain('Sekr3t@');
    expect(safeURL('https://github.com/a/../b/c.git')).toBe('https://github.com/…');
    expect(safeURL('https://github.com/a/%62.git')).toBe('https://github.com/…');
    expect(safeURL('not a url Sekr3t')).not.toContain('Sekr3t');
  });

  it('netrc：保守判定（出现 default 或这个主机名即冲突，包括值和宏正文中出现的），来源与 libcurl 一致，读不了时是查询失败；只报告文件和键', async () => {
    const none = fakeGit({ code: 1, stdout: '' });
    const home = await tmp();
    const netrc = path.join(home, '.netrc');
    await writeFile(netrc, 'machine "GitHub.com" login x password Sekr3t\n');
    expect(await check(none, { HOME: home })).toEqual([{ key: 'netrc 中的主机 github.com', source: netrc }]);
    await writeFile(netrc, 'machine a.example login x password Sekr3t\n\ndefault login y password z\n');
    expect((await check(none, { HOME: home }))[0]?.key).toBe('netrc 中的 default');
    // 有意的保守：宏正文中出现主机名同样拒绝
    await writeFile(netrc, 'macdef init\ncd github.com\n\n');
    expect((await check(none, { HOME: home }))[0]?.key).toBe('netrc 中的主机 github.com');
    // 以空白、引号为界：notgithub.com、github.com.evil 不算
    await writeFile(netrc, 'machine notgithub.com login x password github.com.evil\n');
    expect(await check(none, { HOME: home })).toEqual([]);
    // NETRC 指定的文件；HOME 未设置时账户的主目录
    const alt = path.join(home, 'alt-netrc');
    await writeFile(alt, 'machine github.com\n');
    expect((await check(none, { HOME: path.join(home, 'none'), NETRC: alt }))[0]?.source).toBe(alt);
    const acct = await authConflicts('https://github.com/acme/widgets.git', { exec: none, env: {}, platform: 'linux', accountHome: path.join(home, 'acct') });
    expect(acct).toEqual([]);
    await mkdir(path.join(home, 'acct'));
    await writeFile(path.join(home, 'acct', '.netrc'), 'default\n');
    expect(netrcFiles({ exec: none, env: {}, platform: 'linux', accountHome: path.join(home, 'acct') })).toEqual([path.join(home, 'acct', '.netrc')]);
    expect(netrcFiles({ exec: none, env: { USERPROFILE: 'C:\\u' }, platform: 'win32', accountHome: 'C:\\u' })).toEqual([path.join('C:\\u', '_netrc'), path.join('C:\\u', '.netrc')]);
    // 读不了（这里是目录）：查询失败
    await rm(netrc);
    await mkdir(netrc);
    expect((await check(none, { HOME: home }))[0]?.source).toContain('查询失败');
    expect(JSON.stringify(await check(none, { HOME: home }))).not.toContain('Sekr3t');
    expect(netrcMention("password 'x' default", 'h')).toBe('default');
    expect(netrcMention('machine h.example', 'h')).toBe('');
  });

  // 与 internal/runner/gitaudit_test.go 的 netrcCases 是同一组用例（同样的输入、同样的期望），两边要保持同步：AI 与 CI 的
  // netrc 判定语义相同（主机 github.com）。
  const netrcCases: Array<[string, string, boolean]> = [
    ['主机名', 'machine github.com login x password p\n', true],
    ['带引号、大小写不同的主机名', 'machine "GitHub.com" login x password p\n', true],
    ['单引号为界', 'machine \'github.com\' login x password p\n', true],
    ['垂直制表符分界', 'machine\vgithub.com\vlogin x password p\n', true],
    ['换页分界', 'machine\fgithub.com\flogin x password p\n', true],
    ['回车分界', 'machine github.com\rlogin x password p\r\n', true],
    ['default', 'machine a.example login x password p\n\ndefault login y password z\n', true],
    ['大写 DEFAULT 与垂直制表符', 'x\vDEFAULT\vlogin x password p\n', true],
    ['宏正文中的主机名', 'macdef init\ncd github.com\n\n', true],
    ['引号中的转义', 'machine "git\\hub.com" login x password p\n', true],
    ['别的主机的值中有反斜杠', 'machine a.example login x password "p\\q"\n', true],
    ['主机名是别的词的一部分', 'machine notgithub.com login x password github.com.evil\n', false],
    ['垂直制表符后是别的主机', 'machine\vgithub.community login x password p\n', false],
    ['不间断空格不是分界', 'machine\u00a0github.com\u00a0login x password p\n', false],
    ['引号外的反斜杠', 'machine a.example login x password p\\q\n', false],
    ['空文件', '', false],
  ];
  it.each(netrcCases)('netrc 共用用例：%s', async (_name, content, conflict) => {
    const home = await tmp();
    await writeFile(path.join(home, '.netrc'), content);
    const got = await check(fakeGit({ code: 1, stdout: '' }), { HOME: home });
    expect(got.length > 0).toBe(conflict);
    expect(JSON.stringify(got)).not.toContain('password p');
  });

  it('machineAuthConflicts（doctor、连接器启动时）：只读服务用户的全局和系统配置与 netrc，对 GitHub 生效的请求头、netrc 条目是冲突；别的主机、本地仓库中的配置不算；只有键和来源', async () => {
    const root = await tmp();
    const home = path.join(root, 'home');
    const acct = path.join(root, 'acct');
    await mkdir(home);
    await mkdir(acct);
    const global = path.join(root, 'global.gitconfig');
    await writeFile(global, '');
    const env = { PATH: process.env.PATH, HOME: home, GIT_CONFIG_GLOBAL: global, GIT_CONFIG_NOSYSTEM: '1', LC_ALL: 'C' };
    const dir = await mkdtemp(path.join(root, 'check-'));
    const run = () => machineAuthConflicts({ exec: nodeExec, env, platform: process.platform, accountHome: acct }, dir);
    expect(await run()).toEqual([]);
    // 别的主机的请求头、netrc 条目不算
    await writeFile(global, '[http "https://example.com/"]\n\textraHeader = X-Token: Sekr3tOther\n');
    await writeFile(path.join(home, '.netrc'), 'machine example.com login x password Sekr3tOther\n');
    expect(await run()).toEqual([]);
    // 不限地址的请求头、netrc 中的 github.com（账户主目录下的也算）
    await writeFile(global, '[http]\n\textraHeader = Authorization: Basic Sekr3tHdr\n');
    await writeFile(path.join(acct, '.netrc'), 'machine github.com login x password Sekr3tNetrc\n');
    const got = await run();
    expect(got.map((c) => c.key)).toEqual(['http.extraheader', 'netrc 中的主机 github.com']);
    expect(got[0]?.source).toContain(global);
    expect(got[1]?.source).toBe(path.join(acct, '.netrc'));
    expect(JSON.stringify(got)).not.toMatch(/Sekr3t/);
  });

  it('machineAuthConflicts：全局、系统配置（含 include）中只对 GitHub 的组织、仓库路径声明的请求头也是冲突（按键报告，不含值）；零长度的值、别的主机、标签数不同的通配主机不算；配置损坏是查询失败', async () => {
    const root = await tmp();
    const home = path.join(root, 'home');
    await mkdir(home);
    const global = path.join(root, 'global.gitconfig');
    const system = path.join(root, 'system.gitconfig');
    const inc = path.join(root, 'inc.gitconfig');
    await writeFile(system, '');
    await writeFile(inc, '');
    const env = machineEnv({ HOME: home, GIT_CONFIG_GLOBAL: global, GIT_CONFIG_SYSTEM: system });
    const dir = await mkdtemp(path.join(root, 'check-'));
    const run = () => machineAuthConflicts({ exec: nodeExec, env, platform: process.platform, accountHome: home }, dir);
    // 不算：零长度的值、别的主机、*.github.com（三个标签，匹配不到 github.com）
    await writeFile(
      global,
      `[http "https://github.com/acme/r.git"]\n\textraHeader =\n[http "https://example.com/acme/"]\n\textraHeader = X: Sekr3t1\n[http "https://*.github.com/"]\n\textraHeader = X: Sekr3t2\n[include]\n\tpath = ${inc}\n`,
    );
    expect(await run()).toEqual([]);
    // 只对一个仓库路径（全局）、一个组织（系统）、通配的主机（include 进来的文件）声明的请求头
    await writeFile(global, `[http "https://github.com/acme/r.git"]\n\textraHeader = Authorization: Basic Sekr3t3\n[include]\n\tpath = ${inc}\n`);
    await writeFile(system, '[http "https://GitHub.com/acme/"]\n\textraheader = X: Sekr3t4\n');
    await writeFile(inc, '[http "https://git*.com/"]\n\textraHeader = X: Sekr3t5\n');
    const got = await run();
    expect(got).toEqual([
      { key: 'http.https://github.com/acme/r.extraheader', source: `全局配置 ${global}` },
      { key: 'http.https://git*.com/….extraheader', source: `全局配置 ${inc}` },
      { key: 'http.https://github.com/….extraheader', source: `系统配置 ${system}` },
    ]);
    expect(JSON.stringify(got)).not.toMatch(/Sekr3t/);
    // 配置损坏：查询失败（git 启动时读全部配置，根地址和两个范围的查询都失败）
    await writeFile(system, '');
    await writeFile(global, '[[[ broken\n');
    const broken = await run();
    expect(broken.map((c) => c.key)).toEqual(['http.extraheader', 'http.<地址>.extraheader', 'http.<地址>.extraheader']);
    expect(broken[1]?.source).toContain(`全局配置 ${global}：查询失败`);
    expect(broken[2]?.source).toContain(`系统配置 ${system}：查询失败`);
  });

  it('GIT_CONFIG_NOSYSTEM 按 git 的布尔语义：与真实 git 是否读系统配置一致，git 报错的值按"会读"处理；"0" 时系统配置中只对仓库声明的请求头照样报告', async () => {
    const root = await tmp();
    const system = path.join(root, 'system.gitconfig');
    await writeFile(system, '[sys]\n\tv = 1\n[http "https://github.com/acme/r.git"]\n\textraHeader = X: Sekr3tSys\n');
    const base = machineEnv({ HOME: path.join(root, 'home'), GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_SYSTEM: system });
    for (const v of [undefined, '', '0', '00', '1', '2', '-1', ' 1', 'true', 'TRUE', 'yes', 'Yes', 'on', 'off', 'false', 'No', '1 ', 'abc']) {
      const r = await nodeExec('git', ['config', '--get', 'sys.v'], { cwd: root, env: { ...base, GIT_CONFIG_NOSYSTEM: v } });
      const readsSystem = r.code === 0 || /bad boolean/.test(r.stderr); // git 报错的值：拿不准，按会读
      expect(gitEnvTrue(v), `GIT_CONFIG_NOSYSTEM=${JSON.stringify(v)}`).toBe(!readsSystem);
    }
    const dir = await mkdtemp(path.join(root, 'check-'));
    const got = await machineAuthConflicts({ exec: nodeExec, env: { ...base, GIT_CONFIG_NOSYSTEM: '0' }, platform: process.platform, accountHome: base.HOME }, dir);
    expect(got).toEqual([{ key: 'http.https://github.com/acme/r.extraheader', source: `系统配置 ${system}` }]);
    expect(JSON.stringify(got)).not.toMatch(/Sekr3t/);
  });

  it('machineAuthConflicts：两份全局配置（~/.gitconfig 与 XDG 的 git/config）并存时都检查（git config --global 只读前者），来源指到声明它的文件', async () => {
    const root = await tmp();
    const home = path.join(root, 'home');
    const xdgHome = path.join(root, 'xdg');
    await mkdir(path.join(home, '.config', 'git'), { recursive: true });
    await mkdir(path.join(xdgHome, 'git'), { recursive: true });
    const dir = await mkdtemp(path.join(root, 'check-'));
    const header = '[http "https://github.com/acme/r.git"]\n\textraHeader = Authorization: Basic Sekr3tXdg\n';
    await writeFile(path.join(home, '.gitconfig'), '[user]\n\tname = someone\n');
    const run = (env: Record<string, string | undefined>) =>
      machineAuthConflicts({ exec: nodeExec, env: machineEnv({ HOME: home, GIT_CONFIG_NOSYSTEM: '1', ...env }), platform: process.platform, accountHome: home }, dir);
    // 默认位置 ~/.config/git/config
    const def = path.join(home, '.config', 'git', 'config');
    await writeFile(def, header);
    expect(await run({})).toEqual([{ key: 'http.https://github.com/acme/r.extraheader', source: `全局配置 ${def}` }]);
    // XDG_CONFIG_HOME 指定时读它下面的 git/config，不读 ~/.config/git/config
    const custom = path.join(xdgHome, 'git', 'config');
    await writeFile(custom, header);
    expect(await run({ XDG_CONFIG_HOME: xdgHome })).toEqual([{ key: 'http.https://github.com/acme/r.extraheader', source: `全局配置 ${custom}` }]);
    // GIT_CONFIG_GLOBAL 指定时只读它
    expect(await run({ GIT_CONFIG_GLOBAL: path.join(home, '.gitconfig') })).toEqual([]);
    expect(JSON.stringify(await run({}))).not.toMatch(/Sekr3t/);
  });

  it('extraHeader 的单值查询与 git 实际发出的请求头等价（真实 git + 回环服务）：通用项与按地址的项两种顺序、中间清空、多个来源、GIT_CONFIG', { timeout: 60_000 }, async () => {
    const root = await tmp();
    const seen: string[] = [];
    // conns 记下到达服务的 TCP 连接：首尾带空白的请求头会被 Node 的 HTTP 解析器在进入处理函数之前就以 400 拒绝，
    // 但仍然证明 git 确实发出了请求（没有被继承的传输包装或环境错误挡下）。
    const conns: number[] = [];
    const srv = http.createServer((req, res) => {
      seen.push(String(req.headers['x-k'] ?? ''));
      res.writeHead(404);
      res.end();
    });
    srv.on('connection', () => conns.push(1));
    await new Promise<void>((r) => srv.listen(0, '127.0.0.1', r));
    cleanups.push(() => new Promise<void>((r) => srv.close(() => r())));
    const port = (srv.address() as { port: number }).port;
    const host = `http://127.0.0.1:${port}`;
    const url = `${host}/a/b.git`;
    const repo = path.join(root, 'r');
    await git(root, 'init', '--quiet', repo);
    const glob = path.join(root, 'global');
    const extra = path.join(root, 'only-git-config');
    await writeFile(extra, '[http]\n\textraHeader = X-K: viaGIT_CONFIG\n');
    type Src = 'global' | 'local' | 'env';
    // 与 internal/runner/gitauth_test.go 的 TestExtraHeaderQueryEquivalence 是同一组用例（保持同步）。conservative：git 把这个值
    // 加进了请求头列表（只有零长度的值才清空），但它不是一个能被服务认出的请求头；检查器按原始值保守地判为冲突。
    const cases: Array<{ name: string; entries: Array<[Src, string, string]>; env?: Record<string, string>; conservative?: true }> = [
      { name: '个人头 → 纯空白值', entries: [['global', 'http.extraheader', 'X-K: g'], ['local', 'http.extraheader', '  ']] },
      { name: '个人头 → 真正的空值', entries: [['global', 'http.extraheader', 'X-K: g'], ['local', 'http.extraheader', '']] },
      { name: '只有纯空白值', entries: [['env', 'http.extraheader', '  ']], conservative: true },
      { name: '首尾带空白的值', entries: [['env', 'http.extraheader', ' X-K: p ']], conservative: true },
      { name: '通用 → 按地址', entries: [['global', 'http.extraheader', 'X-K: g'], ['local', `http.${host}.extraheader`, 'X-K: u']] },
      { name: '按地址 → 通用', entries: [['local', `http.${host}.extraheader`, 'X-K: u'], ['local', 'http.extraheader', 'X-K: g']] },
      { name: '按地址 → 通用清空', entries: [['local', `http.${host}.extraheader`, 'X-K: u'], ['local', 'http.extraheader', '']] },
      { name: '通用 → 按地址清空', entries: [['global', 'http.extraheader', 'X-K: g'], ['local', `http.${host}.extraheader`, '']] },
      { name: '按地址 → 清空 → 通用', entries: [['local', `http.${host}.extraheader`, 'X-K: u1'], ['local', `http.${host}.extraheader`, ''], ['local', 'http.extraheader', 'X-K: g']] },
      { name: '清空 → 通用', entries: [['global', 'http.extraheader', ''], ['local', 'http.extraheader', 'X-K: g2']] },
      { name: '按地址两个值', entries: [['local', `http.${host}.extraheader`, 'X-K: u1'], ['local', `http.${host}.extraheader`, 'X-K: u2']] },
      { name: '按路径 → 按主机清空', entries: [['local', `http.${url}.extraheader`, 'X-K: p'], ['local', `http.${host}.extraheader`, '']] },
      { name: '按主机清空 → 按路径', entries: [['local', `http.${host}.extraheader`, ''], ['local', `http.${url}.extraheader`, 'X-K: p']] },
      { name: '文件 → 环境变量配置清空', entries: [['local', 'http.extraheader', 'X-K: f'], ['env', 'http.extraheader', '']] },
      { name: '环境变量配置', entries: [['env', 'http.extraheader', 'X-K: e']] },
      { name: '只在 GIT_CONFIG 中（远程助手不读）', entries: [], env: { GIT_CONFIG: extra } },
    ];
    for (const c of cases) {
      await writeFile(glob, '');
      await writeFile(path.join(repo, '.git', 'config'), '[core]\n\trepositoryformatversion = 0\n\tbare = false\n');
      // 外层 task exec / CI Runner 可能留下编号不定的环境变量配置：经 localGitEnv 全部清掉（0..N 与 GIT_CONFIG_COUNT
      // 一起），测试自己的用例从 0 开始编号；用例自己的 env（例如 GIT_CONFIG）叠在清理之后。
      const env: Record<string, string | undefined> = machineEnv({ HOME: path.join(root, 'nohome'), GIT_CONFIG_GLOBAL: glob, GIT_CONFIG_NOSYSTEM: '1', ...c.env });
      let n = 0;
      for (const [src, k, v] of c.entries) {
        if (src === 'global') await git(root, 'config', '--file', glob, '--add', k, v);
        else if (src === 'local') await git(repo, 'config', '--add', k, v);
        else {
          env[`GIT_CONFIG_KEY_${n}`] = k;
          env[`GIT_CONFIG_VALUE_${n}`] = v;
          env.GIT_CONFIG_COUNT = String(++n);
        }
      }
      seen.length = 0;
      conns.length = 0;
      await nodeExec('git', ['ls-remote', url], { cwd: repo, env });
      // 请求确实离开了 git、到了回环服务（没有被继承的包装或环境错误静默挡下，造成两种"没有请求头"都通过）
      expect(conns.length, c.name).toBeGreaterThan(0);
      const sent = seen.some((h) => h !== '');
      const found = (await authConflicts(url, { exec: nodeExec, env, cwd: repo, platform: process.platform })).some((x) => x.key === 'http.extraheader');
      if (c.conservative) expect([c.name, found], c.name).toEqual([c.name, true]);
      else expect([c.name, found], c.name).toEqual([c.name, sent]);
    }
  });

  it('helper 请求中 git 原样给出的主机（大写、显式默认端口）与任务仓库一致（真实 git 给出的字段）', async () => {
    const root = await tmp();
    // 把真实 CI Runner 的外层环境真正注入 process.env：外层更具体的 credential.https://github.com.helper 会压过 -c 给出的
    // helper。经 localGitEnv 清掉之后，写 dump 的 helper 才会被调用；清理逻辑被删掉时下面的 readFile 直接 ENOENT。
    stubOuterCiGitEnv();
    let seq = 0;
    const fields = async (url: string) => {
      // 每次调用用独立的 dump 文件：上一次的内容不会让这一次的读取假通过
      const dump = path.join(root, `dump-${seq++}`);
      await nodeExec('git', ['-c', 'credential.useHttpPath=true', '-c', `credential.helper=!f() { cat > '${dump}'; }; f`, '-c', 'core.askPass=', 'credential', 'fill'], {
        input: `url=${url}\n\n`,
        env: { ...localGitEnv(process.env), GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1', LC_ALL: 'C' },
      });
      const q = parseCredentialQuery(await readFile(dump, 'utf8'));
      expect(q.protocol, url).not.toBe('');
      expect(q.host, url).not.toBe('');
      expect(q.path, url).not.toBe('');
      return q;
    };
    const clone = 'https://github.com/acme/widgets.git';
    expect(credentialAllowed(await fields('https://GitHub.COM:443/ACME/Widgets'), clone, ['acme/widgets'])).toBe('acme/widgets');
    expect(credentialAllowed(await fields('https://github.com:8443/acme/widgets.git'), clone, ['acme/widgets'])).toBe('');
    expect(credentialAllowed(await fields('http://github.com:80/acme/widgets.git'), clone, ['acme/widgets'])).toBe('');
  });
});

/** connections 是服务端还开着的连接数。 */
function connections(srv: http.Server): Promise<number> {
  return new Promise((resolve, reject) => srv.getConnections((err, n) => (err ? reject(err) : resolve(n))));
}

describe('传输包装与连接器之间的长连接（openOp）', () => {
  it('回复后保持连接，客户端关闭时连接器调用 onClose 一次；没有操作时回复后即关闭', async () => {
    const dir = await tmp();
    const sock = path.join(dir, 's.sock');
    let closes = 0;
    const srv = await serveTools(
      sock,
      () => Promise.resolve({ code: 0, output: '' }),
      (req) => Promise.resolve(req.url === 'none' ? { code: 0, output: '', op: '' } : { code: 0, output: '', op: 'op-1', onClose: () => closes++ }),
    );
    cleanups.push(() => new Promise<void>((r) => srv.close(() => r())));
    const c = await openOp(sock, { attempt: '1', url: 'https://x/a/b.git', pid: '1' });
    expect(c.reply).toEqual({ code: 0, output: '', op: 'op-1' });
    expect(closes).toBe(0);
    c.close();
    await waitFor(() => closes === 1, 5_000, 'onClose');
    const n = await openOp(sock, { attempt: '1', url: 'none', pid: '1' });
    expect(n.reply.op).toBe('');
    n.close();
    expect(closes).toBe(1);
  });

  it('连接在回复之前就关闭：服务端确认断开后才放行 handler，拿到回复后立即调用 onClose 恰好一次', async () => {
    const dir = await tmp();
    const sock = path.join(dir, 's.sock');
    let closes = 0;
    let answer!: () => void;
    const answered = new Promise<void>((r) => (answer = r));
    let entered = false;
    const srv = await serveTools(
      sock,
      () => Promise.resolve({ code: 0, output: '' }),
      async () => {
        entered = true;
        await answered;
        return { code: 0, output: '', op: 'op-1', onClose: () => closes++ };
      },
    );
    cleanups.push(() => new Promise<void>((r) => srv.close(() => r())));
    // 客户端在 handler 回复之前断开（这里由超时触发，同包装被杀）；等服务端看到连接已经关闭再放行 handler
    const p = openOp(sock, { attempt: '1', url: 'u', pid: '1' }, 300);
    await waitFor(() => entered, 5_000, 'handler 开始');
    await expect(p).rejects.toThrow('超时');
    await waitFor(async () => (await connections(srv)) === 0, 5_000, '服务端看到断开');
    expect(closes).toBe(0);
    answer();
    await waitFor(() => closes === 1, 5_000, 'onClose');
    await new Promise((r) => setImmediate(r));
    expect(closes).toBe(1);
  });

  it('回复的第一行没有收齐就断开：立即失败（不等超时）；第一行超过上限时失败', async () => {
    const dir = await tmp();
    const sock = path.join(dir, 'raw.sock');
    let mode: 'cut' | 'big' = 'cut';
    const raw = createNetServer((c) => {
      c.once('data', () => {
        c.write('HTTP/1.1 200 OK\r\ncontent-type: application/x-ndjson\r\ntransfer-encoding: chunked\r\n\r\n');
        if (mode === 'cut') {
          c.write('a\r\n{"code":0,\r\n');
          c.destroy();
        } else {
          const big = `{"code":0,"output":"${'x'.repeat(maxOpReplyBytes)}","op":"o"}\n`;
          c.write(`${Buffer.byteLength(big).toString(16)}\r\n${big}\r\n`);
        }
      });
    });
    await new Promise<void>((r) => raw.listen(sock, r));
    cleanups.push(() => new Promise<void>((r) => raw.close(() => r())));
    const t0 = Date.now();
    await expect(openOp(sock, { attempt: '1', url: 'u', pid: '1' }, 60_000)).rejects.toThrow('没有回复完');
    expect(Date.now() - t0).toBeLessThan(10_000);
    mode = 'big';
    await expect(openOp(sock, { attempt: '1', url: 'u', pid: '1' }, 60_000)).rejects.toThrow('没有读完');
  });

  it('socket 路径超过平台上限时明确报错', async () => {
    const long = `/tmp/${'x'.repeat(maxSocketPathBytes)}.sock`;
    expect(() => checkSocketPath(long)).toThrow(`上限 ${maxSocketPathBytes} 字节`);
    await expect(openOp(long, { attempt: '1', url: 'u', pid: '1' })).rejects.toThrow('上限');
    await expect(serveTools(long, () => Promise.resolve({ code: 0, output: '' }))).rejects.toThrow('上限');
  });
});
