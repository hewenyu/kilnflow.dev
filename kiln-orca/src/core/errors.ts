/** 上报给 Server 的错误分类（CompleteRequest.error_class）。 */
export const ErrorClass = {
  user: 'user',
  timeout: 'timeout',
  shutdown: 'infra.runner_shutdown',
  disconnected: 'infra.disconnected',
  prepare: 'infra.prepare',
  fetch: 'infra.fetch',
  /** 向 Server 要 git 凭据失败（PLAN #33.13）。 */
  credential: 'infra.credential',
  services: 'infra.services',
  /** 准备受管理的工具链失败（kiln-runner toolchain ensure，PLAN #33.12）。 */
  toolchain: 'infra.toolchain',
  /** 无法在 Orca 中启动或跟踪 Agent（本机退避这个 Agent）。 */
  agent: 'infra.agent',
  /** Orca Server 出错。 */
  orca: 'infra.orca',
  /** 无法确认 Orca 中的执行状态（不会再启动一份）。 */
  lost: 'infra.lost',
  push: 'infra.push',
  /** Agent 没有写出有效的报告或交付。 */
  report: 'ai.report',
  delivery: 'ai.delivery',
  /**
   * Orca 报告 Agent 运行中断（无法继续）：致命，但这是任务或仓库的问题，不是这台机器上的 Agent 不能用。收到中止记录的
   * Attempt 以它失败，不调用 agents.failed()，这台机器上的 Agent 因此不进入退避。
   */
  runtime: 'ai.runtime',
} as const;

/** InfraError 是带分类的基础设施错误。 */
export class InfraError extends Error {
  constructor(
    readonly errorClass: string,
    message: string,
    options?: { cause?: unknown },
  ) {
    super(message, options);
    this.name = 'InfraError';
  }
}

export function infra(errorClass: string, err: unknown): InfraError {
  if (err instanceof InfraError) return err;
  return new InfraError(errorClass, errMessage(err), { cause: err });
}

/** StaleError 表示 attempt 已经不属于本节点（epoch 过期、已结束或租约过期），应立即放弃、不再写任何东西。 */
export class StaleError extends Error {
  constructor() {
    super('attempt 已经失效（epoch 过期、已结束或租约过期）');
    this.name = 'StaleError';
  }
}

/** RevokedError 表示节点已被吊销。 */
export class RevokedError extends Error {
  constructor() {
    super('节点已被吊销');
    this.name = 'RevokedError';
  }
}

export function errMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
