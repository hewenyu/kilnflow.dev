import { type ChildProcess, spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { mkdir, readFile, readlink, rm, writeFile } from 'node:fs/promises';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { Code, ConnectError, type ConnectRouter } from '@connectrpc/connect';
import { connectNodeAdapter } from '@connectrpc/connect-node';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import { AIRunnerService } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { RunnerService } from '../src/gen/kiln/runner/v1/runner_pb.js';
import { nodeExec } from '../src/core/exec.js';
import { defaultConfig, Home } from '../src/host/home.js';
import { serveTools, type ToolResponse } from '../src/host/socket.js';
import { buildCli, type CliBuild } from './clibuild.js';
import { shortTmp } from './harness.js';

// 编译后的 kiln-orca 命令行（dist/host/cli.js 的同一份构建）作为真实的子进程运行：stdout / stderr 接管道，读端故意慢
// （等子进程退出或等一段时间后才开始读）。stdout / stderr 是管道时写入是异步的，进程被强制退出会丢掉还没写进管道的部分；
// 命令也不能因为残留的句柄（定时器、socket、监听器）挂住不退出。

const pkg = path.join(path.dirname(fileURLToPath(import.meta.url)), '..');

/** bigOutput 远大于管道缓冲（Linux、macOS 都是几十 KiB）加上读端的缓冲：强制退出时一定丢掉一部分。 */
const bigOutput = 2 << 20;
/** slowReaderMs：命令拿到结果后读端再等这么久才开始读；强制退出的命令在这段时间内已经退出、丢掉了输出。 */
const slowReaderMs = 1_500;
/** exitTimeoutMs：开始读之后命令应在这段时间内退出（没有残留的句柄）。 */
const exitTimeoutMs = 20_000;

let build: CliBuild | undefined;
let cli = '';
beforeAll(async () => {
  build = await buildCli();
  cli = build.cli;
}, 120_000);
afterAll(async () => {
  await build?.cleanup();
});

let root = '';
const cleanups: (() => Promise<void> | void)[] = [];
afterEach(async () => {
  for (const c of cleanups.splice(0).reverse()) await c();
  if (root) await rm(root, { recursive: true, force: true });
  root = '';
});

async function tempRoot(): Promise<string> {
  // 短路径：unix socket 的路径长度有限
  root = await shortTmp('ko-cli-');
  return root;
}

interface Result {
  code: number | null;
  signal: NodeJS.Signals | null;
  stdout: string;
  stderr: string;
}

function run(cmd: string, args: string[]): Promise<Result> {
  const c = spawn(cmd, args, { stdio: ['ignore', 'pipe', 'pipe'] });
  return collect(c);
}

/** collect 读完子进程的 stdout、stderr 并等它退出；超过 exitTimeoutMs 没有退出时结束它并报错（命令挂住）。 */
async function collect(c: ChildProcess): Promise<Result> {
  let stdout = '';
  let stderr = '';
  c.stdout?.setEncoding('utf8').on('data', (d: string) => (stdout += d));
  c.stderr?.setEncoding('utf8').on('data', (d: string) => (stderr += d));
  let timer: NodeJS.Timeout | undefined;
  const closed = new Promise<Result>((resolve) => c.once('close', (code, signal) => resolve({ code, signal, stdout, stderr })));
  const hung = new Promise<never>((_, reject) => {
    timer = setTimeout(() => {
      c.kill('SIGKILL');
      reject(new Error(`命令 ${exitTimeoutMs} ms 内没有退出（有残留的句柄？）：stdout ${stdout.length} 字符，stderr ${stderr.slice(-2000)}`));
    }, exitTimeoutMs);
  });
  try {
    return await Promise.race([closed, hung]);
  } finally {
    clearTimeout(timer);
  }
}

/**
 * hostEnv 是运行测试的进程的环境去掉 ORCA_* 和 CLAUDE_CONFIG_DIR：测试可能就在 Orca 的 Agent 会话中运行，继承那里的身份
 * 和 ORCA_CLI_COMMAND 时 kiln-orca task done 会用真实的 Orca 以这个会话的身份发送 worker_done；CLAUDE_CONFIG_DIR 会让
 * kiln-orca 把 skill 写进运行测试的机器上真实的 Claude 配置目录。
 */
function hostEnv(): NodeJS.ProcessEnv {
  return Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith('ORCA_') && k !== 'CLAUDE_CONFIG_DIR'));
}

