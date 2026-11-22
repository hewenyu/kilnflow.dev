import { randomUUID } from 'node:crypto';
import { constants as fsConstants, existsSync } from 'node:fs';
import { type FileHandle, mkdir, open, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fromBinary, toBinary } from '@bufbuild/protobuf';
import { Code, ConnectError } from '@connectrpc/connect';
import {
  type AITask,
  AITaskSchema,
  CredentialAccess,
  CredentialKind,
  type DevIssue,
  type DevWorkInProgress,
  Outcome,
  ReportKind,
  type TaskRef,
} from '../gen/kiln/airunner/v2/airunner_pb.js';
import { type AgentDef, agentDef, type AgentProbe } from './agents.js';
import { ErrorClass, errMessage, InfraError, infra, RevokedError, StaleError } from './errors.js';
import type { Git, GitAuth } from './git.js';
import { credentialAllowed, credentialCmd, execGitOf, GitOps, OpClosedError, refusedCmd, transportRepo } from './credentials.js';
import { type DoneOutcome, doneCmd, doneOutcomes, doneReceiptCmd, type DoneSend, type OrcaFallback } from './done.js';
import type { Redactor } from './redact.js';
import {
  emptyBinding,
  startUnknown,
  type BindingRecord,
  type CompleteRecord,
  type Journal,
  type JournalRecord,
  type ReclaimRecord,
  type ToolReply,
  type UnreadableRecord,
  type VerdictRecord,
} from './journal.js';
import { type Kiln, retryable, serverDeclined, sleep, withRetry } from './kiln.js';
import type { Logger } from './log.js';
import { type AbortRecord, type Mailbox, runNotFound } from './mailbox.js';
import { bindWorker, checkStart, releaseTerminal, settleStart, type StartCheck, type TerminalDeps } from './dispatch.js';
import { dispatchNotFound, type Orca, type OrcaMessage, type WorkerInfo, type WorkerRef } from './orca.js';
import type { Services } from './services.js';
import { LogShipper } from './logship.js';
import { ciLogTailBytes, formatVerification } from './verification.js';
import type { KeyedMutex } from './mutex.js';
import type { Toolchain } from './toolchain.js';
import { attemptComment, kindText, reclaimTarget, reclaimWorkspace, worktreeName } from './reclaim.js';
import type { SkillProvisioner } from './skill.js';

// 一次 AI 任务（attempt）的执行（PLAN #33.4）：领取即 running；准备仓库和测试服务；在 Orca 中启动主 Agent（启动幂等键
// 先写入 journal）；心跳续约、接收取消；等待 worker_done；校验并提交结果。Agent 用 kiln-orca task 与 Kiln 交互，
// 这些命令由 tool() 处理。连接器退出时不停止 Orca 中的 Agent：重启后按 journal 恢复，继续等待；节点被吊销时（revoke）
// 与租约失效相同，停止 Agent、不提交。
//
// 结束分三步，每一步的产物先写入 journal：确定结论（verdict）→ 推送交付、checkpoint → 记下要提交的请求（complete）→ 提交。
// 恢复时有 complete 就只重发它，有 verdict 就只重做推送、checkpoint，都没有才重新等待、校验（这之前没有任何副作用）。
//
// 并发规则（rec 的读写）：主流程（run）和 Agent 的工具调用（tool，来自本机 socket，随时可能到达）都在 await 两侧读写
// this.rec。规则只有两条：
// 1. 写 rec 的每一段都是同步的，写完调用 save()：Journal 在调用时同步取快照、同一个 attempt 的写入按调用顺序完成，
//    所以磁盘上最终是最新的状态，旧快照不会覆盖新状态。
// 2. 工具调用只写 claim、selectNote、claimReply（重试时重放的回复）和 done（task done 的请求 id），并且彼此串行；主流程在确定结论之前（Agent 结束、
//    或者因停止而结束等待时）先关闭工具（closeTools：之后到达或排队中的调用一律回复"已经停止"），再等正在进行的调用
//    结束（其中的重试等待被停止信号打断）。之后主流程读到的 claim、selectNote 就是最终的：领取成功与取消同时发生时结论一定按"已领取"处理（推
//    checkpoint），正在进行的调用也不会被截断成一半。
// 主流程自己写 stage、binding、services、baseCommit、verdict、complete，工具调用只读其中的 binding。
// run 中意料之外的错误（例如 journal 写不进去）由 abandon 处理：不丢下 Orca 中可能还在运行的 Agent。
//
// 终端的释放（PLAN #33.4）：Agent 结束（worker_done 或自己退出）、确认停止之后，以及 Orca 报告启动失败而已经建了 dispatch
// 时，都用 worker-release 回收它的终端（工作区随后回收，见下）。释放是与业务结果分开的清理责任（release）：结果未知时记进
// journal（rec.release），业务流程照常；结束时只剩它的记录为 releasing，由节点重试（不占容量，见 Node）。
//
// 工作区的回收（PLAN #33.10，reclaim.ts，审计和开发相同）：结果已经了结（提交被 Server 确认，或者已经失效）之后，在 journal
// 中记下回收的责任（rec.reclaim），Agent 确认停止、终端确认释放之后删除这次执行在 Orca 中的工作区和临时检出。结束时先
// 回收一次；没有完成（或者还要等停止、释放）时由节点重试（不占容量）。结果没有确认提交、意料之外的错误放弃执行时不回收
// （工作区留作诊断，由历史残留的周期清理兜底）。开发的交付、checkpoint 在提交结果之前已经推送并用 ls-remote 确认，回收在
// 提交结果被确认之后，所以恢复时（journal 在 finishing）重发请求、重做推送都不会遇到已经回收的工作区；没有推送的改动
// （推送失败、失效、吊销）照样回收，日志写明（reclaim.ts 的 reportUnpushed）。

export const maxReportBytes = 1 << 20;
/** taskTmpDir 是任务目录中给 Agent 写临时文件的子目录，与 Server 的 aiprompt.DirTmp 一致（prompt 中写的是它）。 */
export const taskTmpDir = 'tmp';
const maxReselect = 3;
const maxProgressText = 200;
const settledGraceChecks = 2;
/**
 * execEnvCmd 是 kiln-orca task exec 向连接器要环境变量设置的命令：回复 {"env": ToolchainEnv | null, "git": ExecGit | null}，
 * 任务没有工具链时 env 为 null，仓库地址不是 HTTP(S) 时 git 为 null（见 credentials.ts 的 execGitEnv）。exec 本身在 CLI 进程中
 * 运行 Agent 的命令（继承它的终端和工作目录），不经过连接器。
 */
export const execEnvCmd = 'exec-env';
/** gitExecDir 是任务目录中 git exec 镜像的父目录（见 gitexec.ts）。 */
export const gitExecDir = 'git-exec';
/**
 * credentialAttempts：签发凭据最多尝试的次数。同一个操作只在 Server 明确回答没有签发时重试（serverDeclined）；连接器自己的
 * 操作（准备仓库、推送）签发失败、结果不明时换新的操作重来，同样最多这么多次（withGit）。
 */
export const credentialAttempts = 5;
/** stoppedReply 是停止或开始结束之后给 Agent 工具调用的回复。 */
const stoppedReply = { code: 1, output: '这个任务已经停止或正在结束（取消、超时、失效或 Agent 已经结束），不要再继续。' };

export interface Timing {
  heartbeatMs: number;
  /** 检查邮箱中是否有结束消息的间隔。 */
  pollMs: number;
  /** 用 worker-show 检查 Agent 是否已经结束（没有发送 worker_done 就退出）的间隔。 */
  showMs: number;
  maxDisconnectedMs: number;
  completeTotalMs: number;
  /** 启动请求结果未知时用 request-show 查证的总时长。 */
  startVerifyMs: number;
  /** 心跳 RPC 的期限：Server 不响应时按断连处理，不卡住超时判断和退出。 */
  rpcMs: number;
  /** 签发凭据重试的初始退避（之后加倍）：同一操作在 Server 明确没有签发时重试，连接器自己的操作换新的操作重来。 */
  credentialRetryMs: number;
}

export const defaultTiming: Timing = {
  heartbeatMs: 5_000,
  pollMs: 2_000,
  showMs: 30_000,
  maxDisconnectedMs: 10 * 60_000,
  completeTotalMs: 30 * 60_000,
  startVerifyMs: 10 * 60_000,
  rpcMs: 60_000,
  credentialRetryMs: 1_000,
};

export interface AttemptDeps {
  kiln: Kiln;
  orca: Orca;
  git: Git;
  services: Services | undefined;
  /** 测试服务现在能不能用（不能时的原因）。 */
  servicesReady(): { ok: boolean; reason: string };
  /** 受管理的工具链（任务有 tools 时在准备阶段装好，PLAN #33.12）。 */
  toolchain: Toolchain;
  agents: AgentProbe;
  mailbox: Mailbox;
  journal: Journal;
  log: Logger;
  /** 节点共用的脱敏：签发的凭据登记在其中，上传的日志、错误说明、工具的回复中替换（PLAN #33.13）。 */
  redactor: Redactor;
  /** reposDir 是仓库 clone 的根目录（配置 clone_root）。 */
  reposDir: string;
  tasksDir: string;
  /** Agent 调用本机工具的命令前缀（{{KILN_TOOL}}）。 */
  toolCommand(attemptId: bigint): string;
  /** skill 在启动 Agent 之前把随包分发的 kiln-task skill 放到它的目录中（{{KILN_SKILL}} 指向入口文件）。 */
  skill: SkillProvisioner;
  /**
   * orcaFallback 是 kiln-orca task done 发送 worker_done 的后备 Orca 命令行（Agent 的环境中没有 ORCA_CLI_COMMAND 时用，
   * 见 host/home.ts 的 orcaFallback）。
   */
  orcaFallback: OrcaFallback;
  /** 每个仓库的 clone 一把锁：准备仓库（clone、fetch、worktree prune）与 Orca 的 worker-start（建 worktree）互斥。 */
  repoLock: KeyedMutex;
  orcaRepoIds: Map<string, string>;
  runtimeId(): string;
  timing: Timing;
  now(): number;
  /** onRevoked 在这个 attempt 的 RPC 得知节点被吊销时调用（节点据此停止所有 attempt，见 Node）。 */
  onRevoked?(): void;
}

export interface Result {
  outcome: Outcome;
  errorClass: string;
  message: string;
  reviewReport?: Uint8Array;
  delivery?: { report: Uint8Array; branch: string; sha: string; checkpoint: boolean };
  /** 交付通过校验，还没有推送（结束时推到 attempt 分支）。 */
  push?: boolean;
}

/**
 * Stop 是结束等待的原因。shutdown 只停止跟踪（Agent 继续运行，重启后恢复）。stale（租约失效）和 revoked（节点被吊销）
 * 都表示这个 attempt 已经不属于本节点，处理相同（见 lost）。
 */
type Stop = 'cancel' | 'timeout' | 'stale' | 'revoked' | 'disconnected' | 'shutdown';

/** lost：attempt 已经不属于本节点（失效或吊销）：优先于其他原因，之后不再调用 Server、不推送、不提交，只停止 Agent。 */
function lost(s: Stop | undefined): boolean {
  return s === 'stale' || s === 'revoked';
}

export function ok(message: string): Result {
  return { outcome: Outcome.SUCCEEDED, errorClass: '', message };
}
export function failed(errorClass: string, message: string): Result {
  return { outcome: Outcome.FAILED, errorClass, message };
}
export function fromError(err: unknown): Result {
  if (err instanceof InfraError) return { outcome: Outcome.INFRA_ERROR, errorClass: err.errorClass, message: err.message };
  return { outcome: Outcome.INFRA_ERROR, errorClass: ErrorClass.prepare, message: errMessage(err) };
}

