import { randomUUID } from 'node:crypto';
import path from 'node:path';
import type { MessageInitShape } from '@bufbuild/protobuf';
import type { AIRunnerStateSchema } from '../gen/kiln/airunner/v2/airunner_pb.js';
import { Attempt, type AttemptDeps, inspectJournal, type Recovery } from './attempt.js';
import { errMessage, RevokedError } from './errors.js';
import { type JournalRecord, removeOlderThan, startUnknown, unreadableFix, type UnreadableRecord } from './journal.js';
import { sleep } from './kiln.js';
import { checkStart, releaseTerminal, settleStart, type TerminalDeps } from './dispatch.js';
import type { OrcaStatus } from './orca.js';
import { runnerPlatform } from './platform.js';
import { reclaimFix, reclaimTarget, reclaimWorkspace, sweepResidue } from './reclaim.js';

// 节点的主循环（PLAN #33.4）：上报状态（Orca、Agent、Docker、容量）并长轮询领取任务；同时执行多个任务（max_tasks），
// 节点内部怎么调度交给 Orca。启动时按 journal 恢复未结束的任务（Orca 中的 Agent 在连接器退出期间继续运行）。
// 恢复之后和周期清理时回收删除失败残留的测试服务（只保留还在执行的 attempt 的）。
//
// journal 不完整（PLAN #33.4）：读不了的记录（文件读不了、内容损坏、任务还原不出来，见 Journal.list、recordTask）中可能有
// 还在运行的 Agent，不知道它占不占容量、用不用测试服务。这时可读的记录照常恢复、跟踪、结束；每条读不了的记录按占用一份
// 容量计入 busy，不领取新任务、不回收测试服务、不清理它的任务目录和邮箱落盘的消息，原因写进日志、上报给 Server（Orca 的
// reason）并在 doctor 中显示。读不了的文件不自动删除、不改名（里面可能有要人手工停止的 Agent 的 dispatch），由人处理；
// 每次检测状态时重新读取：读得出来了按恢复流程接着执行，文件被删除了就解除。journal 写不进去、目录读不了也是同一个状态。
//
// 待释放的终端（PLAN #33.4）：任务结束了、Agent 已经结束或已确认停止，但没能确认 Orca 释放了它的终端（journal 记为
// releasing，见 Attempt.release）。不占容量、不阻止领取，恢复之后和每次再停时（restopIntervalMs）重试 worker-release，确认
// 释放（包括 Orca 回答已经释放、不存在）后删除记录；Orca 明确拒绝时记 error、不再重试；超过保留时间放弃。确认停止的
// stopping 同样接着释放，释放没有确认时转为 releasing（不再占容量）。
//
// 工作区的回收（PLAN #33.10，reclaim.ts，审计和开发相同）与释放同构：任务结束时没有回收完的记为 reclaiming（释放还没有确认时先是
// releasing，确认释放之后接着回收），不占容量，恢复之后和每次再停时重试，回收完删除记录；超过保留时间放弃（记 error，写明
// 处理方法）。终端一直没能确认释放、Agent 一直没能确认停止时不回收。每次周期清理（每小时）还有界地回收升级前留下的、
// 已经结束的执行的工作区（sweepResidue：证据齐全才删；回收之前先写一条 reclaiming 记录，没有回收完的按它重试）。
//
// 吊销（PLAN #33.3）：任何 RPC（领取、心跳、日志、工具、提交）得知节点被吊销时，停止领取，所有在途 attempt 按失效停止
// （Attempt.revoke），记下吊销状态（onRevoked，重启后只做清理），然后只做停止、查证、回收：没能确认停止的 Agent 继续
// 再停，全部了结（或收到退出信号）后 run 抛出 RevokedError。以吊销状态启动（revoked）时不联系 Server，恢复的 attempt
// 同样按失效停止。

/** protocolVersion 是本连接器实现的 AI 协议版本（agentcfg.AIProtocol）。 */
export const protocolVersion = 10;
/** maxReportedUnreadable：上报和日志中最多列出这么多条读不了的记录。 */
const maxReportedUnreadable = 3;
/** retention 是本机过程数据（任务目录、邮箱落盘的消息）的保留时间（C10）。 */
export const retentionMs = 12 * 60 * 60_000;
const gcIntervalMs = 60 * 60_000;
/** defaultStatusIntervalMs：每隔这么久重新检测状态（Orca、Docker、Agent、journal）。 */
export const defaultStatusIntervalMs = 60_000;
/** defaultIdleMs：standby 或领取失败后等这么久再上报 / 领取。 */
export const defaultIdleMs = 5_000;
const acquireWaitSeconds = 30;
/** defaultRestopIntervalMs：没能确认停止的 Agent 每隔这么久再停一次。 */
export const defaultRestopIntervalMs = 60_000;

/**
 * Stopping 是结束了但没能确认 Orca 中的 Agent 已经停止的任务。dispatchId 为空时只知道启动幂等键（启动请求可能已经
 * 发出、结果还不知道）：先用 request-show 查证。absentSince 是 request-show 连续报告没有这条请求的开始时间。
 */
