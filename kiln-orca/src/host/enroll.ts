import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import os from 'node:os';
import { createClient, type Transport } from '@connectrpc/connect';
import { RunnerService } from '../gen/kiln/runner/v1/runner_pb.js';
import { publicKey } from '../core/kiln.js';
import { runnerPlatform } from '../core/platform.js';
import type { Exec } from '../core/exec.js';
import type { Home, State } from './home.js';

/** runnerKind 是 AI 执行节点的 Runner 类型（注册令牌决定）。 */
export const runnerKind = 'ai';

/**
 * enroll 用一次性令牌注册（name 为空时用主机名），保存注册信息（state.json）和配置中的 Server 地址、名称，并清除之前的吊销
 * 状态。私钥沿用；Orca 中的邮箱（mailbox.json）属于运行中的连接器，enroll 不碰它（数据目录的写入者见 home.ts）。
 * 令牌不是 AI 执行节点的令牌时不保存。
 */
export async function enroll(home: Home, transport: Transport, server: string, token: string, name: string, version: string): Promise<State> {
  name ||= os.hostname().split('.')[0] ?? 'kiln-orca';
  const seed = await home.key(true);
  const client = createClient(RunnerService, transport);
  const r = await client.enroll({ token, name, publicKey: publicKey(seed), state: { version, platforms: [runnerPlatform()] } });
  const kind = r.kind || 'ci';
  if (kind !== runnerKind) throw new Error(`这个注册令牌是给 ${kind === 'ci' ? 'CI Runner（kiln-runner）' : kind} 用的，kiln-orca 需要 AI 执行节点的令牌`);
  const st: State = { server, runnerId: String(r.runnerId), name, role: r.role };
  await home.saveState(st);
  await home.saveConfig({ ...(await home.loadConfig()), server, name });
  await home.clearRevoked();
  return st;
}

/** hostId 返回本机的稳定标识（硬件 UUID 或 machine-id 的 sha256），Server 用它识别同一台机器上的多个节点。 */
export async function hostId(exec: Exec): Promise<string> {
  let raw = '';
  if (process.platform === 'darwin') {
    const r = await exec('ioreg', ['-rd1', '-c', 'IOPlatformExpertDevice'], { timeoutMs: 10_000 });
    raw = /"IOPlatformUUID" = "([^"]+)"/.exec(r.stdout)?.[1] ?? '';
  } else {
    for (const p of ['/etc/machine-id', '/var/lib/dbus/machine-id']) {
      raw = (await readFile(p, 'utf8').catch(() => '')).trim();
      if (raw) break;
    }
  }
  return createHash('sha256').update(raw || os.hostname()).digest('hex');
}