/** SubstituteVars 是占位符要替换成的值（本机的路径、命令）。 */
export interface SubstituteVars {
  taskDir: string;
  tool: string;
  services: string;
  /** skill 是随包分发的 kiln-task skill 的 SKILL.md 路径（启动 Agent 之前安装好）。 */
  skill: string;
}

/** placeholders 是 Server 生成的文字中的占位符（airunner.v2 的 AITask.prompt）与 SubstituteVars 中对应的值。 */
const placeholders: Readonly<Record<string, keyof SubstituteVars>> = {
  '{{KILN_TASK_DIR}}': 'taskDir',
  '{{KILN_TOOL}}': 'tool',
  '{{KILN_SERVICES}}': 'services',
  '{{KILN_SKILL}}': 'skill',
};
const placeholderPattern = new RegExp(Object.keys(placeholders).map((k) => k.replace(/[{}]/g, '\\$&')).join('|'), 'g');

/**
 * substitute 替换 Server 生成的文字（prompt、领取后的说明）中的占位符。值按字面插入：用替换函数，值中的 $&、$1、$$ 等
 * 不按替换模式解释（路径可以含这些字符）；一次扫描完成，值中碰巧出现的占位符文字不会再被替换。
 */
export function substitute(text: string, v: SubstituteVars): string {
  return text.replace(placeholderPattern, (m) => v[placeholders[m]!]);
}

function issueJSON(is: DevIssue[]) {
  return is.map((i) => ({ number: i.number, title: i.title, body: i.body, labels: i.labels, url: i.url }));
}
function inProgressJSON(ws: DevWorkInProgress[]) {
  return ws.map((w) => ({ work_item_id: Number(w.workItemId), state: w.state, issues: issueJSON(w.issues), plan: w.plan }));
}

function b64(b: Uint8Array): string {
  return Buffer.from(b).toString('base64');
}
function unb64(s: string): Uint8Array {
  return new Uint8Array(Buffer.from(s, 'base64'));
}

/** nonBlocking：打开 FIFO 时不等写端（Windows 没有这个标志，为 0）；对普通文件没有影响。 */
const nonBlocking = fsConstants.O_NONBLOCK ?? 0;

/** ReportFileError 是 Agent 写出的报告文件不合格（超过上限、不是普通文件），与真正的文件系统 I/O 错误区分。 */
export class ReportFileError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'ReportFileError';
  }
}

/**
 * readLimited 读取任务目录中 Agent 写的文件（报告），最多读 limit + 1 字节就能判定超限：文件的大小由 Agent 决定，
 * 一个节点的多个任务共用一个进程，不能按文件的实际大小分配内存。以实际读到的字节数为准（读的过程中文件可能变大）。
 * 文件不存在时返回 undefined；不是普通文件（目录、FIFO、设备）、超过上限时抛出 ReportFileError，不会卡在打开或读取上；
 * 其余打开、读取失败照常抛出（真正的文件系统错误）。
 */
export async function readLimited(p: string, limit = maxReportBytes): Promise<Uint8Array | undefined> {
  let fh: FileHandle;
  try {
    fh = await open(p, fsConstants.O_RDONLY | nonBlocking);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return undefined;
    throw err;
  }
  try {
    if (!(await fh.stat()).isFile()) throw new ReportFileError(`${path.basename(p)} 不是普通文件`);
    const buf = Buffer.alloc(limit + 1);
    let n = 0;
    while (n < buf.length) {
      const { bytesRead } = await fh.read(buf, n, buf.length - n, n);
      if (bytesRead === 0) break;
      n += bytesRead;
    }
    if (n > limit) throw new ReportFileError(`${path.basename(p)} 超过 ${limit} 字节`);
    // 复制出读到的部分：不让调用方长期持有 limit + 1 字节的缓冲区
    return new Uint8Array(buf.subarray(0, n));
  } finally {
    await fh.close();
  }
}

/**
 * recordTask 还原 journal 记录中的任务（spec）：解不开（base64 / protobuf）、缺少 attempt、attempt 或 epoch 与记录不一致时
 * 抛出——这样的记录不能恢复，按读不了的记录处理（见 Node）。
 */
export function recordTask(rec: JournalRecord): AITask {
  const b64 = rec.spec;
  if (!/^[A-Za-z0-9+/]*={0,2}$/.test(b64) || b64.length % 4 !== 0) throw new Error('spec 不是合法的 base64');
  let spec: AITask;
  try {
    spec = fromBinary(AITaskSchema, Buffer.from(b64, 'base64'));
  } catch (err) {
    throw new Error(`spec 不是合法的任务（${errMessage(err)}）`, { cause: err });
  }
  if (!spec.ref) throw new Error('spec 中缺少 attempt');
  if (String(spec.ref.attemptId) !== rec.attemptId) throw new Error(`spec 中的 attempt ${spec.ref.attemptId} 与记录不一致`);
  if (String(spec.ref.epoch) !== rec.epoch) throw new Error(`spec 中的 epoch ${spec.ref.epoch} 与记录（${rec.epoch}）不一致`);
  return spec;
}

/**
 * Recovery 是 journal 中一条可以恢复的记录：stopping 只继续停止它的 Agent，releasing、reclaiming 只继续结束后的清理
 * （释放终端、回收工作区；都不需要任务），其它带着还原出的任务恢复执行。
 */
export type Recovery =
  | { kind: 'stop'; rec: JournalRecord }
  | { kind: 'cleanup'; rec: JournalRecord }
  | { kind: 'resume'; rec: JournalRecord; task: AITask };

/** recovery 判断 rec 怎么恢复；要恢复执行而任务还原不出来（或者清理阶段没有要清理的）时抛出。 */
export function recovery(rec: JournalRecord): Recovery {
  switch (rec.stage) {
    case 'stopping':
      return { kind: 'stop', rec };
    case 'releasing':
      if (!rec.release) throw new Error('阶段是 releasing 但没有要释放的终端（release）');
      return { kind: 'cleanup', rec };
    case 'reclaiming':
      if (!rec.reclaim) throw new Error('阶段是 reclaiming 但没有要回收的工作区（reclaim）');
      return { kind: 'cleanup', rec };
    default:
      return { kind: 'resume', rec, task: recordTask(rec) };
  }
}

/** JournalInspection 是 inspectJournal 的结果。 */
export interface JournalInspection {
  recoveries: Recovery[];
  /** 读不了的记录，包括任务还原不出来的。 */
  unreadable: UnreadableRecord[];
}

/** inspectJournal 读取 journal（节点恢复和 doctor 共用）：可以恢复的记录和读不了的记录。目录读不了时抛出。 */
export async function inspectJournal(j: Journal): Promise<JournalInspection> {
  const scan = await j.list();
  const out: JournalInspection = { recoveries: [], unreadable: [...scan.unreadable] };
  for (const rec of scan.records) {
    try {
      out.recoveries.push(recovery(rec));
    } catch (err) {
      out.unreadable.push({ attemptId: rec.attemptId, file: j.fileOf(rec.attemptId), error: errMessage(err) });
    }
  }
  return out;
}

export class Attempt {
  readonly spec: AITask;
  readonly ref: TaskRef;
  readonly id: bigint;
  private rec: JournalRecord;
  private readonly abort = new AbortController();
  /** exiting 在连接器退出时结束：正在提交结果时不再重试（journal 保留，重启后重新结束）。 */
  private readonly exiting = new AbortController();
  /** staleCtl 只在失效、吊销（lost）时结束：等待中的重试立即停止。取消、超时用 abort（之后仍要保存 checkpoint）。 */
  private readonly staleCtl = new AbortController();
  private stop: Stop | undefined;
  private stageText = '';
  private lastBeat: number;
  private readonly logs: LogShipper;
  private agent: AgentDef | undefined;
  private selectRounds = 0;
  /** 结束时没能确认 Orca 中的 Agent 已经停止：节点继续占用这份容量，稍后再停。 */
  unconfirmedStop = false;
  /**
   * 结果已经了结：提交被 Server 确认（包括 Server 已经有结果），或者 attempt 已经失效（不再提交）。审计只在这之后回收
   * 工作区（见 cleanup）。
   */
  private resultSettled = false;
  /** 工具已经关闭（结束流程开始）：之后的工具调用一律拒绝。 */
  private toolsClosed = false;
  /** 正在进行和排队中的工具调用（串行执行，toolTail 是最后一个）。 */
  private readonly toolCalls = new Set<Promise<unknown>>();
  private toolTail: Promise<unknown> = Promise.resolve();
  /** 这个 attempt 的 git 操作和它们的凭据（PLAN #33.13）。 */
  private readonly ops: GitOps;

  constructor(
    private readonly d: AttemptDeps,
    spec: AITask,
    rec?: JournalRecord,
  ) {
    this.spec = spec;
    if (!spec.ref) throw new Error('任务缺少 attempt');
    this.ref = spec.ref;
    this.id = spec.ref.attemptId;
    this.rec = rec ?? {
      attemptId: String(this.id),
      epoch: String(spec.ref.epoch),
      spec: Buffer.from(toBinary(AITaskSchema, spec)).toString('base64'),
      stage: 'prepare',
      binding: emptyBinding(),
      startedAt: d.now(),
    };
    this.lastBeat = d.now();
    this.selectRounds = rec?.selectRounds ?? 0;
    // 失效后不再上传日志（rpc 拒绝）；从 journal 恢复时 Server 上已经有这次执行的日志，先查询长度再接着传
    this.logs = new LogShipper((offset, data, signal) => this.rpc(() => d.kiln.appendLog(this.ref, offset, data, signal)), { resume: rec !== undefined });
    this.ops = new GitOps({
      issue: async ({ access, opId, repository }, signal) => {
        // 同一个操作（同一个 op_id）只在 Server 明确回答没有签发时重试；其余情形这个操作结束，由 GitOp 按原 op_id 关闭
        const r = await this.retry(() => d.kiln.issueCredential({ ref: this.ref, kind: CredentialKind.GIT, access, opId, repository }), {
          attempts: credentialAttempts,
          signal,
          retryable: serverDeclined,
          baseMs: d.timing.credentialRetryMs,
        });
        if (!r.credential) throw new Error('Server 没有给出凭据');
        return r.credential;
      },
      revoke: ({ opId, secret }) => this.rpc(() => d.kiln.revokeCredential({ ref: this.ref, kind: CredentialKind.GIT, opId, secret })),
      redactor: d.redactor,
      log: d.log,
      print: (line) => this.print(line),
      attemptId: this.rec.attemptId,
    });
  }

  /** fromJournal 按 journal 记录恢复 attempt；任务还原不出来（见 recordTask）时抛出。 */
  static fromJournal(d: AttemptDeps, rec: JournalRecord): Attempt {
    return new Attempt(d, recordTask(rec), rec);
  }

  /** repoLockKey：这个仓库的 clone 的锁（准备仓库、在 Orca 中建 worktree 互斥）。 */
  private get repoLockKey(): string {
    return String(this.spec.repoId);
  }

  /** clone 是这个仓库在连接器中的 clone（repos/<repo id>/<仓库名>）。 */
  private get clone(): string {
    return path.join(this.d.reposDir, String(this.spec.repoId), path.basename(this.spec.repository) || String(this.spec.repoId));
  }

  get taskDir(): string {
    return path.join(this.d.tasksDir, this.rec.attemptId);
  }
  get binding(): BindingRecord {
    return this.rec.binding;
  }
  get kind(): 'review' | 'develop' | undefined {
    return this.spec.kind.case;
  }
  get deadline(): number {
    const t = this.spec.timeoutSeconds > 0 ? this.spec.timeoutSeconds : 3600;
    return this.rec.startedAt + t * 1000;
  }

  private print(line: string): void {
    this.logs.write(`${new Date(this.d.now()).toISOString()} ${this.d.redactor.redact(line)}\n`);
  }

