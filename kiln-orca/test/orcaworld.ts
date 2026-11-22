import { readFileSync } from 'node:fs';
import path from 'node:path';
import type { Exec, ExecOptions } from '../src/core/exec.js';
import { OrcaCli } from '../src/core/orca.js';
import { faults } from './harness.js';

// 用 Orca 1.4.258 的真实输出（test/fixtures/orca/）回答的 OrcaCli 替身：契约测试（orca-contract.test.ts）和需要真实
// worker-list 语义的回收测试（reclaim.test.ts）共用。

const dir = path.join(import.meta.dirname, 'fixtures', 'orca');
export type Json = Record<string, unknown>;

/** fx 读取夹具（orca 的完整 stdout）。 */
export function fx(name: string): string {
  return readFileSync(path.join(dir, `${name}.json`), 'utf8');
}
/** patched 返回改过 result 的夹具（模拟同一版本中其它状态的输出）。 */
export function patched(name: string, f: (result: Json) => void): string {
  const d = JSON.parse(fx(name)) as { result: Json };
  f(d.result);
  return JSON.stringify(d);
}

export interface Call {
  args: string[];
  opts: ExecOptions | undefined;
}
/** cli 返回一个 OrcaCli：每次调用按 respond 的结果回复（夹具名或完整 stdout），并记下调用。 */
export function cli(respond: (args: string[]) => string | { stdout: string; code: number }): { orca: OrcaCli; calls: Call[] } {
  const calls: Call[] = [];
  // 替身自己的错误（例如夹具不存在）记为测试失败：OrcaCli 会把 exec 的错误当成 exec_failed，期望出错的用例可能因此通过
  const exec: Exec = (_cmd, args, opts) =>
    faults().guard(() => {
      calls.push({ args: [...args], opts });
      const r = respond([...args]);
      const out = typeof r === 'string' ? { stdout: r.trimStart().startsWith('{') ? r : fx(r), code: 0 } : r;
      return { code: out.code, stdout: out.stdout, stderr: '' };
    });
  return { orca: new OrcaCli(exec, 'orca'), calls };
}

/** firstRunCursor 是夹具 run-list.json（第一页）的 nextCursor。 */
export const firstRunCursor = (JSON.parse(fx('run-list')) as { result: { nextCursor: string } }).result.nextCursor;

/** worker 返回一个终端状态为 terminalState 的 worker（取自夹具 worker-list-run.json 的第一个，改了 id、状态、存活和工作区）。 */
export function worker(dispatchId: string, terminalState: string, verdict: string, worktreePath: string): Json {
  const w = structuredClone((JSON.parse(fx('worker-list-run')) as { result: { workers: Json[] } }).result.workers[0]!);
  const id = `00000000-0000-4000-8000-000000000623::${worktreePath}`;
  Object.assign(w, { dispatchId, terminalState });
  Object.assign(w.resource as Json, { worktreeId: id, releaseState: terminalState });
  Object.assign(w.projection as Json, { dispatchId, id: dispatchId, workspace: { id, kind: 'folder_or_worktree' }, liveness: { verdict } });
  return w;
}

type Reply = string | { stdout: string; code: number };

/** rowsOf 读取一次采集的几页 worker-list 的 workers[]（新到旧），返回旧到新的 dispatch 行。 */
function rowsOf(...names: string[]): Json[] {
  return names.flatMap((n) => (JSON.parse(fx(n)) as { result: { workers: Json[] } }).result.workers).reverse();
}

/** setTerminalState 把 worker 行 w 的终端状态改成 state（模拟 Orca 中它的状态在两次查询之间变了）。 */
export function setTerminalState(w: Json, state: string | null): void {
  w.terminalState = state;
  if (w.resource && state) (w.resource as Json).releaseState = state;
}