interface Stopping {
  dispatchId: string;
  requestId: string;
  since: number;
  absentSince?: number;
  /** journal 记录：查到 dispatch 时写回（重启后直接停它）。 */
  rec: JournalRecord;
}

/** RestopResult 是 restop 结束跟踪时的原因（要释放的终端已经释放，没有确认的记在 rec.release 中）。 */
interface RestopResult {
  msg: string;
}

/**
 * Cleanup 是只剩结束后清理的任务（不占容量）：释放终端（rec.release），之后回收工作区（rec.reclaim）。lastError 是
 * 上一次没能完成的原因（相同的只记一次日志）。
 */
interface Cleanup {
  rec: JournalRecord;
  lastError: string;
}

export interface NodeOptions {
  version: string;
  capacity: number;
  hostId: string;
  /** 本机检测：Orca 的状态、Docker 是否可用。 */
  orcaStatus(): Promise<OrcaStatus>;
  dockerStatus(): Promise<{ ok: boolean; reason: string }>;
  /** drain 文件存在时不领取新任务。 */
  drained(): boolean;
  /** 没能确认停止的 Agent 再停的间隔（默认 defaultRestopIntervalMs）。 */
  restopIntervalMs?: number;
  /** standby 或领取失败后再次领取的间隔（默认 defaultIdleMs）。 */
  idleMs?: number;
  /** 重新检测状态的间隔（默认 defaultStatusIntervalMs）。 */
  statusIntervalMs?: number;
  /** 节点已经被吊销（上次运行时记下的）：不联系 Server，只停止、查证、回收 journal 中的任务。 */
  revoked?: boolean;
  /** onRevoked 在运行中第一次得知被吊销时调用：持久化吊销状态（重启后不再联系 Server）。 */
  onRevoked?(): Promise<void>;
}

/** cleanupStage：rec 只剩结束后的清理（释放终端、回收工作区）。 */
function cleanupStage(rec: JournalRecord): boolean {
  return (rec.stage === 'releasing' && !!rec.release) || (rec.stage === 'reclaiming' && !!rec.reclaim);
}

export class Node {
  private readonly running = new Map<string, Attempt>();
  private readonly runs = new Map<string, Promise<void>>();
  /** 结束了但没能确认 Orca 中的 Agent 已经停止的任务：继续占用容量，定期再停。 */
  private readonly stopping = new Map<string, Stopping>();
  /** 只剩结束后清理（释放终端、回收工作区）的任务：不占容量，定期重试。 */
  private readonly cleaning = new Map<string, Cleanup>();
  /** 检查历史残留时已经记过日志的跳过（每个进程只记一次）。 */
  private readonly residueLogged = new Set<string>();
  /** 历史残留的候选上一次尝试回收的时间（sweepResidue 的 tried：轮换，受阻的不挡住后面的）。 */
  private readonly residueTried = new Map<string, number>();
  private orca: OrcaStatus = { ready: false, reason: '还没有检测', version: '', runtimeId: '' };
  private docker = { ok: false, reason: '还没有检测' };
  /** 能否准备受管理的工具链（找到了版本一致的 kiln-runner，PLAN #33.12）。 */
  private toolchain = { ok: false, reason: '还没有检测' };
  /**
   * journal 的状态（每次检测状态时更新，见 journalProblem）：writeError 是写不进去的原因（probe），readError 是目录读不了的
   * 原因，unreadable 是读不了、没有在跟踪的记录（attempt → 记录）。
   */
  private readonly journalState = { writeError: '', readError: '', unreadable: new Map<string, UnreadableRecord>() };
  /** 上一次检测时的 journalProblem（状态变化时记日志）。 */
  private lastJournalProblem = '';
  /** 随包分发的 kiln-task skill 不能同步到 Agent 目录的原因（为空表示正常）；有原因时不领取新任务。 */
  private skillError = '';
  /** 上一次检测时的 skillError（状态变化时记日志）。 */
  private lastSkillError = '';
  /** 启动恢复已经读过 journal：之后每次检测状态时重新读取。 */
  private recovered = false;
  private lastStatus = 0;
  private lastGC = 0;
  private lastRestop = 0;
  private drainDirective = false;
  /** 已经得知节点被吊销（之后不再领取、不再联系 Server）。 */
  private revoked = false;
  /** revokedCtl 在得知吊销时结束：打断领取的长轮询和等待。 */
  private readonly revokedCtl = new AbortController();
  /** 持久化吊销状态（onRevoked）的进行中的调用：退出之前等它结束。 */
  private persistRevoked: Promise<void> = Promise.resolve();
  /** attempt 用的依赖：RPC 得知吊销时通知节点。 */
  private readonly ad: AttemptDeps;

  constructor(
    private readonly d: AttemptDeps,
    private readonly o: NodeOptions,
  ) {
    this.ad = { ...d, onRevoked: () => this.revoke() };
  }

  /** busy 是占用的容量：执行中、没能确认停止，以及读不了的记录（可能有还在运行的 Agent）。待释放的终端不占容量。 */
  get busy(): number {
    return this.running.size + this.stopping.size + this.journalState.unreadable.size;
  }
  get runtimeId(): string {
    return this.orca.runtimeId;
  }
  get servicesReady() {
    return this.docker;
  }

