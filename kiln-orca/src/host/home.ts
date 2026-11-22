import { randomBytes, randomUUID } from 'node:crypto';
import { existsSync, realpathSync } from 'node:fs';
import { chmod, link, mkdir, readFile, rename, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { knownAgents } from '../core/agents.js';
import type { Identity, IdentityStore } from '../core/mailbox.js';
import type { ServiceSettings } from '../core/services.js';
import { shellWord } from './escape.js';

// 数据目录（PLAN #33.3）：macOS ~/Library/Application Support/kiln-orca，Linux $XDG_DATA_HOME/kiln-orca
// （KILN_ORCA_HOME 可改）。运行需要的目录由 run 在取锁之前建好（prepareRunDirs）。
//
// 每个文件只有一类写入者，没有两个进程会对同一个文件"读整份 → 改一部分 → 写回整份"（不需要跨进程的文件锁）：
//
// | 文件 | 写入者 | 说明 |
// |---|---|---|
// | config.json | enroll（改 server、name，其余保留）；人手工编辑 | 连接器、doctor 只在启动时读 |
// | state.json | 只有 enroll（整份写入注册信息，0600） | 连接器只在启动时读；改动在重启后生效 |
// | mailbox.json | 只有持有单实例锁的连接器（邮箱身份和没处理完的 check，0600） | enroll 不碰；见 identityStore |
// | keys/node.ed25519 | 只有 enroll，只在不存在时创建（不覆盖，见 key） | 私钥 seed（0600），重新注册沿用 |
// | revoked | 连接器得知吊销时写入（内容是被吊销的 runner id）；enroll 删除 | 只对 state.json 中的同一个 runner id 生效（见 isRevoked） |
// | drain | drain on 创建、drain off 删除（不读内容） | 连接器每次领取前看它在不在 |
// | journal/、inbox/、tasks/、kiln-orca/ | 只有持有单实例锁的连接器（tasks/ 中还有 Agent 写的报告） | doctor 只读 journal/ |
// | repos/（或 clone_root） | 连接器（clone、fetch）与 Orca（在其上建 worktree） | 二者由 repoLock 互斥（attempt.ts） |
// | run/kiln-orca.pid、run/kiln-orca.sock | 单实例锁（独占创建，见 lock.ts）；socket 只由持锁的连接器建、删 | enroll 只读锁看连接器在不在 |
// | docker/ | 只有持锁的连接器（doctor 在临时目录中生成） | 专用 Docker 客户端配置 |
// | toolchains/ | 持锁的连接器调用的 kiln-runner toolchain（安装由它的文件锁互斥） | 受管理的工具链（PLAN #33.12），按 digest 复用 |
// | logs/ | 连接器（追加、每天整理）与服务管理器（service.*.log） | 都只追加 |
//
// 同一个数据目录同时执行两个 enroll 是人自己的并发操作：每个都在 Kiln 中注册一个节点，后写完的 state.json 为准。

/** runDirs 是 kiln-orca run 需要的目录（0700）。docker/ 不在其中：Docker 是可选的，由生成专用配置时建（services.ts）。 */
const runDirs = ['run', 'logs', 'journal', 'tasks', 'repos', 'inbox', 'toolchains'];

/**
 * defaultOrcaBins 是没有配置 orca 时找 Orca 命令行的顺序：macOS 先找 Orca.app 中的，再找 PATH 中的 orca；Linux 上 Orca 的
 * 命令行叫 orca-ide（裸的 orca 通常是 GNOME 的屏幕阅读器，不能用）。
 */
export function defaultOrcaBins(platform: string = process.platform): string[] {
  return platform === 'darwin' ? ['/Applications/Orca.app/Contents/Resources/bin/orca', 'orca'] : ['orca-ide'];
}

/** defaultOrcaBin 是没有配置 orca 时用的 Orca 命令行：defaultOrcaBins 中第一个存在的绝对路径，都没有时是最后一个名字。 */
export function defaultOrcaBin(platform: string = process.platform): string {
  const bins = defaultOrcaBins(platform);
  return bins.find((b) => path.isAbsolute(b) && existsSync(b)) ?? bins[bins.length - 1]!;
}

/**
 * orcaFallback 是 kiln-orca task done 发送 worker_done 的后备 Orca 命令行（Agent 的环境中没有 ORCA_CLI_COMMAND 时用，
 * PLAN #33.5）：连接器自己执行的那个。config.json 的 orca 带路径时给出绝对路径（相对的按连接器的工作目录，与连接器执行
 * 时相同）；只是名字时给出名字和连接器的 PATH，由 task done 用这个 PATH 按名字执行——查找交给真实的执行器，与连接器自己
 * 执行 Orca 时是同一套规则，不在这里重做。
 */
export function orcaFallback(orca: string, env: NodeJS.ProcessEnv = process.env, cwd: string = process.cwd()): { command: string; path: string } {
  return { command: orca.includes('/') ? path.resolve(cwd, orca) : orca, path: env.PATH ?? '' };
}

export interface Config {
  /** Server 地址（注册时写入）。 */
  server: string;
  name: string;
  /** 同时执行的任务数上限。 */
  max_tasks: number;
  /** 提供的 Agent（Kiln 中的名字）。 */
  agents: string[];
  orca: string;
  docker: string;
  /**
   * kiln-runner 的路径（准备受管理的工具链用，PLAN #33.12）。为空时用随 kiln-orca 安装的本平台的 kiln-runner 包
   * （host/toolchain.ts）。
   */
  kiln_runner: string;
  /**
   * clone_root 是仓库 clone 的根目录（绝对路径，PLAN #33.13）：每个仓库一个 clone，Orca 在其上建 worktree。为空时是数据目录
   * 中的 repos/。只在没有执行中、待回收的任务时修改（旧目录中的 clone 不再被使用和回收，可以手工删除）。
   */
  clone_root: string;
  postgres_image: string;
  redis_image: string;
  postgres_port: number;
  redis_port: number;
}

/** State 是注册信息（state.json，只有 enroll 写）。 */
export interface State {
  server: string;
  runnerId: string;
  name: string;
  role: string;
}

/** parseState 从 state.json 的内容取出注册信息；多出的字段（例如早期版本放在这里的 mailbox）忽略。 */
export function parseState(p: string, text: string): State {
  const v: unknown = JSON.parse(text);
  if (v === null || typeof v !== 'object' || Array.isArray(v)) throw new Error(`${p} 不是注册信息`);
  const o = v as Record<string, unknown>;
  const field = (k: keyof State): string => {
    const x = o[k];
    if (typeof x !== 'string') throw new Error(`${p} 缺少 ${k}`);
    return x;
  };
  return { server: field('server'), runnerId: field('runnerId'), name: field('name'), role: field('role') };
}

/** parseIdentity 解析 mailbox.json（邮箱身份）；内容不对时抛出。 */
export function parseIdentity(p: string, text: string): Identity {
  const v: unknown = JSON.parse(text);
  const o = v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
  if (!o || typeof o.handle !== 'string' || typeof o.runId !== 'string') throw new Error(`${p} 不是邮箱身份`);
  const id: Identity = { handle: o.handle, runId: o.runId };
  const c = o.check;
  if (c !== undefined) {
    const r = c !== null && typeof c === 'object' && !Array.isArray(c) ? (c as Record<string, unknown>) : undefined;
    if (!r || typeof r.requestId !== 'string' || typeof r.terminal !== 'string' || typeof r.run !== 'string' || typeof r.waitMs !== 'number') {
      throw new Error(`${p} 中的 check 不完整`);
    }
    id.check = { requestId: r.requestId, terminal: r.terminal, run: r.run, waitMs: r.waitMs };
  }
  return id;
}

/**
 * canonicalDir 返回数据目录的规范路径：绝对路径，已经存在的部分解析符号链接（realpath），还不存在的部分原样接上。一个
 * 数据目录只有一个规范路径（经符号链接、相对路径、/tmp 与 /private/tmp 指定的都相同），而且建好目录前后不变：服务的
 * 身份、单实例锁之外的数据目录标识（homeOwner）都按它计算，同一个目录不会被当成两个节点、装出两个服务。数据目录被移动
 * 后是另一个路径（先在原来的位置 service uninstall）。
 */
export function canonicalDir(p: string): string {
  let dir = path.resolve(p);
  const rest: string[] = [];
  for (;;) {
    try {
      return path.join(realpathSync.native(dir), ...rest);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== 'ENOENT') throw err;
    }
    const parent = path.dirname(dir);
    if (parent === dir) return path.resolve(p);
    rest.unshift(path.basename(dir));
    dir = parent;
  }
}

