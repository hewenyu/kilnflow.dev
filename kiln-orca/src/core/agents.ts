import { type Exec, notRun } from './exec.js';

// Agent 检测（PLAN #30.7、#33.4）：启动时和每 probeIntervalMs 用 --version 检测这个节点提供的 Agent，随 Acquire 上报；
// 不做付费调用。某个 Agent 的任务以 infra.agent 失败后，本机在 backoffMs 内不用它（上报 backoff_until）。

export const probeIntervalMs = 10 * 60_000;
export const backoffMs = 30 * 60_000;

/** AgentDef 是 Kiln 的 Agent 与 Orca 的 Agent id、可执行文件的对应。 */
export interface AgentDef {
  /** Kiln 中的名字。 */
  name: string;
  /** Orca 的 Agent id（orchestration worker-start --agent）。 */
  orca: string;
  /** 检测版本用的可执行文件。 */
  bin: string;
}

/** knownAgents 是 Kiln 认识的 Agent（agentcfg.Catalog）。 */
export const knownAgents: readonly AgentDef[] = [
  { name: 'codex', orca: 'codex', bin: 'codex' },
  { name: 'claude-code', orca: 'claude', bin: 'claude' },
];

export function agentDef(name: string): AgentDef | undefined {
  return knownAgents.find((a) => a.name === name);
}

export interface AgentState {
  name: string;
  installed: boolean;
  version: string;
  available: boolean;
  reason: string;
  checkedAt: number;
  backoffUntil: number;
}

/** AgentProbe 检测并记录提供的 Agent 的状态。 */
export class AgentProbe {
  private readonly states = new Map<string, AgentState>();
  private lastProbe = 0;

  constructor(
    private readonly exec: Exec,
    readonly offered: readonly AgentDef[],
    private readonly now: () => number = Date.now,
  ) {}

  async refresh(force = false): Promise<void> {
    const now = this.now();
    if (!force && this.lastProbe && now - this.lastProbe < probeIntervalMs) return;
    this.lastProbe = now;
    for (const a of this.offered) {
      const prev = this.states.get(a.name);
      const r = await this.exec(a.bin, ['--version'], { timeoutMs: 30_000 });
      const out = (r.stdout || r.stderr).trim().split('\n')[0] ?? '';
      const installed = !notRun(r);
      this.states.set(a.name, {
        name: a.name,
        installed,
        version: r.code === 0 ? out : '',
        available: r.code === 0,
        reason: r.code === 0 ? '' : installed ? `${a.bin} --version 失败：${out}` : `没有安装 ${a.bin}`,
        checkedAt: now,
        backoffUntil: prev?.backoffUntil ?? 0,
      });
    }
  }

  /** failed 记录 Agent 的任务以 infra.agent 失败：backoffMs 内不用它。 */
  failed(name: string): void {
    const s = this.states.get(name);
    if (s) s.backoffUntil = this.now() + backoffMs;
  }

  version(name: string): string {
    return this.states.get(name)?.version ?? '';
  }

  /** usable 判断 Agent 现在能不能用（安装、可用、不在退避中）。 */
  usable(name: string): boolean {
    const s = this.states.get(name);
    return !!s && s.available && s.backoffUntil <= this.now();
  }

  snapshot(): AgentState[] {
    const now = this.now();
    return [...this.states.values()].map((s) => ({ ...s, backoffUntil: s.backoffUntil > now ? s.backoffUntil : 0 }));
  }
}