  /** attempt 返回正在执行的 attempt（Agent 的工具调用用）。 */
  attempt(id: string): Attempt | undefined {
    return this.running.get(id);
  }

  /**
   * run 运行到 signal 结束或节点被吊销。signal 结束时在途 attempt 只停止跟踪（Agent 继续运行，重启后恢复）；被吊销时
   * 在途 attempt 按失效停止，等它们结束停止流程，再继续停止没能确认停止的 Agent，直到全部了结或 signal 结束，然后抛出
   * RevokedError。主循环意料之外的错误按退出处理（重启后恢复）并抛出。
   */
  async run(signal: AbortSignal): Promise<void> {
    if (this.o.revoked) {
      this.revoked = true;
      this.revokedCtl.abort();
      this.d.log.error('这个节点已在 Kiln 中被吊销：不联系 Server，只停止 journal 中还可能在运行的 Agent');
    }
    await this.refreshStatus(true);
    // 邮箱随节点一起结束（包括被吊销等异常退出）：吊销后的停止流程中 Agent 仍可能提问
    const stopPump = new AbortController();
    const pump = this.d.mailbox.pump(AbortSignal.any([signal, stopPump.signal]));
    try {
      await this.recover();
      await this.serve(signal);
    } finally {
      // 被吊销时在途 attempt 已经按失效停止（revoke）；否则只停止跟踪（Agent 继续运行，重启后恢复）
      if (!this.revoked) for (const at of this.running.values()) at.shutdown();
      await this.settle();
      if (this.revoked) await this.finishRevoked(signal);
      stopPump.abort();
      await pump;
    }
    if (this.revoked) throw new RevokedError();
  }

  /** serve 是主循环：上报状态、领取任务，直到 signal 结束或得知被吊销。 */
  private async serve(signal: AbortSignal): Promise<void> {
    const idleMs = this.o.idleMs ?? defaultIdleMs;
    const loop = AbortSignal.any([signal, this.revokedCtl.signal]);
    // 领取的幂等键：失败（包括 Server 已经分配但响应丢失）时用同一个重试，Server 按它返回同一个任务；收到响应后才换新的
    let requestId = randomUUID();
    while (!loop.aborted) {
      await this.refreshStatus(false);
      await this.housekeeping();
      const full = this.busy >= this.o.capacity;
      const standby = full || this.o.drained() || this.drainDirective || !this.orca.ready || this.journalProblem() !== '' || this.skillError !== '';
      let resp;
      try {
        resp = await this.d.kiln.acquire({ state: this.state(), requestId, waitSeconds: standby ? 0 : acquireWaitSeconds, standby }, loop);
      } catch (err) {
        if (err instanceof RevokedError) this.revoke();
        if (loop.aborted) break;
        this.d.log.warn('领取任务失败', { err: errMessage(err) });
        await sleep(idleMs, loop);
        continue;
      }
      requestId = randomUUID();
      this.drainDirective = resp.directives?.drain ?? false;
      if (resp.task) {
        try {
          this.launch(new Attempt(this.ad, resp.task), false);
        } catch (err) {
          // 任务不完整（例如缺少 attempt）：不执行，也不让主循环退出；Server 按失联结束它
          this.d.log.error('无法执行领取到的任务', { task: String(resp.task.taskId), err: errMessage(err) });
        }
      } else if (standby) {
        await sleep(idleMs, loop);
      }
    }
  }

  /**
   * revoke 处理"节点被吊销"（规则只写在这里，领取和各 attempt 的 RPC 都调用它）：只处理第一次；打断主循环、所有在途
   * attempt 按失效停止，并持久化吊销状态（失败只记日志：重启后会再次从 Server 得知）。
   */
  private revoke(): void {
    if (this.revoked) return;
    this.revoked = true;
    this.revokedCtl.abort();
    this.d.log.error('这个节点已在 Kiln 中被吊销：停止领取，停止所有在途任务的 Agent，不再提交结果');
    for (const at of this.running.values()) at.revoke();
    if (this.o.onRevoked) {
      this.persistRevoked = this.o.onRevoked().catch((err) => this.d.log.warn('记下吊销状态失败（重启后会再次从 Kiln 得知）', { err: errMessage(err) }));
    }
  }

  /** settle 等所有在途 attempt 结束（结束期间 launch 不会再加入新的：主循环已经退出）。 */
  private async settle(): Promise<void> {
    while (this.runs.size > 0) await Promise.allSettled([...this.runs.values()]);
  }

