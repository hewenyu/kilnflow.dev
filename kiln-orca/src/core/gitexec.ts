import { createHash, randomUUID } from 'node:crypto';
import { constants as fsConstants, existsSync } from 'node:fs';
import { mkdir, open, readdir, rename, rm, symlink, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { repoOfPath, safeRepoPath } from './credentials.js';
import { type Exec, ExecError, exitedNormally } from './exec.js';

// Agent 的 git 传输（PLAN #33.13）：kiln-orca task exec 把 GIT_EXEC_PATH 指向这个 attempt 的 git exec 镜像，git 发起
// HTTP(S) 传输时执行的远程助手（git-remote-http、git-remote-https 等）是传输包装（kiln-orca task git-remote）。包装先检查
// 认证冲突，再经本机 socket 向连接器开一个操作并保持这个连接，以操作 id 启动真实的远程助手，等它退出后关闭连接：
// 操作随连接关闭而结束（包装被杀时由内核关闭），连接器据此吊销这次传输的凭据。本地命令不启动远程助手，不产生操作。
//
// 镜像保留原目录的全部文件（逐个符号链接：子模块等要用到 git-sh-i18n 之类的脚本），只把远程助手换成包装。镜像按真实的
// exec 目录和包装的内容命名，已经存在时复用；建在临时目录中再改名，同时建的多个 task exec 互不影响。

/** remoteHelpers 是换成传输包装的远程助手（git 按地址的协议找 git-remote-<协议>）。 */
export const remoteHelpers: readonly string[] = ['git-remote-http', 'git-remote-https', 'git-remote-ftp', 'git-remote-ftps'];

/**
 * joinRaw 用 / 拼接 dir 和 rel，不做词法规整（path.join 会 normalize，在文件系统解析符号链接之前就消掉 ..）：用于要与另一个
 * 进程（git、libcurl）按同样的字节打开同一个文件的路径——NETRC、HOME、XDG_CONFIG_HOME、git --exec-path 的输出——由文件系统
 * 按原顺序解析符号链接、缺失的组件和 ..。dir 为空时原样返回 rel（相对本进程的当前目录）。与 CI Runner 的 joinRaw 相同。
 */
export function joinRaw(dir: string, rel: string): string {
  return dir ? `${dir}/${rel}` : rel;
}

/**
 * realExecPath 返回 git 自己的 exec 目录（不受已经设置的 GIT_EXEC_PATH 影响：可能是外层 task exec 的镜像）。只去掉 git 输出
 * 末尾的那一个换行（记录终止符），路径本身的字节原样保留（目录名可以以空格结尾）。
 */
export async function realExecPath(exec: Exec, env: Record<string, string | undefined>): Promise<string> {
  const args = ['--exec-path'];
  const r = await exec('git', args, { env: { ...env, GIT_EXEC_PATH: undefined }, timeoutMs: authQueryTimeoutMs });
  if (r.code !== 0 || !exitedNormally(r)) throw new ExecError('git', args, r);
  const out = r.stdout.endsWith('\n') ? r.stdout.slice(0, -1) : r.stdout;
  if (!path.isAbsolute(out)) throw new Error(`git --exec-path 给出的不是绝对路径：${out}`);
  return out;
}

/**
 * ensureExecMirror 在 root 下准备 real（git 的 exec 目录）的镜像，返回它的路径。wrapper(helper) 是替换 helper（真实远程助手的
 * 绝对路径）的脚本内容。
 */
export async function ensureExecMirror(root: string, real: string, wrapper: (helper: string) => string): Promise<string> {
  const key = createHash('sha256')
    .update(real)
    .update('\0')
    .update(wrapper(''))
    .digest('hex')
    .slice(0, 16);
  const dir = path.join(root, key);
  if (existsSync(dir)) return dir;
  await mkdir(root, { recursive: true, mode: 0o700 });
  const tmp = path.join(root, `.tmp-${randomUUID()}`);
  await mkdir(tmp, { mode: 0o700 });
  try {
    for (const name of await readdir(real)) {
      const target = joinRaw(real, name); // real 是 git --exec-path 的输出：原样拼接，与 git 自己找远程助手的路径相同
      if (remoteHelpers.includes(name)) await writeFile(path.join(tmp, name), wrapper(target), { mode: 0o755 });
      else await symlink(target, path.join(tmp, name));
    }
    await rename(tmp, dir);
  } catch (err) {
    await rm(tmp, { recursive: true, force: true });
    // 另一个 task exec 同时建好了同一个镜像（改名时目标已经存在）
    if (existsSync(dir)) return dir;
    throw err;
  }
  return dir;
}

/** AuthConflict 是一项认证冲突：键（不含值）和能确定的来源。 */
export interface AuthConflict {
  key: string;
  source: string;
}

/** AuthCheck 是检查认证冲突要用到的环境：git 和 curl 看到的环境变量、工作目录、平台、账户的主目录（HOME 未设置时）。 */
export interface AuthCheck {
  exec: Exec;
  env: Record<string, string | undefined>;
  cwd?: string;
  platform: NodeJS.Platform;
  /** accountHome 是账户的主目录（os.userInfo().homedir），HOME 未设置时 libcurl 用它找 netrc。 */
  accountHome?: string;
  /** timeoutMs 是一次 git config 查询的期限（默认 authQueryTimeoutMs；测试中缩短）。 */
  timeoutMs?: number;
}

/**
 * authQueryTimeoutMs 是认证冲突检查中一次 git config 查询的期限（例如配置文件在卡住的网络文件系统上）：超时按查询失败
 * 处理（不限范围的查询超时即冲突，拒绝这次传输），包装、doctor 和连接器启动都不会一直挂着。
 */
export const authQueryTimeoutMs = 30_000;

/** extraHeaderKey 是会给请求加认证头的 git 配置。 */
const extraHeaderKey = 'http.extraheader';

/**
 * authConflicts 检查一次 git 传输（url 是 insteadOf 改写之后的地址）在执行机器上是否会带上别的凭据（PLAN #33.13）：
 * 1. 地址中带用户名或密码；
 * 2. 生效的 http.extraheader：git config --get-urlmatch 退出码 0 且值非空是冲突，1 是没有，其他是查询失败。返回的是最后一个
 *    最具体的匹配值；git 按出现顺序处理多值、只跳过比已经见过的更不具体的条目，所以它为空与最终的请求头列表为空等价，
 *    非空即拒绝是保守的（gitops.test.ts 用真实 git 和回环服务核对了这个等价关系）；
 * 3. libcurl 读的 netrc 中有这个主机或 default（保守判定，见 netrcConflicts）。
 * 查询失败也作为一项返回（调用方同样拒绝这次传输）。结果中只有键和来源，不含值；地址只写净化后的形式（safeURL）。
 */
export async function authConflicts(url: string, c: AuthCheck): Promise<AuthConflict[]> {
  const out: AuthConflict[] = [];
  let u: URL | undefined;
  try {
    u = new URL(url);
  } catch {
    // 不是 URL 的地址（git 的 scp 形式等）不经过 HTTP 远程助手
  }
  if (u && (u.username || u.password)) out.push({ key: '地址中的用户名或密码', source: `地址 ${safeURL(url)}（可能来自 url.<地址>.insteadOf）` });
  // 查询与真实远程助手读到的配置来源一致：GIT_CONFIG 只影响 git config 命令本身（相当于 --file），远程助手不读它
  const env = { ...c.env, GIT_CONFIG: undefined };
  // 已经认出的密码不再放进诊断子进程的参数（用户名参与 URL 匹配，保留）
  const query = u?.password ? withoutPassword(u) : url;
  const r = await c.exec('git', ['config', '-z', '--get-urlmatch', extraHeaderKey, query], { env, cwd: c.cwd, timeoutMs: c.timeoutMs ?? authQueryTimeoutMs });
  // 只有 git 正常退出、明确返回 1 才是没有匹配；被信号终止、超时、其他退出码都是查询失败（拒绝这次传输）
  const unmatched = r.code === 1 && exitedNormally(r);
  if (r.code === 0 && exitedNormally(r) && hasHeaderValue(r.stdout)) out.push({ key: extraHeaderKey, source: await extraHeaderOrigin(query, { ...c, env }) });
  else if (r.code !== 0 && !unmatched) {
    out.push({ key: extraHeaderKey, source: `查询失败（git config --get-urlmatch 退出码 ${r.code}，例如配置文件损坏）：在同一个目录中运行 git config --get-urlmatch ${extraHeaderKey} <地址> 查看原因` });
  }
  if (u) out.push(...(await netrcConflicts(u.hostname.replace(/^\[(.*)\]$/, '$1'), c)));
  return out;
}

/** machineCheckURL 是检查执行机器的认证配置时用的任务仓库主机（Kiln 的仓库都在 GitHub 上）。 */
export const machineCheckURL = 'https://github.com/';

/**
 * machineAuthConflicts 检查服务用户的环境中会对任务仓库主机（machineCheckURL）生效的个人认证配置，只返回键和来源：
 * - 对主机根地址做与传输包装相同的检查（authConflicts）：netrc 中的这个主机或 default（clone 中没有对应的配置可以关掉它），
 *   对根地址生效的 http.extraHeader（来自全局、系统配置或进程环境中的配置）；
 * - 全局、系统配置（含 include）中只对这个主机的某个路径（组织、仓库）声明的 http.<地址>.extraHeader（scopedHeaders）：
 *   它们匹配不到根地址，但会对那些仓库的传输生效（clone 的本地配置只在任务仓库的地址上把它置空，依赖仓库、子模块的地址
 *   不在其中）。
 * dir 必须是不在任何仓库中的空目录：不读任何仓库的本地配置（includeIf "gitdir:" 等按仓库生效的 include 因此也不会生效）。
 * 只用于诊断和告警（doctor、连接器启动时），不改变节点的状态（PLAN #33.13）。
 */
export async function machineAuthConflicts(c: Omit<AuthCheck, 'cwd'>, dir: string): Promise<AuthConflict[]> {
  const env = { ...c.env, GIT_DIR: undefined, GIT_WORK_TREE: undefined, GIT_CEILING_DIRECTORIES: path.dirname(dir) };
  const check = { ...c, env, cwd: dir };
  const out = await authConflicts(machineCheckURL, check);
  for (const [scope, name] of configScopes(env)) {
    if (scope[0] !== '--local') out.push(...(await scopedHeaders(scope, name, scope[0] === '--system' ? '系统配置' : '全局配置', check)));
  }
  return out;
}

/**
 * scopedHeaders 列出一个配置文件（一份全局配置、系统配置，含 include 的文件）中声明的、地址的主机是任务仓库主机的
 * http.<地址>.extraheader（值非零长度、或者没有值的）：按键报告，来源是声明它的具体文件（--show-origin，include 进来的
 * 文件写它自己），不输出值。保守：同一个地址上的多个值不按顺序抵消，有一个非零长度的值就报告。查询失败（不是正常退出的
 * 0 或 1）作为一项返回。
 */
async function scopedHeaders(scope: string[], name: string, kind: string, c: AuthCheck): Promise<AuthConflict[]> {
  const r = await c.exec('git', ['config', ...scope, '--includes', '--show-origin', '-z', '--get-regexp', String.raw`^http\..+\.extraheader$`], {
    env: c.env,
    cwd: c.cwd,
    timeoutMs: c.timeoutMs ?? authQueryTimeoutMs,
  });
  // 1：没有这样的键（文件不存在也是 1）
  if (r.code === 1 && exitedNormally(r)) return [];
  if (r.code !== 0 || !exitedNormally(r)) {
    return [{ key: `http.<地址>.${extraHeaderKey.slice('http.'.length)}`, source: `${name}：查询失败（git config ${scope.join(' ')} --get-regexp 退出码 ${r.code}，例如配置文件损坏）` }];
  }
  const host = new URL(machineCheckURL).hostname;
  const out: AuthConflict[] = [];
  const seen = new Set<string>();
  // -z --show-origin 的输出是交替的"来源\0键\n值\0"，没有值（隐式的 true）时只有键
  const parts = r.stdout.split('\0');
  for (let i = 0; i + 1 < parts.length; i += 2) {
    const origin = (parts[i] ?? '').replace(/^file:/, '');
    const rec = parts[i + 1] ?? '';
    if (!rec) continue;
    const nl = rec.indexOf('\n');
    const key = nl < 0 ? rec : rec.slice(0, nl);
    if (nl >= 0 && rec.length === nl + 1) continue; // 零长度的值：清空请求头，不是冲突
    const url = key.slice('http.'.length, -'.extraheader'.length);
    if (!url || !urlHostMatches(url, host)) continue;
    const shown = `http.${safeURL(url)}.extraheader`;
    if (seen.has(shown)) continue;
    seen.add(shown);
    out.push({ key: shown, source: origin ? `${kind} ${origin}` : name });
  }
  return out;
}

/**
 * urlHostMatches 判断 git 配置中的地址（http.<地址>.* 的 <地址>）的主机是否可能是 host：与 git 的 urlmatch 一样，主机中的
 * * 匹配一个标签内的任意字符，标签数必须相同。解析不了的地址 git 也不会拿它匹配，忽略。
 */
function urlHostMatches(url: string, host: string): boolean {
  let u: URL;
  try {
    u = new URL(url);
  } catch {
    return false;
  }
  const pattern = u.hostname.split('.');
  const labels = host.toLowerCase().split('.');
  return pattern.length === labels.length && pattern.every((p, i) => p === labels[i] || (p.includes('*') && globLabel(p).test(labels[i] ?? '')));
}

/** globLabel 把主机的一个标签（其中的 * 匹配标签内的任意字符）转成正则。 */
function globLabel(p: string): RegExp {
  return new RegExp(`^${p.split('*').map((s) => s.replace(/[.*+?^${}()|[\]\\]/g, String.raw`\$&`)).join('[^.]*')}$`);
}

/**
 * hasHeaderValue 判断 git config -z --get-urlmatch http.extraheader 的输出（生效的最后一个值加一个 NUL）是不是会让 git 带上
 * 请求头。与 git 的 http.c 一致：只有零长度的值清空请求头列表，其余的值（包括只有空白的、首尾带空白的）都追加；最后一个值
 * 是零长度时列表为空，否则至少有一个请求头。按原始值判断，不去掉空白（只去掉 git 追加的 NUL）。
 */
export function hasHeaderValue(out: string): boolean {
  return (out.endsWith('\0') ? out.slice(0, -1) : out).length > 0;
}

function withoutPassword(u: URL): string {
  const v = new URL(u.href);
  v.password = '';
  return v.href;
}

/**
 * safeURL 是诊断中写的地址：只有协议、主机和能认出的仓库标识（owner/name），不含用户名、密码、query、fragment；路径认不出
 * （不是 owner/name、带百分号编码或 . / .. 段）时只写主机；解析不了的地址不回显原文。
 */
export function safeURL(raw: string): string {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return '（不能解析的地址，不显示）';
  }
  const repo = safeRepoPath(raw) ? repoOfPath(u.pathname) : '';
  return `${u.protocol}//${u.host}/${repo || '…'}`;
}

