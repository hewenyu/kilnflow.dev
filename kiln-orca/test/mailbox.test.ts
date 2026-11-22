import { existsSync } from 'node:fs';
import { mkdir, readFile, readdir, rm, stat, writeFile } from 'node:fs/promises';
import { createRequire, syncBuiltinESMExports } from 'node:module';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { Git } from '../src/core/git.js';
import { type Logger, nopLogger } from '../src/core/log.js';
import { Mailbox, maxAbortLogBytes, maxAbortMessageBytes } from '../src/core/mailbox.js';
import { nodeExec } from '../src/core/exec.js';
import { type OrcaMessage, OrcaError } from '../src/core/orca.js';
import { Redactor } from '../src/core/redact.js';
import { type Env, fastMailbox, macrotask, makeEnv, MemIdentity, waitFor } from './harness.js';

// 邮箱在 Orca 1.4.258 的 check 语义下的可靠性（PLAN #33.4）：check 返回即已处理；同一个请求重放同一批。

type WriteFile = (typeof import('node:fs/promises'))['writeFile'];
const fsPromises = createRequire(import.meta.url)('node:fs/promises') as { writeFile: WriteFile };

/**
 * gateWrites 把匹配 pattern 的文件写入卡住（返回的 hit 在第一次匹配时成立，release 放行），其余写入照常进行。
 * src 中的 `import { writeFile } from 'node:fs/promises'` 是被外部化的内置模块，vi.mock 拦不住；改 CommonJS 导出之后用
 * syncBuiltinESMExports 同步到 ESM 的具名导出。只在这个测试里使用，restore 之后恢复。
 */
function gateWrites(pattern: RegExp): { hit: Promise<void>; release: () => void; restore: () => void } {
  const original = fsPromises.writeFile;
  let release = (): void => undefined;
  const blocked = new Promise<void>((r) => {
    release = r;
  });
  let reached = (): void => undefined;
  const hit = new Promise<void>((r) => {
    reached = r;
  });
  fsPromises.writeFile = (async (...args: Parameters<WriteFile>) => {
    if (pattern.test(String(args[0]))) {
      reached();
      await blocked;
    }
    return original(...args);
  }) as WriteFile;
  syncBuiltinESMExports();
  const restore = (): void => {
    release();
    fsPromises.writeFile = original;
    syncBuiltinESMExports();
  };
  return { hit, release, restore };
}

let env: Env | undefined;
afterEach(async () => {
  await env?.cleanup();
  env = undefined;
});

/** box 用 env 的 FakeOrca 和给定的身份存储建一个邮箱（模拟连接器的一次运行）。 */
function box(e: Env, store: MemIdentity, log: Logger = nopLogger, redactor: Redactor = new Redactor()): Mailbox {
  return new Mailbox(e.orca, new Git(nodeExec), store, path.join(e.root, 'home'), log, fastMailbox, redactor);
}

/** run 运行邮箱的 pump 直到 until 成立，然后停止（模拟进程退出）。 */
async function pumpUntil(m: Mailbox, until: () => boolean | Promise<boolean>, what: string): Promise<void> {
  const ac = new AbortController();
  const p = m.pump(ac.signal);
  try {
    await waitFor(until, 5_000, what);
  } finally {
    ac.abort();
    await p;
  }
}

const inbox = (e: Env, d: string) => path.join(e.root, 'home', 'inbox', `${d}.json`);
/** blockInbox 让 worker_done 落盘失败（inbox 是一个文件；不用权限，root 下同样有效）。 */
async function blockInbox(e: Env): Promise<void> {
  await mkdir(path.join(e.root, 'home'), { recursive: true });
  await writeFile(path.join(e.root, 'home', 'inbox'), '');
}
async function unblockInbox(e: Env): Promise<void> {
  await rm(path.join(e.root, 'home', 'inbox'));
}
const done = (dispatchId: string) => ({ type: 'worker_done', dispatchId, outcome: 'succeeded' });

