import { randomUUID } from 'node:crypto';
import { existsSync } from 'node:fs';
import { mkdir, readFile, rename, rm, stat, writeFile } from 'node:fs/promises';
import path from 'node:path';
import type { Git } from './git.js';
import type { Logger } from './log.js';
import { errMessage } from './errors.js';
import { KeyedMutex } from './mutex.js';
import { type CheckRequest, isDefinite, isRefusal, type Orca, type OrcaMessage, type TerminalInfo, terminalUsable } from './orca.js';
import { canon } from './reclaim.js';
import { Redactor } from './redact.js';
import { sleep } from './kiln.js';

// 连接器在 Orca 中的身份（PLAN #33.4）：orchestration 命令要求调用方是一个 Orca 终端，连接器在 Orca 外运行，
// 所以在数据目录中建一个空仓库 kiln-orca，注册到 Orca，在其中建一个终端作为邮箱，用它的句柄建 Run 并发出命令。
// 所有主 Agent 的 worker_done 都到这个邮箱：一个循环读取并落盘（inbox/<dispatch>.json），等待某个 dispatch 的执行从落盘的
// 消息中取。Agent 的提问不由人回答，回复让它用 kiln-orca task 与 Kiln 交互。
//
// Orca 的 check 返回一批消息即标记为已处理（不再等确认）。为了崩溃、响应丢失都不丢消息：每次 check 之前先把请求（带
// --retry-request 的 UUID 和全部参数）存进 mailbox.json，这一批全部处理完（落盘、回复成功）之前不发下一次 check；重启后
// 用存下的请求重发，Orca 重放同一批，再处理一遍（落盘覆盖写、重复回复无害）。
//
// 保证邮箱可用（ensure）的决策表。平时只查终端在不在；Run 只在 Orca 拒绝时查（check 被拒绝为 consumer_fenced、
// run_not_found，或者 worker-start 被拒绝为 run_not_found：runLost），查的办法就是让邮箱终端 run-use 接管它：
//
// | 邮箱终端 | Run | 动作 |
// |---|---|---|
// | 存活 | 没有被拒绝过 | 沿用 |
// | 存活 | 被拒绝过：run-use 成功（可接管，或者被别的终端占用：接管回来） | 沿用 |
// | 存活 | 被拒绝过：run-use 报告 run_not_found | 在这个终端上 run-create，换新 Run（见下） |
// | 不存活（进程已经结束、句柄失效）或者没有记录 | 有记录 | 新建终端，run-use 接管原来的 Run；run_not_found 时在新终端上 run-create |
// | 不存活或者没有记录 | 没有记录 | 新建终端，run-create |
// | 查询失败（连不上 Orca、结果未知） | — | 原样抛出，稍后重试（不重建：不能断定终端已经不在） |
// | 任何 | run-use、run-create 失败（run_not_found 以外） | 原样抛出，稍后重试；已经新建的终端记在内存中，重试时沿用 |
//
// 存活按 orca.ts 的 terminalUsable：Orca 查得到、进程还在（connected）。没有界面窗格（orphaned）的终端照样存活——Orca 界面
// 没有接管它、窗口关着、界面为它的标签另起了终端时都是这样，它照样能 run-use、check；把它当作不在会让每次检查都新建一个
// 终端，旧的还连着、一直留在 Orca 中。换了新终端之后，旧的那个（Orca 还查得到、在邮箱仓库中、标题是邮箱）关掉
// （terminal close），关不掉只记日志。
//
// 多余的邮箱终端（sweep，连接器启动后和每小时）：邮箱仓库 <数据目录>/kiln-orca 是连接器自己的空仓库，只有它在其中建标题为
// mailboxTitle 的终端，所以这个仓库中标题是邮箱、不是当前句柄的终端都是连接器以前建的（换新时没有关掉的、句柄在 Orca 重启后
// 失效但进程还在的、旧版本泄漏的），关掉它们，每轮最多 maxSweepPerRound 个。别的标题的终端（有人在这个工作区中打开的）
// 不能证明是连接器建的，不关，只在日志和 doctor 中提示人处理；别的数据目录的节点的邮箱在别的仓库中，不会被列出。
//
// 换新终端时旧终端上的 check：Run 一旦被新终端 run-use 接管，Orca 结束旧终端上正在等待的 check（consumer_fenced："This
// caller is not bound to Run …, which is bound to <新终端>. Nothing changed."，实测），用旧终端新发的 check 也一样。Orca 没有
// 交出消息，不是问题：换新的请求用新终端接着读（不记 warn、不退避）。已经交出过一批的旧请求照样能重放（实测：被别的终端
// 接管、旧终端关闭之后，同一个请求仍重放同一批）。用当前终端发出的 check 被拒绝为 consumer_fenced 才是 Run 被别的终端占用。
//
// Run 换新（Orca 的编排状态被重置）：旧 Run 中的消息再也收不到。没处理完的 check 是旧 Run 的，Orca 不能再重放，丢弃；在途
// attempt 的 Agent 是在旧 Run 中启动的，它们的 dispatch 多半也随重置没有了，由 attempt 的 worker-show 兜底发现
// （dispatch_not_found）并以 infra.orca 结束（见 attempt.ts 的 waitAndFinish）；dispatch 还在的照常由 worker-show 判断结束。

export interface Identity {
  handle: string;
  runId: string;
  /** 已经（或即将）发出、它返回的一批还没有处理完的 check。 */
  check?: CheckRequest;
}

/** IdentityStore 保存邮箱句柄和 Run（数据目录的 mailbox.json，只有持锁的连接器写）。 */
export interface IdentityStore {
  load(): Promise<Identity | undefined>;
  save(id: Identity): Promise<void>;
}