  /**
   * withGit 开一个访问任务仓库的操作（PLAN #33.13），签发凭据后执行 f，结束后（无论成败）关闭操作、吊销凭据。
   * 签发失败、结果不明（没有收到回复等可以重试的错误）时，这个操作照常按原 op_id 关闭，再换一个新的操作重来（最多
   * credentialAttempts 次，退避）：同一 op_id 不重放（Server 可能已经签出、也可能重启丢了登记）。f 本身的失败不重来。
   * 拿不到凭据时是基础设施错误（失效时抛出 StaleError）。
   *
   * stop 是调用方的停止信号（准备仓库传入准备阶段的停止：取消、超时、断连、连接器退出；交付的推送不传，shutdown 时照常
   * 推完）：退避等待立即结束、不再开新的操作；签发期间停止时关闭这个操作（已经发出的请求照常完成，迟到的凭据由关闭吊销，
   * 同一操作不再重试）；拿到凭据之后停止的不调用 f。失效（staleCtl）总是生效。
   */
  private async withGit<T>(access: CredentialAccess, f: (auth: GitAuth) => Promise<T>, stop?: AbortSignal): Promise<T> {
    let delay = this.d.timing.credentialRetryMs;
    for (let i = 1; ; i++) {
      // 停止之后不再开新的操作（不再签发）
      this.throwIfHalted(stop);
      // 签发失败作为结果返回（操作照常由 run 关闭），f 的错误原样抛出
      const r = await this.ops.run(this.spec.repository, access, async (op): Promise<{ ok: true; v: T } | { ok: false; err: unknown }> => {
        // 签发期间停止：关闭这个操作（同一操作的重试随之结束；已经发出的请求照常完成，迟到的凭据由关闭吊销）
        const onStop = () => void op.close();
        stop?.addEventListener('abort', onStop, { once: true });
        let auth: GitAuth;
        try {
          auth = await op.token();
        } catch (err) {
          return { ok: false, err };
        } finally {
          stop?.removeEventListener('abort', onStop);
        }
        // 拿到凭据之后、开始网络 git 之前停止了：不再执行（操作由 run 关闭）
        if (stop?.aborted) return { ok: false, err: undefined };
        return { ok: true, v: await f(auth) };
      });
      if (r.ok) return r.v;
      const issueErr = r.err;
      if (issueErr instanceof StaleError || this.stale) throw new StaleError();
      this.throwIfHalted(stop);
      const what = access === CredentialAccess.WRITE ? '推送用的 git 凭据' : 'git 凭据';
      if (i >= credentialAttempts || !retryable(issueErr)) {
        throw new InfraError(ErrorClass.credential, `向 Kiln Server 要${what}失败：${errMessage(issueErr)}`, { cause: issueErr });
      }
      this.d.log.warn(`要${what}失败，这个操作已关闭，换新的操作重来`, { attempt: this.rec.attemptId, try: i, err: errMessage(issueErr) });
      await sleep(delay, stop ? AbortSignal.any([this.staleCtl.signal, stop]) : this.staleCtl.signal);
      delay = Math.min(delay * 2, 30_000);
    }
  }

  /** throwIfHalted：失效时抛出 StaleError；stop（调用方给出的停止信号）已经结束时抛出"已停止"。 */
  private throwIfHalted(stop?: AbortSignal): void {
    if (this.stale) throw new StaleError();
    if (stop?.aborted) throw new InfraError(ErrorClass.agent, '已停止');
  }


  /**
   * push 用一个写操作把 worktree wt 推到 attempt 分支（推送并以 ls-remote 确认远端事实后关闭操作、吊销，PLAN #33.13）。
   * 失效、节点被吊销时推送被取消（见 Git.push）。
   */
  private async push(wt: string): Promise<{ branch: string; sha: string }> {
    // 签发之前先检查：失效后不再向 Server 要写凭据
    this.staleGuard();
    return this.withGit(CredentialAccess.WRITE, (auth) => this.d.git.push(wt, this.spec.cloneUrl, this.id, this.staleGuard, auth, this.staleCtl.signal));
  }

  private async save(): Promise<void> {
    await this.d.journal.save(this.rec);
  }

  private setStage(stage: JournalRecord['stage'], text: string): void {
    this.rec.stage = stage;
    this.stageText = text.slice(0, maxProgressText);
  }

  /**
   * shutdown 停止跟踪（连接器退出）：不停止 Orca 中的 Agent，不提交结果，journal 保留。Agent 已经结束、正在结束时，
   * 结束过程照常完成（RPC 有期限）；提交结果还在重试时放弃，重启后从 journal 重新结束（推送、提交都可以重复）。
   */
  shutdown(): void {
    this.exiting.abort();
    this.halt('shutdown');
  }

  /**
   * revoke：节点被吊销，收回执行权限。与租约失效相同（lost）：停止已经启动或可能已经启动的 Agent（启动结果还不知道时
   * 继续查证），不推交付、checkpoint，不提交结果，不再调用 Server；不能确认停止时照常记为 stopping。优先于 shutdown。
   */
  revoke(): void {
    this.halt('revoked');
  }

  /**
   * halt 记下停止的原因并结束等待。失效、吊销（lost）优先于其它原因（包括已经开始的退出）：之后不再写任何东西。
   * Agent 已经结束（finishing）后，结果已经确定，取消、超时、断连、退出都不再改变它。
   */
  private halt(s: Stop): void {
    if (lost(this.stop) || (!lost(s) && (this.stop || this.rec.stage === 'finishing'))) return;
    this.stop = s;
    this.abort.abort();
    if (lost(s)) this.staleCtl.abort();
  }

  /** stale：attempt 已经不属于本节点（失效或吊销）。 */
  private get stale(): boolean {
    return lost(this.stop);
  }

  /**
   * rpc 调用 Server。任何调用返回失效（StaleError）或节点被吊销（RevokedError），包括心跳、日志上传、工具中的调用，都让
   * attempt 进入 lost（吊销时同时通知节点，不必等下一次领取），并按失效抛出 StaleError；已经 lost 后不再调用 Server，
   * 直接抛出 StaleError：这些规则只写在这里。
   */
  private async rpc<T>(f: () => Promise<T>): Promise<T> {
    if (this.stale) throw new StaleError();
    try {
      return await f();
    } catch (err) {
      if (err instanceof RevokedError) {
        this.halt('revoked');
        this.d.onRevoked?.();
        throw new StaleError();
      }
      if (err instanceof StaleError) this.halt('stale');
      throw err;
    }
  }

  /** retry 带退避地重试 Server 调用（每一次都经过 rpc）；失效时退避等待立即结束、不再发下一次，抛出 StaleError。 */
  private async retry<T>(f: () => Promise<T>, opts: { attempts: number; signal?: AbortSignal; retryable?: (err: unknown) => boolean; baseMs?: number }): Promise<T> {
    const signal = opts.signal ? AbortSignal.any([opts.signal, this.staleCtl.signal]) : this.staleCtl.signal;
    try {
      return await withRetry(() => this.rpc(f), { attempts: opts.attempts, signal, retryable: opts.retryable, baseMs: opts.baseMs });
    } catch (err) {
      if (this.stale) throw new StaleError();
      throw err;
    }
  }

  /**
   * throwIfStopped 在发起新的工作（拉起测试服务、在 Orca 中启动 Agent）之前同步检查：已经停止（失效、取消、超时、断连、
   * 连接器退出）时不再发起。检查和发起之间不能有 await，否则等待期间到达的停止盖不住。
   */
  private throwIfStopped(): void {
    if (this.abort.signal.aborted) throw new InfraError(ErrorClass.agent, '已停止');
  }

  /** staleGuard 在真正启动 git push 之前同步检查：已经失效时抛出 StaleError，不再写远端。 */
  private readonly staleGuard = (): void => {
    if (this.stale) throw new StaleError();
  };

  /**
   * run 执行任务直到提交结果（或者失效、连接器退出）。resumed 为 true 时从 journal 恢复。意料之外的错误先由 abandon 处理
   * （停止 Agent 或者记为 unconfirmedStop），再抛给调用者。
   */
  async run(resumed = false): Promise<void> {
    const log = this.d.log;
    log.info(resumed ? '恢复执行' : '开始执行', { attempt: this.rec.attemptId, repo: this.spec.repository, kind: this.kind ?? '' });
    try {
      const beat = this.heartbeatLoop();
      let done: boolean;
      try {
        done = await this.conclude(resumed);
      } finally {
        await beat.stop();
      }
      if (!done) return;
      await this.cleanup();
    } catch (err) {
      await this.abandon(err);
      throw err;
    }
    log.info('执行结束', { attempt: this.rec.attemptId, outcome: Outcome[this.rec.complete?.outcome ?? Outcome.UNSPECIFIED], class: this.rec.complete?.errorClass ?? '' });
  }

  /** record 是这个 attempt 的 journal 记录（结束后节点据此继续停止没能确认停止的 Agent）。 */
  get record(): JournalRecord {
    return this.rec;
  }

  /**
   * agentLaunched：Orca 中可能有这个 attempt 的 Agent——已经知道 dispatch，或者启动请求可能已经发出而结果还不知道
   * （startUnknown，见 start 的不变量）。这两种情况结束时都要停止它（后一种先查证 dispatch）。
   */
  private get agentLaunched(): boolean {
    return !!this.binding.dispatchId || startUnknown(this.rec);
  }

  /** agentMayRun：Orca 中可能还有这个 attempt 的 Agent 在运行（已经启动，且还没有自己结束或被确认停止）。 */
  private get agentMayRun(): boolean {
    if (!this.agentLaunched) return false;
    return this.rec.stage !== 'finishing' || (this.rec.verdict?.stopAgent ?? false);
  }

  /**
   * abandon 处理 run 中意料之外的错误（例如磁盘满、journal 写不进去）：这次执行不再提交结果（Server 按失联结束它），
   * 但不丢下 Orca 中可能还在运行的 Agent：先停止它；不能确认停止时记为 unconfirmedStop（节点继续占用容量、定期再停），
   * 并尽量把 journal 记为 stopping（重启后继续停）；确认停止或者没有 Agent 时释放它的终端（见 release），删除 journal 和
   * 测试服务（终端的释放没有确认时 journal 记为 releasing）。连接器正在退出
   * （且没有失效、吊销）时什么都不做：Agent 继续运行，journal 保留，重启后按它恢复。journal 写不进去时 unconfirmedStop 仍然生效（只在内存中）。
   * 这里不抛出。
   */
  private async abandon(err: unknown): Promise<void> {
    this.toolsClosed = true;
    const log = this.d.log;
    log.error('执行任务时出现意料之外的错误，放弃这次执行（不提交结果）', { attempt: this.rec.attemptId, err: errMessage(err) });
    if (this.exiting.signal.aborted && !this.stale) return;
    await this.ops.closeAll();
    // Agent 已经结束时它的终端已经释放过（waitAndFinish 先释放，没有确认的在 rec.release 中，cleanup 记为 releasing）
    if (this.agentMayRun) await this.stopAgent();
    // 与正常结束相同：删除测试服务；没能确认停止时 journal 记为 stopping，只剩释放终端时记为 releasing，否则删除
    await this.cleanup().catch((e: unknown) => log.error('放弃执行后没能更新 journal（没能确认停止的 Agent 仍由节点继续停止）', { attempt: this.rec.attemptId, err: errMessage(e) }));
  }

