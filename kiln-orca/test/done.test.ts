import { writeFile } from 'node:fs/promises';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { Outcome } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { Attempt } from '../src/core/attempt.js';
import { classifyReceipt, type DoneSend, doneReply, orcaInvocation, parseDoneSend, receiptArgs, workerDoneArgs } from '../src/core/done.js';
import { ErrorClass } from '../src/core/errors.js';
import { type AgentCtx, commitFile, devTask, type Env, makeEnv, recordLog, records, reviewTask, waitFor } from './harness.js';

// kiln-orca task done 的连接器一侧（PLAN #33.5）：校验与 check-report / claim / deliver 相同，通过后在 journal 中记下固定的
// 请求 id、回复发送参数；连接器不发送 worker_done（由 task done 在 Agent 的进程中发送，见 cli.test.ts），结束时照常再校验。

let env: Env | undefined;
afterEach(async () => {
  await env?.cleanup();
  env = undefined;
});

async function run(e: Env, at: Attempt, resumed = false): Promise<void> {
  e.orca.tool = (_id, cmd, args) => at.tool(cmd, args);
  const ac = new AbortController();
  const pump = e.deps.mailbox.pump(ac.signal);
  try {
    await at.run(resumed);
  } finally {
    ac.abort();
    await pump;
  }
}

type Reply = { code: number; output: string };
const send = (r: Reply | undefined): DoneSend => parseDoneSend(r!.output);
const report = JSON.stringify({ summary: '没有问题', findings: [] });

