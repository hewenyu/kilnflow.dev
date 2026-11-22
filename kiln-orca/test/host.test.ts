import { existsSync } from 'node:fs';
import { mkdir, readdir, readFile, rm, stat, symlink, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { Code, ConnectError, createRouterTransport, type Interceptor } from '@connectrpc/connect';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { type AcquireRequest, AIRunnerService } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { RunnerService } from '../src/gen/kiln/runner/v1/runner_pb.js';
import type { Exec } from '../src/core/exec.js';
import { RevokedError } from '../src/core/errors.js';
import { emptyBinding, Journal } from '../src/core/journal.js';
import { nopLogger } from '../src/core/log.js';
import type { SkillProvisioner } from '../src/core/skill.js';
import { enroll } from '../src/host/enroll.js';
import { defaultConfig, Home, validateConfig } from '../src/host/home.js';
import { dailyLog } from '../src/host/logger.js';
import { runConnector } from '../src/host/run.js';
import { shortTmp, waitFor } from './harness.js';

// 宿主的启动路径：从全新的数据目录走真实的 enroll，再走 kiln-orca run 的主体。Server 是进程内的 Connect 路由，
// 外部命令（orca、docker、Agent）一律失败：Orca 没有就绪，节点只上报状态（standby 领取）。

let root = '';
afterEach(async () => {
  if (root) await rm(root, { recursive: true, force: true });
  root = '';
});

/** server 返回进程内的 Server：注册为 AI 审计节点，认证总是成功，记录领取请求；revoked 为 true 时领取返回"已吊销"。 */
function server() {
  const acquires: AcquireRequest[] = [];
  const s = {
    acquires,
    revoked: false,
    authentications: 0,
    /** 注册返回的 runner id（每次注册在 Kiln 中是一个新节点；测试按需改）。 */
    runnerId: 7n,
    /** enrollGate 设置时注册的响应等它（固定注册与其他写入的交错）。 */
    enrollGate: undefined as Promise<void> | undefined,
    transport: (interceptors: Interceptor[] = []) => createRouterTransport(routes, { transport: { interceptors } }),
  };
  function routes(r: Parameters<Parameters<typeof createRouterTransport>[0]>[0]) {
    r.service(RunnerService, {
      enroll: async () => {
        await s.enrollGate;
        return { runnerId: s.runnerId, kind: 'ai', role: 'review' };
      },
      authenticate: () => {
        s.authentications++;
        return { accessToken: 'token', expiresAt: BigInt(Math.floor(Date.now() / 1000) + 3600) };
      },
    });
    r.service(AIRunnerService, {
      acquire: (req) => {
        acquires.push(req);
        if (s.revoked) throw new ConnectError('RUNNER_REVOKED', Code.PermissionDenied);
        return {};
      },
    });
  }
  return s;
}

const noCommands: Exec = (cmd) => Promise.resolve({ code: 127, stdout: '', stderr: `${cmd}: command not found` });

/** testSkill 是 skill 投放的替身：这些测试不读写宿主机上真实的 ~/.agents / ~/.claude。 */
const testSkill: SkillProvisioner = {
  ensureForAgent: () => Promise.resolve('/fake/skills/kiln-task/SKILL.md'),
  ensureAll: () => Promise.resolve(''),
};

/** isolatedAuth 把个人认证配置检查隔离到临时 HOME：不读运行测试的机器上真实的 netrc（账户主目录同样指向那里）。 */
function isolatedAuth(): { env: Record<string, string | undefined>; accountHome: string } {
  const home = path.join(root, 'account-home');
  return { env: { ...process.env, HOME: home }, accountHome: home };
}

describe('宿主', () => {
  it('新注册的数据目录：run 取锁之前建好运行目录（0700），能取到锁并进入主循环；退出时释放锁、删除 socket', async () => {
    root = await shortTmp('ko-host-');
    const home = new Home(path.join(root, 'fresh'));
    const srv = server();
    const st = await enroll(home, srv.transport(), 'http://kiln.test', 'tok', 'node-1', '0.1.0');
    expect(st).toMatchObject({ runnerId: '7', role: 'review', name: 'node-1' });
    // enroll 只写注册信息
    expect((await readdir(home.dir)).sort()).toEqual(['config.json', 'keys', 'state.json']);
    expect((await home.loadConfig()).server).toBe('http://kiln.test');

    const ac = new AbortController();
    const running = runConnector({
      home,
      transport: (i) => srv.transport(i),
      exec: noCommands,
      version: '0.1.0',
      toolCommand: (h, id) => `kiln-orca task --home ${h.dir} --attempt ${id}`,
      skill: testSkill,
      machineAuth: isolatedAuth(),
      signal: ac.signal,
      log: nopLogger,
    });
    try {
      await waitFor(() => srv.acquires.length > 0, 10_000, '领取');
      expect(srv.acquires[0]?.standby).toBe(true);
      expect(srv.acquires[0]?.state?.orca?.ready).toBe(false);
      expect(JSON.parse(await readFile(home.lockPath, 'utf8'))).toMatchObject({ pid: process.pid });
      expect(existsSync(home.socketPath)).toBe(true);
      for (const d of ['run', 'logs', 'journal', 'tasks', 'repos']) expect((await stat(home.path(d))).mode & 0o777).toBe(0o700);
    } finally {
      ac.abort();
      await running;
    }
    expect(existsSync(home.lockPath)).toBe(false);
    expect(existsSync(home.socketPath)).toBe(false);
  });

  it('启动时检查执行机器上对 GitHub 生效的个人认证配置：有则记 warn（键和来源，不含值），照常领取', async () => {
    root = await shortTmp('ko-host-');
    const home = new Home(path.join(root, 'h'));
    const srv = server();
    await enroll(home, srv.transport(), 'http://kiln.test', 'tok', 'node-1', '0.1.0');
    // git config --get-urlmatch 报告一个请求头（值不应出现在日志中），git config get 给出来源
    const exec: Exec = (cmd, args, opts) => {
      if (cmd === 'git' && args[0] === 'config' && args[1] === '-z' && args[2] === '--get-urlmatch') return Promise.resolve({ code: 0, stdout: 'Authorization: Basic Sekr3tHdr\n', stderr: '' });
      if (cmd === 'git' && args[0] === 'config' && args[1] === 'get') return Promise.resolve({ code: 0, stdout: 'file:/u/.gitconfig\tAuthorization: Basic Sekr3tHdr\n', stderr: '' });
      // 全局、系统配置中没有只对某个路径声明的请求头
      if (cmd === 'git' && args[0] === 'config' && args.includes('--get-regexp')) return Promise.resolve({ code: 1, stdout: '', stderr: '' });
      return noCommands(cmd, args, opts);
    };
    const warns: Array<{ msg: string; fields?: Record<string, unknown> }> = [];
    const ac = new AbortController();
    const running = runConnector({
      home,
      transport: (i) => srv.transport(i),
      exec,
      version: '0.1.0',
      toolCommand: (h, id) => `kiln-orca task --home ${h.dir} --attempt ${id}`,
      skill: testSkill,
      machineAuth: isolatedAuth(),
      signal: ac.signal,
      log: { ...nopLogger, warn: (msg, fields) => warns.push({ msg, fields }) },
    });
    try {
      await waitFor(() => srv.acquires.length > 0, 10_000, '领取');
      // 检查在后台进行：等它的告警
      await waitFor(() => warns.some((x) => x.msg.includes('个人认证配置')), 10_000, '告警');
    } finally {
      ac.abort();
      await running;
    }
    const w = warns.filter((x) => x.msg.includes('个人认证配置'));
    expect(w).toHaveLength(1);
    expect(w[0]?.fields).toMatchObject({ key: 'http.extraheader' });
    expect(JSON.stringify(warns)).not.toContain('Sekr3t');
  });

  it('启动时的认证配置检查卡住（例如主目录在无响应的网络文件系统上）：不拖住启动，照常进入领取循环', async () => {
    root = await shortTmp('ko-host-');
    const home = new Home(path.join(root, 'h'));
    const srv = server();
    await enroll(home, srv.transport(), 'http://kiln.test', 'tok', 'node-1', '0.1.0');
    let probed = false;
    // git config 的查询一直不返回
    const exec: Exec = (cmd, args, opts) => {
      if (cmd === 'git' && args[0] === 'config') {
        probed = true;
        return new Promise(() => undefined);
      }
      return noCommands(cmd, args, opts);
    };
    const ac = new AbortController();
    const running = runConnector({
      home,
      transport: (i) => srv.transport(i),
      exec,
      version: '0.1.0',
      toolCommand: (h, id) => `kiln-orca task --home ${h.dir} --attempt ${id}`,
      skill: testSkill,
      machineAuth: isolatedAuth(),
      signal: ac.signal,
      log: nopLogger,
    });
    try {
      await waitFor(() => probed && srv.acquires.length > 0, 10_000, '检查卡住时照常领取');
    } finally {
      ac.abort();
      await running;
    }
  });

  it('启动时的认证配置检查在连接器退出之后才返回：不再写日志（日志可能已经关闭），也不产生未处理的拒绝', async () => {
    root = await shortTmp('ko-host-');
    const home = new Home(path.join(root, 'h'));
    const srv = server();
    await enroll(home, srv.transport(), 'http://kiln.test', 'tok', 'node-1', '0.1.0');
    let release!: () => void;
    const held = new Promise<void>((r) => (release = r));
    let probed = false;
    // git config 的查询等到连接器退出之后才返回一个冲突
    const exec: Exec = async (cmd, args, opts) => {
      if (cmd === 'git' && args[0] === 'config') {
        probed = true;
        await held;
        return { code: 0, stdout: 'X: v\0', stderr: '' };
      }
      return noCommands(cmd, args, opts);
    };
    const warns: string[] = [];
    const ac = new AbortController();
    const running = runConnector({
      home,
      transport: (i) => srv.transport(i),
      exec,
      version: '0.1.0',
      toolCommand: (h, id) => `kiln-orca task --home ${h.dir} --attempt ${id}`,
      skill: testSkill,
      machineAuth: isolatedAuth(),
      signal: ac.signal,
      log: { ...nopLogger, warn: (msg) => warns.push(msg) },
    });
    try {
      await waitFor(() => probed && srv.acquires.length > 0, 10_000, '检查开始、照常领取');
    } finally {
      ac.abort();
      await running;
    }
    release();
    // 让检查的回调跑完
    await new Promise((r) => setTimeout(r, 50));
    expect(warns.filter((m) => m.includes('个人认证配置'))).toEqual([]);
  });

  it('用户的 Docker 配置损坏：节点照常启动、恢复 journal、上报 Docker 不可用及原因，不执行 docker；修好后不用重启即恢复', async () => {
    root = await shortTmp('ko-host-');
    const home = new Home(path.join(root, 'h'));
    const srv = server();
    await enroll(home, srv.transport(), 'http://kiln.test', 'tok', 'node-1', '0.1.0');
    const userDocker = path.join(root, 'user-docker');
    await mkdir(userDocker);
    const userFile = path.join(userDocker, 'config.json');
    await writeFile(userFile, '{');
    // 上次运行留下的任务：结束了但没能确认 Agent 停止（恢复后继续占用容量、继续停）
    await new Journal(home.path('journal')).save({ attemptId: '41', epoch: '1', spec: '', stage: 'stopping', binding: { ...emptyBinding(), dispatchId: 'd-41' }, startedAt: Date.now(), stopSince: Date.now() });
    // docker 命令都成功（修好配置后可用），记录每次调用；其他命令（orca、Agent）都失败
    const dockerCalls: (string | undefined)[] = [];
    const exec: Exec = (cmd, args, opts) => {
      if (cmd !== 'docker') return noCommands(cmd, args, opts);
      dockerCalls.push(opts?.env?.DOCKER_CONFIG);
      return Promise.resolve({ code: 0, stdout: '', stderr: '' });
    };
    vi.stubEnv('DOCKER_CONFIG', userDocker);
    const ac = new AbortController();
    try {
      const running = runConnector({
        home,
        transport: (i) => srv.transport(i),
        exec,
        version: '0.1.0',
        toolCommand: (h, id) => `kiln-orca task --home ${h.dir} --attempt ${id}`,
      skill: testSkill,
        machineAuth: isolatedAuth(),
        signal: ac.signal,
        log: nopLogger,
        intervals: { idleMs: 10, statusIntervalMs: 50 },
      });
      try {
        await waitFor(() => srv.acquires.length > 0, 10_000, '领取');
        const first = srv.acquires[0]?.state;
        expect(first?.docker?.ready).toBe(false);
        expect(first?.docker?.reason).toContain(`${userFile} 不是有效的 JSON`);
        // journal 已经恢复：没能确认停止的 Agent 占用容量
        expect(first?.running).toBe(1);
        // 配置一直坏着：之后的每次检测都再试、都失败，始终没有执行 docker
        await waitFor(() => srv.acquires.length >= 5, 10_000, '多次检测');
        expect(srv.acquires.every((a) => a.state?.docker?.ready === false)).toBe(true);
        expect(dockerCalls).toEqual([]);

        await writeFile(userFile, '{}');
        await waitFor(() => srv.acquires.at(-1)?.state?.docker?.ready === true, 10_000, 'Docker 恢复可用');
      } finally {
        ac.abort();
        await running;
      }
    } finally {
      vi.unstubAllEnvs();
    }
    // 修好之前没有执行过 docker；之后每次都带专用配置
    expect(dockerCalls.length).toBeGreaterThan(0);
    expect(dockerCalls.every((c) => c === home.path('docker'))).toBe(true);
  });

  it('clone_root：必须是绝对路径；为空时是数据目录中的 repos/，设置时按规范路径使用，run 在取锁之前建好（0700）', async () => {
    root = await shortTmp('ko-host-');
    const home = new Home(path.join(root, 'h'));
    expect(validateConfig({ ...defaultConfig(), clone_root: 'repos' })).toEqual(['clone_root 必须是绝对路径（为空时用数据目录中的 repos/）']);
    expect(validateConfig({ ...defaultConfig(), clone_root: '/srv/clones' })).toEqual([]);
    expect(home.reposDir({ clone_root: '' })).toBe(home.path('repos'));
    // 经符号链接指定的与真实路径相同（同一个 clone 不会被当成两个）
    await mkdir(path.join(root, 'real'));
    await symlink(path.join(root, 'real'), path.join(root, 'link'));
    const c = { clone_root: path.join(root, 'link', 'clones') };
    expect(home.reposDir(c)).toBe(path.join(root, 'real', 'clones'));
    await home.prepareRunDirs(c);
    expect((await stat(path.join(root, 'real', 'clones'))).mode & 0o777).toBe(0o700);
    const srv = server();
    await enroll(home, srv.transport(), 'http://kiln.test', 'tok', 'node-1', '0.1.0');
    await writeFile(home.path('config.json'), JSON.stringify({ ...(await home.loadConfig()), clone_root: 'relative' }));
    await expect(home.loadConfig()).rejects.toThrow('clone_root 必须是绝对路径');
  });

  it('drain 在还没有数据目录时也能设置和取消', async () => {
    root = await shortTmp('ko-host-');
    const home = new Home(path.join(root, 'none'));
    await home.setDrain(false);
    await home.setDrain(true);
    expect(existsSync(home.drainFile)).toBe(true);
    await home.setDrain(false);
    expect(existsSync(home.drainFile)).toBe(false);
  });

  it.each([
    { service: true, stderr: false },
    { service: false, stderr: true },
  ])('run 的日志：service=$service 时 stderr=$stderr，都写按日文件', async ({ service, stderr }) => {
    root = await shortTmp('ko-host-');
    const home = new Home(path.join(root, 'h'));
    const srv = server();
    await enroll(home, srv.transport(), 'http://kiln.test', 'tok', 'node-1', '0.1.0');
    const lines: string[] = [];
    const spy = vi.spyOn(process.stderr, 'write').mockImplementation((chunk: string | Uint8Array) => {
      lines.push(String(chunk));
      return true;
    });
    const ac = new AbortController();
    try {
      const running = runConnector({
        home,
        transport: (i) => srv.transport(i),
        exec: noCommands,
        version: '0.1.0',
        toolCommand: (h, id) => `kiln-orca task --home ${h.dir} --attempt ${id}`,
      skill: testSkill,
        machineAuth: isolatedAuth(),
        signal: ac.signal,
        service,
      });
      await waitFor(() => srv.acquires.length > 0, 10_000, '领取');
      ac.abort();
      await running;
    } finally {
      spy.mockRestore();
    }
    expect(await readFile(home.path('logs', dailyLog(new Date())), 'utf8')).toContain('kiln-orca 已启动');
    expect(lines.some((l) => l.includes('kiln-orca 已启动'))).toBe(stderr);
  });

  it('被吊销：记下吊销状态后退出、释放锁和 socket；之后启动不联系 Server 直接结束；重新注册清除吊销状态', async () => {
    root = await shortTmp('ko-host-');
    const home = new Home(path.join(root, 'h'));
    const srv = server();
    await enroll(home, srv.transport(), 'http://kiln.test', 'tok', 'node-1', '0.1.0');
    const run = () =>
      runConnector({
        home,
        transport: (i) => srv.transport(i),
        exec: noCommands,
        version: '0.1.0',
        toolCommand: (h, id) => `kiln-orca task --home ${h.dir} --attempt ${id}`,
      skill: testSkill,
        machineAuth: isolatedAuth(),
        signal: new AbortController().signal,
        log: nopLogger,
      });
    srv.revoked = true;
    await expect(run()).rejects.toBeInstanceOf(RevokedError);
    expect(await home.isRevoked('7')).toBe(true);
    expect(existsSync(home.lockPath)).toBe(false);
    expect(existsSync(home.socketPath)).toBe(false);

    // 服务管理器（或人）再次启动：不认证、不领取，直接以吊销结束
    const seen = { acquires: srv.acquires.length, authentications: srv.authentications };
    await expect(run()).rejects.toBeInstanceOf(RevokedError);
    expect({ acquires: srv.acquires.length, authentications: srv.authentications }).toEqual(seen);

    // 重新注册后恢复联系 Server
    srv.revoked = false;
    srv.runnerId = 8n;
    await enroll(home, srv.transport(), 'http://kiln.test', 'tok2', 'node-1', '0.1.0');
    expect(await home.isRevoked('8')).toBe(false);
    const ac = new AbortController();
    const running = runConnector({
      home,
      transport: (i) => srv.transport(i),
      exec: noCommands,
      version: '0.1.0',
      toolCommand: (h, id) => `kiln-orca task --home ${h.dir} --attempt ${id}`,
      skill: testSkill,
      machineAuth: isolatedAuth(),
      signal: ac.signal,
      log: nopLogger,
    });
    await waitFor(() => srv.acquires.length > seen.acquires, 10_000, '重新注册后领取');
    ac.abort();
    await running;
  });

  it('重新注册（改名）：沿用私钥，更新名称和注册信息；Orca 中的邮箱（mailbox.json）不受影响', async () => {
    root = await shortTmp('ko-host-');
    const home = new Home(path.join(root, 'h'));
    const srv = server();
    await enroll(home, srv.transport(), 'http://kiln.test', 'tok', 'old-name', '0.1.0');
    const key = await home.key(false);
    const mailbox = { handle: 'term_1', runId: 'run_1', check: { requestId: 'req-1', terminal: 'term_1', run: 'run_1', waitMs: 1000 } };
    await home.identityStore().save(mailbox);
    srv.runnerId = 8n;
    const st = await enroll(home, srv.transport(), 'http://kiln.test', 'tok2', 'new-name', '0.1.0');
    expect(st.name).toBe('new-name');
    expect(await home.loadState()).toEqual({ server: 'http://kiln.test', runnerId: '8', name: 'new-name', role: 'review' });
    expect(await home.identityStore().load()).toEqual(mailbox);
    expect(await home.key(false)).toEqual(key);
    expect((await home.loadConfig()).name).toBe('new-name');
  });
});

/**
 * GatedHome 让第一次 loadState 读到内容之后停在 gate 上（模拟另一个进程"读了整份、还没写回"）。注册信息与邮箱身份写在
 * 同一个文件、各自读改写整份时，这样的交错会让一方覆盖另一方的更新（审计的复现）。
 */
class GatedHome extends Home {
  private gated = false;
  readonly hit: Promise<void>;
  private onHit: () => void = () => undefined;
  private open: () => void = () => undefined;
  private readonly gate = new Promise<void>((r) => (this.open = r));
  constructor(dir: string) {
    super(dir);
    this.hit = new Promise<void>((r) => (this.onHit = r));
  }
  override async loadState() {
    const s = await super.loadState();
    if (!this.gated) {
      this.gated = true;
      this.onHit();
      await this.gate;
    }
    return s;
  }
  release(): void {
    this.open();
  }
}

describe('数据目录的并发写入：注册信息只有 enroll 写，邮箱身份只有连接器写', () => {
  const pending = { requestId: 'new-pending-request', terminal: 'term_1', run: 'run_1', waitMs: 1000 };

  it('邮箱保存读到旧注册之后 enroll 完成：注册信息是新的，邮箱身份也在', async () => {
    root = await shortTmp('ko-host-');
    const srv = server();
    await enroll(new Home(path.join(root, 'h')), srv.transport(), 'http://kiln.test', 'tok', 'old-name', '0.1.0');
    const home = new GatedHome(path.join(root, 'h'));
    // 运行中的连接器保存邮箱身份（读到的是旧注册时停住），同时另一个进程重新注册
    const saving = home.identityStore().save({ handle: 'term_1', runId: 'run_1', check: pending });
    await Promise.race([home.hit, saving]);
    srv.runnerId = 8n;
    await enroll(home, srv.transport(), 'http://kiln.test', 'tok2', 'new-name', '0.1.0');
    home.release();
    await saving;
    expect(await home.loadState()).toMatchObject({ runnerId: '8', name: 'new-name' });
    expect((await home.identityStore().load())?.check?.requestId).toBe(pending.requestId);
  });

  it('enroll 读到旧内容之后连接器保存了新的 check：check 的请求 ID 还在，注册信息是新的', async () => {
    root = await shortTmp('ko-host-');
    const srv = server();
    const first = new Home(path.join(root, 'h'));
    await enroll(first, srv.transport(), 'http://kiln.test', 'tok', 'old-name', '0.1.0');
    await first.identityStore().save({ handle: 'term_1', runId: 'run_1' });
    const home = new GatedHome(path.join(root, 'h'));
    let opened = (): void => undefined;
    srv.enrollGate = new Promise<void>((r) => (opened = r));
    srv.runnerId = 8n;
    // 重新注册（注册的响应到达后读旧内容时停住），同时运行中的连接器保存新的 check
    const enrolling = enroll(home, srv.transport(), 'http://kiln.test', 'tok2', 'new-name', '0.1.0');
    opened();
    await Promise.race([home.hit, enrolling]);
    await new Home(home.dir).identityStore().save({ handle: 'term_1', runId: 'run_1', check: pending });
    home.release();
    await enrolling;
    expect(await home.loadState()).toMatchObject({ runnerId: '8', name: 'new-name' });
    expect((await home.identityStore().load())?.check?.requestId).toBe(pending.requestId);
  });

  it('旧节点得知吊销晚于重新注册：新的注册不算被吊销', async () => {
    root = await shortTmp('ko-host-');
    const home = new Home(path.join(root, 'h'));
    const srv = server();
    await enroll(home, srv.transport(), 'http://kiln.test', 'tok', 'n', '0.1.0');
    srv.runnerId = 8n;
    await enroll(home, srv.transport(), 'http://kiln.test', 'tok2', 'n', '0.1.0');
    // 还按 runner 7 运行的连接器这时才得知 7 被吊销
    await home.setRevoked('7');
    expect(await home.isRevoked('7')).toBe(true);
    expect(await home.isRevoked((await home.loadState()).runnerId)).toBe(false);
    // 新的连接器正常启动、领取
    const ac = new AbortController();
    const running = runConnector({
      home,
      transport: (i) => srv.transport(i),
      exec: noCommands,
      version: '0.1.0',
      toolCommand: (h, id) => `kiln-orca task --home ${h.dir} --attempt ${id}`,
      skill: testSkill,
      machineAuth: isolatedAuth(),
      signal: ac.signal,
      log: nopLogger,
    });
    await waitFor(() => srv.acquires.length > 0, 10_000, '领取');
    ac.abort();
    await running;
  });

  it('早期版本带 mailbox 字段的 state.json 可读（多出的字段忽略，不迁移），重新注册后不再带它', async () => {
    root = await shortTmp('ko-host-');
    const home = new Home(path.join(root, 'h'));
    await mkdir(home.dir, { recursive: true });
    const old = { server: 'http://kiln.test', runnerId: '5', name: 'n', role: 'review', mailbox: { handle: 'term_9', runId: 'run_9' } };
    await writeFile(home.path('state.json'), JSON.stringify(old));
    expect(await home.loadState()).toEqual({ server: 'http://kiln.test', runnerId: '5', name: 'n', role: 'review' });
    expect(await home.identityStore().load()).toBeUndefined();
    await enroll(home, server().transport(), 'http://kiln.test', 'tok', 'n', '0.1.0');
    expect(JSON.parse(await readFile(home.path('state.json'), 'utf8'))).toEqual({ server: 'http://kiln.test', runnerId: '7', name: 'n', role: 'review' });
  });

  it('同时生成私钥（两个 enroll）：用同一个 seed，不覆盖', async () => {
    root = await shortTmp('ko-host-');
    const homes = Array.from({ length: 8 }, () => new Home(path.join(root, 'h')));
    const seeds = await Promise.all(homes.map((h) => h.key(true)));
    for (const s of seeds) expect(Buffer.from(s).equals(Buffer.from(seeds[0]!))).toBe(true);
    expect(await readdir(path.join(root, 'h', 'keys'))).toEqual(['node.ed25519']);
  });
});
