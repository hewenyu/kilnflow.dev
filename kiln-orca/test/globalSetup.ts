import { buildCli, cliBuildEnv } from './clibuild.js';

/**
 * globalSetup：整个 pnpm test 周期只编译一次 kiln-orca 命令行（cli.test.ts、transport.test.ts 都要真实运行它），
 * 路径经环境变量传给各测试进程；全部测试结束后删除构建目录。
 */
export default async function setup() {
  // 不理会环境里已有的值（可能是上一次运行留下的）：globalSetup 总是自己编译一份。
  delete process.env[cliBuildEnv];
  const build = await buildCli();
  process.env[cliBuildEnv] = build.cli;
  return build.cleanup;
}