describe('task done：审计', () => {
  it('报告没写、不合格时回复问题、不记请求；通过后回复发送参数，请求 id 先记进 journal，同样的参数重跑沿用', async () => {
    const outs: Reply[] = [];
    let journalDone: unknown;
    env = await makeEnv(async (ctx) => {
      outs.push(await ctx.tool('done', { outcome: 'succeeded' }));
      await writeFile(path.join(ctx.taskDir, 'report.json'), '{}');
      outs.push(await ctx.tool('done', { outcome: 'succeeded' }));
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      outs.push(await ctx.tool('done', { outcome: 'succeeded' }));
      journalDone = (await records(e.journal))[0]?.done;
      outs.push(await ctx.tool('done', { outcome: 'succeeded' }));
      ctx.done();
    });
    const e = env;
    e.kiln.validateFn = (req) => (Buffer.from(req.report ?? []).toString() === '{}' ? { valid: false, problems: ['缺少 summary'] } : { valid: true });
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);

    expect(outs[0]).toEqual({ code: 1, output: `还没有 ${path.join(at.taskDir, 'report.json')}` });
    expect(outs[1]).toEqual({ code: 1, output: '审计报告不合格，请修正后再运行：\n- 缺少 summary' });
    expect(outs[2]?.code).toBe(0);
    const s = send(outs[2]);
    expect(s).toEqual({
      taskId: at.binding.taskId,
      dispatchId: at.binding.dispatchId,
      outcome: 'succeeded',
      subject: `Kiln attempt ${at.id}：succeeded`,
      body: '审计报告已通过校验',
      retryRequest: expect.stringMatching(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/),
      orca: e.deps.orcaFallback,
    });
    expect(journalDone).toEqual({ retryRequest: s.retryRequest, outcome: 'succeeded', summary: '' });
    expect(send(outs[3])).toEqual(s);
    // 结束时照常校验并提交
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
  });

  it('--outcome failed：必须写原因；不校验报告，回复发送参数；Kiln 的结果仍按任务目录中的文件判定', async () => {
    const outs: Reply[] = [];
    env = await makeEnv(async (ctx) => {
      outs.push(await ctx.tool('done', { outcome: 'failed' }));
      outs.push(await ctx.tool('done', { outcome: 'failed', summary: '  环境坏了  ' }));
      outs.push(await ctx.tool('done', { outcome: 'bogus' }));
      ctx.done();
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    await run(e, at);
    expect(outs[0]).toEqual({ code: 2, output: '--outcome failed 时用 --summary 写明为什么做不完（一句话）' });
    expect(send(outs[1])).toMatchObject({ outcome: 'failed', body: '环境坏了' });
    expect(outs[2]).toEqual({ code: 2, output: '需要 --outcome succeeded 或 --outcome failed' });
    expect(e.kiln.completes[0]).toMatchObject({ outcome: Outcome.FAILED, errorClass: ErrorClass.report });
  });

  it('参数变了换新的请求 id；重启后同样的参数仍沿用 journal 中的那个', async () => {
    const outs: Reply[] = [];
    let resume = false;
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      outs.push(await ctx.tool('done', { outcome: 'failed', summary: '先报失败' }));
      outs.push(await ctx.tool('done', { outcome: 'succeeded' }));
      await waitFor(() => resume, 5_000, '恢复');
      outs.push(await ctx.tool('done', { outcome: 'succeeded' }));
      ctx.done();
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    const running = run(e, at);
    await waitFor(() => outs.length === 2, 5_000, '两次 done');
    at.shutdown();
    await running;
    const [rec] = await records(e.journal);
    resume = true;
    await run(e, Attempt.fromJournal(e.deps, rec!), true);

    expect(send(outs[0]).retryRequest).not.toBe(send(outs[1]).retryRequest);
    expect(rec?.done).toEqual({ retryRequest: send(outs[1]).retryRequest, outcome: 'succeeded', summary: '' });
    expect(send(outs[2])).toEqual(send(outs[1]));
  });

  it('并发的调用（例如子 Agent 与主 Agent 同时运行）串行处理，得到同一个请求 id', async () => {
    let outs: Reply[] = [];
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      outs = await Promise.all([ctx.tool('done', { outcome: 'succeeded' }), ctx.tool('done', { outcome: 'succeeded' })]);
      ctx.done();
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    await run(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(send(outs[0]).retryRequest).toBe(send(outs[1]).retryRequest);
  });

  it('Agent 结束之后（工具已经关闭）：done 回复已停止；回执照常记日志，执行不了 Orca 命令行记 error', async () => {
    let ctxRef: AgentCtx | undefined;
    env = await makeEnv(async (ctx) => {
      ctxRef = ctx;
      await writeFile(path.join(ctx.taskDir, 'report.json'), report);
      ctx.done();
    });
    const e = env;
    const log = recordLog(e);
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
    let afterEnd: Reply[] = [];
    const tool = at.tool.bind(at);
    await run(e, at);
    afterEnd = [
      await tool('done', { outcome: 'succeeded' }),
      await tool('done-receipt', { kind: 'reported', action: 'completed', messageId: 'msg_1', command: 'orca', source: 'ORCA_CLI_COMMAND' }),
      await tool('done-receipt', { kind: 'exec', command: '/opt/orca', source: 'config', message: 'spawn /opt/orca ENOENT' }),
      await tool('done-receipt', { kind: 'identity', code: 'no_active_sender_terminal', message: 'x', command: 'orca', source: 'config' }),
    ];
    expect(ctxRef).toBeDefined();
    expect(afterEnd[0]).toEqual({ code: 1, output: '这个任务已经停止或正在结束（取消、超时、失效或 Agent 已经结束），不要再继续。' });
    expect(afterEnd.slice(1)).toEqual([
      { code: 0, output: '' },
      { code: 0, output: '' },
      { code: 0, output: '' },
    ]);
    expect(log.find((l) => l.level === 'error')).toMatchObject({ msg: expect.stringContaining('执行不了 Orca 命令行'), fields: { attempt: String(at.id), command: '/opt/orca', message: 'spawn /opt/orca ENOENT' } });
    expect(log.find((l) => l.level === 'warn' && l.msg.includes('没能报告结束'))?.fields).toMatchObject({ code: 'no_active_sender_terminal' });
  });
});

describe('task done：开发', () => {
  it('选题：没有运行 claim 时不通过；claim 回复不领取之后通过', async () => {
    const outs: Reply[] = [];
    env = await makeEnv(async (ctx) => {
      outs.push(await ctx.tool('done', { outcome: 'succeeded' }));
      await writeFile(path.join(ctx.taskDir, 'selection.json'), JSON.stringify({ selected: [] }));
      outs.push(await ctx.tool('claim'));
      outs.push(await ctx.tool('done', { outcome: 'succeeded' }));
      ctx.done();
    });
    const e = env;
    e.kiln.offers = [{ candidates: [{ number: 12, title: '加功能' }] }];
    e.kiln.claimFn = () => ({ claimed: false, reason: '没有选中', message: '' });
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'select' }));
    await run(e, at);
    expect(outs[0]).toEqual({ code: 1, output: `还没有领取：先写选题报告并运行 kiln-orca task --attempt ${at.id} claim，按它的输出继续。` });
    expect(outs[1]).toEqual({ code: 0, output: `本次没有领取，现在运行 kiln-orca task --attempt ${at.id} done --outcome succeeded 结束这一轮。` });
    expect(send(outs[2])).toMatchObject({ outcome: 'succeeded', body: '已通过交付前的校验' });
  });

  it('实现：交付报告、git 的检查与 deliver 相同，不通过时不记请求；通过后回复发送参数', async () => {
    const outs: Reply[] = [];
    const recs: unknown[] = [];
    env = await makeEnv(async (ctx) => {
      await writeFile(path.join(ctx.taskDir, 'delivery.json'), JSON.stringify({ status: 'delivered', summary: '完成' }));
      // 没有 commit：交付不能通过
      outs.push(await ctx.tool('done', { outcome: 'succeeded' }));
      recs.push((await records(e.journal))[0]?.done);
      await commitFile(ctx.worktree, 'feature.go', 'package x\n', 'feat');
      outs.push(await ctx.tool('deliver'));
      outs.push(await ctx.tool('done', { outcome: 'succeeded', summary: '加了功能' }));
      ctx.done();
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    const at = new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' }));
    await run(e, at);
    expect(outs[0]?.code).toBe(1);
    expect(outs[0]?.output).toMatch(/^交付还不能通过，请修正后再运行/);
    expect(recs[0]).toBeUndefined();
    expect(outs[1]).toEqual({ code: 0, output: `通过：交付报告有效。现在运行 kiln-orca task --attempt ${at.id} done --outcome succeeded 结束这一轮（不要 push，连接器会推送）。` });
    expect(send(outs[2])).toMatchObject({ outcome: 'succeeded', body: '加了功能', dispatchId: at.binding.dispatchId });
    expect(e.kiln.completes[0]?.outcome).toBe(Outcome.SUCCEEDED);
  });
});

describe('task done：发送参数与回执', () => {
  const s: DoneSend = {
    taskId: 'task_1',
    dispatchId: 'ctx_1',
    outcome: 'succeeded',
    subject: 'Kiln attempt 5：succeeded',
    body: '完成',
    retryRequest: '0e3c3f5e-8a3b-4c7d-9f1e-2b6a1c0d9e8f',
    orca: { command: 'orca-ide', path: '/opt/orca/bin:/usr/bin' },
  };

  it('不带 --from，带固定的 --retry-request', () => {
    const args = workerDoneArgs(s);
    expect(args).not.toContain('--from');
    expect(args).toEqual([
      'orchestration', 'send', '--type', 'worker_done', '--task-id', 'task_1', '--dispatch-id', 'ctx_1', '--outcome', 'succeeded',
      '--subject', 'Kiln attempt 5：succeeded', '--body', '完成', '--retry-request', s.retryRequest, '--json', '--full',
    ]);
  });

  it('Orca 命令行：优先 ORCA_CLI_COMMAND；没有时用连接器的——带路径的直接执行，名字按连接器的 PATH 查找；其余环境不动', () => {
    expect(orcaInvocation({ ORCA_CLI_COMMAND: '/Applications/Orca.app/bin/orca', PATH: '/x' }, s.orca)).toEqual({ cmd: '/Applications/Orca.app/bin/orca', env: {}, source: 'ORCA_CLI_COMMAND' });
    expect(orcaInvocation({ ORCA_CLI_COMMAND: '', PATH: '/x' }, s.orca)).toEqual({ cmd: 'orca-ide', env: { PATH: '/opt/orca/bin:/usr/bin' }, source: 'config' });
    expect(orcaInvocation({ PATH: '/x' }, { command: '/opt/orca/bin/orca-ide', path: '/y' })).toEqual({ cmd: '/opt/orca/bin/orca-ide', env: {}, source: 'config' });
  });

  it('连接器的回复格式不对时抛出', () => {
    expect(parseDoneSend(JSON.stringify(s))).toEqual(s);
    for (const bad of [{ ...s, outcome: 'ok' }, { ...s, taskId: '' }, { ...s, retryRequest: 1 }, { ...s, orca: { command: '', path: '' } }, { ...s, orca: undefined }]) {
      expect(() => parseDoneSend(JSON.stringify(bad))).toThrow('连接器回复的发送参数格式不对');
    }
  });

  const ok = (action: string) =>
    JSON.stringify({ id: 'x', ok: true, result: { message: { id: 'msg_9' }, lifecycle: { action, outcome: 'succeeded' }, mutation: { requestId: 'req_9', replayed: false } } });
  const fail = (code: string) => JSON.stringify({ id: 'local', ok: false, error: { code, message: `${code} 的说明` } });
  const inv = { cmd: '/opt/orca', env: {}, source: 'config' as const };

  it.each([
    ['completed', 0, '已向 Orca 报告结束（completed）。现在结束这一轮，不要再运行任何命令。'],
    ['failed', 0, '已向 Orca 报告结束（failed）。现在结束这一轮，不要再运行任何命令。'],
    ['settled', 0, '这个任务之前已经报告过结束（Orca：settled），这次没有改变什么。现在结束这一轮，不要再运行任何命令。'],
  ] as const)('回执 %s：告诉 Agent 立即结束这一轮', (action, code, text) => {
    const r = classifyReceipt({ code: 0, stdout: `{"_keepalive":1}\n${ok(action)}\n`, stderr: '' });
    expect(r).toEqual({ kind: 'reported', action, messageId: 'msg_9', requestId: 'req_9' });
    expect(doneReply(inv, r)).toEqual({ code, text });
    expect(receiptArgs(inv, r)).toEqual({ command: '/opt/orca', source: 'config', kind: 'reported', action, messageId: 'msg_9', requestId: 'req_9' });
  });

  it.each(['no_active_sender_terminal', 'consumer_fenced', 'worker_identity_changed', 'sender_not_assignee'])('身份的拒绝 %s：说明原因，不让 Agent 改用 orca 命令', (code) => {
    const r = classifyReceipt({ code: 1, stdout: fail(code), stderr: '' });
    expect(r).toEqual({ kind: 'identity', code, message: `${code} 的说明` });
    const out = doneReply(inv, r);
    expect(out.code).toBe(1);
    expect(out.text).toContain(`Orca 不接受这次报告的发送者（${code}`);
    expect(out.text).toContain('不要改用 orca 命令自己发送');
  });

  it.each([
    ['operation_unknown', { code: 1, stdout: fail('operation_unknown'), stderr: '' }],
    ['runtime_unavailable', { code: 1, stdout: fail('runtime_unavailable'), stderr: '' }],
    ['no_receipt', { code: 124, stdout: '', stderr: '超时（180000 ms）' }],
    ['no_receipt', { code: 0, stdout: 'Orca screen reader 47.0', stderr: '' }],
    ['no_lifecycle', { code: 0, stdout: JSON.stringify({ ok: true, result: {} }), stderr: '' }],
  ] as const)('结果未知 %s：重跑同一条命令', (code, res) => {
    const r = classifyReceipt(res);
    expect(r).toMatchObject({ kind: 'retry', code });
    expect(doneReply(inv, r)).toEqual({ code: 1, text: expect.stringContaining('稍后再运行同一条命令：同一个请求会重放，不会重复报告') });
  });

  it('执行不了 Orca 命令行：节点的问题，写明命令和来源', () => {
    const r = classifyReceipt({ code: 127, stdout: '', stderr: 'Error: spawn /opt/orca ENOENT' });
    expect(r).toEqual({ kind: 'exec', message: 'Error: spawn /opt/orca ENOENT' });
    const out = doneReply(inv, r);
    expect(out.code).toBe(1);
    expect(out.text).toContain('这台机器上执行不了 Orca 的命令行（/opt/orca，来自 config：Error: spawn /opt/orca ENOENT）。这是节点的问题，不是你的问题');
    expect(receiptArgs(inv, r)).toEqual({ command: '/opt/orca', source: 'config', kind: 'exec', message: 'Error: spawn /opt/orca ENOENT' });
  });

  it('按子进程的结束方式区分：没能启动（end=notrun）才是执行不了，命令自己以 127 退出是结果未知（审计）', () => {
    expect(classifyReceipt({ code: 127, stdout: '', stderr: 'spawn 失败', end: 'notrun' })).toEqual({ kind: 'exec', message: 'spawn 失败' });
    expect(classifyReceipt({ code: 127, stdout: '', stderr: '命令自己退出 127', end: 'exit' })).toMatchObject({ kind: 'retry', code: 'no_receipt' });
    expect(classifyReceipt({ code: 127, stdout: 'not json', stderr: '', end: 'notrun' })).toMatchObject({ kind: 'retry', code: 'no_receipt' });
  });

  it('Orca 的其它拒绝：结束这一轮', () => {
    const r = classifyReceipt({ code: 1, stdout: fail('unknown_dispatch'), stderr: '' });
    expect(r).toEqual({ kind: 'refused', code: 'unknown_dispatch', message: 'unknown_dispatch 的说明' });
    expect(doneReply(inv, r).text).toContain('Orca 拒绝了这次报告（unknown_dispatch');
  });
});