describe('邮箱：返回即已处理的 check', () => {
  it('每次 check 之前先把请求（UUID 与全部参数）存进 state；处理完才换新的请求', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const store = new MemIdentity();
    const m = box(e, store);
    // 每个请求发出时，state 中已经是它
    const storedAtCheck: Array<string | undefined> = [];
    const check = e.orca.check.bind(e.orca);
    e.orca.check = (req, signal) => {
      storedAtCheck.push(store.id?.check?.requestId);
      return check(req, signal);
    };
    e.orca.send(done('ctx_a'));
    await pumpUntil(m, () => e.orca.checks.length >= 3, '三次 check');
    expect(storedAtCheck).toEqual(e.orca.checks.map((c) => c.requestId));
    expect(new Set(e.orca.checks.map((c) => c.requestId)).size).toBe(e.orca.checks.length);
    const { handle, runId } = await m.ensure();
    expect(e.orca.checks[0]).toMatchObject({ terminal: handle, run: runId, waitMs: fastMailbox.waitMs });
    expect(JSON.parse(await readFile(inbox(e, 'ctx_a'), 'utf8'))).toMatchObject({ dispatchId: 'ctx_a', outcome: 'succeeded' });
  });

  it('响应丢失后用同一个请求重放，取回同一批（Orca 中已经标记为已处理）', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    e.orca.send(done('ctx_a'));
    e.orca.checkFault = 'lost-after';
    await pumpUntil(m, () => readFile(inbox(e, 'ctx_a')).then(() => true, () => false), '落盘');
    expect(e.orca.checks[1]?.requestId).toBe(e.orca.checks[0]?.requestId);
    expect(e.orca.handled).toEqual([expect.stringMatching(/^msg_/)]);
  });

  it('崩溃（丢弃邮箱实例）后，重启的邮箱用 state 中的请求取回同一批', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const store = new MemIdentity();
    e.orca.send(done('ctx_a'));
    e.orca.send({ type: 'question', id: 'q1' });
    // 第一次运行：拿到这一批之后、处理之前进程退出（落盘失败、回复失败，然后停止）
    e.orca.replyFaults = 1_000;
    await blockInbox(e);
    const first = box(e, store);
    await pumpUntil(first, () => e.orca.handled.length === 2, '交出这一批');
    const req = store.id?.check;
    expect(req?.requestId).toBe(e.orca.checks[0]?.requestId);
    expect(e.orca.replies).toEqual([]);

    // 重启：新的邮箱实例，用 state 中的请求重放，处理这一批
    await unblockInbox(e);
    e.orca.replyFaults = 0;
    const checksBefore = e.orca.checks.length;
    const second = box(e, store);
    await pumpUntil(second, () => e.orca.replies.length === 1 && e.orca.checks.length > checksBefore + 1, '重放后处理');
    expect(e.orca.checks[checksBefore]).toEqual(req);
    expect(JSON.parse(await readFile(inbox(e, 'ctx_a'), 'utf8'))).toMatchObject({ dispatchId: 'ctx_a' });
    expect(e.orca.replies).toEqual([expect.stringMatching(/^q1:/)]);
    // Orca 中这两条只交出过一次
    expect(e.orca.handled).toHaveLength(2);
  });

  it('处理失败的消息留在内存中重试，同一批中其余的照常处理，处理完之前不读下一批', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    e.orca.send({ type: 'question', id: 'q1' });
    e.orca.send(done('ctx_a'));
    e.orca.replyFaults = 3;
    const checksAtReply: number[] = [];
    const reply = e.orca.reply.bind(e.orca);
    e.orca.reply = (from, id, body) => {
      checksAtReply.push(e.orca.checks.length);
      return reply(from, id, body);
    };
    const ac = new AbortController();
    const p = m.pump(ac.signal);
    // 回复还在失败时，同一批中的 worker_done 已经落盘
    await waitFor(() => readFile(inbox(e, 'ctx_a')).then(() => true, () => false), 5_000, '落盘');
    await waitFor(() => e.orca.replies.length === 1, 5_000, '重试后回复');
    ac.abort();
    await p;
    expect(checksAtReply).toHaveLength(4);
    expect(new Set(checksAtReply).size).toBe(1);
    expect(e.orca.handled).toHaveLength(2);
  });

  it('Orca 明确拒绝的回复（提问的 Agent 已经不在）不重试，不阻塞邮箱；结果未知的回复重试', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    let calls = 0;
    e.orca.reply = () => {
      calls++;
      return Promise.reject(new OrcaError('terminal_not_found', 'Terminal has no live pane'));
    };
    e.orca.send({ type: 'question', id: 'q1' });
    await pumpUntil(m, () => e.orca.checks.length >= 3, '继续读取');
    expect(calls).toBe(1);

    // Orca 不可达、结果未知：这一批留着重试，不读下一批
    const checksAtReply: number[] = [];
    e.orca.reply = () => {
      checksAtReply.push(e.orca.checks.length);
      return Promise.reject(new OrcaError(checksAtReply.length < 3 ? 'runtime_unavailable' : 'exec_failed', '连不上'));
    };
    e.orca.send({ type: 'escalation', id: 'e1' });
    await pumpUntil(m, () => checksAtReply.length >= 4, '重试回复');
    expect(new Set(checksAtReply).size).toBe(1);
  });

  it('Orca 拒绝了的 worker_done 不落盘', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    e.orca.send({ ...done('ctx_a'), rejected: 'sender_not_assignee' });
    e.orca.send(done('ctx_b'));
    await pumpUntil(m, () => readFile(inbox(e, 'ctx_b')).then(() => true, () => false), '落盘');
    expect(await m.done('ctx_a')).toBeUndefined();
  });

  it('等待被打断、Orca 明确拒绝了 check：换新的请求；结果未知：重放', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    e.orca.checkFault = 'interrupted';
    await pumpUntil(m, () => e.orca.checks.length >= 2, '换请求');
    expect(e.orca.checks[1]?.requestId).not.toBe(e.orca.checks[0]?.requestId);

    const n = e.orca.checks.length;
    e.orca.checkFault = 'refused';
    await pumpUntil(m, () => e.orca.checks.length >= n + 3, '换请求');
    const ids = e.orca.checks.slice(n).map((c) => c.requestId);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it('Run 被别的终端接管（consumer_fenced）：邮箱终端重新接管 Run 后继续读取', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    const { handle, runId } = await m.ensure();
    e.orca.runBindings.set(runId, 'term_other');
    e.orca.send(done('ctx_a'));
    await pumpUntil(m, () => readFile(inbox(e, 'ctx_a')).then(() => true, () => false), '落盘');
    expect(e.orca.runUses).toEqual([[handle, runId]]);
  });

  it('邮箱终端重建后接管原来的 Run；没处理完的请求仍按原来的终端重放', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const store = new MemIdentity();
    e.orca.send(done('ctx_a'));
    // 第一次运行：交出这一批后落盘失败，进程退出
    await blockInbox(e);
    const first = box(e, store);
    await pumpUntil(first, () => e.orca.handled.length === 1, '交出这一批');
    await unblockInbox(e);
    const old = store.id!;
    expect(old.check).toBeDefined();
    // 邮箱终端丢失，重启
    e.orca.terminals.delete(old.handle);
    const second = box(e, store);
    await pumpUntil(second, () => readFile(inbox(e, 'ctx_a')).then(() => true, () => false), '落盘');
    const now = await second.ensure();
    expect(now.handle).not.toBe(old.handle);
    expect(now.runId).toBe(old.runId);
    expect(e.orca.runUses).toEqual([[now.handle, old.runId]]);
    // 重放用原来的请求（原来的终端），之后的请求用新的终端
    expect(e.orca.checks[1]).toEqual(old.check);
    expect(e.orca.checks.at(-1)?.terminal).toBe(now.handle);
  });

  it('Orca 中已经没有邮箱的 Run：重建时新建一个', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const store = new MemIdentity();
    const old = await box(e, store).ensure();
    e.orca.terminals.delete(old.handle);
    e.orca.runBindings.delete(old.runId);
    const now = await box(e, store).ensure();
    expect(now.runId).not.toBe(old.runId);
    expect(e.orca.runBindings.get(now.runId)).toBe(now.handle);
  });
});