/** MailboxTiming 是邮箱的等待与重试时间（测试中缩短）。 */
export interface MailboxTiming {
  /** 一次 check 最多等多久（每次 check 在 Orca 中留下一条请求记录，等得太短会很快占满 Orca 的记录上限）。 */
  waitMs: number;
  /** 失败后第一次重试的间隔，之后加倍。 */
  retryMs: number;
  maxRetryMs: number;
}

export const defaultMailboxTiming: MailboxTiming = { waitMs: 5 * 60_000, retryMs: 5_000, maxRetryMs: 60_000 };
/** mailboxTitle 是邮箱终端的标题：清理多余的邮箱终端时据此（和所在的邮箱仓库）认出连接器建的终端。 */
export const mailboxTitle = 'kiln-orca 邮箱';
/** maxSweepPerRound 是每轮清理最多关闭的多余邮箱终端数（每个是一次 orca 调用；多的留到下一轮）。 */
export const maxSweepPerRound = 50;
const questionReply =
  '这个问题不会有人回答：Kiln 的任务没有人值守。请按任务说明自行决定，并用任务说明中的 kiln-orca task 命令与 Kiln 交互（领取、校验、交付、查询 CI）；有疑问的决定记录在报告中。确实无法继续时，按任务说明写出报告（说明受阻的原因）后发送 worker_done 结束。';

// ---- 邮箱消息的分类与落盘 ----
//
// Orca 投递三类消息（PLAN #33.4）：worker_done 是 Agent 结束了这一轮；escalation 和 question 是 Agent 请求协调者处理。
// escalation 中有两种要区别对待：
// - Orca 报告 Agent 运行中断（payload.turnOutcome 为 failure，或者 stopReason 是 needs-input / exited / lost）：这一轮无法
//   继续，即使 Agent 的进程还在，自动化任务也做不下去了。落盘一条中止记录，等待它的 attempt 按 ai.runtime 失败；
// - 系统生命周期通知（stopReason 是 finished 或连接器不认识的其它值）：Agent 的一轮结束了，但任务可能还在继续（例如在等
//   后台的 make test）。只记日志、不回复——回复会打扰正在等后台命令的 Agent。
// 其余 escalation（没有这些字段的主动升级）和 question 是普通求助：没有人值守，自动回复让它自行决定。
//
// 致命的中止记录与 worker_done 分开落盘（inbox/<dispatch>.json、inbox/<dispatch>.abort.json），并且读取时 worker_done
// 优先：同一批消息中两者都到（Agent 恰好先发了 worker_done、Orca 又报告了上一轮的错误）时以 worker_done 为准；写入时也
// 先写 worker_done 再写中止记录，落盘失败的那个留在批次中重试。

/** fatalStopReasons 是 Orca 的 stopReason 中表示这一轮无法继续的值（needs-input 要人在 Agent 所在的位置回答，reply 解不开）。 */
export const fatalStopReasons: ReadonlySet<string> = new Set(['needs-input', 'exited', 'lost']);

/** EscalationKind 是一条 escalation 的处理方式：致命（按失败结束）、系统生命周期通知（继续等）、普通求助（自动回复）。 */
export type EscalationKind = 'fatal' | 'system_stop' | 'ordinary';

/**
 * classifyEscalation 判断一条没有被 Orca 拒绝的 escalation 怎么处理：turnOutcome 为 failure 或者 stopReason 是
 * fatalStopReasons 中的值时为 fatal（即使同时带着 stopReason: finished 也按 failure 处理）；其余带 stopReason 的
 * （finished 以及将来新增的、连接器不认识的值）是 system_stop；没有这些字段的是普通求助。failureKind 是 Agent 私有的
 * 诊断信息，不参与判断。
 */
export function classifyEscalation(m: OrcaMessage): EscalationKind {
  if (m.turnOutcome === 'failure' || fatalStopReasons.has(m.stopReason)) return 'fatal';
  if (m.stopReason !== '') return 'system_stop';
  return 'ordinary';
}

/** maxAbortMessageBytes 是提交给 Server 的中止说明（Complete.message）的字节上限：结果长期保留，只留诊断需要的部分。 */
export const maxAbortMessageBytes = 4096;
/** maxAbortLogBytes 是任务日志中止诊断的字节上限：可以比提交给 Server 的说明更完整。 */
export const maxAbortLogBytes = 16 * 1024;
/**
 * maxAbortMetaBytes 是中止记录中元信息字段（id、turnOutcome、stopReason、failureKind）各自的字节上限：它们只作诊断，
 * 不能让一个超长的值把两份说明里的主因挤掉，也不让落盘的记录无界增长。
 */
export const maxAbortMetaBytes = 128;
/** truncatedMark 是截断后附加的说明。 */
const truncatedMark = '…（已截断）';

/** utf8Prefix 取 text 的前 maxBytes 字节（不截断多字节字符，也不留下半个代理对）。 */
function utf8Prefix(text: string, maxBytes: number): string {
  const buf = Buffer.from(text, 'utf8');
  if (buf.length <= maxBytes) return text;
  let cut = Math.max(0, maxBytes);
  // 落在 UTF-8 的续字节（10xxxxxx）上时往前退到这个字符的起点
  while (cut > 0 && (buf[cut]! & 0xc0) === 0x80) cut--;
  return buf.subarray(0, cut).toString('utf8');
}

