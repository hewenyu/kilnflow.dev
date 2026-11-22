import type { Exec } from './exec.js';
import { tail } from './exec.js';

// Orca 的接口（PLAN #33.4）：只用有文档的 `orca … --json` 命令，不读 Orca 的数据库、不解析终端文字。
// orchestration 命令要求调用方是一个 Orca 终端：连接器在 Orca 外运行（launchd / systemd），所以自己建一个终端作为邮箱，
// 用 --from <邮箱> 发出这些命令（M0 实测，没有时报 no_active_sender_terminal）。

export interface OrcaStatus {
  ready: boolean;
  reason: string;
  version: string;
  runtimeId: string;
}

export interface WorkerStartOptions {
  from: string;
  run: string;
  /** 调用方预先生成的 UUID：响应丢失时用 requestShow 查证，不再启动一份。 */
  requestId: string;
  spec: string;
  title: string;
  repoId: string;
  name: string;
  baseBranch: string;
  comment: string;
  /** Orca 的 Agent id（codex、claude）。 */
  agent: string;
}

export interface WorkerRef {
  runId: string;
  taskId: string;
  dispatchId: string;
  /** Orca 建的 worktree 的路径（回执中有时才有）。 */
  worktreePath: string;
}

export type RequestState = 'completed' | 'pending' | 'failed' | 'absent';

export interface RequestInfo {
  state: RequestState;
  /** completed 时的回执。 */
  worker?: WorkerRef;
  error?: string;
  /** failed 时 Orca 已经建了的 dispatch（可能留下了终端，要 worker-release 回收）；没有时为空。 */
  dispatchId?: string;
}

/**
 * OrcaMessage 是邮箱收到的一条 orchestration 消息。payload 中只取连接器用得上的规范化字符串字段（缺失或者不是字符串时为
 * 空），不保留其余的未知字段。
 */
export interface OrcaMessage {
  id: string;
  type: string;
  subject: string;
  body: string;
  dispatchId: string;
  outcome: string;
  reportPath: string;
  /**
   * Orca 拒绝了这条消息时的原因（payload 的 _orcaLifecycleRejection）：被拒绝的 worker_done 仍以 worker_done 投递，但没有
   * 结束 dispatch（例如 task id 不对、发送者不是这个 dispatch 的 Agent）。没有被拒绝时为空。
   */
  rejected: string;
  /**
   * Orca 对 Agent 这一轮结果的判定（payload.turnOutcome，例如 failure）：这是通用字段，与具体 Agent 无关。Agent 运行中断的
   * escalation 带 failure；普通的提问、主动升级没有它。
   */
  turnOutcome: string;
  /**
   * Orca 报告的系统停止原因（payload.stopReason，例如 needs-input、exited、lost、finished）。finished 只是这一轮结束
   * （Agent 可能在等后台命令），不代表任务做不下去。
   */
  stopReason: string;
  /** Agent 私有的失败种类（payload.failureKind，例如 notSignedIn）：只作诊断，不作分类或退避依据。 */
  failureKind: string;
  /** Orca 报告的 Agent 错误原文（payload.error）。 */
  error: string;
  /** Orca 报告的 Agent 错误细节（payload.detail）。 */
  detail: string;
}

/**
 * CheckRequest 是一次消费型 check 的全部输入。Orca 按 requestId 记下这次 check 返回的一批（返回即已处理）；用同一个
 * requestId 和完全相同的输入重发会重放同一批（输入不同时报 request_mismatch），所以输入要和 requestId 一起保存。
 */
export interface CheckRequest {
  requestId: string;
  terminal: string;
  run: string;
  waitMs: number;
}

export interface CheckResult {
  messages: OrcaMessage[];
  /** 等待被打断（调用方断开、取消）：Orca 没有交出任何消息，这个 requestId 不会再带来消息。 */
  interrupted: boolean;
}

export interface WorkerInfo {
  /** Orca 的 dispatch 阶段：dispatched、completed、failed 等。 */
  dispatch: string;
  /** worker 的结果：succeeded、failed、stopped 等（未结束时为空）。 */
  outcome: string;
  /** 终端（PTY）是否存活：live、exited、unknown。 */
  terminal: string;
  worktreePath: string;
}

