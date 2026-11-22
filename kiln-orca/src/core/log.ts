/** Logger 是连接器自己的日志（host 写到按天轮转的文件）。 */
export interface Logger {
  info(msg: string, fields?: Record<string, unknown>): void;
  warn(msg: string, fields?: Record<string, unknown>): void;
  error(msg: string, fields?: Record<string, unknown>): void;
}

export const nopLogger: Logger = { info() {}, warn() {}, error() {} };

/** formatLine 把一条日志格式化为一行文字。 */
export function formatLine(level: string, msg: string, fields?: Record<string, unknown>, now = new Date()): string {
  const kv = Object.entries(fields ?? {})
    .map(([k, v]) => `${k}=${typeof v === 'string' ? JSON.stringify(v) : String(v)}`)
    .join(' ');
  return `${now.toISOString()} ${level} ${msg}${kv ? ' ' + kv : ''}\n`;
}
