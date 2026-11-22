import { createHash, randomUUID } from 'node:crypto';
import { type Credential, CredentialAccess } from '../gen/kiln/airunner/v2/airunner_pb.js';
import { gitEnvConfig, helperScope } from './git.js';
import { errMessage, StaleError } from './errors.js';
import type { Logger } from './log.js';
import type { Redactor } from './redact.js';

// attempt 的 git 凭据（PLAN #33.13）：按操作签发、用后吊销，执行机器上不需要 git 凭据。
//
// 每个凭据只属于一次能观察到结束的网络操作（GitOp），由持有它的一方在操作结束时关闭：连接器准备仓库（prepareClone，读）、
// 推 attempt 分支（push，写），以及 Agent 经 kiln-orca task exec 发起的每一个 git HTTP(S) 传输（传输包装到连接器的连接，
// 读，见 host/cli.ts 的 git-remote）。一个操作只签一枚、只覆盖一个仓库、不续签；操作之间不共用。
//
// - 签发在这个操作第一次要凭据时进行（IssueCredential 带 op_id 和仓库），同一操作后续的请求返回同一枚。
// - 关闭是终态：之后不再给出；签发还在途时关闭，结果回来后立即吊销、不交出；只要发出过签发请求，关闭时都调用
//   RevokeCredential（带 op_id，收到过凭据时一并带上），Server 据此收回它已经签发、而连接器没有收到的。
// - 签发、吊销都经 attempt 的 rpc：失效、节点被吊销之后不再调用 Server，由 Server 在 attempt 结束时回收，或者 1 小时内
//   自然过期。签发与吊销成对记日志（attempt、op、仓库、读 / 写、凭据指纹），签发到的密钥登记在 Redactor 中。

/** GitCredential 是签发到的 git 凭据。 */
export interface GitCredential {
  username: string;
  secret: string;
  /** expiresAt 是过期时间（毫秒）。 */
  expiresAt: number;
}

export interface GitOpsDeps {
  /** issue 向 Server 要这个操作的凭据（经 attempt 的 rpc / retry：失效时抛出 StaleError；signal 结束时不再重试）。 */
  issue(req: { access: CredentialAccess; opId: string; repository: string }, signal: AbortSignal): Promise<Credential>;
  /** revoke 请 Server 关闭操作并吊销它的凭据（secret 为空：没有收到凭据）。 */
  revoke(req: { opId: string; secret: string }): Promise<void>;
  redactor: Redactor;
  log: Logger;
  /** print 写一行任务日志（Server 上这次执行的日志）。 */
  print(line: string): void;
  attemptId: string;
}

/** OpClosedError 是操作已经关闭（包括签发在途时关闭）之后还要凭据。 */
export class OpClosedError extends Error {
  constructor() {
    super('这次 git 操作已经结束，凭据已经收回');
    this.name = 'OpClosedError';
  }
}

/** accessText 是日志中的权限。 */
function accessText(a: CredentialAccess): string {
  return a === CredentialAccess.WRITE ? '写' : '读';
}

/** tokenFingerprint 是日志中凭据的指纹（sha256 的前 12 位，不能还原出凭据）：签发、吊销按它配对。 */
export function tokenFingerprint(secret: string): string {
  return `sha256:${createHash('sha256').update(secret).digest('hex').slice(0, 12)}`;
}

/** GitOp 是一个网络操作和它的凭据。 */
export class GitOp {
  readonly id = randomUUID();
  private pending: Promise<GitCredential> | undefined;
  private closing: Promise<void> | undefined;
  private readonly ctl = new AbortController();

  constructor(
    private readonly ops: GitOps,
    /** repo 是这个操作访问的仓库（owner/name，按声明的写法）。 */
    readonly repo: string,
    readonly access: CredentialAccess,
  ) {}

  get open(): boolean {
    return !this.closing;
  }