export interface Orca {
  status(): Promise<OrcaStatus>;
  /** 把 path 注册为 Orca 的仓库（已注册时返回原来的 id）。 */
  repoAdd(path: string): Promise<string>;
  terminalCreate(worktreePath: string, title: string): Promise<string>;
  /**
   * 查询一个终端（terminal show）。Orca 明确回答没有这个终端（句柄失效：Orca 重启过、不是这个 Orca 发的）时为 undefined；
   * 查询失败（连不上 Orca、结果未知、Orca 的其它回答）时抛出。能不能用由 terminalUsable 判断。
   */
  terminalShow(handle: string): Promise<TerminalInfo | undefined>;
  /**
   * 关闭一个终端（terminal close：结束它的进程，去掉界面上的标签）；已经关闭的再关也成功。Orca 没有这个终端时返回 false，
   * 其它失败抛出。
   */
  terminalClose(handle: string): Promise<boolean>;
  runCreate(from: string, objective: string): Promise<string>;
  /** 让 from（重建的邮箱终端）接管 Run：Run 只能由绑定它的终端读取。 */
  runUse(from: string, runId: string): Promise<void>;
  workerStart(o: WorkerStartOptions): Promise<WorkerRef>;
  requestShow(requestId: string): Promise<RequestInfo>;
  /**
   * 等待邮箱在 run 中的消息（最多 waitMs），返回最早的一批未读消息并标记为已处理；没有消息时 messages 为空。用同一个
   * CheckRequest 重发时返回同一批（响应丢失、进程崩溃后取回）。
   */
  check(req: CheckRequest, signal?: AbortSignal): Promise<CheckResult>;
  /** 查询 dispatch；Orca 没有这个 dispatch 时抛出 dispatchNotFound 的 OrcaError。 */
  workerShow(dispatchId: string): Promise<WorkerInfo>;
  /** 停止 dispatch 的 Agent，返回是否确认已经停止（或者早已结束；Orca 没有这个 dispatch 时没有可停的，同样为 true）。 */
  workerStop(dispatchId: string): Promise<boolean>;
  /**
   * 释放已经结束（settled）的 dispatch 的终端（只关终端，不删工作区），返回 Orca 确认的结果；结果未知（release_unknown、
   * release_pending、命令没有正常完成）时抛出不是 isDefinite 的 OrcaError，Orca 明确拒绝时抛出 isDefinite 的 OrcaError。
   */
  workerRelease(dispatchId: string): Promise<ReleaseState>;
  /** 回复 Agent 的提问。 */
  reply(from: string, messageId: string, body: string): Promise<void>;
  /** 列出 Orca 中注册的仓库（repo list）。 */
  repoList(): Promise<RepoInfo[]>;
  /**
   * 列出 Orca 中仓库 repoId 的全部工作区（worktree list --repo id:<repoId> --limit 10000）。Orca 报告列表被截断（truncated）
   * 时抛出 list_truncated：不完整的列表不能用来判断"已经删除"。
   */
  worktreeList(repoId: string): Promise<WorktreeInfo[]>;
  /**
   * 删除 Orca 中的一个 Git worktree（worktree rm --worktree id:<id>，不带 --run-hooks、--delete-branch；force 时带 --force）。
   * 返回 removed（Orca 确认删除了）或 not_found（Orca 没有匹配的工作区）；其余都抛出 OrcaError（工作区不干净时 Orca 以
   * runtime_error 拒绝，见 dirtyRefusal）。
   */
  worktreeRemove(id: string, force: boolean): Promise<'removed' | 'not_found'>;
  /**
   * 列出工作区中的终端（terminal list --worktree <worktree>，worktree 是 Orca 的工作区选择器，例如 id:<repoId>::<路径>、
   * path:<路径>）。Orca 只列出进程还在的终端，包括没有界面窗格的（orphaned）；列表被截断时抛出 list_truncated。
   */
  terminalList(worktree: string): Promise<TerminalInfo[]>;
  /**
   * 列出全部 Run 中终端还没有释放的 worker：包括 structured 模式的 Agent，它们不在 terminal list 中。按 run-list 翻完全部
   * Run，对每个 Run 用 worker-list --run（不按状态过滤）翻完全部 dispatch，在本地排除终端已经释放的（见 OrcaCli 的实现）。
   * 不能确认列表完整时抛出：某个回执的作用域不是请求的 Run、某次查询失败（包括 Orca 的翻页游标失效）、页数超过上限、
   * 输出结构不认识、列出的条数与 page.total 不一致、查询期间新建了 Run。
   */
  unreleasedWorkers(): Promise<UnreleasedWorker[]>;
}

/** RepoInfo 是 Orca 中注册的一个仓库（repo list 的 repos[]）。kind 是 git 或 folder。 */
export interface RepoInfo {
  id: string;
  path: string;
  kind: string;
}

/**
 * WorktreeInfo 是 Orca 中的一个工作区（worktree list 的 worktrees[]）。id 是完整 id（Git worktree 是 <repoId>::<路径>），
 * worktree rm 用 id:<它> 精确选择；git 表示它是 Git worktree（有 git 字段；folder workspace 没有）；branch 是检出的分支
 * （refs/heads/…，分离 HEAD 时为空）；comment 是 Orca 元数据中的备注（连接器建的工作区见 reclaim.ts 的 attemptComment）；
 * lastActivityAt 是 Orca 记录的最后活动时间（毫秒，没有时为 0）。
 */
export interface WorktreeInfo {
  id: string;
  repoId: string;
  path: string;
  branch: string;
  comment: string;
  git: boolean;
  isMainWorktree: boolean;
  isBare: boolean;
  lastActivityAt: number;
}

/**
 * UnreleasedWorker 是 Orca 中终端还没有释放的一个 worker（worker-list 的 workers[]）。workspace 是它的工作区 id
 * （<repoId>::<路径>）；terminalState 为空：Orca 还没有为这个没有了结的 dispatch 建终端（之后可能建）。
 */
export interface UnreleasedWorker {
  dispatchId: string;
  terminalState: string;
  liveness: string;
  workspace: string;
}

/** releasedTerminalState 是 worker-list 的 terminalState 中终端已经释放的状态（Orca 中释放之后不会再变，见 unreleasedWorkers）。 */
const releasedTerminalState = 'released';
/** settledDispatchStatuses 是 Orca 中已经了结的 dispatch 状态：没有终端（terminalState 为 null）的这样的 dispatch 不会再建终端。 */
const settledDispatchStatuses: ReadonlySet<string> = new Set(['completed', 'failed', 'circuit_broken']);
/**
 * workerPageLimit 是 worker-list 每页的条数（Orca 的上限）；maxWorkerPages 是每个 Run 最多翻的页数（即每个 Run 最多
 * 200 × 100 = 2 万个 dispatch），超过时按失败处理（不回收）。连接器每启动一次 Agent 在邮箱的 Run 中加一个 dispatch，Orca
 * 不清理单个 dispatch；本机实测每页约 0.12 秒，翻满上限约 25 秒（PLAN #33.10）。
 */
const workerPageLimit = '100';
const maxWorkerPages = 200;
/** runPageLimit 是 run-list 每页的条数（Orca 的上限）；maxRunPages 是最多翻的页数，超过时按失败处理。 */
const runPageLimit = '100';
const maxRunPages = 50;

