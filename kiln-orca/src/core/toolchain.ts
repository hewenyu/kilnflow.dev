import { errMessage } from './errors.js';
import { type Exec, ExecError } from './exec.js';

// 受管理的工具链（PLAN #33.12）：任务声明的 Go、Node、pnpm 等（AITask.tools，版本和摘要由 Server 按仓库的
// .kiln/runtimes.lock 解析）装在连接器自己的数据目录 toolchains/<digest hex>/ 中，已装好的直接复用、缺少的才下载。
// 下载、校验、解包、并发安装和回收的规则只有 Go 一份（internal/toolchain，与 CI Runner 共用）：连接器调用随 kiln-orca
// 一起安装的 kiln-runner 的 toolchain 子命令（不注册为 CI Runner、不读它的配置、不连接 Server），这里只负责调用、
// 解析输出、把环境变量交给 Agent 的命令（kiln-orca task exec）。
//
// ensure 与 gc 在同一个目录上不能同时进行（gc 可能删掉 ensure 刚确认装好的目录）：连接器是这个数据目录唯一的进程
// （单实例锁），所以用进程内的闸门——有 ensure 在进行时跳过这一轮 gc，gc 进行中时 ensure 等它结束。

/** toolchainFormat 是 kiln-runner toolchain 输出的格式版本（toolchainctl.Format），只接受这一版。 */
export const toolchainFormat = 1;

/** info 的期限（只打印版本，不访问网络）。 */
const infoTimeoutMs = 30_000;
/** ensure 的期限：缺少时下载、解包 Go、Node 等（每个几十到上百 MB）。 */
const ensureTimeoutMs = 30 * 60_000;
/** gc 的期限（统计目录大小、删除）。 */
const gcTimeoutMs = 10 * 60_000;

/** ToolSpec 是任务的一个工具（AITask.tools 的一项）。 */
export interface ToolSpec {
  name: string;
  version: string;
  platform: string;
  digest: string;
}

/** ToolchainEnv 是工具链的环境变量：set 直接设置，prepend 中的变量把这些路径按顺序加在原值之前（toolchain.Env）。 */
export interface ToolchainEnv {
  set: Record<string, string>;
  prepend: Record<string, string[]>;
}

/** EnsuredTool 是装好（或复用）的一个工具。 */
export interface EnsuredTool {
  name: string;
  version: string;
  digest: string;
  dir: string;
  reused: boolean;
}

/** ToolchainRecord 是一个 attempt 准备好的工具链（写进 journal：连接器重启后 task exec 仍能用）。 */
export interface ToolchainRecord {
  tools: EnsuredTool[];
  env: ToolchainEnv;
}

/** ToolchainStatus 是节点能否准备工具链（上报给 Server：没有就绪时不派带工具链的任务）。 */
export interface ToolchainStatus {
  ok: boolean;
  reason: string;
  /** kiln-runner 的版本（info 的输出）。 */
  version: string;
  bin: string;
}

/** Locator 返回 kiln-runner 的路径；找不到时 bin 为空，reason 说明原因和处理方法。 */
export type Locator = () => { bin?: string; reason: string };

/** GCSummary 是一次回收的结果（跳过时为 undefined）。 */
export interface GCSummary {
  removed: number;
  removedBytes: number;
  failures: string[];
}

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function stringMap(v: unknown): v is Record<string, string> {
  return isObject(v) && Object.values(v).every((x) => typeof x === 'string');
}

function listMap(v: unknown): v is Record<string, string[]> {
  return isObject(v) && Object.values(v).every((x) => Array.isArray(x) && x.every((y) => typeof y === 'string'));
}

/** parseEnv 校验工具链的环境变量（ensure 的输出、journal 中的记录）；不合格时抛出。 */
export function parseEnv(v: unknown): ToolchainEnv {
  if (!isObject(v) || !stringMap(v.set) || !listMap(v.prepend)) throw new Error('环境变量的格式不对（应为 {set: {…}, prepend: {…}}）');
  return { set: { ...v.set }, prepend: Object.fromEntries(Object.entries(v.prepend).map(([k, p]) => [k, [...p]])) };
}

/**
 * parseEnsure 解析 kiln-runner toolchain ensure 的输出：格式版本必须是 toolchainFormat，工具与请求的逐个对应（名称、
 * 版本、digest，顺序相同），目录是绝对路径。不合格时抛出（不把不认识的输出当成装好了）。
 */
export function parseEnsure(stdout: string, want: readonly ToolSpec[]): ToolchainRecord {
  let v: unknown;
  try {
    v = JSON.parse(stdout);
  } catch (err) {
    throw new Error(`kiln-runner toolchain ensure 的输出不是 JSON（${errMessage(err)}）`, { cause: err });
  }
  if (!isObject(v) || v.format !== toolchainFormat) {
    throw new Error(`kiln-runner toolchain ensure 的输出格式版本是 ${JSON.stringify(isObject(v) ? v.format : undefined)}，连接器只认识 ${toolchainFormat}（kiln-runner 与 kiln-orca 的版本不一致）`);
  }
  const tools = v.tools;
  if (!Array.isArray(tools) || tools.length !== want.length) throw new Error('kiln-runner toolchain ensure 输出的工具与请求的不一致');
  const out: EnsuredTool[] = [];
  for (const [i, t] of tools.entries()) {
    const w = want[i] as ToolSpec;
    if (!isObject(t) || t.name !== w.name || t.version !== w.version || t.digest !== w.digest || typeof t.dir !== 'string' || !t.dir.startsWith('/') || typeof t.reused !== 'boolean') {
      throw new Error(`kiln-runner toolchain ensure 输出的第 ${i + 1} 项与请求的 ${w.name} ${w.version} 不一致`);
    }
    out.push({ name: t.name, version: t.version, digest: t.digest, dir: t.dir, reused: t.reused });
  }
  return { tools: out, env: parseEnv(v.env) };
}

