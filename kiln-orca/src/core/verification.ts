import type { FailedJob, GetVerificationResponse } from '../gen/kiln/airunner/v2/airunner_pb.js';

// kiln-orca task ci 的输出（PLAN #33.5）：commit 的验证结论和失败的 job。输出给 Agent 看，有总量上限；没有全部列出时
// 明确写出没有列出的部分，Agent 知道自己没有看全。

/** ciLogTailBytes 是每个失败 job 向 Server 要的日志尾部字节数。 */
export const ciLogTailBytes = 16 << 10;

/**
 * maxCIOutputBytes 是 task ci 输出的总量上限（不含末尾的省略说明）：约 8 个 job 的完整日志尾部。失败的 job 很多时
 * 前面的 job 带日志，之后的只列名称、原因和地址，再之后只给出数量。
 */
export const maxCIOutputBytes = 128 << 10;

type Verification = Pick<GetVerificationResponse, 'sha' | 'state' | 'failedJobs'>;

const bytes = (s: string) => Buffer.byteLength(s, 'utf8');

function jobHeader(j: FailedJob): string {
  const lines = [`\n## 失败：${j.name}（${j.errorClass || '未分类'}）${j.reason ? '：' + j.reason : ''}`];
  if (j.url) lines.push(j.url);
  if (j.errorClass.startsWith('infra.')) lines.push('（基础设施的问题，不需要改代码）');
  if (j.logExpired) lines.push('（日志已过保留期）');
  return lines.join('\n');
}

function jobLog(j: FailedJob): string {
  if (j.logExpired || j.logTail.length === 0) return '';
  // Server 最多返回请求的字节数：达到时更早的部分没有返回
  const head = j.logTail.length >= ciLogTailBytes ? `\n（日志的最后 ${ciLogTailBytes} 字节，更早的部分没有列出，完整日志见上面的地址）` : '';
  return `${head}\n\`\`\`\n${Buffer.from(j.logTail).toString('utf8')}\n\`\`\``;
}

/**
 * formatVerification 把验证结果写成给 Agent 看的文本。按顺序列出失败的 job：总量不超过 limit 时带日志；放不下日志时
 * 之后的 job 只列标题（名称、原因、地址）；标题也放不下时只给出数量。有省略时开头和末尾都写明省略了什么。
 */
export function formatVerification(r: Verification, limit = maxCIOutputBytes): string {
  const total = r.failedJobs.length;
  const first = `commit ${r.sha}：${r.state}${total > 0 ? `（${total} 个失败的 job）` : ''}`;
  const parts = [first];
  let size = bytes(first);
  /** add 在放得下时加上一段（连同分隔的换行）。 */
  const add = (part: string): boolean => {
    if (size + 1 + bytes(part) > limit) return false;
    parts.push(part);
    size += 1 + bytes(part);
    return true;
  };
  let withoutLog = 0;
  let listed = 0;
  for (const j of r.failedJobs) {
    const header = jobHeader(j);
    const log = jobLog(j);
    // 一旦有 job 的日志放不下，之后的都只列标题：输出保持按顺序，省略的总是末尾的部分
    if (withoutLog === 0 && add(header + log)) {
      listed++;
      continue;
    }
    if (!add(header + (log ? '\n（日志没有列出：输出超过上限）' : ''))) break;
    if (log) withoutLog++;
    listed++;
  }
  const notes: string[] = [];
  if (withoutLog > 0) notes.push(`${withoutLog} 个 job 的日志没有列出`);
  if (listed < total) notes.push(`还有 ${total - listed} 个失败的 job 没有列出`);
  if (notes.length === 0) return parts.join('\n');
  const note = `注意：输出超过 ${limit} 字节的上限，${notes.join('，')}；完整的结果和日志在 Kiln 中查看（各 job 的地址）。`;
  return [first, note, ...parts.slice(1), '', note].join('\n');
}