/** truncateUtf8 把 text 截到 maxBytes 字节以内并附加 truncatedMark（超限时）；不截断多字节字符或代理对。 */
export function truncateUtf8(text: string, maxBytes: number): string {
  if (Buffer.byteLength(text, 'utf8') <= maxBytes) return text;
  const mark = utf8Prefix(truncatedMark, maxBytes);
  const head = utf8Prefix(text, Math.max(0, maxBytes - Buffer.byteLength(mark, 'utf8')));
  return head + mark;
}

/** AbortText 是一条中止记录的两份说明：提交给 Server 的（短）和写进任务日志的（长一些）。 */
export interface AbortText {
  reason: string;
  logDetail: string;
}

/**
 * formatAbortReason 用 Orca 报告的原始原因组装中止说明（PLAN #33.4）：先把每个字段脱敏、去掉首尾空白，再截断——先截断会把
 * 登记过的凭据截成半段，之后替换不掉。元信息（failureKind、stopReason）先限长到 maxAbortMetaBytes，prefix 有界之后主因才
 * 拿得到两份说明的篇幅。主因按 error、detail、body、subject 取第一个非空的；取了 error 而 detail 不在其中时
 * 补上详情，并优先保证 error 的篇幅。reason 留给 Server（长期保留），logDetail 给任务日志。
 */
export function formatAbortReason(m: OrcaMessage, redactor: Redactor): AbortText {
  const clean = (s: string): string => redactor.redact(s).trim();
  const metaField = (s: string): string => truncateUtf8(clean(s), maxAbortMetaBytes);
  const failureKind = metaField(m.failureKind);
  const stopReason = metaField(m.stopReason);
  const error = clean(m.error);
  const detail = clean(m.detail);
  const body = clean(m.body);
  const subject = clean(m.subject);
  const meta = [...new Set([failureKind, stopReason].filter((s) => s !== ''))].join('、');
  const prefix = `Agent 运行中断${meta ? `（${meta}）` : ''}：`;
  const main = error || detail || body || subject || 'Orca 报告 Agent 运行中断';
  const details: Array<{ label: string; text: string }> = [];
  if (error && detail && !error.includes(detail)) details.push({ label: '详情', text: detail });
  if (body && body !== main && !main.includes(body) && !details.some((d) => d.text.includes(body))) details.push({ label: '输出', text: body });
  return {
    reason: compose(prefix, main, details.filter((d) => d.label === '详情'), maxAbortMessageBytes),
    logDetail: compose(prefix, main, details, maxAbortLogBytes),
  };
}

/** compose 把主因（优先占用篇幅）与补充的诊断拼成一条说明，总字节数不超过 max。 */
function compose(prefix: string, main: string, extra: Array<{ label: string; text: string }>, max: number): string {
  let text = truncateUtf8(prefix + main, max);
  for (const x of extra) {
    const sep = `；${x.label}：`;
    const room = max - Buffer.byteLength(text + sep, 'utf8');
    if (room <= 0) break;
    text += sep + truncateUtf8(x.text, room);
  }
  return text;
}

/**
 * AbortRecord 是落在 inbox/<dispatch>.abort.json 中的中止记录：已经脱敏并限长（元信息各 maxAbortMetaBytes，reason、
 * logDetail 见 maxAbortMessageBytes、maxAbortLogBytes），连接器重启后内存中的 Redactor 清空，从磁盘读出的内容也不会带出凭据。
 */
export interface AbortRecord {
  id: string;
  dispatchId: string;
  turnOutcome: string;
  stopReason: string;
  failureKind: string;
  reason: string;
  logDetail: string;
}

/** abortRecord 由一条致命的 escalation 生成中止记录（脱敏、限长之后才落盘）。 */
export function abortRecord(m: OrcaMessage, redactor: Redactor): AbortRecord {
  const text = formatAbortReason(m, redactor);
  const meta = (s: string): string => truncateUtf8(redactor.redact(s), maxAbortMetaBytes);
  return {
    id: meta(m.id),
    dispatchId: m.dispatchId,
    turnOutcome: meta(m.turnOutcome),
    stopReason: meta(m.stopReason),
    failureKind: meta(m.failureKind),
    reason: text.reason,
    logDetail: text.logDetail,
  };
}

/** isFatalEscalation 判断一条消息是不是要落盘中止记录的致命 escalation（Orca 拒绝了的消息不算）。 */
function isFatalEscalation(m: OrcaMessage): boolean {
  return m.type === 'escalation' && !m.rejected && classifyEscalation(m) === 'fatal';
}

/** MailboxSettlement 是一个 dispatch 的结束方式：正常结束的 worker_done，或者 Orca 报告的中止。 */
export type MailboxSettlement = { kind: 'done'; message: OrcaMessage } | { kind: 'abort'; abort: AbortRecord };

/**
 * safeDispatchId 判断 dispatch id 能不能直接用作 inbox 中的文件名：空串、点、父目录、带路径分隔符、带 NUL 或者会被
 * path.basename 改写的值都不能用（Orca 的 id 都是 ctx_… 这样的形式）。
 */
export function safeDispatchId(id: string): boolean {
  return id !== '' && id !== '.' && id !== '..' && !id.includes('/') && !id.includes('\\') && !id.includes('\0') && path.basename(id) === id;
}

/** pathMissing 判断 err 是不是文件不存在（只有 ENOENT 算不存在，其它错误照常抛出）。 */
function pathMissing(err: unknown): boolean {
  return (err as NodeJS.ErrnoException).code === 'ENOENT';
}

