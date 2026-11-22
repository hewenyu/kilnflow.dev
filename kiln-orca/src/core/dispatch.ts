import { errMessage } from './errors.js';
import type { BindingRecord, JournalRecord } from './journal.js';
import type { Logger } from './log.js';
import { isDefinite, type Orca, releaseRefusedFix, releaseWorker, type ReleaseOutcome, StartFailedError, type WorkerRef } from './orca.js';

// Orca 中 dispatch 的查证与回收（PLAN #33.4）：拿到一次启动请求的结果（worker-start 的错误、request-show 的回答）之后对
// journal 记录做什么、释放一个 dispatch 的终端之后记录怎么变，只在这里定义。调用方（Attempt.verifyStart、Attempt.stopAgent、
// Node.restop，以及各处的释放）只决定接下来继续等、停止，还是结束。

/**
 * StartCheck 是查证一次启动请求的结论（checkStart）。settleStart 对 journal 记录要做的事：
 *
 * | kind | 含义 | 记录 |
 * |---|---|---|
 * | started | Orca 启动了 Agent（request-show completed，回执是 started） | 绑定 dispatch |
 * | failed | Orca 明确没有启动 Agent（回执 failed，或者 worker-start 被明确拒绝且 Orca 没有这条请求） | 记启动失败（startFailed）；dispatchId 非空（Orca 已经建了 dispatch）时释放它的终端，结果未知时记为待释放（rec.release） |
 * | pending | 请求还在处理 | 不变 |
 * | absent | Orca 没有这条请求的记录（不能证明没有启动：可能还在途中） | 不变 |
 * | unknown | 查询失败（连不上 Orca、超时、输出不对） | 不变 |
 */
export type StartCheck =
  | { kind: 'started'; worker: WorkerRef }
  | { kind: 'failed'; error: string; dispatchId: string }
  | { kind: 'pending' }
  | { kind: 'absent' }
  | { kind: 'unknown'; error: string };

/**
 * checkStart 用 request-show 查证启动请求（不抛出）。startErr 是同一次执行中 worker-start 的错误（没有时为 undefined）：
 * Orca 明确拒绝了请求（isDefinite）而查不到这条请求时，按没有启动（failed）处理；回执中带出的 dispatch（StartFailedError）
 * 优先于 request-show 中的。
 */
export async function checkStart(orca: Orca, requestId: string, startErr?: unknown): Promise<StartCheck> {
  let info;
  try {
    info = await orca.requestShow(requestId);
  } catch (err) {
    return { kind: 'unknown', error: errMessage(err) };
  }
  const leftover = startErr instanceof StartFailedError ? startErr.dispatchId : '';
  const why = (fallback: string): string => (startErr === undefined ? fallback : errMessage(startErr));
  switch (info.state) {
    case 'completed':
      return info.worker ? { kind: 'started', worker: info.worker } : { kind: 'unknown', error: 'request-show 报告 completed 但没有回执' };
    case 'failed':
      return { kind: 'failed', error: why(info.error ?? ''), dispatchId: leftover || (info.dispatchId ?? '') };
    case 'absent':
      return isDefinite(startErr) ? { kind: 'failed', error: why(''), dispatchId: leftover } : { kind: 'absent' };
    case 'pending':
      return { kind: 'pending' };
    default: {
      const s: never = info.state;
      return { kind: 'unknown', error: `request-show 返回了未知的状态 ${String(s)}` };
    }
  }
}

/** TerminalDeps 是查证、释放时用到的依赖。print 写这个任务的日志（Attempt 中有；节点的跟踪中没有）。 */
export interface TerminalDeps {
  orca: Orca;
  log: Logger;
  now: () => number;
  print?: (line: string) => void;
}

/** bindWorker 记下 Orca 启动的 Agent（回执）。 */
export function bindWorker(b: BindingRecord, w: WorkerRef): void {
  b.runId = w.runId;
  b.taskId = w.taskId;
  b.dispatchId = w.dispatchId;
  if (w.worktreePath) b.worktreePath = w.worktreePath;
}

/** settleStart 按查证的结论更新 rec（见 StartCheck 的表）；不写 journal、不抛出。 */
export async function settleStart(d: TerminalDeps, rec: JournalRecord, c: StartCheck): Promise<void> {
  switch (c.kind) {
    case 'started':
      bindWorker(rec.binding, c.worker);
      return;
    case 'failed':
      rec.startFailed = true;
      // Orca 已经建了 dispatch（例如建 worktree、启动 Agent 时失败）：回收它可能留下的终端（没有时 Orca 回答 retained）
      if (c.dispatchId) await releaseTerminal(d, rec, c.dispatchId);
      return;
    case 'pending':
    case 'absent':
    case 'unknown':
      return;
    default: {
      const _exhaustive: never = c;
      return _exhaustive;
    }
  }
}

/**
 * releaseTerminal 释放 dispatch 的终端（Agent 已经结束、已确认停止，或者启动失败；只关终端，不删工作区），并按结果更新 rec 的
 * 待释放记录：确认（包括已经释放、不存在、Orca 不替连接器关闭）时清除；结果未知（release_unknown、连不上 Orca）时记下
 * （rec.release，第一次没能确认的时间不变；不写 journal，随之后的写入保存），之后由节点重试；Orca 明确拒绝（例如 dispatch
 * 还没有结束）时重试也不会变：记 error 日志（写明处理方法）、清除，不再重试。不抛出。
 */
export async function releaseTerminal(d: TerminalDeps, rec: JournalRecord, dispatchId: string): Promise<ReleaseOutcome> {
  const r = await releaseWorker(d.orca, dispatchId);
  if (r.kind === 'unknown') {
    rec.release ??= { dispatchId, since: d.now() };
    d.print?.(`释放 Orca 中 Agent 的终端没有确认（${r.error}）：任务照常结束，之后由节点重试释放`);
    return r;
  }
  if (rec.release?.dispatchId === dispatchId) rec.release = undefined;
  if (r.kind === 'refused') {
    d.print?.(`Orca 拒绝释放 Agent 的终端：${r.error}`);
    d.log.error('Orca 拒绝释放 Agent 的终端，不再重试', { attempt: rec.attemptId, dispatch: dispatchId, err: r.error, fix: releaseRefusedFix });
  }
  return r;
}
