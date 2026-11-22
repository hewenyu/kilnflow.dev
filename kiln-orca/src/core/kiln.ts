import { createPrivateKey, createPublicKey, randomBytes, sign } from 'node:crypto';
import { type Client, Code, ConnectError, createClient, type Interceptor, type Transport } from '@connectrpc/connect';
import type { MessageInitShape } from '@bufbuild/protobuf';
import {
  AIRunnerService,
  type AcquireRequestSchema,
  type AcquireResponse,
  type ClaimDevIssuesRequestSchema,
  type ClaimDevIssuesResponse,
  type CompleteRequestSchema,
  type CompleteResponse,
  type GetTaskDocRequestSchema,
  type GetTaskDocResponse,
  type GetVerificationRequestSchema,
  type GetVerificationResponse,
  type HeartbeatRequestSchema,
  type HeartbeatResponse,
  type IssueCredentialRequestSchema,
  type IssueCredentialResponse,
  type ListTaskDocsRequestSchema,
  type ListTaskDocsResponse,
  type OfferDevCandidatesResponse,
  type RevokeCredentialRequestSchema,
  type TaskRef,
  type ValidateReportRequestSchema,
  type ValidateReportResponse,
} from '../gen/kiln/airunner/v2/airunner_pb.js';
import { RunnerService } from '../gen/kiln/runner/v1/runner_pb.js';
import { RevokedError, StaleError } from './errors.js';

// 与 Server 的连接（PLAN #33.6）：注册、认证用 kiln.runner.v1，AI 任务用 kiln.airunner.v2。
// 错误约定与 Go 的 internal/rpcerr 相同：FAILED_PRECONDITION + STALE_EPOCH、PERMISSION_DENIED + RUNNER_REVOKED。

export type AcquireInit = MessageInitShape<typeof AcquireRequestSchema>;
export type HeartbeatInit = MessageInitShape<typeof HeartbeatRequestSchema>;
export type CompleteInit = MessageInitShape<typeof CompleteRequestSchema>;
export type ClaimInit = MessageInitShape<typeof ClaimDevIssuesRequestSchema>;
export type ValidateInit = MessageInitShape<typeof ValidateReportRequestSchema>;
export type VerificationInit = MessageInitShape<typeof GetVerificationRequestSchema>;
export type IssueCredentialInit = MessageInitShape<typeof IssueCredentialRequestSchema>;
export type RevokeCredentialInit = MessageInitShape<typeof RevokeCredentialRequestSchema>;
export type ListTaskDocsInit = MessageInitShape<typeof ListTaskDocsRequestSchema>;
export type GetTaskDocInit = MessageInitShape<typeof GetTaskDocRequestSchema>;

/** Kiln 是连接器用到的 Server 接口（测试中替换为进程内的假 Server）。 */
export interface Kiln {
  acquire(req: AcquireInit, signal?: AbortSignal): Promise<AcquireResponse>;
  heartbeat(req: HeartbeatInit, signal?: AbortSignal): Promise<HeartbeatResponse>;
  appendLog(ref: TaskRef, offset: bigint, data: Uint8Array, signal?: AbortSignal): Promise<bigint>;
  complete(req: CompleteInit, signal?: AbortSignal): Promise<CompleteResponse>;
  offerDevCandidates(ref: TaskRef): Promise<OfferDevCandidatesResponse>;
  claimDevIssues(req: ClaimInit): Promise<ClaimDevIssuesResponse>;
  validateReport(req: ValidateInit): Promise<ValidateReportResponse>;
  getVerification(req: VerificationInit): Promise<GetVerificationResponse>;
  /** issueCredential、revokeCredential：按操作（op_id）签发、关闭的短期凭据（PLAN #33.13）。 */
  issueCredential(req: IssueCredentialInit): Promise<IssueCredentialResponse>;
  revokeCredential(req: RevokeCredentialInit): Promise<void>;
  /** listTaskDocs、getTaskDoc：读取任务所属工作项冻结的设计文档快照（PLAN #33）。 */
  listTaskDocs(req: ListTaskDocsInit, signal?: AbortSignal): Promise<ListTaskDocsResponse>;
  getTaskDoc(req: GetTaskDocInit, signal?: AbortSignal): Promise<GetTaskDocResponse>;
}

