import { existsSync } from 'node:fs';
import { mkdir, rm } from 'node:fs/promises';
import path from 'node:path';
import { type Exec, ExecError, type ExecResult, exitedNormally, must, tail } from './exec.js';
import { ErrorClass, errMessage, InfraError } from './errors.js';
import { sleep } from './kiln.js';
import { basicAuth } from './redact.js';

// 仓库的固定 clone 与交付校验（PLAN #33.4、#26.10）。每个仓库一个 clone（<clone_root>/<repo id>），Orca 从它建 worktree；
// clone 的 origin 是任务仓库的真实地址（不含凭据），refspec 照常：Agent 经 kiln-orca task exec 执行 git fetch origin、
// git pull origin <base> 时拿到的是这次传输的只读凭据（推送被 GitHub 拒绝）；不经 task exec 的网络 git 在任务仓库的主机上
// 只有一个回答 quit 的凭据 helper，直接失败，不会用到机器上的个人凭据。连接器自己的网络 git 用显式地址和显式 refspec。
//
// 网络 git（clone、fetch、push、ls-remote，PLAN #33.13）只用 Server 签发的短期 installation token，与执行机器上的 git 配置
// 隔离：不读全局、系统配置（凭据 helper、insteadOf、代理等），清空凭据 helper，不提示输入，不用 SSH；token 经环境变量中的
// git 配置（GIT_CONFIG_COUNT）作为限定在任务仓库地址上的请求头交给 git，不写进任何配置文件，也不出现在命令行参数中。
// 本地命令（status、add、commit、diff……）照常读机器上的配置。

/**
 * quitHelper 是 clone 的本地配置中任务仓库主机的凭据 helper（在清空之后）：只回答 quit=1，git 不再找别的 helper、不提示输入
 * （git credential 协议），不经 kiln-orca task exec 的网络 git 因此不会用到机器上的个人凭据。task exec 的环境变量配置在它之后
 * 读取，照常清空并换成连接器的 helper。
 */
export const quitHelper = '!f() { echo quit=1; }; f';
/**
 * cloneGuard 返回 clone 的本地配置中保护任务仓库的配置（每个键按顺序的全部值），地址没有主机（测试中的本地路径）时为空：
 * - 任务仓库主机的凭据 helper：先清空，再只回答 quit（不经 task exec 的网络 git 不会用到机器上的个人 helper）；
 * - 任务仓库地址上的 http.<地址>.extraHeader 为空值：比机器全局配置中的请求头（不限地址、按主机或更短的地址）更具体，
 *   空值清空前面累积的请求头，访问 origin 时不带个人的认证头，于是走到 quit helper 而失败。只覆盖任务仓库这个地址：
 *   子模块、依赖仓库的地址不在其中（它们经 task exec 时由传输包装检查冲突）。连接器自己的网络 git 经环境变量在同一个
 *   地址上注入请求头（networkEnv），环境变量配置在本地配置之后读取，不受这里的空值影响。
 */
export function cloneGuard(cloneURL: string): Array<[string, string[]]> {
  const host = helperScope(cloneURL);
  if (!host) return [];
  return [
    [`http.${cloneURL}.extraHeader`, ['']],
    [`credential.${host}.helper`, ['', quitHelper]],
  ];
}

function sameValues(got: string[], want: string[]): boolean {
  return got.length === want.length && got.every((v, i) => v === want[i]);
}

/** gitAuthor、gitEmail 是连接器 clone 中的 git 身份（本地配置；checkpoint、Agent 的 commit 都用它，不需要机器上的身份）。 */
export const gitAuthor = 'kiln-orca';
export const gitEmail = 'kiln-orca@localhost';
/** gitTimeoutMs 是连接器一条 git 命令的期限。 */
export const gitTimeoutMs = 10 * 60_000;
/** confirmTimeoutMs 是推送前后用 ls-remote 查远端事实的期限（推送之后确认不了时推送的结果未知）。 */
export const confirmTimeoutMs = 60_000;

/** GitAuth 是网络 git 用的凭据（Server 签发的 installation token，PLAN #33.13）。 */
export interface GitAuth {
  username: string;
  secret: string;
}

/** isHTTPURL 判断仓库地址是不是 HTTP(S)：只有这种地址带凭据（测试中的本地路径不需要）。 */
export function isHTTPURL(url: string): boolean {
  return /^https?:\/\//i.test(url);
}

/** maxGitConfigCount 是 git 接受的 GIT_CONFIG_COUNT 的上限（INT_MAX）。 */
const maxGitConfigCount = 2147483647;

/**
 * gitConfigCount 按 git 的规则（config.c：strtoul 十进制，之后不能有别的字符，不超过 INT_MAX）解析 GIT_CONFIG_COUNT：开头
 * 可以有空白（C 的 isspace）和一个正负号，之后是十进制数字（前导 0 不是八进制、不认 0x）；没有设置和空字符串是 0；"-0" 是
 * 0，其余负数、超过 INT_MAX、没有数字、有别的字符的 git 拒绝（返回 undefined）。与 CI Runner 的 gitConfigCount 相同，在 git
 * 2.39.5、2.47.3 上逐项核实。
 */