/**
 * TerminalInfo 是 Orca 中的一个终端（terminal show 的 terminal、terminal list 的 terminals[]）。Orca 1.4.258 中这几个字段的
 * 含义（宿主的 buildPtyTerminalSummary 与实测，PLAN #33.4"邮箱终端的存活"）：
 * - connected：终端的进程（PTY）还在。shell 退出、被关闭（terminal close、在界面上关掉标签）之后为 false，句柄仍查得到。
 * - orphaned：这个进程没有挂在界面的窗格上（后台终端：没有窗口、界面没有接管它，或者界面为它的标签另起了一个终端）。
 *   只说明界面上看不到它，与能不能用无关：这样的终端照样能 run-use、check（生产节点上见过，Orca 照常接受它作为调用方）。
 * - exitCause：进程结束的原因（exitCause.kind，例如 operator_close、unknown），进程还在时为空。
 */
export interface TerminalInfo {
  handle: string;
  title: string;
  worktreePath: string;
  connected: boolean;
  orphaned: boolean;
  exitCause: string;
}

/**
 * terminalUsable 是"终端还能用"的唯一规则：Orca 查得到它，进程还在（connected）。没有界面窗格（orphaned）不算不能用：
 * 把它当作不在会让邮箱每次检查都新建一个终端，旧的还连着、一直留在 Orca 中。
 */
export function terminalUsable(t: TerminalInfo | undefined): boolean {
  return t?.connected === true;
}

/** terminalGoneCodes 是 terminal show、terminal close 中表示 Orca 没有这个终端（句柄失效）的错误码（夹具 terminal-show-stale.json）。 */
const terminalGoneCodes: readonly string[] = ['terminal_handle_stale', 'terminal_not_found'];

/** terminalGone 判断 err 是不是 Orca 明确回答没有这个终端。 */
function terminalGone(err: unknown): boolean {
  return err instanceof OrcaError && terminalGoneCodes.includes(err.code);
}

/** terminalInfo 解析 terminal show / terminal list 中的一个终端；handle、connected 不对时按输出无法解析处理。 */
function terminalInfo(cmd: string, v: unknown, field: string): TerminalInfo {
  const t = obj(v);
  if (typeof t.connected !== 'boolean') throw badOutput(cmd, `${field}.connected`);
  return {
    handle: needStr(cmd, t, 'handle', `${field}.handle`),
    title: str(t.title),
    worktreePath: str(t.worktreePath),
    connected: t.connected,
    orphaned: t.orphaned === true,
    exitCause: str(obj(t.exitCause).kind),
  };
}

/**
 * ReleaseState 是 worker-release 确认的结果：released（这次释放）、already_released（已经释放过）、retained（Orca 不替
 * 连接器关闭这个终端，例如没有归它管的终端，重试也不会变）、not_found（Orca 没有这个 dispatch，没有要释放的）。
 */
export type ReleaseState = 'released' | 'already_released' | 'retained' | 'not_found';

/** OrcaError 是 orca 返回的错误（code 是 Orca 的 error.code）。 */
export class OrcaError extends Error {
  constructor(
    readonly code: string,
    message: string,
  ) {
    super(`orca：${code}：${message}`);
    this.name = 'OrcaError';
  }
}

/** StartFailedError 是 worker-start 回执中 Orca 明确报告的启动失败；dispatchId 是 Orca 已经建了的 dispatch（没有时为空）。 */
export class StartFailedError extends OrcaError {
  constructor(
    message: string,
    readonly dispatchId: string,
  ) {
    super('start_failed', message);
    this.name = 'StartFailedError';
  }
}

type Json = Record<string, unknown>;

/**
 * minOrcaVersion 是连接器支持的最低 Orca 版本（PLAN #33.4）：连接器按这个版本的 `--json --full` 输出解析，并依赖它的 check
 * 语义（返回即已处理、--retry-request 重放同一批）。更低的版本不领取任务。
 */
export const minOrcaVersion = '1.4.258';

/** 邮箱等待的消息类型：worker_done 是 Agent 结束了这一轮，question / escalation 要回复。 */
const checkTypes = 'worker_done,escalation,question';

/**
 * unknownOutcomeCodes 是结果未知的 OrcaError：命令没有正常执行完、没有输出或输出无法解析（请求可能已经生效），以及
 * worker-release 报告的 release_unknown（不能证明终端已经关闭）、release_pending（释放还在进行）。
 */
export const unknownOutcomeCodes: readonly string[] = ['exec_failed', 'empty_output', 'bad_output', 'release_unknown', 'release_pending'];

/** listLimit 是 worktree list、terminal list 的 --limit：远大于连接器会建的数量；Orca 仍报告截断时按失败处理。 */
const listLimit = '10000';
/** worktreeNotFoundCode 是 Orca 没有匹配的工作区时的错误码（夹具 worktree-rm-not-found.json）。 */
const worktreeNotFoundCode = 'selector_not_found';

/**
 * dirtyRefusal 判断 err 是不是 Orca 因为工作区不干净（有未提交的改动、未跟踪的文件）而拒绝不带 --force 的 worktree rm
 * （夹具 worktree-rm-dirty.json：runtime_error，"Failed to delete worktree at <路径>. <git status --porcelain>"）。只认这
 * 一种文字；带 --force 时的失败（"Failed to force delete…"，例如 git 锁定）、hook 失败（worktree_archive_hook_failed）、
 * 连不上 Orca 等都不是。调用方还要自己核对工作区确实不干净（见 reclaim.ts）。
 */
export function dirtyRefusal(err: unknown): boolean {
  return err instanceof OrcaError && err.code === 'runtime_error' && err.message.includes('Failed to delete worktree at ');
}

