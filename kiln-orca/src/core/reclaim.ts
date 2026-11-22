import { existsSync } from 'node:fs';
import { realpath } from 'node:fs/promises';
import path from 'node:path';
import { errMessage } from './errors.js';
import { tail } from './exec.js';
import type { Git } from './git.js';
import { emptyBinding, type Journal, type JournalRecord, type ReclaimRecord } from './journal.js';
import type { Logger } from './log.js';
import type { KeyedMutex } from './mutex.js';
import { dirtyRefusal, type Orca, type TerminalInfo, type WorktreeInfo } from './orca.js';

// 工作区的回收（PLAN #33.10）：审计和开发相同，一次执行结束之后删除它在 Orca 中的工作区（kiln-a<attempt>-e<epoch>）、
// Agent 在任务目录中建的临时 worktree（tasks/<attempt>/ 之下，例如审计检出被审 commit 的 head）和 Orca 为工作区建的本地分支。
// 每一轮开发（选题、设计、实现、返修）都从远端新建工作区，不依赖上一轮的，交付和 checkpoint 在回收之前已经推到远端并
// 确认。没有推到远端的改动（推送失败、失效、吊销）不特殊处理：照样回收，删除前在日志中写明删掉了什么（reportUnpushed）。
// 什么时候回收由调用方决定（Attempt.cleanup、Node：结果已经了结、Agent 确认停止、终端确认释放之后），这里只定义回收什么、
// 怎么回收，以及升级前留下的工作区（历史残留）怎么认定。
//
// 只删能证明属于这次执行的东西：
// - Orca 中的工作区：在连接器自己的 clone（repos/ 之下、在 Orca 中注册的仓库）上，是 Git worktree（不是 folder workspace、
//   不是 clone 本身），备注（attemptComment）中的 attempt、epoch 与这次一致，名字是 kiln-a<attempt>-e<epoch>，路径与 Orca
//   回执中的一致（知道时），并且 git 中这个 clone 确实有这个 worktree。上一次删除只完成了一部分（git 和磁盘上已经没有、
//   Orca 还登记着）时，条件相同、只是不再要求 git 中有，照样经 Orca 删除并核对，删不掉就报失败、保留回收的责任。
// - 临时检出：git 中这个 clone 的、位于任务目录 tasks/<attempt>/ 之下的 worktree。
// 删除都经 Orca（worktree rm --worktree id:<repoId>::<路径>，Orca 同时去掉它的登记、git 的登记和目录），不用 rm -rf 代替；
// 不带 --run-hooks（不执行仓库的 archive hook）和 --delete-branch。先不带 --force；只有 Orca 因为工作区不干净而拒绝、
// 连接器自己也看到它不干净时（审计留下的构建产物、开发没有提交的改动）才带 --force 重试，并写进日志；其它失败不用 --force 绕过。
// 删除之前确认工作区中没有活着的终端（Agent 已经停止、终端已经释放；有人打开了终端时等它关闭），Orca 的全部 Run 中也
// 没有工作区在这里、终端还没有释放或者状态不明的 worker（structured 模式的 Agent 不在 terminal list 中，只能这样确认；
// 逐个 Run 显式查询，列表不完整时不删，见 orca.ts 的 unreleasedWorkers）；删除之后核对 Orca 的
// 列表、git worktree list 和磁盘上都没有了才算完成。分支由连接器在自己的 clone 中删除（只删名字是 kiln-a<attempt>-e<epoch>
// 或以 /kiln-a<attempt>-e<epoch> 结尾的：Orca 按工作区的名字建分支，可能带用户前缀），而且**先于**工作区：先让工作区脱离
// 分支、删除分支，最后删除工作区。这样任何一步失败时工作区都还在，下一次重试（journal 中的 reclaiming，或者历史残留的
// 周期清理）还能按备注找到这次执行，不会只剩一个找不回来的分支。例外是目录已经不在、只剩登记的工作区（任务目录的
// 12 小时清理删除了 head，或者有人手工删除了目录）：无法在其中脱离分支，先删除它的登记再删分支；分支这时删不掉的，
// journal 中的 reclaiming 重试时按名字删除（历史残留没有 journal 记录，回收之前先写一条，见 residueRecord）。
// 动 clone 的操作都在仓库锁内（与准备仓库的 fetch、worktree prune 和 Orca 的 worker-start 互斥）。
//
// Agent 的历史会话：回收关闭的是 Orca 中的活跃入口（工作区不再显示为进行中），不承诺物理删除 Agent 的会话记录；结论、
// finding、交付和日志在 Kiln 中，Orca 的历史记录是否保留由 Orca 决定。

