// 任务日志上传（AppendLog，按 offset 幂等）：写入先进缓冲区，flush 把 Server 还没有的部分发出去，按 Server 返回的
// committed 截掉已经收到的部分。上传失败时保留缓冲区，下一次 flush 重试。
//
// 从 journal 恢复的 attempt（resume）在 Server 上已经有日志，本地不知道长度：第一次上传之前先用空数据查询 Server 已有的
// 长度（Server 对空数据只返回当前长度，不写入），再从那里接着传。Server 返回的 committed 超过这一块的末尾时，说明本地的
// 位置落后了（Server 按 offset 把这一块当成已有的内容丢掉了）：这一块并没有被收下，改从 Server 的长度重新发送，不截掉。

const maxLogBytes = 8 << 20;
const maxChunk = 256 << 10;

export class LogShipper {
  private buf = Buffer.alloc(0);
  /** committed 是 Server 已经持有的长度；buf 从这个位置开始。 */
  private committed = 0n;
  private total = 0;
  private dropped = false;
  private flushing: Promise<void> | undefined;
  /** synced 为 false 时 committed 未知（恢复的 attempt），上传之前先向 Server 查询。 */
  private synced: boolean;

  constructor(
    private readonly append: (offset: bigint, data: Uint8Array, signal?: AbortSignal) => Promise<bigint>,
    opts: { resume?: boolean } = {},
  ) {
    this.synced = !opts.resume;
  }

  write(s: string): void {
    if (this.total >= maxLogBytes) {
      if (!this.dropped) {
        this.dropped = true;
        this.buf = Buffer.concat([this.buf, Buffer.from('…（日志超过上限，之后的内容没有上传）\n')]);
      }
      return;
    }
    const b = Buffer.from(s);
    this.total += b.length;
    this.buf = Buffer.concat([this.buf, b]);
  }

  /** flush 把缓冲区中的日志发出去；signal 结束时停止上传（缓冲区保留，下一次再传）。 */
  flush(signal?: AbortSignal): Promise<void> {
    this.flushing ??= this.doFlush(signal).finally(() => (this.flushing = undefined));
    return this.flushing;
  }

  private async doFlush(signal?: AbortSignal): Promise<void> {
    if (this.buf.length === 0) return;
    signal?.throwIfAborted();
    if (!this.synced) {
      this.committed = await this.append(0n, new Uint8Array(), signal);
      this.synced = true;
    }
    while (this.buf.length > 0) {
      signal?.throwIfAborted();
      const chunk = this.buf.subarray(0, maxChunk);
      const got = await this.append(this.committed, chunk, signal);
      const n = Number(got - this.committed);
      if (n <= 0) return;
      if (n > chunk.length) {
        // 本地的位置落后于 Server：这一块没有被收下，从 Server 的长度重新发送
        this.committed = got;
        continue;
      }
      this.buf = this.buf.subarray(n);
      this.committed = got;
    }
  }

  /** close 上传剩余的日志（失败时重试几次）。 */
  async close(): Promise<void> {
    for (let i = 0; i < 3; i++) {
      try {
        await this.flush();
        return;
      } catch (err) {
        if (i === 2) throw err;
      }
    }
  }
}