  /**
   * conclude 执行到提交结果，返回这个 attempt 是否已经了结（提交了、已经有结果或已失效）。连接器退出时（Agent 还没结束，
   * 或者提交结果被打断）返回 false，journal 保留。
   */
  private async conclude(resumed: boolean): Promise<boolean> {
    const log = this.d.log;
    if (this.rec.complete) {
      this.print('连接器重启，重新提交记录的结果');
      this.unconfirmedStop = this.rec.verdict?.stopAgent ?? false;
    } else {
      let verdict = this.rec.verdict;
      if (verdict) {
        this.print('连接器重启，按记录的结论继续结束');
      } else {
        let res: Result;
        try {
          res = await this.execute(resumed);
        } catch (err) {
          res = fromError(err);
        }
        // 停止（取消、超时等）时等待在这里结束：确定结论之前关闭工具、等正在进行的调用结束（Agent 正常结束时
        // waitAndFinish 已经关闭）
        await this.closeTools();
        if (this.stop === 'shutdown') {
          // 连接器退出：Agent 继续运行（重启后恢复），进行中的传输的凭据不由连接器收回（随 socket 关闭尽力吊销，否则自然过期）
          await this.logs.flush().catch(() => undefined);
          return false;
        }
        // 启动结果还不知道（查证被停止打断、查证超时）时同样按"Agent 可能在运行"处理：业务结果照常确定，停止时先查证
        const stopAgent = this.agentLaunched && this.rec.stage !== 'finishing';
        // closeTools 已经关闭并等完 Agent 的 git 传输；这里收尾其余还没有关闭的操作（推送用之后单独开的写操作）
        await this.ops.closeAll();
        if (this.stale) {
          // 失效：不记结论、不推送、不提交，只停止还在运行的 Agent（这个 attempt 已经了结，不会再提交）
          if (stopAgent) await this.stopAgent();
          this.resultSettled = true;
          return true;
        }
        verdict = this.verdict(res, stopAgent);
        this.rec.verdict = verdict;
        this.setStage('finishing', '保存结果');
        await this.save();
      }
      const res = await this.effects(verdict);
      if (this.stale) {
        this.resultSettled = true;
        return true;
      }
      this.rec.complete = this.completeRecord(res);
      this.setStage('finishing', '提交结果');
      await this.save();
    }
    if (!this.stale) await this.logs.close().catch((err) => log.warn('上传日志失败', { err: errMessage(err) }));
    if (this.stale) {
      this.resultSettled = true;
      return true;
    }
    if (await this.complete(this.rec.complete)) return true;
    // 连接器退出时还没提交成功：journal 保留，重启后重发同一个请求
    log.warn('连接器退出，结果没有提交，重启后重新提交', { attempt: this.rec.attemptId });
    return false;
  }

  private async execute(resumed: boolean): Promise<Result> {
    if (this.kind === undefined) throw new InfraError(ErrorClass.prepare, '未知的任务类型');
    this.agent = agentDef(this.spec.agent);
    if (!this.agent) throw new InfraError(ErrorClass.prepare, `这个节点不认识 Agent ${this.spec.agent}`);
    // 开始之前已经停止（例如节点已被吊销时恢复的 attempt）：不再准备仓库、测试服务
    this.throwIfStopped();
    if (resumed && this.rec.stage !== 'prepare') {
      this.print('连接器重启，恢复执行');
      // 恢复时不重建任务目录：保证 prompt 中给出的临时目录存在
      await this.ensureTmpDir();
    } else {
      const early = await this.prepare();
      if (early) return early;
    }
    this.throwIfStopped();
    if (!this.binding.dispatchId) await this.start();
    return await this.waitAndFinish();
  }

  // ---- 准备 ----

  /** prepare 准备仓库、任务目录和测试服务。选题没有候选时直接返回结果（不启动 Agent）。 */
  private async prepare(): Promise<Result | undefined> {
    this.setStage('prepare', '准备仓库');
    await this.save();
    const sp = this.spec;
    this.print(`AI ${this.kind === 'review' ? '审计' : '开发'} · ${sp.repository} · ${sp.displayName} · 任务 #${sp.taskId} · attempt #${this.id}`);
    const clone = this.clone;
    const review = sp.kind.case === 'review' ? sp.kind.value : undefined;
    const base = review?.baseSha ?? '';
    // 准备仓库是一个读操作，凭据只在真正的网络操作期间存在：取得仓库锁、确认任务仍可执行之后才签发，prepareClone 返回
    // 即关闭、吊销；之后在锁内做不需要凭据的 Orca 注册
    const missing = await this.d.repoLock.run(this.repoLockKey, async () => {
      this.throwIfStopped();
      // 准备阶段的停止（取消、超时、断连、连接器退出）同时结束签发的重试：等待中立即返回、不再开新的操作，拿到凭据之后
      // 停止的不再 clone / fetch（交付的推送不接收这个信号：shutdown 时照常推完）
      const m = await this.withGit(
        CredentialAccess.READ,
        (auth) => this.d.git.prepareClone(clone, sp.cloneUrl, [sp.sha, base], review?.prNumber ?? 0, auth, this.abort.signal),
        this.abort.signal,
      );
      if (!this.d.orcaRepoIds.has(clone)) this.d.orcaRepoIds.set(clone, await this.d.orca.repoAdd(clone));
      return m;
    });
    // 审计必须从 base 开始（Agent 加载的说明不能来自被审的 PR）：拿不到 base 时不执行
    if (base && missing.includes(base)) throw new InfraError(ErrorClass.fetch, `获取不到审计的 base commit ${base}`);
    await rm(this.taskDir, { recursive: true, force: true });
    await mkdir(this.taskDir, { recursive: true, mode: 0o700 });
    await this.ensureTmpDir();

    if (sp.kind.case === 'develop') {
      const dev = sp.kind.value;
      if (dev.workData.length > 0) await writeFile(path.join(this.taskDir, 'work.json'), dev.workData, { mode: 0o600 });
      if (dev.mode === 'select') {
        this.stageText = '从任务池取候选';
        const offer = await this.offer();
        if (offer.candidates.length === 0) {
          this.print('任务池中没有可选的 Issue，结束');
          return ok('任务池中没有可选的 Issue');
        }
        this.print(`候选 ${offer.candidates.length} 个，进行中的工作项 ${offer.inProgress.length} 个`);
        await this.writeCandidates(offer.candidates, offer.inProgress);
      }
    }

    if (sp.tools.length > 0) {
      this.stageText = '准备工具链';
      this.throwIfStopped();
      try {
        this.rec.toolchain = await this.d.toolchain.ensure(sp.tools, { signal: this.abort.signal, print: (l) => this.print(l) });
      } catch (err) {
        // 停止（取消、超时、退出）打断的安装按停止处理，由结束流程决定结论
        this.throwIfStopped();
        throw infra(ErrorClass.toolchain, new Error(`准备工具链失败：${errMessage(err)}`));
      }
      this.print(`工具链：${this.rec.toolchain.tools.map((t) => `${t.name} ${t.version}`).join('、')}（Agent 用 kiln-orca task exec 运行）`);
      await this.save();
    }

    if (sp.services.length > 0) {
      const ready = this.d.servicesReady();
      if (this.d.services && ready.ok) {
        this.stageText = '拉起测试服务';
        // 准备仓库、取候选期间可能已经停止：不再为它拉起测试服务
        this.throwIfStopped();
        try {
          const st = await this.d.services.start(this.id, this.taskDir, sp.services);
          this.rec.services = { project: st.project, desc: st.desc };
          for (const n of st.notes) this.print(n);
        } catch (err) {
          throw infra(ErrorClass.services, err);
        }
      } else {
        this.rec.services = { project: '', desc: [`测试服务（${sp.services.join('、')}）不可用：${ready.reason || '没有 Docker'}。需要它们的验证记为没有运行`] };
      }
    }
    // 审计的 worktree 建在 base（Agent 启动时加载的说明来自 base），开发建在 sha
    this.rec.binding.worktreeName = worktreeName(this.id, this.ref.epoch);
    this.rec.baseCommit = base || sp.sha;
    this.rec.binding.worktreePath = '';
    await this.save();
    return undefined;
  }

  private async ensureTmpDir(): Promise<void> {
    await mkdir(path.join(this.taskDir, taskTmpDir), { recursive: true, mode: 0o700 });
  }

  private async offer() {
    for (;;) {
      const r = await this.retry(() => this.d.kiln.offerDevCandidates(this.ref), { attempts: 5, signal: this.abort.signal });
      if (r.waitSeconds <= 0) return r;
      await sleep(r.waitSeconds * 1000, this.abort.signal);
      if (this.abort.signal.aborted) throw new InfraError(ErrorClass.prepare, '已停止');
    }
  }

  private async writeCandidates(c: DevIssue[], w: DevWorkInProgress[]): Promise<void> {
    await writeFile(path.join(this.taskDir, 'candidates.json'), JSON.stringify(issueJSON(c), null, 2), { mode: 0o600 });
    await writeFile(path.join(this.taskDir, 'in_progress.json'), JSON.stringify(inProgressJSON(w), null, 2), { mode: 0o600 });
  }

  private vars(skill = '') {
    const svc = this.rec.services?.desc ?? [];
    return {
      taskDir: this.taskDir,
      tool: this.d.toolCommand(this.id),
      services: svc.length > 0 ? svc.join('\n') : '这个任务不需要测试服务。',
      skill,
    };
  }

  // ---- 启动 ----

  /**
   * start 在 Orca 中启动主 Agent。不变量：journal 中有 startRequestId ⇔ 启动请求可能已经发出（恢复时据此查证，查不到记为
   * infra.lost，绝不再启动一份）。所以先写入幂等键再发请求；写入之后、发出之前得知停止时，清除幂等键并写回 journal：
   * 这次确定没有发出，退出后重启会重新启动，失效、取消按原有流程结束，不会被误判为 infra.lost。只剩写入后进程崩溃
   * （来不及清除）这一个窗口，按"可能已经发出"保守处理。
   */
  private async start(): Promise<void> {
    const agent = this.agent as AgentDef;
    const clone = this.clone;
    let ref: WorkerRef;
    if (!this.binding.startRequestId) {
      // 先把随包分发的 kiln-task skill 放到这个 Agent 的目录：拿不到入口文件路径就不启动 Agent（绝不在没有 skill 的
      // 情况下发 worker-start）。恢复时已有启动幂等键的分支不需要它：那次启动已经发出，只查证结果。
      let skillPath: string;
      try {
        skillPath = await this.d.skill.ensureForAgent(this.spec.agent);
      } catch (err) {
        throw new InfraError(ErrorClass.prepare, `准备 ${this.spec.agent} 的 kiln-task skill 失败：${errMessage(err)}`, { cause: err });
      }
      let repoId = this.d.orcaRepoIds.get(clone);
      if (!repoId) {
        repoId = await this.d.orca.repoAdd(clone);
        this.d.orcaRepoIds.set(clone, repoId);
      }
      const { handle, runId } = await this.d.mailbox.ensure();
      const base = this.rec.baseCommit || this.spec.sha;
      // Orca 的 worker-start 在共用的 clone 中建 worktree，与准备仓库（fetch、worktree prune）互斥：fetch 读到建了一半的
      // worktree 会失败（见 git.ts 的 halfBuiltWorktree）。锁只包住这一次 worker-start（到它返回或抛出），查证（verifyStart，
      // 可能要等很久）在锁外。等锁是一次 await：停止检查放在拿到锁之后，检查与发出之间仍然没有 await。
      // 锁的顺序：attempt 只取 repoLock 这一把锁，持有期间调用的 journal.save、Orca 都不会再取 repoLock，没有循环等待。
      const sent = await this.d.repoLock.run(this.repoLockKey, async (): Promise<{ ref: WorkerRef } | { err: unknown }> => {
        this.throwIfStopped();
        // 先记下启动幂等键，再发出启动请求：响应丢失、连接器崩溃后都能查证，不再启动一份
        this.binding.startRequestId = randomUUID();
        this.binding.runtimeId = this.d.runtimeId();
        this.setStage('start', `在 Orca 中启动 ${agent.name}`);
        await this.save();
        if (this.abort.signal.aborted) {
          // 写入期间停止了：请求没有发出，恢复成"没有发出过启动请求"
          this.binding.startRequestId = '';
          this.binding.runtimeId = '';
          await this.save();
          this.throwIfStopped();
        }
        try {
          // 从上面的检查到这里没有 await：已经停止的任务不会再启动 Agent
          return {
            ref: await this.d.orca.workerStart({
              from: handle,
              run: runId,
              requestId: this.binding.startRequestId,
              spec: substitute(this.spec.prompt, this.vars(skillPath)),
              title: `Kiln ${this.kind === 'review' ? '审计' : '开发'} · ${this.spec.repository} · attempt ${this.id}`,
              repoId,
              name: this.binding.worktreeName,
              baseBranch: base,
              // 任务类型写进备注：回收历史残留时据此确认是这个版本建的工作区（reclaim.ts 的 sweepResidue）
              comment: attemptComment({ attemptId: this.id, epoch: this.ref.epoch, taskId: this.spec.taskId, kind: this.kind ?? 'develop' }),
              agent: agent.orca,
            }),
          };
        } catch (err) {
          return { err };
        }
      });
      // Orca 中已经没有邮箱的 Run（编排状态被重置）：告诉邮箱换新，这次启动按 Orca 的错误结束（verifyStart）
      if ('err' in sent && runNotFound(sent.err)) this.d.mailbox.runLost(runId);
      ref = 'ref' in sent ? sent.ref : await this.verifyStart(sent.err);
    } else {
      this.setStage('start', '查证 Orca 中的启动请求');
      ref = await this.verifyStart(undefined);
    }
    bindWorker(this.binding, ref);
    if (!this.binding.worktreePath) {
      this.binding.worktreePath = (await this.d.orca.workerShow(ref.dispatchId).catch(() => undefined))?.worktreePath ?? '';
    }
    this.setStage('running', `${agent.name} 执行中`);
    await this.save();
    this.print(`已在 Orca 中启动 ${agent.name}：dispatch ${ref.dispatchId}，worktree ${this.binding.worktreePath || this.binding.worktreeName}`);
  }