/** TaskKind 是任务的类型（写进 Orca 工作区的备注：回收历史残留时据此确认是这个版本建的工作区）。 */
export type TaskKind = 'review' | 'develop';

/** AttemptMark 是连接器写在 Orca 工作区备注中的标记。kind 为 undefined：旧版本写的备注（没有任务类型）。 */
export interface AttemptMark {
  attemptId: string;
  epoch: string;
  taskId: string;
  kind: TaskKind | undefined;
}

/** attemptComment 是连接器在 Orca 中建的工作区的备注（worker-start --comment）。 */
export function attemptComment(m: { attemptId: bigint | string; epoch: bigint | string; taskId: bigint | string; kind: TaskKind }): string {
  return `kiln:attempt=${m.attemptId};epoch=${m.epoch};task=${m.taskId};kind=${m.kind}`;
}

const commentPattern = /^kiln:attempt=(0|[1-9][0-9]*);epoch=(0|[1-9][0-9]*);task=(0|[1-9][0-9]*)(?:;kind=(review|develop))?$/;

/** parseAttemptComment 解析连接器写的备注；不是（包括任务类型不认识）时返回 undefined。 */
export function parseAttemptComment(c: string): AttemptMark | undefined {
  const m = commentPattern.exec(c);
  if (!m) return undefined;
  return { attemptId: m[1] ?? '', epoch: m[2] ?? '', taskId: m[3] ?? '', kind: m[4] as TaskKind | undefined };
}

/** worktreeName 是连接器给 attempt 在 Orca 中建的工作区的名字。 */
export function worktreeName(attemptId: bigint | string, epoch: bigint | string): string {
  return `kiln-a${attemptId}-e${epoch}`;
}

/** kindText 是任务类型在日志中的名称。 */
export function kindText(kind: TaskKind): string {
  return kind === 'review' ? '审计' : '开发';
}

/** ReclaimTarget 是要回收的一次执行：worktreePath 是 Orca 回执中的工作区路径（不知道时为空，按备注和名字找）。 */
export interface ReclaimTarget {
  attemptId: string;
  epoch: string;
  kind: TaskKind;
  /** 开发：这次执行的起点、推到远端并确认过的 commit（不知道、没有推送时为空），见 reportUnpushed。 */
  start: string;
  pushed: string;
  worktreePath: string;
  /** Kiln 的仓库 id（仓库锁的 key）。 */
  repo: string;
  /** 连接器的 clone（Orca 中的仓库）。 */
  clone: string;
}

/** reclaimTarget 是 journal 记录 rec 要回收的执行（r 是它的 reclaim；没有任务类型的是之前的版本记下的审计）。 */
export function reclaimTarget(rec: JournalRecord, r: ReclaimRecord): ReclaimTarget {
  return {
    attemptId: rec.attemptId,
    epoch: rec.epoch,
    kind: r.kind ?? 'review',
    start: r.start ?? '',
    pushed: r.pushed ?? '',
    worktreePath: rec.binding.worktreePath,
    repo: r.repo,
    clone: r.clone,
  };
}

/**
 * residueRecord 是历史残留的回收记录：journal 中的 reclaiming（与任务结束后没有回收完的相同，不占容量），在回收开始之前
 * 写入。这样回收的责任在删除任何登记之前就已经持久化：只剩登记的工作区（目录已经不在）先删登记再删分支，分支这时删不掉
 * 的，节点按这条记录重试（按名字删除分支），不再依赖 Orca 中的登记找到这次执行。没有任务（reclaiming 不还原任务，spec
 * 为空），startedAt、reclaim.since 是写入的时间（超过保留时间放弃）。reclaimTarget(residueRecord(t)) 就是 t。
 */
export function residueRecord(t: ReclaimTarget, now: number): JournalRecord {
  return {
    attemptId: t.attemptId,
    epoch: t.epoch,
    spec: '',
    stage: 'reclaiming',
    binding: { ...emptyBinding(), worktreePath: t.worktreePath, worktreeName: worktreeName(t.attemptId, t.epoch) },
    startedAt: now,
    reclaim: { since: now, repo: t.repo, clone: t.clone, kind: t.kind, start: t.start, pushed: t.pushed },
  };
}