/**
 * extraHeaderOrigin 尽力给出生效的 http.extraheader 来自哪里，只用于诊断（是否冲突由 authConflicts 中不限范围的
 * --get-urlmatch 决定，不受这里影响，不会漏报）：
 * - git 2.46 起 git config get --show-origin --url=<地址> 给出具体的文件（只取来源，丢掉值）；
 * - 更早的 git（例如 Debian bookworm 的 2.39、macOS 的 Apple git）没有这个子命令：逐个范围（本地、全局、系统）带
 *   --includes 查询 --get-urlmatch，列出有非空值的范围。include 进来的文件算在包含它的范围里（写不出具体是哪个被
 *   include 的文件）；按范围单独查询不考虑别的范围中更具体的空值，可能多列（保守）。全局、系统配置给出 GIT_CONFIG_GLOBAL、
 *   GIT_CONFIG_SYSTEM 指定的文件，没有指定时给出 git 默认读的位置。哪个范围都没有时，来自环境变量配置
 *   （GIT_CONFIG_COUNT、GIT_CONFIG_PARAMETERS）或 git -c。
 */
async function extraHeaderOrigin(url: string, c: AuthCheck): Promise<string> {
  const r = await c.exec('git', ['config', 'get', '--show-origin', `--url=${url}`, extraHeaderKey], { env: c.env, cwd: c.cwd, timeoutMs: c.timeoutMs ?? authQueryTimeoutMs });
  const origin = r.code === 0 ? (r.stdout.split('\t')[0] ?? '').trim() : '';
  if (origin) return `git 配置 ${origin}`;
  const found: string[] = [];
  for (const [scope, name] of configScopes(c.env)) {
    const q = await c.exec('git', ['config', ...scope, '--includes', '-z', '--get-urlmatch', extraHeaderKey, url], { env: c.env, cwd: c.cwd, timeoutMs: c.timeoutMs ?? authQueryTimeoutMs });
    if (q.code === 0 && exitedNormally(q) && hasHeaderValue(q.stdout)) found.push(name);
  }
  return found.length > 0
    ? `git 配置：${found.join('；')}（含其中 include 的文件）`
    : 'git 配置（不在本地、全局、系统配置文件中：来自环境变量配置 GIT_CONFIG_COUNT / GIT_CONFIG_PARAMETERS 或 git -c）';
}