  /** terminalDeps 是查证启动请求、释放终端（dispatch.ts）用的依赖：释放的结果写进这个任务的日志。 */
  private get terminalDeps(): TerminalDeps {
    return { orca: this.d.orca, log: this.d.log, now: this.d.now, print: (line) => this.print(line) };
  }

  /** checkStartOnce 查证一次启动请求，并按结论更新记录（settleStart：绑定、记启动失败并回收留下的终端）。 */
  private async checkStartOnce(startErr?: unknown): Promise<StartCheck> {
    const c = await checkStart(this.d.orca, this.binding.startRequestId, startErr);
    await settleStart(this.terminalDeps, this.rec, c);
    return c;
  }

  /**
   * verifyStart 在启动请求的结果未知时（响应出错、连接器重启）用 request-show 查证（对记录的处理见 dispatch.ts 的
   * StartCheck）：已完成就沿用回执；进行中、查询失败就等；Orca 明确没有启动（failed）时记为 infra.agent（Agent 退避）；查不到时
   * 不能证明没有启动，记为 infra.lost，绝不再启动一份。查证超时、被停止打断时同样是 infra.lost（不是 infra.agent：
   * Agent 不退避）。除了 Orca 明确拒绝，抛出时启动幂等键都留在 binding 中（startUnknown）：结束流程据此继续查证、停止
   * 可能在运行的 Agent，不会因为没有 dispatch 就放下它。
   */
  private async verifyStart(err: unknown): Promise<WorkerRef> {
    const until = this.d.now() + this.d.timing.startVerifyMs;
    for (;;) {
      const c = await this.checkStartOnce(err);
      switch (c.kind) {
        case 'started':
          return c.worker;
        case 'failed':
          // Run 不存在不是 Agent 的问题：不退避 Agent（邮箱已经在换新 Run）
          if (runNotFound(err)) throw new InfraError(ErrorClass.orca, `Orca 中已经没有连接器邮箱的 Run（Orca 的编排状态被重置），没有启动 Agent：${c.error}`);
          this.d.agents.failed(this.spec.agent);
          throw new InfraError(ErrorClass.agent, `在 Orca 中启动 Agent 失败：${c.error}`);
        case 'absent':
          throw new InfraError(ErrorClass.lost, `无法确认 Orca 是否已经启动 Agent（启动请求 ${this.binding.startRequestId} 没有记录）${err ? '：' + errMessage(err) : ''}`);
        case 'pending':
        case 'unknown':
          break;
        default: {
          const _exhaustive: never = c;
          return _exhaustive;
        }
      }
      if (this.abort.signal.aborted) {
        throw new InfraError(ErrorClass.lost, `停止时还没有查明 Orca 中的启动请求 ${this.binding.startRequestId} 的结果（结束时继续查证、停止）`);
      }
      if (this.d.now() >= until) {
        throw new InfraError(ErrorClass.lost, `无法确认 Orca 中的启动请求 ${this.binding.startRequestId} 的结果`);
      }
      await sleep(this.d.timing.pollMs, this.abort.signal);
    }
  }

  // ---- 等待与结束 ----

  /**
   * waitAndFinish 等待 Agent 结束：邮箱落盘的 worker_done，或者 Orca 报告的中止（致命的 escalation），或者 worker-show 兜底
   * （Agent 没有发送 worker_done 就结束了）。中止表示这一轮无法继续：即使 Agent 的进程还在，也直接按 ai.runtime 结束，不进
   * 校验——不能把半截的报告当成正常结束，也不能在 select 任务还没有领取时误判成"没有领取 Issue"的成功。worker-show
   * 查询失败时不能断定 Agent 的状态，继续等；Orca 明确报告没有这个 dispatch（编排状态被重置：Agent 发往旧 Run 的
   * worker_done 再也收不到）时，连续几次（与已经结束相同的宽限）之后以 infra.orca 结束，按结束的流程停止、释放（Orca 中
   * 没有这个 dispatch 时都按已完成处理），不等到超时。
   */
  private async waitAndFinish(): Promise<Result> {
    const dispatch = this.binding.dispatchId;
    let lastShow = 0;
    let settled = 0;
    let missing = 0;
    let showError = '';
    for (;;) {
      if (this.abort.signal.aborted) throw new InfraError(ErrorClass.agent, '已停止');
      const ev = await this.d.mailbox.observe(dispatch);
      if (ev?.kind === 'done') {
        this.printDone(ev.message);
        break;
      }
      if (ev?.kind === 'abort') return this.aborted(ev.abort);
      if (this.d.now() - lastShow >= this.d.timing.showMs) {
        lastShow = this.d.now();
        let w: WorkerInfo | undefined;
        try {
          w = await this.d.orca.workerShow(dispatch);
          showError = '';
          missing = 0;
        } catch (err) {
          missing = dispatchNotFound(err) ? missing + 1 : 0;
          // 查询失败（包括 Orca 的输出缺少字段）不能断定 Agent 的状态：继续等 worker_done，原因写进日志（相同的只写一次）
          const m = errMessage(err);
          if (m !== showError) this.print(`查询 Orca 中的 Agent 状态失败：${m}`);
          showError = m;
        }
        if (missing > settledGraceChecks) {
          throw new InfraError(ErrorClass.orca, `Orca 中已经没有这个 Agent 的 dispatch ${dispatch}（Orca 的编排状态被重置），无法再跟踪它的结束`);
        }
        if (w && (['completed', 'failed', 'stopped', 'released'].includes(w.dispatch) || w.terminal === 'exited')) {
          // 结束消息可能还没有被邮箱读到：连续几次都没有才认为 Agent 没有发送 worker_done 就结束了。按 worker-show 结束之前
          // 再读一次邮箱：兜底的瞬间到达的 worker_done、中止记录都要按它们处理，不能把半截的报告文件当成正常结束
          if (++settled > settledGraceChecks) {
            const finalEv = await this.d.mailbox.observe(dispatch);
            if (finalEv?.kind === 'done') {
              this.printDone(finalEv.message);
              break;
            }
            if (finalEv?.kind === 'abort') return this.aborted(finalEv.abort);
            this.print(`${this.agent?.name} 没有发送 worker_done 就结束了（dispatch ${w.dispatch}，终端 ${w.terminal}）`);
            break;
          }
        } else {
          settled = 0;
        }
      }
      await sleep(this.d.timing.pollMs, this.abort.signal);
    }
    // Agent 已经结束：先同步记为 finishing（之后的取消、超时不再改变结果），再关闭工具、等正在进行的调用结束
    // （校验要读 claim、selectNote）
    this.setStage('finishing', '校验结果');
    await this.closeTools();
    // Agent 的这一轮已经结束：先释放它的终端（只关终端，工作区不动），再记 finishing、校验、推送。释放的结果不影响业务
    // 流程：没有确认时记为待释放（随 finishing 写进 journal），结束后由节点重试。先释放再写 journal：之后的任何出错
    // （包括写 journal 失败）都不会跳过释放；释放后、写入前崩溃时重启会重新等到 worker_done，再释放一次（already_released）
    await this.release(dispatch);
    await this.save();
    return this.kind === 'review' ? await this.finishReview() : await this.finishDevelop();
  }

  /** printDone 把邮箱落盘的 worker_done 写进任务日志。 */
  private printDone(msg: OrcaMessage): void {
    this.print(`${this.agent?.name} 已结束（worker_done：${msg.outcome || '未知'}）${msg.body ? '：' + msg.body.slice(0, 500) : ''}`);
  }

  /**
   * aborted 按 Orca 报告的中止结束：记下诊断（abort 落盘前已经脱敏、限长），以 ai.runtime 失败。不停在这里等 Agent 自己结束，
   * 也不进入 finishing 的校验；之后的结束流程照常停止 Agent（workerStop → workerRelease）、保存已经领取的工作的 checkpoint、
   * 提交结果。不调用 agents.failed()：一次任务的安全检查、凭据问题不代表这台机器上的 Agent 不能用，Agent 因此不进入退避
   * （退避只由 infra.agent 的失败触发）。
   */
  private aborted(a: AbortRecord): Result {
    this.print(`Orca 报告 Agent 运行中断，按失败结束这个任务：${a.logDetail}`);
    return failed(ErrorClass.runtime, a.reason);
  }

  private async finishReview(): Promise<Result> {
    let b: Uint8Array | undefined;
    try {
      b = await readLimited(path.join(this.taskDir, 'report.json'));
    } catch (e) {
      // Agent 写出的文件不合格（超限、不是普通文件）是 AI 失败；真正的文件系统错误是基础设施错误
      if (e instanceof ReportFileError) return failed(ErrorClass.report, e.message);
      throw infra(ErrorClass.report, e);
    }
    if (!b) return failed(ErrorClass.report, 'Agent 没有写出审计报告');
    const v = await this.validate(ReportKind.REVIEW, b);
    if (!v.valid) return failed(ErrorClass.report, `审计报告不合格：${v.problems.join('；')}`);
    return { ...ok('审计完成'), reviewReport: b };
  }

