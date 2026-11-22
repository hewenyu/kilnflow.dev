import { createHash } from 'node:crypto';
import { existsSync } from 'node:fs';
import { mkdir, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { errMessage } from './errors.js';
import { type Exec, ExecError, must } from './exec.js';
import { KeyedMutex } from './mutex.js';

// 标准测试服务（#26.6）：仓库在 .kiln/agent/config.yml 中声明需要 pg、redis，节点检测到 Docker 后用 Docker Compose
// 拉起本机配置的标准服务（只绑定 127.0.0.1，数据放在 tmpfs），连接说明写进 prompt；任务结束后 down -v 删除。
// 没有 Docker 时不拉起，prompt 中说明服务不可用。
//
// 回收：Compose 项目名是 kiln-orca-<数据目录标识>-<attempt>，容器带 kiln.orca.home=<数据目录标识> 标签。删除失败（例如
// Docker 暂时不可用）的项目不在 attempt 中重试，由节点在启动恢复之后和周期清理时 cleanup：只回收本数据目录建的、不属于
// 还在执行的 attempt 的项目（一台机器上的多个节点各有数据目录，互不删除）。
//
// docker 子进程使用专用的客户端配置（DOCKER_CONFIG）：不写 credsStore / credHelpers，并写一个占位的 auths 条目，
// 否则 Docker CLI 会调用平台默认的凭据助手（macOS 钥匙串弹框，无人值守的服务会卡住）。公开镜像一律匿名拉取。
// Docker 是可选能力：专用配置在可用性检测时按需生成，生成失败（用户的 config.json 损坏、读写失败等）时 Docker 按
// 不可用上报，之后的检测再试，修好后不用重启；没有专用配置时不执行任何 docker 命令（不回退到用户自己的配置和凭据）。

export const servicePostgres = 'pg';
export const serviceRedis = 'redis';
const projectPrefix = 'kiln-orca-';
/** homeLabel 标记容器属于哪个数据目录（值是数据目录标识）。 */
const homeLabel = 'kiln.orca.home';
const noCredentialsHost = 'kiln-no-credentials.invalid';
const testDB = { user: 'kiln', password: 'kiln-test-only', name: 'kiln' };
const upTimeoutMs = 10 * 60_000;
const downTimeoutMs = 2 * 60_000;
const probeTimeoutMs = 15_000;

export interface ServiceSettings {
  docker: string;
  postgresImage: string;
  redisImage: string;
  postgresPort: number;
  redisPort: number;
}

export interface StartedServices {
  project: string;
  /** 写进 prompt 的说明（每个服务一行）。 */
  desc: string[];
  /** 写进任务日志的说明（例如端口被占用、改用了其他端口）。 */
  notes: string[];
}

/** Services 是测试服务的操作（测试中替换为假的实现）。 */
export interface Services {
  available(): Promise<{ ok: boolean; reason: string }>;
  start(attemptId: bigint, dir: string, services: string[]): Promise<StartedServices>;
  stop(project: string): Promise<void>;
  /** cleanup 删除本数据目录残留的项目（启动恢复之后、周期清理时）；keepAttempts 中的 attempt 的项目不删。 */
  cleanup(keepAttempts: ReadonlySet<string>): Promise<void>;
}

/** homeOwner 返回数据目录的标识（路径的 sha256 前 12 位），用在 Compose 项目名和标签中。 */
export function homeOwner(dir: string): string {
  return createHash('sha256').update(path.resolve(dir)).digest('hex').slice(0, 12);
}

/** projectName 返回数据目录 owner 中 attempt 的 Compose 项目名。 */
export function projectName(owner: string, attemptId: bigint | string): string {
  return `${projectPrefix}${owner}-${attemptId}`;
}

/** attemptOfProject 返回数据目录 owner 的项目对应的 attempt（不是这个数据目录的项目时返回 undefined）。 */
export function attemptOfProject(owner: string, project: string): string | undefined {
  const prefix = `${projectPrefix}${owner}-`;
  if (!project.startsWith(prefix)) return undefined;
  const id = project.slice(prefix.length);
  return /^\d+$/.test(id) ? id : undefined;
}

export function composeFile(s: ServiceSettings, services: string[], owner: string): string {
  let b = '# kiln-orca 生成的标准测试服务（任务结束后 down -v 删除）\nservices:\n';
  if (services.includes(servicePostgres)) {
    b += `  pg:
    image: ${JSON.stringify(s.postgresImage)}
    environment:
      POSTGRES_USER: ${testDB.user}
      POSTGRES_PASSWORD: ${testDB.password}
      POSTGRES_DB: ${testDB.name}
    ports: ["127.0.0.1:${s.postgresPort}:5432"]
    labels: {${homeLabel}: ${JSON.stringify(owner)}}
    tmpfs: ["/var/lib/postgresql/data"]
    healthcheck: {test: ["CMD-SHELL", "pg_isready -U ${testDB.user} -d ${testDB.name}"], interval: 2s, timeout: 5s, retries: 60}
`;
  }
  if (services.includes(serviceRedis)) {
    b += `  redis:
    image: ${JSON.stringify(s.redisImage)}
    ports: ["127.0.0.1:${s.redisPort}:6379"]
    labels: {${homeLabel}: ${JSON.stringify(owner)}}
    healthcheck: {test: ["CMD", "redis-cli", "ping"], interval: 2s, timeout: 5s, retries: 60}
`;
  }
  return b;
}

/** serviceDesc 返回写进 prompt 的连接说明（Agent 自己设置环境变量）。 */
export function serviceDesc(s: ServiceSettings, services: string[]): string[] {
  const desc: string[] = [];
  if (services.includes(servicePostgres)) {
    const url = `postgres://${testDB.user}:${testDB.password}@127.0.0.1:${s.postgresPort}/${testDB.name}?sslmode=disable`;
    desc.push(
      `PostgreSQL（${s.postgresImage}）：${url}。运行测试前设置环境变量 DATABASE_URL、KILN_TEST_DATABASE_URL 为这个地址，` +
        `以及 PGHOST=127.0.0.1 PGPORT=${s.postgresPort} PGUSER=${testDB.user} PGPASSWORD=${testDB.password} PGDATABASE=${testDB.name}`,
    );
  }
  if (services.includes(serviceRedis)) {
    desc.push(`Redis（${s.redisImage}）：redis://127.0.0.1:${s.redisPort}/0。运行测试前设置环境变量 REDIS_URL 为这个地址`);
  }
  return desc;
}

/** portBusy 判断 127.0.0.1 上的端口是否已被占用。 */
export function portBusy(port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const srv = createServer();
    srv.once('error', () => resolve(true));
    srv.listen(port, '127.0.0.1', () => srv.close(() => resolve(false)));
  });
}

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.once('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const a = srv.address();
      const p = typeof a === 'object' && a ? a.port : 0;
      srv.close(() => resolve(p));
    });
  });
}