/**
 * configScopes 是逐个查询的配置文件（git config 的参数）和它们在诊断中的说明：本地配置；git 实际读的每一份全局配置
 * （globalConfigFiles，分别用 --file 查询：--global 在 ~/.gitconfig 存在时只读它，会漏掉 XDG 的那份）；系统配置
 * （GIT_CONFIG_NOSYSTEM 为真时没有，按 gitEnvTrue；为空的 GIT_CONFIG_SYSTEM 由 git 自己处理：git 不读系统配置，--system 的
 * 查询同样没有结果）。
 */
function configScopes(env: Record<string, string | undefined>): Array<[string[], string]> {
  const out: Array<[string[], string]> = [[['--local'], '本地配置（这个仓库的 .git/config）']];
  for (const f of globalConfigFiles(env)) out.push([['--file', f], `全局配置 ${f}`]);
  if (!gitEnvTrue(env.GIT_CONFIG_NOSYSTEM)) {
    out.push([['--system'], env.GIT_CONFIG_SYSTEM ? `系统配置 ${env.GIT_CONFIG_SYSTEM}` : '系统配置（git config --system --list --show-origin 可以看到文件）']);
  }
  return out;
}

/**
 * gitEnvTrue 判断布尔型的 git 环境变量（git_env_bool）是否为真，在 git 2.39.5、2.47.3 上核实：true / yes / on（不区分大小写）
 * 和非零整数（可以有开头的空白和正负号）为真；没有设置、空、false / no / off、0 为假；其余值 git 直接报错退出。这里只把明确
 * 为真的当作真，其余（包括 git 会报错的、带单位后缀的整数）都当作假：用于决定要不要检查系统配置时宁可多查。
 */