export function gitConfigCount(s: string | undefined): number | undefined {
  if (s === undefined || s === '') return 0;
  const m = /^[ \t\n\v\f\r]*([+-]?)([0-9]+)$/.exec(s);
  if (!m) return undefined;
  const digits = m[2]!.replace(/^0+/, '');
  if (digits === '') return 0;
  if (m[1] === '-' || digits.length > String(maxGitConfigCount).length) return undefined;
  const n = Number(digits);
  return n <= maxGitConfigCount ? n : undefined;
}

/**
 * gitEnvConfig 返回在 base 已有的环境变量配置（GIT_CONFIG_COUNT、GIT_CONFIG_KEY_<n>、GIT_CONFIG_VALUE_<n>）之后追加 entries
 * 的环境变量。环境变量中的配置在全部配置文件之后读取，多值的配置（凭据 helper）可以先用空值清空再追加。base 的
 * GIT_CONFIG_COUNT 是 git 会拒绝的值时返回空（什么都不改：git 照样失败，与没有 Kiln 时相同，不从 0 开始覆盖已有的项）。
 */
export function gitEnvConfig(base: Record<string, string | undefined>, entries: Array<[string, string]>): Record<string, string> {
  const start = gitConfigCount(base.GIT_CONFIG_COUNT);
  if (start === undefined) return {};
  const out: Record<string, string> = { GIT_CONFIG_COUNT: String(start + entries.length) };
  entries.forEach(([k, v], i) => {
    out[`GIT_CONFIG_KEY_${start + i}`] = k;
    out[`GIT_CONFIG_VALUE_${start + i}`] = v;
  });
  return out;
}

/**
 * envCleanup 是一组要从继承的环境中清理掉的变量：静态的键，以及按前缀匹配的键（例如 GIT_CONFIG_KEY_<n> 的编号不定，
 * 要把 base 中实际出现的都清掉）。每组只定义一次，networkEnv 与本地 git 共用。
 */
interface EnvCleanup {
  readonly keys: readonly string[];
  readonly prefixes: readonly string[];
}

/**
 * wrapperEnv 是外层 git 包装与凭据通道的私有变量：kiln-orca task exec 把 GIT_EXEC_PATH 指向这个 attempt 的 git exec 镜像，
 * 传输包装再经本机 socket 向连接器开操作。连接器自己的 git、内层 task exec 都不能继承它们，否则会递归套用外层的远程助手，
 * 被外层按宿主凭据冲突拒绝。
 */
const wrapperEnv: EnvCleanup = {
  keys: ['GIT_EXEC_PATH', 'KILN_ORCA_GIT_OP', 'KILN_GIT_OP', 'KILN_GIT_OP_ERROR', 'KILN_GIT_SOCKET', 'KILN_GIT_EXEC_REAL', 'KILN_GIT_CONFIG_N'],
  prefixes: ['KILN_GIT_CONFIG_KEY_', 'KILN_GIT_CONFIG_VALUE_'],
};

/** configInjectionEnv 是命令行 / 环境变量形式的 git 配置注入（git -c、GIT_CONFIG、GIT_CONFIG_COUNT 等）。 */
const configInjectionEnv: EnvCleanup = {
  keys: ['GIT_CONFIG', 'GIT_CONFIG_PARAMETERS', 'GIT_CONFIG_COUNT'],
  prefixes: ['GIT_CONFIG_KEY_', 'GIT_CONFIG_VALUE_'],
};

/** repoContextEnv 是让 git 指向别的仓库的变量（会覆盖 cwd 对应的仓库）。 */
const repoContextEnv: EnvCleanup = {
  keys: ['GIT_DIR', 'GIT_WORK_TREE', 'GIT_COMMON_DIR', 'GIT_INDEX_FILE', 'GIT_OBJECT_DIRECTORY', 'GIT_ALTERNATE_OBJECT_DIRECTORIES'],
  prefixes: [],
};

/** askpassEnv 是可能让人工输入凭据的 askpass 程序。 */
const askpassEnv: EnvCleanup = {
  keys: ['GIT_ASKPASS', 'SSH_ASKPASS'],
  prefixes: [],
};

/**
 * cleanupOverlay 返回把 groups 中的键从 base 中清掉的 overlay（值为 undefined，nodeExec 合并时删除它们）：静态的键全部
 * 清掉，按前缀匹配的键只清 base 中实际出现的（编号不定，不能列举）。
 */
function cleanupOverlay(groups: readonly EnvCleanup[], base: Record<string, string | undefined>): Record<string, undefined> {
  const out: Record<string, undefined> = {};
  const names = Object.keys(base);
  for (const g of groups) {
    for (const k of g.keys) out[k] = undefined;
    for (const p of g.prefixes) for (const n of names) if (n.startsWith(p)) out[n] = undefined;
  }
  return out;
}

/**
 * localGitEnv 是连接器自己的本地 git 命令（status、add、commit、config……）的环境：去掉外层包装与凭据通道的私有变量、
 * 环境变量形式的配置注入、仓库上下文和 askpass，不提示输入；照常读机器上的全局、系统配置（Git 的本地命令本来就该这样）。
 */
export function localGitEnv(base: Record<string, string | undefined>): Record<string, string | undefined> {
  return {
    ...cleanupOverlay([wrapperEnv, configInjectionEnv, repoContextEnv, askpassEnv], base),
    GIT_TERMINAL_PROMPT: '0',
  };
}

