import { randomBytes } from 'node:crypto';
import { linkSync, readFileSync, rmSync, writeFileSync } from 'node:fs';

// 单实例锁：数据目录中的 run/kiln-orca.pid。
//
// 锁的内容是一行 JSON {"pid":…,"token":…}，token 每次获取时随机生成，所以一份内容只属于一次获取（pid 被复用也不会混淆）。
//
// 持有：锁文件的内容是自己的这一份。获取时先把完整内容写进自己的临时文件，再 link 到锁文件（目标已存在时 link 失败）：
// 锁文件一出现就是完整的，不存在"已创建、还没写入"的中间状态，读到的内容不完整（空、不是这个格式）一定不是本协议写的，
// 报错让人处理，不当作失效删除。
//
// 接管：锁文件记录的进程已经不在（崩溃留下的）时，先用同样的 link 取得这份内容专属的接管锁 <锁文件>.takeover-<token>，
// 持有接管锁后重新读锁文件，内容仍是那份失效的内容才删除，然后回到获取（与新来的进程按 link 竞争，只有一个成功）。
// 只有接管锁的持有者会删除失效的锁文件，而失效的内容不会再变回来（token 唯一），所以不会删掉别人刚拿到的锁；
// 多个进程同时接管同一份失效内容时只有一个取得接管锁。接管锁的持有者本身崩溃时，它的接管锁按同样的规则（递归）接管。
// 接管锁用完即删；之后再有进程取得它，重新读到的锁文件已经不是那份失效内容，不会删除任何东西。
//
// 残余的风险：记录的 pid 被别的进程复用时，失效的锁看起来仍有进程持有，拒绝启动（安全的方向），需要人确认后删除锁文件；
// 锁只在同一个 pid 命名空间内有效（不要让不同容器、不同机器共用一个数据目录）；在获取的几个系统调用之间崩溃会留下临时文件
// 或接管锁（不影响正确性，不自动清理）。

interface Owner {
  pid: number;
  token: string;
}

/** lockBusyError 是另一个活着的进程持有锁。 */
export class LockBusyError extends Error {
  readonly pid: number;
  constructor(pid: number) {
    super(`另一个 kiln-orca（pid ${pid}）正在使用这个数据目录`);
    this.name = 'LockBusyError';
    this.pid = pid;
  }
}

const takeoverWaitMs = 50;
const acquireDeadlineMs = 10_000;

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return (err as NodeJS.ErrnoException).code === 'EPERM';
  }
}

function code(err: unknown): string | undefined {
  return (err as NodeJS.ErrnoException).code;
}

function sleepSync(ms: number): void {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

function newOwner(): Owner {
  return { pid: process.pid, token: randomBytes(12).toString('hex') };
}

function format(o: Owner): string {
  return JSON.stringify(o) + '\n';
}

function parse(p: string, text: string): Owner {
  try {
    const v = JSON.parse(text) as unknown;
    // pid 必须是正整数：0、-1 不可能是一个进程（process.kill(0) 还会命中整个进程组）
    if (typeof v === 'object' && v !== null && 'pid' in v && 'token' in v && Number.isInteger(v.pid) && Number(v.pid) > 0 && typeof v.token === 'string' && v.token) {
      return { pid: Number(v.pid), token: v.token };
    }
  } catch {
    // 下面报错
  }
  throw new Error(`锁文件 ${p} 的内容无效（${JSON.stringify(text.slice(0, 80))}）：确认没有 kiln-orca 在使用这个数据目录后删除它`);
}

/** read 返回锁文件的内容，不存在时返回 undefined。 */
function read(p: string): string | undefined {
  try {
    return readFileSync(p, 'utf8');
  } catch (err) {
    if (code(err) === 'ENOENT') return undefined;
    throw err;
  }
}

/** tryCreate 原子地创建内容为 o 的 p（先写临时文件再 link）：p 已存在时返回 false。 */
function tryCreate(p: string, o: Owner): boolean {
  const tmp = `${p}.${o.pid}.${o.token}.tmp`;
  writeFileSync(tmp, format(o), { mode: 0o600 });
  try {
    linkSync(tmp, p);
    return true;
  } catch (err) {
    if (code(err) === 'EEXIST') return false;
    throw err;
  } finally {
    rmSync(tmp, { force: true });
  }
}

/** stale 判断内容为 o 的锁是否失效（记录的进程已经不在；pid 是自己时是之前的自己留下的，例如容器中重启的 pid 1）。 */
function stale(o: Owner): boolean {
  return o.pid === process.pid || !alive(o.pid);
}

/**
 * removeStale 在 p 的内容仍是 staleText 时删除它，否则什么都不做。返回 false 表示另一个活着的进程正在接管（稍后重试）。
 * 只有取得接管锁 <p>.takeover-<token> 的进程会删除；接管锁本身失效时递归接管。
 */
function removeStale(p: string, staleText: string, staleOwner: Owner): boolean {
  const guard = `${p}.takeover-${staleOwner.token}`;
  const me = newOwner();
  for (;;) {
    if (tryCreate(guard, me)) {
      try {
        if (read(p) === staleText) rmSync(p, { force: true });
      } finally {
        rmSync(guard, { force: true });
      }
      return true;
    }
    const g = read(guard);
    if (g === undefined) continue;
    const go = parse(guard, g);
    if (!stale(go)) return false;
    if (!removeStale(guard, g, go)) return false;
  }
}

/** lockFile 获取锁，返回释放函数（只删除自己的锁）。另一个活着的进程持有时抛出 LockBusyError。 */
export function lockFile(p: string): () => void {
  const me = newOwner();
  const mine = format(me);
  const deadline = Date.now() + acquireDeadlineMs;
  while (Date.now() < deadline) {
    if (tryCreate(p, me)) {
      return () => {
        if (read(p) === mine) rmSync(p, { force: true });
      };
    }
    const text = read(p);
    if (text === undefined) continue;
    const o = parse(p, text);
    if (!stale(o)) throw new LockBusyError(o.pid);
    if (!removeStale(p, text, o)) sleepSync(takeoverWaitMs);
  }
  throw new Error(`无法获取锁 ${p}：另一个进程一直在接管`);
}

/** lockHolder 返回持有锁的活着的进程（不是自己）；没有锁、锁已失效或内容无效时返回 undefined。只读，不接管。 */
export function lockHolder(p: string): number | undefined {
  const text = read(p);
  if (text === undefined) return undefined;
  try {
    const o = parse(p, text);
    return stale(o) ? undefined : o.pid;
  } catch {
    return undefined;
  }
}
