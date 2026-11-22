// 单实例锁测试的子进程（node 直接运行这个 .ts，类型擦除）：等 <控制目录>/go 出现后获取锁，把结果写到
// <控制目录>/<名字>.result；取得锁时一直持有到 <控制目录>/release 出现。pause 让子进程在锁协议的某一步暂停
// （写 <名字>.paused，等 <名字>.resume），用来固定竞争的交错：
//   after-link：锁文件刚 link 出来（已经持有，还没返回）；
//   before-link：完整内容已写进临时文件，还没 link；
//   after-read：第一次读到已存在的锁文件之后；
//   before-rm：第一次删除锁文件（接管失效的锁）之前。
import fs from 'node:fs';
import { syncBuiltinESMExports } from 'node:module';
import path from 'node:path';

const [lockModule, lockPath, control, name, pause = 'none'] = process.argv.slice(2);
if (!lockModule || !lockPath || !control || !name) throw new Error('参数：<lock 模块> <锁文件> <控制目录> <名字> [pause]');
const ctl = (f: string) => path.join(control, f);

function waitFile(f: string): void {
  while (!fs.existsSync(f)) Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 5);
}

/** report 原子地写结果（先写临时文件再改名）：父进程看到文件时内容是完整的。 */
function report(r: Record<string, unknown>): void {
  fs.writeFileSync(ctl(`${name}.result.tmp`), JSON.stringify({ name, pid: process.pid, ...r }));
  fs.renameSync(ctl(`${name}.result.tmp`), ctl(`${name}.result`));
}

function pauseHere(): void {
  fs.writeFileSync(ctl(`${name}.paused`), '');
  waitFile(ctl(`${name}.resume`));
}

const { linkSync, readFileSync, rmSync } = fs;
if (pause === 'after-link' || pause === 'before-link') {
  fs.linkSync = (src: fs.PathLike, dst: fs.PathLike) => {
    if (dst !== lockPath) return linkSync(src, dst);
    if (pause === 'before-link') pauseHere();
    linkSync(src, dst);
    if (pause === 'after-link') pauseHere();
  };
}
if (pause === 'after-read') {
  let paused = false;
  const patched = (p: fs.PathOrFileDescriptor, opts?: unknown): unknown => {
    const r: unknown = Reflect.apply(readFileSync, fs, [p, opts]);
    if (p === lockPath && !paused) {
      paused = true;
      pauseHere();
    }
    return r;
  };
  Reflect.set(fs, 'readFileSync', patched);
}
if (pause === 'before-rm') {
  let paused = false;
  fs.rmSync = (p: fs.PathLike, opts?: fs.RmOptions) => {
    if (p === lockPath && !paused) {
      paused = true;
      pauseHere();
    }
    rmSync(p, opts);
  };
}
syncBuiltinESMExports();

const { lockFile, LockBusyError } = (await import(lockModule)) as typeof import('../src/host/lock.js');

waitFile(ctl('go'));
let release: (() => void) | undefined;
try {
  release = lockFile(lockPath);
  report({ acquired: true });
} catch (err) {
  const busy = err instanceof LockBusyError;
  report({ acquired: false, busy, error: String(err) });
}
if (release) {
  waitFile(ctl('release'));
  release();
}