const reasonStale = 'STALE_EPOCH';
const reasonRevoked = 'RUNNER_REVOKED';
/** tokenRefreshMarginMs：access token 剩余不到这么久时重新认证。 */
const tokenRefreshMarginMs = 60_000;
/** rpcTimeoutMs 是每个 RPC 的期限（领取的长轮询另加等待时间）：Server 不响应时不会一直卡住。 */
const rpcTimeoutMs = 60_000;

/** mapError 把 Server 约定的错误原因转换为 StaleError、RevokedError。 */
export function mapError(err: unknown): unknown {
  const ce = ConnectError.from(err);
  if (ce.code === Code.FailedPrecondition && ce.rawMessage === reasonStale) return new StaleError();
  if (ce.code === Code.PermissionDenied && ce.rawMessage === reasonRevoked) return new RevokedError();
  return err;
}

/**
 * serverDeclined 判断签发凭据的错误是不是 Server 明确回答"GitHub 确定没有签发"：Server 发来的（Connect-ES 只对从回复中
 * 解析出的错误设置 isWireError）UNAVAILABLE。只有这时才用同一个 op_id 重试签发：没有收到回复（连接断开、超时）、Connect
 * 客户端按 HTTP 状态合成的 UNAVAILABLE（网关的 502 / 503）、其它错误都不能说明 Server 的登记中这个操作的状态（Server 可能
 * 已经签出了 token，也可能在两次请求之间重启、丢了登记，同一 op_id 的重放会再签一枚），这个操作就此结束，按原 op_id 关闭。
 */
export function serverDeclined(err: unknown): boolean {
  return err instanceof ConnectError && err.code === Code.Unavailable && err.isWireError;
}

/**
 * retryable 判断错误是否值得用同一个请求重试（与 runnerkit.Retryable 相同，另外 ABORTED 不重试：Server 用它表示 GitHub
 * 签发凭据的结果不明、这个操作已经关闭，同一 op_id 不会再签发，重新执行才是新的操作）。
 */
export function retryable(err: unknown): boolean {
  if (err instanceof StaleError || err instanceof RevokedError) return false;
  // 网络错误等没有收到 Server 响应的错误可以重试
  if (!(err instanceof ConnectError)) return true;
  return ![Code.InvalidArgument, Code.NotFound, Code.PermissionDenied, Code.FailedPrecondition, Code.Unimplemented, Code.AlreadyExists, Code.Aborted].includes(
    err.code,
  );
}

/** ed25519Key 由 32 字节的 seed 构造私钥（与 Go 的 ed25519.NewKeyFromSeed 相同）。 */
export function ed25519Key(seed: Uint8Array) {
  if (seed.length !== 32) throw new Error('ed25519 私钥 seed 必须是 32 字节');
  const der = Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.from(seed)]);
  return createPrivateKey({ key: der, format: 'der', type: 'pkcs8' });
}

/** publicKey 返回 seed 对应的 32 字节公钥（注册时上传）。 */
export function publicKey(seed: Uint8Array): Uint8Array {
  const spki = createPublicKey(ed25519Key(seed)).export({ format: 'der', type: 'spki' });
  // ed25519 的 SPKI DER 是 12 字节的头 + 32 字节的公钥
  return new Uint8Array(spki.subarray(spki.length - 32));
}

/** authMessage 是认证签名的内容（与 internal/server/runnerauth 一致）。 */
export function authMessage(runnerId: bigint, ts: bigint, nonce: string): string {
  return `kiln-runner-auth:${runnerId}:${ts}:${nonce}`;
}

/** ConnectKiln 经 Connect 调用 Server，自动认证和续期 access token。 */
export class ConnectKiln implements Kiln {
  private token = '';
  private expiresAt = 0;
  /** 正在进行的认证：同时调用时共用这一次，不会为每一次 RPC 各发一个认证请求。 */
  private authInflight: Promise<string> | undefined;
  private readonly runner: Client<typeof RunnerService>;
  private readonly ai: Client<typeof AIRunnerService>;

