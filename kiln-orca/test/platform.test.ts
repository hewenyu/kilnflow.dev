import { describe, expect, it } from 'vitest';
import { goArch, runnerPlatform } from '../src/core/platform.js';
import { defaultOrcaBin, defaultOrcaBins } from '../src/host/home.js';
import { runnerPackage } from '../src/host/toolchain.js';

// 平台字符串必须与 Server 的 agentcfg.Platform.RunnerPlatform 一致（Go 的写法），否则 Linux 节点领不到任务
describe('runnerPlatform', () => {
  it('Node 的 x64 写作 amd64，arm64 不变', () => {
    expect(goArch('x64')).toBe('amd64');
    expect(goArch('arm64')).toBe('arm64');
    expect(runnerPlatform('linux', 'x64')).toBe('linux/amd64');
    expect(runnerPlatform('linux', 'arm64')).toBe('linux/arm64');
    expect(runnerPlatform('darwin', 'arm64')).toBe('darwin/arm64');
  });

  it('kiln-runner 的平台包名用同一个写法（release.yml 发布的 kiln-runner-<os>-<arch>）', () => {
    expect(runnerPackage('linux', 'x64')).toBe('@yuebanlaosiji/kilnflow-runner-linux-amd64');
    expect(runnerPackage('linux', 'arm64')).toBe('@yuebanlaosiji/kilnflow-runner-linux-arm64');
    expect(runnerPackage('darwin', 'arm64')).toBe('@yuebanlaosiji/kilnflow-runner-darwin-arm64');
  });
});

// 没有配置 orca 时的 Orca 命令行：Linux 上叫 orca-ide（裸的 orca 通常是屏幕阅读器）。（PLAN #33.3）
describe('Orca 命令行的默认值', () => {
  it('Linux 默认用 orca-ide，macOS 先找 Orca.app 中的', () => {
    expect(defaultOrcaBins('linux')).toEqual(['orca-ide']);
    expect(defaultOrcaBin('linux')).toBe('orca-ide');
    expect(defaultOrcaBins('darwin')).toEqual(['/Applications/Orca.app/Contents/Resources/bin/orca', 'orca']);
  });
});
