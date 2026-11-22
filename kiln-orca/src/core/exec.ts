import { spawn } from 'node:child_process';
import { constants } from 'node:os';

/** 一次命令执行的结果。 */
export interface ExecResult {
  code: number;
  stdout: string;
  stderr: string;
  /**
   * signal 是终止子进程的信号（子进程被信号终止时，code 为 128 + 信号编号，同 shell：与命令自己的退出码 1、5 等不会混淆）。
   * 超时、中止、输出超限由 nodeExec 终止的，code 是 timeoutCode 等，不设置这个字段。
   */
  signal?: NodeJS.Signals;
  /**
   * end 是子进程怎么结束的：exit（自己正常退出，code 是它的退出码）、signal（被外部信号终止）、timeout（超时被终止）、
   * overflow（输出超过上限被终止）、abort（调用方取消）、notrun（没能启动）。只有 exit 时 code 才是命令自己的退出码；
   * 调用方按退出码解释结果（例如 git config 的 1 表示没有）之前先用 exitedNormally 判断。nodeExec 总是设置；测试中的替身
   * 不设置时按 exit 处理。
   */
  end?: ExecEnd;
}

/** ExecEnd 是子进程结束的方式（见 ExecResult.end）。 */
export type ExecEnd = 'exit' | 'signal' | 'timeout' | 'overflow' | 'abort' | 'notrun';

/** exitedNormally 判断命令是不是自己正常退出的（这时 code 才是命令自己的退出码）。 */
export function exitedNormally(r: ExecResult): boolean {
  return (r.end ?? 'exit') === 'exit' && r.signal === undefined;
}

/**
 * notRun 判断子进程是不是没能启动（end 为 notrun）。优先按 end；测试中的替身没有设置 end 时按退出码 127 回退
 * （nodeExec 启动不了命令时的退出码，与 shell 一致）。
 */
export function notRun(r: ExecResult): boolean {
  return r.end === undefined ? r.code === notRunCode : r.end === 'notrun';
}

export interface ExecOptions {
  cwd?: string;
  /** 追加（覆盖）的环境变量；值为 undefined 时从环境中删除。 */
  env?: Record<string, string | undefined>;
  timeoutMs?: number;
  /** 超时发出 SIGTERM 后等多久再 SIGKILL，默认 defaultKillGraceMs（测试中覆盖）。 */
  killGraceMs?: number;
  /** 子进程退出后最多等多久让 stdout/stderr 读完（close），默认 defaultPipeDrainMs（测试中覆盖）。 */
  pipeDrainMs?: number;
  /** stdout、stderr 各自最多收集多少字节，默认 defaultMaxOutputBytes（测试中覆盖）。 */
  maxOutputBytes?: number;
  signal?: AbortSignal;
  /** 写到子进程 stdin 的内容（写完即关闭）；不给时 stdin 是空的。 */
  input?: string;
}

/** Exec 执行外部命令（git、orca、docker、kiln-runner toolchain、Agent 的 --version）。测试中替换为假的实现。 */
export type Exec = (cmd: string, args: readonly string[], opts?: ExecOptions) => Promise<ExecResult>;

/** ExecError 是命令以非 0 退出（或无法启动、超时）。 */
export class ExecError extends Error {
  constructor(
    readonly cmd: string,
    readonly args: readonly string[],
    readonly result: ExecResult,
  ) {
    super(`${cmd} ${args.slice(0, 3).join(' ')}：退出码 ${result.code}：${tail((result.stderr + '\n' + result.stdout).trim(), 2000)}`);
    this.name = 'ExecError';
  }
}

/** tail 返回 s 的最后 n 个字符（前面加省略号）。 */
export function tail(s: string, n: number): string {
  return s.length <= n ? s : '…' + s.slice(s.length - n);
}

/** 超时或中止后发出 SIGTERM，等这么久进程仍未退出再 SIGKILL（git 收到 SIGTERM 会清理自己的锁文件）。 */
export const defaultKillGraceMs = 10_000;

/**
 * 子进程退出（或 SIGKILL）后最多再等这么久让 stdout/stderr 读完（close）；孙进程可能继承了管道一直不关，
 * 到期后主动关闭本端管道再返回。
 */
export const defaultPipeDrainMs = 1_000;

/**
 * defaultMaxOutputBytes 是一条命令的 stdout、stderr 各自最多收集的字节数。输出的大小可能由 Agent 决定（它的 worktree 中
 * git status、git diff --name-only 的输出，Orca 消息的正文），一个节点的多个任务共用一个进程，所以不能无限累加。上限远大于
 * 正常的输出（连接器执行的命令通常只输出几 KB）：超过时不是截断（截断的 JSON 会被当成格式错误），而是终止命令、明确报错。
 */
export const defaultMaxOutputBytes = 16 << 20;

/** 超时的退出码（同 coreutils timeout）。 */
export const timeoutCode = 124;
/** 输出超过上限而被终止的退出码（不是命令自己的退出码，说明在 stderr 中）。 */
export const outputLimitCode = 125;
/** 无法启动或被 signal 中止的退出码（同 shell 的 command not found）。 */
const notRunCode = 127;
const abortMessage = 'AbortError: 已中止';

/**
 * nodeExec 用 child_process 执行命令，不经过 shell。
 *
 * 超时（退出码 124）、signal 中止（退出码 127）或输出超过上限（退出码 125）都由 terminate 结束子进程：先 SIGTERM，宽限期
 * （killGraceMs）后仍未退出再 SIGKILL；进入终止流程后不再改变（先超时后中止仍按超时结束，SIGKILL 兜底不会被取消）。
 * 输出超过上限时不论因为什么结束都按超限返回，stdout 为空（不返回不完整的输出，调用方不会把它当成完整的结果解析）。
 * 子进程退出后最多等 pipeDrainMs 让输出读完；子进程已经退出、孙进程还占着管道时到达期限，立即关闭管道按超时返回。
 * 结果只返回一次，定时器在返回时全部清理。
 */