  /**
   * finishRevoked 是吊销后的清理：回收测试服务，继续停止没能确认停止的 Agent（含还要查证的启动请求）、释放待释放的终端
   * （释放是对本机 Orca 的操作，不需要 Server；同样最多 12 小时），直到全部了结或 signal 结束（剩下的留在 journal 中，
   * 下次以吊销状态启动时继续）。不联系 Server。
   *
   * 有读不了的 journal 记录时不算清理完（其中可能有停不掉的 Agent，以 0 退出后服务管理器不再重启、没人管它）：进程继续
   * 运行，每隔再停的间隔重新读取——读得出来了按失效恢复（停止它的 Agent），人删掉了文件就解除——然后才结束；解除后
   * 补做被跳过的回收（见 scanJournal）。收到退出信号时照常退出，下次以吊销状态启动时继续。
   */
  private async finishRevoked(signal: AbortSignal): Promise<void> {
    // 先等吊销状态写下：之后的清理即使出错退出，重启后也不再联系 Server
    await this.persistRevoked;
    await this.reclaimServices();
    const interval = this.o.restopIntervalMs ?? defaultRestopIntervalMs;
    const pending = () => this.stopping.size > 0 || this.cleaning.size > 0 || this.journalState.unreadable.size > 0;
    let blocked = false;
    while (pending() && !signal.aborted) {
      if (this.journalState.unreadable.size > 0) {
        if (!blocked) this.d.log.error('有读不了的 journal 记录，其中可能有停不掉的 Agent：处理之前不结束清理', { reason: this.journalProblem() });
        blocked = true;
        await this.scanJournal();
        // 读得出来的记录已经按失效恢复：等它们结束停止流程（没能确认停止的转入 stopping）
        await this.settle();
      }
      await this.housekeeping();
      if (pending()) await sleep(interval, signal);
    }
    if (pending()) {
      this.d.log.warn('收到退出信号，还有没能确认停止的 Agent、没能确认释放的终端或读不了的 journal 记录，下次启动时继续', {
        stopping: this.stopping.size,
        cleaning: this.cleaning.size,
        unreadable: this.journalState.unreadable.size,
      });
    }
  }

  /**
   * launch 在后台执行 attempt。run 结束（包括意料之外的错误，见 Attempt.abandon）后，没能确认停止的 Agent 转入 stopping：
   * 继续占用容量、定期再停；只剩释放终端的转入 releasing（journal 写不进去时也一样，只是只在内存中）。run 出错时下一轮立即重新检测状态（包括 journal
   * 能否写入），不能写入时不再领取新任务。
   */
  private launch(at: Attempt, resumed: boolean): void {
    const key = String(at.id);
    // 已经被吊销：不执行，只按失效停止（恢复的 attempt 也一样，不联系 Server）
    if (this.revoked) at.revoke();
    this.running.set(key, at);
    const done = at
      .run(resumed)
      .catch((err) => {
        this.d.log.error('执行任务出错', { attempt: key, err: errMessage(err) });
        this.lastStatus = 0;
      })
      .finally(() => {
        this.running.delete(key);
        this.runs.delete(key);
        if (at.unconfirmedStop) this.track(at.record, this.d.now());
        else if (cleanupStage(at.record)) this.trackCleanup(at.record);
      });
    this.runs.set(key, done);
  }

  /** track 继续停止 rec 的 Agent（有 dispatch，或者启动结果还不知道）；没有要停的时返回 false。 */
  private track(rec: JournalRecord, since: number): boolean {
    if (!rec.binding.dispatchId && !startUnknown(rec)) return false;
    this.stopping.set(rec.attemptId, { dispatchId: rec.binding.dispatchId, requestId: rec.binding.startRequestId, since, rec });
    return true;
  }

  /** trackCleanup 继续 rec 结束后的清理：释放终端、回收工作区（不占容量）。 */
  private trackCleanup(rec: JournalRecord): void {
    this.cleaning.set(rec.attemptId, { rec, lastError: '' });
  }

  /**
   * recover 恢复 journal 中未结束的任务；还没确认停止的 Agent 继续占用容量、继续停止；读不了的记录记下（见 block）。
   * 目录读不了时抛出（节点退出，不当成没有在途任务）。
   */
  private async recover(): Promise<void> {
    // 还没有任何写入：删除上次崩溃时留下的临时文件
    await this.d.journal.sweepTemp().catch(() => undefined);
    const scan = await inspectJournal(this.d.journal);
    this.recovered = true;
    for (const u of scan.unreadable) this.block(u);
    for (const r of scan.recoveries) await this.resume(r);
    this.logJournalProblem();
    await this.reclaimServices();
  }

  /**
   * resume 恢复一个没有在跟踪的 attempt：stopping 继续停止它的 Agent（没有要停的时删除记录），releasing 继续释放终端，
   * 其它恢复执行。
   */
  private async resume(r: Recovery): Promise<void> {
    switch (r.kind) {
      case 'stop':
        if (!this.track(r.rec, r.rec.stopSince ?? this.d.now())) await this.removeJournal(r.rec.attemptId);
        return;
      case 'cleanup':
        this.trackCleanup(r.rec);
        return;
      case 'resume':
        this.launch(new Attempt(this.ad, r.task, r.rec), true);
        return;
      default: {
        const _exhaustive: never = r;
        return _exhaustive;
      }
    }
  }

  /**
   * block 记下一条读不了的记录（已经在跟踪的 attempt 以内存中的状态为准，不记）。第一次记下或原因变化时记 error 日志：
   * 写明文件、原因和处理方法。
   */
  private block(u: UnreadableRecord): void {
    if (this.tracked(u.attemptId)) return;
    const prev = this.journalState.unreadable.get(u.attemptId);
    this.journalState.unreadable.set(u.attemptId, u);
    if (prev?.error === u.error) return;
    this.d.log.error('journal 记录读不了：其中可能有还在运行的 Agent，停止领取新任务、不回收测试服务，等人处理', {
      attempt: u.attemptId,
      file: u.file,
      err: u.error,
      fix: unreadableFix,
    });
  }