/**
 * applyToolchainEnv 返回执行命令用的环境：base（调用者的环境）加上工具链的设置——set 覆盖原值，prepend 的路径加在原值
 * 之前（用 ":" 连接，原值为空时只有这些路径）。规则与 Go 的 toolchain.Env.Apply 相同。
 */
export function applyToolchainEnv(base: Readonly<Record<string, string | undefined>>, env: ToolchainEnv): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(base)) if (v !== undefined) out[k] = v;
  Object.assign(out, env.set);
  for (const [k, paths] of Object.entries(env.prepend)) {
    out[k] = [...paths, ...(out[k] ? [out[k]] : [])].join(':');
  }
  return out;
}

/** Toolchain 调用 kiln-runner toolchain 准备、回收 root 中的工具链。 */
export class Toolchain {
  private ensuring = 0;
  private collecting: Promise<unknown> | undefined;

  constructor(
    private readonly exec: Exec,
    private readonly locate: Locator,
    /** 安装目录（<数据目录>/toolchains）。 */
    readonly root: string,
  ) {}

  /** status 检查能否准备工具链：找到 kiln-runner，且它的 toolchain 子命令是连接器认识的格式版本。 */
  async status(): Promise<ToolchainStatus> {
    const { bin, reason } = this.locate();
    if (!bin) return { ok: false, reason, version: '', bin: '' };
    const r = await this.exec(bin, ['toolchain', 'info'], { timeoutMs: infoTimeoutMs });
    if (r.code !== 0) {
      // 旧版本的 kiln-runner 没有 toolchain 子命令（用法错误）
      return { ok: false, reason: `${bin} 不支持 toolchain 子命令（退出码 ${r.code}：${r.stderr.trim().split('\n')[0] ?? ''}），kiln-runner 与 kiln-orca 的版本不一致`, version: '', bin };
    }
    try {
      const v: unknown = JSON.parse(r.stdout);
      if (!isObject(v) || v.format !== toolchainFormat) {
        return { ok: false, reason: `${bin} 的 toolchain 格式版本是 ${JSON.stringify(isObject(v) ? v.format : undefined)}，连接器只认识 ${toolchainFormat}（kiln-runner 与 kiln-orca 的版本不一致）`, version: '', bin };
      }
      return { ok: true, reason: '', version: typeof v.version === 'string' ? v.version : '', bin };
    } catch (err) {
      return { ok: false, reason: `${bin} toolchain info 的输出不是 JSON（${errMessage(err)}）`, version: '', bin };
    }
  }

  /**
   * ensure 准备 tools（按顺序；已装好的直接复用）：返回目录和环境变量，kiln-runner 写在 stderr 中的进度（每个工具是命中
   * 还是下载安装、用时）逐行交给 print。失败时抛出，说明中带 kiln-runner 的输出。signal 结束时终止安装（kiln-runner 清理
   * 自己的临时文件，半成品不会被当成装好）。
   */
  async ensure(tools: readonly ToolSpec[], o: { signal?: AbortSignal; print?: (line: string) => void } = {}): Promise<ToolchainRecord> {
    while (this.collecting) await this.collecting.catch(() => undefined);
    // 上面的检查与这里之间没有 await：之后开始的 gc 一定看得到这次 ensure
    this.ensuring++;
    try {
      const { bin, reason } = this.locate();
      if (!bin) throw new Error(reason);
      const input = JSON.stringify({ tools: tools.map((t) => ({ name: t.name, version: t.version, platform: t.platform, digest: t.digest })) });
      const r = await this.exec(bin, ['toolchain', 'ensure', '--root', this.root], { input, signal: o.signal, timeoutMs: ensureTimeoutMs });
      for (const line of r.stderr.split('\n')) if (line.trim() !== '' && r.code === 0) o.print?.(line.trim());
      if (r.code !== 0) throw new ExecError(bin, ['toolchain', 'ensure'], r);
      return parseEnsure(r.stdout, tools);
    } finally {
      this.ensuring--;
    }
  }

  /**
   * gc 回收 root：30 天没有用到的、总量超过上限时最久没用的、超过 12 小时的安装残留（规则在 Go 的 toolchain.GC 中）；
   * keep 中的 digest（进行中的任务用到的）不删。有 ensure 在进行时跳过这一轮（返回 undefined），找不到 kiln-runner 时
   * 也跳过。
   */
  async gc(keep: Iterable<string>): Promise<GCSummary | undefined> {
    if (this.ensuring > 0 || this.collecting) return undefined;
    const { bin } = this.locate();
    if (!bin) return undefined;
    const args = ['toolchain', 'gc', '--root', this.root];
    for (const d of new Set(keep)) args.push('--keep', d);
    const run = this.exec(bin, args, { timeoutMs: gcTimeoutMs });
    this.collecting = run;
    let r;
    try {
      r = await run;
    } finally {
      this.collecting = undefined;
    }
    let out: unknown;
    try {
      out = JSON.parse(r.stdout);
    } catch {
      throw new ExecError(bin, args, r);
    }
    if (!isObject(out) || !Array.isArray(out.removed) || !Array.isArray(out.failures)) throw new ExecError(bin, args, r);
    return {
      removed: out.removed.length,
      removedBytes: typeof out.removed_bytes === 'number' ? out.removed_bytes : 0,
      failures: out.failures.map((f) => (isObject(f) ? `${String(f.path)}：${String(f.error)}` : String(f))),
    };
  }
}