  /** token 返回这个操作的凭据：第一次调用时签发（同一时刻只要一次），之后返回同一枚。关闭之后抛出 OpClosedError。 */
  async token(): Promise<GitCredential> {
    if (this.closing) throw new OpClosedError();
    this.pending ??= this.ops.issue(this, this.ctl.signal);
    const c = await this.pending;
    // 签发的过程中关闭了：不交出（close 等到签发结束后吊销它）
    if (this.closing) throw new OpClosedError();
    return c;
  }

  /**
   * close 关闭操作（终态），发出过签发请求时请 Server 吊销。可以重复调用，返回同一个结果。关闭时立即从可查询的表中去掉
   * （之后 get 找不到它），吊销完成之前 GitOps.settle 都会等它。
   */
  close(): Promise<void> {
    this.closing ??= this.ops.track(this, this.finish());
    return this.closing;
  }

  private async finish(): Promise<void> {
    this.ctl.abort();
    if (!this.pending) return;
    const c = await this.pending.catch(() => undefined);
    await this.ops.revoke(this, c);
  }
}

/** GitOps 是一个 attempt 的全部 git 操作。 */
export class GitOps {
  /** 还没有关闭的操作。 */
  private readonly live = new Map<string, GitOp>();
  /** 已经关闭、吊销还没有结束的操作（结束时去掉）。 */
  private readonly settling = new Set<Promise<void>>();

  constructor(private readonly d: GitOpsDeps) {}

  /** open 开一个访问 repo 的操作（还没有签发）。 */
  open(repo: string, access: CredentialAccess): GitOp {
    const op = new GitOp(this, repo, access);
    this.live.set(op.id, op);
    return op;
  }

  /** get 返回还没有关闭的操作。 */
  get(id: string): GitOp | undefined {
    return this.live.get(id);
  }

  /** run 开一个操作执行 f（第一次要凭据时签发），结束后（无论成败）关闭。 */
  async run<T>(repo: string, access: CredentialAccess, f: (op: GitOp) => Promise<T>): Promise<T> {
    const op = this.open(repo, access);
    try {
      return await f(op);
    } finally {
      await op.close();
    }
  }

  /**
   * closeAll 关闭全部还没有关闭的操作（只关 access 的，不给时全部），并等全部已经开始的关闭（包括之前由传输连接关闭
   * 触发的）结束：返回时这个 attempt 不再有吊销在途。
   */
  async closeAll(access?: CredentialAccess): Promise<void> {
    for (const op of [...this.live.values()]) if (access === undefined || op.access === access) void op.close();
    await this.settle();
  }

  /** settle 等全部已经开始的关闭结束（关闭不抛出）。 */
  async settle(): Promise<void> {
    while (this.settling.size > 0) await Promise.all([...this.settling]);
  }

  /** track 在 op 开始关闭时调用：从可查询的表中去掉，关闭结束之前留在 settling 中。 */
  track(op: GitOp, closing: Promise<void>): Promise<void> {
    this.live.delete(op.id);
    const p = closing.finally(() => this.settling.delete(p));
    this.settling.add(p);
    return p;
  }

  /** issue 签发 op 的凭据，登记到脱敏，记日志。 */
  async issue(op: GitOp, signal: AbortSignal): Promise<GitCredential> {
    const r = await this.d.issue({ access: op.access, opId: op.id, repository: op.repo }, signal);
    if (!r.secret) throw new Error('Server 没有给出凭据');
    const c: GitCredential = { username: r.username, secret: r.secret, expiresAt: Number(r.expiresAt) * 1000 };
    this.d.redactor.add(c.secret, c.expiresAt, c.username);
    this.report('签发 git 凭据', op, c);
    return c;
  }