  /**
   * scanJournal 重新读取 journal（每次检测状态时，启动恢复之后）：读不了的记录读得出来了就按恢复流程接着执行，文件被删除
   * 了就解除；新出现的、没有在跟踪的读不了的记录同样记下。没有在跟踪、也不是之前读不了的可读记录不在这里恢复（例如删除
   * 失败留下的已经结束的记录，下次启动时处理）。目录读不了时记为 readError（不领取、不回收），读不了的记录保持原样。
   * 读不了的记录全部解除后，下一次 housekeeping 立即补做被跳过的清理（任务目录、邮箱、测试服务）。
   */
  private async scanJournal(): Promise<void> {
    let scan;
    try {
      scan = await inspectJournal(this.d.journal);
    } catch (err) {
      this.journalState.readError = `journal 目录读不了：${errMessage(err)}`;
      return;
    }
    this.journalState.readError = '';
    const blocked = this.journalState.unreadable.size > 0;
    const bad = new Map(scan.unreadable.map((u) => [u.attemptId, u]));
    const readable = new Map(scan.recoveries.map((r) => [r.rec.attemptId, r]));
    for (const [id, u] of [...this.journalState.unreadable]) {
      if (bad.has(id)) continue;
      this.journalState.unreadable.delete(id);
      const r = readable.get(id);
      if (!r) {
        this.d.log.info('读不了的 journal 记录已经删除，不再占用容量', { attempt: id, file: u.file });
        continue;
      }
      this.d.log.info('journal 记录可以读取了，按恢复流程接着执行', { attempt: id, file: u.file });
      await this.resume(r);
    }
    for (const u of bad.values()) this.block(u);
    if (blocked && this.journalState.unreadable.size === 0) this.lastGC = 0;
  }

  /**
   * journalProblem 是 journal 不可用的原因（写不进去、目录读不了、有读不了的记录），没有问题时为空。有问题时不领取新任务、
   * 不回收测试服务，原因上报给 Server（见 state）。
   */
  private journalProblem(): string {
    const { writeError, readError, unreadable } = this.journalState;
    const parts = [writeError, readError];
    if (unreadable.size > 0) {
      const list = [...unreadable.values()].slice(0, maxReportedUnreadable).map((u) => `${u.file}（${u.error}）`);
      if (unreadable.size > maxReportedUnreadable) list.push(`等 ${unreadable.size} 条`);
      parts.push(`有 ${unreadable.size} 条 journal 记录读不了：${list.join('、')}；${unreadableFix}`);
    }
    return parts.filter((p) => p !== '').join('；');
  }

  /** logJournalProblem 在 journal 的状态变化时记日志。 */
  private logJournalProblem(): void {
    const p = this.journalProblem();
    if (p === this.lastJournalProblem) return;
    if (p) this.d.log.error('journal 不可用，停止领取新任务、不回收测试服务', { reason: p });
    else this.d.log.info('journal 恢复正常，恢复领取');
    this.lastJournalProblem = p;
  }

  /**
   * reclaimServices 回收测试服务删除失败后残留的项目：保留还在执行的 attempt（包括刚恢复的）和 journal 中未结束的 attempt
   * 的项目；stopping 记录的任务已经结束，不需要服务。Docker 不可用时跳过，失败时下一次周期清理再试。journal 不完整（有
   * 读不了的记录，包括这次读取新发现的）时不回收：不知道哪些项目还在用。
   */
  private async reclaimServices(): Promise<void> {
    if (!this.d.services || !this.docker.ok) return;
    const keep = new Set(this.running.keys());
    try {
      // journal 目录读不了时不回收（不知道哪些项目还在用），也不让主循环退出
      const scan = await inspectJournal(this.d.journal);
      const untracked = scan.unreadable.filter((u) => !this.tracked(u.attemptId));
      if (untracked.length > 0) {
        this.d.log.warn('journal 中有读不了的记录，不回收测试服务', { files: untracked.map((u) => u.file) });
        return;
      }
      for (const r of scan.recoveries) if (r.kind === 'resume') keep.add(r.rec.attemptId);
      await this.d.services.cleanup(keep);
    } catch (err) {
      this.d.log.warn('回收残留的测试服务失败，下一次周期清理时重试', { err: errMessage(err) });
    }
  }

  /** tracked：这个 attempt 正在跟踪（执行中、停止中、释放中），以内存中的状态为准。 */
  private tracked(attemptId: string): boolean {
    return this.running.has(attemptId) || this.stopping.has(attemptId) || this.cleaning.has(attemptId);
  }

  /** removeJournal 删除 journal；失败时只记日志（重启后再处理），不让主循环退出。 */
  private async removeJournal(key: string): Promise<void> {
    await this.d.journal.remove(key).catch((err) => this.d.log.warn('删除 journal 失败', { attempt: key, err: errMessage(err) }));
  }