export interface ReclaimDeps {
  orca: Orca;
  git: Git;
  log: Logger;
  reposDir: string;
  tasksDir: string;
  repoLock: KeyedMutex;
  /** clone → Orca 的仓库 id（准备仓库时记下的；没有时按 repo list 查，不注册）。 */
  orcaRepoIds: Map<string, string>;
}

/**
 * ReclaimOutcome 是一次回收的结论：done 已经回收（包括早已回收的）；wait 现在还不能回收（工作区中还有活着的终端、仓库
 * 正忙），稍后再试；failed 回收失败（连不上 Orca、Orca 拒绝、删除后核对没有通过），稍后重试。
 */
export type ReclaimOutcome = { kind: 'done'; removed: string[] } | { kind: 'wait'; reason: string } | { kind: 'failed'; error: string };

/** reclaimFix 是回收放弃时的处理方法（日志中给人看）。 */
export const reclaimFix =
  '确认这次执行已经结束后：orca worktree list --repo path:<clone> --limit 10000 --json 找到它的工作区（备注 kiln:attempt=<attempt>;epoch=<epoch>;…）和 tasks/<attempt>/ 之下的检出，逐个 orca worktree rm --worktree "id:<id>" --json（不干净时加 --force），再在 clone 中 git branch -D <分支>';

/** canon 返回解析了符号链接的绝对路径（git 输出的路径是解析过的）；路径不存在时解析它存在的上级再接上其余部分。 */
export async function canon(p: string): Promise<string> {
  const abs = path.resolve(p);
  try {
    return await realpath(abs);
  } catch {
    const parent = path.dirname(abs);
    if (parent === abs) return abs;
    return path.join(await canon(parent), path.basename(abs));
  }
}

/**
 * blocksReclaim 判断工作区中的终端会不会挡住回收：进程还在，并且挂在界面的窗格上（有人在 Orca 界面上开着它）。这一步是为了
 * 不把工作区从正在看它的人眼前删掉。没有界面窗格（orphaned）的后台终端人在界面上看不到、也关不了，不挡回收，随 worktree rm
 * 一起关掉——否则一个后台 shell 会让已经结束的执行的工作区等到超过保留时间后放弃、留在磁盘上。这与 orca.ts 的
 * terminalUsable（邮箱终端能不能用，只看进程还在，不看窗格）是两件事，各自定义。
 */
export function blocksReclaim(t: TerminalInfo): boolean {
  return t.connected && !t.orphaned;
}

/** within 判断 child 是否在 dir 之下（不含 dir 本身；两者都是 canon 过的路径）。 */
export function within(dir: string, child: string): boolean {
  const rel = path.relative(dir, child);
  return rel !== '' && !rel.startsWith('..') && !path.isAbsolute(rel);
}

/** Victim 是要删除的一个工作区：id 是 Orca 的完整 id（<repoId>::<路径>），what 是日志中的名称。 */
interface Victim {
  id: string;
  path: string;
  canon: string;
  what: 'worktree' | 'head';
}

/**
 * reclaimWorkspace 回收一次执行（审计或开发）的工作区（见文件开头），不抛出。skipIfBusy：仓库锁被占用时不等，返回 wait
 * （节点的周期任务不能被准备仓库的长时间 fetch 卡住）。
 */
export async function reclaimWorkspace(d: ReclaimDeps, t: ReclaimTarget, opts: { skipIfBusy?: boolean } = {}): Promise<ReclaimOutcome> {
  if (opts.skipIfBusy && d.repoLock.busy(t.repo)) return { kind: 'wait', reason: '这个仓库正在准备或启动 Agent，下一轮再回收' };
  try {
    return await d.repoLock.run(t.repo, () => reclaimLocked(d, t));
  } catch (err) {
    return { kind: 'failed', error: errMessage(err) };
  }
}

async function orcaRepoId(d: ReclaimDeps, clone: string): Promise<string | undefined> {
  const known = d.orcaRepoIds.get(clone);
  if (known) return known;
  const want = await canon(clone);
  for (const r of await d.orca.repoList()) {
    if (r.kind === 'git' && (await canon(r.path)) === want) return r.id;
  }
  return undefined;
}

