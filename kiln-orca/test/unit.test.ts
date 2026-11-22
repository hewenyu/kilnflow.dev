import { createPublicKey, sign, verify } from 'node:crypto';
import { existsSync } from 'node:fs';
import { mkdir, mkdtemp, readdir, readFile, realpath, rm, stat, utimes, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { Code, ConnectError, createClient, createRouterTransport, type Interceptor } from '@connectrpc/connect';
import { connectNodeAdapter, createConnectTransport } from '@connectrpc/connect-node';
import { AIRunnerService, FailedJobSchema } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { RunnerService } from '../src/gen/kiln/runner/v1/runner_pb.js';
import { describe, expect, it } from 'vitest';
import { AgentProbe, backoffMs, knownAgents } from '../src/core/agents.js';
import { substitute } from '../src/core/attempt.js';
import { type Exec, nodeExec } from '../src/core/exec.js';
import { Git, halfBuiltWorktreeRetry, heldLock } from '../src/core/git.js';
import { ErrorClass, InfraError, RevokedError, StaleError } from '../src/core/errors.js';
import { emptyBinding, Journal, type JournalRecord } from '../src/core/journal.js';
import { authMessage, ConnectKiln, ed25519Key, mapError, publicKey, retryable, serverDeclined, withRetry } from '../src/core/kiln.js';
import { LogShipper } from '../src/core/logship.js';
import { KeyedMutex } from '../src/core/mutex.js';
import { OrcaCli, OrcaError, parseOutput, worktreePathFromId, type OrcaMessage } from '../src/core/orca.js';
import { abortRecord, classifyEscalation, fatalStopReasons, formatAbortReason, maxAbortLogBytes, maxAbortMessageBytes, maxAbortMetaBytes, safeDispatchId, truncateUtf8 } from '../src/core/mailbox.js';
import { Redactor } from '../src/core/redact.js';
import { composeFile, DockerConfigError, DockerServices, homeOwner, parseDockerUserConfig, pickPorts, prepareDockerConfig, projectName, serviceDesc } from '../src/core/services.js';
import { Home, validateConfig, defaultConfig } from '../src/host/home.js';
import { definedHome, launchdPlist, systemdUnit } from '../src/host/service.js';
import { dailyLog, fileLogger, logRetentionMs, pruneLogs, serviceLogMaxBytes, serviceLogs } from '../src/host/logger.js';
import { callTool, maxToolReplyBytes, parseOpRequest, parseToolRequest, serveTools, ToolReplyTooLargeError } from '../src/host/socket.js';
import { ciLogTailBytes, formatVerification, maxCIOutputBytes } from '../src/core/verification.js';
import { create } from '@bufbuild/protobuf';
import { commitFile, faults, git, Origin, waitFor } from './harness.js';

describe('认证', () => {
  it('签名能用上传的公钥验证（与 Go 的 ed25519 相同）', () => {
    const seed = new Uint8Array(32).fill(7);
    const pub = publicKey(seed);
    expect(pub).toHaveLength(32);
    const msg = Buffer.from(authMessage(5n, 1700000000n, 'n'));
    expect(msg.toString()).toBe('kiln-runner-auth:5:1700000000:n');
    const sig = sign(null, msg, ed25519Key(seed));
    const spki = Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), Buffer.from(pub)]);
    expect(verify(null, msg, createPublicKey({ key: spki, format: 'der', type: 'spki' }), sig)).toBe(true);
  });

  it('错误约定：STALE_EPOCH、RUNNER_REVOKED', () => {
    expect(mapError(new ConnectError('STALE_EPOCH', Code.FailedPrecondition))).toBeInstanceOf(StaleError);
    expect(mapError(new ConnectError('RUNNER_REVOKED', Code.PermissionDenied))).toBeInstanceOf(RevokedError);
    // 原因必须完全一致（与 rpcerr.is 相同）
    expect(mapError(new ConnectError('STALE_EPOCH: x', Code.FailedPrecondition))).toBeInstanceOf(ConnectError);
    expect(retryable(new ConnectError('x', Code.Unavailable))).toBe(true);
    expect(retryable(new ConnectError('ALREADY_EXISTS', Code.AlreadyExists))).toBe(false);
    expect(retryable(new ConnectError('x', Code.InvalidArgument))).toBe(false);
    // 签发凭据的结果不明（操作已经关闭）：同一 op_id 不重试
    expect(retryable(new ConnectError('x', Code.Aborted))).toBe(false);
    expect(retryable(new StaleError())).toBe(false);
  });

  it('认证被拒绝（节点已被吊销）：RPC 报 RevokedError，不是被 Connect 包装后的普通错误', async () => {
    let revoked = false;
    let acquires = 0;
    const transport = (interceptors: Interceptor[]) =>
      createRouterTransport(
        (r) => {
          r.service(RunnerService, {
            authenticate: () => {
              if (revoked) throw new ConnectError('RUNNER_REVOKED', Code.PermissionDenied);
              // 很快过期：下一次调用重新认证
              return { accessToken: 't', expiresAt: BigInt(Math.floor(Date.now() / 1000)) };
            },
          });
          r.service(AIRunnerService, { acquire: () => (acquires++, {}) });
        },
        { transport: { interceptors } },
      );
    const kiln = new ConnectKiln(transport, 7n, new Uint8Array(32).fill(1));
    await kiln.acquire({ standby: true });
    revoked = true;
    await expect(kiln.acquire({ standby: true })).rejects.toBeInstanceOf(RevokedError);
    expect(acquires).toBe(1);
  });

  it('authenticateWith：调用前 signal 已中止时不发起认证，后台认证失败也不产生未处理拒绝', async () => {
    let auths = 0;
    let heartbeats = 0;
    const transport = (interceptors: Interceptor[]) =>
      createRouterTransport(
        (r) => {
          r.service(RunnerService, {
            authenticate: () => {
              auths++;
              throw new ConnectError('boom', Code.Internal);
            },
          });
          r.service(AIRunnerService, { heartbeat: () => (heartbeats++, {}) });
        },
        { transport: { interceptors } },
      );
    const kiln = new ConnectKiln(transport, 7n, new Uint8Array(32).fill(1));
    const unhandled: unknown[] = [];
    const onUnhandled = (e: unknown) => unhandled.push(e);
    process.on('unhandledRejection', onUnhandled);
    try {
      const ac = new AbortController();
      ac.abort();
      await expect(kiln.heartbeat({}, ac.signal)).rejects.toBeTruthy();
      // 给可能的后台拒绝一个被观察到的机会
      await new Promise((r) => setTimeout(r, 20));
    } finally {
      process.off('unhandledRejection', onUnhandled);
    }
    expect(auths).toBe(0);
    expect(heartbeats).toBe(0);
    expect(unhandled).toEqual([]);
  });

  it('真实的 Connect 传输：只有从 Server 回复中解析出的错误是 wire error（serverDeclined、认证被拒绝才按 Server 的结论处理）；网关的普通 502 / 403、连接断开都是客户端合成的', async () => {
    let mode: 'wire-unavailable' | 'plain-502' | 'drop' | 'wire-denied' | 'plain-403' = 'wire-unavailable';
    const srv = createServer((req, res) => {
      req.resume();
      req.on('end', () => {
        switch (mode) {
          case 'wire-unavailable':
            res.writeHead(503, { 'content-type': 'application/json' }).end(JSON.stringify({ code: 'unavailable', message: 'GitHub 确定没有签发' }));
            return;
          case 'wire-denied':
            res.writeHead(403, { 'content-type': 'application/json' }).end(JSON.stringify({ code: 'permission_denied', message: 'RUNNER_REVOKED' }));
            return;
          case 'plain-502':
            res.writeHead(502, { 'content-type': 'text/plain' }).end('Bad Gateway');
            return;
          case 'plain-403':
            res.writeHead(403, { 'content-type': 'text/html' }).end('<h1>Forbidden</h1>');
            return;
          case 'drop':
            req.socket.destroy();
        }
      });
    });
    await new Promise<void>((r) => srv.listen(0, '127.0.0.1', r));
    try {
      const baseUrl = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`;
      const transport = (interceptors: Interceptor[] = []) => createConnectTransport({ baseUrl, httpVersion: '1.1', interceptors });
      const ai = createClient(AIRunnerService, transport());
      const issue = () => ai.issueCredential({ opId: 'op-1', repository: 'acme/widgets' }).then(
        () => undefined,
        (err: unknown) => err,
      );
      const wire = await issue();
      expect(wire).toBeInstanceOf(ConnectError);
      expect([(wire as ConnectError).code, (wire as ConnectError).isWireError, serverDeclined(wire)]).toEqual([Code.Unavailable, true, true]);
      mode = 'plain-502';
      const bad = await issue();
      expect([(bad as ConnectError).code, (bad as ConnectError).isWireError, serverDeclined(bad)]).toEqual([Code.Unavailable, false, false]);
      mode = 'drop';
      const dropped = await issue();
      expect(dropped).toBeInstanceOf(ConnectError);
      expect([(dropped as ConnectError).isWireError, serverDeclined(dropped)]).toEqual([false, false]);
      // 认证：只有 Server 发来的 PERMISSION_DENIED 才说明节点被吊销
      const kiln = new ConnectKiln(transport, 7n, new Uint8Array(32).fill(1));
      mode = 'plain-403';
      const proxy = await kiln.authenticate().then(
        () => undefined,
        (err: unknown) => err,
      );
      expect(proxy).not.toBeInstanceOf(RevokedError);
      // 按暂时性错误交回：各调用方照常重试
      expect((proxy as ConnectError).code).toBe(Code.Unavailable);
      expect(retryable(proxy)).toBe(true);
      mode = 'wire-denied';
      await expect(kiln.authenticate()).rejects.toBeInstanceOf(RevokedError);
    } finally {
      srv.closeAllConnections();
      await new Promise<void>((r) => srv.close(() => r()));
    }
  });

  it('认证层（真实的 Connect 传输）：Kiln 明确的鉴权拒绝时重新认证、重放一次；代理的普通 401 不重放，错误交回调用方', async () => {
    let mode: 'proxy-401' | 'wire-401' = 'proxy-401';
    let auths = 0;
    let issues = 0;
    const adapter = connectNodeAdapter({
      routes: (r) => {
        r.service(RunnerService, {
          authenticate: () => (auths++, { accessToken: `t${auths}`, expiresAt: BigInt(Math.floor(Date.now() / 1000) + 3600) }),
        });
        r.service(AIRunnerService, {
          issueCredential: () => {
            issues++;
            // 第一次被 Kiln 的鉴权拒绝（处理函数之前，没有签发）
            if (mode === 'wire-401' && issues === 1) throw new ConnectError('token 无效', Code.Unauthenticated);
            return { credential: { secret: 's', username: 'x-access-token' } };
          },
        });
      },
    });
    const srv = createServer((req, res) => {
      if (mode === 'proxy-401' && req.url?.endsWith('/IssueCredential')) {
        // 请求到达了 Server（签发了），代理却回复普通的 401
        issues++;
        req.resume();
        req.on('end', () => res.writeHead(401, { 'content-type': 'text/plain' }).end('Unauthorized'));
        return;
      }
      adapter(req, res);
    });
    await new Promise<void>((r) => srv.listen(0, '127.0.0.1', r));
    try {
      const baseUrl = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`;
      const transport = (interceptors: Interceptor[] = []) => createConnectTransport({ baseUrl, httpVersion: '1.1', interceptors });
      const proxied = new ConnectKiln(transport, 7n, new Uint8Array(32).fill(1));
      const err = await proxied.issueCredential({ opId: 'op-1', repository: 'acme/widgets' }).then(
        () => undefined,
        (e: unknown) => e,
      );
      expect(err).toBeInstanceOf(ConnectError);
      expect([(err as ConnectError).code, (err as ConnectError).isWireError]).toEqual([Code.Unauthenticated, false]);
      expect([issues, auths]).toEqual([1, 1]);
      mode = 'wire-401';
      issues = 0;
      auths = 0;
      const wired = new ConnectKiln(transport, 7n, new Uint8Array(32).fill(1));
      const r = await wired.issueCredential({ opId: 'op-1', repository: 'acme/widgets' });
      expect(r.credential?.secret).toBe('s');
      expect([issues, auths]).toEqual([2, 2]);
    } finally {
      srv.closeAllConnections();
      await new Promise<void>((r) => srv.close(() => r()));
    }
  });

  it('同时认证只发起一次；一个调用方停止只结束它自己的等待，共享的认证照常完成（审计）', async () => {
    let auths = 0;
    let releaseAuth!: () => void;
    const gate = new Promise<void>((r) => (releaseAuth = r));
    const acquired: string[] = [];
    const transport = (interceptors: Interceptor[]) =>
      createRouterTransport(
        (r) => {
          r.service(RunnerService, {
            authenticate: async () => {
              auths++;
              await gate;
              return { accessToken: 't', expiresAt: BigInt(Math.floor(Date.now() / 1000) + 3600) };
            },
          });
          r.service(AIRunnerService, {
            acquire: (req) => {
              acquired.push(req.requestId ?? '');
              return {};
            },
          });
        },
        { transport: { interceptors } },
      );
    const kiln = new ConnectKiln(transport, 7n, new Uint8Array(32).fill(1));
    const ac = new AbortController();
    const stopped = kiln.acquire({ requestId: 'a', standby: true }, ac.signal).then(
      () => undefined,
      (err: unknown) => err,
    );
    const kept = kiln.acquire({ requestId: 'b', standby: true });
    await waitFor(() => auths === 1, 5_000, '开始认证');
    ac.abort();
    expect(await stopped).toBeInstanceOf(Error);
    // 共享的认证还在进行，没有被一个调用方的停止打断
    expect(auths).toBe(1);
    releaseAuth();
    await expect(kept).resolves.toBeDefined();
    // 只认证了一次，之后 token 还有效
    expect(auths).toBe(1);
    expect(acquired).toEqual(['b']);
    await kiln.acquire({ requestId: 'c', standby: true });
    expect(auths).toBe(1);
  });

  it('withRetry 只重试可重试的错误', async () => {
    let n = 0;
    await expect(
      withRetry(
        () => {
          n++;
          return Promise.reject(new ConnectError('x', Code.InvalidArgument));
        },
        { attempts: 5, baseMs: 1 },
      ),
    ).rejects.toThrow();
    expect(n).toBe(1);
    n = 0;
    const v = await withRetry(() => (++n < 3 ? Promise.reject(new Error('net')) : Promise.resolve(n)), { attempts: 5, baseMs: 1 });
    expect(v).toBe(3);
  });

  it('withRetry 在等待重试时 signal 结束：不再发起下一次', async () => {
    const ac = new AbortController();
    let n = 0;
    const p = withRetry(
      () => {
        n++;
        return Promise.reject(new Error('net'));
      },
      { attempts: 0, baseMs: 60_000, signal: ac.signal },
    );
    await new Promise((r) => setTimeout(r, 20));
    ac.abort();
    await expect(p).rejects.toThrow('net');
    expect(n).toBe(1);
  });
});