/** releaseDoneStates 是 worker-release 的输出中表示已经确认的 state（见 ReleaseState，not_found 来自错误码）。 */
const releaseDoneStates: readonly string[] = ['released', 'already_released', 'retained'];
/** notFoundCodes 是 Orca 没有这个 dispatch 时的错误码。 */
const notFoundCodes: readonly string[] = ['dispatch_not_found', 'not_found'];

/**
 * dispatchNotFound 判断 err 是不是 Orca 报告没有这个 dispatch（夹具 worker-show-not-found.json、worker-stop-not-found.json、
 * worker-release-not-found.json）。连接器只按 Orca 回执中的 dispatch 操作，Orca 没有它只能是 Orca 的编排状态被重置了。
 */
export function dispatchNotFound(err: unknown): boolean {
  return err instanceof OrcaError && notFoundCodes.includes(err.code);
}

/** parseVersion 取版本号开头的数字部分（1.4.258、1.4.258-beta.1 → [1, 4, 258]），不是版本号时为 undefined。 */
function parseVersion(v: string): number[] | undefined {
  const m = /^v?(\d+(?:\.\d+)*)/.exec(v.trim());
  return m?.[1]?.split('.').map(Number);
}

/** compareVersions 按数字逐段比较版本号（缺的段按 0）；a < b 时为负、相等为 0、a > b 为正。不是版本号时抛出。 */
export function compareVersions(a: string, b: string): number {
  const pa = parseVersion(a);
  const pb = parseVersion(b);
  if (!pa || !pb) throw new Error(`无法比较版本号 ${a} 与 ${b}`);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const d = (pa[i] ?? 0) - (pb[i] ?? 0);
    if (d !== 0) return d;
  }
  return 0;
}

/** versionProblem 检查 Orca 的版本，满足最低版本时返回 undefined，否则返回原因。 */
export function versionProblem(version: string): string | undefined {
  if (!parseVersion(version)) return `无法识别 Orca 的版本（${version || '没有返回版本'}），连接器要求 Orca ${minOrcaVersion} 或更高`;
  if (compareVersions(version, minOrcaVersion) < 0) return `Orca 版本 ${version} 低于连接器支持的最低版本 ${minOrcaVersion}，请升级 Orca`;
  return undefined;
}

/** isDefinite 判断 err 是不是 Orca 明确返回的错误（带错误码的 ok:false 响应），而不是结果未知。 */
export function isDefinite(err: unknown): err is OrcaError {
  return err instanceof OrcaError && !unknownOutcomeCodes.includes(err.code);
}

/** isRefusal 判断 err 是不是 Orca 对这个请求本身的明确拒绝（重试也不会变）：isDefinite，且不是 Orca 不可达（runtime_unavailable）。 */
export function isRefusal(err: unknown): err is OrcaError {
  return isDefinite(err) && err.code !== 'runtime_unavailable';
}

/**
 * ReleaseOutcome 是一次释放的结论（Attempt 和 Node 共用，见 releaseWorker）：done 已经确认（state 是 Orca 的回答）；
 * refused 是 Orca 明确拒绝（例如 dispatch 还没有结束：dispatch_inactive），重试也不会变，不再重试；unknown 是结果未知或
 * 连不上 Orca，稍后重试。
 */
export type ReleaseOutcome = { kind: 'done'; state: ReleaseState } | { kind: 'refused'; error: string } | { kind: 'unknown'; error: string };

/** releaseWorker 释放 dispatch 的终端并归类结果（不抛出）。 */
export async function releaseWorker(orca: Orca, dispatchId: string): Promise<ReleaseOutcome> {
  try {
    return { kind: 'done', state: await orca.workerRelease(dispatchId) };
  } catch (err) {
    return isRefusal(err) ? { kind: 'refused', error: err.message } : { kind: 'unknown', error: err instanceof Error ? err.message : String(err) };
  }
}

/** releaseRefusedFix 是 Orca 拒绝释放时的处理方法（日志中给人看）。 */
export const releaseRefusedFix =
  '连接器不再重试：在 Orca 中用 orca orchestration worker-show --dispatch <dispatch> --json 查看，Agent 确实已经结束时先 worker-stop 再 worker-release，或在 Orca 中关闭它的终端';

function obj(v: unknown): Json {
  return v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Json) : {};
}
function isObj(v: unknown): v is Json {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}
function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

/** badOutput 是某条命令的输出缺少必需的字段（Orca 改了输出格式）。 */
function badOutput(cmd: string, field: string): OrcaError {
  return new OrcaError('bad_output', `orca ${cmd} 的输出缺少 ${field}（连接器按 Orca ${minOrcaVersion} 的 --full 输出解析）`);
}
/** need 取必需的非空字符串字段。 */
function needStr(cmd: string, o: Json, key: string, field = key): string {
  const v = str(o[key]);
  if (!v) throw badOutput(cmd, field);
  return v;
}
/** needObj 取必需的对象字段。 */
function needObj(cmd: string, o: Json, key: string, field = key): Json {
  const v = o[key];
  if (!isObj(v)) throw badOutput(cmd, field);
  return v;
}

/** parseOutput 解析 orca --json 的输出：check --wait 可能先输出若干行 keepalive（旧版本写到 stdout），最后是结果。 */
export function parseOutput(stdout: string): Json {
  const lines = stdout.split('\n').filter((l) => !l.startsWith('{"_keepalive"'));
  const text = lines.join('\n').trim();
  if (!text) throw new OrcaError('empty_output', 'orca 没有输出');
  let v: unknown;
  try {
    v = JSON.parse(text);
  } catch {
    throw new OrcaError('bad_output', `无法解析 orca 的输出：${tail(text, 500)}`);
  }
  const o = obj(v);
  if (o.ok !== true) {
    const e = obj(o.error);
    throw new OrcaError(str(e.code) || 'unknown', str(e.message) || tail(text, 500));
  }
  return obj(o.result);
}