async function reclaimLocked(d: ReclaimDeps, t: ReclaimTarget): Promise<ReclaimOutcome> {
  const clone = await canon(t.clone);
  // 只回收连接器自己的 clone 上的工作区（共享 clone 本身、用户的项目、邮箱工作区都不在这里，也不会被选中）
  if (!within(await canon(d.reposDir), clone)) throw new Error(`${t.clone} 不在连接器的 repos/ 之下，不回收`);
  if (!existsSync(path.join(clone, '.git'))) {
    d.log.info('连接器的 clone 已经不在，没有要回收的工作区', { attempt: t.attemptId, clone: t.clone });
    return { kind: 'done', removed: [] };
  }
  const repoId = await orcaRepoId(d, t.clone);
  if (!repoId) throw new Error(`Orca 中没有注册连接器的 clone ${t.clone}，无法经 Orca 删除工作区`);
  const victims = await findVictims(d, t, clone, repoId);
  // 上一次删除只完成了一部分（例如权限不够：git 去掉了登记，目录还在）：登记已经没有了，不能再确认这个目录属于这次执行，
  // 不删、也不能当作完成，报失败（保留回收的责任，超过保留时间放弃并写明人工处理的方法）
  if (t.worktreePath && !victims.some((v) => v.what === 'worktree') && existsSync(t.worktreePath)) {
    throw new Error(`${t.worktreePath} 还在磁盘上，但 Orca 和 git 中都已经没有这个工作区的登记（上一次删除只完成了一部分，例如权限不够），不能再确认归属，不删除`);
  }
  // Agent 在任务目录中的检出也一样（审计按约定用 git worktree add 建在 tasks/<attempt>/head）：没有登记、目录还在时报失败，
  // 不当作完成；它在连接器的任务目录中，任务目录的 12 小时清理删除它之后重试就能完成
  const head = path.join(d.tasksDir, t.attemptId, 'head');
  const headCanon = await canon(head);
  if (!victims.some((v) => v.canon === headCanon) && existsSync(head)) {
    throw new Error(`${head} 还在磁盘上，但 git 中已经没有这个检出的登记（上一次删除只完成了一部分，例如权限不够），不能再确认归属，不删除；任务目录的 12 小时清理会删除它`);
  }

  // Agent 已经停止、终端已经释放：工作区中不应该还有活着的终端。有时（例如有人在 Orca 中打开了这个工作区的终端）不删，等它关闭
  // （只看界面上开着的，见 blocksReclaim）
  for (const v of victims) {
    const terms = (await d.orca.terminalList(`id:${v.id}`)).filter(blocksReclaim);
    if (terms.length > 0) {
      return { kind: 'wait', reason: `工作区 ${v.path} 中还有 ${terms.length} 个活着的终端（${terms.map((x) => x.title || x.handle).join('、')}），关闭之后再回收` };
    }
  }
  // terminal list 中没有 structured 模式的 Agent：再看 Orca 的全部 Run 中有没有终端还没有释放的 worker 在这些工作区中（Agent 可能还在运行）
  if (victims.length > 0) {
    const ours = new Set(victims.map((v) => v.canon));
    for (const w of await d.orca.unreleasedWorkers()) {
      const i = w.workspace.indexOf('::');
      if (i < 0 || !ours.has(await canon(w.workspace.slice(i + 2)))) continue;
      return {
        kind: 'wait',
        reason: `Orca 中还有工作区 ${w.workspace.slice(i + 2)} 上终端没有释放的 worker（dispatch ${w.dispatchId}，${w.terminalState || '还没有建终端'}，${w.liveness || '存活未知'}），Agent 可能还在运行，释放之后再回收`,
      };
    }
  }
  // 目录已经不在、登记还在的（任务目录的 12 小时清理删除了 head，或者有人手工删除了目录）：不能在其中让它脱离分支，也没有
  // 内容可删，先经 Orca 删除它的登记（Orca 1.4.258 实测：不带 --force 即可删除，git 的登记一起去掉），再删分支
  const missing = victims.filter((v) => !existsSync(v.path));
  const present = victims.filter((v) => existsSync(v.path));
  // 开发：删除之前写明这次回收删掉了哪些没有推送的改动（不因此阻止回收，PLAN #33.10）
  if (t.kind === 'develop') for (const v of present) if (v.what === 'worktree') await reportUnpushed(d, t, clone, v);
  const removed: string[] = [];
  for (const v of missing) {
    await removeVictim(d, t, v);
    removed.push(v.path);
  }
  removed.unshift(...(await deleteBranches(d, t, clone, present)));
  for (const v of present) {
    await removeVictim(d, t, v);
    removed.push(v.path);
  }
  await verifyGone(d, t, clone, repoId, victims);
  return { kind: 'done', removed };
}