export function gitEnvTrue(v: string | undefined): boolean {
  if (v === undefined) return false;
  if (/^(true|yes|on)$/i.test(v)) return true;
  const m = /^[ \t\n\v\f\r]*[+-]?([0-9]+)$/.exec(v);
  return m !== null && /[1-9]/.test(m[1]!);
}

/**
 * globalConfigFiles 是 git 读的全局配置文件（与 git 的规则相同）：GIT_CONFIG_GLOBAL 设置时只有它（为空时没有）；否则是
 * $XDG_CONFIG_HOME/git/config（XDG_CONFIG_HOME 为空时是 ~/.config/git/config）和 ~/.gitconfig，两份都存在时都读。
 * HOME 没有设置时只有 XDG_CONFIG_HOME 下的那份。不存在的文件查询时按没有匹配处理（退出码 1）。
 */
export function globalConfigFiles(env: Record<string, string | undefined>): string[] {
  if (env.GIT_CONFIG_GLOBAL !== undefined) return env.GIT_CONFIG_GLOBAL ? [env.GIT_CONFIG_GLOBAL] : [];
  const files: string[] = [];
  // 与 git 相同按原始分量拼接（joinRaw）：词法规整会让查询的与 git 读的不是同一个文件
  if (env.XDG_CONFIG_HOME) files.push(joinRaw(env.XDG_CONFIG_HOME, 'git/config'));
  else if (env.HOME) files.push(joinRaw(env.HOME, '.config/git/config'));
  if (env.HOME) files.push(joinRaw(env.HOME, '.gitconfig'));
  return files;
}