describe('保证邮箱可用（决策表）', () => {
  it('只删除 Run、保留邮箱终端（编排状态被重置）：check 被拒绝为 run_not_found 后在现有终端上新建 Run 并保存，之后能收消息', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const store = new MemIdentity();
    const m = box(e, store);
    const old = await m.ensure();
    e.orca.runBindings.delete(old.runId);
    expect(await e.orca.terminalAlive(old.handle)).toBe(true);
    e.orca.send(done('ctx_a'));
    await pumpUntil(m, () => readFile(inbox(e, 'ctx_a')).then(() => true, () => false), '新 Run 中的消息落盘');
    const now = await m.ensure();
    expect(now.handle).toBe(old.handle);
    expect(now.runId).not.toBe(old.runId);
    expect(store.id).toMatchObject({ handle: old.handle, runId: now.runId });
    expect(e.orca.runCreates).toEqual([
      [old.handle, old.runId],
      [old.handle, now.runId],
    ]);
    expect(e.orca.terminalCreates).toEqual([old.handle]);
    expect(e.orca.checks.at(-1)).toMatchObject({ terminal: old.handle, run: now.runId });
  });

  it('Run 换新时丢弃旧 Run 的没处理完的 check（Orca 已经不能重放它），之后的 check 用新的请求和新的 Run', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const store = new MemIdentity();
    e.orca.send(done('ctx_a'));
    // 第一次运行：交出这一批后落盘失败，进程退出，check 留在 store 中
    await blockInbox(e);
    await pumpUntil(box(e, store), () => e.orca.handled.length === 1, '交出这一批');
    await unblockInbox(e);
    const old = structuredClone(store.id!);
    expect(old.check?.run).toBe(old.runId);
    // 退出期间编排状态被重置：重启后重放被拒绝（run_not_found），在现有终端上换新 Run
    e.orca.resetOrchestration();
    const second = box(e, store);
    const n = e.orca.checks.length;
    e.orca.send(done('ctx_b'));
    await pumpUntil(second, () => readFile(inbox(e, 'ctx_b')).then(() => true, () => false), '新 Run 中的消息落盘');
    expect(e.orca.checks[n]).toEqual(old.check);
    const now = await second.ensure();
    expect(now.handle).toBe(old.handle);
    expect(now.runId).not.toBe(old.runId);
    const after = e.orca.checks.slice(n + 1);
    expect(after.length).toBeGreaterThan(0);
    expect(after.every((c) => c.run === now.runId && c.requestId !== old.check?.requestId)).toBe(true);
    // 旧 Run 中没有处理完的那一批收不到了
    expect(await second.done('ctx_a')).toBeUndefined();
  });

  it('启动 Agent 时发现 Run 没了（runLost）、邮箱还有没处理完的 check：换新 Run 时丢弃它，不再拿它去重放', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const store = new MemIdentity();
    const m = box(e, store);
    const old = await m.ensure();
    // 上一次 check 的结果未知（例如命令超时），请求留着等重放
    const pending = { requestId: 'pending-request', terminal: old.handle, run: old.runId, waitMs: fastMailbox.waitMs };
    store.id = { ...old, check: pending };
    const again = box(e, store);
    await again.ensure();
    e.orca.runBindings.delete(old.runId);
    again.runLost(old.runId);
    const now = await again.ensure();
    expect(now.runId).not.toBe(old.runId);
    expect(now.check).toBeUndefined();
    expect(store.id.check).toBeUndefined();
  });

  type Term = 'alive' | 'closed' | 'error' | 'none';
  type Run = 'bound' | 'other' | 'missing' | 'error';
  type Action = 'reuse' | 'run-use' | 'run-create' | 'new-terminal+run-use' | 'new-terminal+run-create' | 'throw';
  const table: Array<[Term, Run, Action]> = [
    ['alive', 'bound', 'run-use'],
    ['alive', 'other', 'run-use'],
    ['alive', 'missing', 'run-create'],
    ['alive', 'error', 'throw'],
    ['closed', 'bound', 'new-terminal+run-use'],
    ['closed', 'other', 'new-terminal+run-use'],
    ['closed', 'missing', 'new-terminal+run-create'],
    ['closed', 'error', 'throw'],
    ['error', 'bound', 'throw'],
    ['error', 'missing', 'throw'],
    ['none', 'missing', 'new-terminal+run-create'],
  ];

  it.each(table)('邮箱终端 %s、Run %s（Orca 拒绝过这个 Run）：%s', async (term, run, action) => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const store = new MemIdentity();
    const m = box(e, store);
    let old = { handle: '', runId: '' };
    if (term !== 'none') {
      old = await m.ensure();
      // Orca 拒绝过这个 Run（check 的 consumer_fenced、run_not_found 或者 worker-start 的 run_not_found）
      m.runLost(old.runId);
    }
    if (term === 'closed') e.orca.terminals.delete(old.handle);
    if (term === 'error') e.orca.terminalShowFault = new OrcaError('exec_failed', '连不上 Orca');
    if (run === 'other') e.orca.runBindings.set(old.runId, 'term_other');
    if (run === 'missing') e.orca.runBindings.delete(old.runId);
    if (run === 'error') e.orca.runUseFault = new OrcaError('exec_failed', '结果未知');
    const terminals = e.orca.terminalCreates.length;
    const runs = e.orca.runCreates.length;
    const uses = e.orca.runUses.length;

    if (action === 'throw') {
      await expect(m.ensure()).rejects.toThrow(/连不上 Orca|结果未知/);
      // 查询失败不重建终端；终端已经新建了的，重试时沿用
      expect(e.orca.terminalCreates.length - terminals).toBe(term === 'closed' ? 1 : 0);
      expect(e.orca.runCreates.length).toBe(runs);
      e.orca.terminalShowFault = undefined;
      e.orca.runUseFault = undefined;
      const now = await m.ensure();
      expect(e.orca.terminalCreates.length - terminals).toBe(term === 'closed' ? 1 : 0);
      expect(now.runId === old.runId).toBe(run !== 'missing');
      expect(e.orca.runBindings.get(now.runId)).toBe(now.handle);
      return;
    }
    const now = await m.ensure();
    const newTerminal = action.startsWith('new-terminal');
    expect(e.orca.terminalCreates.length - terminals).toBe(newTerminal ? 1 : 0);
    expect(now.handle === old.handle).toBe(!newTerminal);
    const created = action.endsWith('run-create');
    expect(e.orca.runCreates.length - runs).toBe(created ? 1 : 0);
    expect(now.runId === old.runId).toBe(!created);
    expect(e.orca.runUses.length - uses).toBe(term === 'none' ? 0 : 1);
    // Run 绑定在邮箱终端上，保存的是现在的身份
    expect(e.orca.runBindings.get(now.runId)).toBe(now.handle);
    expect(store.id).toMatchObject({ handle: now.handle, runId: now.runId });
    // 之后（没有再被拒绝）沿用，不再 run-use
    await m.ensure();
    expect(e.orca.runUses.length - uses).toBe(term === 'none' ? 0 : 1);
  });

  it('接管 Run 失败时没有当前身份、没有保存：重试沿用同一个新终端，成功之后才成为当前身份（审计）', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const store = new MemIdentity();
    store.id = { handle: 'term_gone', runId: 'run_old' };
    e.orca.runBindings.set('run_old', 'term_gone');
    e.orca.runUseFault = new OrcaError('exec_failed', '结果未知');
    const m = box(e, store);
    await expect(m.ensure()).rejects.toThrow('结果未知');
    // 别处看不到没有 Run 的句柄；保存的还是原来的身份
    expect(() => m.identity).toThrow('邮箱还没有就绪');
    expect(store.id).toEqual({ handle: 'term_gone', runId: 'run_old' });
    expect(e.orca.terminalCreates).toHaveLength(1);
    // 重试：沿用刚才建的那个终端，不会每轮新建一个
    e.orca.runUseFault = undefined;
    const id = await m.ensure();
    expect(e.orca.terminalCreates).toHaveLength(1);
    expect(id).toEqual({ handle: e.orca.terminalCreates[0], runId: 'run_old' });
    expect(m.identity).toEqual(id);
    expect(store.id).toEqual(id);
  });

  it('终端存活、没有被拒绝过：沿用，不查 Run（Run 不存在由之后的 check 发现）', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    const old = await m.ensure();
    e.orca.runBindings.delete(old.runId);
    expect(await m.ensure()).toEqual(old);
    expect(e.orca.runUses).toEqual([]);
  });

  it('runLost 只处理当前的 Run：已经换过的旧 Run 不再触发查证', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    const id = await m.ensure();
    m.runLost('run_old');
    await m.ensure();
    expect(e.orca.runUses).toEqual([]);
    m.runLost(id.runId);
    await m.ensure();
    expect(e.orca.runUses).toEqual([[id.handle, id.runId]]);
  });
});

