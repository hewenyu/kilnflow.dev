import { type Exec, type ExecResult, notRun, tail } from './exec.js';

// kiln-orca task done（PLAN #33.5）：Agent 用它结束这一轮。连接器按任务的类型和阶段校验（与 check-report / claim / deliver
// 相同的规则）、在 journal 中记下固定的请求 id，回复发送参数；命令行在**自己的进程中**（Agent 的后代，环境原样继承）不带
// --from 执行 Orca 命令行发送 worker_done：Orca 按继承的环境（structured 会话的 ORCA_AGENT_SESSION_ID，终端模式的
// ORCA_TERMINAL_HANDLE + ORCA_PANE_KEY）认定发送者就是这个 Agent，与 Agent 自己发送相同。连接器不冒充 Agent 发送。
// 结束仍以邮箱收到的 worker_done（或 worker-show 兜底）为准，结束时的校验由连接器照常再做一次。

/** doneCmd 是 Agent 结束这一轮的工具命令。 */
export const doneCmd = 'done';
/** doneReceiptCmd 是 task done 把 Orca 的回执摘要交回连接器记日志的内部命令（不在用法中）。 */
export const doneReceiptCmd = 'done-receipt';

/** doneOutcomes 是 task done 的 --outcome（与 Orca 的 worker_done 相同）。 */
export const doneOutcomes = ['succeeded', 'failed'] as const;
export type DoneOutcome = (typeof doneOutcomes)[number];

/**
 * OrcaFallback 是 Agent 的环境中没有 ORCA_CLI_COMMAND 时用的 Orca 命令行：连接器自己执行的那个。command 带 / 时是绝对路径；
 * 只是名字时按 path（连接器的 PATH）查找。
 */
export interface OrcaFallback {
  command: string;
  path: string;
}

/** DoneSend 是连接器校验通过后给 task done 的发送参数。 */
export interface DoneSend {
  taskId: string;
  dispatchId: string;
  outcome: DoneOutcome;
  subject: string;
  body: string;
  /** 固定的请求 id（UUID）：第一次发送之前记进 journal，重跑沿用，Orca 重放同一个请求、不会重复结算。 */
  retryRequest: string;
  orca: OrcaFallback;
}

/** doneSendTimeoutMs 是发送 worker_done 的超时：长于 Orca 命令行在 Orca 暂时不可用时自己的重试（120 秒）。 */
export const doneSendTimeoutMs = 180_000;

/** parseDoneSend 校验连接器的回复（格式不对时抛出）。 */
export function parseDoneSend(text: string): DoneSend {
  const v = JSON.parse(text) as unknown;
  const o = isObj(v) ? v : {};
  const orca = isObj(o.orca) ? o.orca : {};
  const strs = [o.taskId, o.dispatchId, o.subject, o.body, o.retryRequest, orca.command, orca.path];
  if (!strs.every((s) => typeof s === 'string') || !doneOutcomes.includes(o.outcome as DoneOutcome) || !o.taskId || !o.dispatchId || !o.retryRequest || !orca.command) {
    throw new Error('连接器回复的发送参数格式不对');
  }
  return v as DoneSend;
}

/** OrcaInvocation 是 task done 执行的 Orca 命令行：cmd 和在继承的环境上追加的变量。 */
export interface OrcaInvocation {
  cmd: string;
  env: Record<string, string>;
  /** 命令从哪里来：Agent 环境中的 ORCA_CLI_COMMAND，还是连接器的配置。 */
  source: 'ORCA_CLI_COMMAND' | 'config';
}

/**
 * orcaInvocation 选择 Orca 命令行：优先用 Agent 环境中的 ORCA_CLI_COMMAND（Orca 给 structured 会话设置的命令行的绝对路径）；
 * 没有时用连接器的那个——带路径时直接执行，只是名字时把子进程的 PATH 换成连接器的 PATH、按名字执行（查找交给真实的执行器，
 * 与连接器执行 Orca 时相同，不在用户态重做）。其余环境原样继承：不清理 ORCA_*，身份就来自它们。
 */
export function orcaInvocation(env: NodeJS.ProcessEnv, fb: OrcaFallback): OrcaInvocation {
  const fromEnv = env.ORCA_CLI_COMMAND;
  if (fromEnv) return { cmd: fromEnv, env: {}, source: 'ORCA_CLI_COMMAND' };
  return { cmd: fb.command, env: fb.command.includes('/') ? {} : { PATH: fb.path }, source: 'config' };
}

/** workerDoneArgs 是发送 worker_done 的参数：不带 --from（身份来自继承的环境），带固定的 --retry-request。 */
export function workerDoneArgs(s: DoneSend): string[] {
  return [
    'orchestration', 'send', '--type', 'worker_done', '--task-id', s.taskId, '--dispatch-id', s.dispatchId, '--outcome', s.outcome,
    '--subject', s.subject, '--body', s.body, '--retry-request', s.retryRequest, '--json', '--full',
  ];
}

/**
 * DoneReceipt 是发送的结果：reported 是 Orca 已经结算（completed、failed）或者之前已经结算（settled）；identity 是 Orca 不认
 * 发送者；retry 是结果未知或 Orca 暂时不可用（重跑同一条命令，同一个请求重放）；exec 是这台机器上执行不了 Orca 命令行；
 * refused 是 Orca 的其它拒绝。
 */
export type DoneReceipt =
  | { kind: 'reported'; action: 'completed' | 'failed' | 'settled'; messageId: string; requestId: string }
  | { kind: 'identity' | 'retry' | 'refused'; code: string; message: string }
  | { kind: 'exec'; message: string };