/**
 * networkEnv 是连接器的网络 git 命令的环境：不读全局、系统配置（HOME 也指向空设备，curl 不读 ~/.netrc），去掉继承的
 * 配置和调试输出，清空凭据 helper，不允许 SSH 等其它传输（不用机器上的 SSH key），强制校验证书；auth 给出时作为限定在
 * url 上的 Authorization 请求头（clone 的本地配置中如果被改了 insteadOf，改写后的地址不带它）。
 *
 * base 是调用方的环境（默认不读 process.env，保持纯函数）：外层 task exec 的 GIT_EXEC_PATH、凭据通道私有变量、环境变量
 * 配置注入、仓库上下文都在这里清掉，连接器自己的网络 git 不会递归套用外层的传输包装。
 */
export function networkEnv(url: string, auth?: GitAuth, base: Record<string, string | undefined> = {}): Record<string, string | undefined> {
  const entries: Array<[string, string]> = [
    ['credential.helper', ''],
    ['core.askPass', ''],
    ['protocol.ssh.allow', 'never'],
    ['protocol.git.allow', 'never'],
    ['protocol.ext.allow', 'never'],
    ['http.sslVerify', 'true'],
  ];
  if (auth && isHTTPURL(url)) entries.push([`http.${url}.extraHeader`, `Authorization: Basic ${basicAuth(auth.username, auth.secret)}`]);
  return {
    ...cleanupOverlay([wrapperEnv, configInjectionEnv, repoContextEnv, askpassEnv], base),
    GIT_CONFIG_GLOBAL: '/dev/null',
    GIT_CONFIG_NOSYSTEM: '1',
    GIT_CONFIG_SYSTEM: undefined,
    XDG_CONFIG_HOME: undefined,
    HOME: '/dev/null',
    NETRC: undefined,
    GIT_TERMINAL_PROMPT: '0',
    GIT_SSH: undefined,
    GIT_SSH_COMMAND: undefined,
    // 防御性清理：不允许借继承的变量跳过证书校验、放开传输协议、打开调试输出（调试输出可能包含请求头）
    GIT_SSL_NO_VERIFY: undefined,
    GIT_ALLOW_PROTOCOL: undefined,
    GIT_PROTOCOL_FROM_USER: undefined,
    GIT_TRACE: undefined,
    GIT_TRACE_CURL: undefined,
    GIT_TRACE_PACKET: undefined,
    GIT_CURL_VERBOSE: undefined,
    GIT_TRACE_REDACT: undefined,
    GIT_TRACE2: undefined,
    GIT_TRACE2_EVENT: undefined,
    GIT_TRACE2_PERF: undefined,
    ...gitEnvConfig({}, entries),
  };
}
/**
 * git 因为锁文件已经存在而失败时输出里的锁路径（连接器的 git 用 LC_ALL=C，输出是英文）：
 * `Unable to create '<path>.lock': File exists.`（路径原样放在单引号中，本身可能含单引号和空格，所以贪婪匹配到 `': File exists`）、
 * `could not lock config file <path>: File exists`（锁是 <path>.lock）。
 */
const quotedLock = /Unable to create '(.+\.lock)': File exists/;
const configLock = /could not lock config file (.+): File exists/;

/**
 * heldLock 从 git 的错误输出中找出已经存在的锁文件（相对路径按 dir 解析），认不出完整路径时返回 undefined（调用方只给原始输出）。
 *
 * 连接器不自动删除 git 的锁：clone 由 Orca 中的 Agent 共用（它们的 worktree 在这个 clone 上），Agent 的 git 操作不受连接器的
 * 期限和仓库锁约束，锁的年龄不能证明它已经失效。连接器自己的 git 在连接器退出时照常运行到结束、清理自己的锁，
 * 留下的锁通常来自机器崩溃，由人确认后删除。
 */
export function heldLock(dir: string, output: string): string | undefined {
  // 按行匹配：贪婪匹配不跨行，同一行出现两次时宁可认不出
  for (const line of output.split('\n')) {
    const q = quotedLock.exec(line)?.[1];
    if (q) return q.includes("': File exists") ? undefined : path.resolve(dir, q);
    const c = configLock.exec(line)?.[1];
    if (c) return c.includes(': File exists') ? undefined : path.resolve(dir, c + '.lock');
  }
  return undefined;
}

/**
 * 共用的 clone 中正在建的 worktree（Orca 的 worker-start、审计 Agent 自己的 git worktree add）在建好之前，它的 HEAD 还不是
 * 有效的对象；这时读全部 ref 的 git 命令（fetch 之后的连接检查把各个 worktree 的 HEAD 当作已有的引用）失败，输出
 * `fatal: bad object worktrees/<name>/HEAD`（LC_ALL=C）。这是暂时的：只对这一种错误等一会儿重试，次数有上限，用尽后
 * 原样抛出。其它的 bad object（对象真的坏了、缺了）不重试。
 */
const halfBuiltWorktree = /bad object worktrees\/[^/\s]+\/HEAD/;
/** halfBuiltWorktreeRetry：上面这种错误的重试次数和间隔（建 worktree 通常在一秒内完成）。 */
export const halfBuiltWorktreeRetry = { retries: 5, delayMs: 200 };

/** halfBuiltWorktreeError 判断 git 的错误是不是读到了正在建的 worktree 的 HEAD。 */
export function halfBuiltWorktreeError(err: unknown): boolean {
  return err instanceof ExecError && halfBuiltWorktree.test(err.message);
}