  private async finishDevelop(): Promise<Result> {
    const dev = this.spec.kind.case === 'develop' ? this.spec.kind.value : undefined;
    if (dev?.mode === 'select' && !this.rec.claim) {
      return ok(this.rec.selectNote || '没有领取 Issue');
    }
    let b: Uint8Array | undefined;
    try {
      b = await readLimited(path.join(this.taskDir, 'delivery.json'));
    } catch (e) {
      // 与 finishReview 相同：文件不合格是 AI 失败，文件系统错误是基础设施错误
      if (e instanceof ReportFileError) return failed(ErrorClass.delivery, e.message);
      throw infra(ErrorClass.delivery, e);
    }
    if (!b) return failed(ErrorClass.delivery, 'Agent 没有写出交付报告');
    const v = await this.validate(ReportKind.DELIVERY, b);
    if (!v.valid) return failed(ErrorClass.delivery, `交付报告不合格：${v.problems.join('；')}`);
    const report = JSON.parse(Buffer.from(b).toString('utf8')) as { status?: string; summary?: string; design?: { documents?: string[] } };
    if (report.status !== 'delivered') {
      return { ...ok(`${report.status ?? '未交付'}：${report.summary ?? ''}`), delivery: { report: b, branch: '', sha: '', checkpoint: false } };
    }
    const problems = await this.checkGit(report.design?.documents ?? []);
    if (problems.length > 0) return { ...failed(ErrorClass.delivery, `交付不合格：${problems.join('；')}`), delivery: { report: b, branch: '', sha: '', checkpoint: false } };
    return { ...ok(report.summary ?? '已交付'), delivery: { report: b, branch: '', sha: '', checkpoint: false }, push: true };
  }

  private phase(): string {
    const dev = this.spec.kind.case === 'develop' ? this.spec.kind.value : undefined;
    return this.rec.claim?.phase || dev?.phase || 'implement';
  }

  private async checkGit(documents: string[]): Promise<string[]> {
    const wt = this.binding.worktreePath;
    if (!wt || !existsSync(wt)) return [`找不到 Orca 的 worktree（${wt || this.binding.worktreeName}）`];
    const dev = this.spec.kind.case === 'develop' ? this.spec.kind.value : undefined;
    return this.d.git.checkDelivered(wt, { start: this.spec.sha, design: this.phase() === 'design', baseRef: dev?.baseRef ?? '', documents });
  }

  /** validate 请 Server 校验报告；signal 结束时不再重试（工具调用传入停止信号）。 */
  private validate(kind: ReportKind, report: Uint8Array, signal?: AbortSignal) {
    return this.retry(() => this.d.kiln.validateReport({ ref: this.ref, kind, report }), { attempts: 5, signal });
  }

  /** verdict 按停止的原因确定结论（还没有任何副作用）。stopAgent：Agent 还没有结束，要先停止它。 */
  private verdict(res: Result, stopAgent: boolean): VerdictRecord {
    const s = this.stop;
    if (s === 'cancel') res = { outcome: Outcome.CANCELLED, errorClass: '', message: '已取消' };
    else if (s === 'timeout') res = { outcome: Outcome.TIMED_OUT, errorClass: ErrorClass.timeout, message: `超过 ${this.spec.timeoutSeconds} 秒` };
    else if (s === 'disconnected') res = { outcome: Outcome.INFRA_ERROR, errorClass: ErrorClass.disconnected, message: '长时间连不上 Kiln Server，已停止执行' };
    const dev = this.spec.kind.case === 'develop' ? this.spec.kind.value : undefined;
    return {
      outcome: res.outcome,
      errorClass: res.errorClass,
      message: this.d.redactor.redact(res.message),
      reviewReport: res.reviewReport && b64(res.reviewReport),
      deliveryReport: res.delivery && b64(res.delivery.report),
      push: !!res.push,
      checkpoint: !!dev && (dev.mode !== 'select' || !!this.rec.claim),
      stopAgent,
    };
  }

  /**
   * effects 按结论做结束时的副作用：停止还在运行的 Agent、推送交付、已经领取的工作没有成功交付时推 checkpoint。
   * 这些都可以重复（推送是 force 推同一个 attempt 分支；已经有 checkpoint commit 时不再提交，推同一个 commit）。
   * 失效后不再推送。返回要提交的结果。
   */
  private async effects(v: VerdictRecord): Promise<Result> {
    if (v.stopAgent) {
      await this.stopAgent();
      if (!this.unconfirmedStop) {
        v.stopAgent = false;
        await this.save();
      }
    }
    let res: Result = { outcome: v.outcome, errorClass: v.errorClass, message: v.message };
    if (v.reviewReport !== undefined) res.reviewReport = unb64(v.reviewReport);
    const report = v.deliveryReport !== undefined ? unb64(v.deliveryReport) : undefined;
    if (report) res.delivery = { report, branch: '', sha: '', checkpoint: false };
    if (this.stale) return res;
    if (v.push && report) {
      this.stageText = '推送交付';
      try {
        const { branch, sha } = await this.push(this.binding.worktreePath);
        this.print(`已推送交付 ${sha} 到 ${branch}`);
        res.delivery = { report, branch, sha, checkpoint: false };
      } catch (err) {
        if (err instanceof StaleError) return res;
        this.print(`推送交付失败：${errMessage(err)}`);
        res = fromError(err);
      }
    }
    if (v.checkpoint && !(res.outcome === Outcome.SUCCEEDED && res.delivery?.sha)) res = await this.checkpoint(res);
    return res;
  }

  /**
   * stopAgent 停止 Orca 中的 Agent，不能确认时记为 unconfirmedStop。启动结果还不知道（startUnknown）时先查一次
   * request-show（对记录的处理见 dispatch.ts 的 StartCheck）：查到 dispatch 就停止它；Orca 报告启动失败时没有要停的（它建了的
   * dispatch 的终端已经回收或记为待释放）；仍然不知道（进行中、没有记录、查询失败）
   * 时不能确认停止：记为 unconfirmedStop（不推 checkpoint，journal 记为 stopping 带启动幂等键，节点继续查证、停止）。
   */
  private async stopAgent(): Promise<void> {
    // 查到 dispatch 时绑定；Orca 报告启动失败时记下，并回收它建了的 dispatch 的终端（见 settleStart）
    if (startUnknown(this.rec)) await this.checkStartOnce();
    const d = this.binding.dispatchId;
    if (!d) {
      this.unconfirmedStop = startUnknown(this.rec);
      if (this.unconfirmedStop) this.print(`Orca 中的启动请求 ${this.binding.startRequestId} 的结果还不知道，Agent 可能在运行：这个任务继续占用节点的容量，查明后停止`);
      return;
    }
    try {
      this.unconfirmedStop = !(await this.d.orca.workerStop(d));
    } catch (err) {
      this.unconfirmedStop = true;
      this.print(`停止 Orca 中的 Agent 失败：${errMessage(err)}`);
    }
    if (this.unconfirmedStop) {
      this.print('没能确认 Orca 中的 Agent 已经停止：这个任务继续占用节点的容量，稍后再停');
      return;
    }
    // 停止（包括早已结束：alreadySettled）不释放终端，Orca 中它仍是 retained：同样释放
    await this.release(d);
  }

  /** release 释放 dispatch 的终端，没有确认时记为待释放（见 releaseTerminal：业务流程不等待、不重试）。 */
  private async release(dispatchId: string): Promise<void> {
    await releaseTerminal(this.terminalDeps, this.rec, dispatchId);
  }

  /** checkpoint：已经领取（或返修、实现）的工作没有成功交付时，把 worktree 中的改动提交为 checkpoint 推到 attempt 分支。 */
  private async checkpoint(res: Result): Promise<Result> {
    const wt = this.binding.worktreePath;
    if (!wt || !existsSync(wt) || this.unconfirmedStop) return res;
    if (this.stale) return res;
    try {
      if (!(await this.d.git.commitCheckpoint(wt, this.spec.sha, this.id))) return res;
      const { branch, sha } = await this.push(wt);
      this.print(`已把未完成的改动推到 checkpoint ${branch}（${sha}）`);
      const report = res.delivery?.report ?? new Uint8Array();
      return { ...res, delivery: { report, branch, sha, checkpoint: true } };
    } catch (err) {
      if (err instanceof StaleError) return res;
      // 不特殊处理：工作区照样回收，回收时日志写明删掉的没有推送的改动（PLAN #33.10）
      this.print(`无法保存 checkpoint，没有推送的改动会随工作区回收删除：${errMessage(err)}`);
      return res;
    }
  }

  /** completeRecord 确定要提交的请求（之后重发时内容逐字节相同）。 */
  private completeRecord(res: Result): CompleteRecord {
    const c: CompleteRecord = {
      outcome: res.outcome,
      errorClass: res.errorClass,
      message: this.d.redactor.redact(res.message),
      agentVersion: this.d.agents.version(this.spec.agent),
      binding: { ...this.binding },
    };
    if (res.reviewReport) c.reviewReport = b64(res.reviewReport);
    else if (res.delivery) c.delivery = { ...res.delivery, report: b64(res.delivery.report) };
    return c;
  }

  /** complete 提交结果；返回是否已经有了结论（提交成功、已经有结果或已失效）。连接器退出打断重试时返回 false。 */
  private async complete(c: CompleteRecord): Promise<boolean> {
    const req = {
      ref: this.ref,
      outcome: c.outcome,
      errorClass: c.errorClass,
      message: c.message,
      agentVersion: c.agentVersion,
      binding: { ...c.binding },
      result:
        c.reviewReport !== undefined
          ? { case: 'reviewReport' as const, value: unb64(c.reviewReport) }
          : c.delivery
            ? { case: 'delivery' as const, value: { ...c.delivery, report: unb64(c.delivery.report) } }
            : { case: undefined },
    };
    const total = AbortSignal.timeout(this.d.timing.completeTotalMs);
    try {
      // 每次提交有期限；连接器退出或失效时不再重试（正在进行的这一次照常完成）
      await this.retry(() => this.d.kiln.complete(req, AbortSignal.any([total, AbortSignal.timeout(this.d.timing.rpcMs)])), {
        attempts: 0,
        signal: AbortSignal.any([total, this.exiting.signal]),
      });
      this.resultSettled = true;
    } catch (err) {
      if (err instanceof StaleError) {
        this.d.log.warn('提交结果时 attempt 已经失效', { attempt: this.rec.attemptId });
        this.resultSettled = true;
      } else if (err instanceof ConnectError && err.code === Code.AlreadyExists) {
        // Server 已经有这次执行的结果（例如失联后已判定）：不再重试
        this.d.log.warn('这次执行在 Kiln 中已经有结果，没有提交', { attempt: this.rec.attemptId });
        this.resultSettled = true;
      } else if (this.exiting.signal.aborted) {
        return false;
      } else {
        // 结果没有确认提交：工作区不回收（留作诊断，见 cleanup；由历史残留的周期清理兜底）
        this.d.log.error('提交结果失败', { attempt: this.rec.attemptId, err: errMessage(err), workspace: '保留工作区' });
      }
    }
    return true;
  }

  private async cleanup(): Promise<void> {
    if (this.rec.services?.project && this.d.services) {
      // 删除失败时不在这里重试：节点在周期清理（以及重启恢复之后）回收不属于执行中 attempt 的项目
      await this.d.services.stop(this.rec.services.project).catch((err) => this.d.log.warn('删除测试服务失败，稍后由周期清理回收', { project: this.rec.services?.project, err: errMessage(err) }));
    }
    if (this.binding.dispatchId) {
      // 落盘的结束消息删不掉时由周期清理删除（12 小时）
      await this.d.mailbox.forget(this.binding.dispatchId).catch((err) => this.d.log.warn('删除落盘的结束消息失败', { dispatch: this.binding.dispatchId, err: errMessage(err) }));
    }
    // 回收的责任先记下（Agent 确认停止、终端确认释放之后才回收，见下）
    if (this.reclaimDue) this.rec.reclaim ??= this.reclaimRecord();
    if (this.unconfirmedStop) {
      // 记下还要停止的 Agent：连接器重启后继续占用容量、继续停止（确认停止之后由节点释放终端、回收工作区）
      this.rec.stage = 'stopping';
      this.rec.stopSince = this.d.now();
      await this.save();
      return;
    }
    if (this.rec.release) {
      // 只剩没有确认释放的终端（之后可能还要回收工作区）：记为 releasing，节点（包括重启后）继续释放，不占容量
      this.rec.stage = 'releasing';
      await this.save();
      return;
    }
    if (this.rec.reclaim) {
      const t = reclaimTarget(this.rec, this.rec.reclaim);
      const out = await reclaimWorkspace(this.d, t);
      if (out.kind !== 'done') {
        // 没有完成：记为 reclaiming，节点（包括重启后）继续回收，不占容量
        const reason = out.kind === 'wait' ? out.reason : out.error;
        this.d.log.warn(`回收${kindText(t.kind)}的工作区没有完成，稍后由节点重试`, { attempt: this.rec.attemptId, reason });
        this.print(`回收${kindText(t.kind)}的工作区没有完成（${reason}），稍后重试`);
        this.rec.stage = 'reclaiming';
        await this.save();
        return;
      }
      this.print(`已回收${kindText(t.kind)}的工作区（${out.removed.length} 项）`);
    }
    await this.d.journal.remove(this.rec.attemptId);
  }

