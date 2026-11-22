import { existsSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import type { Interceptor, Transport } from '@connectrpc/connect';
import { AgentProbe, agentDef, type AgentDef } from '../core/agents.js';
import { defaultTiming, inspectJournal } from '../core/attempt.js';
import { errMessage, RevokedError } from '../core/errors.js';
import type { Exec } from '../core/exec.js';
import { Git } from '../core/git.js';
import { type AuthConflict, machineAuthConflicts } from '../core/gitexec.js';
import { Journal, unreadableFix } from '../core/journal.js';
import { ConnectKiln } from '../core/kiln.js';
import type { Logger } from '../core/log.js';
import { defaultMailboxTiming, Mailbox } from '../core/mailbox.js';
import { KeyedMutex } from '../core/mutex.js';
import { Node, type NodeOptions, protocolVersion } from '../core/node.js';
import { OrcaCli } from '../core/orca.js';
import { Redactor, redactingLogger } from '../core/redact.js';
import { DockerServices, homeOwner, prepareDockerConfig } from '../core/services.js';
import { bundledSkillDir, SkillManager, type SkillProvisioner } from '../core/skill.js';
import { Toolchain } from '../core/toolchain.js';
import { hostId } from './enroll.js';
import { type Home, orcaFallback, serviceSettings } from './home.js';
import { lockFile } from './lock.js';
import { fileLogger, pruneLogs } from './logger.js';
import { serveTools } from './socket.js';
import { kilnRunnerLocator } from './toolchain.js';

// kiln-orca run 的主体（PLAN #33.3）：准备数据目录、取单实例锁、组装节点并运行到 signal 结束。CLI 只负责参数和信号。

const pruneIntervalMs = 24 * 60 * 60_000;

export interface RunOptions {
  home: Home;
  /** transport 构造到 Server 的连接（测试中是进程内的 Connect Transport）。 */
  transport: (interceptors: Interceptor[]) => Transport;
  exec: Exec;
  version: string;
  /** toolCommand 返回 Agent 调用本机工具的命令（kiln-orca task … --attempt <id>）。 */
  toolCommand(home: Home, attemptId: bigint): string;
  signal: AbortSignal;
  /** 作为服务运行（run --service）：正常日志只写文件，不写 stderr（见 logger.ts）。 */
  service?: boolean;
  /** 默认写到数据目录的 logs/（前台运行时同时写 stderr）。 */
  log?: Logger;
  /** 节点的领取、检测间隔（测试中缩短；默认见 node.ts）。 */
  intervals?: Pick<NodeOptions, 'idleMs' | 'statusIntervalMs'>;
  /** 个人认证配置检查的输入（默认是本进程的环境和账户的主目录）；测试注入临时 HOME，不受运行测试的机器上真实的 netrc 影响。 */
  machineAuth?: Omit<MachineAuthCheck, 'exec'>;
  /**
   * skill 把随包分发的 kiln-task skill 放到 Agent 的目录。默认按本进程的用户主目录和环境构造真实的
   * SkillManager；测试注入替身，不读写宿主机上的 ~/.agents / ~/.claude。
   */
  skill?: SkillProvisioner;
}

/** accountHome 返回账户的主目录（取不到时为 undefined）：os.userInfo().homedir，生产代码不提供环境变量覆写。 */
export function accountHome(): string | undefined {
  try {
    return os.userInfo().homedir;
  } catch {
    // 取不到账户的主目录：只检查 HOME 下的 netrc
    return undefined;
  }
}

/** MachineAuthCheck 是个人认证配置检查的输入：执行命令的 exec，以及决定读哪些配置、netrc 的环境和账户的主目录。 */
export interface MachineAuthCheck {
  exec: Exec;
  /** 默认是 process.env（服务用户的环境）。 */
  env?: Record<string, string | undefined>;
  /** 默认是 accountHome()。 */
  accountHome?: string;
}

/**
 * checkMachineAuth 检查这个进程（服务用户）的环境中会对任务仓库主机生效的个人认证配置（machineAuthConflicts）：netrc 和
 * 全局、系统配置中的请求头是部署前提要求没有的（PLAN #33.13），doctor 据此失败，连接器启动时记 warn。只读，不改变节点状态。
 */
export async function checkMachineAuth(c: MachineAuthCheck): Promise<AuthConflict[]> {
  const dir = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-auth-'));
  try {
    const env = c.env ?? process.env;
    return await machineAuthConflicts({ exec: c.exec, env, platform: process.platform, accountHome: c.accountHome ?? accountHome() }, dir);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

export function offeredAgents(names: string[]): AgentDef[] {
  return names.map((n) => agentDef(n)).filter((a): a is AgentDef => a !== undefined);
}

/** JournalCheck 是 doctor 对 journal 的检查结果：summary 一行，problems 是每条读不了的记录。 */
export interface JournalCheck {
  ok: boolean;
  summary: string;
  problems: string[];
}

/**
 * checkJournal 检查 journal（doctor 用，只读）：与节点恢复时的判断相同（inspectJournal）。有读不了的记录时节点不领取新任务、
 * 不回收测试服务，列出文件、原因和处理方法。
 */
export async function checkJournal(j: Journal): Promise<JournalCheck> {
  let scan;
  try {
    scan = await inspectJournal(j);
  } catch (err) {
    return { ok: false, summary: `${j.dir} 读不了（${errMessage(err)}）：节点不能启动`, problems: [] };
  }
  if (scan.unreadable.length === 0) {
    const cleanup = scan.recoveries.filter((r) => r.kind === 'cleanup');
    const releasing = cleanup.filter((r) => r.rec.release).length;
    const reclaiming = cleanup.filter((r) => r.rec.reclaim).length;
    const parts = [releasing > 0 && `${releasing} 条待释放 Orca 中 Agent 的终端`, reclaiming > 0 && `${reclaiming} 条待回收的工作区`].filter((x): x is string => !!x);
    const note = parts.length > 0 ? `，其中 ${parts.join('、')}（不占容量，节点每分钟重试）` : '';
    return { ok: true, summary: `${j.dir}（${scan.recoveries.length} 条未结束的记录${note}）`, problems: [] };
  }
  return {
    ok: false,
    summary: `${scan.unreadable.length} 条记录读不了，节点不领取新任务、不回收测试服务。${unreadableFix}`,
    problems: scan.unreadable.map((u) => `${u.file}：${u.error}`),
  };
}

/**
 * runConnector 运行连接器直到 signal 结束；节点被吊销时停止在途任务、清理完后抛出 RevokedError（CLI 以退出码 0 退出，
 * 服务管理器不再重启，见 service.ts）。吊销状态记在数据目录中：之后启动时不联系 Server，只继续清理，清理完同样退出。
 */
export async function runConnector(o: RunOptions): Promise<void> {
  const { home, exec } = o;
  const config = await home.loadConfig();
  const state = await home.loadState();
  const seed = await home.key(false);
  // 取锁之前建好运行需要的目录：enroll、service install 都不负责建它们
  await home.prepareRunDirs(config);
  const release = lockFile(home.lockPath);
  try {
    // 签发的凭据登记在 redactor 中：连接器的日志、上传的任务日志、错误说明中都替换（PLAN #33.13）
    const redactor = new Redactor();
    const log = redactingLogger(o.log ?? fileLogger(home.path('logs'), { console: !o.service }), redactor);
    const server = config.server || state.server;
    const git = new Git(exec);
    const orca = new OrcaCli(exec, config.orca);
    // Docker 是可选的：专用配置在可用性检测时生成，失败只让 Docker 不可用（见 services.ts），不影响启动和恢复
    const services = new DockerServices(exec, serviceSettings(config), () => prepareDockerConfig(home.path('docker')), homeOwner(home.dir));
    const agents = new AgentProbe(exec, offeredAgents(config.agents));
    const skill =
      o.skill ??
      new SkillManager({
        homeDir: os.homedir(),
        env: process.env,
        sourceDir: bundledSkillDir(),
        version: o.version,
        protocol: protocolVersion,
        log,
      });
    const mailbox = new Mailbox(orca, git, home.identityStore(), home.dir, log, defaultMailboxTiming, redactor);
    const node: Node = new Node(
      {
        kiln: new ConnectKiln(o.transport, BigInt(state.runnerId), seed),
        orca,
        git,
        services,
        servicesReady: () => node.servicesReady,
        toolchain: new Toolchain(exec, kilnRunnerLocator(config.kiln_runner), home.toolchainsDir),
        agents,
        mailbox,
        journal: new Journal(home.journalDir),
        log,
        redactor,
        reposDir: home.reposDir(config),
        tasksDir: home.path('tasks'),
        toolCommand: (id) => o.toolCommand(home, id),
        skill,
        orcaFallback: orcaFallback(config.orca),
        repoLock: new KeyedMutex(),
        orcaRepoIds: new Map(),
        runtimeId: () => node.runtimeId,
        timing: defaultTiming,
        now: Date.now,
      },
      {
        version: o.version,
        capacity: config.max_tasks,
        hostId: await hostId(exec),
        orcaStatus: () => orca.status(),
        dockerStatus: () => services.available(),
        drained: () => existsSync(home.drainFile),
        revoked: await home.isRevoked(state.runnerId),
        onRevoked: () => home.setRevoked(state.runnerId),
        ...o.intervals,
      },
    );
    const notHere = (id: string) => `attempt ${id} 不在这个节点上执行（已经结束或不存在），不要再继续。`;
    /** 传输连接关闭后正在进行的关闭（吊销）：退出前等它们结束。 */
    const closing = new Set<Promise<unknown>>();
    const tools = await serveTools(
      home.socketPath,
      async (req) => {
        const at = node.attempt(req.attempt);
        if (!at) return { code: 1, output: notHere(req.attempt) };
        return at.tool(req.cmd, req.args);
      },
      (req) => {
        const at = node.attempt(req.attempt);
        if (!at) return Promise.resolve({ code: 1, output: notHere(req.attempt), op: '' });
        const r = at.openTransport(req.url, req.pid);
        const close = r.onClose;
        if (!close) return Promise.resolve(r);
        return Promise.resolve({
          ...r,
          onClose: () => {
            const p = close().finally(() => closing.delete(p));
            closing.add(p);
          },
        });
      },
    );
    // 启动时整理一次（包括服务管理器写的 service.*.log），之后每天一次
    await pruneLogs(home.path('logs')).catch(() => undefined);
    const prune = setInterval(() => void pruneLogs(home.path('logs')).catch(() => undefined), pruneIntervalMs);
    log.info('kiln-orca 已启动', { version: o.version, server, runner: state.runnerId, name: state.name, maxTasks: config.max_tasks, agents: config.agents });
    // 部署前提（PLAN #33.13）：这台机器上没有任务仓库主机的个人认证配置。只告警，不停止领取（kiln-orca doctor 同样检查）。
    // 在后台检查：探针卡住（例如主目录在无响应的网络文件系统上）时不拖住启动，到期按无法检查告警
    // 连接器已经退出时不再写日志（日志可能已经关闭）；日志本身出错不变成未处理的拒绝
    let running = true;
    void checkMachineAuth({ exec, ...o.machineAuth })
      .catch((err: unknown) => [{ key: '检查失败', source: errMessage(err) }])
      .then((conflicts) => {
        if (!running) return;
        for (const c of conflicts) {
          log.warn('执行机器上有会对任务仓库主机生效的个人认证配置（或无法检查）：不经 kiln-orca task exec 的 git 可能以这个身份访问仓库；去掉它（kiln-orca doctor 查看）', {
            key: c.key,
            source: c.source,
          });
        }
      })
      .catch(() => undefined);
    const onAbort = () => log.info('收到退出信号，停止领取；Orca 中的 Agent 继续运行，重启后恢复');
    o.signal.addEventListener('abort', onAbort, { once: true });
    try {
      await node.run(o.signal);
    } catch (err) {
      if (err instanceof RevokedError) log.error('这个节点已在 Kiln 中被吊销：退出，服务不再自动重启。重新注册（kiln-orca enroll）后用 kiln-orca service restart 启动');
      throw err;
    } finally {
      running = false;
      o.signal.removeEventListener('abort', onAbort);
      clearInterval(prune);
      // 停止监听；回调在全部连接（包括传输包装的连接）关闭之后才调用
      const stopped = new Promise<void>((r) => tools.close(() => r()));
      // 传输包装保持的连接（git 传输的操作）不会自己结束：attempt 都停下之后断开它们，连接器才能退出；操作随之关闭，
      // 等尽力的吊销结束（RPC 有期限）再退出
      tools.closeAllConnections();
      await stopped;
      await Promise.allSettled([...closing]);
      await rm(home.socketPath, { force: true });
    }
  } finally {
    release();
  }
}