  constructor(
    transport: (interceptors: Interceptor[]) => Transport,
    private readonly runnerId: bigint,
    private readonly seed: Uint8Array,
  ) {
    this.runner = createClient(RunnerService, transport([]));
    const auth: Interceptor = (next) => async (req) => {
      req.header.set('Authorization', `Bearer ${await this.authenticate()}`);
      return next(req);
    };
    this.ai = createClient(AIRunnerService, transport([auth]));
  }

  /** authenticate 返回可用的 access token：还有效时直接用，否则发起（或共用正在进行的）一次认证。 */
  authenticate(): Promise<string> {
    if (this.token && this.expiresAt - Date.now() > tokenRefreshMarginMs) return Promise.resolve(this.token);
    this.authInflight ??= this.refreshToken().finally(() => (this.authInflight = undefined));
    return this.authInflight;
  }

  /**
   * authenticateWith 与 authenticate 相同，但调用方的 signal 结束时只结束这一次等待：共享的认证刷新照常完成，
   * 不让一个调用方的停止误杀其他调用方仍在等待的认证。
   */
  private async authenticateWith(signal?: AbortSignal): Promise<string> {
    if (!signal) return this.authenticate();
    // 调用前已经中止：直接结束，不发起认证（认证会失败也没有意义）。
    if (signal.aborted) throw signal.reason ?? new DOMException('已中止', 'AbortError');
    const pending = this.authenticate();
    // 后台共享的认证在等待者全部中止之后仍会继续；消费它的失败，避免成为未处理的拒绝。Promise.race 仍然接收原始
    // pending 的 rejection。
    pending.catch(() => undefined);
    let onAbort: () => void = () => undefined;
    const aborted = new Promise<never>((_resolve, reject) => {
      onAbort = () => reject(signal.reason ?? new DOMException('已中止', 'AbortError'));
      signal.addEventListener('abort', onAbort, { once: true });
    });
    try {
      return await Promise.race([pending, aborted]);
    } finally {
      signal.removeEventListener('abort', onAbort);
    }
  }

  private async refreshToken(): Promise<string> {
    const nonce = randomBytes(16).toString('base64url');
    const ts = BigInt(Math.floor(Date.now() / 1000));
    const signature = sign(null, Buffer.from(authMessage(this.runnerId, ts, nonce)), ed25519Key(this.seed));
    try {
      const r = await this.runner.authenticate({ runnerId: this.runnerId, timestamp: ts, nonce, signature }, { timeoutMs: rpcTimeoutMs });
      this.token = r.accessToken;
      this.expiresAt = Number(r.expiresAt) * 1000;
      return this.token;
    } catch (err) {
      // 只有 Server 发来的拒绝才说明节点被吊销：代理、网关的 403（Connect 客户端按 HTTP 状态合成）不是
      const ce = ConnectError.from(err);
      if (ce.code === Code.PermissionDenied && ce.isWireError) throw new RevokedError();
      // 代理、网关的普通 401 / 403 不是 Server 的答复：按暂时性错误（UNAVAILABLE）交回，各调用方按自己的规则重试，不当作
      // 被拒绝（PERMISSION_DENIED 在 retryable 中不重试）
      if ((ce.code === Code.PermissionDenied || ce.code === Code.Unauthenticated) && !ce.isWireError) {
        throw new ConnectError(`认证失败（没有收到 Server 的明确答复）：${ce.rawMessage}`, Code.Unavailable);
      }
      throw err;
    }
  }

  /**
   * call 调用一个 RPC；Server 明确回答 token 无效（Server 发来的 UNAUTHENTICATED，isWireError）时重新认证并重放一次：
   * Kiln 的鉴权在处理函数之前，被拒绝的请求没有执行，重放是安全的。来源不明的 UNAUTHENTICATED（反向代理、网关的普通
   * 401，Connect 客户端按 HTTP 状态合成）不重放：请求可能已经到达 Server 并执行（例如签发了凭据），错误交回调用方按
   * 各自的规则处理（签发按结果不明结束操作、按原 op_id 关闭）；缓存的 token 照样作废，下一次调用重新认证。
   * 认证在调用之前、拦截器之外完成：拦截器中抛出的错误会被 Connect 包装成 code 为 unknown 的 ConnectError，RevokedError
   * 的类型就丢了（认证被拒绝时节点认不出自己已被吊销）。
   */
  private async call<T>(f: () => Promise<T>, signal?: AbortSignal): Promise<T> {
    for (let i = 0; ; i++) {
      try {
        await this.authenticateWith(signal);
        signal?.throwIfAborted();
        return await f();
      } catch (err) {
        if (err instanceof RevokedError) throw err;
        const ce = ConnectError.from(err);
        if (ce.code === Code.Unauthenticated) {
          this.token = '';
          if (i === 0 && ce.isWireError) continue;
        }
        throw mapError(err);
      }
    }
  }