/**
 * pickPorts 返回实际使用的端口：配置的端口被占用时（例如另一个任务的服务）改用空闲端口。必须先检查：Docker 运行在
 * 虚拟机中时，宿主机端口被占用不会让 compose up 失败，只是端口转发静默失效。
 */
export async function pickPorts(s: ServiceSettings, services: string[], busy = portBusy): Promise<{ s: ServiceSettings; notes: string[] }> {
  const out = { ...s };
  const notes: string[] = [];
  if (services.includes(servicePostgres) && (await busy(out.postgresPort))) {
    const p = await freePort();
    notes.push(`配置的 PostgreSQL 端口 ${out.postgresPort} 已被占用，改用 ${p}`);
    out.postgresPort = p;
  }
  if (services.includes(serviceRedis) && (await busy(out.redisPort))) {
    const p = await freePort();
    notes.push(`配置的 Redis 端口 ${out.redisPort} 已被占用，改用 ${p}`);
    out.redisPort = p;
  }
  return { s: out, notes };
}

/** DockerConfigError 是生成专用 Docker 客户端配置时某一步失败（消息写明哪一步、哪个文件）。 */
export class DockerConfigError extends Error {
  constructor(step: string, cause: unknown) {
    super(`${step}：${errMessage(cause)}`, { cause });
    this.name = 'DockerConfigError';
  }
}

/** step 执行生成配置的一步，失败时抛出写明这一步的 DockerConfigError。 */
async function step<T>(what: string, f: () => Promise<T>): Promise<T> {
  try {
    return await f();
  } catch (err) {
    throw new DockerConfigError(what, err);
  }
}

