import { mkdir, open, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { createServer, type Server, type ServerResponse } from 'node:http';
import path from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Outcome } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { Attempt, maxReportBytes, readLimited } from '../src/core/attempt.js';
import { type Exec, ExecError, nodeExec, outputLimitCode } from '../src/core/exec.js';
import { ErrorClass } from '../src/core/errors.js';
import { Git } from '../src/core/git.js';
import { isDefinite, OrcaCli, OrcaError } from '../src/core/orca.js';
import { dailyLog, pruneLogs, serviceLogMaxBytes, serviceLogs } from '../src/host/logger.js';
import { callTool, maxToolReplyBytes, maxToolRequestBytes, serveTools, ToolReplyTooLargeError } from '../src/host/socket.js';
import { type Env, devTask, git, makeEnv, reviewTask, shortTmp } from './harness.js';

// 外部（Agent、Orca 等）能控制大小的输入都有上限（PLAN #33）：超过上限时明确报错，不按输入的实际大小分配内存。

/** bigFile 是测试用的大文件大小：远大于各个上限（稀疏文件，不占磁盘）。 */
const bigFile = 64 << 20;

let dirs: string[] = [];
afterEach(async () => {
  for (const d of dirs) await rm(d, { recursive: true, force: true });
  dirs = [];
});
async function tempDir(): Promise<string> {
  const d = await shortTmp('ko-limits-');
  dirs.push(d);
  return d;
}

/** sparse 在 p 建一个 size 字节的稀疏文件，末尾是 tail。 */
async function sparse(p: string, size: number, tail = ''): Promise<void> {
  const fh = await open(p, 'w');
  try {
    await fh.truncate(size);
    if (tail) await fh.write(tail, size - Buffer.byteLength(tail));
  } finally {
    await fh.close();
  }
}

type ReadResult = { bytesRead: number };

/**
 * bytesRead 统计 f 执行期间经文件句柄（FileHandle.read）读到的字节数。用读取的字节数而不是进程内存判断有界读取：
 * 内存统计受 GC 时机影响，在不同的 Node 版本和平台上不稳定。不经文件句柄、一次读完整个文件（readFile）时为 0。
 */
async function bytesRead<T>(dir: string, f: () => Promise<T>): Promise<{ bytes: number; result: PromiseSettledResult<T> }> {
  const probe = await open(path.join(dir, 'probe'), 'w');
  const proto = Object.getPrototypeOf(probe) as { read: (...args: unknown[]) => Promise<ReadResult> };
  await probe.close();
  const spy = vi.spyOn(proto, 'read');
  try {
    const [result] = await Promise.allSettled([f()]);
    const reads = await Promise.all(spy.mock.results.map((r) => r.value as Promise<ReadResult>));
    return { bytes: reads.reduce((n, r) => n + r.bytesRead, 0), result: result! };
  } finally {
    spy.mockRestore();
  }
}