/** toolCommand 是给 Agent 的本机工具命令（kiln-orca task … --attempt <id>），每个词按 shell 引用（见 escape.ts）。 */
export function toolCommand(node: string, cli: string, home: string, attemptId: bigint): string {
  return [node, cli, 'task', '--home', home, '--attempt', String(attemptId)].map(shellWord).join(' ');
}

export function defaultHome(): string {
  if (process.env.KILN_ORCA_HOME) return path.resolve(process.env.KILN_ORCA_HOME);
  if (process.platform === 'darwin') return path.join(os.homedir(), 'Library', 'Application Support', 'kiln-orca');
  return path.join(process.env.XDG_DATA_HOME || path.join(os.homedir(), '.local', 'share'), 'kiln-orca');
}

export function defaultConfig(): Config {
  return {
    server: '',
    name: '',
    max_tasks: 2,
    agents: knownAgents.map((a) => a.name),
    orca: defaultOrcaBin(),
    docker: 'docker',
    kiln_runner: '',
    clone_root: '',
    postgres_image: 'postgres:17',
    redis_image: 'redis:7',
    postgres_port: 55432,
    redis_port: 56379,
  };
}

/** stringFields 是 Config 中的字符串字段（validateConfig 检查类型）。 */
const stringFields = ['server', 'name', 'orca', 'docker', 'kiln_runner', 'clone_root', 'postgres_image', 'redis_image'] as const;

