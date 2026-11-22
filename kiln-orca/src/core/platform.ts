// 本机平台的写法（PLAN #33.3）：Kiln 的平台字符串（AIRunnerState.platform、注册时的 platforms）和 kiln-runner 的 npm 包名
// 都用 Go 的写法（GOOS/GOARCH，例如 linux/amd64、darwin/arm64），与 Server 的 agentcfg.Platform.RunnerPlatform 一致；
// Server 按它与任务的平台匹配，写错了节点就领不到任务。Node 的写法不同的只有 x64（Go 写作 amd64）。

/** goArch 把 Node 的架构名称（process.arch）换成 Go 的写法：x64 → amd64，其余（arm64）相同。 */
export function goArch(arch: string): string {
  return arch === 'x64' ? 'amd64' : arch;
}

/** runnerPlatform 返回本机的平台字符串 <os>/<arch>（Go 的写法），参数默认为本机（测试中指定）。 */
export function runnerPlatform(platform: string = process.platform, arch: string = process.arch): string {
  return `${platform}/${goArch(arch)}`;
}