/** worktreePathFromId 从 Orca 的 worktree id（<repo id>::<path>）取出路径。 */
export function worktreePathFromId(id: string): string {
  const i = id.indexOf('::');
  return i < 0 ? '' : id.slice(i + 2);
}

/**
 * StartReceipt 是 worker-start 的回执（worker-start 的输出、request-show 中 completed 的 receipt）：started 时 Orca 建了
 * dispatch（ready，或者 Agent 可能还在启动、Orca 自己也不确定）；failed 时 Orca 明确报告启动失败。
 */
type StartReceipt = { kind: 'started'; ref: WorkerRef } | { kind: 'failed'; error: string; dispatchId: string };

/** startFailedStates：worker-start 回执中 Orca 明确表示没有启动起来的状态。 */
const startFailedStates: readonly string[] = ['failed', 'stopped', 'abandoned'];

function startReceipt(cmd: string, r: Json): StartReceipt {
  const state = needStr(cmd, r, 'state');
  if (startFailedStates.includes(state)) {
    const stage = str(r.failedStage);
    return { kind: 'failed', error: `${stage ? stage + '：' : ''}${str(r.lastError) || state}`, dispatchId: str(r.dispatchId) };
  }
  let worktreePath = '';
  for (const e of Array.isArray(r.effects) ? r.effects : []) {
    const eo = obj(e);
    if (eo.kind === 'worktree') worktreePath = worktreePathFromId(str(eo.id));
  }
  const runId = str(r.runId) || str(obj(r.run).id);
  if (!runId) throw badOutput(cmd, 'runId');
  return { kind: 'started', ref: { runId, taskId: needStr(cmd, r, 'taskId'), dispatchId: needStr(cmd, r, 'dispatchId'), worktreePath } };
}

/** payloadOf 取消息的 payload：Orca 的完整记录中是 JSON 字符串，精简视图中是对象。 */
function payloadOf(v: unknown): Json {
  if (isObj(v)) return v;
  if (typeof v !== 'string' || !v) return {};
  try {
    return obj(JSON.parse(v));
  } catch {
    return {};
  }
}

/** rejectedMark 是非标准形状的 _orcaLifecycleRejection 的说明：这样的值同样算拒绝。 */
const rejectedMark = '被 Orca 拒绝';

/**
 * rejectionOf 解析消息 payload 中的 _orcaLifecycleRejection：没有拒绝时为空。Orca 拒绝一条消息时把它放在这里——完整记录中是
 * 带 reason / code 的对象，精简视图中是原因字符串。非空但不是这两种形状的值（true、数字、数组等）同样算拒绝，原因用固定的
 * 说明：形状不认识不能当成没有拒绝，否则被 Orca 拒绝的 worker_done 会被误当作结束。
 */
function rejectionOf(v: unknown): string {
  if (v === undefined || v === null || v === false || v === '') return '';
  if (typeof v === 'string') return v;
  if (isObj(v)) return str(v.reason) || str(v.code) || rejectedMark;
  return rejectedMark;
}

function message(m: unknown): OrcaMessage {
  const o = obj(m);
  const payload = payloadOf(o.payload);
  return {
    id: needStr('orchestration check', o, 'id', 'messages[].id'),
    type: needStr('orchestration check', o, 'type', 'messages[].type'),
    subject: str(o.subject),
    body: str(o.body),
    dispatchId: str(payload.dispatchId),
    outcome: str(payload.outcome),
    reportPath: str(payload.reportPath),
    rejected: rejectionOf(payload._orcaLifecycleRejection),
    turnOutcome: str(payload.turnOutcome),
    stopReason: str(payload.stopReason),
    failureKind: str(payload.failureKind),
    error: str(payload.error),
    detail: str(payload.detail),
  };
}

/**
 * OrcaCli 通过 orca 命令行操作 Orca。orchestration 命令都带 --full，按完整记录解析（精简视图的字段随版本变化）。
 * bin 是 config.json 的 orca（名字或路径），按 spawn 的规则执行。本机执行不了它（找不到、没有权限、被信号结束、超时）时
 * 是 exec_failed——结果未知（unknownOutcomeCodes），不是 Orca 的回答：任何路径上都不会被当成 dispatch 不存在、已经停止、
 * 已经释放或者没有这个请求。
 */
export class OrcaCli implements Orca {
  constructor(
    private readonly exec: Exec,
    private readonly bin: string,
    private readonly timeoutMs = 120_000,
  ) {}

  private async call(args: string[], opts: { timeoutMs?: number; signal?: AbortSignal } = {}): Promise<Json> {
    const full = args[0] === 'orchestration' ? ['--full'] : [];
    const r = await this.exec(this.bin, [...args, '--json', ...full], {
      timeoutMs: opts.timeoutMs ?? this.timeoutMs,
      signal: opts.signal,
      // 连接器用 --from / --terminal 指明自己的邮箱终端；从 Agent 会话中启动时继承的会话身份会让 Orca 拒绝（consumer_fenced）
      env: { ORCA_AGENT_SESSION_ID: undefined },
    });
    if (r.code !== 0 && !r.stdout.trim()) throw new OrcaError('exec_failed', tail(r.stderr.trim(), 1000) || `退出码 ${r.code}`);
    return parseOutput(r.stdout);
  }

  async status(): Promise<OrcaStatus> {
    try {
      const rt = needObj('status', await this.call(['status'], { timeoutMs: 30_000 }), 'runtime');
      const version = str(rt.appVersion);
      const runtimeId = str(rt.runtimeId);
      if (rt.state !== 'ready' || rt.reachable === false) return { ready: false, reason: `Orca runtime 状态为 ${str(rt.state) || '未知'}`, version, runtimeId };
      const problem = versionProblem(version);
      return { ready: !problem, reason: problem ?? '', version, runtimeId };
    } catch (err) {
      return { ready: false, reason: err instanceof Error ? err.message : String(err), version: '', runtimeId: '' };
    }
  }

