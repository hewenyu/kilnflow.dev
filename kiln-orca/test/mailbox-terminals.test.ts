import { mkdir } from 'node:fs/promises';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { Git } from '../src/core/git.js';
import { type Logger, nopLogger } from '../src/core/log.js';
import { Mailbox, mailboxRepoDir, mailboxReport, mailboxTitle, maxSweepPerRound } from '../src/core/mailbox.js';
import { nodeExec } from '../src/core/exec.js';
import { OrcaError, type TerminalInfo } from '../src/core/orca.js';
import { canon } from '../src/core/reclaim.js';
import { type Env, fastMailbox, makeEnv, MemIdentity, waitFor } from './harness.js';

// 邮箱终端的存活、换新时关闭旧终端、清理多余的邮箱终端（PLAN #33.4"邮箱终端的存活"）。生产节点上（Orca 1.4.258）邮箱终端
// 没有界面窗格（orphaned）但进程还在，旧的存活判断把它当作不在，每分钟新建一个终端，留下了近 200 个。

let env: Env | undefined;
afterEach(async () => {
  await env?.cleanup();
  env = undefined;
});

interface Line {
  level: 'info' | 'warn' | 'error';
  msg: string;
  fields: Record<string, unknown>;
}

function memLog(): { log: Logger; lines: Line[] } {
  const lines: Line[] = [];
  const at = (level: Line['level']) => (msg: string, fields: Record<string, unknown> = {}) => void lines.push({ level, msg, fields });
  return { log: { info: at('info'), warn: at('warn'), error: at('error') }, lines };
}

const home = (e: Env) => path.join(e.root, 'home');

function box(e: Env, store: MemIdentity, log: Logger): Mailbox {
  return new Mailbox(e.orca, new Git(nodeExec), store, home(e), log, fastMailbox);
}

/** term 是放进 Orca 的一个终端（默认：连接器的邮箱仓库中、标题是邮箱、进程还在、有界面窗格）。 */
function term(e: Env, handle: string, o: Partial<TerminalInfo> = {}): TerminalInfo {
  return { handle, title: mailboxTitle, worktreePath: mailboxRepoDir(home(e)), connected: true, orphaned: false, exitCause: '', ...o };
}

/** closeAs 让终端的进程结束（Orca 中仍查得到，connected:false）。 */
function closeAs(e: Env, handle: string, exitCause = 'operator_close'): void {
  const t = e.orca.terminals.get(handle)!;
  t.connected = false;
  t.exitCause = exitCause;
}

