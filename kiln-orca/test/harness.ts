import { existsSync, rmSync } from 'node:fs';
import { cp, mkdir, mkdtemp, realpath, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { create, type MessageInitShape } from '@bufbuild/protobuf';
import { Code, ConnectError } from '@connectrpc/connect';
import {
  type AITask,
  AITaskSchema,
  ClaimDevIssuesResponseSchema,
  CompleteResponseSchema,
  CredentialAccess,
  GetVerificationResponseSchema,
  IssueCredentialResponseSchema,
  ListTaskDocsResponseSchema,
  GetTaskDocResponseSchema,
  HeartbeatResponseSchema,
  OfferDevCandidatesResponseSchema,
  ValidateReportResponseSchema,
  AcquireResponseSchema,
  type AcquireResponse,
  type ClaimDevIssuesResponse,
  type GetTaskDocResponse,
  type ListTaskDocsResponse,
  type OfferDevCandidatesResponse,
  type TaskRef,
} from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { AgentProbe, knownAgents } from '../src/core/agents.js';
import { type AttemptDeps, type Timing } from '../src/core/attempt.js';
import { StaleError } from '../src/core/errors.js';
import { type Exec, type ExecResult, must, nodeExec } from '../src/core/exec.js';
import { Git, localGitEnv } from '../src/core/git.js';
import { Journal, type JournalRecord } from '../src/core/journal.js';
import type { AcquireInit, ClaimInit, CompleteInit, GetTaskDocInit, HeartbeatInit, IssueCredentialInit, Kiln, ListTaskDocsInit, RevokeCredentialInit, ValidateInit, VerificationInit } from '../src/core/kiln.js';
import { Redactor } from '../src/core/redact.js';
import { type Logger, nopLogger } from '../src/core/log.js';
import { type Identity, Mailbox, type MailboxTiming } from '../src/core/mailbox.js';
import { KeyedMutex } from '../src/core/mutex.js';
import {
  type CheckRequest,
  type CheckResult,
  type Orca,
  OrcaError,
  type OrcaMessage,
  type OrcaStatus,
  type ReleaseState,
  type RepoInfo,
  type RequestInfo,
  StartFailedError,
  type TerminalInfo,
  terminalUsable,
  type UnreleasedWorker,
  type WorkerInfo,
  type WorkerRef,
  type WorkerStartOptions,
  type WorktreeInfo,
} from '../src/core/orca.js';
import { parseWorktreeList } from '../src/core/git.js';
import { canon } from '../src/core/reclaim.js';
import { attemptOfProject, projectName, type Services, type StartedServices } from '../src/core/services.js';
import type { SkillProvisioner } from '../src/core/skill.js';
import { Toolchain, type ToolSpec } from '../src/core/toolchain.js';

/**
 * shortTmp 建一个放 unix socket 的临时目录：socket 路径有上限（macOS 103 字节、Linux 107 字节），macOS 的 $TMPDIR
 * （/var/folders/…/T，取 realpath 后带 /private）很长，所以优先用 /tmp（macOS 上取 realpath 是 /private/tmp）。由调用方删除。
 */
export async function shortTmp(prefix = 'ko-'): Promise<string> {
  const base = existsSync('/tmp') ? '/tmp' : os.tmpdir();
  return realpath(await mkdtemp(path.join(base, prefix)));
}

export const fastTiming: Timing = {
  heartbeatMs: 20,
  pollMs: 10,
  showMs: 40,
  maxDisconnectedMs: 2_000,
  completeTotalMs: 3_000,
  startVerifyMs: 300,
  rpcMs: 200,
  credentialRetryMs: 10,
};

/**
 * Faults 收集一个测试中不能被产品代码吞掉的失败：Agent 脚本中的错误，以及测试注入到替身（FakeOrca、FakeKiln、
 * FakeServices，包装的 Git、Journal、Exec 等）的钩子中的同步失败（waitFor 超时、expect 失败、意料之外的错误）。
 * 产品代码会把钩子抛出的错误当成普通的故障处理（重试、记为失败、放弃推送……），测试可能因此"通过"，而断言成立的原因
 * 是测试自己的故障。这里记下的失败由 test/setup.ts 在 afterEach 中让测试失败。
 */
export class Faults {
  readonly errors: unknown[] = [];

  record(err: unknown): void {
    this.errors.push(err);
  }

  /** guard 执行钩子中的一段代码：出错时记下，并照常抛给调用者（产品代码）。 */
  async guard<T>(f: () => T | Promise<T>): Promise<T> {
    try {
      return await f();
    } catch (err) {
      this.record(err);
      throw err;
    }
  }
}

/** current 是正在运行的测试的 Faults（每个测试一个，由 test/setup.ts 建立和检查）。 */
let current: Faults | undefined;

/** faults 返回正在运行的测试的 Faults。 */
export function faults(): Faults {
  if (!current) throw new Error('不在测试中：Faults 由 test/setup.ts 在每个测试开始时建立');
  return current;
}

/** beginFaults 在每个测试开始时调用。 */
export function beginFaults(): void {
  current = new Faults();
}

/** endFaults 在每个测试结束时调用：有记下的失败时抛出（只有一个时原样抛出，断言的差异照常显示）。 */
export function endFaults(): void {
  const f = current;
  current = undefined;
  const errs = f?.errors ?? [];
  if (errs.length === 1) throw errs[0];
  if (errs.length > 1) throw new AggregateError(errs, `钩子或 Agent 脚本中有 ${errs.length} 个失败：${errs.map((e) => (e instanceof Error ? e.message : String(e))).join('；')}`);
}

/**
 * waitFor 等到 f 成立，超过 ms 时抛出。超时同时记到发起等待的那个测试的 Faults 中：在钩子里等待时，超时即使被产品代码
 * 吞掉，测试也会失败（测试结束之后才超时的不算到之后的测试上）。
 */
export async function waitFor(f: () => boolean | Promise<boolean>, ms = 10_000, what = '条件'): Promise<void> {
  const sink = current;
  const until = Date.now() + ms;
  while (!(await f())) {
    if (Date.now() > until) {
      const err = new Error(`等待${what}超时`);
      sink?.record(err);
      throw err;
    }
    await new Promise((r) => setTimeout(r, 10));
  }
}

/** macrotask 等一轮宏任务：之前已经交出（resolve / reject）的 Promise 的处理（微任务）都已经执行完。 */
export const macrotask = () => new Promise<void>((r) => setImmediate(r));

/** records 返回 journal 中的记录；有读不了的记录时抛出（测试中不应出现，除非专门构造）。 */
export async function records(j: Journal): Promise<JournalRecord[]> {
  const scan = await j.list();
  if (scan.unreadable.length > 0) throw new Error(`journal 中有读不了的记录：${JSON.stringify(scan.unreadable)}`);
  return scan.records;
}

// git 是测试自己运行 git 的辅助函数：与连接器的本地 git 一样剥离外层 task exec / 凭据通道的私有变量（GIT_EXEC_PATH、KILN_*）、
// 环境变量配置注入和仓库上下文。测试可能就在外层的 kiln-orca task exec 中运行，继承那里的 GIT_EXEC_PATH 会让测试的 git 走
// 外层的传输包装。
export const git = (dir: string, ...args: string[]) => must(nodeExec, 'git', args, { cwd: dir, env: localGitEnv(process.env) });

export async function commitFile(dir: string, file: string, content: string, msg: string): Promise<string> {
  const { writeFile, mkdir } = await import('node:fs/promises');
  await mkdir(path.dirname(path.join(dir, file)), { recursive: true });
  await writeFile(path.join(dir, file), content);
  await git(dir, 'add', '-A');
  await git(dir, '-c', 'user.name=t', '-c', 'user.email=t@t', 'commit', '--quiet', '-m', msg);
  return git(dir, 'rev-parse', 'HEAD');
}

/**
 * 初始仓库模板：每个测试进程懒构建一次，Origin.create 把它递归拷贝到测试自己的目录，不再为每个测试跑 6 个 git 子进程。
 * 建好之后只读；模板仓库没有配置远端，.git 里也没有绝对路径，拷贝到任意目录都能照常使用。
 * 存在 globalThis 上（不是模块级变量）：Vitest 按测试文件重建模块，globalThis 才能让同一进程的多个文件共用一份。
 */
const originTemplateSlot = globalThis as { __kilnOrcaOriginTemplate?: Promise<string> };

function originTemplate(): Promise<string> {
  return (originTemplateSlot.__kilnOrcaOriginTemplate ??= buildOriginTemplate());
}

async function buildOriginTemplate(): Promise<string> {
  const dir = await mkdtemp(path.join(os.tmpdir(), `kiln-orca-origin-${process.pid}-`));
  // 进程结束时删除模板目录；测试自己拷贝出的仓库由 makeEnv 的 cleanup 删除。
  process.once('exit', () => rmSync(dir, { recursive: true, force: true }));
  const bare = path.join(dir, 'origin.git');
  const work = path.join(dir, 'origin-work');
  await must(nodeExec, 'git', ['init', '--quiet', '--bare', '-b', 'master', bare]);
  await must(nodeExec, 'git', ['init', '--quiet', '-b', 'master', work]);
  await commitFile(work, 'README.md', '# repo\n', 'init');
  await git(work, 'push', '--quiet', bare, 'master');
  return dir;
}

/** Origin 是一个本地的裸仓库（代替 GitHub）和往里推送的工作目录。 */
export class Origin {
  constructor(
    readonly bare: string,
    readonly work: string,
  ) {}

  static async create(root: string): Promise<Origin> {
    const template = await originTemplate();
    const bare = path.join(root, 'origin.git');
    const work = path.join(root, 'origin-work');
    await cp(path.join(template, 'origin.git'), bare, { recursive: true });
    await cp(path.join(template, 'origin-work'), work, { recursive: true });
    return new Origin(bare, work);
  }

  async commit(file: string, content: string, branch = 'master'): Promise<string> {
    const sha = await commitFile(this.work, file, content, `change ${file}`);
    await git(this.work, 'push', '--quiet', '--force', this.bare, `HEAD:refs/heads/${branch}`);
    return sha;
  }

  /** branchSha 返回分支的 commit（按完整 ref 名，git show-ref --verify），不存在时为空。 */
  async branchSha(branch: string): Promise<string> {
    const r = await nodeExec('git', ['show-ref', '--verify', '--hash', `refs/heads/${branch}`], { cwd: this.bare, env: localGitEnv(process.env) });
    return r.code === 0 ? r.stdout.trim() : '';
  }
}

/** AgentCtx 是假 Agent 能用的东西：它的 worktree、任务目录和 Kiln 工具。 */
export interface AgentCtx {
  opts: WorkerStartOptions;
  worktree: string;
  taskDir: string;
  dispatchId: string;
  tool(cmd: string, args?: Record<string, string>): Promise<{ code: number; output: string }>;
  /** done 发送 worker_done。 */
  done(): void;
  stopped: AbortSignal;
}

export type AgentScript = (ctx: AgentCtx) => Promise<void>;

interface FakeWorker {
  ref: WorkerRef;
  opts: WorkerStartOptions;
  /** dispatch 的状态：running 还没有结束；completed（发了 worker_done 或 Agent 脚本结束）、stopped 都是已经结束（settled）。 */
  state: 'running' | 'completed' | 'stopped';
  /** 终端已经释放（worker-release）。按 Orca 1.4.258：停止不释放终端，已经结束的 worker 才能释放。 */
  released: boolean;
  stop: AbortController;
}

/** FakeOrca 在内存中模拟 Orca：worker-start 用真实的 git worktree 建工作区，再运行脚本化的 Agent。 */
export class FakeOrca implements Orca {
  readonly repos = new Map<string, string>();
  readonly workers = new Map<string, FakeWorker>();
  readonly requests = new Map<string, WorkerRef>();
  readonly starts: WorkerStartOptions[] = [];
  readonly stops: string[] = [];
  readonly releases: string[] = [];
  readonly replies: string[] = [];
  /** 已经被 check 交出（标记为已处理）的消息 id。 */
  readonly handled: string[] = [];
  /** check 用到的 Run。 */
  readonly checkRuns = new Set<string>();
  /** 发出过的 check 请求（包括重放）。 */
  readonly checks: CheckRequest[] = [];
  /** run-use 的调用（终端、Run）。 */
  readonly runUses: Array<[string, string]> = [];
  /** run-create 的调用（终端、建出的 Run）。 */
  readonly runCreates: Array<[string, string]> = [];
  /** 建过的终端。 */
  readonly terminalCreates: string[] = [];
  /** terminal close 的调用。 */
  readonly terminalCloses: string[] = [];
  /** 设置时 terminal show 以它失败（查询失败：连不上 Orca、结果未知）。 */
  terminalShowFault: Error | undefined;
  /** 返回 Error 时这次 terminal close 按它失败。 */
  terminalCloseFault: ((handle: string) => Error | undefined) | undefined;
  /** 设置时新的 check 在通过绑定检查之后、交出消息之前等它（模拟正在等待中的 check），之后按 Orca 再核对一次绑定。 */
  checkHold: Promise<void> | undefined;
  /** 设置时 run-use 以它失败。 */
  runUseFault: Error | undefined;
  /** 接下来几次 reply 失败。 */
  replyFaults = 0;
  /**
   * 下一次（新的）check：lost-after 交出一批并记下后响应丢失（exec_failed）；interrupted 等待被打断（没有交出消息）；
   * refused 以错误码拒绝（没有交出消息）。
   */
  checkFault: 'lost-after' | 'interrupted' | 'refused' | undefined;
  private queue: OrcaMessage[] = [];
  /** 按 Orca 1.4.258 的语义：每个 check 请求的结果按 requestId 记下，用同一个请求重发时重放同一批。 */
  private readonly checkReceipts = new Map<string, { input: string; result: CheckResult }>();
  /** 每个 Run 绑定的终端：只有它能读取（consumer_fenced）。 */
  readonly runBindings = new Map<string, string>();
  private seq = 0;
  ready = true;
  /**
   * Orca 中的终端：按 Orca 1.4.258，关闭了的（connected:false）仍查得到；删掉（delete）表示句柄失效（terminal_handle_stale）。
   * orphaned 只表示没有界面窗格，与能不能用无关。
   */
  readonly terminals = new Map<string, TerminalInfo>();
  /**
   * 下一次 worker-start：在真正启动后抛出（响应丢失）或者在启动前抛出（拒绝）；failed 按 Orca 的失败回执（建了 dispatch、
   * 没有启动 Agent，夹具 worker-start-failed.json），之后 request-show 报告 failed、worker-release 回答 retained。
   */
  startFault: 'lost-after' | 'refused' | 'lost-before' | 'failed' | undefined;
  /** 启动失败（startFault: failed）的请求 → Orca 建了的 dispatch。 */
  readonly failedStarts = new Map<string, string>();
  /** requestShowFn 返回值时 request-show 按它回复（Error 为查询失败）；返回 undefined 时按实际启动过的请求回复。 */
  requestShowFn: ((requestId: string) => RequestInfo | Error | undefined) | undefined;
  /** request-show 查过的请求。 */
  readonly requestShows: string[] = [];
  /** stopConfirms 为 false 时 worker-stop 不能确认停止。 */
  stopConfirms = true;
  /**
   * releaseFn 返回 Error 时这次 worker-release 按它失败（例如 release_unknown、exec_failed、明确拒绝），返回 ReleaseState
   * 时按它回答；返回 undefined 时按 Orca 1.4.258 的语义回答（见 workerRelease）。
   */
  releaseFn: ((dispatchId: string) => Error | ReleaseState | undefined) | undefined;
  tool: (attempt: string, cmd: string, args: Record<string, string>) => Promise<{ code: number; output: string }> = () => Promise.resolve({ code: 1, output: '没有工具' });
  /** Orca 建的工作区（worker-start）的元数据：canon 过的路径 → 仓库、备注、最后活动时间。 */
  readonly worktreeMeta = new Map<string, { repoId: string; comment: string; lastActivityAt: number }>();
  /** showExternal：worktree list 中显示不是 Orca 建的 worktree（Agent 的临时检出）；默认不显示（Orca 的默认 hide）。 */
  showExternal = false;
  /** 额外的工作区（folder workspace 等），按 repoId 出现在 worktree list 中。 */
  readonly extraWorktrees: WorktreeInfo[] = [];
  /** 额外的仓库（repo list 中）。 */
  readonly extraRepos: RepoInfo[] = [];
  /** 工作区（canon 过的路径）中额外的活着的终端（例如有人打开的终端）。 */
  readonly extraTerminals = new Map<string, number>();
  /** worktree rm 的调用。 */
  readonly worktreeRemoves: Array<{ id: string; force: boolean }> = [];
  /** worktreeFault 返回 Error 时这次工作区命令按它失败（连不上 Orca、Orca 拒绝、列表截断……）。 */
  worktreeFault: ((op: 'repo-list' | 'list' | 'rm' | 'terminals' | 'workers', arg: string) => Error | undefined) | undefined;

  /** faults 记下 Agent 脚本中抛出的错误（包括 expect 失败、waitFor 超时），测试结束时检查。 */
  constructor(
    private readonly root: string,
    public agent: AgentScript,
    private readonly faults: Faults,
  ) {}

  status(): Promise<OrcaStatus> {
    return Promise.resolve({ ready: this.ready, reason: this.ready ? '' : '没有运行', version: '1.4.258', runtimeId: 'rt-1' });
  }
  /** repoAddHold 设置时 repoAdd 先等它返回的 Promise（参数是注册的路径；测试控制 Orca 注册仓库的时机）。 */
  repoAddHold: ((p: string) => Promise<void> | undefined) | undefined;
  async repoAdd(p: string): Promise<string> {
    await this.repoAddHold?.(p);
    let id = this.repos.get(p);
    if (!id) {
      id = `repo-${this.repos.size + 1}`;
      this.repos.set(p, id);
    }
    return Promise.resolve(id);
  }
  terminalCreate(worktreePath: string, title: string): Promise<string> {
    const h = `term_${++this.seq}`;
    this.addTerminal({ handle: h, title, worktreePath, connected: true, orphaned: false, exitCause: '' });
    this.terminalCreates.push(h);
    return Promise.resolve(h);
  }
  /** addTerminal 放进一个终端（测试中模拟以前建的、别人建的终端）。 */
  addTerminal(t: TerminalInfo): void {
    this.terminals.set(t.handle, { ...t });
  }
  terminalShow(h: string): Promise<TerminalInfo | undefined> {
    const err = this.terminalShowFault;
    if (err) return Promise.reject(err);
    const t = this.terminals.get(h);
    return Promise.resolve(t && { ...t });
  }
  /** terminalAlive：测试用，终端还能用（terminalUsable）。 */
  async terminalAlive(h: string): Promise<boolean> {
    return terminalUsable(await this.terminalShow(h));
  }
  terminalClose(h: string): Promise<boolean> {
    this.terminalCloses.push(h);
    const err = this.terminalCloseFault?.(h);
    if (err) return Promise.reject(err);
    const t = this.terminals.get(h);
    if (!t) return Promise.resolve(false);
    t.connected = false;
    t.exitCause = 'operator_close';
    return Promise.resolve(true);
  }
  runCreate(from: string): Promise<string> {
    const run = `run_${++this.seq}`;
    this.runBindings.set(run, from);
    this.runCreates.push([from, run]);
    return Promise.resolve(run);
  }
  runUse(from: string, run: string): Promise<void> {
    this.runUses.push([from, run]);
    if (this.runUseFault) return Promise.reject(this.runUseFault);
    if (!this.runBindings.has(run)) return Promise.reject(new OrcaError('run_not_found', `Run ${run} was not found or is inspect-only.`));
    this.runBindings.set(run, from);
    return Promise.resolve();
  }

  async workerStart(o: WorkerStartOptions): Promise<WorkerRef> {
    this.starts.push(o);
    const fault = this.startFault;
    this.startFault = undefined;
    if (fault === 'refused') throw new OrcaError('agent_unavailable', 'claude 不可用');
    if (fault === 'lost-before') throw new OrcaError('exec_failed', '连接断开');
    if (fault === 'failed') {
      const dispatchId = `ctx_${++this.seq}`;
      this.failedStarts.set(o.requestId, dispatchId);
      throw new StartFailedError('worktree_create：fatal: invalid reference', dispatchId);
    }
    const prev = this.requests.get(o.requestId);
    if (prev) return prev;
    if (!this.runBindings.has(o.run)) throw new OrcaError('run_not_found', `Run ${o.run} was not found.`);
    const clone = [...this.repos].find(([, id]) => id === o.repoId)?.[0];
    if (!clone) throw new OrcaError('selector_not_found', `没有仓库 ${o.repoId}`);
    const wt = path.join(this.root, 'orca-worktrees', o.name);
    // 与 Orca 相同：按工作区的名字建分支（带前缀）
    await git(clone, 'worktree', 'add', '--quiet', '-b', `orca-user/${o.name}`, wt, o.baseBranch);
    this.worktreeMeta.set(await canon(wt), { repoId: o.repoId, comment: o.comment, lastActivityAt: Date.now() });
    const ref: WorkerRef = { runId: o.run, taskId: `task_${++this.seq}`, dispatchId: `ctx_${++this.seq}`, worktreePath: wt };
    this.requests.set(o.requestId, ref);
    const w: FakeWorker = { ref, opts: o, state: 'running', released: false, stop: new AbortController() };
    this.workers.set(ref.dispatchId, w);
    const attempt = /kiln:attempt=(\d+)/.exec(o.comment)?.[1] ?? '';
    const taskDir = /dir=(\S+)/.exec(o.spec)?.[1] ?? '';
    const ctx: AgentCtx = {
      opts: o,
      worktree: wt,
      taskDir,
      dispatchId: ref.dispatchId,
      tool: (cmd, args = {}) => this.tool(attempt, cmd, args),
      done: () => {
        // Orca 接受 worker_done 时 dispatch 即结束（settled）
        if (w.state === 'running') w.state = 'completed';
        this.send({ type: 'worker_done', dispatchId: ref.dispatchId, outcome: 'succeeded' });
      },
      stopped: w.stop.signal,
    };
    void this.agent(ctx).then(
      () => {
        if (w.state === 'running') w.state = 'completed';
      },
      (err: unknown) => {
        this.faults.record(err);
        if (w.state === 'running') w.state = 'completed';
      },
    );
    if (fault === 'lost-after') throw new OrcaError('exec_failed', '响应丢失');
    return ref;
  }

  requestShow(id: string): Promise<RequestInfo> {
    this.requestShows.push(id);
    const o = this.requestShowFn?.(id);
    if (o instanceof Error) return Promise.reject(o);
    if (o) return Promise.resolve(o);
    const failed = this.failedStarts.get(id);
    if (failed) return Promise.resolve({ state: 'failed', error: 'worktree_create：fatal: invalid reference', dispatchId: failed });
    const r = this.requests.get(id);
    return Promise.resolve(r ? { state: 'completed', worker: r } : { state: 'absent' });
  }

  send(m: Partial<OrcaMessage> & { type: string }): void {
    this.queue.push({
      id: `msg_${++this.seq}`,
      subject: '',
      body: '',
      dispatchId: '',
      outcome: '',
      reportPath: '',
      rejected: '',
      turnOutcome: '',
      stopReason: '',
      failureKind: '',
      error: '',
      detail: '',
      ...m,
    });
  }

  /**
   * 按 Orca 1.4.258 的语义：返回最早的一批未读消息并标记为已处理（不再等确认）；同一个 requestId 与相同的输入重放同一批，
   * 输入不同时 request_mismatch；新的请求只有 Run 绑定的终端能读取。
   */
  async check(req: CheckRequest, signal?: AbortSignal): Promise<CheckResult> {
    this.checks.push({ ...req });
    this.checkRuns.add(req.run);
    const input = JSON.stringify([req.terminal, req.run, req.waitMs]);
    const prev = this.checkReceipts.get(req.requestId);
    if (prev) {
      if (prev.input !== input) throw new OrcaError('request_mismatch', req.requestId);
      return prev.result;
    }
    if (!this.runBindings.has(req.run)) throw new OrcaError('run_not_found', `Run ${req.run} was not found.`);
    if (this.runBindings.get(req.run) !== req.terminal) throw new OrcaError('consumer_fenced', `${req.run} 绑定在 ${this.runBindings.get(req.run)}`);
    const fault = this.checkFault;
    this.checkFault = undefined;
    if (fault === 'refused') throw new OrcaError('terminal_handle_stale', req.terminal);
    if (this.checkHold) await this.checkHold;
    if (this.queue.length === 0) await new Promise((r) => setTimeout(r, 10));
    // 等待期间 Run 被别的终端接管：Orca 结束这次 check（没有交出消息）
    if (this.runBindings.get(req.run) !== req.terminal) throw new OrcaError('consumer_fenced', `This caller is not bound to Run ${req.run}, which is bound to ${this.runBindings.get(req.run)}. Nothing changed.`);
    let result: CheckResult;
    if (signal?.aborted || fault === 'interrupted') {
      result = { messages: [], interrupted: true };
    } else {
      result = { messages: this.queue, interrupted: false };
      this.handled.push(...this.queue.map((m) => m.id));
      this.queue = [];
    }
    this.checkReceipts.set(req.requestId, { input, result });
    if (fault === 'lost-after') throw new OrcaError('exec_failed', '响应丢失');
    return result;
  }
  workerShow(d: string): Promise<WorkerInfo> {
    const w = this.workers.get(d);
    if (!w) return Promise.reject(new OrcaError('dispatch_not_found', `Worker Dispatch ${d} was not found.`));
    return Promise.resolve({ dispatch: w.state === 'running' ? 'dispatched' : w.state, outcome: '', terminal: w.state === 'running' ? 'live' : 'exited', worktreePath: w.ref.worktreePath });
  }
  /** 按 OrcaCli 的语义：没有这个 dispatch 时没有可停的（dispatch_not_found → true，见夹具 worker-stop-not-found.json）。 */
  workerStop(d: string): Promise<boolean> {
    this.stops.push(d);
    const w = this.workers.get(d);
    if (!this.stopConfirms) return Promise.resolve(false);
    if (w) {
      w.state = 'stopped';
      w.stop.abort();
    }
    return Promise.resolve(true);
  }
  /**
   * 按 Orca 1.4.258 的语义（夹具 worker-release*.json）：没有这个 dispatch → not_found；还没有结束 → dispatch_inactive；
   * 已经释放过 → already_released；否则释放终端 → released。
   */
  workerRelease(d: string): Promise<ReleaseState> {
    this.releases.push(d);
    const o = this.releaseFn?.(d);
    if (o instanceof Error) return Promise.reject(o);
    if (o) return Promise.resolve(o);
    if ([...this.failedStarts.values()].includes(d)) return Promise.resolve('retained');
    const w = this.workers.get(d);
    if (!w) return Promise.resolve('not_found');
    if (w.state === 'running') return Promise.reject(new OrcaError('dispatch_inactive', `Dispatch ${d} is ready; only a settled worker can release.`));
    if (w.released) return Promise.resolve('already_released');
    w.released = true;
    return Promise.resolve('released');
  }
  /**
   * resetOrchestration 模拟 orca orchestration reset：Run、dispatch、请求的记录和消息都没有了，终端（包括邮箱终端）还在。
   * Agent 的脚本被告知停止（只为测试结束时不留下等待中的脚本；真实 Orca 中它的终端可能还在运行）。
   */
  resetOrchestration(): void {
    this.runBindings.clear();
    this.requests.clear();
    this.checkReceipts.clear();
    this.failedStarts.clear();
    this.queue = [];
    for (const w of this.workers.values()) w.stop.abort();
    this.workers.clear();
  }

  private cloneOf(repoId: string): string {
    const clone = [...this.repos].find(([, id]) => id === repoId)?.[0];
    if (!clone) throw new OrcaError('selector_not_found', `没有仓库 ${repoId}`);
    return clone;
  }

  private fault(op: 'repo-list' | 'list' | 'rm' | 'terminals' | 'workers', arg: string): void {
    const err = this.worktreeFault?.(op, arg);
    if (err) throw err;
  }

  repoList(): Promise<RepoInfo[]> {
    this.fault('repo-list', '');
    return Promise.resolve([...[...this.repos].map(([p, id]) => ({ id, path: p, kind: 'git' })), ...this.extraRepos]);
  }

  /** 按 Orca 1.4.258：仓库本身和 Orca 建的工作区都列出，不是 Orca 建的 worktree 只在 showExternal 时列出。 */
  async worktreeList(repoId: string): Promise<WorktreeInfo[]> {
    this.fault('list', repoId);
    const clone = this.cloneOf(repoId);
    const out: WorktreeInfo[] = [];
    const wts = parseWorktreeList(await git(clone, 'worktree', 'list', '--porcelain', '-z'));
    for (const [i, w] of wts.entries()) {
      const meta = this.worktreeMeta.get(await canon(w.path));
      if (i > 0 && !meta && !this.showExternal) continue;
      out.push({
        id: `${repoId}::${w.path}`,
        repoId,
        path: w.path,
        branch: w.branch,
        comment: meta?.comment ?? '',
        git: true,
        isMainWorktree: i === 0,
        isBare: false,
        lastActivityAt: meta?.lastActivityAt ?? 0,
      });
    }
    return [...out, ...this.extraWorktrees.filter((w) => w.repoId === repoId)];
  }

  /** 按 Orca 1.4.258（夹具 worktree-rm*.json）：没有匹配的 → selector_not_found（OrcaCli 返回 not_found）；不带 --force 时不干净 → runtime_error。 */
  async worktreeRemove(id: string, force: boolean): Promise<'removed' | 'not_found'> {
    this.worktreeRemoves.push({ id, force });
    this.fault('rm', id);
    const i = id.indexOf('::');
    const repoId = id.slice(0, i);
    const p = id.slice(i + 2);
    const clone = this.cloneOf(repoId);
    const c = await canon(p);
    const wts = parseWorktreeList(await git(clone, 'worktree', 'list', '--porcelain', '-z'));
    const hit = await Promise.all(wts.map(async (w, n) => n > 0 && (await canon(w.path)) === c));
    if (i < 0 || !hit.includes(true)) return 'not_found';
    // 目录已经不在时没有内容可查，按 Orca 1.4.258 实测：不带 --force 也删除登记（git 的登记一起去掉）
    if (!force && existsSync(p)) {
      const dirty = await git(p, 'status', '--porcelain');
      if (dirty) throw new OrcaError('runtime_error', `Failed to delete worktree at ${p}. ${dirty}`);
    }
    await git(clone, 'worktree', 'remove', ...(force ? ['--force'] : []), p);
    this.worktreeMeta.delete(c);
    return 'removed';
  }

  /**
   * 工作区（id:<repoId>::<路径> 或 path:<路径>）中进程还在的终端：terminalCreate 建的（邮箱）、addTerminal 放进的，加上
   * extraTerminals（例如有人打开的终端）。按真实 Orca 的 structured 模式，Agent 的 worker 不在 terminal list 中。
   */
  async terminalList(worktree: string): Promise<TerminalInfo[]> {
    this.fault('terminals', worktree);
    const p = worktree.startsWith('path:') ? worktree.slice('path:'.length) : worktree.slice(worktree.indexOf('::') + 2);
    const c = await canon(p);
    const out: TerminalInfo[] = [];
    for (const t of this.terminals.values()) if (t.connected && (await canon(t.worktreePath)) === c) out.push({ ...t });
    for (let n = 0; n < (this.extraTerminals.get(c) ?? 0); n++) out.push({ handle: `term_extra_${n}`, title: 'Terminal 1', worktreePath: c, connected: true, orphaned: false, exitCause: '' });
    return out;
  }

  /** 终端还没有释放的 worker（worker-list）：Agent 在运行、已经停止但没有释放的都算。 */
  unreleasedWorkers(): Promise<UnreleasedWorker[]> {
    this.fault('workers', '');
    return Promise.resolve(
      [...this.workers.values()]
        .filter((w) => !w.released)
        .map((w) => ({
          dispatchId: w.ref.dispatchId,
          terminalState: w.state === 'running' ? 'active' : 'reclaimable',
          liveness: w.state === 'running' ? 'live' : 'exited',
          workspace: `${w.opts.repoId}::${w.ref.worktreePath}`,
        })),
    );
  }

  reply(_from: string, id: string, body: string): Promise<void> {
    if (this.replyFaults > 0) {
      this.replyFaults--;
      return Promise.reject(new OrcaError('exec_failed', '回复失败'));
    }
    this.replies.push(`${id}:${body}`);
    return Promise.resolve();
  }
}

export type HeartbeatFn = (req: HeartbeatInit) => { cancelRequested?: boolean } | Error;
type ValidateResult = { valid: boolean; problems?: string[] } | Error;

/** FakeKiln 记录调用，按测试设置的规则回复。 */
export class FakeKiln implements Kiln {
  /** taskDocs 是 task doc list/get 返回的冻结文档（测试可设置）。 */
  taskDocs: Array<{ id: string; title: string; revision: number; sha256: string; url: string; content: string }> = [];
  /** taskDocRefs / taskDocGets 记录 list/get 收到的 ref 与 doc_id。 */
  readonly taskDocRefs: Array<{ attemptId?: bigint; epoch?: bigint }> = [];
  readonly taskDocGets: string[] = [];
  readonly completes: CompleteInit[] = [];
  readonly heartbeats: HeartbeatInit[] = [];
  readonly acquires: AcquireInit[] = [];
  readonly claims: ClaimInit[] = [];
  readonly logs: string[] = [];
  tasks: AITask[] = [];
  heartbeatFn: HeartbeatFn = () => ({});
  offers: Array<MessageInitShape<typeof OfferDevCandidatesResponseSchema>> = [];
  claimFn: (req: ClaimInit) => MessageInitShape<typeof ClaimDevIssuesResponseSchema> = () => ({ claimed: true, workItemId: 9n, branch: 'kiln/dev-9', phase: 'implement', message: '领取成功' });
  validateFn: (req: ValidateInit) => ValidateResult | Promise<ValidateResult> = () => ({ valid: true });
  completeFault: Error | undefined;
  /** completeDown 设置时每次 Complete 都以它失败（Server 一直连不上）。 */
  completeDown: Error | undefined;
  /** Complete 被调用的次数（包括失败的）。 */
  completeCalls = 0;
  /** heartbeatHang 为 true 时心跳不响应：只在 signal 结束时失败。 */
  heartbeatHang = false;
  /**
   * acquireFaults：接下来几次 Acquire 的故障。error：请求没有到达 Server（网络错误）；lost：Server 已经处理（有任务就分配），
   * 但响应丢失。
   */
  acquireFaults: Array<'error' | 'lost'> = [];
  /** 按 request_id 记下已经分配的任务：同一个 request_id 重试时返回同一个任务（Server 的幂等语义）。 */
  private readonly assigned = new Map<string, AITask>();
  /** logStore 是每个 attempt 在 Server 上的日志。 */
  readonly logStore = new Map<bigint, Buffer>();

  acquire(req: AcquireInit, signal?: AbortSignal): Promise<AcquireResponse> {
    this.acquires.push(req);
    const fault = this.acquireFaults.shift();
    if (fault === 'error') return Promise.reject(new ConnectError('连接断开', Code.Unavailable));
    const id = req.requestId ?? '';
    const task = this.assigned.get(id) ?? (req.standby ? undefined : this.tasks.shift());
    if (task) this.assigned.set(id, task);
    if (fault === 'lost') return Promise.reject(new ConnectError('响应丢失', Code.Unavailable));
    if (task) return Promise.resolve(create(AcquireResponseSchema, { task }));
    return new Promise((resolve) => {
      const t = setTimeout(() => resolve(create(AcquireResponseSchema, {})), 15);
      signal?.addEventListener('abort', () => {
        clearTimeout(t);
        resolve(create(AcquireResponseSchema, {}));
      });
    });
  }
  heartbeat(req: HeartbeatInit, signal?: AbortSignal) {
    this.heartbeats.push(req);
    if (this.heartbeatHang) {
      return new Promise<never>((_, reject) => {
        if (signal?.aborted) return reject(new Error('aborted'));
        signal?.addEventListener('abort', () => reject(new Error('aborted')), { once: true });
      });
    }
    const r = this.heartbeatFn(req);
    if (r instanceof Error) return Promise.reject(r);
    return Promise.resolve(create(HeartbeatResponseSchema, r));
  }
  /** logSignals 是每次 appendLog 收到的停止信号（心跳上传带 attempt 的停止信号）。 */
  readonly logSignals: Array<AbortSignal | undefined> = [];

  /** appendLog 按 internal/server/logstore 的 appendAt 规则保存：已有的部分忽略，超过已有长度的 offset 报错，返回当前长度。 */
  appendLog(ref: TaskRef, offset: bigint, data: Uint8Array, signal?: AbortSignal): Promise<bigint> {
    this.logSignals.push(signal);
    const cur = this.logStore.get(ref.attemptId) ?? Buffer.alloc(0);
    const size = BigInt(cur.length);
    if (offset > size) return Promise.reject(new ConnectError(`日志不连续，已有 ${size} 字节，收到 offset ${offset}`, Code.FailedPrecondition));
    const end = offset + BigInt(data.length);
    if (end <= size) return Promise.resolve(size);
    const add = Buffer.from(data).subarray(Number(size - offset));
    this.logs.push(add.toString('utf8'));
    this.logStore.set(ref.attemptId, Buffer.concat([cur, add]));
    return Promise.resolve(end);
  }
  complete(req: CompleteInit) {
    this.completeCalls++;
    if (this.completeDown) return Promise.reject(this.completeDown);
    if (this.completeFault) {
      const e = this.completeFault;
      this.completeFault = undefined;
      return Promise.reject(e);
    }
    this.completes.push(req);
    return Promise.resolve(create(CompleteResponseSchema, {}));
  }
  offerDevCandidates(): Promise<OfferDevCandidatesResponse> {
    return Promise.resolve(create(OfferDevCandidatesResponseSchema, this.offers.shift() ?? {}));
  }
  claimDevIssues(req: ClaimInit): Promise<ClaimDevIssuesResponse> {
    this.claims.push(req);
    return Promise.resolve(create(ClaimDevIssuesResponseSchema, this.claimFn(req)));
  }
  async validateReport(req: ValidateInit) {
    const r = await this.validateFn(req);
    if (r instanceof Error) throw r;
    return create(ValidateReportResponseSchema, r);
  }
  verificationFn: (req: VerificationInit) => MessageInitShape<typeof GetVerificationResponseSchema> = (req) => ({
    sha: req.sha,
    state: 'failed',
    failedJobs: [{ name: 'test', reason: 'exit 1', errorClass: 'user', logTail: Buffer.from('FAIL x') }],
  });
  readonly verifications: VerificationInit[] = [];
  /** verificationHold 设置时 GetVerification 先等它（测试控制返回的时机）。 */
  verificationHold: Promise<void> | undefined;
  async getVerification(req: VerificationInit) {
    this.verifications.push(req);
    await this.verificationHold;
    return create(GetVerificationResponseSchema, this.verificationFn(req));
  }

  /*
   * 凭据部分只对齐了连接器实际用到的那部分签发登记语义：一个操作至多一枚、同一操作换仓库或权限被拒绝、关闭是终态（之后
   * 同一操作的签发返回 FAILED_PRECONDITION）、ABORTED 时操作已经关闭、签发在途时关闭不交出。它不是完整的登记：同一操作的
   * 两个并发请求在这里可能各签一枚（连接器由 GitOp 合并本地的申请，不会并发），确定未签发（UNAVAILABLE）之后也不保留第一次
   * 的范围。这两条由 Go 的登记测试证明（internal/server/gitcred 的 TestIssue_SameOpConcurrent、TestIssue_NotIssued）；
   * INTERNAL 之后操作已经关闭也不在这里模拟，用到的测试由 credentialFn 依次给出回复。新增的测试依赖这些语义时，先在这里
   * 补上对应的行为。
   */
  /** 签发过的凭据（按顺序）：登记键（attempt/epoch/op）、密钥、权限、操作、仓库、过期时间（秒）和是否已经吊销。 */
  readonly credentials: Array<{ key: string; secret: string; access: CredentialAccess; opId: string; repository: string; expiresAt: bigint; revoked: boolean }> = [];
  /** credentialTTLSeconds 是签发的凭据的有效期（与 GitHub 相同，1 小时）。 */
  credentialTTLSeconds = 3600;
  /**
   * credentialFn 返回 Error 时签发失败（默认成功）。FakeKiln 被直接调用、不经 Connect，返回的 ConnectError 都不是 wire
   * error（相当于客户端本地合成的）；要模拟 Server 发来的错误用 wireError。
   */
  credentialFn: (req: IssueCredentialInit) => Error | undefined = () => undefined;
  /** credentialLost 返回 true 时这次签发照常登记（Server 已经签出），但回复丢失：抛出客户端合成的 UNAVAILABLE。 */
  credentialLost: (req: IssueCredentialInit) => boolean = () => false;
  /** credentialHold 设置时签发先等它返回的 Promise（测试控制签发返回的时机）。 */
  credentialHold: ((req: IssueCredentialInit) => Promise<void>) | undefined;
  readonly issues: IssueCredentialInit[] = [];
  readonly revokes: RevokeCredentialInit[] = [];
  /** 已经关闭的操作（登记键）：与 Server 的签发登记相同，关闭是终态，之后同一操作的签发拒绝。 */
  readonly closedOps = new Set<string>();
  private credentialSeq = 0;
  /** opKey 是签发登记的键：attempt、epoch 和 op_id（与 Server 相同，不同 attempt 的 op_id 互不相干）。 */
  private opKey(ref: { attemptId?: bigint; epoch?: bigint } | undefined, opId: string | undefined): string {
    return `${ref?.attemptId ?? 0n}/${ref?.epoch ?? 0n}/${opId ?? ''}`;
  }
  async issueCredential(req: IssueCredentialInit) {
    this.issues.push(req);
    if (!req.opId) throw new ConnectError('缺少 op_id', Code.InvalidArgument);
    const key = this.opKey(req.ref, req.opId);
    // 已经关闭的操作不再签发
    if (this.closedOps.has(key)) throw new ConnectError('操作已经关闭', Code.FailedPrecondition);
    const access = req.access ?? CredentialAccess.UNSPECIFIED;
    const repository = req.repository ?? '';
    // 每个操作至多一枚：同一操作的重复请求返回同一枚，仓库和权限必须与第一次一致
    const prev = this.credentials.find((x) => x.key === key);
    if (prev && (prev.repository !== repository || prev.access !== access)) throw new ConnectError('同一操作的仓库或权限与第一次不一致', Code.InvalidArgument);
    await this.credentialHold?.(req);
    const err = this.credentialFn(req);
    // 与 Server 相同：GitHub 签发结果不明（ABORTED）时这个操作以错误关闭，之后同一 op_id 不再签发
    if (err instanceof ConnectError && err.code === Code.Aborted) this.closedOps.add(key);
    if (err) throw err;
    const secret = prev?.secret ?? `ghs_test${access === CredentialAccess.WRITE ? 'W' : 'R'}${++this.credentialSeq}x${Math.random().toString(36).slice(2, 10)}`;
    const expiresAt = prev?.expiresAt ?? BigInt(Math.floor(Date.now() / 1000) + this.credentialTTLSeconds);
    if (!prev) this.credentials.push({ key, secret, access, opId: req.opId, repository, expiresAt, revoked: false });
    // 签发在途时关闭：Server 吊销拿到的凭据、不交出
    if (this.closedOps.has(key)) {
      for (const c of this.credentials) if (c.key === key) c.revoked = true;
      throw new ConnectError('操作已经关闭', Code.FailedPrecondition);
    }
    if (this.credentialLost(req)) throw new ConnectError('响应丢失（连接断开）', Code.Unavailable);
    return create(IssueCredentialResponseSchema, { credential: { kind: req.kind, access, username: 'x-access-token', secret, expiresAt, scopes: [repository] } });
  }
  revokeCredential(req: RevokeCredentialInit) {
    this.revokes.push(req);
    if (!req.opId) return Promise.reject(new ConnectError('缺少 op_id', Code.InvalidArgument));
    const key = this.opKey(req.ref, req.opId);
    this.closedOps.add(key);
    // 有登记时只按操作吊销；没有登记（Server 重启丢了登记）时才按 secret 补吊销，并且只吊销这个 attempt 的
    const mine = this.credentials.filter((c) => c.key === key);
    if (mine.length > 0) for (const c of mine) c.revoked = true;
    else if (req.secret) for (const c of this.credentials) if (c.secret === req.secret && c.key.startsWith(`${req.ref?.attemptId ?? 0n}/`)) c.revoked = true;
    return Promise.resolve();
  }

  listTaskDocs(req: ListTaskDocsInit): Promise<ListTaskDocsResponse> {
    if (req.ref) this.taskDocRefs.push(req.ref);
    return Promise.resolve(create(ListTaskDocsResponseSchema, {
      docs: this.taskDocs.map((d) => ({ id: d.id, title: d.title, revision: BigInt(d.revision), sha256: d.sha256, url: d.url })),
    }));
  }
  getTaskDoc(req: GetTaskDocInit): Promise<GetTaskDocResponse> {
    this.taskDocGets.push(req.docId ?? '');
    const d = this.taskDocs.find((x) => x.id === req.docId);
    if (!d) return Promise.reject(new ConnectError('没有这篇设计文档的快照', Code.NotFound));
    return Promise.resolve(create(GetTaskDocResponseSchema, {
      id: d.id, title: d.title, revision: BigInt(d.revision), sha256: d.sha256, content: d.content, url: d.url,
    }));
  }

  get log(): string {
    return this.logs.join('');
  }
}

export const staleError = () => new StaleError();

/** wireError 是 Server 发来的 Connect 错误（Connect-ES 对从回复中解析出的错误设置 isWireError）。 */
export function wireError(message: string, code: Code): ConnectError {
  const e = new ConnectError(message, code);
  e.isWireError = true;
  return e;
}

/**
 * heartbeatGate 让钩子在一个确定的位置让 attempt 停止。fire() 之后的心跳回复 reply()（失效、取消……）；fire() 等到
 * FakeKiln 交出这个回复、再过一轮宏任务才返回：心跳循环和 rpc 对回复的处理（halt）在交出它的那一轮微任务中就完成了。
 * 不依赖之后还有心跳（失效之后 rpc 在入口拒绝，心跳不再到达 FakeKiln）。等待超时由 waitFor 记为测试失败。
 * fire() 之前心跳按 before 回复。
 */
export function heartbeatGate(e: Env, reply: () => ReturnType<HeartbeatFn>, before: HeartbeatFn = () => ({})) {
  let armed = false;
  let answered = -1;
  let passed = false;
  e.kiln.heartbeatFn = (req) => {
    if (!armed) return before(req);
    if (answered < 0) answered = e.kiln.heartbeats.length;
    return reply();
  };
  return {
    async fire(): Promise<void> {
      armed = true;
      await waitFor(() => answered >= 0, 5_000, '心跳交出停止的回复');
      await macrotask();
      passed = true;
    },
    /** answered 是交出回复时 FakeKiln 收到的心跳数（还没有交出时为 -1）。 */
    get answered(): number {
      return answered;
    },
    /** passed：fire() 已经返回（同步点之后的代码执行到了）。 */
    get passed(): boolean {
      return passed;
    },
  };
}

/** FakeServices 在内存中模拟测试服务：拉起的项目在 projects 中；failStops 次 stop 失败（Docker 暂时不可用）。 */
export class FakeServices implements Services {
  readonly projects = new Set<string>();
  readonly stops: string[] = [];
  /** 每次 start 拉起的项目（包括之后删掉的）。 */
  readonly started: string[] = [];
  /** 每次 cleanup 的保留集合。 */
  readonly cleanups: string[][] = [];
  failStops = 0;

  constructor(readonly owner = 'home-a') {}

  available() {
    return Promise.resolve({ ok: true, reason: '' });
  }
  start(attemptId: bigint): Promise<StartedServices> {
    const project = projectName(this.owner, attemptId);
    this.started.push(project);
    this.projects.add(project);
    return Promise.resolve({ project, desc: ['PostgreSQL：postgres://127.0.0.1:55432/kiln'], notes: [] });
  }
  stop(project: string): Promise<void> {
    this.stops.push(project);
    if (this.failStops > 0) {
      this.failStops--;
      return Promise.reject(new Error('Docker daemon 不可用'));
    }
    this.projects.delete(project);
    return Promise.resolve();
  }
  async cleanup(keep: ReadonlySet<string>): Promise<void> {
    this.cleanups.push([...keep].sort());
    for (const p of [...this.projects]) {
      const id = attemptOfProject(this.owner, p);
      if (id !== undefined && !keep.has(id)) await this.stop(p);
    }
  }
}

/** fastMailbox 是测试中邮箱的等待与重试时间。 */
export const fastMailbox: MailboxTiming = { waitMs: 50, retryMs: 20, maxRetryMs: 200 };

export class MemIdentity {
  id: Identity | undefined;
  load() {
    return Promise.resolve(this.id);
  }
  save(id: Identity) {
    this.id = id;
    return Promise.resolve();
  }
}

/** okExec 让所有 Agent 的 --version 成功。 */
export const agentExec: Exec = (cmd) => Promise.resolve({ code: 0, stdout: `${cmd} 1.0.0\n`, stderr: '' });

/**
 * FakeRunner 模拟 kiln-runner 的 toolchain 子命令（info、ensure、gc），输出格式与 internal/toolchainctl 相同：ensure 在
 * root/<digest hex>/bin 建目录（第二次起 reused），gc 删除 keep 以外装过的。calls 记下每次调用的参数和 stdin；
 * fail 给出时 ensure 按它失败，hang 为 true 时 ensure 一直等到 signal 结束。
 */
export class FakeRunner {
  readonly calls: { args: string[]; input?: string }[] = [];
  readonly installed = new Set<string>();
  fail: ExecResult | undefined;
  hang = false;
  infoFormat = 1;
  constructor(readonly root: string) {}

  dir(digest: string): string {
    return path.join(this.root, digest.replace(/^sha256:/, ''));
  }

  readonly exec: Exec = async (_cmd, args, opts = {}) => {
    this.calls.push({ args: [...args], input: opts.input });
    const [sub, verb] = args;
    if (sub !== 'toolchain') return { code: 2, stdout: '', stderr: '用法错误' };
    if (verb === 'info') return { code: 0, stdout: JSON.stringify({ format: this.infoFormat, version: 'kiln-runner v0.0.0-test' }), stderr: '' };
    if (verb === 'gc') {
      const keep = new Set(args.flatMap((a, i) => (args[i - 1] === '--keep' ? [a] : [])));
      const removed = [...this.installed].filter((d) => !keep.has(d));
      for (const d of removed) {
        this.installed.delete(d);
        await rm(this.dir(d), { recursive: true, force: true });
      }
      return { code: 0, stdout: JSON.stringify({ format: 1, removed: removed.map((d) => ({ name: d, path: this.dir(d), size: 1 })), removed_bytes: removed.length, kept: this.installed.size, failures: [] }), stderr: '' };
    }
    if (verb !== 'ensure') return { code: 2, stdout: '', stderr: '用法错误' };
    if (this.hang) {
      await new Promise<void>((r) => (opts.signal?.aborted ? r() : opts.signal?.addEventListener('abort', () => r(), { once: true })));
      return { code: 127, stdout: '', stderr: 'AbortError: 已中止' };
    }
    if (this.fail) return this.fail;
    const want = (JSON.parse(opts.input ?? '{}') as { tools: ToolSpec[] }).tools;
    const tools = [];
    const lines = [];
    for (const t of want) {
      const dir = this.dir(t.digest);
      const reused = this.installed.has(t.digest);
      await mkdir(path.join(dir, 'bin'), { recursive: true });
      this.installed.add(t.digest);
      tools.push({ ...t, dir, reused, millis: 1 });
      lines.push(`${t.name} ${t.version}（${t.platform}）：${reused ? '已安装，直接使用' : '下载并安装，用时 1ms'}`);
    }
    const env = { set: { GOTOOLCHAIN: 'local' }, prepend: { PATH: tools.map((t) => path.join(t.dir, 'bin')) } };
    return { code: 0, stdout: JSON.stringify({ format: 1, tools, env }), stderr: lines.join('\n') + '\n' };
  };
}

/** tool 是任务中的一个工具（假的 digest）。 */
export function tool(name: string, version: string, hex: string): ToolSpec {
  return { name, version, platform: 'darwin/arm64', digest: `sha256:${hex}` };
}

/** FakeSkill 记录 skill 同步的调用（不读写宿主机上的 ~/.agents / ~/.claude），可以注入失败。 */
export class FakeSkill implements SkillProvisioner {
  /** ensureForAgent 成功的 Agent。 */
  readonly ensured: string[] = [];
  /** ensureAll 收到的 Agent 列表。 */
  readonly all: string[][] = [];
  /** failFor 返回非空时这个 Agent 的 ensureForAgent 以它失败（worker-start 不应发生）。 */
  failFor: (agent: string) => string | undefined = () => undefined;
  /** failAll 是 ensureAll 返回的失败说明（节点据此 standby）。 */
  failAll = '';

  async ensureForAgent(agent: string): Promise<string> {
    const why = this.failFor(agent);
    if (why) throw new Error(why);
    this.ensured.push(agent);
    return `/fake/skills/${agent}/kiln-task/SKILL.md`;
  }
  ensureAll(agents: readonly string[]): Promise<string> {
    this.all.push([...agents]);
    return Promise.resolve(this.failAll);
  }
}

export interface Env {
  root: string;
  /** 假的 kiln-runner（受管理的工具链）。 */
  runner: FakeRunner;
  origin: Origin;
  orca: FakeOrca;
  kiln: FakeKiln;
  deps: AttemptDeps;
  journal: Journal;
  /** skill 是 skill 投放的替身。 */
  skill: FakeSkill;
  /** 这个测试的 Faults：钩子中的同步代码用 faults.guard 执行（waitFor 的超时自动记下）。 */
  faults: Faults;
  cleanup(): Promise<void>;
}

export async function makeEnv(agent: AgentScript, opts: { services?: Services; servicesReady?: { ok: boolean; reason: string } } = {}): Promise<Env> {
  const f = faults();
  const root = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-test-'));
  const origin = await Origin.create(root);
  const orca = new FakeOrca(root, agent, f);
  const kiln = new FakeKiln();
  const g = new Git(nodeExec);
  const journal = new Journal(path.join(root, 'home', 'journal'));
  const agents = new AgentProbe(agentExec, knownAgents);
  await agents.refresh(true);
  const redactor = new Redactor();
  const runner = new FakeRunner(path.join(root, 'home', 'toolchains'));
  const skill = new FakeSkill();
  const deps: AttemptDeps = {
    kiln,
    orca,
    git: g,
    services: opts.services,
    servicesReady: () => opts.servicesReady ?? { ok: !!opts.services, reason: opts.services ? '' : '没有 Docker' },
    toolchain: new Toolchain(runner.exec, () => ({ bin: '/fake/kiln-runner', reason: '' }), runner.root),
    agents,
    mailbox: new Mailbox(orca, g, new MemIdentity(), path.join(root, 'home'), nopLogger, fastMailbox, redactor),
    journal,
    log: nopLogger,
    redactor,
    reposDir: path.join(root, 'home', 'repos'),
    tasksDir: path.join(root, 'home', 'tasks'),
    toolCommand: (id) => `kiln-orca task --attempt ${id}`,
    skill,
    orcaFallback: { command: '/opt/Orca/orca', path: '/usr/bin:/bin' },
    repoLock: new KeyedMutex(),
    orcaRepoIds: new Map(),
    runtimeId: () => 'rt-1',
    timing: fastTiming,
    now: Date.now,
  };
  return { root, runner, origin, orca, kiln, deps, journal, skill, faults: f, cleanup: () => rm(root, { recursive: true, force: true }) };
}

let nextAttempt = 100n;

export function reviewTask(origin: Origin, o: { sha: string; baseSha: string; agent?: string; services?: string[]; tools?: ToolSpec[] }): AITask {
  const id = nextAttempt++;
  return create(AITaskSchema, {
    ref: { attemptId: id, epoch: 1n },
    taskId: id,
    displayName: 'PR #7',
    repoId: 42n,
    repository: 'acme/widgets',
    cloneUrl: origin.bare,
    sha: o.sha,
    timeoutSeconds: 600,
    agent: o.agent ?? 'claude-code',
    services: o.services ?? [],
    tools: o.tools ?? [],
    prompt: 'dir={{KILN_TASK_DIR}}\ntool={{KILN_TOOL}}\nservices={{KILN_SERVICES}}\nskill={{KILN_SKILL}}',
    kind: { case: 'review', value: { trigger: 'pull_request', prNumber: 7, baseRef: 'master', baseSha: o.baseSha, headRef: 'feature' } },
  });
}

export function devTask(origin: Origin, o: { sha: string; mode: 'select' | 'implement' | 'fix'; phase?: string; services?: string[]; tools?: ToolSpec[] }): AITask {
  const id = nextAttempt++;
  return create(AITaskSchema, {
    ref: { attemptId: id, epoch: 1n },
    taskId: id,
    displayName: 'AI 开发',
    repoId: 42n,
    repository: 'acme/widgets',
    cloneUrl: origin.bare,
    sha: o.sha,
    timeoutSeconds: 600,
    agent: 'codex',
    services: o.services ?? [],
    tools: o.tools ?? [],
    prompt: 'dir={{KILN_TASK_DIR}}\ntool={{KILN_TOOL}}\nservices={{KILN_SERVICES}}\nskill={{KILN_SKILL}}',
    kind: { case: 'develop', value: { mode: o.mode, maxIssues: 5, baseRef: 'master', phase: o.phase ?? '', workItemId: o.mode === 'select' ? 0n : 9n } },
  });
}

/** LogLine 是记下的一条日志。 */
export interface LogLine {
  level: 'info' | 'warn' | 'error';
  msg: string;
  fields: Record<string, unknown>;
}

/** recordLog 换上记下每条日志的 Logger。 */
export function recordLog(e: Env): LogLine[] {
  const lines: LogLine[] = [];
  const at = (level: LogLine['level']) => (msg: string, fields: Record<string, unknown> = {}) => void lines.push({ level, msg, fields });
  const log: Logger = { info: at('info'), warn: at('warn'), error: at('error') };
  e.deps.log = log;
  return lines;
}