/** writeAtomic 先写每次唯一的临时文件再改名：读到的总是完整的内容（权限 0600，目录 0700）。 */
async function writeAtomic(target: string, data: string): Promise<void> {
  await mkdir(path.dirname(target), { recursive: true, mode: 0o700 });
  const tmp = `${target}.${randomUUID()}.tmp`;
  try {
    await writeFile(tmp, data, { mode: 0o600 });
    await rename(tmp, target);
  } catch (err) {
    await rm(tmp, { force: true }).catch(() => undefined);
    throw err;
  }
}

/** readJsonIfExists 读一个 JSON 文件：不存在时返回 undefined，内容损坏、没有权限等其它错误照常抛出（不静默吞掉）。 */
async function readJsonIfExists(p: string): Promise<unknown> {
  let text: string;
  try {
    text = await readFile(p, 'utf8');
  } catch (err) {
    if (pathMissing(err)) return undefined;
    throw err;
  }
  return JSON.parse(text);
}

/** fileExists 判断文件在不在（与读文件相同：只有 ENOENT 算不在）。 */
async function fileExists(p: string): Promise<boolean> {
  try {
    await stat(p);
    return true;
  } catch (err) {
    if (pathMissing(err)) return false;
    throw err;
  }
}

/** runNotFound 判断 err 是不是 Orca 报告 Run 不存在。 */
export function runNotFound(err: unknown): boolean {
  return isDefinite(err) && err.code === 'run_not_found';
}

/** mailboxRepoDir 是数据目录 dir 中邮箱的空仓库。 */
export function mailboxRepoDir(dir: string): string {
  return path.join(dir, 'kiln-orca');
}

/**
 * MailboxTerminals 是邮箱仓库中的终端：current 是当前句柄（Orca 中没有这个终端、没有句柄时为 undefined）；extra 是能证明
 * 属于连接器的多余邮箱终端（在邮箱仓库中、标题是 mailboxTitle、不是当前句柄）；foreign 是邮箱仓库中别的终端（不能证明是
 * 连接器建的）。
 */
export interface MailboxTerminals {
  current: TerminalInfo | undefined;
  extra: TerminalInfo[];
  foreign: TerminalInfo[];
}

/** ownMailboxTerminal 判断 t 能不能证明是连接器建的邮箱终端：在邮箱仓库 repo（canon 过的路径）中、标题是 mailboxTitle。 */
async function ownMailboxTerminal(t: TerminalInfo, repo: string): Promise<boolean> {
  return t.title === mailboxTitle && t.worktreePath !== '' && (await canon(t.worktreePath)) === repo;
}

/**
 * inspectMailboxTerminals 查看数据目录 dir 的邮箱仓库中的终端（只读：连接器的清理和 doctor 共用）。handle 是 mailbox.json
 * 中的当前句柄（没有时为空）。查询失败时抛出。
 */
export async function inspectMailboxTerminals(orca: Orca, dir: string, handle: string): Promise<MailboxTerminals> {
  const repoDir = mailboxRepoDir(dir);
  const repo = await canon(repoDir);
  const current = handle ? await orca.terminalShow(handle) : undefined;
  const out: MailboxTerminals = { current, extra: [], foreign: [] };
  for (const t of await orca.terminalList(`path:${repoDir}`)) {
    if (t.handle === handle) continue;
    (await ownMailboxTerminal(t, repo) ? out.extra : out.foreign).push(t);
  }
  return out;
}

/**
 * mailboxReport 是 doctor 中邮箱终端的几行说明（只读）：当前句柄的状态，邮箱仓库中多余的邮箱终端、不能证明是连接器建的
 * 终端的数量。handle 是 mailbox.json 中的句柄（没有时为空）。只是说明，不影响 doctor 的结论：终端不在时连接器运行时新建，
 * 多余的由连接器清理。
 */
export async function mailboxReport(orca: Orca, dir: string, handle: string): Promise<string[]> {
  if (!existsSync(mailboxRepoDir(dir))) return ['- Orca 邮箱终端：还没有建立（连接器第一次运行时建立）'];
  let found: MailboxTerminals;
  try {
    found = await inspectMailboxTerminals(orca, dir, handle);
  } catch (err) {
    return [`- Orca 邮箱终端：查询失败（${errMessage(err)}）`];
  }
  const t = found.current;
  const lines = [
    !handle
      ? '- Orca 邮箱终端：还没有建立（连接器运行时建立）'
      : !t
        ? `- Orca 邮箱终端：${handle} 在 Orca 中已经不在（句柄失效，例如 Orca 重启过）：连接器运行时新建一个`
        : terminalUsable(t)
          ? `✓ Orca 邮箱终端：${handle}（进程在运行${t.orphaned ? '；没有界面窗格，照样能用' : ''}）`
          : `- Orca 邮箱终端：${handle} 的进程已经结束（${t.exitCause || '原因不明'}）：连接器运行时新建一个，并关掉这个`,
  ];
  if (found.extra.length > 0) {
    lines.push(`- 邮箱仓库中有 ${found.extra.length} 个多余的邮箱终端：连接器启动后和每小时关闭（每轮最多 ${maxSweepPerRound} 个）；也可以按 DEPLOY.md 手工关闭`);
  }
  if (found.foreign.length > 0) {
    const list = found.foreign.map((x) => `${x.handle}（${x.title || '没有标题'}）`).join('、');
    lines.push(`- 邮箱仓库中有 ${found.foreign.length} 个不能证明是连接器建的终端，连接器不关闭：${list}；确认没有用之后 orca terminal close --terminal <句柄> --json`);
  }
  return lines;
}

/** SweepResult 是一轮清理的结果：closed 关掉了的、failed 关不掉的、left 超过每轮上限留到下一轮的、foreign 不能证明是连接器建的。 */
export interface SweepResult {
  closed: number;
  failed: number;
  left: number;
  foreign: number;
}