/** DockerUserConfig 是从用户 Docker 配置中沿用的一部分（其余不继承，不带任何凭据）。 */
interface DockerUserConfig {
  currentContext?: string;
  cliPluginsExtraDirs?: string[];
  HttpHeaders?: Record<string, string>;
}

/** parseDockerUserConfig 按 unknown 解析用户 Docker 配置，只取要沿用的字段并校验类型；其余字段忽略。 */
export function parseDockerUserConfig(v: unknown): DockerUserConfig {
  if (v === null || typeof v !== 'object' || Array.isArray(v)) throw new Error('顶层不是 JSON 对象');
  const o = v as Record<string, unknown>;
  const out: DockerUserConfig = {};
  if (o.currentContext !== undefined) {
    if (typeof o.currentContext !== 'string') throw new Error('currentContext 应为字符串');
    out.currentContext = o.currentContext;
  }
  if (o.cliPluginsExtraDirs !== undefined) {
    if (!Array.isArray(o.cliPluginsExtraDirs)) throw new Error('cliPluginsExtraDirs 应为字符串数组');
    const dirs: string[] = [];
    for (const d of o.cliPluginsExtraDirs) {
      if (typeof d !== 'string') throw new Error('cliPluginsExtraDirs 应为字符串数组');
      dirs.push(d);
    }
    out.cliPluginsExtraDirs = dirs;
  }
  if (o.HttpHeaders !== undefined) {
    if (o.HttpHeaders === null || typeof o.HttpHeaders !== 'object' || Array.isArray(o.HttpHeaders)) throw new Error('HttpHeaders 应为字符串到字符串的对象');
    const headers: Record<string, string> = {};
    for (const [k, val] of Object.entries(o.HttpHeaders)) {
      if (typeof val !== 'string') throw new Error('HttpHeaders 应为字符串到字符串的对象');
      headers[k] = val;
    }
    out.HttpHeaders = headers;
  }
  return out;
}

/**
 * prepareDockerConfig 在 dir 中生成专用的 Docker 客户端配置：沿用用户配置的 currentContext、插件目录和 HttpHeaders，
 * contexts 链接到原配置目录，不带任何凭据。返回 docker 子进程追加的环境变量。任何一步失败抛出 DockerConfigError。
 */
export async function prepareDockerConfig(dir: string, userDir = process.env.DOCKER_CONFIG || path.join(os.homedir(), '.docker')): Promise<Record<string, string>> {
  userDir = path.resolve(userDir);
  let user: DockerUserConfig = {};
  const file = path.join(userDir, 'config.json');
  if (existsSync(file)) {
    const text = (await step(`读取 ${file} 失败`, () => readFile(file, 'utf8'))).trim();
    if (text) user = await step(`${file} 不是有效的 JSON`, () => Promise.resolve(parseDockerUserConfig(JSON.parse(text) as unknown)));
  }
  const cfg = {
    auths: { [noCredentialsHost]: {} },
    ...(user.currentContext ? { currentContext: user.currentContext } : {}),
    cliPluginsExtraDirs: [...(user.cliPluginsExtraDirs ?? []), path.join(userDir, 'cli-plugins')],
    ...(user.HttpHeaders ? { HttpHeaders: user.HttpHeaders } : {}),
  };
  const out = path.join(dir, 'config.json');
  await step(`写入 ${out} 失败`, async () => {
    await mkdir(dir, { recursive: true, mode: 0o700 });
    await writeFile(out, JSON.stringify(cfg, null, 2), { mode: 0o600 });
  });
  const link = path.join(dir, 'contexts');
  await rm(link, { force: true, recursive: false }).catch(() => undefined);
  const contexts = path.join(userDir, 'contexts');
  if (existsSync(contexts)) await step(`建立 ${link} → ${contexts} 的符号链接失败`, () => symlink(contexts, link));
  const env: Record<string, string> = { DOCKER_CONFIG: dir };
  if (!process.env.DOCKER_CERT_PATH) env.DOCKER_CERT_PATH = userDir;
  return env;
}

/** DockerServices 通过 docker compose 操作测试服务。 */
export class DockerServices implements Services {
  /** 专用配置的环境变量（生成成功后不再变）；正在生成或上次失败时为 undefined。 */
  private env?: Promise<Record<string, string>>;