  async repoAdd(path: string): Promise<string> {
    const list = await this.call(['repo', 'list']);
    if (!Array.isArray(list.repos)) throw badOutput('repo list', 'repos');
    for (const r of list.repos) {
      const ro = obj(r);
      if (str(ro.path) === path) return needStr('repo list', ro, 'id', 'repos[].id');
    }
    // 已经注册时（例如路径经过符号链接，list 中是解析后的路径）repo add 返回原来的记录
    return needStr('repo add', needObj('repo add', await this.call(['repo', 'add', '--path', path]), 'repo'), 'id', 'repo.id');
  }

  async terminalCreate(worktreePath: string, title: string): Promise<string> {
    const r = await this.call(['terminal', 'create', '--worktree', `path:${worktreePath}`, '--title', title]);
    return needStr('terminal create', needObj('terminal create', r, 'terminal'), 'handle', 'terminal.handle');
  }

  async terminalShow(handle: string): Promise<TerminalInfo | undefined> {
    let r: Json;
    try {
      r = await this.call(['terminal', 'show', '--terminal', handle]);
    } catch (err) {
      // 只有 Orca 明确回答没有这个终端才算不在；连不上、结果未知和其它回答都不能断定（不重建邮箱）
      if (terminalGone(err)) return undefined;
      throw err;
    }
    // 关闭了的终端仍然查得到（connected:false、exitCause），见 terminalUsable
    return terminalInfo('terminal show', needObj('terminal show', r, 'terminal'), 'terminal');
  }

  async terminalClose(handle: string): Promise<boolean> {
    try {
      needObj('terminal close', await this.call(['terminal', 'close', '--terminal', handle]), 'close');
      return true;
    } catch (err) {
      if (terminalGone(err)) return false;
      throw err;
    }
  }

  async runCreate(from: string, objective: string): Promise<string> {
    const r = await this.call(['orchestration', 'run-create', '--from', from, '--objective', objective]);
    return needStr('orchestration run-create', needObj('orchestration run-create', r, 'run'), 'id', 'run.id');
  }

  async runUse(from: string, runId: string): Promise<void> {
    const r = await this.call(['orchestration', 'run-use', '--id', runId, '--from', from]);
    const run = needObj('orchestration run-use', r, 'run');
    if (str(run.id) !== runId) throw badOutput('orchestration run-use', `run.id（${runId}）`);
  }

  async workerStart(o: WorkerStartOptions): Promise<WorkerRef> {
    const r = await this.call(
      [
        'orchestration', 'worker-start', '--from', o.from, '--run', o.run, '--retry-request', o.requestId,
        '--spec', o.spec, '--task-title', o.title, '--worktree', 'new-top-level', '--repo', `id:${o.repoId}`,
        '--name', o.name, '--base-branch', o.baseBranch, '--comment', o.comment, '--agent', o.agent,
      ],
      { timeoutMs: 10 * 60_000 },
    );
    const rc = startReceipt('orchestration worker-start', r);
    if (rc.kind === 'failed') throw new StartFailedError(rc.error, rc.dispatchId);
    return rc.ref;
  }

  async requestShow(requestId: string): Promise<RequestInfo> {
    const cmd = 'orchestration request-show';
    let r: Json;
    try {
      r = await this.call(['orchestration', 'request-show', '--request', requestId]);
    } catch (err) {
      if (err instanceof OrcaError && (err.code === 'not_found' || err.code === 'request_not_found')) return { state: 'absent' };
      throw err;
    }
    const state = needStr(cmd, r, 'state');
    if (state === 'completed') {
      const rc = startReceipt(cmd, needObj(cmd, r, 'receipt'));
      return rc.kind === 'started' ? { state, worker: rc.ref } : { state: 'failed', error: rc.error, dispatchId: rc.dispatchId };
    }
    if (state === 'pending' || state === 'absent') return { state };
    if (state === 'failed') return { state, error: str(obj(r.error).message) || str(r.error) || str(r.interpretation) };
    throw new OrcaError('bad_output', `orca ${cmd} 返回了未知的状态 ${state}`);
  }

  async check(req: CheckRequest, signal?: AbortSignal): Promise<CheckResult> {
    // 显式指定 Run（Run 绑定在邮箱终端上）；参数只由 req 决定：重放时与第一次完全相同
    const args = ['orchestration', 'check', '--terminal', req.terminal, '--run', req.run, '--wait', '--types', checkTypes, '--timeout-ms', String(req.waitMs), '--retry-request', req.requestId];
    const cmd = 'orchestration check';
    const r = await this.call(args, { timeoutMs: req.waitMs + 60_000, signal });
    if (!Array.isArray(r.messages)) throw badOutput(cmd, 'messages');
    return { messages: r.messages.map(message), interrupted: r.cancelled === true || r.connectionLost === true };
  }

  async workerShow(dispatchId: string): Promise<WorkerInfo> {
    const cmd = 'orchestration worker-show';
    const r = await this.call(['orchestration', 'worker-show', '--dispatch', dispatchId]);
    const p = needObj(cmd, r, 'projection');
    return {
      dispatch: needStr(cmd, needObj(cmd, p, 'stage', 'projection.stage'), 'dispatch', 'projection.stage.dispatch'),
      outcome: str(p.outcome),
      terminal: str(obj(r.observation).status) || 'unknown',
      worktreePath: worktreePathFromId(str(obj(p.workspace).id)),
    };
  }