export class Mailbox {
  private id: Identity | undefined;
  /** 已经新建、但还没有接管 Run / 建好 Run 的终端句柄：它还不是当前身份，接管失败重试时沿用，不会每轮都新建一个。 */
  private pendingHandle: string | undefined;
  /** 正在进行的 ensure：同时调用的（邮箱循环、状态检测、启动 Agent）共用这一次，不会建出两个邮箱终端、两次改写 mailbox.json。 */
  private ensuring: Promise<Identity> | undefined;
  /**
   * Orca 拒绝过这个 Run（check 报告 consumer_fenced、run_not_found，或者 runLost）：下一次 ensure 让邮箱终端 run-use 接管它，
   * Run 不存在时换新。
   */
  private recheck = false;
  /** 写 mailbox.json 串行进行（ensure 与 pump 都会写）。 */
  private saving: Promise<void> = Promise.resolve();
  /** 换了新终端、还没有关掉的旧邮箱终端（Orca 还查得到它）：新终端接管 Run 并保存之后关闭。 */
  private readonly retired = new Map<string, TerminalInfo>();
  /** ensure 与 sweep 串行进行：清理不会把 ensure 刚建、还没有成为当前句柄的终端当成多余的。 */
  private readonly serial = new KeyedMutex();
  /** 上一次提示过的、邮箱仓库中不能证明是连接器建的终端（变化时再提示）。 */
  private foreignLogged = '';
  /** 同一个 dispatch 的落盘、读取串行进行：同批消息的处理顺序、重复的处理与 attempt 的读取不会交错。 */
  private readonly dispatchLock = new KeyedMutex();

  constructor(
    private readonly orca: Orca,
    private readonly git: Git,
    private readonly store: IdentityStore,
    private readonly dir: string,
    private readonly log: Logger,
    private readonly timing: MailboxTiming = defaultMailboxTiming,
    /** 节点共用的脱敏：中止记录落盘前替换登记过的凭据（PLAN #33.13）。 */
    private readonly redactor: Redactor = new Redactor(),
  ) {}

  get identity(): Identity {
    if (!this.id) throw new Error('邮箱还没有就绪');
    return this.id;
  }

  private get repoDir(): string {
    return mailboxRepoDir(this.dir);
  }
  private inboxFile(dispatchId: string): string {
    return path.join(this.dir, 'inbox', `${dispatchId}.json`);
  }
  private abortFile(dispatchId: string): string {
    return path.join(this.dir, 'inbox', `${dispatchId}.abort.json`);
  }

  /** ensure 确认邮箱终端存在和 Run 可用（见文件开头的决策表）。同时调用时共用正在进行的那一次。 */
  ensure(): Promise<Identity> {
    this.ensuring ??= this.serial.run('', () => this.doEnsure()).finally(() => (this.ensuring = undefined));
    return this.ensuring;
  }

  /**
   * runLost 由启动 Agent 的一方报告：Orca 以 run_not_found 拒绝了用 runId 发出的请求。runId 仍是当前的 Run 时，下一次
   * ensure 查证并换新（已经换过的不再处理）。
   */
  runLost(runId: string): void {
    if (this.id?.runId === runId) this.recheck = true;
  }

  private async doEnsure(): Promise<Identity> {
    const saved = this.id ?? (await this.store.load());
    // 查询失败时抛出：不能断定终端已经不在，不重建
    const term = saved ? await this.orca.terminalShow(saved.handle) : undefined;
    let id: Identity;
    if (saved && terminalUsable(term) && !this.recheck && !this.pendingHandle) {
      this.id = saved;
      return saved;
    }
    if (saved && terminalUsable(term)) {
      id = saved;
    } else {
      // 终端不在（或者还没有建过）：新建一个。Run 沿用原来的，下面接管它；新终端先记在 pendingHandle 中，接管失败重试时沿用，
      // 接管成功、Run 建好之后才成为当前身份（this.id），别处不会看到没有 Run 的句柄。
      // 旧终端 Orca 还查得到（进程已经结束）时，新终端接管 Run 之后关掉它
      if (saved && term) this.retired.set(saved.handle, term);
      // 重试时沿用上次新建的终端，但先确认它还在：进程已经结束或句柄失效时不能一直沿用（会一直接管 / 建 Run 失败）。
      // 查询失败（结果未知）时抛出不重建；明确不在时清掉 pendingHandle，仍查得到的移入 retired 待关闭。
      let handle = this.pendingHandle;
      if (handle) {
        const pending = await this.orca.terminalShow(handle);
        if (!terminalUsable(pending)) {
          if (pending) this.retired.set(handle, pending);
          this.pendingHandle = undefined;
          handle = undefined;
        }
      }
      handle ??= await this.newTerminal();
      this.pendingHandle = handle;
      id = { handle, runId: saved?.runId ?? '', ...(saved?.check ? { check: saved.check } : {}) };
    }
    if (id.runId) {
      try {
        await this.orca.runUse(id.handle, id.runId);
        this.log.info('邮箱终端接管 Run', { handle: id.handle, runId: id.runId });
      } catch (err) {
        if (!runNotFound(err)) throw err;
        this.log.warn('Orca 中已经没有邮箱的 Run（Orca 的编排状态被重置）：新建一个，旧 Run 中没有收到的消息不再能收到', { runId: id.runId, check: id.check?.requestId ?? '' });
        // 没处理完的 check 属于旧 Run：Orca 已经不能重放它
        id = { handle: id.handle, runId: '' };
      }
    }
    if (!id.runId) {
      id = { ...id, runId: await this.orca.runCreate(id.handle, 'Kiln AI 任务（kiln-orca）') };
    }
    // 接管、建成 Run 之后才更新当前身份；失败时 pendingHandle 保留，下一次沿用同一个终端
    this.id = id;
    this.pendingHandle = undefined;
    this.recheck = false;
    await this.persist();
    this.log.info('Orca 邮箱就绪', { handle: id.handle, runId: id.runId });
    await this.closeRetired(id.handle);
    return id;
  }

