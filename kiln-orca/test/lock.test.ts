import { spawn, type ChildProcess } from 'node:child_process';
import { existsSync } from 'node:fs';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, describe, expect, it } from 'vitest';
import { lockFile, lockHolder } from '../src/host/lock.js';
import { waitFor } from './harness.js';

// 单实例锁（src/host/lock.ts）：真实的多个 Node 子进程竞争同一把锁。

const here = path.dirname(fileURLToPath(import.meta.url));
const lockModule = path.join(here, '..', 'src', 'host', 'lock.ts');
const child = path.join(here, 'lockchild.ts');

interface ChildResult {
  name: string;
  pid: number;
  acquired: boolean;
  busy?: boolean;
  error?: string;
}

let dir = '';
let children: ChildProcess[] = [];
afterEach(async () => {
  if (dir) await writeFile(path.join(dir, 'ctl', 'release'), '').catch(() => undefined);
  await Promise.all(children.map((c) => (c.exitCode === null && c.signalCode === null ? new Promise((r) => c.once('exit', r)) : undefined)));
  children = [];
  if (dir) await rm(dir, { recursive: true, force: true });
  dir = '';
});

async function setup(): Promise<{ lock: string; ctl: string }> {
  dir = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-lock-'));
  const ctl = path.join(dir, 'ctl');
  await mkdir(ctl);
  return { lock: path.join(dir, 'kiln-orca.pid'), ctl };
}

function start(lock: string, ctl: string, name: string, pause = 'none'): ChildProcess {
  const c = spawn(process.execPath, [child, lockModule, lock, ctl, name, pause], { stdio: ['ignore', 'ignore', 'pipe'] });
  let err = '';
  c.stderr?.on('data', (d: Buffer) => (err += d.toString()));
  c.once('exit', (code) => {
    if (code !== 0) console.error(`锁测试子进程 ${name} 退出码 ${code}：${err}`);
  });
  children.push(c);
  return c;
}

async function result(ctl: string, name: string): Promise<ChildResult> {
  const f = path.join(ctl, `${name}.result`);
  await waitFor(() => existsSync(f), 20_000, `${name} 的结果`);
  return JSON.parse(await readFile(f, 'utf8')) as ChildResult;
}

/** deadPid 返回一个已经退出的进程的 pid。 */
async function deadPid(): Promise<number> {
  const c = spawn(process.execPath, ['-e', '']);
  const pid = c.pid ?? 0;
  await new Promise((r) => c.once('exit', r));
  return pid;
}

const names = (n: number) => Array.from({ length: n }, (_, i) => `c${i}`);