describe('邮箱终端的存活', () => {
  it('没有界面窗格（orphaned）但进程还在：沿用，不新建、不接管、不关闭', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log } = memLog();
    const store = new MemIdentity();
    const m = box(e, store, log);
    const first = await m.ensure();
    e.orca.terminals.get(first.handle)!.orphaned = true;
    // 重启后（从 mailbox.json 读出）也一样
    for (const mb of [m, box(e, store, log)]) expect(await mb.ensure()).toEqual(first);
    expect(e.orca.terminalCreates).toEqual([first.handle]);
    expect(e.orca.runUses).toEqual([]);
    expect(e.orca.terminalCloses).toEqual([]);
  });

  it('生产节点的现场：邮箱循环在读、状态检测每分钟 ensure，终端没有界面窗格——不再每次新建终端，也没有 consumer_fenced 的 warn', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log, lines } = memLog();
    const m = box(e, new MemIdentity(), log);
    const first = await m.ensure();
    e.orca.terminals.get(first.handle)!.orphaned = true;
    const ac = new AbortController();
    const pump = m.pump(ac.signal);
    for (let i = 0; i < 10; i++) {
      await m.ensure();
      await new Promise((r) => setTimeout(r, 5));
    }
    await waitFor(() => e.orca.checks.length >= 3, 5_000, '继续读取');
    ac.abort();
    await pump;
    expect(e.orca.terminalCreates).toEqual([first.handle]);
    expect(e.orca.checks.every((c) => c.terminal === first.handle)).toBe(true);
    expect(lines.filter((l) => l.level !== 'info')).toEqual([]);
  });

  it('进程已经结束（Orca 仍查得到）：新建终端接管原来的 Run、保存之后关掉旧的', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log, lines } = memLog();
    const store = new MemIdentity();
    const m = box(e, store, log);
    const old = await m.ensure();
    closeAs(e, old.handle);
    // 关闭旧终端时，mailbox.json 中已经是新的身份
    let savedAtClose: string | undefined;
    const close = e.orca.terminalClose.bind(e.orca);
    e.orca.terminalClose = (h) => {
      savedAtClose = store.id?.handle;
      return close(h);
    };
    const now = await m.ensure();
    expect(now.handle).not.toBe(old.handle);
    expect(now.runId).toBe(old.runId);
    expect(e.orca.runUses).toEqual([[now.handle, old.runId]]);
    expect(e.orca.terminalCloses).toEqual([old.handle]);
    expect(savedAtClose).toBe(now.handle);
    expect(lines).toContainEqual(expect.objectContaining({ level: 'info', msg: '关闭了换下来的旧邮箱终端', fields: { handle: old.handle, exitCause: 'operator_close' } }));
    // 之后沿用新终端，不再关闭什么
    expect(await m.ensure()).toEqual(now);
    expect(e.orca.terminalCloses).toEqual([old.handle]);
  });

  it('句柄失效（Orca 中没有这个终端）：新建终端，没有要关闭的', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log } = memLog();
    const m = box(e, new MemIdentity(), log);
    const old = await m.ensure();
    e.orca.terminals.delete(old.handle);
    const now = await m.ensure();
    expect(now.handle).not.toBe(old.handle);
    expect(e.orca.terminalCloses).toEqual([]);
  });

  it('关不掉旧终端只记日志，邮箱照常就绪', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log, lines } = memLog();
    const m = box(e, new MemIdentity(), log);
    const old = await m.ensure();
    closeAs(e, old.handle, 'unknown');
    e.orca.terminalCloseFault = () => new OrcaError('exec_failed', '连不上 Orca');
    const now = await m.ensure();
    expect(now.handle).not.toBe(old.handle);
    expect(e.orca.terminalCloses).toEqual([old.handle]);
    expect(lines).toContainEqual(expect.objectContaining({ level: 'warn', msg: '关闭换下来的旧邮箱终端失败，不影响邮箱' }));
  });

  it('接管 Run 失败时不关旧终端；重试成功之后才关', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log } = memLog();
    const m = box(e, new MemIdentity(), log);
    const old = await m.ensure();
    closeAs(e, old.handle);
    e.orca.runUseFault = new OrcaError('exec_failed', '结果未知');
    await expect(m.ensure()).rejects.toThrow(/结果未知/);
    expect(e.orca.terminalCloses).toEqual([]);
    e.orca.runUseFault = undefined;
    // 关闭旧终端时 Run 已经绑定在新终端上
    let boundAtClose: string | undefined;
    const close = e.orca.terminalClose.bind(e.orca);
    e.orca.terminalClose = (h) => {
      boundAtClose = e.orca.runBindings.get(old.runId);
      return close(h);
    };
    const now = await m.ensure();
    expect(e.orca.terminalCreates).toEqual([old.handle, now.handle]);
    expect(e.orca.terminalCloses).toEqual([old.handle]);
    expect(boundAtClose).toBe(now.handle);
  });

  it.each([
    ['标题不是邮箱', { title: 'zsh' }],
    ['不在邮箱仓库中', { worktreePath: '/elsewhere/kiln-orca' }],
  ] as const)('换下来的旧终端%s：不能证明是连接器建的，不关闭', async (_what, patch) => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log, lines } = memLog();
    const m = box(e, new MemIdentity(), log);
    const old = await m.ensure();
    closeAs(e, old.handle);
    Object.assign(e.orca.terminals.get(old.handle)!, patch);
    const now = await m.ensure();
    expect(now.handle).not.toBe(old.handle);
    expect(e.orca.terminalCloses).toEqual([]);
    expect(lines).toContainEqual(expect.objectContaining({ level: 'warn', msg: '换下来的旧邮箱终端不在邮箱仓库中或者标题不是邮箱，不关闭' }));
  });

  it('查询失败：不重建、不关闭', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log } = memLog();
    const m = box(e, new MemIdentity(), log);
    const old = await m.ensure();
    m.runLost(old.runId);
    e.orca.terminalShowFault = new OrcaError('runtime_unavailable', '连不上 Orca');
    await expect(m.ensure()).rejects.toThrow(/连不上 Orca/);
    expect(e.orca.terminalCreates).toEqual([old.handle]);
    expect(e.orca.terminalCloses).toEqual([]);
  });
});