  /**
   * closeRetired 关闭换下来的旧邮箱终端（不是当前句柄的 retired）。只关能证明是连接器建的（在邮箱仓库中、标题是邮箱，
   * 并且是 mailbox.json 记过的句柄）；关不掉只记日志，不影响邮箱（还连着的会在之后的清理中再关）。
   */
  private async closeRetired(current: string): Promise<void> {
    const repo = await canon(this.repoDir);
    for (const [handle, t] of [...this.retired]) {
      this.retired.delete(handle);
      if (handle === current) continue;
      if (!(await ownMailboxTerminal(t, repo))) {
        this.log.warn('换下来的旧邮箱终端不在邮箱仓库中或者标题不是邮箱，不关闭', { handle, title: t.title, worktree: t.worktreePath });
        continue;
      }
      try {
        await this.orca.terminalClose(handle);
        this.log.info('关闭了换下来的旧邮箱终端', { handle, exitCause: t.exitCause });
      } catch (err) {
        this.log.warn('关闭换下来的旧邮箱终端失败，不影响邮箱', { handle, err: errMessage(err) });
      }
    }
  }

  /**
   * sweep 关闭邮箱仓库中多余的邮箱终端（见文件开头"多余的邮箱终端"），每轮最多 maxSweepPerRound 个。邮箱还没有就绪（没有
   * 当前句柄）时不清理。查询失败时抛出；关不掉的计入 failed、记日志，下一轮再试。
   */
  sweep(): Promise<SweepResult> {
    return this.serial.run('', () => this.doSweep());
  }

  private async doSweep(): Promise<SweepResult> {
    const out: SweepResult = { closed: 0, failed: 0, left: 0, foreign: 0 };
    if (!this.id) return out;
    const found = await inspectMailboxTerminals(this.orca, this.dir, this.id.handle);
    out.foreign = found.foreign.length;
    const foreign = found.foreign.map((t) => `${t.handle}（${t.title || '没有标题'}）`).join('、');
    if (foreign !== this.foreignLogged) {
      this.foreignLogged = foreign;
      if (foreign) this.log.warn('邮箱仓库中有不能证明是连接器建的终端（标题不是邮箱），不关闭；确认没有用之后可以手工关闭（orca terminal close --terminal <句柄>）', { terminals: foreign });
    }
    // 还没接管 Run / 建好 Run 的 pendingHandle 不是多余的：ensure 正在进行（或失败待重试）时不能把它关掉。
    const extra = found.extra.filter((t) => t.handle !== this.pendingHandle);
    const batch = extra.slice(0, maxSweepPerRound);
    out.left = extra.length - batch.length;
    for (const t of batch) {
      try {
        await this.orca.terminalClose(t.handle);
        out.closed++;
      } catch (err) {
        out.failed++;
        this.log.warn('关闭多余的邮箱终端失败，下一轮再试', { handle: t.handle, err: errMessage(err) });
      }
    }
    if (out.closed > 0 || out.left > 0) this.log.info('关闭了邮箱仓库中多余的邮箱终端', { closed: out.closed, failed: out.failed, left: out.left, current: this.id.handle });
    return out;
  }

  /** newTerminal 在数据目录的空仓库 kiln-orca 中新建一个终端作为邮箱。 */
  private async newTerminal(): Promise<string> {
    if (!existsSync(path.join(this.repoDir, '.git'))) {
      await mkdir(this.repoDir, { recursive: true });
      await this.git.run(this.repoDir, 'init', '--quiet');
      await this.git.run(this.repoDir, '-c', 'user.name=kiln-orca', '-c', 'user.email=kiln-orca@localhost', 'commit', '--quiet', '--allow-empty', '-m', 'kiln-orca 邮箱');
    }
    await this.orca.repoAdd(this.repoDir);
    return this.orca.terminalCreate(this.repoDir, mailboxTitle);
  }

  /** persist 把当前的身份（含没有处理完的 check）写进 mailbox.json。调用时同步取快照，排队中的写入不会写进之后的状态。 */
  private persist(): Promise<void> {
    const snapshot = this.id ? { ...this.id, ...(this.id.check ? { check: { ...this.id.check } } : {}) } : undefined;
    const write = this.saving.then(() => (snapshot ? this.store.save(snapshot) : undefined));
    this.saving = write.catch(() => undefined);
    return write;
  }

  /**
   * nextCheck 返回要发出的 check：有没处理完的（上次崩溃、响应丢失、处理失败）就是它，否则新建一个并先写进 mailbox.json，
   * 写成功之后才发出。确保身份、生成 check 与持久化全在 serial 内完成：不会和 ensure、sweep 交错，也不会用 serial 外读到的
   * 过期句柄 / Run 生成 check 或覆盖它们刚写下的状态。
   */
  private nextCheck(): Promise<CheckRequest> {
    return this.serial.run('', async () => {
      const id = await this.doEnsure();
      if (id.check) return id.check;
      const check: CheckRequest = { requestId: randomUUID(), terminal: id.handle, run: id.runId, waitMs: this.timing.waitMs };
      id.check = check;
      try {
        await this.persist();
      } catch (err) {
        id.check = undefined;
        throw err;
      }
      return check;
    });
  }