/**
 * netrcFiles 是 libcurl 可能读的 netrc：NETRC 环境变量指定的文件（curl 8.16 起）；HOME 下、账户主目录下（HOME 未设置时
 * libcurl 用它；与 CI Runner 的 internal/runner/gitauth.go 相同，HOME 设置了也检查，可能误报）、Windows 的 USERPROFILE 下的
 * .netrc，Windows 还有 _netrc。全部检查（保守：不按 libcurl 版本区分）。
 */
export function netrcFiles(c: AuthCheck): string[] {
  const files: string[] = [];
  // 都按原始分量拼接（joinRaw，与 libcurl 的 "%s/.netrc" 相同），由文件系统解析符号链接和 ..；相对的 NETRC 按传输的工作
  // 目录（与 CI Runner 相同；没有时相对本进程的当前目录）
  if (c.env.NETRC) files.push(path.isAbsolute(c.env.NETRC) ? c.env.NETRC : joinRaw(c.cwd ?? '', c.env.NETRC));
  const homes = [c.env.HOME, c.accountHome, c.platform === 'win32' ? c.env.USERPROFILE : undefined].filter((h): h is string => !!h);
  const names = c.platform === 'win32' ? ['_netrc', '.netrc'] : ['.netrc'];
  for (const h of new Set(homes)) for (const n of names) files.push(joinRaw(h, n));
  return [...new Set(files)];
}