/**
 * orcaWorld 按 Orca 1.4.258 的语义回答 run-list 和 worker-list（夹具 run-list.json、run-list-last.json 两页共四个 Run，其中
 * run_000000000600 是邮箱的 Run）：不带 --run 的 worker-list 按推断的调用方（邮箱）回答 worker-list-bound.json；带 --run
 * 时按 runs（每个 Run 的 dispatch 行，旧到新，下标 + 1 是行号）回答 worker-list-all-p1.json 的形状（scope.source=flag、
 * scope.run 是请求的 Run）。不带 --terminal-state 的翻页与 Orca 相同：第一页记下当时的行数作为快照，从新到旧，游标是
 * 上一页最后一行的行号，每页按读到时的状态返回快照中的行，page.total 是快照中的行数；带 --terminal-state 时按查询时的
 * 状态过滤（只模拟一页）。默认 run_000000000601 中是夹具 worker-list-all-p1..p3.json 的七个 dispatch（三个终端保留，
 * 都不在连接器的工作区中；四个没有终端、已经结束），run_000000000602 中是 worker-list-all-released.json 的十个（终端已经
 * 释放或者没有终端、已经结束）。beforeList 在回答每次 worker-list 之前调用（用来在两页之间改状态、加 dispatch）；
 * override 返回 undefined 以外的值时用它回答。calls 记下每次调用的参数。
 */
export function orcaWorld() {
  const runs = new Map<string, Json[]>([
    ['run_000000000601', rowsOf('worker-list-all-p1', 'worker-list-all-p2', 'worker-list-all-p3')],
    ['run_000000000602', rowsOf('worker-list-all-released')],
  ]);
  const calls: string[][] = [];
  const answer = (args: string[]): Reply => {
    const flag = (name: string) => (args.includes(name) ? (args[args.indexOf(name) + 1] ?? '') : '');
    if (args[1] === 'run-list') return flag('--cursor') ? 'run-list-last' : 'run-list';
    if (args[1] !== 'worker-list') throw new Error(`没有模拟 orca ${args.join(' ')}`);
    const run = flag('--run');
    if (!run) return 'worker-list-bound';
    w.beforeList?.(args);
    const rows = runs.get(run) ?? [];
    const limit = Number(flag('--limit') || '100');
    const state = flag('--terminal-state');
    let page: Json[];
    let total: number;
    let next: string | null = null;
    if (state) {
      page = rows.filter((x) => x.terminalState === state).reverse();
      if (page.length > limit) throw new Error('没有模拟带 --terminal-state 的翻页');
      total = page.length;
    } else {
      // 游标 c<快照的行数>-<上一页最后一行的行号>
      const [snap, after] = flag('--cursor') ? flag('--cursor').slice(1).split('-').map(Number) : [rows.length, rows.length + 1];
      const from = Math.min(snap!, after! - 1);
      page = rows.slice(Math.max(0, from - limit), from).reverse();
      total = snap!;
      const last = from - page.length + 1;
      if (last > 1) next = `c${snap}-${last}`;
    }
    const counts: Record<string, number> = {};
    for (const x of state ? page : rows.slice(0, total)) if (typeof x.terminalState === 'string') counts[x.terminalState] = (counts[x.terminalState] ?? 0) + 1;
    return patched('worker-list-all-p1', (r) => {
      r.workers = page;
      r.scope = { run, source: 'flag' };
      r.counts = counts;
      r.page = { limit, total, hasMore: next !== null, nextCursor: next };
      delete r.warnings;
    });
  };
  const w: {
    runs: Map<string, Json[]>;
    /** add 在 Run run 中加一个 dispatch（行号最大，最新）。 */
    add: (run: string, row: Json) => void;
    beforeList: ((args: string[]) => void) | undefined;
    override: ((args: string[]) => Reply | undefined) | undefined;
    calls: string[][];
    orca: OrcaCli;
  } = {
    runs,
    add: (run, row) => runs.set(run, [...(runs.get(run) ?? []), row]),
    beforeList: undefined,
    override: undefined,
    calls,
    orca: cli((args) => {
      calls.push(args);
      return w.override?.(args) ?? answer(args);
    }).orca,
  };
  return w;
}