/**
 * applyConfig 把 config.json 中认识的字段按类型写入 c（先放默认值）：类型不对时抛出，不认识的字段忽略。JSON 按 unknown
 * 解析，不把未校验的内容直接当成 Config。
 */
function applyConfig(c: Config, v: unknown, p: string): void {
  if (v === null || typeof v !== 'object' || Array.isArray(v)) throw new Error(`${p} 不是配置对象`);
  const o = v as Record<string, unknown>;
  const text = (k: string): string | undefined => {
    const x = o[k];
    if (x === undefined) return undefined;
    if (typeof x !== 'string') throw new Error(`${p}：${k} 应为字符串`);
    return x;
  };
  const integer = (k: string): number | undefined => {
    const x = o[k];
    if (x === undefined) return undefined;
    if (typeof x !== 'number' || !Number.isInteger(x)) throw new Error(`${p}：${k} 应为整数`);
    return x;
  };
  for (const k of stringFields) {
    const x = text(k);
    if (x !== undefined) c[k] = x;
  }
  const max = integer('max_tasks');
  if (max !== undefined) c.max_tasks = max;
  const pg = integer('postgres_port');
  if (pg !== undefined) c.postgres_port = pg;
  const redis = integer('redis_port');
  if (redis !== undefined) c.redis_port = redis;
  const agents = o.agents;
  if (agents !== undefined) {
    if (!Array.isArray(agents)) throw new Error(`${p}：agents 应为字符串数组`);
    const names: string[] = [];
    for (const a of agents) {
      if (typeof a !== 'string') throw new Error(`${p}：agents 应为字符串数组`);
      names.push(a);
    }
    c.agents = names;
  }
}