  private async refreshStatus(force: boolean): Promise<void> {
    const now = this.d.now();
    if (!force && now - this.lastStatus < (this.o.statusIntervalMs ?? defaultStatusIntervalMs)) return;
    this.lastStatus = now;
    this.orca = await this.o.orcaStatus();
    this.docker = await this.o.dockerStatus();
    this.toolchain = await this.d.toolchain.status();
    this.journalState.writeError = await this.d.journal.probe().then(
      () => '',
      (err: unknown) => `journal 写不进去（磁盘满、只读或权限）：${errMessage(err)}`,
    );
    if (this.recovered) await this.scanJournal();
    this.logJournalProblem();
    if (this.orca.ready) {
      try {
        await this.d.mailbox.ensure();
      } catch (err) {
        this.orca = { ...this.orca, ready: false, reason: `无法在 Orca 中建立连接器的邮箱：${errMessage(err)}` };
      }
    }
    await this.d.agents.refresh(force);
    await this.syncSkills();
  }

  /**
   * syncSkills 把随包分发的 kiln-task skill 同步到对外提供的 Agent 的目录：失败时记入 skillError（不领取新任务，
   * 原因上报给 Server），冲突消除后下一次检测自动恢复。不抛出：skill 的问题不阻断恢复 journal 中已经在途的任务（它们启动
   * Agent 时自己会再调用 ensureForAgent，失败时按 infra.prepare 结束）。
   */
  private async syncSkills(): Promise<void> {
    try {
      this.skillError = await this.d.skill.ensureAll(this.d.agents.offered.map((a) => a.name));
    } catch (err) {
      this.skillError = `准备 kiln-task skill 失败：${errMessage(err)}`;
    }
    if (this.skillError === this.lastSkillError) return;
    if (this.skillError) this.d.log.error('kiln-task skill 不能同步到 Agent 的目录，停止领取新任务', { reason: this.skillError });
    else this.d.log.info('kiln-task skill 恢复正常，恢复领取');
    this.lastSkillError = this.skillError;
  }

  /** terminalDeps 是查证启动请求、释放终端（dispatch.ts）用的依赖。 */
  private get terminalDeps(): TerminalDeps {
    return { orca: this.d.orca, log: this.d.log, now: this.d.now };
  }

  /**
   * restop 再停一次；结束跟踪时返回原因。只有启动幂等键时先查证（checkStart，对记录的处理见 dispatch.ts 的 StartCheck）：
   * started → 写回 journal 并停止它；failed → Orca 没有启动，结束（它建了的 dispatch 的终端已经回收或记为待释放）；absent
   * （查询成功、Orca 明确没有这条请求的记录）从进入跟踪时算起持续超过查证窗口（startVerifyMs）→ 结束（启动命令已经返回，
   * 请求不会还在途中：一直查不到就是 Orca 没有收到）；pending、查询失败（连不上 Orca、超时）都不能断定，继续跟踪（最多到
   * 保留时间），也不计入 absent 的窗口。确认停止后释放它的终端（停止不释放）。
   */
  private async restop(key: string, s: Stopping, now: number): Promise<RestopResult | undefined> {
    if (!s.dispatchId) {
      const c = await checkStart(this.d.orca, s.requestId);
      await settleStart(this.terminalDeps, s.rec, c);
      switch (c.kind) {
        case 'failed':
          return { msg: 'Orca 报告启动请求失败，没有启动 Agent' };
        case 'absent':
          s.absentSince ??= now;
          if (now - s.absentSince >= this.d.timing.startVerifyMs) return { msg: `Orca 一直没有启动请求 ${s.requestId} 的记录，按没有启动处理` };
          return undefined;
        case 'pending':
        case 'unknown':
          s.absentSince = undefined;
          return undefined;
        case 'started':
          s.absentSince = undefined;
          s.dispatchId = c.worker.dispatchId;
          this.d.log.info('查到了启动请求启动的 Agent，停止它', { attempt: key, dispatch: s.dispatchId });
          // 写不进去时只在内存中，重启后再查证
          await this.d.journal.save(s.rec).catch((err) => this.d.log.warn('写 journal 失败', { attempt: key, err: errMessage(err) }));
          break;
        default: {
          const _exhaustive: never = c;
          return _exhaustive;
        }
      }
    }
    const stopped = await this.d.orca.workerStop(s.dispatchId).catch(() => false);
    if (!stopped) return undefined;
    await releaseTerminal(this.terminalDeps, s.rec, s.dispatchId);
    return { msg: '已确认 Orca 中的 Agent 停止' };
  }