  /**
   * revoke 尽力请 Server 关闭 op、回收它的凭据（c 为 undefined：没有收到凭据；失败只记日志：凭据 1 小时内自然过期）。
   * RPC 成功只说明 Server 已经关闭了这个操作：Server 对 GitHub 的吊销失败时只记 warn（带同一个指纹和到期时间）、照样
   * 返回成功（不重试，PLAN #33.13），所以日志写"已请求 Server 关闭"，不写成已吊销；实际的吊销结果以 Server 日志为准。
   */
  async revoke(op: GitOp, c: GitCredential | undefined): Promise<void> {
    try {
      await this.d.revoke({ opId: op.id, secret: c?.secret ?? '' });
      this.report(c ? '已请求 Server 关闭 git 操作、回收凭据' : '已请求 Server 关闭 git 操作（没有收到凭据）', op, c);
    } catch (err) {
      const f = this.fields(op, c);
      if (err instanceof StaleError) {
        // 失效、节点被吊销之后不再调用 Server：由 Server 在 attempt 结束时回收，或者自然过期
        this.d.log.info('没有吊销 git 凭据：attempt 已经失效，由 Server 回收或自然过期', f);
        return;
      }
      const until = c ? new Date(c.expiresAt).toISOString() : '';
      this.d.log.warn('请求 Server 关闭 git 操作失败（凭据到期自然失效）', { ...f, expiresAt: until, err: errMessage(err) });
      this.d.print(`请求 Server 关闭 git ${accessText(op.access)}操作失败（op ${op.id}，${c ? tokenFingerprint(c.secret) : '没有收到凭据'}，${until ? `${until} ` : ''}到期自然失效）`);
    }
  }

  private fields(op: GitOp, c: GitCredential | undefined): Record<string, unknown> {
    return { attempt: this.d.attemptId, op: op.id, repo: op.repo, access: accessText(op.access), token: c ? tokenFingerprint(c.secret) : '' };
  }

  private report(msg: string, op: GitOp, c: GitCredential | undefined): void {
    this.d.log.info(msg, this.fields(op, c));
    this.d.print(`${msg}：${op.repo}，${accessText(op.access)}，op ${op.id}${c ? `，${tokenFingerprint(c.secret)}` : ''}`);
  }
}

/**
 * maxCredentialQueryBytes 是 credential helper 从 stdin 读的请求的上限：git 给出的请求只有几行（protocol、host、path 等），
 * 超过的不是 git 的正常请求（例如直接调用 helper 喂入的大量数据），不给凭据。
 */
export const maxCredentialQueryBytes = 64 << 10;

/** readBounded 读完 src，总长超过 max 字节时停止读取并返回 undefined。 */
export async function readBounded(src: AsyncIterable<Buffer | string>, max: number): Promise<string | undefined> {
  const chunks: Buffer[] = [];
  let n = 0;
  for await (const c of src) {
    const b = typeof c === 'string' ? Buffer.from(c) : c;
    n += b.length;
    if (n > max) return undefined;
    chunks.push(b);
  }
  return Buffer.concat(chunks).toString('utf8');
}

/** ExecGit 是 kiln-orca task exec 中 git 用的设置（exec-env 的回复，任务仓库不是 HTTP(S) 地址时没有）。 */
export interface ExecGit {
  /** helperURL 是 credential helper 限定的地址（任务仓库的 <协议>://<主机>）。 */
  helperURL: string;
  /** goprivate 是加进 GOPRIVATE 的模块路径模式（声明的依赖仓库，见 goPrivatePattern）。 */
  goprivate: string[];
  /** execRoot 是这个 attempt 的 git exec 镜像目录的父目录（在任务目录中，见 gitexec.ts）。 */
  execRoot: string;
}

/** credentialCmd 是 credential helper 调用的 kiln-orca task 子命令（git 在它后面加上 get、store、erase）。 */
export const credentialCmd = 'git-credential';
/** remoteCmd 是传输包装调用的 kiln-orca task 子命令（git-remote-http(s) 的替身，见 gitexec.ts）。 */
export const remoteCmd = 'git-remote';
/** refusedCmd 是传输包装因为认证冲突拒绝一次传输时告诉连接器的命令（连接器记 warn 日志和任务日志）。 */
export const refusedCmd = 'git-refused';
/** gitOpEnv 是传输包装交给真实远程助手（再由它交给 credential helper）的环境变量：这次传输的操作 id。 */
export const gitOpEnv = 'KILN_ORCA_GIT_OP';