export const nodeExec: Exec = (cmd, args, opts = {}) =>
  new Promise((resolve) => {
    const { signal } = opts;
    if (signal?.aborted) {
      resolve({ code: notRunCode, stdout: '', stderr: abortMessage, end: 'abort' });
      return;
    }
    const env: NodeJS.ProcessEnv = { ...process.env };
    for (const [k, v] of Object.entries(opts.env ?? {})) {
      if (v === undefined) delete env[k];
      else env[k] = v;
    }
    const child = opts.input === undefined ? spawn(cmd, [...args], { cwd: opts.cwd, env, stdio: ['ignore', 'pipe', 'pipe'] }) : spawn(cmd, [...args], { cwd: opts.cwd, env, stdio: ['pipe', 'pipe', 'pipe'] });
    if (opts.input !== undefined) {
      // 子进程不读或提前退出时写入会失败（EPIPE）：结果以退出码为准，这里不再报错
      child.stdin?.on('error', () => undefined);
      child.stdin?.end(opts.input);
    }
    let stdout = '';
    let stderr = '';
    const maxOutput = opts.maxOutputBytes ?? defaultMaxOutputBytes;
    const sizes = { stdout: 0, stderr: 0 };
    /** 超过上限的输出（之后的输出都不再收集）。 */
    let overflow: 'stdout' | 'stderr' | undefined;
    let settled = false;
    let exited = false;
    let exitCode: number | null = null;
    let exitSignal: NodeJS.Signals | null = null;
    let ending: 'timeout' | 'abort' | 'overflow' | undefined;
    const drainMs = opts.pipeDrainMs ?? defaultPipeDrainMs;
    const timers = new Set<NodeJS.Timeout>();
    const after = (ms: number, fn: () => void) => timers.add(setTimeout(fn, ms));

    const result = (): ExecResult => {
      if (overflow) return { code: outputLimitCode, stdout: '', stderr: stderr + `\n${overflow} 超过 ${maxOutput} 字节，已终止命令`, end: 'overflow' };
      if (ending === 'timeout') return { code: timeoutCode, stdout, stderr: stderr + `\n超时（${opts.timeoutMs} ms）`, end: 'timeout' };
      if (ending === 'abort') return { code: notRunCode, stdout, stderr: stderr + '\n' + abortMessage, end: 'abort' };
      // 被信号终止（不是 nodeExec 终止的）：没有退出码，按 shell 的约定返回 128 + 信号编号并带上信号，不能当成退出码 1
      if (exitCode === null) {
        const sig = exitSignal ?? undefined;
        return { code: 128 + (sig ? (constants.signals[sig] ?? 0) : 0), stdout, stderr: stderr + `\n被信号 ${sig ?? '（未知）'} 终止`, signal: sig, end: 'signal' };
      }
      return { code: exitCode, stdout, stderr, end: 'exit' };
    };
    const settle = (r: ExecResult = result()) => {
      if (settled) return;
      settled = true;
      for (const t of timers) clearTimeout(t);
      signal?.removeEventListener('abort', onAbort);
      // 孙进程可能还占着管道：不再等 close，也不让它们拖住事件循环
      child.stdout.destroy();
      child.stderr.destroy();
      resolve(r);
    };
    const terminate = (reason: 'timeout' | 'abort' | 'overflow') => {
      if (settled || ending) return;
      ending = reason;
      if (exited) {
        settle();
        return;
      }
      child.kill('SIGTERM');
      after(opts.killGraceMs ?? defaultKillGraceMs, () => {
        if (!exited) child.kill('SIGKILL');
        // 不依赖之后一定还有 exit：SIGKILL 后最多再等 drainMs
        after(drainMs, () => settle());
      });
    };
    const onAbort = () => terminate('abort');

    if (opts.timeoutMs) after(opts.timeoutMs, () => terminate('timeout'));
    signal?.addEventListener('abort', onAbort, { once: true });
    const collect = (stream: 'stdout' | 'stderr') => (d: string) => {
      if (overflow) return;
      sizes[stream] += Buffer.byteLength(d);
      if (sizes[stream] > maxOutput) {
        overflow = stream;
        terminate('overflow');
        return;
      }
      if (stream === 'stdout') stdout += d;
      else stderr += d;
    };
    child.stdout.setEncoding('utf8').on('data', collect('stdout'));
    child.stderr.setEncoding('utf8').on('data', collect('stderr'));
    child.on('error', (err) => {
      // 没有 pid 是没能启动；其它错误（例如发信号失败）不代表进程已经结束，照常等 exit 或定时器
      if (child.pid === undefined) settle({ code: notRunCode, stdout, stderr: stderr + String(err), end: 'notrun' });
    });
    child.on('exit', (code, sig) => {
      exited = true;
      exitCode = code;
      exitSignal = sig;
      after(drainMs, () => settle());
    });
    child.on('close', () => settle());
  });

/** must 执行命令，非 0 退出时抛出 ExecError，返回去掉首尾空白的 stdout。 */
export async function must(exec: Exec, cmd: string, args: readonly string[], opts?: ExecOptions): Promise<string> {
  const r = await exec(cmd, args, opts);
  if (r.code !== 0) throw new ExecError(cmd, args, r);
  return r.stdout.trim();
}