/** emptyPath 是没有任何命令的 PATH：orca、docker、git、Agent 都找不到（命令行只用到 node 本身）。 */
async function emptyPath(): Promise<string> {
  const d = path.join(root, 'empty-bin');
  await mkdir(d, { recursive: true });
  return d;
}

/**
 * cli 运行 kiln-orca；slowAfter 给出时读端慢：等 slowAfter 成立（命令已经拿到结果）后再等 slowReaderMs，期间子进程
 * 退出也一样，之后才开始读。
 */
async function kilnOrca(args: string[], o: { slowAfter?: Promise<unknown>; env?: Record<string, string>; input?: string } = {}): Promise<Result> {
  if (!root) root = await shortTmp('ko-cli-');
  // HOME 指向临时目录：kiln-orca run / doctor 把 kiln-task skill 放在这里，绝不读写宿主机上的 ~/.agents、~/.claude
  const c = spawn(process.execPath, [cli, ...args], {
    stdio: [o.input === undefined ? 'ignore' : 'pipe', 'pipe', 'pipe'],
    env: { ...hostEnv(), HOME: path.join(root, 'home'), PATH: await emptyPath(), ...o.env },
  });
  if (o.input !== undefined) c.stdin?.end(o.input);
  if (o.slowAfter) {
    const exited = new Promise((r) => c.once('exit', r));
    await Promise.race([exited, o.slowAfter.then(() => new Promise((r) => setTimeout(r, slowReaderMs)))]);
  }
  return collect(c);
}

/** toolServer 在数据目录的 socket 上起连接器的工具服务（与 kiln-orca run 中相同的 serveTools），回复 reply。 */
async function toolServer(home: Home, reply: ToolResponse): Promise<{ called: Promise<void> }> {
  let called!: () => void;
  const p = new Promise<void>((r) => (called = r));
  const srv = await serveTools(home.socketPath, () => {
    called();
    return Promise.resolve(reply);
  });
  cleanups.push(() => new Promise<void>((r) => srv.close(() => r())));
  return { called: p };
}

/** big 是 n 字节左右的多行文本（中英文混合，多字节字符跨越写入的边界），以 marker 结尾。 */
function big(n: number, marker: string): string {
  const line = '## 失败：job-0001（user）FAIL expected 1 got 2 — 日志 —\n';
  return line.repeat(Math.ceil(n / Buffer.byteLength(line))) + marker;
}

interface KilnServer {
  url: string;
  acquires: number;
  revoked: boolean;
  enrollError?: string;
  enrollCalled: Promise<void>;
}