/**
 * execGitEnv 返回 kiln-orca task exec 中 Agent 命令的 git 相关环境变量（在 base 之上，只返回要改的；值为 undefined 的
 * 要从环境中删除）：
 * - GIT_EXEC_PATH 指向这个 attempt 的 git exec 镜像（execPath，HTTP(S) 传输经过传输包装，见 gitexec.ts）；
 * - 在已有的环境变量配置之后，对任务仓库的主机先清空凭据 helper 再加上连接器的（toolCommand credentialCmd，经本机 socket
 *   向连接器要这次传输的读凭据），按路径区分仓库（useHttpPath）；
 * - 不给凭据时不能退回到个人的交互凭据：去掉 GIT_ASKPASS、SSH_ASKPASS，core.askPass 设为空，不提示输入
 *   （GIT_TERMINAL_PROMPT=0 不阻止 askpass）；
 * - 不继承外层的操作 id（只由传输包装设置）；声明的依赖仓库加进 GOPRIVATE（保留原来的值）。凭据本身不在环境变量中。
 */
export function execGitEnv(base: Record<string, string | undefined>, git: ExecGit, toolCommand: string, execPath: string): Record<string, string | undefined> {
  const scope = `credential.${git.helperURL}`;
  const out: Record<string, string | undefined> = {
    ...gitEnvConfig(base, [
      [`${scope}.helper`, ''],
      [`${scope}.helper`, `!${toolCommand} ${credentialCmd}`],
      [`${scope}.useHttpPath`, 'true'],
      ['core.askPass', ''],
    ]),
    GIT_EXEC_PATH: execPath,
    GIT_TERMINAL_PROMPT: '0',
    GIT_ASKPASS: undefined,
    SSH_ASKPASS: undefined,
    [gitOpEnv]: undefined,
  };
  if (git.goprivate.length > 0) out.GOPRIVATE = [base.GOPRIVATE, ...git.goprivate].filter((v) => v).join(',');
  return out;
}

/** CredentialQuery 是 git credential helper 收到的请求中用到的字段。 */
export interface CredentialQuery {
  protocol: string;
  host: string;
  path: string;
}

/** parseCredentialQuery 解析 git credential helper 协议的输入（每行 key=value，空行结束）。 */
export function parseCredentialQuery(input: string): CredentialQuery {
  const q: CredentialQuery = { protocol: '', host: '', path: '' };
  for (const line of input.split('\n')) {
    if (line.trim() === '') break;
    const i = line.indexOf('=');
    if (i < 0) continue;
    const k = line.slice(0, i);
    if (k === 'protocol' || k === 'host' || k === 'path') q[k] = line.slice(i + 1).trimEnd();
  }
  return q;
}

/**
 * repoKey 是仓库名 owner/name 用于比较的形式：GitHub 的仓库名不区分大小写（与 Server 的 agentcfg.Config.Repositories、
 * store.FindRepo 相同），连接器中比较仓库名都经过它。
 */
export function repoKey(repo: string): string {
  return repo.toLowerCase();
}

/** repoOfPath 把 helper 请求中的路径（owner/name 或 owner/name.git，可能带开头的 /）转换为 repoKey 形式；不是这种形式时返回空。 */
export function repoOfPath(p: string): string {
  const m = /^\/?([^/]+\/[^/]+?)(?:\.git)?\/?$/i.exec(p);
  return m?.[1] ? repoKey(m[1]) : '';
}

/** globMeta 是 GOPRIVATE 模式（Go 的 path.Match）中的特殊字符，原样出现时用反斜杠转义。 */
const globMeta = /[*?[\]\\]/g;