describe('换新终端时旧终端上的 check（consumer_fenced）', () => {
  it('正在等待的 check 被新终端接管 Run 结束：不记 warn、不退避，用新终端继续读取', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log, lines } = memLog();
    const m = box(e, new MemIdentity(), log);
    const old = await m.ensure();
    let release!: () => void;
    e.orca.checkHold = new Promise((r) => (release = r));
    const ac = new AbortController();
    const pump = m.pump(ac.signal);
    await waitFor(() => e.orca.checks.length === 1, 5_000, 'check 在等待');
    // 等待期间旧终端的进程结束，状态检测换了新终端
    closeAs(e, old.handle);
    const now = await m.ensure();
    e.orca.checkHold = undefined;
    release();
    e.orca.send({ type: 'worker_done', dispatchId: 'ctx_a', outcome: 'succeeded' });
    await waitFor(() => m.done('ctx_a').then(Boolean), 5_000, '新终端收到消息');
    ac.abort();
    await pump;
    expect(e.orca.checks[0]?.terminal).toBe(old.handle);
    expect(e.orca.checks.slice(1).every((c) => c.terminal === now.handle)).toBe(true);
    // 只接管过一次（没有因为这次 consumer_fenced 再查证 Run）
    expect(e.orca.runUses).toEqual([[now.handle, old.runId]]);
    expect(lines.filter((l) => l.level !== 'info')).toEqual([]);
    expect(lines).toContainEqual(expect.objectContaining({ msg: '邮箱已经换了终端，旧终端上的 check 被 Orca 结束（没有交出消息），用新终端继续读取' }));
  });

  it('当前终端的 check 被拒绝为 consumer_fenced（Run 被别的终端占用）：仍记 warn，并接管回来', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log, lines } = memLog();
    const m = box(e, new MemIdentity(), log);
    const { handle, runId } = await m.ensure();
    e.orca.runBindings.set(runId, 'term_other');
    e.orca.send({ type: 'worker_done', dispatchId: 'ctx_a', outcome: 'succeeded' });
    const ac = new AbortController();
    const pump = m.pump(ac.signal);
    await waitFor(() => m.done('ctx_a').then(Boolean), 5_000, '接管回来后收到');
    ac.abort();
    await pump;
    expect(e.orca.runUses).toEqual([[handle, runId]]);
    expect(lines).toContainEqual(expect.objectContaining({ level: 'warn', msg: '读取 Orca 邮箱失败', fields: expect.objectContaining({ err: expect.stringContaining('consumer_fenced') }) }));
  });
});

