import { createHash, randomBytes, randomUUID } from 'node:crypto';
import { constants as fsConstants } from 'node:fs';
import { access, chmod, link, lstat, mkdir, readFile, readdir, rename, rm, stat, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { errMessage } from './errors.js';
import type { Logger } from './log.js';

// kiln-task skill 的投放：连接器在启动 Agent 之前，把随包分发的 skill 放到 Agent 会加载的目录中。
// 命令的用法、退出码和输出格式只在 skill 中定义一次（Server 的 prompt 只保留任务本身的规则并指向它）。
//
// 目标目录按 Kiln 的 Agent 名解析：Codex 是 ~/.agents/skills/kiln-task，Claude Code 是
// $CLAUDE_CONFIG_DIR/skills/kiln-task（没有设置时 ~/.claude/skills/kiln-task）。
//
// 只覆盖自己安装的目录：目录中有 .kiln-orca-skill.json（managedBy=kiln-orca、skill=kiln-task）才是我们的。
// 用户自建的同名目录、符号链接、普通文件一律拒绝覆盖，返回清楚的冲突说明（kiln-orca doctor 也这样检查）。
//
// 安装过程：跨进程锁（<skillsRoot>/.kiln-task.lock）包住一次同步。锁的协议与 host/lock.ts 同构：内容是一行 JSON
// {"pid":…,"token":…}，token 每次获取时随机生成，所以一份内容只属于一次获取。获取时先把完整内容写进自己的临时文件，
// 再用 POSIX link 变成锁文件（目标已存在时精确得到 EEXIST）：锁文件一出现就是完整的，不存在"已创建、还没写入"的中间状态；
// 读到的内容不合法（空、不是这个格式）一定不是本协议写的，报错让人处理，绝不当作失效删除。
//
// 接管：锁记录的进程已经不在时，先用同样的 link 取得这份内容专属的接管锁 <锁文件>.takeover-<token>，持有接管锁后重新
// 读锁文件，内容仍是那份失效的内容才删除，然后回到获取（与新来的进程按 link 竞争，只有一个成功）。只有接管锁的持有者会
// 删除失效的锁文件，而失效的内容不会再变回来（token 唯一），所以不会删掉别人刚拿到的锁；多个进程同时接管同一份失效内容
// 时只有一个取得接管锁。接管锁的持有者本身崩溃时，它的接管锁按同样的规则（递归）接管，递归深度有上限。
// 绝不按时间抢占活进程的锁：锁只在持有进程已经不在（!processAlive）时才算失效；活进程持锁超过等待时间时只是超时失败。
// 释放时先确认锁的内容仍是自己的那一份再删；因为活进程的锁永远不会被别人回收，正常路径不会和接管者互相删除。
//
// 首次安装先在同父目录的 staging 目录写齐全部文件和元数据，再原子 rename 到目标：中途失败不会留下"没有元数据的半安装目录"
// 让节点永久 standby。更新已有目录时按清单在目录内单文件原子写（先 references/commands.md、再 SKILL.md，清理旧文件，
// 最后写 .kiln-orca-skill.json），中断后仍有合法的元数据可自愈重试。
// 已经是最新内容时不做任何写操作；同机并存更高协议的连接器（protocol 更大）时不降级覆盖，但保留前先确认它确实完整。

/** skillName 是随连接器分发的 skill 名（Agent 加载时用的名字）。 */
export const skillName = 'kiln-task';
/** skillMetaFile 记录目标目录由 kiln-orca 管理、安装的版本和文件摘要（所有权凭据）。 */
export const skillMetaFile = '.kiln-orca-skill.json';
/** skillEntryFile 是 Agent 加载的入口文件，{{KILN_SKILL}} 指向它。 */
const skillEntryFile = 'SKILL.md';
/** skillCommandsFile 是全部公开子命令的详细契约（SKILL.md 指向它）。 */
const skillCommandsFile = 'references/commands.md';
/** managedBy 与 skill 一起确认一个目录是本连接器安装的。 */
const managedBy = 'kiln-orca';

const lockFile = '.kiln-task.lock';
/** 默认的锁等待：超过时说明另一个连接器长时间占着锁，放弃这次同步（下一次检测状态时重试）。 */
const defaultLockWaitMs = 10_000;
const defaultLockRetryMs = 25;
/** 接管锁本身也被遗留时递归接管的上限：防止损坏的文件系统把调用栈耗尽（超限后按退避重试，不是放弃）。 */
const maxTakeoverDepth = 8;
/** 锁文件（及其临时文件）的权限：只有属主可读写；临时文件名按 pid 与 token 唯一，不会与别人的冲突。 */
const lockFileMode = 0o600;
const fileMode = 0o644;
const dirMode = 0o755;
/** sha256Hex 是 64 位十六进制的摘要（元数据中的 sha256 与 digest）。 */
const sha256Hex = /^[0-9a-f]{64}$/;

/**
 * bundledSkillDir 返回随包分发的 skill 目录（发布包中的 skills/kiln-task）。按 import.meta.url 解析：编译后是
 * dist/core/skill.js，源码中测试时是 src/core/skill.ts，两种布局的上一级都是包的根目录。
 */
export function bundledSkillDir(): string {
  return path.join(path.dirname(fileURLToPath(import.meta.url)), '..', '..', 'skills', skillName);
}

/** claudeConfigDir 返回 Claude Code 的配置目录：设置了 CLAUDE_CONFIG_DIR 时按它（解析为绝对路径），否则 ~/.claude。 */
export function claudeConfigDir(env: Record<string, string | undefined>, homeDir: string): string {
  const configured = env.CLAUDE_CONFIG_DIR?.trim();
  return configured ? path.resolve(configured) : path.join(homeDir, '.claude');
}

/** claudeConfigDirEnv 只用于默认值：没有环境时按 ~/.claude。 */
const noEnv: Record<string, string | undefined> = {};

/** skillDirFor 返回 Kiln 的 Agent 对应的 skill 目录；不认识的 Agent 抛出（不知道放在哪里）。 */
export function skillDirFor(agentName: string, homeDir: string, env: Record<string, string | undefined> = noEnv): string {
  switch (agentName) {
    case 'codex':
      return path.join(homeDir, '.agents', 'skills', skillName);
    case 'claude-code':
      return path.join(claudeConfigDir(env, homeDir), 'skills', skillName);
    default:
      throw new Error(`未知的 Agent ${agentName}：不知道把 ${skillName} skill 放到哪里`);
  }
}

/**
 * isSafeRelPath 判断 rel 是可以安全地拼在托管目录下的相对路径：非空、不含 NUL 与反斜杠、不是绝对路径、不以 / 结尾、
 * 没有空段，也没有 . 或 .. 段，并且不是元数据文件本身（清单不能声称托管它）。
 */
export function isSafeRelPath(rel: string): boolean {
  if (rel === '' || rel.includes('\0') || rel.includes('\\')) return false;
  if (rel.startsWith('/') || rel.endsWith('/')) return false;
  if (rel === skillMetaFile) return false;
  return rel.split('/').every((part) => part !== '' && part !== '.' && part !== '..');
}

/** safeUnder 在 isSafeRelPath 之外再按 target 解析一次，确认结果仍在 target 之内（防御平台差异）。 */
function safeUnder(target: string, rel: string): boolean {
  if (!isSafeRelPath(rel)) return false;
  const resolved = path.relative(target, path.resolve(target, rel));
  return resolved !== '' && !path.isAbsolute(resolved) && !resolved.startsWith('..') && resolved === rel;
}

/** SkillProvisioner 是 Attempt 与 Node 用到的 skill 投放接口（测试注入替身）。 */
export interface SkillProvisioner {
  /** ensureForAgent 把 skill 同步到 agentName 的目录，返回 SKILL.md 的路径（{{KILN_SKILL}}）。 */
  ensureForAgent(agentName: string): Promise<string>;
  /** ensureAll 同步多个 Agent 的 skill，返回失败说明（为空表示全部成功）；不抛出。 */
  ensureAll(agentNames: readonly string[]): Promise<string>;
}

/** SkillCheckItem 是 doctor 的一项检查结果（只读）。 */
export interface SkillCheckItem {
  /** agent 是 Kiln 的 Agent 名；内置 skill 本身有问题时是说明文字。 */
  agent: string;
  /** path 是检查的目录（或内置 skill 的目录）。 */
  path: string;
  ok: boolean;
  detail: string;
}

/** SkillSyncResult 是一次同步的结果。 */
export interface SkillSyncResult {
  /** skillPath 是 Agent 加载的 SKILL.md 路径。 */
  skillPath: string;
  /** changed 表示这次写了文件（false 表示已经是最新内容，或者保留了更高的版本）。 */
  changed: boolean;
}

/** SkillDeps 是 SkillManager 的依赖（全部显式注入，没有包级可变状态）。 */
export interface SkillDeps {
  /** homeDir 是执行机器上的用户主目录（Codex 的 ~/.agents、Claude Code 的默认 ~/.claude 按它解析）。 */
  homeDir: string;
  /** env 用来读 CLAUDE_CONFIG_DIR（测试注入，不读进程环境）。 */
  env: Record<string, string | undefined>;
  /** sourceDir 是随包分发的 skill 目录。 */
  sourceDir: string;
  /** version 是连接器的版本（写进元数据，doctor 显示）。 */
  version: string;
  /** protocol 是当前连接器的 AI 协议版本：已安装的版本协议更高时不降级覆盖。 */
  protocol: number;
  log: Logger;
  /** now 默认 Date.now（测试注入时钟）。 */
  now?(): number;
  /** processAlive 默认用信号 0 判断进程是否还在（测试注入）。 */
  processAlive?(pid: number): boolean;
  /** canWriteDir 判断目录可写（默认 access W_OK|X_OK）：测试注入，root 容器中也能真实检查权限。 */
  canWriteDir?(dir: string): Promise<boolean>;
  /** writeFile 写文件（默认 fs.writeFile，带 mode）：测试注入，用来模拟首次安装写到一半失败。 */
  writeFile?(p: string, data: Buffer, mode: number): Promise<void>;
  /** 锁的等待与重试（默认 10s / 25ms，测试可以缩短）。 */
  lockWaitMs?: number;
  lockRetryMs?: number;
}

/** BundleFile 是内置 skill 的一个文件。 */
interface BundleFile {
  /** path 是相对 skill 目录的路径（用 / 分隔）。 */
  path: string;
  data: Buffer;
  sha256: string;
}

/** Bundle 是内置 skill 的清单与摘要（digest 覆盖所有文件的路径和摘要）。 */
interface Bundle {
  files: BundleFile[];
  digest: string;
}

/** SkillMeta 是 .kiln-orca-skill.json 的内容。 */
interface SkillMeta {
  managedBy: string;
  skill: string;
  version: string;
  protocol: number;
  digest: string;
  files: { path: string; sha256: string }[];
}

/** MetaRead 是读取元数据的结果：不存在、冲突（符号链接、非普通文件、内容非法）或合法。 */
type MetaRead = { kind: 'missing' } | { kind: 'conflict'; reason: string } | { kind: 'ok'; meta: SkillMeta };

/** hash 返回 data 的 sha256（十六进制）。 */
function hash(data: Buffer | string): string {
  return createHash('sha256').update(data).digest('hex');
}

/** isObj 判断 v 是普通的 JSON 对象。 */
function isObj(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

/** listFiles 递归列出 dir 中的普通文件（相对路径，/ 分隔，按名字排序）；符号链接和其它特殊文件不算。 */
async function listFiles(dir: string, prefix: string): Promise<string[]> {
  const entries = await readdir(path.join(dir, prefix), { withFileTypes: true });
  entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  const out: string[] = [];
  for (const e of entries) {
    const rel = prefix === '' ? e.name : `${prefix}/${e.name}`;
    if (e.isDirectory()) out.push(...(await listFiles(dir, rel)));
    else if (e.isFile()) out.push(rel);
  }
  return out;
}

/** readBundle 读取内置 skill 的清单；缺少入口或命令说明时抛出（随包的文件不完整）。 */
async function readBundle(dir: string): Promise<Bundle> {
  const files: BundleFile[] = [];
  for (const rel of await listFiles(dir, '')) {
    const data = await readFile(path.join(dir, rel));
    files.push({ path: rel, data, sha256: hash(data) });
  }
  const have = new Set(files.map((f) => f.path));
  for (const need of [skillEntryFile, skillCommandsFile]) {
    if (!have.has(need)) throw new Error(`内置的 ${skillName} skill 不完整：${path.join(dir, need)} 不存在`);
  }
  const digest = hash(files.map((f) => `${f.path}\0${f.sha256}`).join('\n'));
  return { files, digest };
}

/** lstatOrUndefined 返回 lstat（不跟随符号链接）；不存在时为 undefined（祖先不是目录时同样算不存在：那个祖先本身会被检查）。 */
async function lstatOrUndefined(p: string): Promise<Awaited<ReturnType<typeof lstat>> | undefined> {
  try {
    return await lstat(p);
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    if (code === 'ENOENT' || code === 'ENOTDIR') return undefined;
    throw err;
  }
}

/** Writer 写一个文件（带权限）：默认用 fs.writeFile，测试可以注入失败。 */
type Writer = (p: string, data: Buffer, mode: number) => Promise<void>;

/** defaultWrite 用 fs.writeFile 写文件并设置权限。 */
const defaultWrite: Writer = (p, data, mode) => writeFile(p, data, { mode });

/** writeAtomic 在同一目录写临时文件再 rename 覆盖：不会留下半写的文件，也不会出现空窗。 */
async function writeAtomic(p: string, data: Buffer, write: Writer = defaultWrite): Promise<void> {
  await mkdir(path.dirname(p), { recursive: true, mode: dirMode });
  const tmp = `${p}.${randomUUID()}.tmp`;
  try {
    await write(tmp, data, fileMode);
    await chmod(tmp, fileMode);
    await rename(tmp, p);
  } catch (err) {
    await rm(tmp, { force: true }).catch(() => undefined);
    throw err;
  }
}

/** writeOrder 返回写文件的顺序：SKILL.md 最后写（先写它引用的 references/commands.md）。 */
function writeOrder(files: readonly BundleFile[]): BundleFile[] {
  const rank = (f: BundleFile) => (f.path === skillEntryFile ? 1 : 0);
  return [...files].sort((a, b) => rank(a) - rank(b) || (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
}

/** refuse 是拒绝覆盖时的错误（路径与原因）。 */
function refuse(p: string, why: string): Error {
  return new Error(`${p} ${why}：kiln-orca 不会覆盖不是它管理的文件，移开或改名后重试`);
}

/** metaFor 按内置清单构造要写入的元数据。 */
function metaFor(version: string, protocol: number, bundle: Bundle): SkillMeta {
  return {
    managedBy,
    skill: skillName,
    version,
    protocol,
    digest: bundle.digest,
    files: bundle.files.map((f) => ({ path: f.path, sha256: f.sha256 })),
  };
}

/** metaBytes 是元数据文件的字节内容。 */
function metaBytes(meta: SkillMeta): Buffer {
  return Buffer.from(JSON.stringify(meta, null, 2) + '\n');
}

/**
 * readMeta 读取所有权元数据，并严格校验它声明的路径与摘要：文件不存在是 missing；符号链接、非普通文件、内容不是
 * kiln-orca 的 kiln-task、协议不是正整数、清单路径不安全或不唯一、sha256 不是 64 位十六进制、清单中没有 SKILL.md
 * 都是 conflict。真实的 I/O 错误（例如 EACCES）直接抛出，由调用方转成清楚的失败说明。
 */
async function readMeta(dir: string): Promise<MetaRead> {
  const p = path.join(dir, skillMetaFile);
  let st: Awaited<ReturnType<typeof lstat>>;
  try {
    st = await lstat(p);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return { kind: 'missing' };
    throw err;
  }
  if (st.isSymbolicLink()) return { kind: 'conflict', reason: `${p} 是符号链接` };
  if (!st.isFile()) return { kind: 'conflict', reason: `${p} 不是普通文件` };
  let text: string;
  try {
    text = await readFile(p, 'utf8');
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return { kind: 'missing' };
    throw err;
  }
  const bad = (reason: string): MetaRead => ({ kind: 'conflict', reason: `无效的 ${skillMetaFile}（${reason}）` });
  let v: unknown;
  try {
    v = JSON.parse(text);
  } catch {
    return bad('不是 JSON');
  }
  if (!isObj(v) || v.managedBy !== managedBy || v.skill !== skillName) return bad(`不是 kiln-orca 的 ${skillName}`);
  const { version, protocol, digest, files } = v;
  if (typeof version !== 'string' || typeof protocol !== 'number' || typeof digest !== 'string' || !Array.isArray(files)) {
    return bad('缺少 version / protocol / digest / files');
  }
  // 协议必须是正整数：拒绝 Infinity、NaN、负数、0 和小数（否则会把不认识的版本当成"更高协议"保留或降级覆盖）
  if (!Number.isSafeInteger(protocol) || protocol <= 0) return bad(`protocol 不是正整数：${String(protocol)}`);
  if (!sha256Hex.test(digest)) return bad('digest 不是 64 位十六进制');
  const list: { path: string; sha256: string }[] = [];
  const seen = new Set<string>();
  for (const f of files) {
    if (!isObj(f) || typeof f.path !== 'string' || typeof f.sha256 !== 'string') return bad('清单项缺少 path / sha256');
    if (!safeUnder(dir, f.path)) return bad(`清单路径不安全：${JSON.stringify(f.path)}`);
    if (seen.has(f.path)) return bad(`清单路径重复：${f.path}`);
    if (!sha256Hex.test(f.sha256)) return bad(`${f.path} 的 sha256 不是 64 位十六进制`);
    seen.add(f.path);
    list.push({ path: f.path, sha256: f.sha256 });
  }
  if (!seen.has(skillEntryFile)) return bad(`清单中没有 ${skillEntryFile}`);
  return { kind: 'ok', meta: { managedBy, skill: skillName, version, protocol, digest, files: list } };
}

/** checkOwnership 确认 target 是我们安装的目录（不存在时返回 undefined）；冲突时抛出。 */
async function checkOwnership(target: string): Promise<SkillMeta | undefined> {
  const st = await lstatOrUndefined(target);
  if (!st) return undefined;
  if (st.isSymbolicLink()) throw refuse(target, '是一个符号链接');
  if (!st.isDirectory()) throw refuse(target, '不是目录');
  const r = await readMeta(target);
  switch (r.kind) {
    case 'missing':
      throw refuse(target, `不是 kiln-orca 安装的 ${skillName} skill（缺少 ${skillMetaFile}）`);
    case 'conflict':
      throw refuse(target, `不是 kiln-orca 安装的 ${skillName} skill（${r.reason}）`);
    case 'ok':
      return r.meta;
  }
}

/**
 * ensureNoSymlinks 检查 target 下这些相对路径的每一级：已有的符号链接、该是文件却是目录（或反过来）都拒绝。
 * 用 lstat，不跟随链接。调用方只传入通过安全路径校验的路径。
 */
async function ensureNoSymlinks(target: string, relPaths: readonly string[]): Promise<void> {
  for (const rel of relPaths) {
    const parts = rel.split('/');
    let cur = target;
    for (let i = 0; i < parts.length; i++) {
      cur = path.join(cur, parts[i]!);
      const st = await lstatOrUndefined(cur);
      if (!st) break;
      if (st.isSymbolicLink()) throw refuse(cur, '是一个符号链接');
      const last = i === parts.length - 1;
      if (last && !st.isFile()) throw refuse(cur, '不是普通文件');
      if (!last && !st.isDirectory()) throw refuse(cur, '不是目录');
    }
  }
}

/**
 * verifyInstalled 确认元数据声明的安装确实完整：清单包含 SKILL.md，每一项都是普通文件、不是符号链接，内容与 sha256
 * 一致。损坏（被删、被换成链接、被改写）时抛出——保留更高协议的目录之前、doctor 报告"已安装"之前都要先过这一关：
 * sync 与 check 的保留分支都只调用它，顺序与结论完全一致。
 */
async function verifyInstalled(target: string, meta: SkillMeta): Promise<void> {
  if (!meta.files.some((f) => f.path === skillEntryFile)) {
    throw refuse(path.join(target, skillEntryFile), `不在 ${skillMetaFile} 的安装清单中（安装已损坏）`);
  }
  await ensureNoSymlinks(target, [skillMetaFile, ...meta.files.map((f) => f.path)]);
  for (const f of meta.files) {
    const p = path.join(target, f.path);
    const st = await lstatOrUndefined(p);
    if (!st) throw refuse(p, '不存在（安装已损坏）');
    if (!st.isFile()) throw refuse(p, '不是普通文件（安装已损坏）');
    if (hash(await readFile(p)) !== f.sha256) throw refuse(p, '内容与安装清单不一致（安装已损坏）');
  }
}

/**
 * upToDate 判断目录中的内容与内置 skill 完全一致：摘要、协议、连接器版本相同，清单路径集合完全一致（没有多余的陈旧
 * 条目），每个文件都是普通文件且内容摘要相同。
 */
async function upToDate(target: string, meta: SkillMeta, bundle: Bundle, version: string, protocol: number): Promise<boolean> {
  if (meta.digest !== bundle.digest) return false;
  if (meta.protocol !== protocol || meta.version !== version) return false;
  const want = new Set(bundle.files.map((f) => f.path));
  if (meta.files.length !== want.size || meta.files.some((f) => !want.has(f.path))) return false;
  for (const f of bundle.files) {
    const p = path.join(target, f.path);
    const st = await lstatOrUndefined(p);
    if (!st?.isFile()) return false;
    if (hash(await readFile(p)) !== f.sha256) return false;
  }
  return true;
}

/** pidAlive 判断进程是否还在（EPERM 表示进程存在，只是不属于当前用户）。 */
function pidAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return (err as NodeJS.ErrnoException).code === 'EPERM';
  }
}

/** canWriteDirDefault 用 access 判断目录可写（需要写和执行权限）。 */
async function canWriteDirDefault(dir: string): Promise<boolean> {
  try {
    await access(dir, fsConstants.W_OK | fsConstants.X_OK);
    return true;
  } catch {
    return false;
  }
}

/** LockSettings 是一次加锁用到的参数（导出供测试直接验证锁的属主校验与失效接管）。 */
export interface LockSettings {
  now(): number;
  processAlive(pid: number): boolean;
  waitMs: number;
  retryMs: number;
}

/** LockOwner 是锁文件的内容：一个进程一次获取（token 唯一）。 */
interface LockOwner {
  pid: number;
  token: string;
}

/** sleepMs 让出事件循环一小段时间（锁重试之间的退避）。 */
function sleepMs(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** newLockOwner 生成这次获取的属主：自己的 pid 与随机的 token。 */
function newLockOwner(): LockOwner {
  return { pid: process.pid, token: randomBytes(12).toString('hex') };
}

/** formatLock 是锁文件的完整内容：一行 JSON（token 唯一，所以一份内容只属于一次获取）。 */
function formatLock(o: LockOwner): string {
  return JSON.stringify(o) + '\n';
}

/** lockTokenPattern 限制锁 token 的字符集：不带路径分隔符与点号，拼接接管锁名时不会逃出锁目录。 */
const lockTokenPattern = /^[A-Za-z0-9_-]+$/;

/**
 * parseLock 严格校验锁文件的内容：pid 必须是正整数（0、-1 不可能是一个进程，process.kill(0) 还会命中整个进程组），
 * token 必须是非空且只含安全字符的字符串。内容不合法一定不是本协议写的（经 link 原子创建，不会有半截内容），抛出让人处理，
 * 绝不当作失效删除——删除可能是别人正在写入的锁。
 */
function parseLock(p: string, text: string): LockOwner {
  try {
    const v = JSON.parse(text) as unknown;
    if (isObj(v) && Number.isInteger(v.pid) && Number(v.pid) > 0 && typeof v.token === 'string' && lockTokenPattern.test(v.token)) {
      return { pid: Number(v.pid), token: v.token };
    }
  } catch {
    // 下面报错
  }
  throw new Error(`锁文件 ${p} 的内容无效（${JSON.stringify(text.slice(0, 80))}）：确认没有连接器在同步 ${skillName} skill 后删除它`);
}

/** readLock 返回锁文件的内容，不存在时返回 undefined。 */
async function readLock(p: string): Promise<string | undefined> {
  try {
    return await readFile(p, 'utf8');
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return undefined;
    throw err;
  }
}

/**
 * tryCreateLock 原子地创建内容为 o 的 p：先把完整内容写进自己的临时文件，再 link 到 p（p 已存在时精确得到 EEXIST）。
 * 锁文件一出现就是完整的；临时文件总在 finally 中删掉。
 */
async function tryCreateLock(p: string, o: LockOwner): Promise<boolean> {
  const tmp = `${p}.${o.pid}.${o.token}.tmp`;
  try {
    await writeFile(tmp, formatLock(o), { mode: lockFileMode, flag: 'wx' });
  } catch (err) {
    await rm(tmp, { force: true }).catch(() => undefined);
    throw err;
  }
  try {
    await link(tmp, p);
    return true;
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'EEXIST') return false;
    throw err;
  } finally {
    await rm(tmp, { force: true }).catch(() => undefined);
  }
}

/**
 * removeStaleLock 删除失效的锁 p（内容仍是 staleText）：先取得这份内容专属的接管锁 <p>.takeover-<token>，取得后
 * 重新读 p，内容没变才删除。只有接管锁的持有者会删除，所以不会删掉别人刚拿到的锁；多个进程同时接管同一份失效内容时
 * 只有一个取得接管锁，其余返回 false 退避重试。接管锁本身失效（持有者崩溃）时递归接管，深度以 maxTakeoverDepth 为限；
 * 接管锁被活着的进程持有时返回 false（不抢占）。
 */
async function removeStaleLock(p: string, staleText: string, staleOwner: LockOwner, s: LockSettings, depth = 0): Promise<boolean> {
  if (depth >= maxTakeoverDepth) return false;
  const guard = `${p}.takeover-${staleOwner.token}`;
  const me = newLockOwner();
  const mine = formatLock(me);
  if (await tryCreateLock(guard, me)) {
    try {
      if ((await readLock(p)) === staleText) await rm(p, { force: true });
    } finally {
      // 确认自己仍持有接管锁才删除（双保险）
      if ((await readLock(guard)) === mine) await rm(guard, { force: true });
    }
    return true;
  }
  const text = await readLock(guard);
  if (text === undefined) return false;
  const owner = parseLock(guard, text);
  if (s.processAlive(owner.pid)) return false;
  return removeStaleLock(guard, text, owner, s, depth + 1);
}

/**
 * acquireLock 取得跨进程锁（原子创建 <skillsRoot>/.kiln-task.lock）。遇 EEXIST 时短退避重试；只有持有进程已经不在
 * （!processAlive）时才接管它的锁，绝不让活着但长时间持锁的连接器被抢；内容不合法时抛出。超过 waitMs 时抛出。
 * 返回的函数释放自己持有的锁（删之前确认内容仍是自己的那一份）。
 */
export async function acquireLock(p: string, s: LockSettings): Promise<() => Promise<void>> {
  const me = newLockOwner();
  const mine = formatLock(me);
  const until = s.now() + s.waitMs;
  for (;;) {
    if (await tryCreateLock(p, me)) {
      let released = false;
      return async () => {
        if (released) return;
        released = true;
        if ((await readLock(p)) === mine) await rm(p, { force: true });
      };
    }
    if (s.now() >= until) throw new Error(`等待 ${p} 超过 ${s.waitMs} 毫秒：另一个连接器正在安装 ${skillName} skill`);
    const text = await readLock(p);
    if (text === undefined) continue;
    const o = parseLock(p, text);
    if (s.processAlive(o.pid) || !(await removeStaleLock(p, text, o, s))) await sleepMs(s.retryMs);
  }
}

/** nearestExisting 向上找到最近一个已经存在的祖先，返回它是不是目录、可不可写（只读，不创建目录）。 */
async function nearestExisting(
  dir: string,
  canWrite: (dir: string) => Promise<boolean>,
): Promise<{ exists: boolean; path: string; dir: boolean; writable: boolean }> {
  let cur = path.resolve(dir);
  for (;;) {
    const st = await lstatOrUndefined(cur);
    if (st) {
      // 祖先可以是符号链接（例如主目录、~/.claude 指向别处）：按它指向的目录判断
      const real = st.isSymbolicLink() ? await stat(cur).catch(() => undefined) : st;
      if (!real?.isDirectory()) return { exists: true, path: cur, dir: false, writable: false };
      return { exists: true, path: cur, dir: true, writable: await canWrite(cur) };
    }
    const parent = path.dirname(cur);
    if (parent === cur) return { exists: false, path: cur, dir: false, writable: false };
    cur = parent;
  }
}

/**
 * SkillManager 把内置的 kiln-task skill 安装到 Agent 的目录（实现 SkillProvisioner）。构造时注入全部外部依赖；
 * 同一个实例可以被多个 attempt 并发调用，跨进程锁保证同一时刻只有一个安装者。
 */
export class SkillManager implements SkillProvisioner {
  private readonly now: () => number;
  private readonly alive: (pid: number) => boolean;
  private readonly canWrite: (dir: string) => Promise<boolean>;
  private readonly write: Writer;

  constructor(private readonly d: SkillDeps) {
    this.now = d.now ?? Date.now;
    this.alive = d.processAlive ?? pidAlive;
    this.canWrite = d.canWriteDir ?? canWriteDirDefault;
    this.write = d.writeFile ?? defaultWrite;
  }

  async ensureForAgent(agentName: string): Promise<string> {
    return (await this.sync(agentName)).skillPath;
  }

  async ensureAll(agentNames: readonly string[]): Promise<string> {
    const problems: string[] = [];
    for (const name of agentNames) {
      try {
        await this.sync(name);
      } catch (err) {
        problems.push(`${name}：${errMessage(err)}`);
      }
    }
    return problems.join('；');
  }

  /**
   * check 严格只读地检查每个 Agent 的 skill（doctor 用）：不创建目录、不写文件。尚未安装但最近的祖先目录可写、或者
   * 已安装且能完成同步（包括锁文件目录、清单涉及的目录可写）都算 ok；冲突（用户自建目录、符号链接、普通文件、
   * 安装已损坏、需要同步但不可写）是失败。任何 I/O 异常都转成失败项，不抛出（doctor 不能因此崩溃）。
   */
  async check(agentNames: readonly string[]): Promise<SkillCheckItem[]> {
    let bundle: Bundle;
    try {
      bundle = await readBundle(this.d.sourceDir);
    } catch (err) {
      return [{ agent: '（内置）', path: this.d.sourceDir, ok: false, detail: `随包分发的 ${skillName} skill 不完整：${errMessage(err)}` }];
    }
    const items: SkillCheckItem[] = [];
    for (const name of agentNames) items.push(await this.checkAgent(name, bundle));
    return items;
  }

  private lockSettings(): LockSettings {
    return {
      now: this.now,
      processAlive: this.alive,
      waitMs: this.d.lockWaitMs ?? defaultLockWaitMs,
      retryMs: this.d.lockRetryMs ?? defaultLockRetryMs,
    };
  }

  /** sync 同步一个 Agent 的 skill（ensureForAgent 的实现；测试也直接用它看 changed）。 */
  async sync(agentName: string): Promise<SkillSyncResult> {
    const bundle = await readBundle(this.d.sourceDir);
    const target = skillDirFor(agentName, this.d.homeDir, this.d.env);
    const skillsRoot = path.dirname(target);
    await mkdir(skillsRoot, { recursive: true, mode: dirMode });
    const release = await acquireLock(path.join(skillsRoot, lockFile), this.lockSettings());
    try {
      const meta = await checkOwnership(target);
      const skillPath = path.join(target, skillEntryFile);
      if (meta) {
        // 同机并存更高协议的连接器：保留它的版本，但先确认它确实完整（文件都在、内容匹配、没有符号链接）
        if (meta.protocol > this.d.protocol) {
          await verifyInstalled(target, meta);
          this.d.log.warn(`${skillName} skill 已安装的版本比这个连接器新（协议 ${meta.protocol} > ${this.d.protocol}），保留它的版本、不降级覆盖`, {
            agent: agentName,
            path: target,
            version: meta.version,
          });
          return { skillPath, changed: false };
        }
        await ensureNoSymlinks(target, managedPaths(meta, bundle));
        if (await upToDate(target, meta, bundle, this.d.version, this.d.protocol)) return { skillPath, changed: false };
        // 更新已有目录：目录内单文件原子写，最后写元数据（中断后仍有合法的元数据可自愈重试）
        for (const f of writeOrder(bundle.files)) await writeAtomic(path.join(target, f.path), f.data, this.write);
        await removeStale(target, meta, bundle);
        await writeAtomic(path.join(target, skillMetaFile), metaBytes(metaFor(this.d.version, this.d.protocol, bundle)), this.write);
        this.d.log.info(`已安装 ${skillName} skill`, { agent: agentName, path: target, version: this.d.version, protocol: this.d.protocol });
        return { skillPath, changed: true };
      }
      await this.installFresh(target, skillsRoot, bundle);
      this.d.log.info(`已安装 ${skillName} skill`, { agent: agentName, path: target, version: this.d.version, protocol: this.d.protocol });
      return { skillPath, changed: true };
    } finally {
      await release();
    }
  }

  /**
   * installFresh 首次安装：在同父目录的 staging 目录写齐全部文件和元数据，最后原子 rename 到 target。中途任何失败都
   * 清理 staging，target 绝不会以"没有元数据的半安装目录"留在磁盘上（否则节点会永久 standby）。
   */
  private async installFresh(target: string, skillsRoot: string, bundle: Bundle): Promise<void> {
    const staging = path.join(skillsRoot, `.${skillName}.stage-${randomUUID()}`);
    try {
      await mkdir(staging, { recursive: true, mode: dirMode });
      for (const f of writeOrder(bundle.files)) await writeAtomic(path.join(staging, f.path), f.data, this.write);
      await writeAtomic(path.join(staging, skillMetaFile), metaBytes(metaFor(this.d.version, this.d.protocol, bundle)), this.write);
      await rename(staging, target);
    } catch (err) {
      await rm(staging, { recursive: true, force: true }).catch(() => undefined);
      throw err;
    }
  }

  /** checkAgent 检查一个 Agent；任何异常都转成失败项（doctor 不崩溃）。 */
  private async checkAgent(agentName: string, bundle: Bundle): Promise<SkillCheckItem> {
    let target: string;
    try {
      target = skillDirFor(agentName, this.d.homeDir, this.d.env);
    } catch (err) {
      return { agent: agentName, path: '', ok: false, detail: errMessage(err) };
    }
    try {
      return await this.checkInstall(agentName, target, bundle);
    } catch (err) {
      return { agent: agentName, path: target, ok: false, detail: `检查 ${skillName} skill 失败：${errMessage(err)}` };
    }
  }

  private async checkInstall(agentName: string, target: string, bundle: Bundle): Promise<SkillCheckItem> {
    const st = await lstatOrUndefined(target);
    if (!st) {
      const parent = await nearestExisting(path.dirname(target), this.canWrite);
      if (!parent.exists) return { agent: agentName, path: target, ok: false, detail: `找不到可以放置 skill 的目录（${path.dirname(target)} 不存在）` };
      if (!parent.dir) return { agent: agentName, path: target, ok: false, detail: `${parent.path} 不是目录，无法放置 skill` };
      if (!parent.writable) return { agent: agentName, path: target, ok: false, detail: `${parent.path} 不可写（权限不足），无法放置 skill` };
      return { agent: agentName, path: target, ok: true, detail: `尚未安装，启动连接器时会自动安装到 ${target}` };
    }
    if (st.isSymbolicLink()) return { agent: agentName, path: target, ok: false, detail: `${target} 是符号链接：kiln-orca 不会覆盖它，移开或改名后重试` };
    if (!st.isDirectory()) return { agent: agentName, path: target, ok: false, detail: `${target} 不是目录：kiln-orca 不会覆盖它，移开或改名后重试` };
    const r = await readMeta(target);
    if (r.kind === 'missing') {
      return {
        agent: agentName,
        path: target,
        ok: false,
        detail: `${target} 已存在，但不是 kiln-orca 安装的 ${skillName} skill（缺少 ${skillMetaFile}）：移开或改名后重试，kiln-orca 不会覆盖它`,
      };
    }
    if (r.kind === 'conflict') {
      return {
        agent: agentName,
        path: target,
        ok: false,
        detail: `${target} 已存在，但不是 kiln-orca 安装的 ${skillName} skill（${r.reason}）：移开或改名后重试，kiln-orca 不会覆盖它`,
      };
    }
    const meta = r.meta;
    if (meta.protocol > this.d.protocol) {
      // 与 sync 的保留分支完全一致：先确认没有符号链接、安装完整（verifyInstalled 先查链接再核对摘要），再确认能建锁
      await verifyInstalled(target, meta);
      const blocked = await this.lockRootBlocker(target);
      if (blocked) return { agent: agentName, path: target, ok: false, detail: blocked };
      return { agent: agentName, path: target, ok: true, detail: `已安装更新的版本（${meta.version}，协议 ${meta.protocol}）：启动连接器时保留，不降级覆盖` };
    }
    await ensureNoSymlinks(target, managedPaths(meta, bundle));
    if (await upToDate(target, meta, bundle, this.d.version, this.d.protocol)) {
      const blocked = await this.lockRootBlocker(target);
      if (blocked) return { agent: agentName, path: target, ok: false, detail: blocked };
      return { agent: agentName, path: target, ok: true, detail: `已安装（${meta.version}，${target}）` };
    }
    // 需要更新：预检锁文件目录、目标目录与要写入或清理的已存在子目录的写权限，避免 doctor 报 ok 而 run 报失败
    const blocked = await this.updateBlocker(target, meta, bundle);
    if (blocked) return { agent: agentName, path: target, ok: false, detail: blocked };
    return { agent: agentName, path: target, ok: true, detail: `已安装版本 ${meta.version || '未知'}，启动连接器时会更新到 ${this.d.version}（${target}）` };
  }

  /**
   * lockRootBlocker 返回已有安装无法同步的原因：sync 每次都会在 skillsRoot（目标的父目录）创建锁文件
   * <skillsRoot>/.kiln-task.lock，目录不可写时更新与"已经是最新"都做不了。可写时返回 undefined。
   */
  private async lockRootBlocker(target: string): Promise<string | undefined> {
    const root = path.dirname(target);
    if (await this.canWrite(root)) return undefined;
    return `${root} 不可写（权限不足），无法在 ${root} 创建锁文件 ${lockFile}`;
  }

  /**
   * updateBlocker 返回更新已有安装前不能进行的原因：除了锁文件目录与目标目录，清单中要写入或清理的每个路径在
   * target 内最近的已存在祖先目录（例如已有的 <target>/references，或新增嵌套目录 assets/new/a.md 时的已有
   * <target>/assets）也必须可写；target 本身已经在前面检查过。
   */
  private async updateBlocker(target: string, meta: SkillMeta, bundle: Bundle): Promise<string | undefined> {
    const locked = await this.lockRootBlocker(target);
    if (locked) return locked;
    if (!(await this.canWrite(target))) return `${target} 不可写（权限不足），无法更新 skill`;
    const dirs = new Set<string>();
    for (const rel of [...bundle.files.map((f) => f.path), ...meta.files.map((f) => f.path)]) {
      let cur = path.join(target, path.dirname(rel));
      while (cur !== target) {
        if (await lstatOrUndefined(cur)) {
          dirs.add(cur);
          break;
        }
        const parent = path.dirname(cur);
        if (parent === cur) break;
        cur = parent;
      }
    }
    for (const dir of dirs) {
      if (!(await this.canWrite(dir))) return `${dir} 不可写（权限不足），无法更新 skill`;
    }
    return undefined;
  }
}

/** managedPaths 是这次要检查的托管路径：所有权凭据、已安装清单与内置清单的并集。 */
function managedPaths(meta: SkillMeta, bundle: Bundle): string[] {
  return [...new Set([skillMetaFile, ...meta.files.map((f) => f.path), ...bundle.files.map((f) => f.path)])];
}

/** removeStale 删除上一次安装清单中有、这次内置清单中已经没有的托管文件（只处理通过安全路径校验的路径）。 */
async function removeStale(target: string, meta: SkillMeta, bundle: Bundle): Promise<void> {
  const current = new Set(bundle.files.map((f) => f.path));
  for (const f of meta.files) {
    if (current.has(f.path) || !isSafeRelPath(f.path)) continue;
    await rm(path.join(target, f.path), { force: true });
  }
}