  async workerStop(dispatchId: string): Promise<boolean> {
    let r: Json;
    try {
      r = await this.call(['orchestration', 'worker-stop', '--dispatch', dispatchId]);
    } catch (err) {
      // Orca 没有这个 dispatch（编排状态被重置）：Orca 中没有可停的，再停也不会变
      if (dispatchNotFound(err)) return true;
      throw err;
    }
    // stop_unknown（不能证明 Agent 已经退出）时 orca 以 1 退出，输出仍是 ok:true 的记录
    return r.alreadySettled === true || needStr('orchestration worker-stop', r, 'state') === 'stopped';
  }

  async workerRelease(dispatchId: string): Promise<ReleaseState> {
    const cmd = 'orchestration worker-release';
    let r: Json;
    try {
      r = await this.call(['orchestration', 'worker-release', '--dispatch', dispatchId]);
    } catch (err) {
      if (dispatchNotFound(err)) return 'not_found';
      throw err;
    }
    const state = needStr(cmd, r, 'state');
    if (releaseDoneStates.includes(state)) return state as ReleaseState;
    // release_unknown（以 1 退出）、release_pending 以及不认识的 state：不能确认终端已经关闭，按结果未知处理（稍后重试）
    const code = state === 'release_pending' ? state : 'release_unknown';
    throw new OrcaError(code, str(r.lastError) || `${state}：无法确认 ${dispatchId} 的终端已经关闭`);
  }

  async reply(from: string, messageId: string, body: string): Promise<void> {
    await this.call(['orchestration', 'reply', '--from', from, '--id', messageId, '--body', body]);
  }

  async repoList(): Promise<RepoInfo[]> {
    const list = await this.call(['repo', 'list']);
    if (!Array.isArray(list.repos)) throw badOutput('repo list', 'repos');
    return list.repos.map((r) => {
      const ro = obj(r);
      return { id: needStr('repo list', ro, 'id', 'repos[].id'), path: needStr('repo list', ro, 'path', 'repos[].path'), kind: str(ro.kind) };
    });
  }

  async worktreeList(repoId: string): Promise<WorktreeInfo[]> {
    const cmd = 'worktree list';
    const r = await this.call(['worktree', 'list', '--repo', `id:${repoId}`, '--limit', listLimit]);
    if (r.truncated !== false) throw new OrcaError('list_truncated', `orca ${cmd} 的结果不完整（truncated 不是 false）`);
    if (!Array.isArray(r.worktrees)) throw badOutput(cmd, 'worktrees');
    return r.worktrees.map((w) => {
      const o = obj(w);
      return {
        id: needStr(cmd, o, 'id', 'worktrees[].id'),
        repoId: needStr(cmd, o, 'repoId', 'worktrees[].repoId'),
        path: needStr(cmd, o, 'path', 'worktrees[].path'),
        branch: str(o.branch),
        comment: str(o.comment),
        git: isObj(o.git),
        isMainWorktree: o.isMainWorktree === true,
        isBare: o.isBare === true,
        lastActivityAt: typeof o.lastActivityAt === 'number' ? o.lastActivityAt : 0,
      };
    });
  }

  async worktreeRemove(id: string, force: boolean): Promise<'removed' | 'not_found'> {
    let r: Json;
    try {
      // 不带 --run-hooks（不执行仓库的 archive hook）、--delete-branch（分支由连接器在自己的 clone 中删除，见 reclaim.ts）
      r = await this.call(['worktree', 'rm', '--worktree', `id:${id}`, ...(force ? ['--force'] : [])]);
    } catch (err) {
      if (err instanceof OrcaError && err.code === worktreeNotFoundCode) return 'not_found';
      throw err;
    }
    if (r.removed !== true) throw new OrcaError('remove_unknown', `orca worktree rm 没有报告已经删除 ${id}`);
    return 'removed';
  }

  /**
   * worker-list 不带 --run 时，Orca 列出的是"调用方"绑定的 Run 的 worker，没有调用方时才列出全部 Run 的（scope.source 为
   * bound、all）。连接器在 Orca 外运行，没有终端身份，Orca 就从当前目录所在的工作区、或者整个 Orca 中唯一的终端推断调用方：
   * 执行节点上通常只有连接器的邮箱终端，于是推断为邮箱、列出的是邮箱 Run 的 worker（生产实测：scope.source=bound）。
   * 所以不依赖推断：run-list 翻完全部 Run，逐个 Run 显式 --run 查询，每个回执都必须是 scope.source=flag、scope.run 等于
   * 请求的 Run；最后再列一次 Run，查询期间新建了 Run（它的 worker 可能没有列出）时报 list_changed。
   *
   * 每个 Run 只翻一遍、不按 --terminal-state 过滤：按状态分开查的几次查询不是同一时刻的，一个 worker 在两次查询之间从一个
   * 没有释放的状态变到另一个（例如 worker-keep-open 让 active 变成 retained）就两次都不在结果中。Orca 1.4.258 不带状态
   * 过滤的翻页：第一页记下这个 Run 当时最大的 dispatch 行号作为快照，按行号从新到旧、游标是上一页最后一行的行号，每页按
   * 当时的状态返回快照中的行；Orca 只在 orchestration reset 时删除 dispatch，游标所在的行没有了时报
   * worker_list_cursor_expired。所以快照中的每个 dispatch 恰好出现一次，状态是读到它那一页时的；终端释放之后不会再变
   * （释放了的终端不能再转给别的 dispatch），没有终端、已经了结的 dispatch 也不会再建终端，这两种可以排除，其余的
   * （包括不认识的状态、没有终端但还没有了结的）都算没有释放。另外核对每页的 page.total（快照中的 dispatch 数）不变、
   * 不重复、翻完时条数与它相等，不一致时报 list_changed。快照之后才建的 dispatch 不在结果中，见 PLAN #33.10 的已知窗口。
   */
  async unreleasedWorkers(): Promise<UnreleasedWorker[]> {
    const runs = await this.runIds();
    const out: UnreleasedWorker[] = [];
    // Run 逐个查询：调用次数是全部 Run 的页数之和，回收在仓库锁内、节点的周期任务中进行
    for (const run of runs) out.push(...(await this.unreleasedOf(run)));
    const known = new Set(runs);
    const added = (await this.runIds()).filter((id) => !known.has(id));
    if (added.length > 0) throw new OrcaError('list_changed', `查询期间 Orca 中新建了 Run（${added.join('、')}），worker 列表可能不完整`);
    return out;
  }