describe('任务目录中的报告：有界读取', () => {
  it('远大于上限的文件最多读上限 + 1 字节就拒绝，错误信息不变', async () => {
    const dir = await tempDir();
    const p = path.join(dir, 'report.json');
    await sparse(p, bigFile);
    const { bytes, result } = await bytesRead(dir, () => readLimited(p));
    expect(result).toMatchObject({ status: 'rejected', reason: expect.objectContaining({ message: `report.json 超过 ${maxReportBytes} 字节` }) });
    expect(bytes).toBeGreaterThan(maxReportBytes);
    expect(bytes).toBeLessThanOrEqual(maxReportBytes + 1);
  });

  it('刚好等于上限的文件能完整读出；上限 + 1 字节被拒绝；不存在时为 undefined', async () => {
    const dir = await tempDir();
    const p = path.join(dir, 'delivery.json');
    await sparse(p, maxReportBytes, 'END');
    const b = await readLimited(p);
    expect(b?.length).toBe(maxReportBytes);
    expect(Buffer.from(b!.subarray(maxReportBytes - 3)).toString()).toBe('END');
    await sparse(p, maxReportBytes + 1);
    await expect(readLimited(p)).rejects.toThrow(`delivery.json 超过 ${maxReportBytes} 字节`);
    expect(await readLimited(path.join(dir, 'missing.json'))).toBeUndefined();
  });

  it('不是普通文件（目录、FIFO）时报错，不会卡在打开或读取上', async () => {
    const dir = await tempDir();
    await mkdir(path.join(dir, 'selection.json'));
    await expect(readLimited(path.join(dir, 'selection.json'))).rejects.toThrow('selection.json 不是普通文件');
    if (process.platform === 'win32') return;
    const fifo = path.join(dir, 'report.json');
    await nodeExec('mkfifo', [fifo]);
    expect((await stat(fifo)).isFIFO()).toBe(true);
    await expect(readLimited(fifo)).rejects.toThrow('report.json 不是普通文件');
  });

  describe('Agent 的工具调用', () => {
    let env: Env | undefined;
    afterEach(async () => {
      await env?.cleanup();
      env = undefined;
    });

    it('check-report 遇到过大的 report.json 回复超限，Agent 改正后照常结束', async () => {
      const outputs: string[] = [];
      env = await makeEnv(async (ctx) => {
        const p = path.join(ctx.taskDir, 'report.json');
        await sparse(p, bigFile);
        outputs.push((await ctx.tool('check-report')).output);
        await writeFile(p, JSON.stringify({ version: 1, verdict: 'approve', summary: 'ok', findings: [] }));
        ctx.done();
      });
      const e = env;
      const sha = await e.origin.branchSha('master');
      const at = new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha }));
      e.orca.tool = (_id, cmd, args) => at.tool(cmd, args);
      const ac = new AbortController();
      const pump = e.deps.mailbox.pump(ac.signal);
      try {
        await at.run(false);
      } finally {
        ac.abort();
        await pump;
      }
      expect(outputs).toHaveLength(1);
      expect(outputs[0]).toContain(`report.json 超过 ${maxReportBytes} 字节`);
      expect(e.kiln.completes).toHaveLength(1);
    });
  });
});