/**
 * netrcConflicts 检查 netrc 中有没有可能对 host 生效的条目。有意采用保守判定，不解析 netrc 的词法（与 CI Runner 的
 * internal/runner/gitauth.go netrcConflicts 相同）：文件中以 libcurl 认的空白、引号或文件首尾为界出现了 default 关键字，
 * 或者出现了这个主机名（都不区分大小写），或者引号中有反斜杠（较新的 libcurl 处理转义，按文本找不出对哪个主机生效），
 * 就算冲突——宏正文或值里碰巧出现主机名也会被拒绝，可以接受（Kiln 不使用执行机器上的个人凭据，去掉这个文件或其中的
 * 条目即可）。存在但读不了时同样是冲突（查询失败）。只报告文件和键，不输出内容。
 */
/** maxNetrcBytes 是检查 netrc 时最多读的字节数（正常的 netrc 只有几行；更大的按无法检查处理）。 */
export const maxNetrcBytes = 1 << 20;

/** NetrcRead 是读一个 netrc 的结果：内容、不存在，或者无法检查（原因）。 */
type NetrcRead = { text: string } | { missing: true } | { problem: string };

/**
 * readNetrc 有界地读一个 netrc：最多等 timeoutMs（例如主目录在无响应的网络文件系统上；到期返回无法检查，底层的读取
 * 自己结束后关闭文件），以 O_NONBLOCK 打开（没有写端的 FIFO 不会卡在打开上），不是普通文件、超过 maxNetrcBytes 的都按
 * 无法检查处理。无法检查由调用方按冲突处理（传输包装拒绝这次传输；doctor 失败，连接器启动时告警）。
 */