  /** runIds 翻完 run-list 的所有页，返回全部 Run 的 id。 */
  private async runIds(): Promise<string[]> {
    const cmd = 'orchestration run-list';
    const ids = new Set<string>();
    let cursor = '';
    for (let page = 0; ; page++) {
      if (page >= maxRunPages) throw new OrcaError('list_truncated', `orca ${cmd} 超过 ${maxRunPages} 页`);
      const r = await this.call(['orchestration', 'run-list', '--limit', runPageLimit, ...(cursor ? ['--cursor', cursor] : [])]);
      if (!Array.isArray(r.runs)) throw badOutput(cmd, 'runs');
      for (const run of r.runs) ids.add(needStr(cmd, obj(run), 'id', 'runs[].id'));
      // 翻页（夹具 run-list.json、run-list-last.json）：最后一页 nextCursor 为 null；没有这个字段时不能确认已经翻完
      if (!('nextCursor' in r) || (r.nextCursor !== null && typeof r.nextCursor !== 'string')) throw badOutput(cmd, 'nextCursor');
      cursor = str(r.nextCursor);
      if (!cursor) break;
    }
    return [...ids];
  }

  /** unreleasedOf 翻完 Run run 的全部 dispatch（worker-list --run，不按状态过滤），返回终端没有释放的（见 unreleasedWorkers）。 */
  private async unreleasedOf(run: string): Promise<UnreleasedWorker[]> {
    const cmd = `orchestration worker-list --run ${run}`;
    const out: UnreleasedWorker[] = [];
    const seen = new Set<string>();
    let total: number | undefined;
    let cursor = '';
    for (let page = 0; ; page++) {
      if (page >= maxWorkerPages) throw new OrcaError('list_truncated', `orca ${cmd} 超过 ${maxWorkerPages} 页（这个 Run 的 dispatch 超过 ${maxWorkerPages * Number(workerPageLimit)} 个），不能确认工作区中没有运行的 Agent`);
      const r = await this.call(['orchestration', 'worker-list', '--run', run, '--limit', workerPageLimit, ...(cursor ? ['--cursor', cursor] : [])]);
      // 夹具 worker-list-all-p*.json：显式 --run 时作用域是 flag；不是请求的 Run 的列表不能用来判断这个 Run 中有没有 Agent
      const scope = obj(r.scope);
      const source = str(scope.source);
      if (source !== 'flag' || str(scope.run) !== run) {
        throw new OrcaError('list_scoped', `orca ${cmd} 列出的不是这个 Run 的 worker（scope.source=${source || '无'}，scope.run=${str(scope.run) || '无'}），不能确认工作区中没有运行的 Agent`);
      }
      if (!Array.isArray(r.workers)) throw badOutput(cmd, 'workers');
      const pg = obj(r.page);
      if (typeof pg.total !== 'number') throw badOutput(cmd, 'page.total');
      if (total === undefined) total = pg.total;
      else if (pg.total !== total) throw new OrcaError('list_changed', `orca ${cmd} 翻页期间 page.total 从 ${total} 变成 ${pg.total}，列表可能不完整`);
      for (const w of r.workers.map(obj)) {
        const dispatchId = needStr(cmd, w, 'dispatchId', 'workers[].dispatchId');
        if (seen.has(dispatchId)) throw new OrcaError('list_changed', `orca ${cmd} 翻页时 dispatch ${dispatchId} 出现了两次，列表可能不完整`);
        seen.add(dispatchId);
        const state = w.terminalState;
        if (state !== null && typeof state !== 'string') throw badOutput(cmd, 'workers[].terminalState');
        if (state === releasedTerminalState) continue;
        if (state === null && settledDispatchStatuses.has(str(w.dispatchStatus))) continue;
        const projection = obj(w.projection);
        out.push({
          dispatchId,
          terminalState: str(state),
          liveness: str(obj(projection.liveness).verdict),
          workspace: str(obj(w.resource).worktreeId) || str(obj(projection.workspace).id),
        });
      }
      // --full 的翻页在 page 中：hasMore 时必须有 nextCursor
      cursor = str(pg.nextCursor);
      if (pg.hasMore === true && !cursor) throw badOutput(cmd, 'page.nextCursor');
      if (!cursor) break;
    }
    if (seen.size !== total) throw new OrcaError('list_changed', `orca ${cmd} 列出了 ${seen.size} 个 dispatch，page.total 是 ${total}，列表可能不完整`);
    return out;
  }

  async terminalList(worktree: string): Promise<TerminalInfo[]> {
    const cmd = 'terminal list';
    const r = await this.call(['terminal', 'list', '--worktree', worktree, '--limit', listLimit]);
    // 与 worktreeList 相同：没有明确回答 truncated === false 时不能当作列表完整
    if (r.truncated !== false) throw new OrcaError('list_truncated', `orca ${cmd} 的结果不完整（truncated 不是 false）`);
    if (!Array.isArray(r.terminals)) throw badOutput(cmd, 'terminals');
    return r.terminals.map((t) => terminalInfo(cmd, t, 'terminals[]'));
  }
}