  /**
   * finishStopping 结束对 s 的跟踪（确认停止、查证完毕或超过保留时间）。还有没能确认释放的终端（rec.release）或要回收的
   * 工作区（rec.reclaim，只在确认停止时）时转为 releasing / reclaiming（先写 journal 再释放容量），否则删除 journal、
   * 释放容量（先删 journal：容量释放时 journal 中已经没有这条）。
   */
  private async finishStopping(key: string, s: Stopping, done: RestopResult | undefined): Promise<void> {
    const msg = done ? `${done.msg}，不再占用容量` : '超过保留时间仍不能确认 Agent 停止，不再占用容量';
    if (s.rec.reclaim && !done) {
      // Agent 可能还在工作区中运行：不回收
      this.d.log.error('没能确认 Agent 停止，不回收工作区（需要人处理）', { attempt: key, worktree: s.rec.binding.worktreePath, fix: reclaimFix });
      s.rec.reclaim = undefined;
    }
    if (s.rec.release || s.rec.reclaim) {
      s.rec.stage = s.rec.release ? 'releasing' : 'reclaiming';
      await this.d.journal.save(s.rec).catch((err) => this.d.log.warn('写 journal 失败', { attempt: key, err: errMessage(err) }));
      this.stopping.delete(key);
      this.trackCleanup(s.rec);
      const pending = s.rec.release ? '释放它的终端没有确认' : '还要回收工作区';
      this.d.log.info(`${msg}；${pending}，稍后进行`, { attempt: key, dispatch: s.rec.release?.dispatchId ?? '' });
      return;
    }
    await this.removeJournal(key);
    this.stopping.delete(key);
    this.d.log.info(msg, { attempt: key });
  }

  /**
   * recleanup 继续 c 结束后的清理，全部完成（或放弃）后删除记录：
   * - 释放终端（releaseTerminal）：确认（包括已经释放、不存在）或被 Orca 明确拒绝时不再释放；结果未知时继续跟踪，超过
   *   保留时间放弃（记 warn，写明 dispatch；这时也不回收工作区）。
   * - 回收工作区（reclaimWorkspace，仓库正忙时这一轮跳过）：回收完结束；没有完成时继续跟踪，超过保留时间放弃（记 error，
   *   写明处理方法）。终端被 Orca 拒绝释放时 Agent 可能还没有结束：回收前的检查（工作区中没有活着的终端）会让它等。
   */
  private async recleanup(key: string, c: Cleanup, now: number): Promise<void> {
    const rec = c.rec;
    if (rec.release) {
      const { dispatchId, since } = rec.release;
      const out = await releaseTerminal(this.terminalDeps, rec, dispatchId);
      if (out.kind === 'unknown') {
        if (now - since <= retentionMs) {
          if (out.error !== c.lastError) this.d.log.warn('释放 Orca 中 Agent 的终端还没有确认，稍后重试', { attempt: key, dispatch: dispatchId, err: out.error });
          c.lastError = out.error;
          return;
        }
        this.d.log.warn('超过保留时间仍不能确认释放 Orca 中 Agent 的终端，放弃（可在 Orca 中 worker-release 或关闭它的终端）', { attempt: key, dispatch: dispatchId, err: out.error });
        rec.release = undefined;
        if (rec.reclaim) {
          this.d.log.error('终端一直没能确认释放，不回收工作区（需要人处理）', { attempt: key, worktree: rec.binding.worktreePath, fix: reclaimFix });
          rec.reclaim = undefined;
        }
      } else if (out.kind === 'done') {
        this.d.log.info('已释放 Orca 中 Agent 的终端', { attempt: key, dispatch: dispatchId, state: out.state });
      }
    }
    if (rec.reclaim) {
      if (rec.stage !== 'reclaiming') {
        rec.stage = 'reclaiming';
        await this.d.journal.save(rec).catch((err) => this.d.log.warn('写 journal 失败', { attempt: key, err: errMessage(err) }));
      }
      const t = reclaimTarget(rec, rec.reclaim);
      const out = await reclaimWorkspace(this.d, t, { skipIfBusy: true });
      if (out.kind !== 'done') {
        const reason = out.kind === 'wait' ? out.reason : out.error;
        if (now - rec.reclaim.since <= retentionMs) {
          if (reason !== c.lastError) this.d.log.warn('回收工作区还没有完成，稍后重试', { attempt: key, kind: t.kind, reason });
          c.lastError = reason;
          return;
        }
        this.d.log.error('超过保留时间仍没能回收工作区，放弃（需要人处理）', { attempt: key, kind: t.kind, worktree: rec.binding.worktreePath, reason, fix: reclaimFix });
      } else {
        this.d.log.info('已回收工作区', { attempt: key, kind: t.kind, removed: out.removed.join('、') });
      }
    }
    await this.removeJournal(key);
    this.cleaning.delete(key);
  }

  private async housekeeping(): Promise<void> {
    const now = this.d.now();
    if (now - this.lastRestop >= (this.o.restopIntervalMs ?? defaultRestopIntervalMs) && (this.stopping.size > 0 || this.cleaning.size > 0)) {
      this.lastRestop = now;
      // 先处理已有的待释放：这一轮由 stopping 转来的下一轮再试
      for (const [key, c] of [...this.cleaning]) await this.recleanup(key, c, now);
      for (const [key, s] of [...this.stopping]) {
        const done = await this.restop(key, s, now);
        if (done || now - s.since > retentionMs) await this.finishStopping(key, s, done);
      }
    }
    if (now - this.lastGC >= gcIntervalMs) {
      this.lastGC = now;
      // 读不了的记录的任务目录保留；它的结束消息（按 dispatch 落盘）不知道是哪个，journal 不完整时不清理邮箱
      const keep = new Set([...this.running.keys(), ...this.journalState.unreadable.keys()]);
      const cutoff = now - retentionMs;
      await removeOlderThan(this.d.tasksDir, cutoff, keep).catch(() => undefined);
      if (this.journalState.unreadable.size === 0) await removeOlderThan(path.join(path.dirname(this.d.tasksDir), 'inbox'), cutoff).catch(() => undefined);
      await this.reclaimServices();
      await this.reclaimToolchains();
      await this.sweepResidue(now);
      await this.sweepMailbox();
    }
  }