/** validateConfig 检查配置，返回问题（为空表示有效）。 */
export function validateConfig(c: Config): string[] {
  const problems: string[] = [];
  for (const k of stringFields) if (typeof c[k] !== 'string') problems.push(`${k} 必须是字符串`);
  if (!Array.isArray(c.agents)) {
    problems.push('agents 必须是字符串数组');
  } else {
    for (const a of c.agents) {
      if (typeof a !== 'string' || !knownAgents.some((k) => k.name === a)) problems.push(`agents 中的 ${a} 不是 Kiln 认识的 Agent（${knownAgents.map((k) => k.name).join('、')}）`);
    }
  }
  if (!Number.isInteger(c.max_tasks) || c.max_tasks < 1 || c.max_tasks > 32) problems.push('max_tasks 必须是 1–32 的整数');
  if (typeof c.kiln_runner !== 'string' || (c.kiln_runner !== '' && !path.isAbsolute(c.kiln_runner))) problems.push('kiln_runner 必须是绝对路径（为空时用随 kiln-orca 安装的 kiln-runner）');
  if (typeof c.clone_root !== 'string' || (c.clone_root !== '' && !path.isAbsolute(c.clone_root))) problems.push('clone_root 必须是绝对路径（为空时用数据目录中的 repos/）');
  for (const [k, v] of [['postgres_port', c.postgres_port], ['redis_port', c.redis_port]] as const) {
    if (!Number.isInteger(v) || v < 1024 || v > 65535) problems.push(`${k} 必须是 1024–65535 的整数`);
  }
  return problems;
}

/** writeAtomic 先写临时文件再改名；临时文件每次唯一，并发的写入（例如另一个进程中的 enroll）不会移走彼此的临时文件。 */
async function writeAtomic(p: string, data: string | Uint8Array, mode: number): Promise<void> {
  await mkdir(path.dirname(p), { recursive: true, mode: 0o700 });
  const tmp = `${p}.${randomUUID()}.tmp`;
  try {
    await writeFile(tmp, data, { mode });
    await chmod(tmp, mode);
    await rename(tmp, p);
  } catch (err) {
    await rm(tmp, { force: true }).catch(() => undefined);
    throw err;
  }
}

export class Home {
  constructor(readonly dir: string) {}

  path(...p: string[]): string {
    return path.join(this.dir, ...p);
  }
  get socketPath(): string {
    return this.path('run', 'kiln-orca.sock');
  }
  get lockPath(): string {
    return this.path('run', 'kiln-orca.pid');
  }
  /** toolchainsDir 是受管理的工具链的安装目录（toolchains/<digest hex>/，PLAN #33.12）。 */
  get toolchainsDir(): string {
    return this.path('toolchains');
  }
  /** journalDir 是本机执行日志（未结束的 attempt 的记录，见 core/journal.ts）的目录。 */
  get journalDir(): string {
    return this.path('journal');
  }

  /** reposDir 是仓库 clone 的根目录：配置的 clone_root（规范路径，见 canonicalDir），为空时是数据目录中的 repos/。 */
  reposDir(c: Pick<Config, 'clone_root'>): string {
    return c.clone_root ? canonicalDir(c.clone_root) : this.path('repos');
  }

  /** prepareRunDirs 建好运行需要的目录（0700；run/ 中有 socket，已经存在时也改为 0700），包括 c 的 clone 根目录。 */
  async prepareRunDirs(c: Pick<Config, 'clone_root'>): Promise<void> {
    await mkdir(this.dir, { recursive: true, mode: 0o700 });
    for (const d of runDirs) await mkdir(this.path(d), { recursive: true, mode: 0o700 });
    await mkdir(this.reposDir(c), { recursive: true, mode: 0o700 });
    await chmod(this.path('run'), 0o700);
  }

  /** setDrain 停止（on）或恢复（off）领取新任务。 */
  async setDrain(on: boolean): Promise<void> {
    if (!on) return rm(this.drainFile, { force: true });
    await mkdir(this.dir, { recursive: true, mode: 0o700 });
    await writeFile(this.drainFile, '');
  }
  get drainFile(): string {
    return this.path('drain');
  }