/** attemptBranch 是 attempt 的交付分支（与 devreport.AttemptBranch 相同）。 */
export function attemptBranch(attemptId: bigint): string {
  return `kiln/attempt-${attemptId}`;
}

/** isMarkdown 判断文件是否是 Markdown（与 devreport.IsMarkdown 相同：*.md，不区分大小写）。 */
export function isMarkdown(file: string): boolean {
  return file.toLowerCase().endsWith('.md');
}

/** GitWorktree 是 git worktree list --porcelain 中的一项：路径（git 记下的绝对路径）、检出的分支（分离 HEAD 时为空）。 */
export interface GitWorktree {
  path: string;
  branch: string;
}

/** parseWorktreeList 解析 git worktree list --porcelain -z 的输出（字段以 NUL 分隔，空字段分隔各项）。 */
export function parseWorktreeList(out: string): GitWorktree[] {
  const list: GitWorktree[] = [];
  let cur: GitWorktree | undefined;
  for (const field of out.split('\0')) {
    if (field.startsWith('worktree ')) {
      cur = { path: field.slice('worktree '.length), branch: '' };
      list.push(cur);
    } else if (cur && field.startsWith('branch ')) {
      cur.branch = field.slice('branch '.length);
    }
  }
  return list;
}

export class Git {
  /** timeoutMs 是一条 git 命令的期限（默认 gitTimeoutMs；测试中缩短）。 */
  constructor(
    private readonly exec: Exec,
    private readonly timeoutMs = gitTimeoutMs,
  ) {}

  /** run 在 dir 中执行本地 git 命令（不提示输入凭据，照常读机器上的配置；外层包装的私有变量不继承，见 localGitEnv）。 */
  run(dir: string, ...args: string[]): Promise<string> {
    return this.git(dir, args, localGitEnv(process.env));
  }

  /** remote 在 dir 中执行访问远端 url 的 git 命令（clone、fetch、push、ls-remote）：与机器上的 git 配置隔离（networkEnv）。 */
  private remote(dir: string, url: string, auth: GitAuth | undefined, args: string[], signal?: AbortSignal): Promise<string> {
    return this.git(dir, args, networkEnv(url, auth, process.env), { signal });
  }

  private git(dir: string, args: string[], env: Record<string, string | undefined>, o: { timeoutMs?: number; signal?: AbortSignal } = {}): Promise<string> {
    // LC_ALL=C：错误输出是英文（heldLock 按英文解析）；只影响连接器自己的 git，解析的其它输出（sha、porcelain、文件名）与语言无关
    return must(this.exec, 'git', args, { cwd: dir, timeoutMs: o.timeoutMs ?? this.timeoutMs, signal: o.signal, env: { ...env, LC_ALL: 'C' } });
  }

  async ok(dir: string, ...args: string[]): Promise<boolean> {
    try {
      await this.run(dir, ...args);
      return true;
    } catch (err) {
      if (err instanceof ExecError) return false;
      throw err;
    }
  }

  /**
   * fetch 从 url 执行 git fetch（会读全部 ref，见 halfBuiltWorktree）：读到正在建的 worktree 时按 halfBuiltWorktreeRetry 重试，
   * 其它错误和重试用尽时原样抛出。prepareClone 中的 fetch 都经过这里；地址、refspecs 总是显式给出（与机器上的配置隔离）。
   */
  private async fetch(dir: string, url: string, auth: GitAuth | undefined, signal?: AbortSignal, ...refspecs: string[]): Promise<void> {
    for (let attempt = 0; ; attempt++) {
      try {
        await this.remote(dir, url, auth, ['fetch', '--no-tags', '--quiet', url, ...refspecs], signal);
        return;
      } catch (err) {
        if (attempt >= halfBuiltWorktreeRetry.retries || !halfBuiltWorktreeError(err)) throw err;
        await sleep(halfBuiltWorktreeRetry.delayMs, signal);
        signal?.throwIfAborted();
      }
    }
  }

  /**
   * fetchOk 同 fetch，失败（命令出错）时返回 false。signal 已经结束时抛出：中止不是"这个 ref 拿不到"，
   * 把中止当成失败会让调用方继续执行（例如继续用已经放弃的 worktree 干活）。
   */
  private async fetchOk(dir: string, url: string, auth: GitAuth | undefined, signal?: AbortSignal, ...refspecs: string[]): Promise<boolean> {
    try {
      await this.fetch(dir, url, auth, signal, ...refspecs);
      return true;
    } catch (err) {
      if (signal?.aborted) throw err;
      if (err instanceof ExecError) return false;
      throw err;
    }
  }

  /**
   * worktrees 列出 clone 的全部 git worktree（包括 clone 本身）。git 输出路径时解析了符号链接（例如 macOS 的 /tmp →
   * /private/tmp），比较路径前要同样解析（见 reclaim.ts）。用 -z：路径中可能有空格、换行。
   */
  async worktrees(clone: string): Promise<GitWorktree[]> {
    // run 会去掉首尾的空白，NUL 不受影响
    return parseWorktreeList(await this.run(clone, 'worktree', 'list', '--porcelain', '-z'));
  }

  /** dirty 返回 dir 中未提交的改动和未跟踪的文件（git status --porcelain；干净时为空）。 */
  dirty(dir: string): Promise<string> {
    return this.run(dir, '--no-optional-locks', 'status', '--porcelain');
  }