/**
 * reportUnpushed 在删除开发的工作区之前把其中没有推到远端的内容写进日志：相对推送的 commit（没有推送时相对起点）多出的
 * commit 数、未提交的改动和未跟踪的文件。起点和推送的 commit 都不知道时（历史残留）写明不能确定。只写日志，不抛出、不阻止回收。
 */
async function reportUnpushed(d: ReclaimDeps, t: ReclaimTarget, clone: string, v: Victim): Promise<void> {
  const base = { attempt: t.attemptId, worktree: v.path, start: t.start, pushed: t.pushed };
  try {
    let branch = '';
    for (const w of await d.git.worktrees(clone)) if ((await canon(w.path)) === v.canon) branch = w.branch;
    const head = await d.git.run(v.path, 'rev-parse', 'HEAD');
    const dirty = (await d.git.dirty(v.path)).split('\n').filter((l) => l !== '');
    const from = t.pushed || t.start;
    const commits = from ? Number(await d.git.run(v.path, 'rev-list', '--count', `${from}..HEAD`)) : undefined;
    const fields = { ...base, branch, head, commits, dirtyFiles: dirty.length, dirty: tail(dirty.join('\n'), 500) };
    if (commits === undefined) {
      d.log.warn('回收开发的工作区：不知道起点和推送的 commit，不能确定其中有没有没推送的改动，照样删除', fields);
    } else if (commits > 0 || dirty.length > 0) {
      d.log.warn(`回收开发的工作区：删除了没有推送的改动（${t.pushed ? '推送的 commit' : '起点'}之后 ${commits} 个 commit，${dirty.length} 个未提交或未跟踪的文件）`, fields);
    } else {
      d.log.info('回收开发的工作区：其中的改动都已经推到远端', fields);
    }
  } catch (err) {
    d.log.warn('回收开发的工作区：查不出其中有没有没推送的改动，照样删除', { ...base, err: errMessage(err) });
  }
}

/**
 * deleteBranches 删除名字与这次执行绑定的本地分支（只在连接器自己的 clone 中），返回删除的 ref。分支被要删除的工作区检出时
 * 先让工作区脱离分支（停在同一个 commit，改动不动）；还被别的（不删除的）worktree 检出的不删。在删除工作区之前调用：
 * 分支删不掉时抛出，工作区还在，下一次重试能再找到它。
 */
async function deleteBranches(d: ReclaimDeps, t: ReclaimTarget, clone: string, victims: Victim[]): Promise<string[]> {
  const name = worktreeName(t.attemptId, t.epoch);
  const bound = (ref: string) => ref === `refs/heads/${name}` || ref.endsWith(`/${name}`);
  const ours = new Set(victims.map((v) => v.canon));
  for (const w of await d.git.worktrees(clone)) {
    if (w.branch !== '' && bound(w.branch) && ours.has(await canon(w.path))) await d.git.detach(w.path);
  }
  const checkedOut = new Set((await d.git.worktrees(clone)).map((w) => w.branch).filter((b) => b !== ''));
  const removed: string[] = [];
  for (const ref of await d.git.branches(clone)) {
    if (!bound(ref)) continue;
    if (checkedOut.has(ref)) {
      d.log.warn(`${kindText(t.kind)}工作区的分支还被别的 worktree 检出，不删除`, { attempt: t.attemptId, branch: ref, clone: t.clone });
      continue;
    }
    await d.git.deleteBranch(clone, ref);
    d.log.info(`已删除${kindText(t.kind)}工作区的本地分支`, { attempt: t.attemptId, branch: ref, clone: t.clone });
    removed.push(ref);
  }
  return removed;
}

/**
 * findVictims 找出这次执行要删除的工作区（见文件开头的条件）。备注是这次执行、但其它条件不符的工作区不删（记 warn）；
 * 符合条件的 Orca 工作区不止一个时不能确定是哪个，抛出。
 */