describe('清理多余的邮箱终端（sweep）', () => {
  it('只关邮箱仓库中标题是邮箱、不是当前句柄的终端；别的标题的、别的数据目录的不关，提示一次', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log, lines } = memLog();
    const m = box(e, new MemIdentity(), log);
    const cur = await m.ensure();
    e.orca.terminals.get(cur.handle)!.orphaned = true;
    const leaked = ['term_l1', 'term_l2', 'term_l3'];
    for (const h of leaked) e.orca.addTerminal(term(e, h, { orphaned: true }));
    e.orca.addTerminal(term(e, 'term_user', { title: 'zsh' }));
    const other = path.join(e.root, 'other-home', 'kiln-orca');
    await mkdir(other, { recursive: true });
    e.orca.addTerminal(term(e, 'term_other_node', { worktreePath: other }));
    // 已经结束的不在 terminal list 中
    e.orca.addTerminal(term(e, 'term_dead', { connected: false, exitCause: 'operator_close' }));

    expect(await m.sweep()).toEqual({ closed: 3, failed: 0, left: 0, foreign: 1 });
    expect(e.orca.terminalCloses).toEqual(leaked);
    expect(await e.orca.terminalAlive(cur.handle)).toBe(true);
    expect(await e.orca.terminalAlive('term_user')).toBe(true);
    expect(await e.orca.terminalAlive('term_other_node')).toBe(true);
    expect(lines.filter((l) => l.level === 'warn').map((l) => l.fields.terminals)).toEqual(['term_user（zsh）']);
    // 第二轮：没有多余的了；别的标题的终端不再重复提示
    expect(await m.sweep()).toEqual({ closed: 0, failed: 0, left: 0, foreign: 1 });
    expect(e.orca.terminalCloses).toEqual(leaked);
    expect(lines.filter((l) => l.level === 'warn')).toHaveLength(1);
  });

  it('每轮最多关闭 maxSweepPerRound 个，其余留到下一轮；关不掉的下一轮再试', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log } = memLog();
    const m = box(e, new MemIdentity(), log);
    await m.ensure();
    const n = maxSweepPerRound + 7;
    for (let i = 0; i < n; i++) e.orca.addTerminal(term(e, `term_l${i}`, { orphaned: true }));
    e.orca.terminalCloseFault = (h) => (h === 'term_l0' ? new OrcaError('exec_failed', '超时') : undefined);
    expect(await m.sweep()).toEqual({ closed: maxSweepPerRound - 1, failed: 1, left: 7, foreign: 0 });
    expect(e.orca.terminalCloses).toHaveLength(maxSweepPerRound);
    e.orca.terminalCloseFault = undefined;
    expect(await m.sweep()).toEqual({ closed: 8, failed: 0, left: 0, foreign: 0 });
    expect((await e.orca.terminalList(`path:${mailboxRepoDir(home(e))}`)).length).toBe(1);
  });

  it('Orca 没有报告路径的终端不能证明在邮箱仓库中（即使当前目录就是邮箱仓库），不关', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log } = memLog();
    const m = box(e, new MemIdentity(), log);
    await m.ensure();
    const list = e.orca.terminalList.bind(e.orca);
    // 按工作区列出时 Orca 会带上路径；这里模拟某个版本没有带
    e.orca.terminalList = async (w) => [...(await list(w)), { ...term(e, 'term_nopath'), worktreePath: '' }];
    const cwd = process.cwd();
    process.chdir(mailboxRepoDir(home(e)));
    try {
      expect(await m.sweep()).toMatchObject({ closed: 0, foreign: 1 });
    } finally {
      process.chdir(cwd);
    }
    expect(e.orca.terminalCloses).toEqual([]);
  });

  it('邮箱还没有就绪（没有当前句柄）时不清理', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log } = memLog();
    await mkdir(mailboxRepoDir(home(e)), { recursive: true });
    e.orca.addTerminal(term(e, 'term_l1'));
    const m = box(e, new MemIdentity(), log);
    expect(await m.sweep()).toEqual({ closed: 0, failed: 0, left: 0, foreign: 0 });
    expect(e.orca.terminalCloses).toEqual([]);
  });

  it('与 ensure 串行：ensure 刚建、还没有成为当前句柄的终端不会被当成多余的', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log } = memLog();
    const m = box(e, new MemIdentity(), log);
    const old = await m.ensure();
    closeAs(e, old.handle);
    // 清理列出终端时，ensure 已经建好了新终端（没有串行时清理拿的当前句柄还是旧的）
    let created!: () => void;
    const gate = new Promise<void>((r) => (created = r));
    const create = e.orca.terminalCreate.bind(e.orca);
    e.orca.terminalCreate = async (p, t) => {
      const h = await create(p, t);
      created();
      return h;
    };
    const list = e.orca.terminalList.bind(e.orca);
    e.orca.terminalList = async (w) => {
      await gate;
      return list(w);
    };
    const [now] = await Promise.all([m.ensure(), m.sweep()]);
    expect(now.handle).not.toBe(old.handle);
    expect(await e.orca.terminalAlive(now.handle)).toBe(true);
    expect(e.orca.terminalCloses).toEqual([old.handle]);
  });

  it('重试时新建的终端自己也不在了：清掉失效的 pendingHandle，重新建一个', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log } = memLog();
    const m = box(e, new MemIdentity(), log);
    const first = await m.ensure();
    closeAs(e, first.handle);
    // 第一次接管失败：新终端 B 记在 pendingHandle 中
    e.orca.runUseFault = new OrcaError('exec_failed', '结果未知');
    await expect(m.ensure()).rejects.toThrow(/结果未知/);
    const pending = e.orca.terminalCreates.at(-1)!;
    expect(pending).not.toBe(first.handle);
    // 重试之前 B 也消失了：不能一直沿用失效的句柄，应清掉并新建 C
    e.orca.terminals.delete(pending);
    e.orca.runUseFault = undefined;
    const now = await m.ensure();
    expect(now.handle).not.toBe(pending);
    expect(now.handle).not.toBe(first.handle);
    expect(e.orca.terminalCreates).toEqual([first.handle, pending, now.handle]);
    expect(await e.orca.terminalAlive(now.handle)).toBe(true);
    // 旧的（进程已经结束）在接管成功后关闭；失效的 B 已经不在，没有可关闭的
    expect(e.orca.terminalCloses).toEqual([first.handle]);
  });

  it('清理不会关闭还没有接管 Run 的 pendingHandle', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const { log } = memLog();
    const m = box(e, new MemIdentity(), log);
    const first = await m.ensure();
    closeAs(e, first.handle);
    // 第一次接管失败：新终端 B 是 pendingHandle，清理不能把它当成多余的
    e.orca.runUseFault = new OrcaError('exec_failed', '结果未知');
    await expect(m.ensure()).rejects.toThrow(/结果未知/);
    const pending = e.orca.terminalCreates.at(-1)!;
    expect(await m.sweep()).toEqual({ closed: 0, failed: 0, left: 0, foreign: 0 });
    expect(e.orca.terminalCloses).toEqual([]);
    // 之后重试沿用同一个终端
    e.orca.runUseFault = undefined;
    expect((await m.ensure()).handle).toBe(pending);
  });

  it('nextCheck 与重建终端并发：check 用当前身份并持久化，不丢失', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const store = new MemIdentity();
    const m = box(e, store, nopLogger);
    const first = await m.ensure();
    const realEnsure = m.ensure.bind(m);
    closeAs(e, first.handle);
    // 旧实现会在 serial 之外先 ensure 拿到旧身份，然后在重建之后用它生成 check、持久化却写的是新身份（check 丢失）。
    // 这里让（旧的）ensure 卡住到重建完成、返回旧身份；新实现直接在 serial 内 doEnsure，不会调用它。
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    (m as unknown as { ensure: () => ReturnType<Mailbox['ensure']> }).ensure = async () => {
      await gate;
      return first;
    };
    const nextCheck = (m as unknown as { nextCheck: () => Promise<{ requestId: string; terminal: string; run: string }> }).nextCheck.bind(m);
    const pendingCheck = nextCheck();
    const rebuilt = await realEnsure();
    expect(rebuilt.handle).not.toBe(first.handle);
    release();
    const check = await pendingCheck;
    expect(check.terminal).toBe(rebuilt.handle);
    expect(check.run).toBe(rebuilt.runId);
    expect(store.id?.check?.requestId).toBe(check.requestId);
    expect(store.id?.handle).toBe(rebuilt.handle);
  });
});