  /** revokedFile 记下在 Kiln 中被吊销的节点（runner id）：运行中得知时写入，重新注册时删除。 */
  get revokedFile(): string {
    return this.path('revoked');
  }
  /**
   * isRevoked 判断 runnerId 这个节点是否已被吊销（revokedFile 中记的是它）。文件中是别的 runner id 时不算：重新注册与运行中的
   * 连接器同时进行时，旧节点得知吊销写入的文件可能晚于 enroll 的删除，不能让它挡住新的注册。
   */
  async isRevoked(runnerId: string): Promise<boolean> {
    const text = await readFile(this.revokedFile, 'utf8').catch((err: unknown) => {
      if ((err as NodeJS.ErrnoException).code === 'ENOENT') return undefined;
      throw err;
    });
    return text !== undefined && text.split('\n')[0]?.trim() === runnerId;
  }
  /** setRevoked 记下 runnerId 这个节点已被吊销。 */
  async setRevoked(runnerId: string): Promise<void> {
    await writeAtomic(this.revokedFile, `${runnerId}\n${new Date().toISOString()}\n`, 0o600);
  }
  /** clearRevoked 清除吊销状态（重新注册时）。 */
  async clearRevoked(): Promise<void> {
    await rm(this.revokedFile, { force: true });
  }

  async loadConfig(): Promise<Config> {
    const c = defaultConfig();
    const p = this.path('config.json');
    if (existsSync(p)) applyConfig(c, JSON.parse(await readFile(p, 'utf8')) as unknown, p);
    const problems = validateConfig(c);
    if (problems.length > 0) throw new Error(`${p}：${problems.join('；')}`);
    return c;
  }

  async saveConfig(c: Config): Promise<void> {
    await writeAtomic(this.path('config.json'), JSON.stringify(c, null, 2) + '\n', 0o644);
  }

  async loadState(): Promise<State> {
    const p = this.path('state.json');
    if (!existsSync(p)) throw new Error('还没有注册：先运行 kiln-orca enroll --server <地址> --token <注册令牌>');
    return parseState(p, await readFile(p, 'utf8'));
  }

  async saveState(s: State): Promise<void> {
    const st: State = { server: s.server, runnerId: s.runnerId, name: s.name, role: s.role };
    await writeAtomic(this.path('state.json'), JSON.stringify(st, null, 2) + '\n', 0o600);
  }

  /**
   * key 读取私钥 seed（32 字节）；create 为 true 时不存在就生成。生成的 seed 只在文件还不存在时放进去（不覆盖）：同时执行的
   * 两个 enroll 用同一个 seed，不会有一个用被覆盖掉的私钥注册。
   */
  async key(create: boolean): Promise<Uint8Array> {
    const p = this.path('keys', 'node.ed25519');
    if (!existsSync(p)) {
      if (!create) throw new Error(`没有私钥 ${p}：先注册`);
      await mkdir(path.dirname(p), { recursive: true, mode: 0o700 });
      const tmp = `${p}.${randomUUID()}.tmp`;
      try {
        await writeFile(tmp, randomBytes(32), { mode: 0o600 });
        await chmod(tmp, 0o600);
        // link 在目标已经存在时失败（EEXIST）：另一个 enroll 先放好了，用它的
        await link(tmp, p).catch((err: unknown) => {
          if ((err as NodeJS.ErrnoException).code !== 'EEXIST') throw err;
        });
      } finally {
        await rm(tmp, { force: true }).catch(() => undefined);
      }
    }
    const b = await readFile(p);
    if (b.length !== 32) throw new Error(`${p} 不是 32 字节的 ed25519 seed`);
    return b;
  }

  /** mailboxFile 是连接器在 Orca 中的邮箱身份（只有持有单实例锁的连接器写，见文件开头的表）。 */
  get mailboxFile(): string {
    return this.path('mailbox.json');
  }

  /** identityStore 把邮箱身份保存在 mailbox.json 中（与注册信息分开：enroll 和运行中的连接器各写各的文件）。 */
  identityStore(): IdentityStore {
    return {
      load: async () => {
        const text = await readFile(this.mailboxFile, 'utf8').catch((err: unknown) => {
          if ((err as NodeJS.ErrnoException).code === 'ENOENT') return undefined;
          throw err;
        });
        return text === undefined ? undefined : parseIdentity(this.mailboxFile, text);
      },
      save: (id) => writeAtomic(this.mailboxFile, JSON.stringify(id, null, 2) + '\n', 0o600),
    };
  }
}

export function serviceSettings(c: Config): ServiceSettings {
  return { docker: c.docker, postgresImage: c.postgres_image, redisImage: c.redis_image, postgresPort: c.postgres_port, redisPort: c.redis_port };
}