async function findVictims(d: ReclaimDeps, t: ReclaimTarget, clone: string, repoId: string): Promise<Victim[]> {
  const rows = await d.orca.worktreeList(repoId);
  const gitPaths = new Map<string, string>();
  for (const w of await d.git.worktrees(clone)) gitPaths.set(await canon(w.path), w.path);
  const name = worktreeName(t.attemptId, t.epoch);
  const recorded = t.worktreePath ? await canon(t.worktreePath) : '';
  const out: Victim[] = [];

  const mains: Victim[] = [];
  for (const r of rows) {
    const m = parseAttemptComment(r.comment);
    if (!m || m.attemptId !== t.attemptId || m.epoch !== t.epoch) continue;
    const c = await canon(r.path);
    const why = mismatch(r, repoId, name, c, recorded, clone, gitPaths);
    if (why) {
      d.log.warn('Orca 中这个工作区的备注是这次执行，但不能确认归属，不删除', { attempt: t.attemptId, worktree: r.path, id: r.id, reason: why });
      continue;
    }
    mains.push({ id: r.id, path: r.path, canon: c, what: 'worktree' });
  }
  if (mains.length > 1) throw new Error(`Orca 中有 ${mains.length} 个工作区符合这次执行（${mains.map((v) => v.path).join('、')}），不能确定删哪个`);
  out.push(...mains);

  // 任务目录之下的检出：git 的登记为准（Orca 可能不显示不是它建的 worktree），也包括 Orca 列表中还有、git 中已经没有的
  const taskDir = await canon(path.join(d.tasksDir, t.attemptId));
  const heads = new Map<string, Victim>();
  for (const [c, p] of gitPaths) {
    if (within(taskDir, c)) heads.set(c, { id: `${repoId}::${p}`, path: p, canon: c, what: 'head' });
  }
  for (const r of rows) {
    const c = await canon(r.path);
    if (!heads.has(c) && within(taskDir, c) && r.git && !r.isMainWorktree && r.repoId === repoId) heads.set(c, { id: r.id, path: r.path, canon: c, what: 'head' });
  }
  // 先删检出（它是 Agent 从工作区中检出的），再删工作区
  return [...heads.values(), ...out];
}

/** mismatch 返回 Orca 工作区 r 不能确认属于这次执行的原因；能确认时返回 undefined。 */
function mismatch(r: WorktreeInfo, repoId: string, name: string, c: string, recorded: string, clone: string, gitPaths: Map<string, string>): string | undefined {
  if (!r.git || r.isBare) return '不是 Git worktree';
  if (r.isMainWorktree || c === clone) return '是仓库本身（根工作区）';
  if (r.repoId !== repoId || r.id !== `${repoId}::${r.path}`) return '不在连接器的 clone 上';
  if (path.basename(r.path) !== name) return `名字不是 ${name}`;
  if (recorded && c !== recorded) return `路径与 Orca 回执中的不一致（${recorded}）`;
  if (!gitPaths.has(c)) {
    if (existsSync(r.path)) return '磁盘上有这个目录，但连接器的 clone 中没有这个 git worktree';
    if (!recorded) return '连接器的 clone 中没有这个 git worktree，也没有 Orca 回执中的路径可以核对';
    // git 和磁盘上都已经没有、Orca 还登记着（上一次删除只完成了一部分）：其余条件都对得上，仍是这次执行的，再经 Orca 删除
  }
  return undefined;
}

/** removeVictim 经 Orca 删除一个工作区：先不带 --force；只有 Orca 因为不干净而拒绝、连接器也看到它不干净时带 --force 重试。 */
async function removeVictim(d: ReclaimDeps, t: ReclaimTarget, v: Victim): Promise<void> {
  const fields = { attempt: t.attemptId, path: v.path };
  const what = v.what === 'worktree' ? `已删除${kindText(t.kind)}的 Orca 工作区` : `已删除${kindText(t.kind)}的临时检出`;
  let out: 'removed' | 'not_found';
  try {
    out = await d.orca.worktreeRemove(v.id, false);
  } catch (err) {
    if (!dirtyRefusal(err)) throw err;
    // 认定不干净只用连接器自己看到的：Orca 的文字只用来区分拒绝的原因
    const dirty = await d.git.dirty(v.path).catch(() => '');
    if (!dirty) throw err;
    d.log.info(`${kindText(t.kind)}工作区中有未提交的改动或未跟踪的文件（一次性的产物；开发没有推送的已在上面写明），带 --force 删除`, { ...fields, dirty: tail(dirty, 500) });
    out = await d.orca.worktreeRemove(v.id, true);
    d.log.info(`${what}（--force）`, { ...fields, orca: out });
    return;
  }
  d.log.info(what, { ...fields, orca: out });
}