  /**
   * sweepMailbox 关闭邮箱仓库中多余的邮箱终端（mailbox.ts 的 sweep：连接器以前建的、没有关掉的；每轮有上限）。Orca 没有
   * 就绪时不清理；失败只记日志，下一次周期清理再试。不抛出。
   */
  private async sweepMailbox(): Promise<void> {
    if (!this.orca.ready) return;
    try {
      await this.d.mailbox.sweep();
    } catch (err) {
      this.d.log.warn('清理多余的邮箱终端失败，下一次周期清理时重试', { err: errMessage(err) });
    }
  }

  /**
   * sweepResidue 有界地回收升级前留下的（或者回收记录丢失的）已经结束的执行的工作区（reclaim.ts 的 sweepResidue）。journal 中
   * 有记录的、正在跟踪的 attempt 不碰；journal 不完整（有读不了的记录）、Orca 没有就绪时不检查。不抛出。
   */
  private async sweepResidue(now: number): Promise<void> {
    if (!this.orca.ready) return;
    try {
      const scan = await inspectJournal(this.d.journal);
      if (scan.unreadable.length > 0) {
        this.d.log.warn('journal 中有读不了的记录，不检查历史残留的工作区', { files: scan.unreadable.map((u) => u.file) });
        return;
      }
      const keep = new Set([...this.running.keys(), ...this.stopping.keys(), ...this.cleaning.keys(), ...scan.recoveries.map((r) => r.rec.attemptId)]);
      await sweepResidue(this.d, { keep, now, logged: this.residueLogged, tried: this.residueTried, journal: this.d.journal, track: (rec) => this.trackCleanup(rec) });
    } catch (err) {
      this.d.log.warn('检查历史残留的工作区失败，下一次周期清理时重试', { err: errMessage(err) });
    }
  }

  /**
   * reclaimToolchains 回收工具链目录（PLAN #33.12）：30 天没有用到的、超过容量上限时最久没用的（规则在 kiln-runner 中，与
   * CI Runner 相同）；执行中的 attempt 和 journal 中未结束的 attempt 用到的不删。journal 不完整时不回收（不知道哪些还在用）。
   * 失败时下一次周期清理再试。
   */
  private async reclaimToolchains(): Promise<void> {
    const keep = new Set<string>();
    const add = (rec: JournalRecord) => {
      for (const t of rec.toolchain?.tools ?? []) keep.add(t.digest);
    };
    try {
      for (const a of this.running.values()) {
        add(a.record);
        for (const t of a.spec.tools) keep.add(t.digest);
      }
      const scan = await inspectJournal(this.d.journal);
      const untracked = scan.unreadable.filter((u) => !this.tracked(u.attemptId));
      if (untracked.length > 0) {
        this.d.log.warn('journal 中有读不了的记录，不回收工具链', { files: untracked.map((u) => u.file) });
        return;
      }
      for (const r of scan.recoveries) {
        add(r.rec);
        if (r.kind === 'resume') for (const t of r.task.tools) keep.add(t.digest);
      }
      const res = await this.d.toolchain.gc(keep);
      if (!res) return;
      if (res.removed > 0) this.d.log.info('回收了不再使用的工具链', { removed: res.removed, bytes: res.removedBytes });
      if (res.failures.length > 0) this.d.log.warn('部分工具链没有删掉，下一次周期清理时重试', { failures: res.failures });
    } catch (err) {
      this.d.log.warn('回收工具链失败，下一次周期清理时重试', { err: errMessage(err) });
    }
  }

  state(): MessageInitShape<typeof AIRunnerStateSchema> {
    const journal = this.journalProblem();
    return {
      version: this.o.version,
      protocol: protocolVersion,
      platform: runnerPlatform(),
      hostId: this.o.hostId,
      running: this.busy,
      capacity: this.o.capacity,
      agents: this.d.agents.snapshot().map((a) => ({
        name: a.name,
        installed: a.installed,
        version: a.version,
        state: a.available ? 'available' : 'unavailable',
        reason: a.reason,
        checkedAt: BigInt(Math.floor(a.checkedAt / 1000)),
        backoffUntil: BigInt(Math.floor(a.backoffUntil / 1000)),
      })),
      // journal 不可用、kiln-task skill 同步不了时节点同样不能执行任务：放进 Orca 的就绪状态上报（Server 据此不派任务，
      // 并在 Dashboard 中显示原因）
      orca: {
        ready: this.orca.ready && journal === '' && this.skillError === '',
        reason: [journal && `连接器（kiln-orca）${journal}`, this.skillError && `kiln-task skill：${this.skillError}`, this.orca.reason]
          .filter((r) => r !== '')
          .join('；'),
        version: this.orca.version,
        runtimeId: this.orca.runtimeId,
      },
      docker: { ready: this.docker.ok, reason: this.docker.reason },
      toolchain: { ready: this.toolchain.ok, reason: this.toolchain.reason },
    };
  }
}
