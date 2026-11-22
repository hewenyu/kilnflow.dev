import { randomUUID } from 'node:crypto';
import { lstat, mkdir, readdir, readFile, rename, rm, stat, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { type DoneOutcome, doneOutcomes } from './done.js';
import { errMessage } from './errors.js';
import { KeyedMutex } from './mutex.js';
import type { TaskKind } from './reclaim.js';
import { type EnsuredTool, parseEnv, type ToolchainRecord } from './toolchain.js';

// 本机执行日志（PLAN #33.4 恢复）：每个未结束的 attempt 一个文件 journal/<attempt>.json。启动 Orca 中的 Agent 之前
// 先写入启动幂等键，连接器重启后据此查证、继续等待，绝不再启动一份。结束时先记下结论（verdict）再推送、checkpoint，
// 再记下要提交的请求（complete）再提交：重启后按记录重做推送、重发同一个请求，不重新校验。
//
// 写入是原子的（先写每次唯一的临时文件再改名），同一个 attempt 的写入、删除按调用顺序串行：调用时同步取快照，
// 所以后完成的写入一定是较晚调用时的状态，旧快照不会覆盖新状态，删除之后也不会被之前排队的写入写回来。
//
// 读取时逐个校验（parseRecord）：读不了、不是合法 JSON、字段缺失或类型不对、未知的 stage、格式版本不认识、attemptId
// 与文件名不一致的记录都作为"读不了的记录"返回（Journal.list），不静默丢弃——其中可能有还在运行的 Agent，节点据此
// 不领取新任务、不回收测试服务，等人处理（见 Node）。

export interface BindingRecord {
  runtimeId: string;
  startRequestId: string;
  runId: string;
  taskId: string;
  dispatchId: string;
  worktreePath: string;
  worktreeName: string;
}

export interface ClaimRecord {
  workItemId: string;
  branch: string;
  phase: string;
  issues: number[];
  /** 领取成功时给 Agent 的完整回复（Server 的说明，已替换占位符）：回复没有送到、Agent 重试 claim 时原样重放。 */
  reply?: string;
}

/** DoneRecord 是 kiln-orca task done 校验通过后记下的请求（PLAN #33.5）：同样的 outcome 和说明重跑时沿用 retryRequest。 */
export interface DoneRecord {
  retryRequest: string;
  outcome: DoneOutcome;
  summary: string;
}

/** ToolReply 是给 Agent 的工具调用的回复。 */
export interface ToolReply {
  code: number;
  output: string;
}

/**
 * VerdictRecord 是 Agent 结束（或被停止）后确定的结论，在推送、checkpoint 之前写入。恢复时按它重做这些副作用，
 * 不再校验（checkpoint 会改动 worktree，重新校验可能得出不同的结论）。
 */
export interface VerdictRecord {
  outcome: number;
  errorClass: string;
  message: string;
  /** 审计报告（base64）。 */
  reviewReport?: string;
  /** 交付报告（base64）。 */
  deliveryReport?: string;
  /** 交付通过校验：把 worktree 的 HEAD 推到 attempt 分支。 */
  push: boolean;
  /** 已经领取了工作：没有成功交付时把 worktree 中的改动保存为 checkpoint。 */
  checkpoint: boolean;
  /** Agent 还没有结束（取消、超时、断连、出错）：先停止它；确认停止后改为 false。 */
  stopAgent: boolean;
}

/**
 * ReleaseRecord 是还没有确认释放的 Orca 终端（PLAN #33.4"终端的释放"）：dispatch 的 Agent 已经结束或已确认停止，只剩
 * worker-release 回收它的终端（只关终端；工作区之后另行回收，见 ReclaimRecord）。释放的结果未知（release_unknown、连不上 Orca）时记下，由节点在恢复
 * 之后和周期清理中重试，确认释放（包括 Orca 回答已经释放、不存在）后才删除；不占容量。since 是第一次没能确认的时间
 * （超过保留时间后放弃）。
 */
export interface ReleaseRecord {
  dispatchId: string;
  since: number;
}

/**
 * ReclaimRecord 是任务结束后还要回收的工作区（PLAN #33.10"工作区的回收"，审计和开发相同）：这次执行在 Orca 中的工作区、
 * 任务目录中的临时检出（tasks/<attempt>/…）和 Orca 为工作区建的本地分支。结果已经了结（提交被 Server 确认，或者已经失效）
 * 之后记下；Agent 确认停止、终端确认释放之后才回收。没有完成时由节点每分钟重试（不占容量），超过保留时间放弃（记 error）。
 * since 是记下的时间；repo 是 Kiln 的仓库 id（仓库锁的 key），clone 是连接器的 clone 的路径（Orca 中的仓库）。
 * kind 是任务类型（没有时是审计：之前的版本只回收审计）。开发另记 start（这次执行的起点）和 pushed（推到远端、ls-remote
 * 确认过的 commit，没有推送时为空）：回收时据此在日志中写明删掉了哪些没有推送的改动（见 reclaim.ts 的 reportUnpushed）。
 */
export interface ReclaimRecord {
  since: number;
  repo: string;
  clone: string;
  kind?: TaskKind;
  start?: string;
  pushed?: string;
}

/** CompleteRecord 是要提交的 Complete 请求（推送、checkpoint 之后写入）。恢复时逐字重发，Server 按内容摘要返回原回执。 */
export interface CompleteRecord {
  outcome: number;
  errorClass: string;
  message: string;
  agentVersion: string;
  binding: BindingRecord;
  /** 审计报告（base64）。 */
  reviewReport?: string;
  /** 交付：报告（base64）、分支和 commit。 */
  delivery?: { report: string; branch: string; sha: string; checkpoint: boolean };
}

export interface JournalRecord {
  attemptId: string;
  epoch: string;
  /** AITask 的 protobuf 编码（base64），恢复时还原任务。 */
  spec: string;
  /**
   * finishing：Agent 已经结束或正在被停止，正在确定、提交结果（有 verdict、complete 时按它们继续）；
   * stopping：任务已经结束，但没能确认 Orca 中的 Agent 已经停止（继续占用容量，定期再停）。要停的是 binding.dispatchId；
   * 只有启动幂等键（启动请求可能已经发出、还不知道 dispatch）时先用 request-show 查证，查到 dispatch 再停（见 Node）。
   * releasing：任务已经结束，Agent 已经结束或已确认停止，只剩释放终端（release；不占容量，定期再释放），之后可能还要回收
   * 工作区（reclaim）。
   * reclaiming：任务已经结束，终端已经释放，只剩回收工作区（reclaim；不占容量，定期再回收）。
   */
  stage: 'prepare' | 'start' | 'running' | 'finishing' | 'stopping' | 'releasing' | 'reclaiming';
  binding: BindingRecord;
  /** 选题后领取到的工作项（恢复后交付校验要用）。 */
  claim?: ClaimRecord;
  /** 选题没有领取时的说明（结束时作为结果说明）。 */
  selectNote?: string;
  /** 领取冲突后重新选题的轮数（PLAN #31）：记在 journal 中，重启后不会从头计算。 */
  selectRounds?: number;
  /**
   * 最近一次没有领取成功的 claim 给 Agent 的回复：已经确定不领取（有 selectNote）时，或者领取冲突、已换新候选（退出码 3，
   * selection.json 已删除）而 Agent 还没有写新的选题时，重试 claim 原样重放它（回复没有送到时 Agent 会重试）。
   */
  claimReply?: ToolReply;
  /** 已经拉起的测试服务的 Compose 项目名和连接说明。 */
  services?: { project: string; desc: string[] };
  /** 准备好的受管理的工具链（PLAN #33.12）：task exec 用它的环境变量；回收工具链时不删其中的目录。 */
  toolchain?: ToolchainRecord;
  /** Orca 建 worktree 的起点（审计：base；开发：任务的 sha）。 */
  baseCommit?: string;
  /** 结束时确定的结论（推送、checkpoint 之前写入）。 */
  verdict?: VerdictRecord;
  /** 要提交的请求（推送、checkpoint 之后写入）。 */
  complete?: CompleteRecord;
  /** kiln-orca task done 校验通过后记下的发送参数（第一次发送之前写入，重跑沿用同一个请求 id）。 */
  done?: DoneRecord;
  startedAt: number;
  /** 进入 stopping 的时间（超过保留时间后不再占用容量）。 */
  stopSince?: number;
  /** Orca 明确表示启动请求没有启动任何东西（启动被拒绝、request-show 报失败）：有启动幂等键也不用再停。 */
  startFailed?: boolean;
  /** 还没有确认释放的终端（释放的结果未知时记下）；stage 为 releasing 时必须有。 */
  release?: ReleaseRecord;
  /** 还要回收的工作区；stage 为 reclaiming 时必须有。 */
  reclaim?: ReclaimRecord;
}

/** probeFile 是 Journal.probe 写的探测文件（不以 .json 结尾，list 不会读到）。 */
const probeFile = '.probe';
/** recordExt 是记录文件的扩展名：其它文件（临时文件、探测文件）都不是记录。 */
const recordExt = '.json';
/** journalFormat 是记录的格式版本（写在每个文件中；没有这个字段的记录按这个版本读取）。 */
export const journalFormat = 1;

/**
 * startUnknown：启动请求可能已经发出（journal 中有启动幂等键，见 Attempt.start 的不变量），但还不知道它启动的 dispatch，
 * 也不能断定没有启动——Orca 中可能有这个 attempt 的 Agent 在运行。
 */
export function startUnknown(r: JournalRecord): boolean {
  return !!r.binding.startRequestId && !r.binding.dispatchId && !r.startFailed;
}

export function emptyBinding(): BindingRecord {
  return { runtimeId: '', startRequestId: '', runId: '', taskId: '', dispatchId: '', worktreePath: '', worktreeName: '' };
}

/** Journal 读写 dir 中的执行记录（多个 attempt 共用；同一个 attempt 的写入、删除串行）。 */
export class Journal {
  private readonly order = new KeyedMutex();

  constructor(readonly dir: string) {}

  /** fileOf 是 attempt 的记录文件。 */
  fileOf(attemptId: string): string {
    return path.join(this.dir, `${attemptId}${recordExt}`);
  }

  /** save 写入 r 在调用这一刻的内容（同步取快照）；同一个 attempt 的写入按调用顺序完成。 */
  save(r: JournalRecord): Promise<void> {
    const data = JSON.stringify({ format: journalFormat, ...r });
    return this.order.run(r.attemptId, () => this.write(this.fileOf(r.attemptId), data));
  }

  remove(attemptId: string): Promise<void> {
    return this.order.run(attemptId, () => rm(this.fileOf(attemptId), { force: true }));
  }

  /** probe 用与 save 相同的方式写一个探测文件再删除：持久化不可用（磁盘满、只读、权限）时抛出。 */
  async probe(): Promise<void> {
    const f = path.join(this.dir, probeFile);
    await this.write(f, '{}');
    await rm(f, { force: true });
  }

  private async write(file: string, data: string): Promise<void> {
    await mkdir(this.dir, { recursive: true, mode: 0o700 });
    // 临时文件每次唯一（不以 .json 结尾，list 不会读到）：并发的写入不会移走、覆盖彼此的临时文件
    const tmp = `${file}.${randomUUID()}.tmp`;
    try {
      await writeFile(tmp, data, { mode: 0o600 });
      await rename(tmp, file);
    } catch (err) {
      await rm(tmp, { force: true }).catch(() => undefined);
      throw err;
    }
  }

  /** sweepTemp 删除崩溃时留下的临时文件。只在还没有任何写入时调用（节点启动恢复之前）：否则会删掉正在写的临时文件。 */
  async sweepTemp(): Promise<void> {
    for (const f of await readdir(this.dir).catch(() => [])) {
      if (f.endsWith('.tmp')) await rm(path.join(this.dir, f), { force: true });
    }
  }

  /**
   * list 返回全部未结束的记录和读不了的记录。只忽略确定不是记录的文件（不以 .json 结尾：写入用的临时文件、探测文件）
   * 和列出之后被删除的文件（记录已经结束）。目录不存在时为空；目录读不了时抛出（不能当成"没有在途任务"）。
   */
  async list(): Promise<JournalScan> {
    const out: JournalScan = { records: [], unreadable: [] };
    let files: string[];
    try {
      files = await readdir(this.dir);
    } catch (err) {
      if (isErrno(err, 'ENOENT')) return out;
      throw err;
    }
    for (const f of files.sort()) {
      if (!f.endsWith(recordExt)) continue;
      const file = path.join(this.dir, f);
      const attemptId = f.slice(0, -recordExt.length);
      let text: string;
      try {
        text = await readFile(file, 'utf8');
      } catch (err) {
        // 列出之后被删除（记录已经结束）；文件还在（例如指向不存在的目标的链接）时算读不了
        if (isErrno(err, 'ENOENT') && !(await lstat(file).then(() => true, () => false))) continue;
        out.unreadable.push({ attemptId, file, error: errMessage(err) });
        continue;
      }
      try {
        out.records.push(parseRecord(text, attemptId));
      } catch (err) {
        out.unreadable.push({ attemptId, file, error: errMessage(err) });
      }
    }
    return out;
  }
}

/** unreadableFix 是读不了的记录的处理方法（日志、上报、doctor 中给人看）。 */
export const unreadableFix =
  '其中可能有还在运行的 Agent：查看文件内容中的 dispatchId、startRequestId，在 Orca 中确认并停止对应的 Agent，然后删除这个文件（连接器不会自动删除）';

/** UnreadableRecord 是读不了的记录：attemptId 取自文件名（记录本身读不出来）。 */
export interface UnreadableRecord {
  attemptId: string;
  file: string;
  error: string;
}

/** JournalScan 是 Journal.list 的结果：可读的记录和读不了的记录。 */
export interface JournalScan {
  records: JournalRecord[];
  unreadable: UnreadableRecord[];
}

function isErrno(err: unknown, code: string): boolean {
  return err instanceof Error && 'code' in err && err.code === code;
}

// ---- 校验 ----

/** Problem 是不合格的位置和原因。 */
interface Problem {
  at: string;
  msg: string;
}
/** Rule 校验一个值：合格时返回 undefined。 */
type Rule = (v: unknown) => Problem | undefined;

const bad = (msg: string): Problem => ({ at: '', msg });
const str: Rule = (v) => (typeof v === 'string' ? undefined : bad('应为字符串'));
const num: Rule = (v) => (typeof v === 'number' && Number.isFinite(v) ? undefined : bad('应为数字'));
const bool: Rule = (v) => (typeof v === 'boolean' ? undefined : bad('应为布尔值'));
const optional =
  (r: Rule): Rule =>
  (v) =>
    v === undefined ? undefined : r(v);
/** count 是非负整数（轮数、次数）。 */
const count: Rule = (v) => (typeof v === 'number' && Number.isInteger(v) && v >= 0 ? undefined : bad('应为非负整数'));
const oneOf =
  (values: readonly string[]): Rule =>
  (v) =>
    typeof v === 'string' && values.includes(v) ? undefined : bad(`应为 ${values.join(' | ')} 之一，实际是 ${JSON.stringify(v)}`);
const listOf =
  (r: Rule): Rule =>
  (v) => {
    if (!Array.isArray(v)) return bad('应为数组');
    for (const [i, x] of v.entries()) {
      const p = r(x);
      if (p) return { at: `[${i}]${p.at && !p.at.startsWith('[') ? '.' : ''}${p.at}`, msg: p.msg };
    }
    return undefined;
  };
/** shape 校验对象的每个字段（类型参数保证 T 的每个字段都有规则；不认识的字段忽略）。 */
const shape =
  <T>(fields: { [K in keyof Required<T>]: Rule }): Rule =>
  (v) => {
    if (typeof v !== 'object' || v === null || Array.isArray(v)) return bad('应为对象');
    for (const [k, r] of Object.entries<Rule>(fields)) {
      const p = r(Reflect.get(v, k));
      if (p) return { at: p.at ? `${k}${p.at.startsWith('[') ? '' : '.'}${p.at}` : k, msg: p.msg };
    }
    return undefined;
  };

/** taskKinds 是 TaskKind 的全部取值。 */
const taskKinds = ['review', 'develop'] as const satisfies readonly TaskKind[];

/** stages 是 JournalRecord.stage 的全部取值。 */
const stages = ['prepare', 'start', 'running', 'finishing', 'stopping', 'releasing', 'reclaiming'] as const satisfies readonly JournalRecord['stage'][];

const bindingRule = shape<BindingRecord>({
  runtimeId: str,
  startRequestId: str,
  runId: str,
  taskId: str,
  dispatchId: str,
  worktreePath: str,
  worktreeName: str,
});
const replyRule = shape<ToolReply>({ code: num, output: str });
const envRule: Rule = (v) => {
  try {
    parseEnv(v);
    return undefined;
  } catch (err) {
    return bad(errMessage(err));
  }
};
const toolchainRule = shape<ToolchainRecord>({
  tools: listOf(shape<EnsuredTool>({ name: str, version: str, digest: str, dir: str, reused: bool })),
  env: envRule,
});

const recordRule = shape<JournalRecord>({
  done: optional(shape<DoneRecord>({ retryRequest: str, outcome: oneOf(doneOutcomes), summary: str })),
  attemptId: str,
  epoch: str,
  spec: str,
  stage: oneOf(stages),
  binding: bindingRule,
  claim: optional(shape<ClaimRecord>({ workItemId: str, branch: str, phase: str, issues: listOf(num), reply: optional(str) })),
  selectNote: optional(str),
  selectRounds: optional(count),
  claimReply: optional(replyRule),
  services: optional(shape<NonNullable<JournalRecord['services']>>({ project: str, desc: listOf(str) })),
  toolchain: optional(toolchainRule),
  baseCommit: optional(str),
  verdict: optional(
    shape<VerdictRecord>({
      outcome: num,
      errorClass: str,
      message: str,
      reviewReport: optional(str),
      deliveryReport: optional(str),
      push: bool,
      checkpoint: bool,
      stopAgent: bool,
    }),
  ),
  complete: optional(
    shape<CompleteRecord>({
      outcome: num,
      errorClass: str,
      message: str,
      agentVersion: str,
      binding: bindingRule,
      reviewReport: optional(str),
      delivery: optional(shape<NonNullable<CompleteRecord['delivery']>>({ report: str, branch: str, sha: str, checkpoint: bool })),
    }),
  ),
  startedAt: num,
  stopSince: optional(num),
  startFailed: optional(bool),
  release: optional(shape<ReleaseRecord>({ dispatchId: str, since: num })),
  reclaim: optional(shape<ReclaimRecord>({ since: num, repo: str, clone: str, kind: optional(oneOf(taskKinds)), start: optional(str), pushed: optional(str) })),
});

/** attemptIdPattern：attempt id 是规范的十进制数（同一个 attempt 不会有两个文件名）。 */
const attemptIdPattern = /^(0|[1-9][0-9]*)$/;

/**
 * parseRecord 解析、校验文件名为 <attemptId>.json 的记录内容；不合格时抛出（说明哪里不对）。只校验与文件名、格式版本、
 * 字段的类型和取值，以及恢复依赖的不变量（有 complete 就有 verdict；releasing 有 release，且 dispatch 不为空；reclaiming 有
 * reclaim、没有 release）；spec 能否还原为任务由 Attempt.fromJournal 检查。
 */
export function parseRecord(text: string, attemptId: string): JournalRecord {
  let v: unknown;
  try {
    v = JSON.parse(text);
  } catch (err) {
    throw new Error(`不是合法的 JSON（${errMessage(err)}）`, { cause: err });
  }
  if (typeof v === 'object' && v !== null && 'format' in v && v.format !== journalFormat) {
    throw new Error(`格式版本 ${JSON.stringify(v.format)} 不是这个版本的连接器能识别的（${journalFormat}），可能由更新版本的连接器写入`);
  }
  const p = recordRule(v);
  if (p) throw new Error(`字段 ${p.at || '（整条记录）'}：${p.msg}`);
  // 上面已经逐个字段校验过
  const { format: _format, ...rec } = v as JournalRecord & { format?: number };
  if (!attemptIdPattern.test(attemptId)) throw new Error(`文件名中的 attempt id ${JSON.stringify(attemptId)} 不是规范的十进制数`);
  if (rec.attemptId !== attemptId) throw new Error(`记录中的 attemptId ${JSON.stringify(rec.attemptId)} 与文件名不一致`);
  if (rec.complete && !rec.verdict) throw new Error('有要提交的请求（complete）但没有结论（verdict）');
  if (rec.stage === 'releasing' && !rec.release) throw new Error('阶段是 releasing 但没有要释放的终端（release）');
  if (rec.release && !rec.release.dispatchId) throw new Error('字段 release.dispatchId：不能为空');
  if (rec.stage === 'reclaiming' && !rec.reclaim) throw new Error('阶段是 reclaiming 但没有要回收的工作区（reclaim）');
  if (rec.stage === 'reclaiming' && rec.release) throw new Error('阶段是 reclaiming 但还有没有确认释放的终端（release）');
  if (rec.reclaim && !rec.reclaim.clone) throw new Error('字段 reclaim.clone：不能为空');
  return rec;
}

/** removeOlderThan 删除 dir 中修改时间早于 cutoff 的条目（keep 中的名字除外）。 */
export async function removeOlderThan(dir: string, cutoff: number, keep: Set<string> = new Set()): Promise<string[]> {
  const removed: string[] = [];
  for (const name of await readdir(dir).catch(() => [])) {
    if (keep.has(name)) continue;
    const p = path.join(dir, name);
    const st = await stat(p).catch(() => undefined);
    if (st && st.mtimeMs < cutoff) {
      await rm(p, { recursive: true, force: true });
      removed.push(name);
    }
  }
  return removed;
}