  acquire(req: AcquireInit, signal?: AbortSignal) {
    return this.call(() => this.ai.acquire(req, { signal, timeoutMs: (req.waitSeconds ?? 0) * 1000 + rpcTimeoutMs }), signal);
  }
  heartbeat(req: HeartbeatInit, signal?: AbortSignal) {
    return this.call(() => this.ai.heartbeat(req, { signal, timeoutMs: rpcTimeoutMs }), signal);
  }
  async appendLog(ref: TaskRef, offset: bigint, data: Uint8Array, signal?: AbortSignal) {
    return (await this.call(() => this.ai.appendLog({ ref, offset, data }, { signal, timeoutMs: rpcTimeoutMs }), signal)).committed;
  }
  complete(req: CompleteInit, signal?: AbortSignal) {
    return this.call(() => this.ai.complete(req, { signal, timeoutMs: rpcTimeoutMs }), signal);
  }
  offerDevCandidates(ref: TaskRef) {
    return this.call(() => this.ai.offerDevCandidates({ ref }, { timeoutMs: rpcTimeoutMs }));
  }
  claimDevIssues(req: ClaimInit) {
    return this.call(() => this.ai.claimDevIssues(req, { timeoutMs: rpcTimeoutMs }));
  }
  validateReport(req: ValidateInit) {
    return this.call(() => this.ai.validateReport(req, { timeoutMs: rpcTimeoutMs }));
  }
  getVerification(req: VerificationInit) {
    return this.call(() => this.ai.getVerification(req, { timeoutMs: rpcTimeoutMs }));
  }
  issueCredential(req: IssueCredentialInit) {
    return this.call(() => this.ai.issueCredential(req, { timeoutMs: rpcTimeoutMs }));
  }
  async revokeCredential(req: RevokeCredentialInit) {
    await this.call(() => this.ai.revokeCredential(req, { timeoutMs: rpcTimeoutMs }));
  }
  listTaskDocs(req: ListTaskDocsInit, signal?: AbortSignal) {
    return this.call(() => this.ai.listTaskDocs(req, { signal, timeoutMs: rpcTimeoutMs }), signal);
  }
  getTaskDoc(req: GetTaskDocInit, signal?: AbortSignal) {
    return this.call(() => this.ai.getTaskDoc(req, { signal, timeoutMs: rpcTimeoutMs }), signal);
  }
}

/** withRetry 对可重试的错误（默认按 retryable 判断）做指数退避重试（最多 attempts 次，0 表示直到 signal 结束）。 */
export async function withRetry<T>(
  f: () => Promise<T>,
  opts: { attempts?: number; signal?: AbortSignal; baseMs?: number; maxMs?: number; retryable?: (err: unknown) => boolean } = {},
): Promise<T> {
  const canRetry = opts.retryable ?? retryable;
  let delay = opts.baseMs ?? 1000;
  for (let i = 0; ; i++) {
    try {
      return await f();
    } catch (err) {
      const last = opts.attempts !== undefined && opts.attempts > 0 && i + 1 >= opts.attempts;
      if (!canRetry(err) || last || opts.signal?.aborted) throw err;
      await sleep(delay, opts.signal);
      // 等待中 signal 结束（例如连接器退出）：不再发起下一次
      if (opts.signal?.aborted) throw err;
      delay = Math.min(delay * 2, opts.maxMs ?? 30_000);
    }
  }
}

/** sleep 等待 ms 毫秒或 signal 结束。 */
export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal?.aborted) return resolve();
    const t = setTimeout(done, ms);
    function done() {
      clearTimeout(t);
      signal?.removeEventListener('abort', done);
      resolve();
    }
    signal?.addEventListener('abort', done, { once: true });
  });
}