  /**
   * reclaimDue：要回收这次的工作区——在 Orca 中发过启动请求（可能建了工作区），并且结果已经了结（审计和开发相同）。结果
   * 没有确认提交、意料之外的错误放弃执行时不回收（工作区留作诊断）。
   */
  private get reclaimDue(): boolean {
    return this.kind !== undefined && this.resultSettled && !!this.binding.startRequestId;
  }

  /** reclaimRecord 是回收的责任：开发另记起点和提交给 Server 的交付或 checkpoint 的 commit（已推到远端、ls-remote 确认过；回收时据此写明删掉的没有推送的改动）。 */
  private reclaimRecord(): ReclaimRecord {
    const r: ReclaimRecord = { since: this.d.now(), repo: this.repoLockKey, clone: this.clone, kind: this.kind ?? 'review' };
    if (this.kind === 'develop') {
      r.start = this.spec.sha;
      r.pushed = this.rec.complete?.delivery?.sha ?? '';
    }
    return r;
  }

  // ---- 心跳 ----

  private heartbeatLoop(): { stop(): Promise<void> } {
    const ctl = new AbortController();
    const loop = (async () => {
      while (!ctl.signal.aborted) {
        if (this.d.now() >= this.deadline && !this.stop) this.halt('timeout');
        try {
          const r = await this.rpc(() =>
            this.d.kiln.heartbeat(
              { ref: this.ref, stage: this.rec.stage, text: this.stageText, binding: { ...this.binding } },
              AbortSignal.any([ctl.signal, AbortSignal.timeout(this.d.timing.rpcMs)]),
            ),
          );
          this.lastBeat = this.d.now();
          if (r.cancelRequested && !this.stop) this.halt('cancel');
        } catch (err) {
          if (!(err instanceof StaleError) && this.d.now() - this.lastBeat > this.d.timing.maxDisconnectedMs && !this.stop) {
            this.halt('disconnected');
          }
        }
        // 上传失败时保留缓冲区，下一次再传；失效由 rpc 处理，之后不再上传（见构造函数）。心跳循环停止后不再发起上传：
        // 那一次 flush 会带着已结束的信号失败，白白留下一个悬空的 Promise。
        if (!ctl.signal.aborted && !this.stale) this.logs.flush(ctl.signal).catch(() => undefined);
        await sleep(this.d.timing.heartbeatMs, ctl.signal);
      }
    })();
    return {
      stop: async () => {
        ctl.abort();
        await loop;
      },
    };
  }

  // ---- Agent 的工具（kiln-orca task …） ----

  private get toolsOpen(): boolean {
    return !this.stop && !this.toolsClosed;
  }

  /**
   * closeTools 关闭工具并等正在进行（包括排队中）的工具调用结束：之后主流程读到的 claim、selectNote 不会再变。
   * 调用中的重试等待被停止信号打断，已经发出的 RPC 有期限，所以这里不会一直等。
   */
  private async closeTools(): Promise<void> {
    this.toolsClosed = true;
    await Promise.allSettled([...this.toolCalls]);
    // 工具关闭后 Agent 不再拿到凭据：同时关闭它还没有结束的 git 传输（读操作）并等吊销结束；推送的写操作之后单独开
    await this.ops.closeAll(CredentialAccess.READ);
  }

  /**
   * tool 处理 Agent 的一个命令，返回给 Agent 看的输出和退出码。同一个 attempt 的命令串行执行；结束流程开始后（或者
   * 已经停止）不再执行，排队中的也一样。
   */
  async tool(cmd: string, args: Record<string, string>): Promise<{ code: number; output: string }> {
    // 回执只记日志：worker_done 送到之后工具已经关闭，回执通常在那之后才到
    if (cmd === doneReceiptCmd) return this.toolDoneReceipt(args);
    if (!this.toolsOpen) return stoppedReply;
    // 只读、不调用 Server：不排队（Agent 并行运行多条命令时不互相等待）
    if (cmd === execEnvCmd) {
      return { code: 0, output: JSON.stringify({ env: this.rec.toolchain?.env ?? null, git: execGitOf(this.spec.cloneUrl, this.spec.repositories, path.join(this.taskDir, gitExecDir)) }) };
    }
    if (cmd === credentialCmd) return this.toolCredential(args);
    if (cmd === refusedCmd) return this.toolRefused(args);
    const prev = this.toolTail;
    const call = (async () => {
      await prev;
      return this.toolsOpen ? this.runTool(cmd, args) : stoppedReply;
    })();
    this.toolTail = call;
    this.toolCalls.add(call);
    try {
      return await call;
    } finally {
      this.toolCalls.delete(call);
    }
  }

  /** runTool 执行一个命令（不抛出）。 */
  private async runTool(cmd: string, args: Record<string, string>): Promise<{ code: number; output: string }> {
    try {
      switch (cmd) {
        case 'info':
          return { code: 0, output: JSON.stringify(this.info(), null, 2) };
        case 'check-report':
          return await this.toolCheckReport();
        case 'claim':
          return await this.toolClaim();
        case 'deliver':
          return await this.toolDeliver();
        case 'ci':
          return await this.toolCI(args.sha ?? '');
        case 'doc':
          return await this.toolDoc(args);
        case doneCmd:
          return await this.toolDone(args);
        default:
          return { code: 2, output: `未知的命令 ${cmd}：可用 info、check-report、claim、deliver、ci、doc、exec、done` };
      }
    } catch (err) {
      if (err instanceof StaleError) return { code: 1, output: '这个任务已经失效，不要再继续。' };
      return { code: 1, output: `执行失败（可以稍后重试）：${this.d.redactor.redact(errMessage(err))}` };
    }
  }

  /**
   * openTransport 为 Agent 的一次 git 传输开操作（传输包装经 socket 请求，PLAN #33.13）：地址是任务仓库或声明的依赖仓库时
   * 开一个读操作（第一次要凭据时签发），连接关闭时（onClose）关闭操作、吊销；不是这些仓库时这次传输不给凭据（op 为空）。
   * 停止之后不再开。
   */
  openTransport(url: string, pid: string): { code: number; output: string; op: string; onClose?: () => Promise<void> } {
    if (!this.toolsOpen) return { ...stoppedReply, op: '' };
    const repo = transportRepo(url, this.spec.cloneUrl, [this.spec.repository, ...this.spec.repositories]);
    if (!repo) return { code: 0, output: '', op: '' };
    const op = this.ops.open(repo, CredentialAccess.READ);
    this.d.log.info('开始 git 传输', { attempt: this.rec.attemptId, op: op.id, repo, pid });
    return {
      code: 0,
      output: '',
      op: op.id,
      onClose: () => {
        this.d.log.info('git 传输结束', { attempt: this.rec.attemptId, op: op.id, repo, pid });
        return op.close();
      },
    };
  }

  /**
   * toolCredential 回答 git 传输中 credential helper 的请求（PLAN #33.13）：只回答还没有关闭的操作，并且请求的协议、主机、
   * 路径与这个操作的仓库一致；第一次要时签发，同一操作后续的请求返回同一枚。别的请求不回答（输出为空，git 不用凭据）。
   * 不排队（并行的 git 不互相等待）；停止、失效之后不再给出。
   */
  private async toolCredential(args: Record<string, string>): Promise<{ code: number; output: string }> {
    const op = this.ops.get(args.op ?? '');
    if (!op || op.access !== CredentialAccess.READ) return { code: 0, output: '' };
    const q = { protocol: args.protocol ?? '', host: args.host ?? '', path: args.path ?? '' };
    if (!credentialAllowed(q, this.spec.cloneUrl, [op.repo])) return { code: 0, output: '' };
    try {
      const c = await op.token();
      // 要凭据的过程中停止了：不再交出（操作随后由 closeAll 关闭、吊销）
      if (!this.toolsOpen) return stoppedReply;
      return { code: 0, output: `username=${c.username}\npassword=${c.secret}\n` };
    } catch (err) {
      if (err instanceof StaleError || !this.toolsOpen) return stoppedReply;
      if (err instanceof OpClosedError) return { code: 1, output: errMessage(err) };
      return { code: 1, output: `向 Kiln Server 要 git 凭据失败（重新执行这条 git 命令即是新的请求）：${this.d.redactor.redact(errMessage(err))}` };
    }
  }

  /** toolRefused 记下传输包装因为认证冲突拒绝的一次传输（只有键和来源，没有值）。 */
  private toolRefused(args: Record<string, string>): { code: number; output: string } {
    const conflicts = args.conflicts ?? '';
    this.d.log.warn('拒绝了一次 git 传输：执行机器上有会带上别的凭据的配置', { attempt: this.rec.attemptId, url: args.url ?? '', conflicts });
    this.print(`拒绝了一次 git 传输（${args.url ?? ''}）：${conflicts}`);
    return { code: 0, output: '' };
  }

  private info() {
    const dev = this.spec.kind.case === 'develop' ? this.spec.kind.value : undefined;
    return {
      attempt: Number(this.id),
      repository: this.spec.repository,
      kind: this.kind,
      mode: dev?.mode,
      phase: this.kind === 'develop' ? this.phase() : undefined,
      work_item: this.rec.claim?.workItemId ?? (dev?.workItemId ? String(dev.workItemId) : undefined),
      task_dir: this.taskDir,
      worktree: this.binding.worktreePath,
      start_commit: this.spec.sha,
      deadline: new Date(this.deadline).toISOString(),
      tools: this.rec.toolchain?.tools.map((t) => `${t.name} ${t.version}`),
    };
  }

  private async toolCheckReport() {
    if (this.kind !== 'review') return { code: 2, output: 'check-report 只用于审计任务' };
    const b = await readLimited(path.join(this.taskDir, 'report.json'));
    if (!b) return { code: 1, output: `还没有 ${path.join(this.taskDir, 'report.json')}` };
    const v = await this.validate(ReportKind.REVIEW, b, this.abort.signal);
    return v.valid ? { code: 0, output: `通过：审计报告有效。现在运行 ${this.doneCommand('succeeded')} 结束这一轮。` } : { code: 1, output: `审计报告不合格，请修正后再运行：\n- ${v.problems.join('\n- ')}` };
  }