describe('doctor 中的邮箱终端', () => {
  it('还没有建立、能用（没有界面窗格）、进程已经结束、句柄失效；多余的和别的终端的数量', async () => {
    env = await makeEnv(() => Promise.resolve());
    const e = env;
    const dir = home(e);
    expect(await mailboxReport(e.orca, dir, '')).toEqual(['- Orca 邮箱终端：还没有建立（连接器第一次运行时建立）']);
    const { log } = memLog();
    const cur = await box(e, new MemIdentity(), log).ensure();
    expect(await mailboxReport(e.orca, dir, cur.handle)).toEqual([`✓ Orca 邮箱终端：${cur.handle}（进程在运行）`]);
    e.orca.terminals.get(cur.handle)!.orphaned = true;
    e.orca.addTerminal(term(e, 'term_l1', { orphaned: true }));
    e.orca.addTerminal(term(e, 'term_l2'));
    e.orca.addTerminal(term(e, 'term_user', { title: 'zsh' }));
    expect(await mailboxReport(e.orca, dir, cur.handle)).toEqual([
      `✓ Orca 邮箱终端：${cur.handle}（进程在运行；没有界面窗格，照样能用）`,
      `- 邮箱仓库中有 2 个多余的邮箱终端：连接器启动后和每小时关闭（每轮最多 ${maxSweepPerRound} 个）；也可以按 DEPLOY.md 手工关闭`,
      '- 邮箱仓库中有 1 个不能证明是连接器建的终端，连接器不关闭：term_user（zsh）；确认没有用之后 orca terminal close --terminal <句柄> --json',
    ]);
    closeAs(e, cur.handle);
    expect((await mailboxReport(e.orca, dir, cur.handle))[0]).toBe(`- Orca 邮箱终端：${cur.handle} 的进程已经结束（operator_close）：连接器运行时新建一个，并关掉这个`);
    e.orca.terminals.delete(cur.handle);
    expect((await mailboxReport(e.orca, dir, cur.handle))[0]).toBe(`- Orca 邮箱终端：${cur.handle} 在 Orca 中已经不在（句柄失效，例如 Orca 重启过）：连接器运行时新建一个`);
    e.orca.terminalShowFault = new OrcaError('runtime_unavailable', '连不上 Orca');
    expect(await mailboxReport(e.orca, dir, cur.handle)).toEqual([expect.stringMatching(/^- Orca 邮箱终端：查询失败（.*连不上 Orca/)]);
    // 路径按解析了符号链接的比较：canon 过的邮箱仓库与 Orca 报告的路径一致
    expect(await canon(mailboxRepoDir(dir))).toBe(await canon(e.orca.terminals.get('term_l1')!.worktreePath));
  });
});