/** verifyGone 核对删除的工作区在 Orca 的列表、git worktree list 和磁盘上都没有了；还有时抛出。 */
async function verifyGone(d: ReclaimDeps, t: ReclaimTarget, clone: string, repoId: string, victims: Victim[]): Promise<void> {
  if (victims.length === 0) return;
  const rows = new Set<string>();
  for (const r of await d.orca.worktreeList(repoId)) rows.add(await canon(r.path));
  const inGit = new Set<string>();
  for (const w of await d.git.worktrees(clone)) inGit.add(await canon(w.path));
  const left: string[] = [];
  for (const v of victims) {
    const where = [rows.has(v.canon) && 'Orca 的列表', inGit.has(v.canon) && 'git worktree list', existsSync(v.path) && '磁盘'].filter((x): x is string => !!x);
    if (where.length > 0) left.push(`${v.path}（${where.join('、')}）`);
  }
  if (left.length > 0) throw new Error(`删除后核对没有通过，还在：${left.join('；')}`);
  d.log.info(`已核对：${kindText(t.kind)}的工作区在 Orca、git 和磁盘上都没有了`, { attempt: t.attemptId, count: victims.length });
}

// ---- 历史残留 ----

/** residueQuietMs：历史残留的工作区至少这么久没有活动才回收（另外要求没有活着的终端、journal 中没有这个 attempt）。 */
export const residueQuietMs = 60 * 60_000;
/** maxResiduePerSweep：每一轮最多尝试回收这么多个历史残留（有界：不在一轮中做太多删除）。 */
export const maxResiduePerSweep = 10;

export interface SweepOptions {
  /** journal 中有记录的、正在跟踪的 attempt：都不碰（由正常的流程处理）。 */
  keep: ReadonlySet<string>;
  now: number;
  /** 已经记过日志的跳过（同一个原因每个进程只记一次）。 */
  logged: Set<string>;
  /**
   * 每个候选（Orca 工作区的 id）上一次尝试回收的时间，由调用方跨轮保留。每轮先尝试从没试过的，再按上一次尝试从早到晚：
   * 一直受阻（有人打开的终端、删除一直失败）的只拖延它自己，不会占满每一轮的名额、让后面的永远轮不到。
   */
  tried: Map<string, number>;
  /** 写、删历史残留的回收记录（residueRecord）的 journal。 */
  journal: Pick<Journal, 'save' | 'remove'>;
  /** track 接着按 reclaiming 重试没有回收完（failed）的历史残留（记录留在 journal 中）：节点的 trackCleanup。 */
  track(rec: JournalRecord): void;
}

/**
 * sweepResidue 有界地回收升级前留下的（或回收记录丢失的）已经结束的执行的工作区（审计和开发相同），证据齐全才删：
 * 在连接器自己的 clone 上、备注是带任务类型的 kiln:attempt=…;epoch=…;task=…;kind=review|develop、名字一致、journal 中
 * 没有这个 attempt（不在途）、至少 residueQuietMs 没有活动，回收时再确认没有活着的终端和没有释放的 worker（见
 * reclaimWorkspace）。任何一条不满足就跳过，原因写进日志（每个进程只记一次）；不按名字批量删除。更早的版本写的备注没有
 * 任务类型，不能确认是连接器这样建的工作区，一律跳过（人工清理见 DEPLOY）。不抛出。
 */