  /**
   * toolClaim 选题后领取。回复可能没有送到 Agent（socket 断开、连接器重启），Agent 会重试：已经有结果的重试原样重放
   * 第一次的回复（领取成功的完整说明、不领取的结束说明、冲突后重新选题的说明），不再调用 Server。
   */
  private async toolClaim(): Promise<ToolReply> {
    const dev = this.spec.kind.case === 'develop' ? this.spec.kind.value : undefined;
    if (dev?.mode !== 'select') return { code: 2, output: 'claim 只用于选题任务' };
    if (this.rec.claim) return { code: 0, output: this.rec.claim.reply ?? `已经领取了工作项 #${this.rec.claim.workItemId}（分支 ${this.rec.claim.branch}），继续开发。` };
    if (this.rec.selectNote && this.rec.claimReply) return this.rec.claimReply;
    const b = await readLimited(path.join(this.taskDir, 'selection.json'));
    // 冲突后 selection.json 已删除：Agent 还没有写新的选题就重试，重放"重新选题"的回复
    if (!b) return this.rec.claimReply ?? { code: 1, output: `还没有 ${path.join(this.taskDir, 'selection.json')}` };
    const v = await this.validate(ReportKind.SELECTION, b, this.abort.signal);
    if (!v.valid) return { code: 1, output: `选题报告不合格，请修正后再运行：\n- ${v.problems.join('\n- ')}` };
    const sel = JSON.parse(Buffer.from(b).toString('utf8')) as { selected?: number[] };
    const issues = sel.selected ?? [];
    const r = await this.retry(() => this.d.kiln.claimDevIssues({ ref: this.ref, issues, selection: b }), { attempts: 5, signal: this.abort.signal });
    const msg = substitute(r.message, this.vars());
    if (r.claimed) {
      // 先改内存再写盘：写盘失败时回滚，重试才会重新走一遍（否则会命中没有持久化的内存缓存而跳过写盘）
      const prevClaim = this.rec.claim;
      const prevReply = this.rec.claimReply;
      this.rec.claim = { workItemId: String(r.workItemId), branch: r.branch, phase: r.phase, issues, reply: msg };
      this.rec.claimReply = undefined;
      try {
        await this.save();
      } catch (err) {
        this.rec.claim = prevClaim;
        this.rec.claimReply = prevReply;
        throw err;
      }
      this.print(`已领取 ${issues.join('、')}，工作项 #${r.workItemId}，分支 ${r.branch}（${r.phase}）`);
      return { code: 0, output: msg };
    }
    // 这一轮先只在本地算出来：候选写失败时重试还是同一轮（selectRounds 只在写成功、随 save 落盘时才算数）
    const nextRounds = this.selectRounds + 1;
    if (issues.length > 0 && !dev.dryRun && r.candidates.length > 0 && nextRounds < maxReselect) {
      this.print(`领取没有成功（${r.reason}），用新的 ${r.candidates.length} 个候选重新选题`);
      await this.writeCandidates(r.candidates, r.inProgress);
      const prevRounds = this.selectRounds;
      this.selectRounds = nextRounds;
      this.rec.selectRounds = nextRounds;
      // 先记下回复再删除 selection.json：之后没有 selection.json 的重试都重放它
      const prevReply = this.rec.claimReply;
      this.rec.claimReply = { code: 3, output: msg };
      try {
        await this.save();
      } catch (err) {
        this.rec.claimReply = prevReply;
        this.selectRounds = prevRounds;
        this.rec.selectRounds = prevRounds;
        throw err;
      }
      await rm(path.join(this.taskDir, 'selection.json'), { force: true });
      return this.rec.claimReply;
    }
    const prevRounds = this.selectRounds;
    this.selectRounds = nextRounds;
    this.rec.selectRounds = nextRounds;
    const prevNote = this.rec.selectNote;
    const prevReply = this.rec.claimReply;
    this.rec.selectNote = issues.length === 0 ? '没有选中 Issue' : dev.dryRun ? `dry_run：选中 ${issues.join('、')}，没有领取` : `没有领取到 Issue：${r.reason}`;
    this.rec.claimReply = { code: 0, output: msg || `本次没有领取，现在运行 ${this.doneCommand('succeeded')} 结束这一轮。` };
    try {
      await this.save();
    } catch (err) {
      this.rec.selectNote = prevNote;
      this.rec.claimReply = prevReply;
      this.selectRounds = prevRounds;
      this.rec.selectRounds = prevRounds;
      throw err;
    }
    this.print(this.rec.selectNote);
    return this.rec.claimReply;
  }

  private async toolDeliver() {
    if (this.kind !== 'develop') return { code: 2, output: 'deliver 只用于开发任务' };
    const dev = this.spec.kind.case === 'develop' ? this.spec.kind.value : undefined;
    if (dev?.mode === 'select' && !this.rec.claim) return { code: 1, output: '还没有领取工作项：先写选题报告并运行 claim。' };
    const p = path.join(this.taskDir, 'delivery.json');
    const b = await readLimited(p);
    if (!b) return { code: 1, output: `还没有 ${p}` };
    const v = await this.validate(ReportKind.DELIVERY, b, this.abort.signal);
    if (!v.valid) return { code: 1, output: `交付报告不合格，请修正后再运行：\n- ${v.problems.join('\n- ')}` };
    const report = JSON.parse(Buffer.from(b).toString('utf8')) as { status?: string; design?: { documents?: string[] } };
    if (report.status === 'delivered') {
      const problems = await this.checkGit(report.design?.documents ?? []);
      if (problems.length > 0) return { code: 1, output: `交付还不能通过，请修正后再运行：\n- ${problems.join('\n- ')}` };
    }
    return { code: 0, output: `通过：交付报告有效。现在运行 ${this.doneCommand('succeeded')} 结束这一轮（不要 push，连接器会推送）。` };
  }

  /** doneCommand 是结束这一轮的完整命令（给 Agent 看）。 */
  private doneCommand(outcome: DoneOutcome): string {
    return `${this.d.toolCommand(this.id)} ${doneCmd} --outcome ${outcome}`;
  }

  /**
   * toolDone 处理 kiln-orca task done（PLAN #33.5）：校验通过后记下固定的请求 id、回复发送参数，由 task done 在 Agent 的进程中
   * 发送 worker_done（连接器不冒充 Agent 发送）。
   * - succeeded：按任务当前的类型和阶段做与 check-report / claim / deliver 相同的校验（复用它们），不通过时回复问题、不发送。
   *   选题没有领取时要有确定的结论（claim 已经回复不领取）；领取了的按交付校验。
   * - failed：Agent 明确报告做不完（例如环境坏了、写不出合格的报告），必须带 --summary 写明原因，不校验报告。Kiln 的结果
   *   仍按结束时任务目录中的文件判定（与 Agent 自己发送 worker_done 相同），--outcome 和说明只记进日志。
   * 请求 id 按（outcome、说明）固定：同样的参数重跑沿用同一个（Orca 重放，不会重复结算）；参数变了换新的（之前的没有结算时
   * 由它结算，已经结算时 Orca 降级为普通消息、回执 settled）。子 Agent 继承主 Agent 的环境，它运行 task done 与主 Agent
   * 相同。结束校验仍由连接器在收到 worker_done 后照常再做（不因为这里校验过就跳过）。
   */
  private async toolDone(args: Record<string, string>): Promise<ToolReply> {
    const outcome = args.outcome as DoneOutcome;
    if (!doneOutcomes.includes(outcome)) return { code: 2, output: `需要 --outcome ${doneOutcomes.join(' 或 --outcome ')}` };
    const summary = (args.summary ?? '').trim();
    if (outcome === 'failed' && !summary) return { code: 2, output: '--outcome failed 时用 --summary 写明为什么做不完（一句话）' };
    if (outcome === 'succeeded') {
      const gate = await this.doneGate();
      if (gate.code !== 0) return gate;
    }
    const { taskId, dispatchId } = this.binding;
    if (!taskId || !dispatchId) return { code: 1, output: 'Orca 中还没有这次执行的 dispatch，稍后重试。' };
    const prev = this.rec.done;
    if (!prev || prev.outcome !== outcome || prev.summary !== summary) {
      this.rec.done = { retryRequest: randomUUID(), outcome, summary };
      try {
        await this.save();
      } catch (err) {
        // 写盘失败时回滚：重试（Agent 重跑 task done）不会命中没有持久化的请求 id
        this.rec.done = prev;
        throw err;
      }
    }
    const done = this.rec.done!;
    this.print(`Agent 运行了 done（${outcome}${summary ? `：${summary.slice(0, 200)}` : ''}），由它发送 worker_done（请求 ${done.retryRequest}）`);
    const send: DoneSend = {
      taskId,
      dispatchId,
      outcome,
      subject: `Kiln attempt ${this.id}：${outcome}`,
      body: summary || (this.kind === 'review' ? '审计报告已通过校验' : '已通过交付前的校验'),
      retryRequest: done.retryRequest,
      orca: this.d.orcaFallback,
    };
    return { code: 0, output: JSON.stringify(send) };
  }

  /** doneGate 是 task done --outcome succeeded 的校验：复用 check-report、deliver，选题没有领取时要有确定的结论。 */
  private async doneGate(): Promise<ToolReply> {
    if (this.kind === 'review') return this.toolCheckReport();
    const dev = this.spec.kind.case === 'develop' ? this.spec.kind.value : undefined;
    if (dev?.mode === 'select' && !this.rec.claim) {
      if (this.rec.selectNote) return { code: 0, output: '' };
      return { code: 1, output: `还没有领取：先写选题报告并运行 ${this.d.toolCommand(this.id)} claim，按它的输出继续。` };
    }
    return this.toolDeliver();
  }

  /**
   * toolDoneReceipt 记下 task done 发送 worker_done 的回执摘要（只记日志，不改变状态）。执行不了 Orca 命令行是节点的问题：
   * 记 error（Agent 的环境中没有 ORCA_CLI_COMMAND，config.json 的 orca 在 Agent 那里也执行不了）。
   */
  private toolDoneReceipt(args: Record<string, string>): ToolReply {
    const fields = { attempt: this.rec.attemptId, ...args };
    const kind = args.kind ?? '';
    if (kind === 'reported') {
      this.print(`Agent 已向 Orca 报告结束（${args.action ?? ''}，消息 ${args.messageId ?? ''}）`);
    } else if (kind === 'exec') {
      this.d.log.error('Agent 的 kiln-orca task done 执行不了 Orca 命令行（节点的问题：检查 config.json 的 orca，或者 Orca 给 Agent 的 ORCA_CLI_COMMAND）', fields);
      this.print(`Agent 的 kiln-orca task done 执行不了 Orca 命令行 ${args.command ?? ''}：${args.message ?? ''}`);
    } else {
      this.d.log.warn('Agent 的 kiln-orca task done 没能报告结束', fields);
      this.print(`Agent 的 kiln-orca task done 没能报告结束（${args.code ?? kind}：${args.message ?? ''}）`);
    }
    return { code: 0, output: '' };
  }

  private async toolCI(sha: string) {
    const r = await this.retry(() => this.d.kiln.getVerification({ ref: this.ref, sha, logTailBytes: ciLogTailBytes }), { attempts: 5, signal: this.abort.signal });
    return { code: 0, output: formatVerification(r) };
  }

  /** toolDoc 读取任务所属工作项冻结的设计文档快照（无 id 时列出，给 id 时返回正文）。 */
  private async toolDoc(args: Record<string, string>): Promise<{ code: number; output: string }> {
    const id = args.id ?? '';
    if (!id) {
      const r = await this.retry(() => this.d.kiln.listTaskDocs({ ref: this.ref }), { attempts: 5, signal: this.abort.signal });
      if (!r.docs.length) return { code: 0, output: '这个任务没有外部设计文档快照。' };
      return {
        code: 0,
        output: JSON.stringify(
          r.docs.map((d) => ({ id: d.id, title: d.title, revision: Number(d.revision), sha256: d.sha256, url: d.url })),
          null,
          2,
        ),
      };
    }
    const r = await this.retry(() => this.d.kiln.getTaskDoc({ ref: this.ref, docId: id }), { attempts: 5, signal: this.abort.signal });
    return { code: 0, output: `# ${r.title}（修订 ${Number(r.revision)}，${r.url}）\n\n${r.content}` };
  }
}
