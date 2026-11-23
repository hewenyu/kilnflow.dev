import { copyFile, mkdtemp, rm, symlink } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// 编译后的 kiln-orca 命令行（与发布的包相同的布局）：作为真实的子进程运行的测试（cli.test.ts、transport.test.ts）共用。

const pkg = path.join(path.dirname(fileURLToPath(import.meta.url)), '..');

/** cliBuildEnv 是 globalSetup 编译好的命令行路径（<根>/dist/host/cli.js）。 */
export const cliBuildEnv = 'KILN_ORCA_TEST_CLI';

export interface CliBuild {
  /** cli 是编译出的 host/cli.js。 */
  cli: string;
  cleanup(): Promise<void>;
}

/**
 * buildCli 返回要运行的 kiln-orca 命令行：整个 pnpm test 周期用 globalSetup 编译好的那一份（路径在环境变量里）；
 * 没有时（单独运行这个文件、不经 Vitest 的 globalSetup）自己编译一份，并负责清理。
 */
export async function buildCli(): Promise<CliBuild> {
  const shared = process.env[cliBuildEnv];
  if (shared) return { cli: shared, cleanup: async () => {} };
  const build = await mkdtemp(path.join(os.tmpdir(), 'ko-cli-build-'));
  try {
    return { cli: await compileCli(build), cleanup: () => rm(build, { recursive: true, force: true }) };
  } catch (err) {
    await rm(build, { recursive: true, force: true });
    throw err;
  }
}

/** compileCli 把 kiln-orca 编译进 build：<根>/dist/host/cli.js 读 <根>/package.json，依赖从 <根>/node_modules 解析。 */
async function compileCli(build: string): Promise<string> {
  const out = path.join(build, 'dist');
  const tsc = await new Promise<{ code: number | null; text: string }>((resolve) => {
    const c = spawn(process.execPath, [path.join(pkg, 'node_modules', 'typescript', 'bin', 'tsc'), '-p', path.join(pkg, 'tsconfig.build.json'), '--outDir', out, '--declaration', 'false', '--sourceMap', 'false'], {
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let text = '';
    c.stdout.setEncoding('utf8').on('data', (d: string) => (text += d));
    c.stderr.setEncoding('utf8').on('data', (d: string) => (text += d));
    c.on('close', (code) => resolve({ code, text }));
  });
  if (tsc.code !== 0) throw new Error(`编译失败：${tsc.text}`);
  await copyFile(path.join(pkg, 'package.json'), path.join(build, 'package.json'));
  // skills/ 与发布的包相同：连接器按 <根>/skills/kiln-task 找随包分发的 skill
  await symlink(path.join(pkg, 'skills'), path.join(build, 'skills'), 'dir');
  await symlink(path.join(pkg, 'node_modules'), path.join(build, 'node_modules'), 'dir');
  return path.join(out, 'host', 'cli.js');
}