describe('结束时读取报告：Agent 写出的文件不合格是 AI 失败，不是基础设施错误（审计）', () => {
  let env: Env | undefined;
  afterEach(async () => {
    await env?.cleanup();
    env = undefined;
  });

  async function runAttempt(e: Env, at: Attempt): Promise<void> {
    e.orca.tool = (_id, cmd, args) => at.tool(cmd, args);
    const ac = new AbortController();
    const pump = e.deps.mailbox.pump(ac.signal);
    try {
      await at.run(false);
    } finally {
      ac.abort();
      await pump;
    }
  }

  it('审计报告是目录（不是普通文件）：以 ai.report 失败并写明原因，不是基础设施错误', async () => {
    env = await makeEnv(async (ctx) => {
      await mkdir(path.join(ctx.taskDir, 'report.json'));
      ctx.done();
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    await runAttempt(e, new Attempt(e.deps, reviewTask(e.origin, { sha, baseSha: sha })));
    expect(e.kiln.completes).toHaveLength(1);
    expect(e.kiln.completes[0]).toMatchObject({ outcome: Outcome.FAILED, errorClass: ErrorClass.report, message: expect.stringContaining('report.json 不是普通文件') });
  });

  it('交付报告超过上限：以 ai.delivery 失败并写明原因，不是基础设施错误', async () => {
    env = await makeEnv(async (ctx) => {
      await sparse(path.join(ctx.taskDir, 'delivery.json'), bigFile);
      ctx.done();
    });
    const e = env;
    const sha = await e.origin.branchSha('master');
    await runAttempt(e, new Attempt(e.deps, devTask(e.origin, { sha, mode: 'implement' })));
    expect(e.kiln.completes).toHaveLength(1);
    expect(e.kiln.completes[0]).toMatchObject({
      outcome: Outcome.FAILED,
      errorClass: ErrorClass.delivery,
      message: expect.stringContaining(`delivery.json 超过 ${maxReportBytes} 字节`),
    });
  });
});

describe('命令输出的上限', () => {
  const limit = 1024;
  const run = (script: string) => nodeExec(process.execPath, ['-e', script], { maxOutputBytes: limit, killGraceMs: 300, timeoutMs: 30_000 });

  it('stdout 一直输出：超过上限即终止命令，明确报错，不返回不完整的输出', async () => {
    // 先输出上限以内的一段（已经收集），再一直输出
    const r = await run(`process.stdout.write('x'.repeat(${limit - 24})); setTimeout(() => { for (;;) process.stdout.write('x'.repeat(4096)); }, 200)`);
    expect(r.code).toBe(outputLimitCode);
    expect(r.stdout).toBe('');
    expect(r.stderr).toContain(`stdout 超过 ${limit} 字节，已终止命令`);
  });

  it('stderr 超过上限同样报错；刚好等于上限时照常返回', async () => {
    const r = await run(`process.stderr.write('e'.repeat(${limit + 1}))`);
    expect(r.code).toBe(outputLimitCode);
    expect(r.stderr).toContain(`stderr 超过 ${limit} 字节`);
    expect(await run(`process.stdout.write('x'.repeat(${limit}))`)).toEqual({ code: 0, stdout: 'x'.repeat(limit), stderr: '', end: 'exit' });
  });

  it('按字节计算（多字节字符）', async () => {
    // 400 个字符、1200 字节
    const r = await run(`process.stdout.write('中'.repeat(400))`);
    expect(r.code).toBe(outputLimitCode);
  });

  it('git 的输出由 Agent 的 worktree 决定：超限时 git 命令失败并说明原因', async () => {
    const dir = await tempDir();
    await git(dir, 'init', '--quiet');
    for (let i = 0; i < 100; i++) await writeFile(path.join(dir, `untracked-${i}.txt`), '');
    const g = new Git((cmd, args, o) => nodeExec(cmd, args, { ...o, maxOutputBytes: limit }));
    const err = await g.run(dir, 'status', '--porcelain').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ExecError);
    expect((err as ExecError).message).toContain(`stdout 超过 ${limit} 字节`);
  });

  it('orca 的输出超限：结果未知的错误（不是格式错误），带上原因', async () => {
    const exec: Exec = (_cmd, _args, o) => nodeExec(process.execPath, ['-e', `process.stdout.write('{"ok":true,"result":{}}'.padEnd(${limit + 1}))`], { ...o, maxOutputBytes: limit });
    const err = await new OrcaCli(exec, 'orca').reply('h', 'm', 'b').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OrcaError);
    expect(err).toMatchObject({ code: 'exec_failed' });
    expect((err as OrcaError).message).toContain(`stdout 超过 ${limit} 字节`);
    expect(isDefinite(err)).toBe(false);
  });
});

describe('本机工具的请求体', () => {
  it('超过上限的请求明确回复超限，不交给处理函数；按字节计算', async () => {
    const dir = await tempDir();
    const sock = path.join(dir, 'run', 's.sock');
    const calls: string[] = [];
    const srv = await serveTools(sock, (r) => {
      calls.push(r.cmd);
      return Promise.resolve({ code: 0, output: 'ok' });
    });
    try {
      const big = await callTool(sock, { attempt: '1', cmd: 'ci', args: { sha: 'a'.repeat(maxToolRequestBytes) } });
      expect(big).toEqual({ code: 1, output: `请求无效：超过 ${maxToolRequestBytes} 字节` });
      // 字符数不到上限、字节数超过（每个汉字 3 字节）
      const wide = await callTool(sock, { attempt: '1', cmd: 'ci', args: { sha: '中'.repeat(maxToolRequestBytes / 2) } });
      expect(wide.output).toContain('超过');
      expect(calls).toEqual([]);
      expect(await callTool(sock, { attempt: '1', cmd: 'ci', args: { sha: '中'.repeat(1000) } })).toEqual({ code: 0, output: 'ok' });
      expect(calls).toEqual(['ci']);
    } finally {
      srv.close();
    }
  });
});