export async function readNetrc(file: string, timeoutMs: number): Promise<NetrcRead> {
  let timer: NodeJS.Timeout | undefined;
  const expired = new Promise<NetrcRead>((resolve) => {
    timer = setTimeout(() => resolve({ problem: `读取超时（${timeoutMs} ms）` }), timeoutMs);
  });
  try {
    return await Promise.race([readNetrcBounded(file), expired]);
  } finally {
    clearTimeout(timer);
  }
}

async function readNetrcBounded(file: string): Promise<NetrcRead> {
  let fh;
  try {
    fh = await open(file, fsConstants.O_RDONLY | fsConstants.O_NONBLOCK);
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    // ENOENT：不存在；ENOTDIR：路径中有一段不是目录（例如 HOME=/dev/null 时的 /dev/null/.netrc）——都按没有这个文件处理，
    // 不是"无法检查"。其余（没有权限、符号链接循环、文件名过长、I/O 错误等）仍然拒绝这次传输。
    return code === 'ENOENT' || code === 'ENOTDIR' ? { missing: true } : { problem: `读不了（${code ?? '未知错误'}）` };
  }
  try {
    const st = await fh.stat();
    if (!st.isFile()) return { problem: '不是普通文件' };
    if (st.size > maxNetrcBytes) return { problem: `超过 ${maxNetrcBytes} 字节` };
    const buf = Buffer.alloc(maxNetrcBytes + 1);
    let n = 0;
    for (;;) {
      const { bytesRead } = await fh.read(buf, n, buf.length - n, n);
      if (bytesRead === 0) break;
      n += bytesRead;
      if (n > maxNetrcBytes) return { problem: `超过 ${maxNetrcBytes} 字节` };
    }
    return { text: buf.subarray(0, n).toString('utf8') };
  } catch (err) {
    return { problem: `读不了（${(err as NodeJS.ErrnoException).code ?? '未知错误'}）` };
  } finally {
    await fh.close();
  }
}

const netrcKey = {
  machine: (host: string) => `netrc 中的主机 ${host}`,
  default: () => 'netrc 中的 default',
  escape: () => 'netrc 中带转义的引号（无法确定对哪个主机生效）',
} as const;

async function netrcConflicts(host: string, c: AuthCheck): Promise<AuthConflict[]> {
  const out: AuthConflict[] = [];
  for (const file of netrcFiles(c)) {
    const r = await readNetrc(file, c.timeoutMs ?? authQueryTimeoutMs);
    if ('missing' in r) continue;
    if ('problem' in r) {
      out.push({ key: 'netrc', source: `查询失败：${file} ${r.problem}` });
      continue;
    }
    const m = netrcMention(r.text, host);
    if (m) out.push({ key: netrcKey[m](host), source: file });
  }
  return out;
}

/**
 * netrcSep 是 netrc 中词的分界：libcurl 的 ISSPACE（空格、\t、\n、\v、\f、\r，只有 ASCII；JS 正则的 \s 还含 Unicode 空白，
 * 不用它）和引号。与 internal/runner/gitauth.go 的 netrcSep 相同。
 */
const netrcSep = `[ \t\n\v\f\r"']`;
/** netrcEscape 匹配引号中的反斜杠（与 internal/runner/gitauth.go 的 netrcEscapeRe 相同）。 */
const netrcEscape = /"[^"\n]*\\/;

/**
 * netrcMention 返回 netrc 的内容中以 netrcSep 或首尾为界出现的 host 或 default（不区分大小写），或者引号中有反斜杠
 * （escape），都没有时为空。
 */
export function netrcMention(text: string, host: string): '' | 'machine' | 'default' | 'escape' {
  const word = (w: string) => new RegExp(`(^|${netrcSep})${w.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}(${netrcSep}|$)`, 'i');
  if (word(host).test(text)) return 'machine';
  if (word('default').test(text)) return 'default';
  if (netrcEscape.test(text)) return 'escape';
  return '';
}