describe('单实例锁', () => {
  it('N 个进程同时抢一把不存在的锁：恰好一个取得，锁文件任何时候都是完整的', { timeout: 60_000 }, async () => {
    const { lock, ctl } = await setup();
    const ns = names(8);
    for (const n of ns) start(lock, ctl, n);
    // 所有子进程就绪后同时开始；同时不停地读锁文件，读到的内容只能是不存在或完整的
    await new Promise((r) => setTimeout(r, 500));
    await writeFile(path.join(ctl, 'go'), '');
    const seen = new Set<string>();
    let done = false;
    const reader = (async () => {
      while (!done) {
        const t = await readFile(lock, 'utf8').catch(() => undefined);
        if (t !== undefined) seen.add(t);
        await new Promise((r) => setImmediate(r));
      }
    })();
    const rs = await Promise.all(ns.map((n) => result(ctl, n)));
    done = true;
    await reader;
    expect(rs.filter((r) => r.acquired)).toHaveLength(1);
    expect(rs.filter((r) => !r.acquired).every((r) => r.busy)).toBe(true);
    const holder = rs.find((r) => r.acquired);
    for (const t of seen) expect(JSON.parse(t)).toMatchObject({ pid: holder?.pid });
  });

  it('N 个进程同时接管死进程留下的锁：恰好一个取得', { timeout: 60_000 }, async () => {
    const { lock, ctl } = await setup();
    await writeFile(lock, JSON.stringify({ pid: await deadPid(), token: 'dead' }) + '\n');
    const ns = names(8);
    for (const n of ns) start(lock, ctl, n);
    await new Promise((r) => setTimeout(r, 500));
    await writeFile(path.join(ctl, 'go'), '');
    const rs = await Promise.all(ns.map((n) => result(ctl, n)));
    expect(rs.filter((r) => r.acquired)).toHaveLength(1);
    expect(rs.filter((r) => !r.acquired).every((r) => r.busy)).toBe(true);
    expect(JSON.parse(await readFile(lock, 'utf8'))).toMatchObject({ pid: rs.find((r) => r.acquired)?.pid });
  });

  it('锁文件刚出现、持有者还没返回时（审计复现的交错）：另一个进程看到完整的锁，拒绝', { timeout: 30_000 }, async () => {
    const { lock, ctl } = await setup();
    start(lock, ctl, 'a', 'after-link');
    await writeFile(path.join(ctl, 'go'), '');
    await waitFor(() => existsSync(path.join(ctl, 'a.paused')), 10_000, 'a 暂停');
    start(lock, ctl, 'b');
    const b = await result(ctl, 'b');
    expect(b).toMatchObject({ acquired: false, busy: true });
    await writeFile(path.join(ctl, 'a.resume'), '');
    expect(await result(ctl, 'a')).toMatchObject({ acquired: true });
  });

  it('内容写好、还没 link 时暂停：另一个进程先取得，暂停的进程恢复后拒绝', { timeout: 30_000 }, async () => {
    const { lock, ctl } = await setup();
    start(lock, ctl, 'a', 'before-link');
    await writeFile(path.join(ctl, 'go'), '');
    await waitFor(() => existsSync(path.join(ctl, 'a.paused')), 10_000, 'a 暂停');
    // 锁文件还不存在（没有半成品）
    expect(existsSync(lock)).toBe(false);
    start(lock, ctl, 'b');
    expect(await result(ctl, 'b')).toMatchObject({ acquired: true });
    await writeFile(path.join(ctl, 'a.resume'), '');
    expect(await result(ctl, 'a')).toMatchObject({ acquired: false, busy: true });
  });

  it('读到失效的锁后暂停、期间别人已经接管：恢复后不删除别人的锁，拒绝', { timeout: 30_000 }, async () => {
    const { lock, ctl } = await setup();
    await writeFile(lock, JSON.stringify({ pid: await deadPid(), token: 'dead' }) + '\n');
    start(lock, ctl, 'a', 'after-read');
    await writeFile(path.join(ctl, 'go'), '');
    await waitFor(() => existsSync(path.join(ctl, 'a.paused')), 10_000, 'a 暂停');
    start(lock, ctl, 'b');
    const b = await result(ctl, 'b');
    expect(b).toMatchObject({ acquired: true });
    const held = await readFile(lock, 'utf8');
    await writeFile(path.join(ctl, 'a.resume'), '');
    expect(await result(ctl, 'a')).toMatchObject({ acquired: false, busy: true });
    expect(await readFile(lock, 'utf8')).toBe(held);
  });

  it('接管者确认锁仍是失效的、删除之前暂停：另一个进程等它接管完，不会也删除、也取得', { timeout: 30_000 }, async () => {
    const { lock, ctl } = await setup();
    await writeFile(lock, JSON.stringify({ pid: await deadPid(), token: 'dead' }) + '\n');
    start(lock, ctl, 'a', 'before-rm');
    await writeFile(path.join(ctl, 'go'), '');
    await waitFor(() => existsSync(path.join(ctl, 'a.paused')), 10_000, 'a 暂停');
    start(lock, ctl, 'b');
    // b 在 a 接管期间反复尝试；给它足够的时间（如果没有接管锁，它会删掉失效的锁并取得）
    await new Promise((r) => setTimeout(r, 1_000));
    await writeFile(path.join(ctl, 'a.resume'), '');
    const rs = [await result(ctl, 'a'), await result(ctl, 'b')];
    expect(rs.filter((r) => r.acquired)).toHaveLength(1);
    expect(rs.find((r) => !r.acquired)).toMatchObject({ busy: true });
  });

  it('同一进程内：活着的进程拒绝，残留的锁接管，内容无效时报错不删除，释放只删自己的', async () => {
    const { lock } = await setup();
    await writeFile(lock, JSON.stringify({ pid: process.ppid, token: 'x' }));
    expect(() => lockFile(lock)).toThrow(/正在使用/);
    await writeFile(lock, JSON.stringify({ pid: await deadPid(), token: 'dead' }));
    const release = lockFile(lock);
    expect(JSON.parse(await readFile(lock, 'utf8'))).toMatchObject({ pid: process.pid });
    release();
    expect(existsSync(lock)).toBe(false);
    // 空文件、旧格式不是本协议写的：报错，不当作失效删除；pid 不是正整数（0、-1 会命中整个进程组）同样不是本协议写的（审计）
    for (const bad of ['', '12345', JSON.stringify({ pid: 0, token: 'x' }), JSON.stringify({ pid: -1, token: 'x' }), JSON.stringify({ pid: 1.5, token: 'x' })]) {
      await writeFile(lock, bad);
      expect(() => lockFile(lock)).toThrow(/内容无效/);
      expect(await readFile(lock, 'utf8')).toBe(bad);
    }
  });

  it('lockHolder：只报告活着的别的进程；没有锁、失效、内容无效、自己持有时为 undefined，不改动锁文件', async () => {
    const { lock } = await setup();
    expect(lockHolder(lock)).toBeUndefined();
    const live = JSON.stringify({ pid: process.ppid, token: 'x' });
    await writeFile(lock, live);
    expect(lockHolder(lock)).toBe(process.ppid);
    expect(await readFile(lock, 'utf8')).toBe(live);
    const dead = JSON.stringify({ pid: await deadPid(), token: 'dead' });
    await writeFile(lock, dead);
    expect(lockHolder(lock)).toBeUndefined();
    expect(await readFile(lock, 'utf8')).toBe(dead);
    await writeFile(lock, '12345');
    expect(lockHolder(lock)).toBeUndefined();
    await writeFile(lock, JSON.stringify({ pid: 0, token: 'x' }));
    expect(lockHolder(lock)).toBeUndefined();
    await rm(lock);
    const release = lockFile(lock);
    expect(lockHolder(lock)).toBeUndefined();
    release();
  });
});
