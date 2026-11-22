import { afterEach, beforeEach } from 'vitest';
import { beginFaults, endFaults } from './harness.js';

// 每个测试一个 Faults（见 harness.ts）：钩子、Agent 脚本中的失败在测试结束时让测试失败，不会被产品代码吞掉。
beforeEach(() => beginFaults());
afterEach(() => endFaults());