/** identityCodes 是 Orca 不认发送者的错误码（环境中的身份不对或者被清理了）。 */
export const identityCodes: readonly string[] = ['no_active_sender_terminal', 'consumer_fenced', 'worker_identity_changed', 'sender_not_assignee'];
/** retryCodes 是要重跑同一条命令的错误码：结算没有确认、Orca 暂时不可用。 */
export const retryCodes: readonly string[] = ['operation_unknown', 'runtime_unavailable'];
/** reportedActions 是 worker_done 已经结算的回执（lifecycle.action）。 */
const reportedActions = ['completed', 'failed', 'settled'] as const;
/** classifyReceipt 按 Orca 命令行的输出（--json）归类发送的结果。 */
export function classifyReceipt(r: ExecResult): DoneReceipt {
  const text = r.stdout
    .split('\n')
    .filter((l) => !l.startsWith('{"_keepalive"'))
    .join('\n')
    .trim();
  let v: unknown;
  try {
    v = text ? JSON.parse(text) : undefined;
  } catch {
    v = undefined;
  }
  if (!isObj(v)) {
    const why = tail((r.stderr.trim() || text || `退出码 ${r.code}`).replace(/\s+/g, ' '), 500);
    // 启动不了（找不到、没有权限）：本机的问题。其它（超时、被中止、输出不是 Orca 的回执）：结果未知，重跑同一个请求
    return notRun(r) && !text ? { kind: 'exec', message: why } : { kind: 'retry', code: 'no_receipt', message: why };
  }
  if (v.ok === true) {
    const res = isObj(v.result) ? v.result : {};
    const life = isObj(res.lifecycle) ? res.lifecycle : {};
    const action = life.action;
    if (reportedActions.includes(action as (typeof reportedActions)[number])) {
      const msg = isObj(res.message) ? res.message : {};
      const mut = isObj(res.mutation) ? res.mutation : {};
      return { kind: 'reported', action: action as (typeof reportedActions)[number], messageId: str(msg.id), requestId: str(mut.requestId) };
    }
    return { kind: 'retry', code: 'no_lifecycle', message: `Orca 的回执中没有结算的结果（lifecycle.action：${str(action) || '没有'}）` };
  }
  const e = isObj(v.error) ? v.error : {};
  const code = str(e.code) || 'unknown';
  const message = tail(str(e.message), 500);
  if (identityCodes.includes(code)) return { kind: 'identity', code, message };
  if (retryCodes.includes(code)) return { kind: 'retry', code, message };
  return { kind: 'refused', code, message };
}

/**
 * sendWorkerDone 在本进程中执行 Orca 命令行发送 worker_done（kiln-orca task done 的发送这一步，可以单独调用）。env 是本进程的
 * 环境（选择命令行用），子进程继承它（Exec 只追加 inv.env）。
 */
export async function sendWorkerDone(exec: Exec, s: DoneSend, env: NodeJS.ProcessEnv): Promise<{ inv: OrcaInvocation; receipt: DoneReceipt }> {
  const inv = orcaInvocation(env, s.orca);
  const r = await exec(inv.cmd, workerDoneArgs(s), { env: inv.env, timeoutMs: doneSendTimeoutMs });
  return { inv, receipt: classifyReceipt(r) };
}

/** doneReply 是 task done 给 Agent 的最后一句话和退出码。 */
export function doneReply(inv: OrcaInvocation, r: DoneReceipt): { code: number; text: string } {
  switch (r.kind) {
    case 'reported':
      return r.action === 'settled'
        ? { code: 0, text: '这个任务之前已经报告过结束（Orca：settled），这次没有改变什么。现在结束这一轮，不要再运行任何命令。' }
        : { code: 0, text: `已向 Orca 报告结束（${r.action}）。现在结束这一轮，不要再运行任何命令。` };
    case 'identity':
      return {
        code: 1,
        text:
          `Orca 不接受这次报告的发送者（${r.code}：${r.message}）。kiln-orca task done 要在 Orca 启动你的会话中直接运行，` +
          '环境中的 ORCA_* 变量原样保留（不要用 env -i 或别的方式清理环境，不要在另开的终端中运行）。能修正就修正后重跑同一条命令；' +
          '做不到时结束这一轮。不要改用 orca 命令自己发送。',
      };
    case 'retry':
      return { code: 1, text: `Orca 没能确认这次报告（${r.code}：${r.message}）。稍后再运行同一条命令：同一个请求会重放，不会重复报告。` };
    case 'exec':
      return {
        code: 1,
        text:
          `这台机器上执行不了 Orca 的命令行（${inv.cmd}，来自 ${inv.source}：${r.message}）。这是节点的问题，不是你的问题，` +
          '已经报告给 Kiln。稍后再运行同一条命令；还是不行就结束这一轮。不要自己去找别的 orca 命令。',
      };
    case 'refused':
      return { code: 1, text: `Orca 拒绝了这次报告（${r.code}：${r.message}）。这不是你能修正的问题，已经报告给 Kiln；结束这一轮。` };
  }
}

/** receiptArgs 是交回连接器的回执摘要（doneReceiptCmd 的参数）。 */
export function receiptArgs(inv: OrcaInvocation, r: DoneReceipt): Record<string, string> {
  const base = { command: inv.cmd, source: inv.source, kind: r.kind };
  switch (r.kind) {
    case 'reported':
      return { ...base, action: r.action, messageId: r.messageId, requestId: r.requestId };
    case 'exec':
      return { ...base, message: r.message };
    default:
      return { ...base, code: r.code, message: r.message };
  }
}

function isObj(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}
function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}