  /** checkDone 表示这一批已经处理完（或者 Orca 明确没有交出消息）：下一次 check 用新的请求。 */
  private checkDone(req: CheckRequest): void {
    if (this.id?.check === req) this.id.check = undefined;
  }

  /**
   * pump 持续读取邮箱，直到 signal 结束。一批消息全部处理完（worker_done 落盘、提问已回复）之后才读下一批；处理失败的消息
   * 留在内存中退避重试（其余的照常处理），进程崩溃时由 mailbox.json 中的 check 请求在重启后重放取回。check 的结果未知
   * （命令失败、输出无法解析）时同样重放；Orca 明确拒绝了这次 check（有错误码），或者等待被打断时没有交出消息，换新的请求。
   */
  async pump(signal: AbortSignal): Promise<void> {
    let batch: { req: CheckRequest; left: OrcaMessage[] } | undefined;
    let failures = 0;
    while (!signal.aborted) {
      try {
        if (!batch) {
          const req = await this.nextCheck();
          const r = await this.check(req, signal);
          if (r.interrupted) {
            this.checkDone(req);
            continue;
          }
          batch = { req, left: r.messages };
        }
        const errors: unknown[] = [];
        const left: OrcaMessage[] = [];
        const failedDone = new Set<string>();
        // 阶段 1：先处理 worker_done、提问、普通求助和系统生命周期通知（致命 escalation 留到阶段 2），并记下落盘失败的
        // worker_done——同一批里它的致命 escalation 不能抢先，否则 attempt 会先读到中止记录、把一条已经正常结束的
        // worker_done 丢在后面。
        const fatal: OrcaMessage[] = [];
        for (const m of batch.left) {
          if (isFatalEscalation(m)) {
            fatal.push(m);
            continue;
          }
          try {
            await this.handle(m);
          } catch (err) {
            errors.push(err);
            if (m.type === 'worker_done' && !m.rejected && safeDispatchId(m.dispatchId)) failedDone.add(m.dispatchId);
            left.push(m);
          }
        }
        // 阶段 2：落盘中止记录。同批的 worker_done 没有落盘时先不写，等 worker_done 重试成功之后下一轮再写（读取时
        // worker_done 同样优先）。
        for (const m of fatal) {
          if (failedDone.has(m.dispatchId)) {
            left.push(m);
            continue;
          }
          try {
            await this.handle(m);
          } catch (err) {
            errors.push(err);
            left.push(m);
          }
        }
        batch.left = left;
        if (errors.length > 0) throw new AggregateError(errors, errors.map(String).join('；'));
        this.checkDone(batch.req);
        batch = undefined;
        failures = 0;
      } catch (err) {
        if (signal.aborted) return;
        failures++;
        this.log.warn(batch ? '处理 Orca 邮箱的消息失败，稍后重试没有处理完的' : '读取 Orca 邮箱失败', { err: errMessage(err), left: batch?.left.length ?? 0 });
        await sleep(Math.min(this.timing.retryMs * 2 ** (failures - 1), this.timing.maxRetryMs), signal);
      }
    }
  }

  /**
   * check 发出一次 check。Orca 明确拒绝时（有错误码）没有交出消息，换新的请求；Run 绑定在别的终端上（consumer_fenced）或者
   * 不存在（run_not_found）时，下一次 ensure 让邮箱终端接管 Run、Run 不存在时换新。例外是邮箱已经换了终端、这次 check 是
   * 旧终端发的：Run 已经由新终端接管，consumer_fenced 是预期的（Orca 没有交出消息），按等待被打断处理，换新的请求接着读。
   */
  private async check(req: CheckRequest, signal: AbortSignal) {
    try {
      return await this.orca.check(req, signal);
    } catch (err) {
      if (isDefinite(err)) {
        this.checkDone(req);
        if (err.code === 'consumer_fenced' && this.id && req.terminal !== this.id.handle) {
          this.log.info('邮箱已经换了终端，旧终端上的 check 被 Orca 结束（没有交出消息），用新终端继续读取', { terminal: req.terminal, current: this.id.handle });
          return { messages: [], interrupted: true };
        }
        if (err.code === 'consumer_fenced' || err.code === 'run_not_found') this.recheck = true;
      }
      throw err;
    }
  }