/** kilnServer 是 HTTP 上真实的 Connect 服务（与连接器连 Server 的方式相同）：注册为 AI 审计节点，领取总是没有任务。 */
async function kilnServer(): Promise<KilnServer> {
  let enrolled!: () => void;
  const s: KilnServer = { url: '', acquires: 0, revoked: false, enrollCalled: new Promise((r) => (enrolled = r)) };
  const routes = (r: ConnectRouter) => {
    r.service(RunnerService, {
      enroll: () => {
        enrolled();
        if (s.enrollError !== undefined) throw new ConnectError(s.enrollError, Code.Internal);
        return { runnerId: 7n, kind: 'ai', role: 'review' };
      },
      authenticate: () => {
        if (s.revoked) throw new ConnectError('RUNNER_REVOKED', Code.PermissionDenied);
        return { accessToken: 'token', expiresAt: BigInt(Math.floor(Date.now() / 1000) + 3600) };
      },
    });
    r.service(AIRunnerService, {
      acquire: () => {
        s.acquires++;
        if (s.revoked) throw new ConnectError('RUNNER_REVOKED', Code.PermissionDenied);
        return {};
      },
    });
  };
  const srv: Server = createServer(connectNodeAdapter({ routes }));
  await new Promise<void>((r) => srv.listen(0, '127.0.0.1', r));
  cleanups.push(
    () =>
      new Promise<void>((r) => {
        srv.closeAllConnections();
        srv.close(() => r());
      }),
  );
  s.url = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`;
  return s;
}

/** enrolled 用命令行注册一个数据目录，再把 orca、docker 指向不存在的命令（不碰本机真实的 Orca、Docker）。 */
async function enrolled(srv: KilnServer): Promise<Home> {
  const home = new Home(path.join(root, 'h'));
  const r = await kilnOrca(['enroll', '--server', srv.url, '--token', 'tok', '--name', 'n1', '--home', home.dir]);
  expect(r.stderr).toBe('');
  expect(r.code).toBe(0);
  expect(r.stdout).toContain('已注册：n1（id 7，角色 review）');
  await writeFile(home.path('config.json'), JSON.stringify({ ...(await home.loadConfig()), orca: path.join(root, 'no-orca'), docker: path.join(root, 'no-docker') }));
  return home;
}

describe('kiln-orca 命令行：管道中的输出完整写出', () => {
  it.each([
    { code: 0, what: '成功' },
    { code: 1, what: '失败' },
    { code: 3, what: '重新选题' },
  ])('task ci 的大输出（$what，退出码 $code）在读端慢时完整收到、以结尾标记结束', { timeout: 60_000 }, async ({ code }) => {
    const home = new Home(path.join(await tempRoot(), 'h'));
    const output = big(bigOutput, `END-${code}`);
    const srv = await toolServer(home, { code, output });
    const r = await kilnOrca(['task', 'ci', '--attempt', '12', '--home', home.dir], { slowAfter: srv.called });
    expect(r.stderr).toBe('');
    expect(r.stdout.length).toBe(output.length + 1);
    expect(r.stdout).toBe(output + '\n');
    expect(r.code).toBe(code);
  });

  it('stderr 上的大错误信息（命令失败）在读端慢时完整收到，退出码 1', { timeout: 60_000 }, async () => {
    await tempRoot();
    const srv = await kilnServer();
    srv.enrollError = big(bigOutput, 'END-ERR');
    const r = await kilnOrca(['enroll', '--server', srv.url, '--token', 'tok', '--home', path.join(root, 'h')], { slowAfter: srv.enrollCalled });
    expect(r.stdout).toBe('');
    expect(r.stderr.startsWith('kiln-orca：')).toBe(true);
    expect(r.stderr.endsWith('END-ERR\n')).toBe(true);
    expect(r.stderr.length).toBeGreaterThan(bigOutput / 2);
    expect(r.code).toBe(1);
  });
});

describe('kiln-orca 命令行：各命令正常退出', () => {
  it('version、用法错误、--help、不认识的命令', { timeout: 60_000 }, async () => {
    const h = path.join(await tempRoot(), 'h');
    const version = (JSON.parse(await readFile(path.join(pkg, 'package.json'), 'utf8')) as { version: string }).version;
    expect(await kilnOrca(['version', '--home', h])).toMatchObject({ code: 0, stdout: `${version}\n`, stderr: '' });
    const bogus = await kilnOrca(['bogus', '--home', h]);
    expect(bogus).toMatchObject({ code: 2, stdout: '' });
    expect(bogus.stderr).toContain('用法：kiln-orca');
    const opt = await kilnOrca(['version', '--nope']);
    expect(opt).toMatchObject({ code: 2, stdout: '' });
    expect(opt.stderr).toMatch(/^kiln-orca：.*--nope/);
    expect(await kilnOrca(['--help', '--home', h])).toMatchObject({ code: 0, stdout: '' });
    expect(await kilnOrca(['task', 'info', '--home', h])).toMatchObject({ code: 1, stdout: '', stderr: 'kiln-orca：需要 --attempt <id>\n' });
  });

  it('task 在连接器没有运行时：说明无法连接，退出码 1', { timeout: 60_000 }, async () => {
    const h = path.join(await tempRoot(), 'h');
    const r = await kilnOrca(['task', 'info', '--attempt', '1', '--home', h]);
    expect(r.stdout).toMatch(/^无法连接 kiln-orca（.*）：稍后重试/);
    expect(r.code).toBe(1);
  });

  it('task exec：用连接器给的工具链环境运行命令（本机 PATH 中同名的工具不会被用到），退出码原样返回', { timeout: 60_000 }, async () => {
    const h = new Home(path.join(await tempRoot(), 'h'));
    await h.prepareRunDirs({ clone_root: '' });
    // 本机 PATH 中有一个错误版本的 go；受管理的 go 在工具链目录中
    const host = path.join(root, 'host-bin');
    const managed = path.join(root, 'toolchains', 'abc', 'bin');
    for (const [d, text] of [
      [host, 'echo "wrong go"'],
      [managed, 'echo "managed go $GOTOOLCHAIN $1"; exit 3'],
    ] as const) {
      await mkdir(d, { recursive: true });
      await writeFile(path.join(d, 'go'), `#!/bin/sh\n${text}\n`, { mode: 0o755 });
    }
    const env = { set: { GOTOOLCHAIN: 'local' }, prepend: { PATH: [managed] } };
    await toolServer(h, { code: 0, output: JSON.stringify({ env }) });
    const r = await kilnOrca(['task', '--home', h.dir, '--attempt', '5', 'exec', '--', 'go', 'version'], { env: { PATH: host, GOTOOLCHAIN: 'auto' } });
    expect(r).toMatchObject({ code: 3, stdout: 'managed go local version\n' });
  });

  it('task exec：任务没有工具链时按原环境运行；连不上连接器时不运行命令；缺少 -- 时是用法错误', { timeout: 60_000 }, async () => {
    const h = new Home(path.join(await tempRoot(), 'h'));
    await h.prepareRunDirs({ clone_root: '' });
    const host = path.join(root, 'host-bin');
    await mkdir(host, { recursive: true });
    await writeFile(path.join(host, 'go'), '#!/bin/sh\necho "host go"\n', { mode: 0o755 });
    const down = await kilnOrca(['task', 'exec', '--home', h.dir, '--attempt', '5', '--', 'go'], { env: { PATH: host } });
    expect(down.code).toBe(1);
    expect(down.stdout).toMatch(/^无法从 kiln-orca 取得工具链.*不要改用本机安装的工具/);
    expect(down.stdout).not.toContain('host go');
    expect(await kilnOrca(['task', 'exec', '--home', h.dir, '--attempt', '5', 'go'], { env: { PATH: host } })).toMatchObject({ code: 2, stdout: '' });
    await toolServer(h, { code: 0, output: JSON.stringify({ env: null }) });
    expect(await kilnOrca(['task', 'exec', '--home', h.dir, '--attempt', '5', '--', 'go'], { env: { PATH: host } })).toMatchObject({ code: 0, stdout: 'host go\n' });
  });

  it('task exec：git 的远程助手换成传输包装（GIT_EXEC_PATH 指向镜像），不属于传输的 helper 请求不回答，个人 helper 和 askpass 不被调用；GOPRIVATE 加上依赖仓库', { timeout: 60_000 }, async () => {
    const h = new Home(path.join(await tempRoot(), 'h'));
    await h.prepareRunDirs({ clone_root: '' });
    const trace = path.join(root, 'personal-called');
    const global = path.join(root, 'global.gitconfig');
    await writeFile(global, `[credential]\n\thelper = "!f() { echo helper >> '${trace}'; echo username=evil; echo password=evil; }; f"\n`);
    const askpass = path.join(root, 'askpass.sh');
    await writeFile(askpass, `#!/bin/sh\necho askpass >> '${trace}'\necho evil\n`, { mode: 0o755 });
    const execRoot = path.join(root, 'task', 'git-exec');
    const asked: Array<Record<string, string>> = [];
    const srv = await serveTools(h.socketPath, (req) => {
      if (req.cmd === 'exec-env') return Promise.resolve({ code: 0, output: JSON.stringify({ env: null, git: { helperURL: 'https://git.example', goprivate: ['git.example/acme/lib'], execRoot } }) });
      asked.push({ cmd: req.cmd, ...req.args });
      return Promise.resolve({ code: 0, output: 'username=x-access-token\npassword=ghs_fromkiln\n' });
    });
    cleanups.push(() => new Promise<void>((r) => srv.close(() => r())));
    const which = await run('/bin/sh', ['-c', 'command -v git']);
    const gitDir = path.dirname(which.stdout.trim());
    const real = (await nodeExec(which.stdout.trim(), ['--exec-path'], { env: { GIT_EXEC_PATH: undefined } })).stdout.trim();
    const e = { PATH: `${gitDir}:/usr/bin:/bin`, GIT_CONFIG_GLOBAL: global, GIT_ASKPASS: askpass, SSH_ASKPASS: askpass, GOPRIVATE: 'corp.example', KILN_ORCA_GIT_OP: 'outer' };
    const exec = (argv: string[], input?: string) => kilnOrca(['task', '--home', h.dir, '--attempt', '5', 'exec', '--', ...argv], { env: e, input });
    const fill = (p: string, host = 'git.example') => exec(['git', 'credential', 'fill'], `protocol=https\nhost=${host}\npath=${p}\n\n`);

    // 不经传输包装（没有操作）的请求：连接器的 helper 不回答（也不去问连接器），不提示、不调用 askpass、不退回到个人 helper
    const denied = await fill('acme/widgets.git');
    expect(denied.code).not.toBe(0);
    expect(denied.stdout).not.toContain('password=');
    expect(asked).toEqual([]);
    expect(existsSync(trace)).toBe(false);
    // 别的主机不归 Kiln 管：照常用机器上的配置
    expect((await fill('x/y.git', 'other.example')).stdout).toContain('password=evil');
    const vars = await exec(['env']);
    const v = Object.fromEntries(vars.stdout.split('\n').map((l) => [l.slice(0, l.indexOf('=')), l.slice(l.indexOf('=') + 1)]));
    expect(v.GOPRIVATE).toBe('corp.example,git.example/acme/lib');
    expect(v.GIT_TERMINAL_PROMPT).toBe('0');
    expect(v.GIT_ASKPASS).toBeUndefined();
    expect(v.SSH_ASKPASS).toBeUndefined();
    expect(v.KILN_ORCA_GIT_OP).toBeUndefined();
    expect(vars.stdout).not.toContain('ghs_fromkiln');
    // 镜像：在 execRoot 下；远程助手是调用 task git-remote 的 sh 脚本，其它文件是指向 git 自己的目录的符号链接
    const mirror = v.GIT_EXEC_PATH ?? '';
    expect(path.dirname(mirror)).toBe(execRoot);
    const https = await readFile(path.join(mirror, 'git-remote-https'), 'utf8');
    expect(https).toMatch(/^#!\/bin\/sh\nexec .* git-remote --helper .*git-remote-https'? -- "\$@"\n$/);
    expect(https).toContain(path.join(real, 'git-remote-https'));
    expect(await readlink(path.join(mirror, 'git-upload-pack'))).toBe(path.join(real, 'git-upload-pack'));
    // 再次运行复用同一个镜像
    expect((await exec(['/bin/sh', '-c', 'echo "$GIT_EXEC_PATH"'])).stdout.trim()).toBe(mirror);
  });

  describe('task done：在本进程中（Agent 的后代）执行 Orca 命令行发送 worker_done（PLAN #33.5）', () => {
    const retryRequest = '0e3c3f5e-8a3b-4c7d-9f1e-2b6a1c0d9e8f';
    /** identity 是 Agent 的会话身份（Orca 给 structured 会话、终端模式设置的环境变量）：task done 原样传给 Orca 命令行。 */
    const identity = { ORCA_AGENT_SESSION_ID: 'sess-1', ORCA_TERMINAL_HANDLE: 'structworker_1', ORCA_PANE_KEY: 'pane-1', ORCA_STRUCTURED_SESSION: '1' };

    /**
     * fakeOrca 在 dir 中写一个 Orca 命令行的替身（名字 orca-ide）：记下参数、ORCA_* 和 PATH，输出 receipt、以 code 退出。
     * 返回它的路径和读记录的函数（没有执行时为 undefined）。
     */
    async function fakeOrca(dir: string, receipt: string, code = 0) {
      await mkdir(dir, { recursive: true });
      const bin = path.join(dir, 'orca-ide');
      const log = path.join(dir, 'calls.json');
      const js = `const fs = require('fs');
fs.writeFileSync(${JSON.stringify(log)}, JSON.stringify({ argv: process.argv.slice(2), env: Object.fromEntries(Object.entries(process.env).filter(([k]) => k.startsWith('ORCA_') || k === 'PATH')) }));
process.stdout.write(${JSON.stringify(receipt)});
process.exitCode = ${code};
`;
      await writeFile(bin, `#!${process.execPath}\n${js}`, { mode: 0o755 });
      return { bin, called: async () => (existsSync(log) ? (JSON.parse(await readFile(log, 'utf8')) as { argv: string[]; env: Record<string, string> }) : undefined) };
    }

    const completed = JSON.stringify({ ok: true, result: { message: { id: 'msg_1' }, lifecycle: { action: 'completed', outcome: 'succeeded' }, mutation: { requestId: 'req_1' } } });

    /** connector 是连接器的工具服务：done 回复 reply.done（发送参数或校验的问题，可以改），done-receipt 记下回执。 */
    async function connector(home: Home, done: ToolResponse) {
      const calls: { cmd: string; args: Record<string, string> }[] = [];
      const reply = { done };
      const srv = await serveTools(home.socketPath, (req) => {
        calls.push({ cmd: req.cmd, args: req.args });
        return Promise.resolve(req.cmd === 'done' ? reply.done : { code: 0, output: '' });
      });
      cleanups.push(() => new Promise<void>((r) => srv.close(() => r())));
      return Object.assign(calls, { reply });
    }

    const sendReply = (orca: { command: string; path: string }): ToolResponse => ({
      code: 0,
      output: JSON.stringify({ taskId: 'task_1', dispatchId: 'ctx_1', outcome: 'succeeded', subject: 'Kiln attempt 5：succeeded', body: '完成', retryRequest, orca }),
    });

    async function home(): Promise<Home> {
      const h = new Home(path.join(await tempRoot(), 'h'));
      await h.prepareRunDirs({ clone_root: '' });
      return h;
    }

    it('优先用环境中的 ORCA_CLI_COMMAND；不带 --from，环境（身份）原样继承；回执交回连接器，告诉 Agent 结束这一轮', { timeout: 60_000 }, async () => {
      const h = await home();
      const orca = await fakeOrca(path.join(root, 'orca-app'), completed);
      const other = await fakeOrca(path.join(root, 'configured'), completed);
      const calls = await connector(h, sendReply({ command: other.bin, path: '' }));
      const r = await kilnOrca(['task', 'done', '--home', h.dir, '--attempt', '5', '--outcome', 'succeeded', '--summary', '完成'], { env: { ...identity, ORCA_CLI_COMMAND: orca.bin } });
      expect(r).toMatchObject({ code: 0, stdout: '已向 Orca 报告结束（completed）。现在结束这一轮，不要再运行任何命令。\n' });
      const c = await orca.called();
      expect(c?.argv).toEqual([
        'orchestration', 'send', '--type', 'worker_done', '--task-id', 'task_1', '--dispatch-id', 'ctx_1', '--outcome', 'succeeded',
        '--subject', 'Kiln attempt 5：succeeded', '--body', '完成', '--retry-request', retryRequest, '--json', '--full',
      ]);
      expect(c?.env).toMatchObject({ ...identity, ORCA_CLI_COMMAND: orca.bin });
      expect(await other.called()).toBeUndefined();
      expect([...calls]).toEqual([
        { cmd: 'done', args: { outcome: 'succeeded', summary: '完成' } },
        { cmd: 'done-receipt', args: { command: orca.bin, source: 'ORCA_CLI_COMMAND', kind: 'reported', action: 'completed', messageId: 'msg_1', requestId: 'req_1' } },
      ]);
    });

    it('没有 ORCA_CLI_COMMAND：连接器的命令是名字时按连接器的 PATH 执行（Agent 的 PATH 中没有它）；带路径时直接执行', { timeout: 60_000 }, async () => {
      const h = await home();
      const dir = path.join(root, 'orca-bin');
      const orca = await fakeOrca(dir, completed);
      const conn = await connector(h, sendReply({ command: 'orca-ide', path: dir }));
      const byName = await kilnOrca(['task', 'done', '--home', h.dir, '--attempt', '5', '--outcome', 'succeeded'], { env: identity });
      expect(byName.code).toBe(0);
      const c = await orca.called();
      expect(c?.env).toMatchObject({ ...identity, PATH: dir });
      expect(c?.argv).not.toContain('--from');

      const abs = await fakeOrca(path.join(root, 'abs'), completed);
      conn.reply.done = sendReply({ command: abs.bin, path: dir });
      const agentPath = await emptyPath();
      expect((await kilnOrca(['task', 'done', '--home', h.dir, '--attempt', '5', '--outcome', 'succeeded'], { env: identity })).code).toBe(0);
      expect((await abs.called())?.env.PATH).toBe(agentPath);
    });

    it('执行不了 Orca 命令行：告诉 Agent 这是节点的问题，回执（exec）交回连接器', { timeout: 60_000 }, async () => {
      const h = await home();
      const calls = await connector(h, sendReply({ command: path.join(root, 'no-orca'), path: '' }));
      const r = await kilnOrca(['task', 'done', '--home', h.dir, '--attempt', '5', '--outcome', 'succeeded'], { env: identity });
      expect(r.code).toBe(1);
      expect(r.stdout).toContain(`这台机器上执行不了 Orca 的命令行（${path.join(root, 'no-orca')}，来自 config：`);
      expect(r.stdout).toContain('这是节点的问题，不是你的问题');
      expect(calls[1]).toMatchObject({ cmd: 'done-receipt', args: { kind: 'exec', source: 'config', message: expect.stringContaining('ENOENT') } });
    });

    it('Orca 不认发送者：说明原因，退出码 1', { timeout: 60_000 }, async () => {
      const h = await home();
      const orca = await fakeOrca(path.join(root, 'orca-app'), JSON.stringify({ id: 'local', ok: false, error: { code: 'no_active_sender_terminal', message: 'Could not determine the calling terminal' } }), 1);
      const calls = await connector(h, sendReply({ command: orca.bin, path: '' }));
      const r = await kilnOrca(['task', 'done', '--home', h.dir, '--attempt', '5', '--outcome', 'succeeded']);
      expect(r.code).toBe(1);
      expect(r.stdout).toContain('Orca 不接受这次报告的发送者（no_active_sender_terminal：Could not determine the calling terminal）');
      expect(calls[1]).toMatchObject({ args: { kind: 'identity', code: 'no_active_sender_terminal' } });
    });

    it('连接器校验不通过：原样输出问题，不执行 Orca；用法错误', { timeout: 60_000 }, async () => {
      const h = await home();
      const orca = await fakeOrca(path.join(root, 'orca-app'), completed);
      const calls = await connector(h, { code: 1, output: '审计报告不合格，请修正后再运行：\n- 缺少 summary' });
      const r = await kilnOrca(['task', 'done', '--home', h.dir, '--attempt', '5', '--outcome', 'succeeded'], { env: { ORCA_CLI_COMMAND: orca.bin } });
      expect(r).toMatchObject({ code: 1, stdout: '审计报告不合格，请修正后再运行：\n- 缺少 summary\n' });
      expect(await orca.called()).toBeUndefined();
      expect(calls).toHaveLength(1);
      expect((await kilnOrca(['task', 'done', '--home', h.dir, '--attempt', '5', 'extra'])).code).toBe(2);
      expect((await kilnOrca(['task', 'done', '--home', h.dir, '--attempt', '5', '--', 'x'])).code).toBe(2);
    });
  });

  it('doctor：没有注册时列出问题，退出码 1；注册后 Orca 不可用同样是 1；drain on / off', { timeout: 60_000 }, async () => {
    await tempRoot();
    const fresh = await kilnOrca(['doctor', '--home', path.join(root, 'fresh')]);
    expect(fresh.stdout).toContain('✗ 注册：还没有注册');
    expect(fresh.code).toBe(1);
    const srv = await kilnServer();
    const home = await enrolled(srv);
    const r = await kilnOrca(['doctor', '--home', home.dir]);
    expect(r.stdout).toContain('✓ 注册：n1（id 7，角色 review）');
    expect(r.stdout).toContain(`✓ 连接 Server：${srv.url}`);
    expect(r.stdout).toContain('✗ Orca');
    // 随包分发的 kiln-task skill：只读检查；还没有安装时说明启动连接器时自动安装，不创建任何目录
    expect(r.stdout).toContain('✓ kiln-task skill（codex）：尚未安装，启动连接器时会自动安装到');
    expect(r.stdout).toContain('✓ kiln-task skill（claude-code）：尚未安装，启动连接器时会自动安装到');
    expect(existsSync(path.join(root, 'home', '.agents'))).toBe(false);
    expect(existsSync(path.join(root, 'home', '.claude'))).toBe(false);
    expect(r.code).toBe(1);
    // 服务用户的 netrc 中有 github.com：git 认证配置一项失败，只有键和来源
    const userHome = path.join(root, 'user-home');
    await mkdir(userHome, { recursive: true });
    await writeFile(path.join(userHome, '.netrc'), 'machine github.com login x password Sekr3tNetrc\n');
    const gitDir = path.dirname((await nodeExec('sh', ['-c', 'command -v git'])).stdout.trim());
    // HOME 指向 userHome：不读运行测试的机器上真实的 netrc
    const auth = await kilnOrca(['doctor', '--home', home.dir], {
      env: { HOME: userHome, PATH: gitDir, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null' },
    });
    expect(auth.stdout).toContain(`✗ git 认证配置：netrc 中的主机 github.com（来源：${path.join(userHome, '.netrc')}）`);
    expect(auth.stdout).not.toContain('Sekr3t');
    expect(auth.code).toBe(1);
    // 只对一个仓库路径设置了请求头（根地址匹配不到），而且只写在 XDG 的那份全局配置中（~/.gitconfig 也存在）：同样报告，
    // 来源指到那个文件
    await rm(path.join(userHome, '.netrc'));
    await writeFile(path.join(userHome, '.gitconfig'), '[user]\n\tname = someone\n');
    await mkdir(path.join(userHome, '.config', 'git'), { recursive: true });
    const scoped = path.join(userHome, '.config', 'git', 'config');
    await writeFile(scoped, '[http "https://github.com/acme/r.git"]\n\textraHeader = Authorization: Basic Sekr3tScoped\n');
    const repoAuth = await kilnOrca(['doctor', '--home', home.dir], { env: { HOME: userHome, PATH: gitDir, GIT_CONFIG_NOSYSTEM: '1' } });
    expect(repoAuth.stdout).toContain(`✗ git 认证配置：http.https://github.com/acme/r.extraheader（来源：全局配置 ${scoped}）`);
    expect(repoAuth.stdout).not.toContain('Sekr3t');
    expect(repoAuth.code).toBe(1);
    expect(await kilnOrca(['drain', 'on', '--home', home.dir])).toMatchObject({ code: 0, stdout: '已停止领取新任务，正在执行的任务继续\n' });
    expect(await kilnOrca(['drain', 'off', '--home', home.dir])).toMatchObject({ code: 0, stdout: '已恢复领取\n' });
    expect(defaultConfig().max_tasks).toBeGreaterThan(0);
  });

  it('run：收到 SIGTERM 后停止并自己退出（没有残留的句柄），退出码 0；再次运行能取到锁', { timeout: 60_000 }, async () => {
    await tempRoot();
    const srv = await kilnServer();
    const home = await enrolled(srv);
    for (const round of [1, 2]) {
      const before = srv.acquires;
      const c = spawn(process.execPath, [cli, 'run', '--home', home.dir], {
        stdio: ['ignore', 'pipe', 'pipe'],
        env: { ...hostEnv(), HOME: path.join(root, 'home'), PATH: await emptyPath() },
      });
      const done = collect(c);
      const until = Date.now() + exitTimeoutMs;
      while (srv.acquires === before && Date.now() < until) await new Promise((r) => setTimeout(r, 20));
      expect(srv.acquires, `第 ${round} 次运行领取`).toBeGreaterThan(before);
      c.kill('SIGTERM');
      const r = await done;
      expect(r, r.stderr).toMatchObject({ code: 0, signal: null });
      expect(r.stderr).toContain('收到退出信号');
    }
  });

  it('run：被吊销后清理完自己退出，退出码 0（服务管理器不再重启）', { timeout: 60_000 }, async () => {
    await tempRoot();
    const srv = await kilnServer();
    const home = await enrolled(srv);
    srv.revoked = true;
    const r = await kilnOrca(['run', '--home', home.dir]);
    expect(r, r.stderr).toMatchObject({ code: 0, signal: null });
    expect(r.stderr).toContain('已在 Kiln 中被吊销');
    expect(await home.isRevoked('7')).toBe(true);
  });
});
