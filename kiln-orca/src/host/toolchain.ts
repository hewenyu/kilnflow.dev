import { accessSync, constants as fsConstants } from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import { goArch } from '../core/platform.js';
import type { Locator } from '../core/toolchain.js';

// kiln-runner 从哪里来（PLAN #33.12）：kiln-orca 的发布包把本平台的 @yuebanlaosiji/kilnflow-runner-<os>-<arch>（release.yml 已经
// 为 CI Runner 的自升级发布的包，包中只有这一个二进制）列为 optionalDependencies，版本在发布时与 kiln-orca 对齐。npm 只装
// 与本机 os/cpu 一致的那个；没有这个平台的包、或安装时跳过了，节点上报不支持工具链（不派带 tools 的任务），不影响别的任务。
// config.json 的 kiln_runner 可以指定路径（开发时、或者用本机已有的 kiln-runner），指定了就只用它。

/** runnerScope 是 kiln-runner 的 npm 包的 scope（internal/upgrade.Scope）。 */
export const runnerScope = '@yuebanlaosiji';

/** runnerPackagePrefix 是发布包名的前缀（internal/upgrade.PackagePrefix）：二进制 kiln-runner 发布为 kilnflow-runner-<os>-<arch>。 */
export const runnerPackagePrefix = 'kilnflow';

/** runnerPackage 是本平台的 kiln-runner 包名：@yuebanlaosiji/kilnflow-runner-<os>-<arch>（Go 的写法，x64 写作 amd64）。 */
export function runnerPackage(platform: string = process.platform, arch: string = process.arch): string {
  return `${runnerScope}/${runnerPackagePrefix}-runner-${platform}-${goArch(arch)}`;
}

/** runnerNotFound 是找不到 kiln-runner 时的说明（处理方法）。 */
export function runnerNotFound(pkg: string): string {
  return `没有找到 kiln-runner（${pkg} 没有随 kiln-orca 安装：这个平台没有发布，或安装时跳过了可选依赖）：重新安装 kiln-orca（npm install -g @yuebanlaosiji/kilnflow-orca，不要加 --omit=optional），或在 config.json 的 kiln_runner 中指定路径`;
}

/**
 * kilnRunnerLocator 返回找 kiln-runner 的函数：configured 不为空时只用它（必须可执行）；否则从 kiln-orca 自己的位置解析
 * 本平台的 kiln-runner 包（resolveFrom 是解析的起点，默认这个模块）。每次调用重新查找：重新安装 kiln-orca 之后不用重启。
 */
export function kilnRunnerLocator(configured: string, resolveFrom: string = import.meta.url, pkg: string = runnerPackage()): Locator {
  return () => {
    if (configured !== '') {
      return executable(configured) ? { bin: configured, reason: '' } : { reason: `config.json 中的 kiln_runner（${configured}）不存在或不能执行` };
    }
    let dir: string;
    try {
      dir = path.dirname(createRequire(resolveFrom).resolve(`${pkg}/package.json`));
    } catch {
      return { reason: runnerNotFound(pkg) };
    }
    const bin = path.join(dir, 'kiln-runner');
    return executable(bin) ? { bin, reason: '' } : { reason: `${bin} 不存在或不能执行：重新安装 kiln-orca` };
  };
}

function executable(p: string): boolean {
  try {
    accessSync(p, fsConstants.X_OK);
    return true;
  } catch {
    return false;
  }
}