describe('本机工具的回复', () => {
  /** rawServer 在 unix socket 上起一个直接写回复的 HTTP 服务（模拟不按协议回复的一方）。 */
  async function rawServer(sock: string, reply: (res: ServerResponse) => void): Promise<Server> {
    await mkdir(path.dirname(sock), { recursive: true });
    const srv = createServer((req, res) => {
      req.resume();
      req.on('end', () => reply(res));
    });
    await new Promise<void>((r) => srv.listen(sock, r));
    return srv;
  }

  it('大的回复完整收到（带长度）；超过上限的回复连接器不发送，改为明确回复过大', async () => {
    const dir = await tempDir();
    const sock = path.join(dir, 'run', 's.sock');
    const big = `${'中'.repeat(700_000)}END`;
    const srv = await serveTools(sock, (r) => Promise.resolve(r.cmd === 'big' ? { code: 3, output: big } : { code: 0, output: 'x'.repeat(maxToolReplyBytes) }));
    try {
      expect(await callTool(sock, { attempt: '1', cmd: 'big', args: {} })).toEqual({ code: 3, output: big });
      const over = await callTool(sock, { attempt: '1', cmd: 'over', args: {} });
      expect(over.code).toBe(1);
      expect(over.output).toContain(`超过 ${maxToolReplyBytes} 字节的上限，没有发送`);
    } finally {
      srv.close();
    }
  });

  it('kiln-orca task 读到超过上限的回复：停止读取并报错，不当作完整的输出', async () => {
    const dir = await tempDir();
    const sock = path.join(dir, 'run', 's.sock');
    const srv = await rawServer(sock, (res) => {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ code: 0, output: 'x'.repeat(maxToolReplyBytes) }));
    });
    try {
      await expect(callTool(sock, { attempt: '1', cmd: 'ci', args: {} })).rejects.toBeInstanceOf(ToolReplyTooLargeError);
    } finally {
      srv.closeAllConnections();
      srv.close();
    }
  });

  it('回复写完之前连接断开：报错，不把收到的一部分当作回复', async () => {
    const dir = await tempDir();
    const sock = path.join(dir, 'run', 's.sock');
    const body = JSON.stringify({ code: 0, output: 'y'.repeat(1 << 20) });
    const srv = await rawServer(sock, (res) => {
      res.writeHead(200, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(body) });
      // 只写一半就断开
      res.write(body.slice(0, body.length / 2), () => res.destroy());
    });
    try {
      await expect(callTool(sock, { attempt: '1', cmd: 'ci', args: {} })).rejects.toThrow();
    } finally {
      srv.close();
    }
  });
});

describe('服务管理器写的日志', () => {
  it('很大的 service.*.log 只读末尾移进按日文件', async () => {
    const dir = await tempDir();
    await sparse(path.join(dir, serviceLogs.err), bigFile, 'TAIL\n');
    const { bytes, result } = await bytesRead(dir, () => pruneLogs(dir));
    expect(result.status).toBe('fulfilled');
    expect(bytes).toBeGreaterThan(0);
    expect(bytes).toBeLessThanOrEqual(serviceLogMaxBytes);
    const daily = await readFile(path.join(dir, dailyLog(new Date())), 'utf8');
    expect(daily).toContain(`dropped_bytes=${bigFile - serviceLogMaxBytes}`);
    expect(daily.endsWith('TAIL\n')).toBe(true);
    expect((await stat(path.join(dir, serviceLogs.err))).size).toBe(0);
  });
});
