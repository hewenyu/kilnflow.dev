import { appendFileSync, mkdirSync } from 'node:fs';
import { appendFile, open, stat, truncate, utimes } from 'node:fs/promises';
import path from 'node:path';
import { formatLine, type Logger } from '../core/log.js';
import { removeOlderThan } from '../core/journal.js';

// 连接器自身的日志（PLAN #33.3）：按天写 logs/kiln-orca-<日期>.log，保留 7 天。前台运行（kiln-orca run）同时写 stderr，
// 方便在终端里看；作为服务运行（run --service，服务定义里带这个参数）时正常日志只写文件，stderr 只留给启动失败、崩溃和
// "写日志文件失败"时的兜底。
//
// launchd 把服务的 stdout / stderr 追加到固定的 logs/service.out.log、service.err.log（systemd 进 journald，由它轮转）。
// 这两个文件由服务管理器打开（O_APPEND），不能删除或改名（运行中的进程会继续写进已经删除的文件，崩溃输出就丢了），
// 所以只原地截断：清理日志时（启动时和之后每天）把其中的内容移进它最后写入那天的按日文件（超过 serviceLogMaxBytes
// 时只留末尾），再截断为空；最后写入已经超过保留期的直接截断。这样超过保留期的内容随按日文件一起删除，崩溃循环也不会
// 让它们无限增长。

/** logRetentionMs 是连接器自身日志的保留时间（服务日志 7 天，C10）。 */
export const logRetentionMs = 7 * 24 * 60 * 60_000;
/** serviceLogs 是服务管理器（launchd）写的 stdout、stderr 文件名。 */
export const serviceLogs = { out: 'service.out.log', err: 'service.err.log' } as const;
/** serviceLogMaxBytes：服务管理器写的文件移进按日文件时最多保留的末尾字节数。 */
export const serviceLogMaxBytes = 1 << 20;

/** dailyLog 是 t 那天的按日日志文件名。 */
export function dailyLog(t: Date): string {
  return `kiln-orca-${t.toISOString().slice(0, 10)}.log`;
}

export interface FileLoggerOptions {
  /** 同时写 stderr（前台运行）。为 false 时只在写文件失败时写 stderr。 */
  console: boolean;
  /** stderr 的写入（测试中替换）。 */
  stderr?: (line: string) => void;
}

/** fileLogger 写到 dir/kiln-orca-<日期>.log（按天轮转）；console 为 true 时同时写 stderr，否则只在写文件失败时写。 */
export function fileLogger(dir: string, o: FileLoggerOptions): Logger {
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const stderr = o.stderr ?? ((line: string) => void process.stderr.write(line));
  const write = (level: string, msg: string, fields?: Record<string, unknown>) => {
    const now = new Date();
    const line = formatLine(level, msg, fields, now);
    let written = true;
    try {
      appendFileSync(path.join(dir, dailyLog(now)), line, { mode: 0o600 });
    } catch {
      written = false;
    }
    if (o.console || !written) stderr(line);
  };
  return {
    info: (m, f) => write('INFO', m, f),
    warn: (m, f) => write('WARN', m, f),
    error: (m, f) => write('ERROR', m, f),
  };
}

/**
 * readTail 读取 file 最后最多 max 字节，返回读到的末尾和前面没有读的字节数。只读末尾：崩溃循环可能让服务管理器写的文件
 * 很大，不按它的大小分配内存。
 */
async function readTail(file: string, max: number): Promise<{ kept: Buffer; dropped: number }> {
  const fh = await open(file, 'r');
  try {
    const size = (await fh.stat()).size;
    const start = Math.max(0, size - max);
    const buf = Buffer.alloc(size - start);
    let n = 0;
    while (n < buf.length) {
      const { bytesRead } = await fh.read(buf, n, buf.length - n, start + n);
      if (bytesRead === 0) break;
      n += bytesRead;
    }
    return { kept: buf.subarray(0, n), dropped: start };
  } finally {
    await fh.close();
  }
}

/** pruneLogs 删除超过保留期的按日文件，并整理服务管理器写的文件（见文件开头）。 */
export async function pruneLogs(dir: string, now = Date.now()): Promise<void> {
  const cutoff = now - logRetentionMs;
  await removeOlderThan(dir, cutoff, new Set(Object.values(serviceLogs)));
  for (const name of Object.values(serviceLogs)) await rotateServiceLog(dir, name, cutoff);
}

async function rotateServiceLog(dir: string, name: string, cutoff: number): Promise<void> {
  const file = path.join(dir, name);
  const st = await stat(file).catch(() => undefined);
  if (!st || st.size === 0) return;
  if (st.mtimeMs >= cutoff) {
    const { kept, dropped } = await readTail(file, serviceLogMaxBytes);
    const header = formatLine('WARN', `服务管理器记录的输出（${name}：启动失败、崩溃或写日志文件失败时的输出）`, dropped > 0 ? { dropped_bytes: dropped } : undefined, st.mtime);
    const tail = kept.length > 0 && kept[kept.length - 1] !== 0x0a ? '\n' : '';
    const daily = path.join(dir, dailyLog(st.mtime));
    const prev = await stat(daily).catch(() => undefined);
    await appendFile(daily, header + kept.toString('utf8') + tail, { mode: 0o600 });
    // 追加不能延长按日文件的保留期（按修改时间删除）：修改时间仍是这一天最后一条内容的时间
    const last = new Date(Math.max(prev?.mtimeMs ?? 0, st.mtimeMs));
    await utimes(daily, last, last);
  }
  // 原地截断：服务管理器打开的文件描述符继续有效（O_APPEND，之后的写入从开头开始）
  await truncate(file, 0);
}
