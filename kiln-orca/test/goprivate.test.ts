import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import http from 'node:http';
import type { AddressInfo } from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { afterEach, beforeAll, describe, expect, it } from 'vitest';
import { execGitEnv, execGitOf } from '../src/core/credentials.js';
import { gitEnvConfig } from '../src/core/git.js';
import { commitFile, git } from './harness.js';

// 声明的依赖仓库与 go.mod 中的模块路径大小写不同（GitHub 的仓库名不区分大小写）时，task exec 设置的 GOPRIVATE 仍要让
// go 把它当作私有模块：直接用 git 获取，不发往模块代理（PLAN #33.13）。用真实的 go 命令验证：模块代理是记录请求、一律
// 404 的替身，模块的 git 仓库是本地裸仓库（git 的 insteadOf 把 https://github.com/… 映射过去），不访问网络。
//
// 需要 go 命令：开发环境本来就有（AGENTS.md #4），CI 的 kiln-orca job 声明了 runtimes: [pnpm, go]。找不到时测试失败并说明，
// 不跳过（跳过的通过不算数）。

let root = '';
afterEach(async () => {
  if (root) await rm(root, { recursive: true, force: true });
  root = '';
});

let goBin = '';
beforeAll(async () => {
  const r = await run('/bin/sh', ['-c', 'command -v go'], { env: process.env, cwd: os.tmpdir() });
  goBin = r.stdout.trim();
});

interface Result {
  code: number | null;
  stdout: string;
  stderr: string;
}

function run(cmd: string, args: string[], o: { env: NodeJS.ProcessEnv; cwd: string }): Promise<Result> {
  return new Promise((resolve, reject) => {
    const c = spawn(cmd, args, { env: o.env, cwd: o.cwd, stdio: ['ignore', 'pipe', 'pipe'] });
    let stdout = '';
    let stderr = '';
    c.stdout.setEncoding('utf8').on('data', (d: string) => (stdout += d));
    c.stderr.setEncoding('utf8').on('data', (d: string) => (stderr += d));
    c.on('error', reject);
    c.on('close', (code) => resolve({ code, stdout, stderr }));
  });
}

/** proxy 是模块代理的替身：记下请求的路径，一律 404。 */
async function proxy(): Promise<{ url: string; requests: string[]; close(): Promise<void> }> {
  const requests: string[] = [];
  const srv = http.createServer((req, res) => {
    requests.push(req.url ?? '');
    res.writeHead(404, { 'Content-Type': 'text/plain' });
    res.end('not found');
  });
  await new Promise<void>((r) => srv.listen(0, '127.0.0.1', r));
  return {
    url: `http://127.0.0.1:${(srv.address() as AddressInfo).port}`,
    requests,
    close: () =>
      new Promise((r) => {
        srv.closeAllConnections();
        srv.close(() => r());
      }),
  };
}

/** moduleRepo 建模块 modulePath 的裸仓库（打了 tag v0.1.0），返回它的路径。 */
async function moduleRepo(modulePath: string): Promise<string> {
  const work = path.join(root, 'lib-work');
  const bare = path.join(root, 'lib.git');
  await git(root, 'init', '--quiet', '-b', 'main', work);
  await writeFile(path.join(work, 'go.mod'), `module ${modulePath}\n\ngo 1.21\n`);
  await commitFile(work, 'lib.go', 'package lib\n\nconst Name = "lib"\n', 'init');
  await git(work, 'tag', 'v0.1.0');
  await git(root, 'clone', '--quiet', '--bare', work, bare);
  return bare;
}

describe('task exec 的 GOPRIVATE', () => {
  it.each([
    { declared: 'Acme/Lib', modulePath: 'github.com/acme/lib' },
    { declared: 'acme/lib', modulePath: 'github.com/Acme/Lib' },
  ])('声明 $declared、模块路径 $modulePath：模块不走代理，直接从 git 下载成功', { timeout: 120_000 }, async ({ declared, modulePath }) => {
    if (!goBin) throw new Error('找不到 go 命令：这个测试用真实的 go 验证 GOPRIVATE（CI 的 kiln-orca job 声明了 runtimes: go；本机请安装 Go）');
    root = await mkdtemp(path.join(os.tmpdir(), 'ko-goprivate-'));
    const bare = await moduleRepo(modulePath);
    const p = await proxy();
    try {
      const work = path.join(root, 'work');
      await mkdir(work, { recursive: true });
      // 只给 go 需要的环境：不读本机的 go env、git 全局配置，缓存都在临时目录中
      const base: NodeJS.ProcessEnv = {
        PATH: process.env.PATH,
        HOME: path.join(root, 'home'),
        GOENV: 'off',
        GOTOOLCHAIN: 'local',
        GO111MODULE: 'on',
        GOFLAGS: '-modcacherw',
        GOPATH: path.join(root, 'gopath'),
        GOMODCACHE: path.join(root, 'modcache'),
        GOCACHE: path.join(root, 'gocache'),
        GOPROXY: p.url,
        GOSUMDB: 'off',
        GIT_CONFIG_GLOBAL: '/dev/null',
        GIT_CONFIG_NOSYSTEM: '1',
      };
      // 与 kiln-orca task exec 相同的设置（任务仓库 acme/widgets 在 github.com 上，声明了依赖仓库）
      const g = execGitOf('https://github.com/acme/widgets.git', [declared], path.join(root, 'git-exec'));
      // file:// 不经过远程助手：GIT_EXEC_PATH 用 git 自己的目录
      const withGit = { ...base, ...execGitEnv(base, g!, 'false', await git(root, '--exec-path')) };
      // 测试专用：把模块的 GitHub 地址映射到本地裸仓库
      const env = { ...withGit, ...gitEnvConfig(withGit, [[`url.file://${bare}.insteadOf`, `https://${modulePath}`], ['protocol.file.allow', 'always']]) };
      const r = await run(goBin, ['mod', 'download', '-json', `${modulePath}@v0.1.0`], { env, cwd: work });
      // 没有发往代理：代理替身没有收到请求，输出中也没有 go 访问代理的报错（"reading <代理地址>/…: 404 Not Found"）。
      // 按带代理地址的完整特征串断言，不按 "404" 这样的短串：输出里有随机生成的提交哈希，短串会碰巧出现在其中。
      expect(p.requests).toEqual([]);
      expect(r.stderr + r.stdout).not.toContain(`reading ${p.url}/`);
      expect(r.code).toBe(0);
      expect(JSON.parse(r.stdout)).toMatchObject({ Path: modulePath, Version: 'v0.1.0' });
    } finally {
      await p.close();
    }
  });
});