export async function sweepResidue(d: ReclaimDeps, o: SweepOptions): Promise<void> {
  const once = (key: string, msg: string, fields: Record<string, unknown>) => {
    if (o.logged.has(key)) return;
    o.logged.add(key);
    d.log.info(msg, fields);
  };
  let repos;
  try {
    repos = await d.orca.repoList();
  } catch (err) {
    d.log.warn('检查历史残留的工作区失败（列不出 Orca 的仓库），下一次周期清理时重试', { err: errMessage(err) });
    return;
  }
  const reposDir = await canon(d.reposDir);
  const candidates: { r: WorktreeInfo; t: ReclaimTarget }[] = [];
  for (const repo of repos) {
    if (repo.kind !== 'git') continue;
    const clone = await canon(repo.path);
    // 连接器的 clone 是 repos/<Kiln 仓库 id>/<名字>
    const rel = path.relative(reposDir, clone).split(path.sep);
    if (!within(reposDir, clone) || rel.length !== 2 || !rel[0] || !existsSync(path.join(clone, '.git'))) continue;
    let rows: WorktreeInfo[];
    try {
      rows = await d.orca.worktreeList(repo.id);
    } catch (err) {
      d.log.warn('检查历史残留的工作区失败（列不出 Orca 中的工作区），下一次周期清理时重试', { clone: repo.path, err: errMessage(err) });
      continue;
    }
    for (const r of rows) {
      const m = parseAttemptComment(r.comment);
      if (!m) continue;
      const fields = { worktree: r.path, comment: r.comment };
      if (m.kind === undefined) {
        once(`old:${r.id}`, '跳过旧版本建的工作区：备注中没有任务类型（人工清理见 DEPLOY）', fields);
        continue;
      }
      if (o.keep.has(m.attemptId)) continue;
      const why = !r.git || r.isMainWorktree || r.isBare ? '不是 Git worktree' : path.basename(r.path) !== worktreeName(m.attemptId, m.epoch) ? '名字与备注不一致' : '';
      if (why) {
        once(`bad:${r.id}`, `跳过历史残留的工作区：${why}`, fields);
        continue;
      }
      if (!(r.lastActivityAt > 0)) {
        once(`time:${r.id}`, '跳过历史残留的工作区：Orca 没有给出最后活动时间', fields);
        continue;
      }
      if (o.now - r.lastActivityAt < residueQuietMs) continue;
      candidates.push({ r, t: { attemptId: m.attemptId, epoch: m.epoch, kind: m.kind, start: '', pushed: '', worktreePath: r.path, repo: rel[0], clone: repo.path } });
    }
  }

  // 已经不是候选的（回收了、不再符合条件）不再记着
  const ids = new Set(candidates.map((c) => c.r.id));
  for (const id of o.tried.keys()) if (!ids.has(id)) o.tried.delete(id);
  const last = (id: string) => o.tried.get(id) ?? -Infinity;
  candidates.sort((a, b) => last(a.r.id) - last(b.r.id));
  for (const { r, t } of candidates.slice(0, maxResiduePerSweep)) {
    o.tried.set(r.id, o.now);
    // 先把回收的责任写进 journal（见 residueRecord）：写不进去时不回收（之后删了登记、分支却删不掉时就再也找不到它）
    const rec = residueRecord(t, o.now);
    try {
      await o.journal.save(rec);
    } catch (err) {
      d.log.warn('写不进 journal，不回收这个历史残留的工作区，下一次周期清理时重试', { attempt: t.attemptId, worktree: r.path, err: errMessage(err) });
      continue;
    }
    const out = await reclaimWorkspace(d, t, { skipIfBusy: true });
    // 回收完了、或者还不能回收（wait 都在删除任何东西之前）：不再需要这条记录，下一次周期清理照常从 Orca 的登记找到它
    const forget = () => o.journal.remove(t.attemptId).catch((err: unknown) => d.log.warn('删除 journal 失败', { attempt: t.attemptId, err: errMessage(err) }));
    switch (out.kind) {
      case 'done':
        await forget();
        o.tried.delete(r.id);
        d.log.info(`回收了已经结束的${kindText(t.kind)}留下的工作区（历史残留）`, { attempt: t.attemptId, worktree: r.path, removed: out.removed.length });
        break;
      case 'wait':
        await forget();
        once(`wait:${r.id}`, `历史残留的工作区暂不回收：${out.reason}`, { worktree: r.path, comment: r.comment, attempt: t.attemptId });
        break;
      case 'failed':
        // 可能已经删除了一部分（例如登记删了、分支没删掉）：记录留在 journal 中，由节点按 reclaiming 重试
        o.track(rec);
        d.log.warn('回收历史残留的工作区失败，按 journal 中的回收记录重试', { attempt: t.attemptId, worktree: r.path, err: out.error, fix: reclaimFix });
        break;
      default: {
        const _exhaustive: never = out;
        return _exhaustive;
      }
    }
  }
}