  /** branches 列出 clone 的全部本地分支（refs/heads/…）。 */
  async branches(clone: string): Promise<string[]> {
    const out = await this.run(clone, 'for-each-ref', '--format=%(refname)', 'refs/heads/');
    return out.split('\n').filter((l) => l !== '');
  }

  /** deleteBranch 删除 clone 中的本地分支 ref（refs/heads/…）；分支被某个 worktree 检出时 git 拒绝（抛出）。 */
  async deleteBranch(clone: string, ref: string): Promise<void> {
    await this.run(clone, 'branch', '-D', '--', ref.replace(/^refs\/heads\//, ''));
  }

  /** detach 让 worktree dir 脱离分支，停在同一个 commit（工作区中的改动不动）。 */
  async detach(dir: string): Promise<void> {
    await this.run(dir, 'switch', '--quiet', '--detach');
  }

  /** hasCommit 判断 dir 中有没有 sha 这个 commit（objectExists：git 出错时抛出，不当作没有）。 */
  hasCommit(dir: string, sha: string): Promise<boolean> {
    return this.objectExists(dir, `${sha}^{commit}`);
  }

  /**
   * objectExists 用 git cat-file --batch-check 判断 name 指向的对象是否存在：只认这个名字那一行的 "<名字> missing"（确定
   * 没有）和 "<对象名> <类型> <大小>"（有）。cat-file -e 的退出码区分不了"没有"和"git 出错"（例如配置损坏、路径不存在都是
   * 128），这里命令没有正常退出、非零、输出不是完整的一行或无法识别时都抛出（不当作没有）。
   */
  private async objectExists(dir: string, name: string): Promise<boolean> {
    const args = ['cat-file', '--batch-check'];
    if (name.includes('\n')) throw new ExecError('git', args, { code: 0, stdout: '', stderr: `对象名中有换行：${JSON.stringify(name)}`, end: 'exit' });
    const r = await this.exec('git', args, { cwd: dir, timeoutMs: this.timeoutMs, input: `${name}\n`, env: { ...localGitEnv(process.env), LC_ALL: 'C' } });
    if (!exitedNormally(r) || r.code !== 0) throw new ExecError('git', args, r);
    const line = r.stdout.endsWith('\n') ? r.stdout.slice(0, -1) : undefined;
    if (line === `${name} missing`) return false;
    if (line !== undefined && /^[0-9a-f]{40,64} [a-z]+ \d+$/.test(line)) return true;
    throw new ExecError('git', args, { ...r, stderr: `${r.stderr}\n无法识别的结果：${JSON.stringify(r.stdout.slice(0, 200))}` });
  }

  /**
   * exitIs0 执行 git，返回它是否以 0 退出：正常退出、返回 1 是"否"；其余（没有正常退出、其他退出码，例如 128 是 git 出错）
   * 都抛出，不当作"否"。用于 merge-base --is-ancestor、rev-parse --verify --quiet 这类用 1 表示"否"的查询。
   */
  private async exitIs0(dir: string, ...args: string[]): Promise<boolean> {
    const r = await this.raw(dir, args);
    if (exitedNormally(r) && r.code === 0) return true;
    if (exitedNormally(r) && r.code === 1) return false;
    throw new ExecError('git', args, r);
  }

  /**
   * prepareClone 准备 dir 中的固定 clone：不存在时 clone，fetch 全部分支，按需获取 PR 的 head 和 wants 中的 commit。
   * 都用显式地址 cloneURL 和凭据 auth（网络 git 与机器上的配置隔离，见 networkEnv）；origin 设为真实地址（configure），
   * clone 的本地配置中写入连接器的 git 身份。返回拿不到的 commit（除 wants[0] 外，例如 base；wants[0] 拿不到时抛出错误）。
   */
  async prepareClone(dir: string, cloneURL: string, wants: string[], pr = 0, auth?: GitAuth, signal?: AbortSignal): Promise<string[]> {
    try {
      if (!existsSync(path.join(dir, '.git'))) {
        await rm(dir, { recursive: true, force: true });
        await mkdir(path.dirname(dir), { recursive: true });
        // 保护在 clone 建好时就写进本地配置（clone -c 在初始化之后、拉取之前写入）：origin 一出现就是真实地址
        const protect = cloneGuard(cloneURL).flatMap(([k, vs]) => vs.flatMap((v) => ['-c', `${k}=${v}`]));
        await this.remote(path.dirname(dir), cloneURL, auth, ['clone', '--no-checkout', '--quiet', ...protect, cloneURL, dir], signal);
      } else {
        // 任务目录（审计时 Agent 在其中检出被审 commit）按保留期删除后，清掉 git 中失效的 worktree 记录
        await this.ok(dir, 'worktree', 'prune');
      }
      await this.configure(dir, cloneURL);
      await this.fetch(dir, cloneURL, auth, signal, '--prune', '+refs/heads/*:refs/remotes/origin/*');
      const first = wants[0];
      if (pr > 0 && first && !(await this.hasCommit(dir, first))) {
        // PR 的 head 可能来自已经删除的分支
        await this.fetchOk(dir, cloneURL, auth, signal, `+refs/pull/${pr}/head:refs/remotes/origin/pr/${pr}`);
      }
      const missing: string[] = [];
      for (const sha of wants) {
        if (!sha || (await this.hasCommit(dir, sha))) continue;
        // 不在任何分支上的 commit（例如已经被 force push 覆盖）：按 sha 直接获取
        await this.fetchOk(dir, cloneURL, auth, signal, sha);
        if (!(await this.hasCommit(dir, sha))) missing.push(sha);
      }
      if (first && missing.includes(first)) throw new InfraError(ErrorClass.fetch, `获取不到 commit ${first}`);
      return missing;
    } catch (err) {
      if (err instanceof InfraError) throw err;
      const msg = err instanceof Error ? err.message : String(err);
      const lock = err instanceof ExecError ? heldLock(dir, msg) : undefined;
      if (lock) {
        throw new InfraError(
          ErrorClass.fetch,
          `git 的锁文件 ${lock} 已经存在（通常是机器崩溃时中断的 git 操作留下的）。确认没有 git 进程在使用仓库 ${dir} 后删除这个文件：${msg}`,
          { cause: err },
        );
      }
      throw new InfraError(ErrorClass.fetch, msg, { cause: err });
    }
  }

  /**
   * configure 让 clone 的本地配置成为要求的样子：保护任务仓库的配置（cloneGuard：任务仓库地址上的请求头为空、主机上只有
   * 回答 quit 的凭据 helper），origin 是任务仓库的真实地址，去掉以前留下的推送地址（remote.origin.pushurl），fetch 的
   * refspec 照常，git 身份是连接器的。
   *
   * clone 由 Orca 中的 Agent 共用（它们的 worktree 读这份配置），连接器的仓库锁管不到它们，所以：已经是要求的值时不写；
   * 保护先装好、再改 origin；改一个保护键时先用一条命令把它的全部值原子地替换成第一个值（空值：清空请求头或 helper 列表），
   * 再追加其余的值。任何一步之后（包括中途被中断）个人的 helper 和全局配置中的个人请求头都不生效。
   */
  private async configure(dir: string, cloneURL: string): Promise<void> {
    for (const [key, want] of cloneGuard(cloneURL)) {
      if (sameValues(await this.getAll(dir, key), want)) continue;
      await this.run(dir, 'config', '--local', '--replace-all', key, want[0]!);
      for (const v of want.slice(1)) await this.run(dir, 'config', '--local', '--add', key, v);
    }
    if (!sameValues(await this.getAll(dir, 'remote.origin.url'), [cloneURL])) await this.run(dir, 'remote', 'set-url', 'origin', cloneURL);
    if ((await this.getAll(dir, 'remote.origin.pushurl')).length > 0) await this.unsetAll(dir, 'remote.origin.pushurl');
    const refspec = '+refs/heads/*:refs/remotes/origin/*';
    if (!sameValues(await this.getAll(dir, 'remote.origin.fetch'), [refspec])) await this.run(dir, 'config', '--local', '--replace-all', 'remote.origin.fetch', refspec);
    for (const [k, v] of [
      ['user.name', gitAuthor],
      ['user.email', gitEmail],
    ] as const) {
      if (!sameValues(await this.getAll(dir, k), [v])) await this.run(dir, 'config', '--local', '--replace-all', k, v);
    }
  }

  /**
   * raw 执行本地 git 命令并返回原始结果（退出码、输出），期限与其他 git 命令相同（timeoutMs）：调用方自己解释退出码。
   * 只有正常退出的退出码有含义（被信号终止时 code 是 128 + 信号编号，超时是 timeoutCode，都不会与 git 的 1、5 混淆）。
   */
  private raw(dir: string, args: string[]): Promise<ExecResult> {
    return this.exec('git', args, { cwd: dir, timeoutMs: this.timeoutMs, env: { ...localGitEnv(process.env), LC_ALL: 'C' } });
  }

  /** getAll 返回 clone 本地配置中 key 的全部值（没有时为空：git 正常退出、返回 1）。 */
  private async getAll(dir: string, key: string): Promise<string[]> {
    const args = ['config', '--local', '-z', '--get-all', key];
    const r = await this.raw(dir, args);
    if (r.code === 1 && exitedNormally(r)) return [];
    if (r.code !== 0) throw new ExecError('git', args, r);
    return r.stdout.split('\0').slice(0, -1);
  }

  /** unsetAll 删除 clone 本地配置中 key 的全部值（没有时什么都不做：git 正常退出、返回 5）。 */
  private async unsetAll(dir: string, key: string): Promise<void> {
    const args = ['config', '--local', '--unset-all', key];
    const r = await this.raw(dir, args);
    if (exitedNormally(r) && (r.code === 0 || r.code === 5)) return;
    throw new ExecError('git', args, r);
  }

  /**
   * checkDelivered 校验 worktree 中的交付（与 #26.10 第 5 步相同）：工作区干净、HEAD 是起点的后代、相对起点有 commit、
   * 设计阶段只改了 Markdown、列出的设计文档在 HEAD 中。返回给 Agent 看的问题（为空表示通过）。
   */
  async checkDelivered(wt: string, o: { start: string; design: boolean; baseRef: string; documents: string[] }): Promise<string[]> {
    const problems: string[] = [];
    const st = await this.run(wt, '--no-optional-locks', 'status', '--porcelain');
    if (st) problems.push(`工作区还有未提交的改动或未跟踪的文件（git status --porcelain：${tail(st, 500)}）`);
    if (!(await this.exitIs0(wt, 'merge-base', '--is-ancestor', o.start, 'HEAD'))) {
      problems.push(`HEAD 不是起点 ${o.start} 的后代：不要 rebase 或重置起点之前的历史`);
      return problems;
    }
    if ((await this.run(wt, 'rev-parse', 'HEAD')) === o.start) problems.push('status 是 delivered，但相对起点没有任何 commit');
    if (o.design) {
      const out = await this.run(wt, '-c', 'core.quotePath=false', 'diff', '--name-only', '--no-renames', await this.designRange(wt, o.start, o.baseRef));
      const code = out.split('\n').filter((f) => f && !isMarkdown(f));
      if (code.length > 0) {
        problems.push(`设计阶段只能改 Markdown（*.md），这些文件不是：${tail(code.join('、'), 500)}。请撤回这些改动（不要重写起点之前的历史）`);
      }
    }
    for (const doc of o.documents) {
      if (!(await this.objectExists(wt, `HEAD:${doc}`))) problems.push(`design.documents 中的 ${doc} 不在 HEAD 中（请 commit 它，或者改正路径）`);
    }
    return problems;
  }

  /** designRange 返回核对设计只改 Markdown 的范围：有目标分支时是相对它的完整改动，否则从起点算。 */
  private async designRange(wt: string, start: string, baseRef: string): Promise<string> {
    if (baseRef) {
      const target = `refs/remotes/origin/${baseRef}`;
      if (await this.exitIs0(wt, 'rev-parse', '--verify', '--quiet', `${target}^{commit}`)) return `${target}...HEAD`;
    }
    return `${start}..HEAD`;
  }

  /** commitCheckpoint 把工作区的全部改动提交为一个临时 commit，返回相对起点是否有改动。 */
  async commitCheckpoint(wt: string, start: string, attemptId: bigint): Promise<boolean> {
    if (await this.run(wt, 'status', '--porcelain')) {
      await this.run(wt, 'add', '-A');
      await this.run(wt, '-c', `user.name=${gitAuthor}`, '-c', `user.email=${gitEmail}`, 'commit', '--quiet', '--no-verify', '-m', `kiln: checkpoint（attempt ${attemptId}，未完成的改动）`);
    }
    return (await this.run(wt, 'rev-parse', 'HEAD')) !== start;
  }

  /**
   * push 把 worktree 的 HEAD（开始时解析一次，之后固定为这个 commit）推到 attempt 分支（显式地址，凭据 auth），以 ls-remote
   * 中这个完整 ref 的值确认远端事实。结果分三种：
   * - 已证明成功：远端的 ref 就是这个 commit（推送之前已经是，或者推送之后、推送出错之后查到）；
   * - 明确失败：推送未能启动（notrun），或推送本身正常退出且 `--porcelain` 给出了这个 ref 的明确拒绝，或推送成功但确认到的远端不是这个 commit：抛出 InfraError（push，非 PushUnknownError）；
   * - 结果未知：推送被取消（signal）、超时、被信号/超限终止，或推送非零退出但没有远端对该 ref 的明确拒绝，或者确认本身失败（且推送并非 notrun）：抛出 PushUnknownError（下次写之前先查远端）。
   * 确认都用 confirmTimeoutMs 的期限；signal 在确认开始之前已经结束时只受期限约束（还能执行时做限时确认），之后结束时确认
   * 被取消、结果未知。不递归推送子模块。guard 在真正执行 git push 之前同步调用，抛出时不再推送，错误原样抛出。
   */
  async push(wt: string, cloneURL: string, attemptId: bigint, guard?: () => void, auth?: GitAuth, signal?: AbortSignal): Promise<{ branch: string; sha: string }> {
    const branch = attemptBranch(attemptId);
    const ref = `refs/heads/${branch}`;
    let sha: string;
    try {
      sha = await this.run(wt, 'rev-parse', '--verify', 'HEAD^{commit}');
    } catch (err) {
      throw new InfraError(ErrorClass.push, errMessage(err), { cause: err });
    }
    const env = networkEnv(cloneURL, auth, process.env);
    const lsRemote = () => this.git(wt, ['ls-remote', cloneURL, ref], env, { timeoutMs: confirmTimeoutMs, signal: signal?.aborted ? undefined : signal });
    const remoteHead = async () => lsRemoteRef(await lsRemote(), ref);
    const unknown = (what: string, err: unknown) =>
      new PushUnknownError(`推送 ${branch} 的结果未知（${what}：${errMessage(err)}），下次推送之前先查远端`, { cause: err });
    let before: string;
    try {
      before = await lsRemote();
    } catch (err) {
      throw new InfraError(ErrorClass.push, errMessage(err), { cause: err });
    }
    if (lsRemoteRef(before, ref) === sha) return { branch, sha };
    // git push 按后缀解析目标：远端没有这个分支、却有后缀相同的别的 ref（例如 refs/tags/refs/heads/kiln/attempt-<n>）时，
    // 推送会写到那个 ref 上。这种情况不推送
    const others = lsRemoteRefs(before).filter((r) => r !== ref);
    if (lsRemoteRef(before, ref) === '' && others.length > 0) {
      throw new InfraError(ErrorClass.push, `远端有与 ${ref} 后缀相同的 ref（${others.join('、')}），git push 会写到它上面：没有推送，请先删除这个 ref`);
    }
    guard?.();
    let pushErr: unknown;
    try {
      // 同一个 attempt 的分支只属于这次执行（重试时覆盖自己）；--no-follow-tags：推 v* tag 即发布；不递归推送子模块
      // --porcelain：每个 ref 一行状态（stdout），只有它能可靠地说明远端是否明确拒绝了这个 ref（见 porcelainRejection）
      await this.git(wt, ['push', '--porcelain', '--force', '--quiet', '--no-verify', '--no-follow-tags', '--recurse-submodules=no', cloneURL, `${sha}:${ref}`], env, { signal });
    } catch (err) {
      pushErr = err;
    }
    const notRun = pushErr instanceof ExecError && pushErr.result.end === 'notrun';
    // 只有 git push 未能启动或自己正常退出且给出远端明确拒绝才是明确失败；其余任何一种本地终止（调用方取消、超时、被外部信号
    // 终止、输出超过上限）都可能已经部分发出（远端的远程助手不一定停止），按结果不明处理：后查已到位则成功，否则结果未知。
    // 没能启动的不算（一定没有写入）。
    const interrupted = pushErr !== undefined && !notRun && (signal?.aborted === true || !(pushErr instanceof ExecError) || !exitedNormally(pushErr.result));
    let got: string;
    try {
      got = await remoteHead();
    } catch (err) {
      if (notRun) throw new InfraError(ErrorClass.push, `未能启动 git push：${errMessage(pushErr)}`, { cause: pushErr });
      throw unknown(pushErr === undefined ? '推送已经返回，确认远端失败' : `推送：${errMessage(pushErr)}；确认远端失败`, err);
    }
    if (got === sha) return { branch, sha };
    if (notRun) throw new InfraError(ErrorClass.push, `未能启动 git push：${errMessage(pushErr)}`, { cause: pushErr });
    if (interrupted) throw unknown(`推送没有正常结束（被取消、超时、被信号终止或输出超过上限），远端 ${branch} 现在是 ${JSON.stringify(got)}`, pushErr);
    if (pushErr !== undefined) {
      // git push 正常退出且非零不能证明远端拒绝了推送（例如连接在 receive-pack 给出最终状态之前断开：RPC failed、128）：
      // 只有 --porcelain 中这个 ref 的明确拒绝才是明确失败，其余（包括认证失败、没有这一行）都按结果未知
      const rejected = pushErr instanceof ExecError ? porcelainRejection(pushErr.result.stdout, ref) : '';
      if (rejected) throw new InfraError(ErrorClass.push, `远端拒绝了 ${branch}：${rejected}`, { cause: pushErr });
      throw unknown(`git push 失败，但没有远端对 ${ref} 的明确拒绝，远端 ${branch} 现在是 ${JSON.stringify(got)}`, pushErr);
    }
    throw new InfraError(ErrorClass.push, `推送后远端 ${branch} 是 ${JSON.stringify(got)}，应为 ${sha}`);
  }
}

/**
 * porcelainRejection 从 git push --porcelain 的输出（stdout）中找 ref 被明确拒绝的那一行，返回它的状态（例如
 * "[remote rejected] (pre-receive hook declined)"、"[rejected] (non-fast-forward)"），没有时为空。每个 ref 一行：
 * `<标记>\t<源>:<目标>\t<状态>`，标记 ! 表示被拒绝；只认状态以 [rejected] 或 [remote rejected] 开头的（[remote failure]
 * 是远端没有给出这个 ref 的状态，不算明确拒绝）。git 2.39 与 2.47 的格式相同。
 */
export function porcelainRejection(stdout: string, ref: string): string {
  for (const line of stdout.split('\n')) {
    const [flag, refs, status] = line.split('\t');
    if (flag !== '!' || refs === undefined || status === undefined) continue;
    if (refs.slice(refs.lastIndexOf(':') + 1) !== ref) continue;
    if (/^\[(rejected|remote rejected)\]/.test(status)) return status.trim();
  }
  return '';
}

/** PushUnknownError 是推送的结果未知（取消、超时或确认失败）：下次推送之前先查远端。 */
export class PushUnknownError extends InfraError {
  constructor(message: string, options?: { cause?: unknown }) {
    super(ErrorClass.push, message, options);
    this.name = 'PushUnknownError';
  }
}

/** lsRemoteRefs 是 git ls-remote 的输出中的全部 ref 名。 */
function lsRemoteRefs(out: string): string[] {
  const refs: string[] = [];
  for (const line of out.split('\n')) {
    const i = line.indexOf('\t');
    if (i > 0) refs.push(line.slice(i + 1));
  }
  return refs;
}

/** lsRemoteRef 从 git ls-remote 的输出（每行 <sha>\t<ref>）中取完整 ref 名等于 ref 的那一行的 sha，没有时为空。 */
export function lsRemoteRef(out: string, ref: string): string {
  for (const line of out.split('\n')) {
    const i = line.indexOf('\t');
    if (i > 0 && line.slice(i + 1) === ref) return line.slice(0, i);
  }
  return '';
}

/** helperScope 返回凭据 helper 限定的地址（HTTP(S) 地址的 <协议>://<主机>），不是 HTTP(S) 地址时为空。 */
export function helperScope(url: string): string {
  if (!isHTTPURL(url)) return '';
  try {
    const u = new URL(url);
    return `${u.protocol}//${u.host}`;
  } catch {
    return '';
  }
}