describe('Orca 输出', () => {
  it('跳过 keepalive 行，ok:false 转为 OrcaError', () => {
    expect(parseOutput('{"_keepalive":true}\n{"_keepalive":true}\n{"ok":true,"result":{"a":1}}\n')).toEqual({ a: 1 });
    expect(() => parseOutput('{"ok":false,"error":{"code":"no_active_sender_terminal","message":"m"}}')).toThrow(
      expect.objectContaining({ code: 'no_active_sender_terminal' }),
    );
    expect(() => parseOutput('')).toThrow(expect.objectContaining({ code: 'empty_output' }));
    expect(() => parseOutput('oops')).toThrow(expect.objectContaining({ code: 'bad_output' }));
    expect(worktreePathFromId('repo-1::/x/y')).toBe('/x/y');
  });

  it('exec 失败且没有输出时是 exec_failed（结果未知）', async () => {
    const o = new OrcaCli(() => Promise.resolve({ code: 1, stdout: '', stderr: 'boom' }), 'orca');
    await expect(o.workerRelease('d')).rejects.toEqual(expect.objectContaining({ code: 'exec_failed' }));
    expect(new OrcaError('x', 'y').message).toContain('x');
  });
});

describe('Agent 检测', () => {
  it('失败后退避', async () => {
    const now = 1_000;
    const exec: Exec = (cmd) => Promise.resolve(cmd === 'claude' ? { code: 127, stdout: '', stderr: '' } : { code: 0, stdout: 'codex 1.2\n', stderr: '' });
    const p = new AgentProbe(exec, knownAgents, () => now);
    await p.refresh(true);
    const s = Object.fromEntries(p.snapshot().map((a) => [a.name, a]));
    expect(s.codex?.available).toBe(true);
    expect(s.codex?.version).toBe('codex 1.2');
    expect(s['claude-code']?.installed).toBe(false);
    p.failed('codex');
    expect(p.snapshot().find((a) => a.name === 'codex')?.backoffUntil).toBe(now + backoffMs);
    // 命令自己以 127 退出（end=exit）说明安装了、只是不能用；只有没能启动（notrun）才是没有安装（审计）
    const exit127: Exec = (cmd) => Promise.resolve(cmd === 'claude' ? { code: 127, stdout: '', stderr: 'x', end: 'exit' } : { code: 0, stdout: 'codex 1.2\n', stderr: '' });
    const q = new AgentProbe(exit127, knownAgents, () => now);
    await q.refresh(true);
    expect(q.snapshot().find((a) => a.name === 'claude-code')).toMatchObject({ installed: true, available: false, reason: 'claude --version 失败：x' });
  });
});

describe('测试服务', () => {
  it('compose 只绑定 127.0.0.1，占用的端口换成空闲端口', async () => {
    const s = { docker: 'docker', postgresImage: 'postgres:17', redisImage: 'redis:7', postgresPort: 55432, redisPort: 56379 };
    const f = composeFile(s, ['pg', 'redis'], 'abc123');
    expect(f).toContain('127.0.0.1:55432:5432');
    expect(f).toContain('127.0.0.1:56379:6379');
    const { s: got, notes } = await pickPorts(s, ['pg'], (p) => Promise.resolve(p === 55432));
    expect(got.postgresPort).not.toBe(55432);
    expect(got.redisPort).toBe(56379);
    expect(notes).toHaveLength(1);
    expect(serviceDesc(got, ['pg'])[0]).toContain(`127.0.0.1:${got.postgresPort}`);
    // 容器带数据目录的标签（回收时只认自己的）
    expect(f.match(/kiln\.orca\.home: "abc123"/g)).toHaveLength(2);
  });

  it('回收只删除本数据目录、不在保留集合中的项目；Docker 中其他数据目录和其他程序的项目不动', async () => {
    const homeA = homeOwner('/data/a');
    const homeB = homeOwner('/data/b');
    expect(homeA).not.toBe(homeB);
    expect(homeOwner('/data/a/../a')).toBe(homeA);
    // 假的 docker：按标签过滤容器，down 删除项目的容器
    const containers = [
      { project: projectName(homeA, 102n), home: homeA },
      { project: projectName(homeA, 100n), home: homeA },
      { project: projectName(homeA, 100n), home: homeA },
      { project: projectName(homeA, 101n), home: homeA },
      { project: projectName(homeB, 100n), home: homeB },
      { project: 'other-app', home: homeA },
      { project: 'kiln-ai-7', home: '' },
    ];
    const downs: string[] = [];
    let failDown = '';
    const exec: Exec = (_cmd, args) => {
      const a = [...args];
      if (a[0] === 'ps') {
        const filter = a[a.indexOf('--filter') + 1] ?? '';
        const m = /^label=kiln\.orca\.home=(.+)$/.exec(filter);
        const proj = /^label=com\.docker\.compose\.project=(.+)$/.exec(filter);
        const hit = containers.filter((c) => (m ? c.home === m[1] : proj ? c.project === proj[1] : false));
        return Promise.resolve({ code: 0, stdout: hit.map((c) => (m ? c.project : 'id')).join('\n') + '\n', stderr: '' });
      }
      if (a[0] === 'compose' && a.includes('down')) {
        const p = a[a.indexOf('-p') + 1] ?? '';
        downs.push(p);
        if (p === failDown) return Promise.resolve({ code: 1, stdout: '', stderr: 'daemon 不可用' });
        for (let i = containers.length - 1; i >= 0; i--) if (containers[i]?.project === p) containers.splice(i, 1);
        return Promise.resolve({ code: 0, stdout: '', stderr: '' });
      }
      return Promise.resolve({ code: 1, stdout: '', stderr: `未模拟 ${a.join(' ')}` });
    };
    const settings = { docker: 'docker', postgresImage: 'postgres:17', redisImage: 'redis:7', postgresPort: 55432, redisPort: 56379 };
    const a = new DockerServices(exec, settings, () => Promise.resolve({}), homeA);
    failDown = projectName(homeA, 102n);
    await expect(a.cleanup(new Set(['101']))).rejects.toThrow(/daemon 不可用/);
    // 一个项目失败不影响其他项目；保留的、别的数据目录的、别的程序的都不动
    expect(downs.sort()).toEqual([projectName(homeA, 100n), projectName(homeA, 102n)].sort());
    expect(containers.map((c) => c.project).sort()).toEqual([projectName(homeA, 101n), projectName(homeA, 102n), projectName(homeB, 100n), 'kiln-ai-7', 'other-app'].sort());
    failDown = '';
    await a.cleanup(new Set());
    expect(containers.map((c) => c.project).sort()).toEqual([projectName(homeB, 100n), 'kiln-ai-7', 'other-app'].sort());
  });
});

