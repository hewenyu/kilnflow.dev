/** KeyedMutex 按 key 串行执行（同一个仓库的 clone：准备仓库、在 Orca 中建 worktree 同时只有一个在进行）。 */
export class KeyedMutex {
  private readonly tails = new Map<string, Promise<unknown>>();

  /** busy：key 上有正在进行或排队中的调用（同步检查：检查之后不经过 await 调用 run 时，run 立即开始）。 */
  busy(key: string): boolean {
    return this.tails.has(key);
  }

  async run<T>(key: string, f: () => Promise<T>): Promise<T> {
    const prev = this.tails.get(key) ?? Promise.resolve();
    const next = prev.then(f, f);
    const tail = next.catch(() => undefined);
    this.tails.set(key, tail);
    try {
      return await next;
    } finally {
      if (this.tails.get(key) === tail) this.tails.delete(key);
    }
  }
}