  constructor(
    private readonly exec: Exec,
    private readonly settings: ServiceSettings,
    /** prepareConfig 生成专用的 Docker 客户端配置，返回 docker 子进程追加的环境变量（通常是 prepareDockerConfig）。 */
    private readonly prepareConfig: () => Promise<Record<string, string>>,
    /** owner 是数据目录标识（homeOwner），区分同一台机器上的多个节点。 */
    private readonly owner: string,
  ) {}

  /**
   * dockerEnv 返回专用配置的环境变量：还没有生成或上次失败时生成（同时调用时共用同一次），失败时抛出、下次调用再试。
   * 成功后沿用：之后再写配置文件可能让正在执行的 docker 读到写了一半的文件。
   */
  private dockerEnv(): Promise<Record<string, string>> {
    this.env ??= this.prepareConfig().catch((err: unknown) => {
      this.env = undefined;
      throw err;
    });
    return this.env;
  }

  /** run 执行 docker 命令：没有专用配置时抛出，不执行（不能用用户自己的配置和凭据）。 */
  private async run(args: string[], timeoutMs: number): Promise<string> {
    const env = await this.dockerEnv();
    return must(this.exec, this.settings.docker, args, { env, timeoutMs });
  }

  async available() {
    try {
      await this.dockerEnv();
    } catch (err) {
      return { ok: false, reason: `无法生成专用的 Docker 客户端配置（${errMessage(err)}）` };
    }
    try {
      await this.run(['info', '--format', '{{.ServerVersion}}'], probeTimeoutMs);
    } catch {
      return { ok: false, reason: 'Docker daemon 不可用' };
    }
    try {
      await this.run(['compose', 'version'], probeTimeoutMs);
    } catch {
      return { ok: false, reason: '没有 docker compose 插件' };
    }
    return { ok: true, reason: '' };
  }

  /** 同时执行的任务的 start 串行：选端口到 compose up 完成之间不能有别的任务选同一个端口。 */
  private readonly starting = new KeyedMutex();

  start(attemptId: bigint, dir: string, services: string[]): Promise<StartedServices> {
    return this.starting.run('start', () => this.doStart(attemptId, dir, services));
  }

  private async doStart(attemptId: bigint, dir: string, services: string[]): Promise<StartedServices> {
    const project = projectName(this.owner, attemptId);
    const { s, notes } = await pickPorts(this.settings, services);
    const file = path.join(dir, 'compose.yml');
    await writeFile(file, composeFile(s, services, this.owner), { mode: 0o600 });
    try {
      await this.run(['compose', '-p', project, '-f', file, 'up', '-d', '--wait', '--quiet-pull'], upTimeoutMs);
    } catch (err) {
      await this.stop(project).catch(() => undefined);
      const msg = err instanceof ExecError ? err.message : String(err);
      if (/pull access denied|unauthorized|authentication required/i.test(msg)) {
        throw new Error(`${msg}（测试服务镜像只匿名拉取，不使用任何 registry 凭据：postgres_image、redis_image 必须是公开镜像）`);
      }
      throw err;
    }
    return { project, desc: serviceDesc(s, services), notes };
  }

  async stop(project: string) {
    await this.run(['compose', '-p', project, 'down', '-v', '--remove-orphans', '--timeout', '5'], downTimeoutMs);
    const left = await this.run(['ps', '-aq', '--filter', `label=com.docker.compose.project=${project}`], probeTimeoutMs);
    if (left) throw new Error(`删除后仍有容器残留：${left.split(/\s+/).join(' ')}`);
  }

  async cleanup(keepAttempts: ReadonlySet<string>) {
    const out = await this.run(['ps', '-a', '--filter', `label=${homeLabel}=${this.owner}`, '--format', '{{.Label "com.docker.compose.project"}}'], probeTimeoutMs);
    const projects = new Set(
      out.split(/\s+/).filter((p) => {
        const id = attemptOfProject(this.owner, p);
        return id !== undefined && !keepAttempts.has(id);
      }),
    );
    // 一个项目删除失败不影响其他项目；最后报告第一个错误
    let first: unknown;
    for (const p of projects) await this.stop(p).catch((err: unknown) => (first ??= err));
    if (first !== undefined) throw first;
  }
}