describe('邮箱：Agent 运行中断（Orca 1.4.263）', () => {
  const home = (e: Env) => path.join(e.root, 'home');
  const inboxDir = (e: Env) => path.join(home(e), 'inbox');
  const abortPath = (e: Env, d: string) => path.join(inboxDir(e), `${d}.abort.json`);
  /** fatal 是一条致命的 escalation（Orca 报告 Agent 运行中断）；默认带 1.4.263 的 failure 字段。 */
  const fatal = (dispatchId: string, o: Partial<OrcaMessage> = {}): Partial<OrcaMessage> & { type: string } => ({
    type: 'escalation',
    dispatchId,
    subject: 'Agent hit an error: Safety check blocked this request.',
    body: '原始输出：Safety check blocked this request.',
    turnOutcome: 'failure',
    failureKind: 'safetyCheck',
    error: 'Safety check blocked this request. Please rephrase your prompt.',
    detail: 'Safety check: blocked by policy.',
    ...o,
  });

  it.each<[string, Partial<OrcaMessage>]>([
    ['turnOutcome 为 failure', { turnOutcome: 'failure', failureKind: 'safetyCheck', error: 'Safety check blocked this request. Please rephrase your prompt.', detail: 'Safety check: blocked by policy.' }],
    ['stopReason 为 needs-input', { turnOutcome: '', stopReason: 'needs-input', error: '', detail: '' }],
    ['stopReason 为 exited', { turnOutcome: '', stopReason: 'exited', error: '', detail: '' }],
    ['stopReason 为 lost', { turnOutcome: '', stopReason: 'lost', error: '', detail: '' }],
    ['turnOutcome 为 failure 且 stopReason 为 finished', { turnOutcome: 'failure', stopReason: 'finished', error: '一次性的安全检查拒绝了这次请求' }],
  ])('%s：零回复，写中止记录，等待它的 attempt 能读到', async (_name, fields) => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    e.orca.send({ ...fatal('ctx_fatal'), ...fields });
    await pumpUntil(m, () => existsSync(abortPath(e, 'ctx_fatal')), '写中止记录');
    expect(e.orca.replies).toEqual([]);
    const ev = await m.observe('ctx_fatal');
    expect(ev?.kind).toBe('abort');
    const abort = ev?.kind === 'abort' ? ev.abort : undefined;
    expect(abort).toMatchObject({ dispatchId: 'ctx_fatal', turnOutcome: fields.turnOutcome ?? '', stopReason: fields.stopReason ?? '' });
    // 原始原因保留在说明里：有 error 时用它，否则用 body
    expect(abort?.reason).toContain(fields.error || '原始输出');
    expect(await m.done('ctx_fatal')).toBeUndefined();
  });

  it('finished 和连接器不认识的 stopReason：只记日志、零回复、不中止，之后仍以 worker_done 结束', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    e.orca.send({ type: 'escalation', dispatchId: 'ctx_a', stopReason: 'finished' });
    e.orca.send({ type: 'escalation', dispatchId: 'ctx_b', stopReason: 'someFutureStop' });
    e.orca.send(done('ctx_a'));
    await pumpUntil(m, () => existsSync(path.join(inboxDir(e), 'ctx_a.json')), '落盘');
    expect(e.orca.replies).toEqual([]);
    for (const d of ['ctx_a', 'ctx_b']) expect(existsSync(abortPath(e, d))).toBe(false);
    expect((await m.observe('ctx_a'))?.kind).toBe('done');
  });

  it('普通提问和没有致命标记的 escalation：照常自动回复，不写中止记录（提问没有 dispatch 也能回复）', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    e.orca.send({ type: 'question', id: 'q1' });
    e.orca.send({ type: 'escalation', id: 'e1', dispatchId: 'ctx_a', subject: '需要决定' });
    await pumpUntil(m, () => e.orca.replies.length === 2, '回复');
    expect(e.orca.replies.map((r) => r.split(':')[0])).toEqual(['q1', 'e1']);
    expect(existsSync(abortPath(e, 'ctx_a'))).toBe(false);
  });

  it('Orca 拒绝了的消息（任何类型、任何形状的原因）：零回复、零落盘，不影响其它任务', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    e.orca.send({ type: 'worker_done', dispatchId: 'ctx_rej', rejected: 'worker_done references unknown task task_x' });
    e.orca.send({ ...fatal('ctx_rej2'), id: 'msg_rej2', rejected: 'sender_not_assignee' });
    e.orca.send({ type: 'question', id: 'q_rej', dispatchId: 'ctx_rej3', rejected: '被 Orca 拒绝' });
    e.orca.send(done('ctx_ok'));
    await pumpUntil(m, () => existsSync(path.join(inboxDir(e), 'ctx_ok.json')), '继续读取');
    expect(e.orca.replies).toEqual([]);
    for (const d of ['ctx_rej', 'ctx_rej2', 'ctx_rej3']) expect(existsSync(abortPath(e, d))).toBe(false);
    expect(existsSync(path.join(inboxDir(e), 'ctx_rej.json'))).toBe(false);
  });

  it('缺 dispatch id、带路径分隔符的致命 escalation 和 worker_done：不落盘、不回复、不影响邮箱', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    e.orca.send(fatal(''));
    e.orca.send(fatal('../x'));
    e.orca.send(fatal('a/b'));
    e.orca.send({ type: 'worker_done', dispatchId: '../done' });
    e.orca.send(done('ctx_ok'));
    await pumpUntil(m, () => existsSync(path.join(inboxDir(e), 'ctx_ok.json')), '继续读取');
    expect(e.orca.replies).toEqual([]);
    expect(existsSync(path.join(home(e), 'x.abort.json'))).toBe(false);
    expect(existsSync(path.join(inboxDir(e), 'a'))).toBe(false);
    // ../done 越界后 path.join 规整成 <数据目录>/done.json（不是 inbox 内的文件）
    expect(existsSync(path.join(home(e), 'done.json'))).toBe(false);
    expect(await readdir(inboxDir(e))).toEqual(['ctx_ok.json']);
  });

  it('第一次的中止记录保留：之后的致命事件不覆盖原因，也不刷新修改时间', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    e.orca.send(fatal('ctx_a', { error: '第一次的原因' }));
    await pumpUntil(m, () => existsSync(abortPath(e, 'ctx_a')), '写中止记录');
    const before = await stat(abortPath(e, 'ctx_a'));
    const n = e.orca.checks.length;
    e.orca.send(fatal('ctx_a', { error: '第二次的原因' }));
    await pumpUntil(m, () => e.orca.checks.length > n && existsSync(abortPath(e, 'ctx_a')), '处理下一批');
    const after = await stat(abortPath(e, 'ctx_a'));
    expect(after.mtimeMs).toBe(before.mtimeMs);
    expect((await m.abort('ctx_a'))?.reason).toContain('第一次的原因');
    expect((await m.abort('ctx_a'))?.reason).not.toContain('第二次的原因');
  });

  it.each([
    ['致命 escalation 在前', ['fatal', 'done']],
    ['worker_done 在前', ['done', 'fatal']],
  ])('同一批中%s：worker_done 胜出', async (_name, order) => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    for (const what of order) e.orca.send(what === 'fatal' ? fatal('ctx_a') : done('ctx_a'));
    await pumpUntil(m, () => existsSync(path.join(inboxDir(e), 'ctx_a.json')) && existsSync(abortPath(e, 'ctx_a')), '两份都落盘');
    expect((await m.observe('ctx_a'))?.kind).toBe('done');
    expect(e.orca.replies).toEqual([]);
  });

  it('同批的 worker_done 没有落盘时，它的致命 escalation 不抢先；worker_done 重试成功之后才写中止记录', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const lines: string[] = [];
    const log: Logger = { info: (m) => void lines.push(m), warn: (m) => void lines.push(m), error: (m) => void lines.push(m) };
    const m = box(e, new MemIdentity(), log);
    // 让 worker_done 的落盘一直失败（目标被一个非空目录占住），中止记录本身写得进去
    await mkdir(path.join(inboxDir(e), 'ctx_a.json', 'keep'), { recursive: true });
    e.orca.send(fatal('ctx_a'));
    e.orca.send(done('ctx_a'));
    const ac = new AbortController();
    const p = m.pump(ac.signal);
    try {
      await waitFor(() => lines.some((l) => l.includes('处理 Orca 邮箱的消息失败')), 5_000, 'worker_done 落盘失败、重试');
      // worker_done 还没有落盘：中止记录不能先出现
      expect(existsSync(abortPath(e, 'ctx_a'))).toBe(false);
      // 修好之后：先落盘 worker_done，再写中止记录
      await rm(path.join(inboxDir(e), 'ctx_a.json'), { recursive: true, force: true });
      await waitFor(() => existsSync(path.join(inboxDir(e), 'ctx_a.json')) && existsSync(abortPath(e, 'ctx_a')), 5_000, '两份都落盘');
    } finally {
      ac.abort();
      await p;
    }
    expect((await m.observe('ctx_a'))?.kind).toBe('done');
  });

  it('中止记录写不进去时留在批次中重试，同批其它 dispatch 照常处理（提问已回复）', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const lines: string[] = [];
    const log: Logger = { info: (m) => void lines.push(m), warn: (m) => void lines.push(m), error: (m) => void lines.push(m) };
    const m = box(e, new MemIdentity(), log);
    // 文件名超过文件系统的上限：这个 dispatch 的中止记录一直写不进去
    const tooLong = `ctx_${'a'.repeat(300)}`;
    e.orca.send(fatal(tooLong));
    e.orca.send({ type: 'question', id: 'q1', dispatchId: 'ctx_q' });
    await pumpUntil(m, () => lines.some((l) => l.includes('处理 Orca 邮箱的消息失败')), '落盘失败、重试');
    expect(e.orca.replies).toEqual([expect.stringMatching(/^q1:/)]);
    // 这一批没有处理完之前不读下一批
    expect(e.orca.checks).toHaveLength(1);
  });

  it('崩溃后重启：用 mailbox.json 中的原请求重放，取回同一批并写中止记录', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const store = new MemIdentity();
    await blockInbox(e);
    e.orca.send(fatal('ctx_a'));
    await pumpUntil(box(e, store), () => e.orca.handled.length === 1, '交出这一批');
    expect(store.id?.check).toBeDefined();
    await unblockInbox(e);
    await pumpUntil(box(e, store), () => existsSync(abortPath(e, 'ctx_a')), '重放后写中止记录');
    expect(e.orca.checks[1]?.requestId).toBe(e.orca.checks[0]?.requestId);
    expect(e.orca.handled).toHaveLength(1);
  });

  it('observe 区分"还没有"与"文件坏了"：不存在时是 undefined，损坏的 JSON 抛出', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    await mkdir(inboxDir(e), { recursive: true });
    expect(await m.observe('ctx_none')).toBeUndefined();
    expect(await m.done('ctx_none')).toBeUndefined();
    expect(await m.abort('ctx_none')).toBeUndefined();
    await writeFile(path.join(inboxDir(e), 'ctx_bad.json'), '{"type":');
    await expect(m.observe('ctx_bad')).rejects.toThrow();
    await expect(m.done('ctx_bad')).rejects.toThrow();
    await writeFile(abortPath(e, 'ctx_bad2'), '[1,');
    await expect(m.observe('ctx_bad2')).rejects.toThrow();
    await expect(m.abort('ctx_bad2')).rejects.toThrow();
  });

  it('中止记录落盘前已经脱敏、限长：磁盘上不留明文或半段凭据，新进程用空 Redactor 读取同样安全', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    // 登记的两个凭据分别横跨 reason（4 KiB）与 logDetail（16 KiB）的**真实**截断点：截断说明占 18 字节，保留前
    // max - 18 字节，凭据从截断点前 20 字节开始，一半落在保留的文本里、一半被截掉。先截断后脱敏时留下的那半段替换不掉。
    const mark = '…（已截断）';
    const markBytes = Buffer.byteLength(mark, 'utf8');
    const keptBytes = (max: number): number => max - markBytes;
    const redactor = new Redactor();
    const reasonSecret = 'ghs_TESTREASON0123456789abcdef';
    const logSecret = 'ghs_TESTLOGDETAIL0123456789abcdef';
    for (const s of [reasonSecret, logSecret]) redactor.add(s, Date.now() + 3_600_000);
    const m = box(e, new MemIdentity(), nopLogger, redactor);
    const prefix = 'Agent 运行中断（safetyCheck）：';
    const detail = '细节';
    const straddle = 20;
    const error = 'A'.repeat(keptBytes(maxAbortMessageBytes) - Buffer.byteLength(prefix, 'utf8') - straddle) + reasonSecret + 'B'.repeat(2_000);
    // logDetail：主因、详情之后追加输出，截断点落在 body 内（按原始文本计算——先截断的变异就是在这个长度上截断）
    const beforeBody = `${prefix}${error}；详情：${detail}；输出：`;
    const room = maxAbortLogBytes - Buffer.byteLength(beforeBody, 'utf8');
    const body = 'C'.repeat(room - markBytes - straddle) + logSecret + 'D'.repeat(2_000);
    e.orca.send({ ...fatal('ctx_a'), detail, error, body });
    await pumpUntil(m, () => existsSync(abortPath(e, 'ctx_a')), '写中止记录');
    const disk = await readFile(abortPath(e, 'ctx_a'), 'utf8');
    // 明文前缀与两段凭据（含被截断的半段）都不能落到磁盘上
    expect(disk).not.toContain('ghs_TEST');
    expect(disk).not.toContain(reasonSecret.slice(0, straddle));
    expect(disk).not.toContain(logSecret.slice(0, straddle));
    const rec = JSON.parse(disk) as { reason: string; logDetail: string };
    for (const [text, max] of [[rec.reason, maxAbortMessageBytes], [rec.logDetail, maxAbortLogBytes]] as const) {
      expect(Buffer.byteLength(text, 'utf8')).toBeLessThanOrEqual(max);
      expect(text.endsWith(mark)).toBe(true);
      expect(text).not.toContain('ghs_TEST');
      expect(text).not.toContain(reasonSecret.slice(0, straddle));
      expect(text).not.toContain(logSecret.slice(0, straddle));
    }
    // 重启后的连接器（内存中的 Redactor 是空的）读到的内容同样安全
    expect((await box(e, new MemIdentity()).abort('ctx_a'))?.reason).toBe(rec.reason);
  });

  it('forget 同时删除 worker_done 与中止记录；不能用作文件名的 dispatch id 什么也不做', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    await mkdir(inboxDir(e), { recursive: true });
    await writeFile(path.join(inboxDir(e), 'ctx_a.json'), JSON.stringify({ type: 'worker_done', dispatchId: 'ctx_a' }));
    await writeFile(abortPath(e, 'ctx_a'), JSON.stringify({ dispatchId: 'ctx_a' }));
    await m.forget('ctx_a');
    expect(existsSync(path.join(inboxDir(e), 'ctx_a.json'))).toBe(false);
    expect(existsSync(abortPath(e, 'ctx_a'))).toBe(false);
    // 已经删除的再删也成功
    await m.forget('ctx_a');
    await expect(m.forget('../x')).resolves.toBeUndefined();
  });

  it('落盘进行中的 worker_done 挡住并发的 observe 与 forget：读到 done，删除之后不留晚到的文件', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const m = box(e, new MemIdentity());
    // 先有旧的中止记录：observe 如果不与落盘串行，会在这里提前拿到 abort
    await mkdir(inboxDir(e), { recursive: true });
    await writeFile(abortPath(e, 'ctx_a'), JSON.stringify({ id: 'msg_old', dispatchId: 'ctx_a', turnOutcome: 'failure', stopReason: '', failureKind: '', reason: '旧的原因', logDetail: '旧的原因' }));
    // 只卡住这个 dispatch 的 worker_done 临时文件的写入（writeAtomic 先写 <目标>.<uuid>.tmp 再改名，其余写文件不受影响）
    const gate = gateWrites(/ctx_a\.json\.[^/]+\.tmp$/);
    const ac = new AbortController();
    const pump = m.pump(ac.signal);
    try {
      e.orca.send(done('ctx_a'));
      await gate.hit; // worker_done 的落盘已经开始（在 dispatch 的锁内）
      const observed = m.observe('ctx_a');
      const forgotten = m.forget('ctx_a');
      let settled = false;
      void observed.then(() => {
        settled = true;
      });
      await macrotask();
      // 落盘还没有完成：observe 不能提前返回旧的中止记录
      expect(settled).toBe(false);
      gate.release();
      // 落盘完成后才轮到 observe：看到的是 worker_done
      expect(await observed).toMatchObject({ kind: 'done', message: { dispatchId: 'ctx_a', outcome: 'succeeded' } });
      // forget 排在后面：两份文件都删除，不会留下晚到的 worker_done
      await forgotten;
      expect(existsSync(inbox(e, 'ctx_a'))).toBe(false);
      expect(existsSync(abortPath(e, 'ctx_a'))).toBe(false);
    } finally {
      gate.restore();
      ac.abort();
      await pump;
    }
  });
});
