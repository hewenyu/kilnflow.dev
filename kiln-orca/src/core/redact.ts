import type { Logger } from './log.js';

// 脱敏（PLAN #33.13）：Server 签发的凭据只在连接器的内存中，连接器的日志、上传给 Server 的任务日志、错误说明（Complete 的
// message）和给 Agent 的工具回复中都不能出现。签发到的密钥登记在节点共用的 Redactor 中（与 CI Runner 的 AddSecret 相同的
// 做法），写出之前统一替换；同时登记 HTTP Basic 认证中的形式（base64("用户名:密钥")），git 的输出中如果带出请求头也认得出。

/** redactedMark 是替换后的文字。 */
export const redactedMark = '***';
/** minSecretLength：短于它的字符串不登记（替换会误伤正常的文字；installation token 远比它长）。 */
const minSecretLength = 8;
/** expiredGraceMs：密钥过期之后再保留这么久才不再替换（过期的密钥已经没用，只是防止刚过期就出现在日志中）。 */
const expiredGraceMs = 60 * 60_000;

/** basicAuth 是 HTTP Basic 认证的凭据部分（base64("用户名:密钥")），git 的请求头中用它。 */
export function basicAuth(username: string, secret: string): string {
  return Buffer.from(`${username}:${secret}`, 'utf8').toString('base64');
}

/** Redactor 记下要替换的密钥；同一个连接器进程中的全部 attempt 共用一个。 */
export class Redactor {
  /** 密钥 → 过期时间（毫秒）。 */
  private readonly secrets = new Map<string, number>();

  constructor(private readonly now: () => number = Date.now) {}

  /** add 登记密钥（以及它在 Basic 认证中的形式），expiresAt 之后再过 expiredGraceMs 不再替换。 */
  add(secret: string, expiresAt: number, username?: string): void {
    this.prune();
    for (const s of [secret, username === undefined ? '' : basicAuth(username, secret)]) {
      if (s.length >= minSecretLength) this.secrets.set(s, Math.max(expiresAt, this.secrets.get(s) ?? 0));
    }
  }

  /** redact 替换 text 中登记过的密钥。 */
  redact(text: string): string {
    let out = text;
    for (const s of this.secrets.keys()) if (out.includes(s)) out = out.replaceAll(s, redactedMark);
    return out;
  }

  /** prune 去掉过期已久的密钥（集合不随运行时间无限增长）。 */
  private prune(): void {
    const cutoff = this.now() - expiredGraceMs;
    for (const [s, exp] of this.secrets) if (exp < cutoff) this.secrets.delete(s);
  }
}

/** circularMark 是循环引用处的替换文字（这种值本来也写不成 JSON）。 */
export const circularMark = '[Circular]';

/**
 * redactValue 递归脱敏一个值：字符串替换，数组的每个元素、普通对象（含嵌套）的每个字段都处理，其它原样返回。
 * ancestors 是当前这条路径上的对象：循环引用（对象包含自己、经由数组或其它字段绕回来）在这里结束，不会无限递归。
 * 同一个对象在兄弟位置出现两次（不是循环）时照常各展开一次：展开完就离开路径。
 */
function redactValue(v: unknown, r: Redactor, ancestors: Set<object> = new Set()): unknown {
  if (typeof v === 'string') return r.redact(v);
  if (v instanceof Error) return r.redact(v.message);
  if (typeof v !== 'object' || v === null) return v;
  if (ancestors.has(v)) return circularMark;
  if (Array.isArray(v)) {
    ancestors.add(v);
    try {
      return v.map((x) => redactValue(x, r, ancestors));
    } finally {
      ancestors.delete(v);
    }
  }
  const proto = Object.getPrototypeOf(v) as unknown;
  // 只展开普通对象（JSON 那种）；类实例、Map、Buffer 等不按字段遍历
  if (proto !== Object.prototype && proto !== null) return v;
  ancestors.add(v);
  try {
    return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, redactValue(x, r, ancestors)]));
  } finally {
    ancestors.delete(v);
  }
}

/** redactingLogger 返回写出之前替换密钥的 Logger：消息和每个字段（含数组、嵌套对象中的文字）都替换。 */
export function redactingLogger(log: Logger, r: Redactor): Logger {
  const fields = (f?: Record<string, unknown>) =>
    f && Object.fromEntries(Object.entries(f).map(([k, v]) => [k, redactValue(v, r)]));
  return {
    info: (msg, f) => log.info(r.redact(msg), fields(f)),
    warn: (msg, f) => log.warn(r.redact(msg), fields(f)),
    error: (msg, f) => log.error(r.redact(msg), fields(f)),
  };
}