describe('专用 Docker 配置', () => {
  const settings = { docker: 'docker', postgresImage: 'postgres:17', redisImage: 'redis:7', postgresPort: 55432, redisPort: 56379 };

  /** dockerExec 记录每次 docker 调用的参数和环境变量，命令总是成功。 */
  function dockerExec() {
    const calls: { args: string[]; env: Record<string, string | undefined> | undefined }[] = [];
    const exec: Exec = (_cmd, args, opts) => {
      calls.push({ args: [...args], env: opts?.env });
      return Promise.resolve({ code: 0, stdout: args[0] === 'info' ? '27.0.0\n' : '', stderr: '' });
    };
    return { calls, exec };
  }

  it('生成的配置不带凭据，沿用 currentContext、插件目录和 contexts', async () => {
    const root = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-docker-'));
    try {
      const user = path.join(root, 'user');
      await mkdir(path.join(user, 'contexts'), { recursive: true });
      await writeFile(path.join(user, 'config.json'), JSON.stringify({ auths: { 'ghcr.io': { auth: 'c2VjcmV0' } }, credsStore: 'osxkeychain', currentContext: 'colima' }));
      const dir = path.join(root, 'out');
      const env = await prepareDockerConfig(dir, user);
      expect(env.DOCKER_CONFIG).toBe(dir);
      const cfg = JSON.parse(await readFile(path.join(dir, 'config.json'), 'utf8')) as Record<string, unknown>;
      expect(cfg).toEqual({ auths: { 'kiln-no-credentials.invalid': {} }, currentContext: 'colima', cliPluginsExtraDirs: [path.join(user, 'cli-plugins')] });
      expect(await realpath(path.join(dir, 'contexts'))).toBe(await realpath(path.join(user, 'contexts')));
      expect((await stat(dir)).mode & 0o777).toBe(0o700);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });

  it('用户的 config.json 损坏：生成失败写明哪个文件、哪一步；Docker 不可用，不执行任何 docker 命令；修好后下一次检测恢复', async () => {
    const root = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-docker-'));
    try {
      const user = path.join(root, 'user');
      await mkdir(user);
      const userFile = path.join(user, 'config.json');
      await writeFile(userFile, '{');
      const dir = path.join(root, 'out');
      await expect(prepareDockerConfig(dir, user)).rejects.toThrow(DockerConfigError);
      await expect(prepareDockerConfig(dir, user)).rejects.toThrow(`${userFile} 不是有效的 JSON`);

      const { calls, exec } = dockerExec();
      let prepares = 0;
      const docker = new DockerServices(
        exec,
        settings,
        () => {
          prepares++;
          return prepareDockerConfig(dir, user);
        },
        homeOwner(root),
      );
      const st = await docker.available();
      expect(st.ok).toBe(false);
      expect(st.reason).toContain('无法生成专用的 Docker 客户端配置');
      expect(st.reason).toContain(`${userFile} 不是有效的 JSON`);
      // 没有专用配置时任何操作都不执行 docker（不能回退到用户自己的配置和凭据）
      await expect(docker.start(5n, root, ['pg'])).rejects.toThrow(DockerConfigError);
      await expect(docker.stop(projectName(homeOwner(root), 5n))).rejects.toThrow(DockerConfigError);
      await expect(docker.cleanup(new Set())).rejects.toThrow(DockerConfigError);
      expect(calls).toEqual([]);
      // 还没有专用配置时每次操作（包括 start 失败后的 stop）都重新尝试生成
      expect(prepares).toBe(5);

      // 修好之后：下一次检测生成配置并恢复可用，之后沿用（不再改写配置文件）
      await writeFile(userFile, '{"currentContext": "default"}');
      expect(await docker.available()).toEqual({ ok: true, reason: '' });
      await docker.cleanup(new Set());
      expect(await docker.available()).toEqual({ ok: true, reason: '' });
      expect(prepares).toBe(6);
      expect(calls.map((c) => c.args[0])).toEqual(['info', 'compose', 'ps', 'info', 'compose']);
      for (const c of calls) expect(c.env?.DOCKER_CONFIG).toBe(dir);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });

  it('同时检测时共用同一次生成', async () => {
    const { exec } = dockerExec();
    let prepares = 0;
    let finish: (env: Record<string, string>) => void = () => undefined;
    const docker = new DockerServices(
      exec,
      settings,
      () => {
        prepares++;
        return new Promise((resolve) => (finish = resolve));
      },
      'abc',
    );
    const both = Promise.all([docker.available(), docker.available()]);
    await waitFor(() => prepares > 0, 1_000, '开始生成');
    finish({ DOCKER_CONFIG: '/x' });
    expect(await both).toEqual([
      { ok: true, reason: '' },
      { ok: true, reason: '' },
    ]);
    expect(prepares).toBe(1);
  });
});

  it('用户配置中要沿用的字段类型不对时明确报错，不把数字、null 当成字符串（审计）', () => {
    expect(parseDockerUserConfig({ currentContext: 'default', cliPluginsExtraDirs: ['/x'], HttpHeaders: { a: 'b' }, credsStore: 'osxkeychain' })).toEqual({
      currentContext: 'default',
      cliPluginsExtraDirs: ['/x'],
      HttpHeaders: { a: 'b' },
    });
    expect(parseDockerUserConfig({ auths: { 'ghcr.io': { auth: 'x' } } })).toEqual({});
    for (const bad of [null, [], 1, 'x']) expect(() => parseDockerUserConfig(bad)).toThrow('顶层不是 JSON 对象');
    expect(() => parseDockerUserConfig({ currentContext: 1 })).toThrow('currentContext 应为字符串');
    expect(() => parseDockerUserConfig({ cliPluginsExtraDirs: '/x' })).toThrow('cliPluginsExtraDirs 应为字符串数组');
    expect(() => parseDockerUserConfig({ cliPluginsExtraDirs: ['/x', 2] })).toThrow('cliPluginsExtraDirs 应为字符串数组');
    expect(() => parseDockerUserConfig({ HttpHeaders: null })).toThrow('HttpHeaders 应为字符串到字符串的对象');
    expect(() => parseDockerUserConfig({ HttpHeaders: ['a'] })).toThrow('HttpHeaders 应为字符串到字符串的对象');
    expect(() => parseDockerUserConfig({ HttpHeaders: { a: 1 } })).toThrow('HttpHeaders 应为字符串到字符串的对象');
  });

  it('要沿用的字段类型不对时生成失败，写明文件和原因', async () => {
    const root = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-docker-'));
    try {
      const user = path.join(root, 'user');
      await mkdir(user);
      const userFile = path.join(user, 'config.json');
      await writeFile(userFile, JSON.stringify({ cliPluginsExtraDirs: 'x' }));
      const err = await prepareDockerConfig(path.join(root, 'out'), user).then(
        () => undefined,
        (e: unknown) => e,
      );
      expect(err).toBeInstanceOf(DockerConfigError);
      expect((err as Error).message).toContain(userFile);
      expect((err as Error).message).toContain('cliPluginsExtraDirs 应为字符串数组');
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });

describe('git 锁', () => {
  /** prepareLocked 让 prepareClone 因为已经存在的锁失败，返回错误（锁文件不能被删除）。 */
  async function prepareLocked(root: string, lockRel: string): Promise<{ err: unknown; lock: string }> {
    const origin = await Origin.create(root);
    await origin.commit('gone.txt', 'x\n', 'gone');
    const clone = path.join(root, 'repos', '42', 'widgets');
    const g = new Git(nodeExec);
    await g.prepareClone(clone, origin.bare, [await origin.branchSha('master')]);
    // 远端删掉分支：再次 fetch --prune 要删除 refs/remotes/origin/gone，需要 packed-refs 的锁
    await git(origin.bare, 'update-ref', '-d', 'refs/heads/gone');
    // 配置已经是要求的值时准备仓库不写配置（不碰 config.lock）：先改掉一项，让它必须写
    if (lockRel === 'config.lock') await git(clone, 'config', 'user.name', 'someone-else');
    const lock = path.join(clone, '.git', lockRel);
    await writeFile(lock, '');
    const err = await g.prepareClone(clone, origin.bare, [await origin.branchSha('master')]).then(
      () => undefined,
      (e: unknown) => e,
    );
    return { err, lock };
  }

  // 路径中有单引号和空格时（git 原样放在单引号中，不转义），提示的也必须是真实存在的锁文件
  for (const { dirName, prefix } of [
    { dirName: '普通路径', prefix: 'kiln-orca-locks-' },
    { dirName: '路径含单引号和空格', prefix: "kiln-orca-locks q'uote 's " },
  ]) {
    for (const lockRel of ['packed-refs.lock', 'config.lock']) {
      it(`准备仓库时不删除已经存在的 git 锁（${lockRel}，${dirName}），错误写明锁的路径和处理方法`, async () => {
        const root = await mkdtemp(path.join(os.tmpdir(), prefix));
        try {
          const { err, lock } = await prepareLocked(root, lockRel);
          expect(existsSync(lock)).toBe(true);
          expect(err).toBeInstanceOf(InfraError);
          expect((err as InfraError).errorClass).toBe(ErrorClass.fetch);
          const msg = (err as InfraError).message;
          const hinted = /git 的锁文件 (.+) 已经存在（/.exec(msg)?.[1];
          expect(hinted, msg).toBeDefined();
          // git 给出的路径可能是解析了符号链接的（macOS 的 /var → /private/var）
          expect(await realpath(hinted!)).toBe(await realpath(lock));
          expect(msg).toContain('确认没有 git 进程在使用仓库');
        } finally {
          await rm(root, { recursive: true, force: true });
        }
      });
    }
  }

  it('连接器的 git 用 LC_ALL=C（错误输出是英文，heldLock 才能认出锁）', async () => {
    const envs: Array<Record<string, string | undefined> | undefined> = [];
    const exec: Exec = (_cmd, _args, opts) => {
      envs.push(opts?.env);
      return Promise.resolve({ code: 0, stdout: 'abc\n', stderr: '' });
    };
    await new Git(exec).run('/r', 'rev-parse', 'HEAD');
    expect(envs[0]).toEqual(expect.objectContaining({ LC_ALL: 'C', GIT_TERMINAL_PROMPT: '0' }));
  });

  it('heldLock 识别 git 的锁错误', () => {
    expect(heldLock('/r', "fatal: Unable to create '/r/.git/index.lock': File exists.")).toBe('/r/.git/index.lock');
    expect(heldLock('/r', "error: cannot lock ref 'refs/heads/x': Unable to create '/r/.git/refs/heads/x.lock': File exists.")).toBe('/r/.git/refs/heads/x.lock');
    expect(heldLock('/r', 'error: could not lock config file .git/config: File exists')).toBe('/r/.git/config.lock');
    expect(heldLock('/r', 'fatal: unable to access remote')).toBeUndefined();
    // 路径中的单引号、空格：贪婪匹配到 ': File exists
    expect(heldLock('/r', "fatal: Unable to create '/t/q'uote 's/r/.git/packed-refs.lock': File exists.\n\nAnother git process seems to be running")).toBe(
      "/t/q'uote 's/r/.git/packed-refs.lock",
    );
    expect(heldLock('/r', "error: could not lock config file /t/q'uote 's/r/.git/config: File exists")).toBe("/t/q'uote 's/r/.git/config.lock");
    // 认不出完整路径（同一行两处、其它语言的输出）时不给出路径
    expect(heldLock('/r', "Unable to create '/a.lock': File exists Unable to create '/b.lock': File exists")).toBeUndefined();
    expect(heldLock('/r', "致命错误：无法创建 '/r/.git/index.lock'：文件已存在。")).toBeUndefined();
  });
});

describe('fetchOk 与中止', () => {
  /** fetchOk 是 prepareClone 内部的方法：直接取出来验证，不经过完整的准备流程。 */
  function fetchOk(g: Git) {
    return (
      g as unknown as {
        fetchOk(dir: string, url: string, auth: undefined, signal: AbortSignal | undefined, ...refspecs: string[]): Promise<boolean>;
      }
    ).fetchOk.bind(g);
  }

  it('signal 结束时 fetchOk 抛出：中止不是 ref 拿不到，调用方不能当成失败继续执行', async () => {
    const ac = new AbortController();
    const exec: Exec = () => {
      ac.abort();
      return Promise.resolve({ code: 128, stdout: '', stderr: 'fatal: 远端不可用' });
    };
    await expect(fetchOk(new Git(exec))('/r', 'https://example.com/x.git', undefined, ac.signal, 'abc')).rejects.toThrow('远端不可用');
  });

  it('没有中止时命令失败仍返回 false（例如 PR 的 ref 已经不存在）', async () => {
    const exec: Exec = () => Promise.resolve({ code: 128, stdout: '', stderr: 'fatal: 远端不可用' });
    await expect(fetchOk(new Git(exec))('/r', 'https://example.com/x.git', undefined, undefined, 'abc')).resolves.toBe(false);
  });
});

describe('共用 clone 中正在建的 worktree', () => {
  /**
   * setup 准备好一个 clone（与 Orca 共用的那种），在其中建一个 worktree，然后 origin 上有新的 commit（fetch 要下载对象，
   * 会做连接检查）。fetches 记下之后每次 git fetch 的参数；inject 返回非 undefined 时代替真实的 git 回复这次调用。
   */
  async function setup(root: string, inject?: (args: string[]) => { code: number; stdout: string; stderr: string } | undefined) {
    const origin = await Origin.create(root);
    const clone = path.join(root, 'repos', '42', 'widgets');
    const fetches: string[][] = [];
    let after: ((args: string[]) => Promise<void>) | undefined;
    // 准备好之后才注入
    let armed = false;
    const exec: Exec = async (cmd, args, opts) => {
      const a = [...args];
      if (a[0] === 'fetch') fetches.push(a);
      const r = (armed && a[0] === 'fetch' && inject?.(a)) || (await nodeExec(cmd, args, opts));
      if (a[0] === 'fetch') await after?.(a);
      return r;
    };
    const g = new Git(exec);
    await g.prepareClone(clone, origin.bare, [await origin.branchSha('master')]);
    const wt = path.join(root, 'wt-a');
    await git(clone, 'worktree', 'add', '--quiet', '--detach', wt, 'HEAD');
    const head = path.join(clone, '.git', 'worktrees', 'wt-a', 'HEAD');
    const valid = await readFile(head, 'utf8');
    const sha = await origin.commit('new.txt', 'x\n');
    fetches.length = 0;
    armed = true;
    return {
      g,
      clone,
      origin,
      sha,
      fetches,
      /** halfBuilt 让 worktree 的 HEAD 指向不存在的对象（与正在建的 worktree 相同的状态）；restore 恢复。 */
      halfBuilt: () => writeFile(head, '1234567890123456789012345678901234567890\n'),
      restore: () => writeFile(head, valid),
      afterFetch: (f: (args: string[]) => Promise<void>) => (after = f),
    };
  }

  async function withRoot(f: (root: string) => Promise<void>): Promise<void> {
    const root = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-wt-'));
    try {
      await f(root);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  }

  const raceOutput = (name: string) => ({ code: 1, stdout: '', stderr: `fatal: bad object worktrees/${name}/HEAD\nerror: origin did not send all necessary objects\n` });

  it('fetch 时有建了一半的 worktree（真实的 git 状态）：等它建好后重试成功', async () => {
    await withRoot(async (root) => {
      const s = await setup(root);
      await s.halfBuilt();
      // 第一次 fetch 失败之后，worktree 建好了
      s.afterFetch(async () => {
        if (s.fetches.length === 1) await s.restore();
      });
      expect(await s.g.prepareClone(s.clone, s.origin.bare, [s.sha])).toEqual([]);
      expect(s.fetches).toHaveLength(2);
      expect(s.fetches[1]).toEqual(s.fetches[0]);
      expect(await s.g.hasCommit(s.clone, s.sha)).toBe(true);
    });
  });

  it('一直是建了一半的：重试有限次后 infra.fetch，带原始错误', async () => {
    await withRoot(async (root) => {
      const s = await setup(root);
      await s.halfBuilt();
      const err = await s.g.prepareClone(s.clone, s.origin.bare, [s.sha]).catch((e: unknown) => e);
      expect(err).toBeInstanceOf(InfraError);
      expect((err as InfraError).errorClass).toBe(ErrorClass.fetch);
      expect((err as InfraError).message).toContain('fatal: bad object worktrees/wt-a/HEAD');
      expect(s.fetches).toHaveLength(halfBuiltWorktreeRetry.retries + 1);
    });
  });

  it.each(['fatal: bad object refs/remotes/origin/x', 'fatal: bad object HEAD', 'fatal: bad object worktrees/wt-a/ORIG_HEAD'])('其它的 bad object（%s）不重试', async (stderr) => {
    await withRoot(async (root) => {
      const s = await setup(root, () => ({ code: 128, stdout: '', stderr }));
      const err = await s.g.prepareClone(s.clone, s.origin.bare, [s.sha]).catch((e: unknown) => e);
      expect(err).toBeInstanceOf(InfraError);
      expect((err as InfraError).message).toContain(stderr);
      expect(s.fetches).toHaveLength(1);
    });
  });

  it('获取 PR 的 head、按 sha 获取同样重试', async () => {
    await withRoot(async (root) => {
      // 只在 refs/pull/7/head 上的 commit：fetch 全部分支拿不到
      let once = true;
      const s = await setup(root, (a) => {
        if (!a.includes('--prune') && once) {
          once = false;
          return raceOutput('kiln-a9-e1');
        }
        return undefined;
      });
      const pr = await commitFile(s.origin.work, 'pr.txt', 'pr\n', 'pr');
      await git(s.origin.work, 'push', '--quiet', s.origin.bare, 'HEAD:refs/pull/7/head');
      await git(s.origin.work, 'reset', '--quiet', '--hard', 'HEAD~1');
      expect(await s.g.prepareClone(s.clone, s.origin.bare, [pr], 7)).toEqual([]);
      expect(s.fetches.filter((a) => a.some((x) => x.includes('refs/pull/7/head')))).toHaveLength(2);

      // 按 sha 获取（不经过 PR 的 ref）
      const other = await commitFile(s.origin.work, 'pr2.txt', 'pr2\n', 'pr2');
      await git(s.origin.work, 'push', '--quiet', s.origin.bare, 'HEAD:refs/pull/8/head');
      once = true;
      s.fetches.length = 0;
      expect(await s.g.prepareClone(s.clone, s.origin.bare, [other])).toEqual([]);
      expect(s.fetches.filter((a) => a.at(-1) === other)).toHaveLength(2);
    });
  });
});

describe('命令超时', () => {
  // 子进程启动后才装好 SIGTERM 处理；超时要留够 node 启动的时间
  const timeoutMs = 2_000;

  it('超时先 SIGTERM：子进程清理自己的文件后退出，不等宽限期，退出码 124', async () => {
    const root = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-exec-'));
    try {
      const file = path.join(root, 'held.lock');
      const script = `const fs = require('node:fs'); fs.writeFileSync(process.argv[1], '');
process.on('SIGTERM', () => { fs.rmSync(process.argv[1]); process.exit(0); });
setInterval(() => {}, 1000);`;
      const graceMs = 60_000;
      const start = Date.now();
      const r = await nodeExec(process.execPath, ['-e', script, file], { timeoutMs, killGraceMs: graceMs });
      expect(r.code).toBe(124);
      expect(r.stderr).toContain(`超时（${timeoutMs} ms）`);
      // SIGKILL 不会给子进程清理的机会
      expect(existsSync(file)).toBe(false);
      expect(Date.now() - start).toBeLessThan(graceMs / 2);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });

  it('子进程忽略 SIGTERM 时宽限期后 SIGKILL，进程退出后返回 124', { timeout: 10_000 }, async () => {
    const script = `process.on('SIGTERM', () => {}); console.log(process.pid); setInterval(() => {}, 1000);`;
    const r = await nodeExec(process.execPath, ['-e', script], { timeoutMs, killGraceMs: 300 });
    expect(r.code).toBe(124);
    expect(r.stderr).toContain(`超时（${timeoutMs} ms）`);
    const pid = Number(r.stdout.trim());
    expect(pid).toBeGreaterThan(0);
    expect(() => process.kill(pid, 0)).toThrow();
  });

  it('子进程在期限内退出时照常返回退出码和输出', async () => {
    const r = await nodeExec(process.execPath, ['-e', "process.stdout.write('ok'); process.exit(3)"], { timeoutMs, killGraceMs: 300 });
    expect(r).toEqual({ code: 3, stdout: 'ok', stderr: '', end: 'exit' });
  });
});

describe('命令的结束（超时、中止、孙进程占着管道）', () => {
  const timeoutMs = 2_000;
  /** 直接子进程启动继承 stdout/stderr 的孙进程（pid 写进 argv[1]），输出 child done 后立即退出。 */
  const orphanScript = `const { spawn } = require('node:child_process'); const fs = require('node:fs');
const c = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], { stdio: ['ignore', 'inherit', 'inherit'] });
fs.writeFileSync(process.argv[1], String(c.pid)); process.stdout.write('child done'); process.exit(0);`;
  /** 忽略 SIGTERM 的子进程：pid 写进 argv[1]，收到 SIGTERM 时写 argv[2]。 */
  const stubbornScript = `const fs = require('node:fs'); process.on('SIGTERM', () => fs.writeFileSync(process.argv[2], 'term'));
fs.writeFileSync(process.argv[1], String(process.pid)); setInterval(() => {}, 1000);`;

  /** withDir 给测试一个临时目录，结束时 SIGKILL 目录中 pid 文件记下的全部进程（测试启动的子孙进程）。 */
  async function withDir(f: (dir: string) => Promise<void>): Promise<void> {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-exec-'));
    try {
      await f(dir);
    } finally {
      for (const name of ['pid', 'grandchild']) {
        const pid = Number(await readFile(path.join(dir, name), 'utf8').catch(() => ''));
        if (pid > 0) {
          try {
            process.kill(pid, 'SIGKILL');
          } catch {
            // 已经退出
          }
        }
      }
      await rm(dir, { recursive: true, force: true });
    }
  }
  const alive = (pid: number) => {
    try {
      process.kill(pid, 0);
      return true;
    } catch {
      return false;
    }
  };

  it('子进程先退出、孙进程占着管道时到达期限：立即关闭管道按超时返回，不等宽限期和管道', { timeout: 15_000 }, async () => {
    await withDir(async (dir) => {
      const start = Date.now();
      const r = await nodeExec(process.execPath, ['-e', orphanScript, path.join(dir, 'grandchild')], { timeoutMs, killGraceMs: 60_000, pipeDrainMs: 60_000 });
      expect(r.code).toBe(124);
      expect(r.stdout).toBe('child done');
      expect(Date.now() - start).toBeLessThan(timeoutMs + 5_000);
    });
  });

  it('没有期限时子进程退出、孙进程占着管道：等管道 pipeDrainMs 后返回子进程的退出码', { timeout: 15_000 }, async () => {
    await withDir(async (dir) => {
      const r = await nodeExec(process.execPath, ['-e', orphanScript, path.join(dir, 'grandchild')], { pipeDrainMs: 300 });
      expect(r).toEqual({ code: 0, stdout: 'child done', stderr: '', end: 'exit' });
    });
  });

  it('超时后子进程收到 SIGTERM 退出、管道关闭：读完输出按超时返回，不等宽限期', { timeout: 15_000 }, async () => {
    const script = `process.on('SIGTERM', () => { process.stdout.write('bye', () => process.exit(0)); }); setInterval(() => {}, 1000);`;
    const start = Date.now();
    const r = await nodeExec(process.execPath, ['-e', script], { timeoutMs, killGraceMs: 60_000, pipeDrainMs: 60_000 });
    expect(r.code).toBe(124);
    expect(r.stdout).toBe('bye');
    expect(Date.now() - start).toBeLessThan(timeoutMs + 5_000);
  });

  it('超时宽限期间 signal 中止：不取消 SIGKILL 兜底，宽限期后进程已被杀死，仍按超时返回', { timeout: 15_000 }, async () => {
    await withDir(async (dir) => {
      const term = path.join(dir, 'term');
      const ctl = new AbortController();
      const graceMs = 1_000;
      let settled = false;
      const p = nodeExec(process.execPath, ['-e', stubbornScript, path.join(dir, 'pid'), term], { timeoutMs, killGraceMs: graceMs, signal: ctl.signal }).finally(() => (settled = true));
      await waitFor(() => existsSync(term), 10_000, '子进程收到 SIGTERM');
      const aborted = Date.now();
      ctl.abort();
      await new Promise((r) => setTimeout(r, graceMs / 4));
      expect(settled).toBe(false);
      const r = await p;
      expect(Date.now() - aborted).toBeGreaterThanOrEqual(graceMs / 2);
      expect(r.code).toBe(124);
      expect(alive(Number(await readFile(path.join(dir, 'pid'), 'utf8')))).toBe(false);
    });
  });

  it('signal 中止：先 SIGTERM，忽略时宽限期后 SIGKILL，进程结束后返回 127', { timeout: 15_000 }, async () => {
    await withDir(async (dir) => {
      const pidFile = path.join(dir, 'pid');
      const ctl = new AbortController();
      const p = nodeExec(process.execPath, ['-e', stubbornScript, pidFile, path.join(dir, 'term')], { killGraceMs: 300, signal: ctl.signal });
      await waitFor(() => existsSync(pidFile), 10_000, '子进程启动');
      ctl.abort();
      const r = await p;
      expect(r.code).toBe(127);
      expect(r.stderr).toContain('AbortError');
      expect(existsSync(path.join(dir, 'term'))).toBe(true);
      expect(alive(Number(await readFile(pidFile, 'utf8')))).toBe(false);
    });
  });

  it('signal 已经中止时不启动命令，返回 127', async () => {
    const r = await nodeExec(process.execPath, ['-e', 'process.exit(0)'], { signal: AbortSignal.abort() });
    expect(r.code).toBe(127);
  });
});

describe('杂项', () => {
  it('substitute 替换全部占位符', () => {
    expect(
      substitute('{{KILN_TOOL}} x {{KILN_TOOL}} {{KILN_TASK_DIR}} {{KILN_SERVICES}} {{KILN_SKILL}}', {
        tool: 't',
        taskDir: 'd',
        services: 's',
        skill: 'k',
      })
    ).toBe('t x t d s k');
  });

  // 值按字面插入：String.replace 的替换字符串中 $&、$1、$$、$`、$' 是模式，路径中可以有这些字符
  it.each(['taskDir', 'tool', 'services', 'skill'] as const)('substitute 按字面插入 %s 的值：替换模式字符不被解释', (key) => {
    const vars = { taskDir: 'd', tool: 't', services: 's', skill: 'k' };
    const placeholder = { taskDir: '{{KILN_TASK_DIR}}', tool: '{{KILN_TOOL}}', services: '{{KILN_SERVICES}}', skill: '{{KILN_SKILL}}' }[key];
    for (const value of ["/x/$&/y", '/x/$1/y', '/x/$$/y', '/x/$`/y', "/x/$'/y", "$& $1 $$ $` $' $<n> $0"]) {
      expect(substitute(`a ${placeholder} b ${placeholder}`, { ...vars, [key]: value })).toBe(`a ${value} b ${value}`);
    }
  });

  it('substitute 一次扫描：值中碰巧出现的占位符文字不再替换', () => {
    expect(substitute('{{KILN_TASK_DIR}} {{KILN_TOOL}}', { taskDir: '/x/{{KILN_TOOL}}', tool: 't', services: 's', skill: 'k' })).toBe(
      '/x/{{KILN_TOOL}} t'
    );
  });

  it('KeyedMutex 同一个 key 串行', async () => {
    const m = new KeyedMutex();
    const order: string[] = [];
    const slow = m.run('a', async () => {
      await new Promise((r) => setTimeout(r, 20));
      order.push('1');
    });
    const fast = m.run('a', () => {
      order.push('2');
      return Promise.resolve();
    });
    await Promise.all([slow, fast]);
    expect(order).toEqual(['1', '2']);
  });

  it('LogShipper 按 committed 续传', async () => {
    const got: Buffer[] = [];
    let committed = 0n;
    let fail = true;
    const s = new LogShipper((offset, data) => {
      if (fail) {
        fail = false;
        return Promise.reject(new Error('net'));
      }
      // 断言失败记为测试失败（LogShipper 会把上传函数的错误当成上传失败）
      return faults().guard(() => {
        expect(offset).toBe(committed);
        got.push(Buffer.from(data));
        committed += BigInt(data.length);
        return committed;
      });
    });
    s.write('a\n');
    await expect(s.flush()).rejects.toThrow();
    s.write('b\n');
    await s.close();
    expect(Buffer.concat(got).toString()).toBe('a\nb\n');
  });
});

describe('Journal', () => {
  function record(attemptId: string, i: number, pad = 0): JournalRecord {
    return { attemptId, epoch: '1', spec: 'x'.repeat(pad), stage: 'running', binding: { ...emptyBinding(), runId: String(i) }, startedAt: i };
  }

  async function withDir(f: (dir: string) => Promise<void>): Promise<void> {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-journal-'));
    try {
      await f(dir);
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  }

  it('同一个 attempt 并发写入：全部成功，最终是最后一次调用的内容，合法 JSON，没有残留的临时文件', async () => {
    await withDir(async (dir) => {
      const j = new Journal(dir);
      const n = 20;
      // 前面的写入更大（写得更慢）：不按调用顺序完成时，较早的快照会最后改名、覆盖较新的
      const saves: Array<Promise<void>> = [];
      for (let i = 0; i < n; i++) {
        saves.push(j.save(record('7', i, (n - i) * 64 * 1024)));
        saves.push(j.save(record('8', i)));
      }
      const results = await Promise.allSettled(saves);
      expect(results.filter((r) => r.status === 'rejected')).toEqual([]);
      for (const id of ['7', '8']) {
        const got = JSON.parse(await readFile(path.join(dir, `${id}.json`), 'utf8')) as JournalRecord;
        expect(got.startedAt).toBe(n - 1);
      }
      expect((await readdir(dir)).sort()).toEqual(['7.json', '8.json']);
      expect((await j.list()).records.map((r) => r.attemptId).sort()).toEqual(['7', '8']);
    });
  });

  it('删除排在之前的写入之后：之前排队的写入不会把文件写回来', async () => {
    await withDir(async (dir) => {
      const j = new Journal(dir);
      const pending = [j.save(record('9', 1, 1 << 20)), j.save(record('9', 2, 1 << 20))];
      await j.remove('9');
      await Promise.all(pending);
      expect(await readdir(dir)).toEqual([]);
    });
  });

  it('写入失败时删除自己的临时文件；sweepTemp 删除崩溃留下的临时文件；probe 不留下文件', async () => {
    await withDir(async (dir) => {
      const j = new Journal(dir);
      // 目标路径是一个非空目录：改名失败
      await writeFile(path.join(dir, 'placeholder'), '');
      await mkdir(path.join(dir, '5.json', 'x'), { recursive: true });
      await expect(j.save(record('5', 1))).rejects.toThrow();
      expect((await readdir(dir)).filter((f) => f.endsWith('.tmp'))).toEqual([]);
      await writeFile(path.join(dir, '6.json.abc.tmp'), '{');
      await j.sweepTemp();
      expect((await readdir(dir)).filter((f) => f.endsWith('.tmp'))).toEqual([]);
      await j.probe();
      expect((await readdir(dir)).sort()).toEqual(['5.json', 'placeholder']);
    });
  });
});

describe('日志续传', () => {
  /** server 按 internal/server/logstore 的 appendAt 规则保存日志（已有的部分忽略，返回当前长度）。 */
  function server(existing: string) {
    let buf = Buffer.from(existing);
    const calls: Array<{ offset: bigint; len: number }> = [];
    const append = (offset: bigint, data: Uint8Array) => {
      calls.push({ offset, len: data.length });
      const size = BigInt(buf.length);
      if (offset > size) return Promise.reject(new Error(`offset ${offset} 超过已有的 ${size}`));
      const end = offset + BigInt(data.length);
      if (end <= size) return Promise.resolve(size);
      buf = Buffer.concat([buf, Buffer.from(data).subarray(Number(size - offset))]);
      return Promise.resolve(end);
    };
    return { append, calls, text: () => buf.toString() };
  }
  const old = 'x'.repeat(2016) + '\n';

  it('恢复的 attempt 先查询 Server 已有的长度，再接着传（新日志比原有的短、长都完整）', async () => {
    for (const add of ['after restart: fatal push failure\n', 'y'.repeat(5000) + '\n']) {
      const srv = server(old);
      const s = new LogShipper(srv.append, { resume: true });
      s.write(add);
      await s.close();
      expect(srv.text()).toBe(old + add);
      expect(srv.calls[0]).toEqual({ offset: 0n, len: 0 });
    }
  });

  it('新的 attempt 不查询；Server 返回的长度超过这一块的末尾时不截掉没发过的内容，改从 Server 的长度重发', async () => {
    const fresh = server('');
    const a = new LogShipper(fresh.append);
    a.write('hello\n');
    await a.close();
    expect(fresh.calls).toEqual([{ offset: 0n, len: 6 }]);
    // 本地的位置落后（不知道 Server 已有日志）：第一块被 Server 当成已有内容，返回的长度超过块的末尾
    const srv = server(old);
    const s = new LogShipper(srv.append);
    s.write('after restart\n');
    await s.close();
    expect(srv.text()).toBe(old + 'after restart\n');
  });
});

  it('flush 把停止信号交给上传：停止后不再上传、缓冲区保留，下一次照常上传（审计）', async () => {
    const calls: Array<{ offset: bigint; len: number; signal?: AbortSignal }> = [];
    const ac = new AbortController();
    const s = new LogShipper((offset, data, signal) => {
      calls.push({ offset, len: data.length, signal });
      if (calls.length === 1) {
        // 第一次上传挂着，等停止信号
        return new Promise((_resolve, reject) => signal?.addEventListener('abort', () => reject(new Error('已中止')), { once: true }));
      }
      return Promise.resolve(offset + BigInt(data.length));
    });
    s.write('hello\n');
    const p = s.flush(ac.signal);
    await waitFor(() => calls.length === 1, 5_000, '开始上传');
    expect(calls[0]?.signal).toBe(ac.signal);
    ac.abort();
    await expect(p).rejects.toThrow('已中止');
    // 缓冲区没有丢：没有信号的下一次上传从原位置发送全部内容
    await s.flush();
    expect(calls[1]).toEqual({ offset: 0n, len: 6, signal: undefined });
  });

  it('已经停止时 flush 不再发起上传', async () => {
    const calls: number[] = [];
    const s = new LogShipper((offset, data) => {
      calls.push(data.length);
      return Promise.resolve(offset + BigInt(data.length));
    });
    s.write('x');
    const ac = new AbortController();
    ac.abort();
    await expect(s.flush(ac.signal)).rejects.toBeDefined();
    expect(calls).toEqual([]);
    await s.flush();
    expect(calls).toEqual([1]);
  });

describe('宿主', () => {
  it('配置校验', () => {
    expect(validateConfig(defaultConfig())).toEqual([]);
    expect(validateConfig({ ...defaultConfig(), max_tasks: 0, agents: ['gemini'] })).toHaveLength(2);
  });

  it('私钥、注册信息、邮箱身份是 0600', async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-home-'));
    try {
      const h = new Home(dir);
      const seed = await h.key(true);
      expect(await h.key(false)).toEqual(Buffer.from(seed));
      expect((await stat(h.path('keys', 'node.ed25519'))).mode & 0o777).toBe(0o600);
      await h.saveState({ server: 's', runnerId: '1', name: 'n', role: 'review' });
      await h.identityStore().save({ handle: 'h', runId: 'r' });
      expect(await h.identityStore().load()).toEqual({ handle: 'h', runId: 'r' });
      expect((await stat(h.path('state.json'))).mode & 0o777).toBe(0o600);
      expect((await stat(h.mailboxFile)).mode & 0o777).toBe(0o600);
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it('state.json 并发写入（自查）：全部成功，没有残留的临时文件', async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-home-'));
    try {
      const h = new Home(dir);
      const writes = Array.from({ length: 20 }, (_, i) => h.saveState({ server: 's', runnerId: String(i), name: 'n', role: 'review' }));
      expect((await Promise.allSettled(writes)).filter((r) => r.status === 'rejected')).toEqual([]);
      expect(await readdir(dir)).toEqual(['state.json']);
      expect((await h.loadState()).name).toBe('n');
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it('本机工具经 unix socket 往返', async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'ko-'));
    const sock = path.join(dir, 'run', 's.sock');
    const srv = await serveTools(sock, (r) => Promise.resolve({ code: r.cmd === 'ci' ? 0 : 1, output: `${r.attempt}:${r.cmd}:${r.args.sha ?? ''}` }));
    try {
      expect(await callTool(sock, { attempt: '7', cmd: 'ci', args: { sha: 'abc' } })).toEqual({ code: 0, output: '7:ci:abc' });
      expect((await stat(path.dirname(sock))).mode & 0o777).toBe(0o700);
    } finally {
      srv.close();
      await rm(dir, { recursive: true, force: true });
    }
  });

  it('本机工具的请求按 unknown 解析：字段类型不对时拒绝，不把数字、null 当成字符串（审计）', () => {
    expect(parseToolRequest({ attempt: '7', cmd: 'info' })).toEqual({ attempt: '7', cmd: 'info', args: {} });
    expect(parseToolRequest({ attempt: '7', cmd: 'ci', args: { sha: 'abc' } })).toEqual({ attempt: '7', cmd: 'ci', args: { sha: 'abc' } });
    for (const bad of [null, [], 1, 'x']) expect(() => parseToolRequest(bad)).toThrow('请求应为 JSON 对象');
    expect(() => parseToolRequest({ attempt: 7, cmd: 'info' })).toThrow('attempt 应为字符串');
    expect(() => parseToolRequest({ attempt: '7' })).toThrow('cmd 应为字符串');
    expect(() => parseToolRequest({ attempt: '7', cmd: 'info', args: [] })).toThrow('args 应为对象');
    expect(() => parseToolRequest({ attempt: '7', cmd: 'ci', args: { sha: 1 } })).toThrow('args.sha 应为字符串');
    expect(parseOpRequest({ attempt: '7' })).toEqual({ attempt: '7', url: '', pid: '' });
    expect(parseOpRequest({ attempt: '7', url: 'https://x/y', pid: '1' })).toEqual({ attempt: '7', url: 'https://x/y', pid: '1' });
    for (const bad of [null, [], 'x']) expect(() => parseOpRequest(bad)).toThrow('请求应为 JSON 对象');
    expect(() => parseOpRequest({ attempt: 7 })).toThrow('attempt 应为字符串');
    expect(() => parseOpRequest({ attempt: '7', url: 1 })).toThrow('url 应为字符串');
    expect(() => parseOpRequest({ attempt: '7', pid: null })).toThrow('pid 应为字符串');
  });

  it('回复超过上限的错误写明这一次实际用的上限（审计）', () => {
    expect(new ToolReplyTooLargeError().message).toContain(`${maxToolReplyBytes} 字节`);
    expect(new ToolReplyTooLargeError(64).limit).toBe(64);
    expect(new ToolReplyTooLargeError(64).message).toContain('64 字节');
  });

  it('config.json 中字段类型不对时 loadConfig 报错，不把数字、null 当成字符串（审计）', async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-home-'));
    try {
      const h = new Home(dir);
      const write = (v: unknown) => writeFile(h.path('config.json'), JSON.stringify(v));
      await write({ max_tasks: '8' });
      await expect(h.loadConfig()).rejects.toThrow('max_tasks 应为整数');
      await write({ agents: ['codex', 1] });
      await expect(h.loadConfig()).rejects.toThrow('agents 应为字符串数组');
      await write({ orca: 1 });
      await expect(h.loadConfig()).rejects.toThrow('orca 应为字符串');
      await write([1]);
      await expect(h.loadConfig()).rejects.toThrow('不是配置对象');
      // 类型正确时照常读入，不认识的字段忽略
      await write({ max_tasks: 8, unknown: 'x' });
      expect((await h.loadConfig()).max_tasks).toBe(8);
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });

  it('服务定义：用户级、记录 PATH、常驻（以 0 退出时不重启）', () => {
    const spec = { name: 'Mini AI.01', node: '/n/node', cli: '/a b/cli.js', home: '/h', path: '/usr/bin:/opt/x', logDir: '/h/logs' };
    const plist = launchdPlist(spec);
    expect(plist).toContain('<string>/a b/cli.js</string>');
    expect(plist).toContain('<string>/usr/bin:/opt/x</string>');
    // 只在非 0 退出、崩溃时重启：被吊销（以 0 退出）后不被反复拉起
    expect(plist).toContain('<key>KeepAlive</key>\n  <dict>\n    <key>SuccessfulExit</key>\n    <false/>\n  </dict>');
    // 服务标识只由数据目录决定（名称只在说明中）；服务定义中记下数据目录
    expect(plist).toContain(`<string>dev.kiln.orca.${homeOwner('/h')}</string>`);
    expect(plist).toContain('<!-- Kiln AI 执行节点 Mini AI.01（kiln-orca） -->');
    expect(definedHome(plist)).toBe('/h');
    const unit = systemdUnit(spec);
    expect(definedHome(unit)).toBe('/h');
    expect(unit).toContain('\nDescription=Kiln AI 执行节点 Mini AI.01（kiln-orca）\n');
    // 服务运行带 --service：正常日志只写按日轮转的文件，不重复写进服务管理器的 stderr
    expect(unit).toContain('ExecStart="/n/node" "/a b/cli.js" "run" "--service" "--home" "/h"');
    expect(plist).toContain('<string>run</string>\n    <string>--service</string>\n    <string>--home</string>');
    expect(unit).toContain('Environment="PATH=/usr/bin:/opt/x"');
    expect(unit).toContain('\nRestart=on-failure\n');
    expect(unit).not.toContain('Restart=always');
  });
});

describe('连接器日志', () => {
  const day = 24 * 60 * 60_000;

  async function withDir(f: (dir: string) => Promise<void>): Promise<void> {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-logs-'));
    try {
      await f(dir);
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  }

  it('服务模式只写按日文件、不写 stderr；前台运行同时写 stderr', async () => {
    await withDir(async (dir) => {
      const svc: string[] = [];
      fileLogger(dir, { console: false, stderr: (l) => svc.push(l) }).info('服务模式', { a: 1 });
      const fg: string[] = [];
      fileLogger(dir, { console: true, stderr: (l) => fg.push(l) }).warn('前台');
      expect(svc).toEqual([]);
      expect(fg).toHaveLength(1);
      expect(fg[0]).toMatch(/ WARN 前台\n$/);
      const file = await readFile(path.join(dir, dailyLog(new Date())), 'utf8');
      expect(file).toMatch(/ INFO 服务模式 a=1\n.* WARN 前台\n$/);
    });
  });

  it('服务模式下写日志文件失败时退回 stderr', async () => {
    await withDir(async (dir) => {
      // 今天的日志文件是一个目录：写不进去
      await mkdir(path.join(dir, dailyLog(new Date())));
      const got: string[] = [];
      fileLogger(dir, { console: false, stderr: (l) => got.push(l) }).error('磁盘坏了');
      expect(got).toHaveLength(1);
      expect(got[0]).toMatch(/ ERROR 磁盘坏了\n$/);
    });
  });

  /**
   * fileTimeToleranceMs：比较文件修改时间的容差。utimes 以浮点秒写入、stat 读回的往返有约 1 ms 的误差，有的文件系统
   * 只精确到秒（FAT 为 2 秒）；被测的行为（追加没有把修改时间改成现在）相差的是天，不受这个容差影响。
   */
  const fileTimeToleranceMs = 2_000;

  it('pruneLogs 之后超过保留期的内容不在任何日志文件里：service.*.log 原地截断，最近的内容移进它那天的按日文件', async () => {
    await withDir(async (dir) => {
      const now = Date.now();
      const at = async (name: string, content: string, t: number) => {
        await writeFile(path.join(dir, name), content);
        await utimes(path.join(dir, name), new Date(t), new Date(t));
      };
      const old = now - 8 * day;
      const recent = now - 2 * day;
      await at(dailyLog(new Date(old)), 'OLD-DAILY\n', old);
      await at(serviceLogs.err, 'OLD-CRASH\n', old);
      await at(serviceLogs.out, 'RECENT-CRASH', recent);
      const errIno = (await stat(path.join(dir, serviceLogs.err))).ino;

      await pruneLogs(dir, now);
      const all = async () => {
        const out: string[] = [];
        for (const f of await readdir(dir)) out.push(await readFile(path.join(dir, f), 'utf8'));
        return out.join('');
      };
      let text = await all();
      expect(text).not.toContain('OLD-DAILY');
      expect(text).not.toContain('OLD-CRASH');
      // 服务管理器的文件保留（同一个文件，没有删除或改名），只是清空
      expect((await stat(path.join(dir, serviceLogs.err))).ino).toBe(errIno);
      expect((await stat(path.join(dir, serviceLogs.err))).size).toBe(0);
      expect((await stat(path.join(dir, serviceLogs.out))).size).toBe(0);
      const moved = path.join(dir, dailyLog(new Date(recent)));
      expect(await readFile(moved, 'utf8')).toMatch(/service\.out\.log.*\nRECENT-CRASH\n$/);
      // 移进去的内容随那一天的文件一起过期（追加没有延长它的保留期）：修改时间仍是那一天最后一条内容的时间。
      // 文件时间经 stat → Date → utimes 往返有毫秒级误差（浮点秒），按文件时间精度比较
      expect(Math.abs((await stat(moved)).mtimeMs - recent)).toBeLessThanOrEqual(fileTimeToleranceMs);
      await pruneLogs(dir, recent + logRetentionMs + day);
      text = await all();
      expect(text).not.toContain('RECENT-CRASH');
      expect((await readdir(dir)).sort()).toEqual([serviceLogs.err, serviceLogs.out].sort());
    });
  });

  it('service.*.log 超过上限时只把末尾移进按日文件', async () => {
    await withDir(async (dir) => {
      const big = 'x'.repeat(serviceLogMaxBytes) + 'TAIL\n';
      await writeFile(path.join(dir, serviceLogs.err), 'HEAD' + big);
      await pruneLogs(dir);
      const daily = await readFile(path.join(dir, dailyLog(new Date())), 'utf8');
      expect(daily).not.toContain('HEAD');
      expect(daily).toContain('dropped_bytes=9');
      expect(daily.endsWith('TAIL\n')).toBe(true);
    });
  });
});

describe('Journal.list', () => {
  it('目录不存在时为空；其它读取错误抛出', async () => {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-journal-'));
    try {
      expect(await new Journal(path.join(dir, 'missing')).list()).toEqual({ records: [], unreadable: [] });
      // 不是目录（ENOTDIR）
      await writeFile(path.join(dir, 'file'), '');
      await expect(new Journal(path.join(dir, 'file')).list()).rejects.toMatchObject({ code: 'ENOTDIR' });
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });
});

describe('task ci 的输出', () => {
  const job = (i: number, log: string, extra: Partial<{ logExpired: boolean; url: string; errorClass: string }> = {}) =>
    create(FailedJobSchema, { name: `job-${i}`, reason: 'exit 1', errorClass: 'user', logTail: Buffer.from(log), ...extra });

  it('没有超过上限：全部列出，没有省略说明；日志达到请求的字节数时说明更早的部分没有列出', () => {
    expect(formatVerification({ sha: 's', state: 'succeeded', failedJobs: [] })).toBe('commit s：succeeded');
    const out = formatVerification({
      sha: 's',
      state: 'failed',
      failedJobs: [job(0, 'short'), job(1, 'y'.repeat(ciLogTailBytes), { url: 'https://kiln/1' }), job(2, '', { logExpired: true, errorClass: 'infra.disk' })],
    });
    expect(out.split('\n')[0]).toBe('commit s：failed（3 个失败的 job）');
    expect(out).not.toContain('注意');
    expect(out).toContain('short');
    // 只有达到请求字节数的日志带"更早的部分没有列出"
    expect(out.match(/更早的部分没有列出/g)).toHaveLength(1);
    expect(out.indexOf('更早的部分没有列出')).toBeGreaterThan(out.indexOf('job-1'));
    expect(out).toContain('（日志已过保留期）');
    expect(out).toContain('（基础设施的问题，不需要改代码）');
  });

  it('超过上限：按顺序先带日志，之后只列标题，再之后只给出数量；开头和末尾写明省略了什么，正文不超过上限', () => {
    const limit = 4096;
    const jobs = Array.from({ length: 200 }, (_, i) => job(i, `日志-${i}\n`.padEnd(1000, 'x')));
    const out = formatVerification({ sha: 's', state: 'failed', failedJobs: jobs }, limit);
    const lines = out.split('\n');
    const note = lines[1] ?? '';
    expect(note).toMatch(/^注意：输出超过 4096 字节的上限，\d+ 个 job 的日志没有列出，还有 \d+ 个失败的 job 没有列出；/);
    expect(lines.at(-1)).toBe(note);
    const [, withoutLog, notListed] = /(\d+) 个 job 的日志没有列出，还有 (\d+) 个/.exec(note)?.map(Number) ?? [];
    const withLog = jobs.filter((_, i) => out.includes(`日志-${i}\n`)).length;
    const headers = jobs.filter((_, i) => out.includes(`## 失败：job-${i}（`)).length;
    expect(withLog).toBeGreaterThan(0);
    expect(headers).toBe(withLog + (withoutLog ?? 0));
    expect(headers + (notListed ?? 0)).toBe(jobs.length);
    // 列出的是开头连续的 job：带日志的在前
    expect(out.includes(`## 失败：job-${headers - 1}（`) && !out.includes(`## 失败：job-${headers}（`)).toBe(true);
    expect(out.includes(`日志-${withLog - 1}\n`) && !out.includes(`日志-${withLog}\n`)).toBe(true);
    // 去掉两处省略说明后不超过上限
    const body = [lines[0], ...lines.slice(2, -2)].join('\n');
    expect(Buffer.byteLength(body)).toBeLessThanOrEqual(limit);
    expect(maxCIOutputBytes).toBeGreaterThanOrEqual(ciLogTailBytes);
  });

  it('一个 job 的日志放不下之后，后面放得下的短日志也不列出：省略的总是末尾的部分', () => {
    const out = formatVerification({ sha: 's', state: 'failed', failedJobs: [job(0, 'a'.repeat(3000)), job(1, 'b'.repeat(3000)), job(2, 'short-2')] }, 4096);
    expect(out).toContain('a'.repeat(3000));
    expect(out).not.toContain('bbb');
    expect(out).not.toContain('short-2');
    expect(out).toContain('## 失败：job-2（');
    expect(out.split('\n')[1]).toMatch(/^注意：输出超过 4096 字节的上限，2 个 job 的日志没有列出；/);
  });
});

describe('邮箱消息的分类与中止说明', () => {
  const msg = (o: Partial<OrcaMessage> = {}): OrcaMessage => ({
    id: 'msg_1',
    type: 'escalation',
    subject: '',
    body: '',
    dispatchId: 'ctx_1',
    outcome: '',
    reportPath: '',
    rejected: '',
    turnOutcome: '',
    stopReason: '',
    failureKind: '',
    error: '',
    detail: '',
    ...o,
  });

  it('分类：turnOutcome 为 failure 或者 stopReason 致命时是 fatal，其余带 stopReason 的是系统通知，没有的是普通求助', () => {
    expect([...fatalStopReasons]).toEqual(['needs-input', 'exited', 'lost']);
    expect(classifyEscalation(msg({ turnOutcome: 'failure' }))).toBe('fatal');
    for (const stopReason of fatalStopReasons) expect(classifyEscalation(msg({ stopReason }))).toBe('fatal');
    // 同时带着 finished 也按 failure：Agent 明确报错，比系统停止原因可信
    expect(classifyEscalation(msg({ turnOutcome: 'failure', stopReason: 'finished' }))).toBe('fatal');
    // finished 与连接器不认识的 stopReason 只是这一轮结束，不中止任务
    expect(classifyEscalation(msg({ stopReason: 'finished' }))).toBe('system_stop');
    expect(classifyEscalation(msg({ stopReason: 'someFutureStop' }))).toBe('system_stop');
    // 没有这些字段：普通求助；Agent 私有的 failureKind 不参与判断
    expect(classifyEscalation(msg())).toBe('ordinary');
    expect(classifyEscalation(msg({ failureKind: 'notSignedIn' }))).toBe('ordinary');
    // 不是这一轮的失败（succeeded、空串）同样不中止
    expect(classifyEscalation(msg({ turnOutcome: 'succeeded' }))).toBe('ordinary');
  });

  it('dispatch id 只有在能直接用作文件名时才安全', () => {
    for (const id of ['ctx_1', 'ctx-000000000001', 'a.b']) expect(safeDispatchId(id)).toBe(true);
    for (const id of ['', '.', '..', '../x', 'a/b', 'a\\b', 'a\0b']) expect(safeDispatchId(id)).toBe(false);
  });

  it('truncateUtf8：按字节截断但不截断多字节字符和代理对，附加说明后不超限', () => {
    expect(truncateUtf8('short', 10)).toBe('short');
    expect(truncateUtf8('中文中文', 100)).toBe('中文中文');
    // 11 个汉字是 33 字节：说明占 18 字节，剩下的 12 字节正好是四个汉字（第 13 字节落在下一个汉字中间）
    const out = truncateUtf8('中'.repeat(11), 30);
    expect(out).toBe('中中中中…（已截断）');
    expect(Buffer.byteLength(out)).toBeLessThanOrEqual(30);
    expect(out).not.toContain('�');
    // 代理对（emoji）不被截成半个
    const emoji = truncateUtf8('😀😀😀', 10);
    expect(Buffer.byteLength(emoji)).toBeLessThanOrEqual(10);
    expect(emoji).not.toContain('�');
    // 上限小于说明本身的字节数时，说明也按字符边界截断，总长度仍然不超限
    const tiny = truncateUtf8('abcdef', 5);
    expect(Buffer.byteLength(tiny)).toBeLessThanOrEqual(5);
    expect(tiny).not.toContain('�');
  });

  it('中止说明：元信息、主因优先级、error 之外的详情补充，先脱敏再截断', () => {
    // 元信息去重、用顿号连接；error 是主因
    const t = formatAbortReason(msg({ failureKind: 'notSignedIn', stopReason: 'exited', error: '登录已失效', detail: '认证返回 401 状态码' }), new Redactor());
    expect(t.reason).toBe('Agent 运行中断（notSignedIn、exited）：登录已失效；详情：认证返回 401 状态码');
    // detail 已经在 error 中时不再重复
    expect(formatAbortReason(msg({ error: '认证失败：401', detail: '401' }), new Redactor()).reason).toBe('Agent 运行中断：认证失败：401');
    // 没有 error 时按 detail、body、subject 取主因；body 作为日志中的补充输出
    expect(formatAbortReason(msg({ detail: '细节', body: '原始输出', subject: '标题' }), new Redactor()).reason).toBe('Agent 运行中断：细节');
    const withBody = formatAbortReason(msg({ error: '中断', body: '原始输出' }), new Redactor());
    expect(withBody.reason).toBe('Agent 运行中断：中断');
    expect(withBody.logDetail).toBe('Agent 运行中断：中断；输出：原始输出');
    // 什么都没有时兜底
    expect(formatAbortReason(msg(), new Redactor()).reason).toBe('Agent 运行中断：Orca 报告 Agent 运行中断');
    // 先脱敏再截断：两个登记的凭据分别横跨 reason（4 KiB）与 logDetail（16 KiB）的**真实**截断点。截断说明占 18 字节，
    // 保留前 max - 18 字节；凭据从截断点前 20 字节开始，一半落在保留的文本里、一半被截掉——先截断后脱敏时留下的那半段
    // 替换不掉，下面的断言就会失败（把凭据整个放在截断点之后时，变异存活）。
    const mark = '…（已截断）';
    const markBytes = Buffer.byteLength(mark, 'utf8');
    const keptBytes = (max: number): number => max - markBytes;
    const redactor = new Redactor();
    const reasonSecret = 'ghs_TESTREASON0123456789abcdef';
    const logSecret = 'ghs_TESTLOGDETAIL0123456789abcdef';
    for (const s of [reasonSecret, logSecret]) redactor.add(s, Date.now() + 3_600_000);
    const prefix = 'Agent 运行中断（safetyCheck）：';
    const straddle = 20;
    // reason：主因就是 error，截断点在 prefix 之后保留 keptBytes(maxAbortMessageBytes) 字节处
    const error = 'A'.repeat(keptBytes(maxAbortMessageBytes) - Buffer.byteLength(prefix, 'utf8') - straddle) + reasonSecret + 'B'.repeat(1000);
    // logDetail：主因、详情之后追加输出，截断点落在 body 内（按原始文本计算——先截断的变异就是在这个长度上截断）
    const detail = '细节';
    const beforeBody = `${prefix}${error}；详情：${detail}；输出：`;
    const room = maxAbortLogBytes - Buffer.byteLength(beforeBody, 'utf8');
    const body = 'C'.repeat(room - markBytes - straddle) + logSecret + 'D'.repeat(1000);
    const big = formatAbortReason(msg({ failureKind: 'safetyCheck', detail, error, body }), redactor);
    for (const [text, max] of [[big.reason, maxAbortMessageBytes], [big.logDetail, maxAbortLogBytes]] as const) {
      expect(Buffer.byteLength(text, 'utf8')).toBeLessThanOrEqual(max);
      expect(text.endsWith(mark)).toBe(true);
      // 明文前缀与两段凭据（含被截断的半段）都不能出现
      expect(text).not.toContain('ghs_TEST');
      expect(text).not.toContain(reasonSecret.slice(0, straddle));
      expect(text).not.toContain(logSecret.slice(0, straddle));
    }
    expect(big.reason.startsWith(`${prefix}${'A'.repeat(100)}`)).toBe(true);
    expect(big.logDetail).toContain('；输出：');
  });

  it('元信息限长：超长的 failureKind / stopReason / id 不挤掉主因，落盘记录大小有界', () => {
    // 300 KB 的 UTF-8 元信息：不限制时 prefix 会占满（甚至超过）4 KiB，主因被挤掉
    const huge = '失'.repeat(100_000);
    const error = `主因：${'E'.repeat(200)}`;
    const rec = abortRecord(msg({ id: huge, turnOutcome: huge, stopReason: huge, failureKind: huge, error }), new Redactor());
    for (const v of [rec.id, rec.turnOutcome, rec.stopReason, rec.failureKind]) {
      expect(Buffer.byteLength(v, 'utf8')).toBeLessThanOrEqual(maxAbortMetaBytes);
      expect(v).not.toContain('�');
      expect(v.endsWith('…（已截断）')).toBe(true);
    }
    // 主因完整保留：元信息只用掉有界的篇幅
    expect(rec.reason).toContain(error);
    expect(rec.reason.startsWith('Agent 运行中断（')).toBe(true);
    // 序列化后的记录有界：两份说明 + 四个元信息字段（另留键名与 JSON 转义的余量）
    expect(Buffer.byteLength(JSON.stringify(rec), 'utf8')).toBeLessThanOrEqual(maxAbortMessageBytes + maxAbortLogBytes + 4 * maxAbortMetaBytes + 1024);
  });
});
