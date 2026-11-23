import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    include: ['test/**/*.test.ts'],
    // 钩子与 Agent 脚本中的失败让测试失败（test/harness.ts 的 Faults）
    setupFiles: ['test/setup.ts'],
    // 编译后的命令行整个测试周期只构建一次（cli.test.ts、transport.test.ts 共用）
    globalSetup: ['test/globalSetup.ts'],
    // 测试用真实的 git 仓库和定时器
    testTimeout: 30_000,
  },
});