/**
 * goPrivatePattern 返回依赖仓库 owner/name 在 GOPRIVATE 中的模式（host/owner/name）。GOPRIVATE 按 path.Match 匹配模块
 * 路径的前缀，区分大小写；GitHub 的仓库名不区分（见 repoKey），go.mod 中的模块路径可能与声明的大小写不同（也可能合法地
 * 含大写），所以 owner/name 中的每个字母写成大小写字符类（Acme → [aA][cC][mM][eE]），其它字符按字面。主机名按小写写出：
 * Go 要求模块路径的第一段是小写（大写的主机名不是合法的模块路径），URL 解析出的主机名也已经是小写。
 */
export function goPrivatePattern(host: string, repo: string): string {
  const literal = (t: string) => t.replace(globMeta, (c) => `\\${c}`);
  const anyCase = [...repo].map((c) => (/[a-z]/i.test(c) ? `[${c.toLowerCase()}${c.toUpperCase()}]` : literal(c))).join('');
  return `${literal(host.toLowerCase())}/${anyCase}`;
}

/** execGitOf 返回任务的 exec 中 git 用的设置：仓库地址不是 HTTP(S) 时为 null（没有可签发的凭据）。 */
export function execGitOf(cloneURL: string, repositories: readonly string[], execRoot: string): ExecGit | null {
  const u = httpURL(cloneURL);
  if (!u) return null;
  return { helperURL: helperScope(cloneURL), goprivate: repositories.map((r) => goPrivatePattern(u.host, r)), execRoot };
}

/** httpURL 解析 HTTP(S) 地址，不是时返回 undefined。 */
function httpURL(s: string): URL | undefined {
  let u: URL;
  try {
    u = new URL(s);
  } catch {
    return undefined;
  }
  return u.protocol === 'https:' || u.protocol === 'http:' ? u : undefined;
}

/**
 * credentialAllowed 判断 helper 请求是不是 repos 中的某个仓库（与任务仓库同一个协议和主机），返回这个仓库（按 repos 中的
 * 写法），不是时返回空。
 */
export function credentialAllowed(q: CredentialQuery, cloneURL: string, repos: readonly string[]): string {
  const u = httpURL(cloneURL);
  if (!u) return '';
  if (`${q.protocol}:` !== u.protocol || normalHost(q.protocol, q.host) !== u.host) return '';
  const repo = repoOfPath(q.path);
  return repo === '' ? '' : (repos.find((r) => repoKey(r) === repo) ?? '');
}

/**
 * normalHost 把 helper 请求中的主机（git 原样给出，可能带默认端口、大写，例如 GitHub.COM:443）归一化为与 URL.host 相同的
 * 形式（小写、去掉协议的默认端口）；不合法时返回空。
 */
function normalHost(protocol: string, host: string): string {
  if (!host || /[/?#@\\]/.test(host)) return '';
  try {
    return new URL(`${protocol}://${host}`).host;
  } catch {
    return '';
  }
}

/**
 * transportRepo 返回一次 git 传输的地址（insteadOf 改写之后）访问的仓库：任务仓库或声明的依赖仓库之一（按声明的写法）；
 * 不是这些仓库、不是同一个协议和主机、地址中带用户名或密码、路径中有百分号编码或 . / .. 段（见 safeRepoPath）时返回空
 * （这次传输不给凭据）。
 */
export function transportRepo(url: string, cloneURL: string, repos: readonly string[]): string {
  const u = httpURL(url);
  if (!u || u.username || u.password || !safeRepoPath(url)) return '';
  return credentialAllowed({ protocol: u.protocol.slice(0, -1), host: u.host, path: u.pathname }, cloneURL, repos);
}

/** safeRepoPath 判断原始地址的路径中没有百分号编码和 . / .. 段（WHATWG URL 会把它们归一化掉，git、curl 的处理不同）。 */
export function safeRepoPath(raw: string): boolean {
  const p = /^[a-z][a-z0-9+.-]*:\/\/[^/?#]*([^?#]*)/i.exec(raw)?.[1] ?? '';
  return !p.includes('%') && !p.includes('\\') && !/(^|\/)\.\.?(\/|$)/.test(p);
}