  /**
   * handle 处理一条消息（可能重复处理同一条）。Orca 拒绝了的消息（payload 带 _orcaLifecycleRejection）在任何分支之前过滤：
   * 不落盘、不回复——被拒绝的 worker_done 没有结束 dispatch（例如 Agent 派生的子 Agent 用了主 Agent 的 dispatch id）。
   * worker_done 落盘给等待它的 attempt；致命的 escalation 落盘中止记录（首次的保留），系统生命周期通知只记日志（不回复，
   * 免得打扰正在等后台命令的 Agent）；普通求助（question 和没有致命标记的 escalation）回复它自行决定。落盘都在这个 dispatch
   * 的锁内，与 observe、done、abort、forget 串行。回复被 Orca 明确拒绝（已经有不同的回答、提问已关闭、提问的 Agent 已经
   * 不在等）时重试也不会成功，只记日志；结果未知或者 Orca 不可达时抛出（重试）。回复不持有 dispatch 的锁：慢回复不阻塞
   * 别的 dispatch 的落盘和读取。
   */
  private async handle(m: OrcaMessage): Promise<void> {
    if (m.rejected) {
      this.log.warn('Orca 拒绝了这条消息，不落盘、不回复', { type: m.type, dispatch: m.dispatchId, reason: m.rejected });
      return;
    }
    switch (m.type) {
      case 'worker_done':
        if (this.checkDispatch(m, 'worker_done')) {
          // 与 writeAbort、observe、forget 用同一把按 dispatch 的锁：落盘进行中的 worker_done 不会和读取、删除交错
          await this.dispatchLock.run(m.dispatchId, () => writeAtomic(this.inboxFile(m.dispatchId), JSON.stringify(m)));
        }
        return;
      case 'escalation': {
        const kind = classifyEscalation(m);
        if (kind === 'fatal') {
          if (!this.checkDispatch(m, '致命的 escalation')) return;
          const first = await this.writeAbort(abortRecord(m, this.redactor));
          this.log.info(first ? 'Orca 报告 Agent 运行中断，按失败结束这个任务' : 'Orca 再次报告 Agent 运行中断，保留第一次记录的原因', {
            dispatch: m.dispatchId,
            turnOutcome: m.turnOutcome,
            stopReason: m.stopReason,
            failureKind: m.failureKind,
          });
          return;
        }
        if (kind === 'system_stop') {
          this.log.info('Orca 报告 Agent 的一轮结束（系统通知），继续等待 worker_done', { dispatch: m.dispatchId, stopReason: m.stopReason });
          return;
        }
        await this.reply(m);
        return;
      }
      case 'question':
        await this.reply(m);
        return;
      default:
        return;
    }
  }

  /**
   * checkDispatch 检查消息能不能落盘：dispatch id 要用作文件名，必须是安全的（见 safeDispatchId）。不能用时只记 warn
   * 并消费这条消息：不猜它的归属、不写文件、不回复，也不影响别的任务。
   */
  private checkDispatch(m: OrcaMessage, what: string): boolean {
    if (safeDispatchId(m.dispatchId)) return true;
    this.log.warn(`Orca 投递的${what}没有可用的 dispatch id，忽略（不落盘、不回复）`, { id: m.id, type: m.type, dispatch: m.dispatchId });
    return false;
  }

  /**
   * writeAbort 落盘一条中止记录：同一个 dispatch 串行，第一次的保留（之后重复的、新的致命事件不覆盖，也不刷新修改时间，
   * 便于从最早的根因排查）。返回是否写入了。
   */
  private writeAbort(record: AbortRecord): Promise<boolean> {
    return this.dispatchLock.run(record.dispatchId, async () => {
      const target = this.abortFile(record.dispatchId);
      if (await fileExists(target)) return false;
      await writeAtomic(target, JSON.stringify(record));
      return true;
    });
  }

  /** reply 自动回复一条普通求助（question 或者没有致命标记的 escalation）。 */
  private async reply(m: OrcaMessage): Promise<void> {
    try {
      await this.orca.reply(this.identity.handle, m.id, questionReply);
      this.log.info('Agent 请求协调者处理，已自动回复', { type: m.type, dispatch: m.dispatchId, subject: m.subject });
    } catch (err) {
      if (!isRefusal(err)) throw err;
      this.log.warn('Orca 拒绝了对这条消息的回复，不再回复', { id: m.id, type: m.type, dispatch: m.dispatchId, err: err.message });
    }
  }

  /**
   * observe 读取一个 dispatch 的结束方式（与写入串行）：worker_done 优先于中止记录（同一批里两者都到、或者写入期间崩溃时，
   * 都以 Agent 正常结束为准）。都不存在时返回 undefined；文件损坏、没有权限等错误照常抛出，不当成"还没有结束"。
   */
  observe(dispatchId: string): Promise<MailboxSettlement | undefined> {
    if (!safeDispatchId(dispatchId)) return Promise.reject(new Error(`dispatch id 不能用作文件名：${JSON.stringify(dispatchId)}`));
    return this.dispatchLock.run(dispatchId, async () => {
      const message = (await readJsonIfExists(this.inboxFile(dispatchId))) as OrcaMessage | undefined;
      if (message !== undefined) return { kind: 'done', message };
      const abort = (await readJsonIfExists(this.abortFile(dispatchId))) as AbortRecord | undefined;
      return abort === undefined ? undefined : { kind: 'abort', abort };
    });
  }

  /** done 返回 dispatch 的结束消息（还没有收到时为 undefined；文件损坏等错误抛出）。 */
  done(dispatchId: string): Promise<OrcaMessage | undefined> {
    if (!safeDispatchId(dispatchId)) return Promise.reject(new Error(`dispatch id 不能用作文件名：${JSON.stringify(dispatchId)}`));
    return this.dispatchLock.run(dispatchId, async () => (await readJsonIfExists(this.inboxFile(dispatchId))) as OrcaMessage | undefined);
  }

  /** abort 返回 dispatch 的中止记录（没有时为 undefined；文件损坏等错误抛出）。 */
  abort(dispatchId: string): Promise<AbortRecord | undefined> {
    if (!safeDispatchId(dispatchId)) return Promise.reject(new Error(`dispatch id 不能用作文件名：${JSON.stringify(dispatchId)}`));
    return this.dispatchLock.run(dispatchId, async () => (await readJsonIfExists(this.abortFile(dispatchId))) as AbortRecord | undefined);
  }

  /** forget 删除 dispatch 的结束消息和中止记录（dispatch id 不可用时什么也不做）。 */
  forget(dispatchId: string): Promise<void> {
    if (!safeDispatchId(dispatchId)) return Promise.resolve();
    return this.dispatchLock.run(dispatchId, async () => {
      await rm(this.inboxFile(dispatchId), { force: true });
      await rm(this.abortFile(dispatchId), { force: true });
    });
  }
}
