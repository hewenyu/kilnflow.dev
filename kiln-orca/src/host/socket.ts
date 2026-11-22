import { mkdir, rm } from 'node:fs/promises';
import { createServer, type IncomingMessage, request, type Server, type ServerResponse } from 'node:http';
import path from 'node:path';

// 给 Agent 用的本机工具（kiln-orca task …，PLAN #33.5）：命令经数据目录中的 unix socket 交给正在运行的连接器，
// 连接器只接受它正在执行的 attempt。socket 只有本用户能访问（目录 0700）。
//
// 两种请求：工具调用（POST /task，callTool）回复后关闭连接；git 传输的操作（POST /op，openOp，PLAN #33.13）由传输包装
// 保持连接：连接器回复一行 JSON 后不结束回复，连接关闭（包装正常退出、被杀）即操作结束。

export interface ToolRequest {
  attempt: string;
  cmd: string;
  args: Record<string, string>;
}

export interface ToolResponse {
  code: number;
  output: string;
}

export type ToolHandler = (req: ToolRequest) => Promise<ToolResponse>;

/** OpRequest 是传输包装开一个操作的请求：attempt、这次传输的地址（insteadOf 改写之后）、包装的进程号（日志用）。 */
export interface OpRequest {
  attempt: string;
  url: string;
  pid: string;
}

/** OpReply 是开操作的回复：code 不为 0 时拒绝这次传输（output 是说明）；op 为空时这次传输不给凭据（不是 Kiln 管的仓库）。 */
export interface OpReply {
  code: number;
  output: string;
  op: string;
}

/** OpHandler 处理开操作的请求；回复中有 op 时，onClose 在连接关闭时调用一次（包括回复之前就已经关闭）。 */
export type OpHandler = (req: OpRequest) => Promise<OpReply & { onClose?: () => unknown }>;

/**
 * maxSocketPathBytes 是 unix socket 路径的上限（sockaddr_un.sun_path：Linux 108 字节、macOS 104 字节，都含结尾的 NUL）。
 * 超过时 listen / connect 的错误不说明原因，这里先检查并明确报错。
 */
export const maxSocketPathBytes = process.platform === 'darwin' ? 103 : 107;

/** checkSocketPath 检查 socket 路径的长度，超过上限时抛出说明（让数据目录用短一些的路径）。 */
export function checkSocketPath(socketPath: string): void {
  const n = Buffer.byteLength(socketPath, 'utf8');
  if (n > maxSocketPathBytes) {
    throw new Error(`unix socket 路径 ${socketPath} 有 ${n} 字节，超过这个平台的上限 ${maxSocketPathBytes} 字节：把 kiln-orca 的数据目录（--home 或 KILN_ORCA_HOME）换到更短的路径`);
  }
}

/**
 * maxToolRequestBytes 是一次工具调用的请求体上限：请求来自 Agent 执行的 kiln-orca task（参数只有 attempt、命令名和
 * 几个短参数，报告经任务目录中的文件传递），正常只有几百字节。超过时不再收集，读完后明确回复请求过大。
 */
export const maxToolRequestBytes = 64 << 10;

/**
 * maxToolReplyBytes 是一次工具调用的回复（JSON）上限。连接器的回复都有界（task ci 的输出见 maxCIOutputBytes），这个上限
 * 只防意外：连接器不发送超过上限的回复，改为明确回复过大；kiln-orca task 读到超过上限的回复时停止读取并报错，不会截断后
 * 当作完整的输出。
 */
export const maxToolReplyBytes = 4 << 20;

/** ToolReplyTooLargeError 是读到的回复超过上限（limit 是那一次实际使用的上限，见 maxToolReplyBytes、maxOpReplyBytes）。 */
export class ToolReplyTooLargeError extends Error {
  constructor(readonly limit: number = maxToolReplyBytes) {
    super(`kiln-orca 连接器的回复超过 ${limit} 字节，没有读完`);
    this.name = 'ToolReplyTooLargeError';
  }
}

/** isObject 表示 v 是 JSON 对象（不是数组、null）。 */
function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** parseToolRequest 按 unknown 解析工具调用请求，校验字段类型（不接受数字、null 等）。 */
export function parseToolRequest(v: unknown): ToolRequest {
  if (!isObject(v)) throw new Error('请求应为 JSON 对象');
  if (typeof v.attempt !== 'string') throw new Error('attempt 应为字符串');
  if (typeof v.cmd !== 'string') throw new Error('cmd 应为字符串');
  const args: Record<string, string> = {};
  if (v.args !== undefined) {
    if (!isObject(v.args)) throw new Error('args 应为对象');
    for (const [k, x] of Object.entries(v.args)) {
      if (typeof x !== 'string') throw new Error(`args.${k} 应为字符串`);
      args[k] = x;
    }
  }
  return { attempt: v.attempt, cmd: v.cmd, args };
}

/** parseOpRequest 按 unknown 解析 git 传输的操作请求，校验字段类型。 */
export function parseOpRequest(v: unknown): OpRequest {
  if (!isObject(v)) throw new Error('请求应为 JSON 对象');
  if (typeof v.attempt !== 'string') throw new Error('attempt 应为字符串');
  const url = v.url === undefined ? '' : v.url;
  if (typeof url !== 'string') throw new Error('url 应为字符串');
  const pid = v.pid === undefined ? '' : v.pid;
  if (typeof pid !== 'string') throw new Error('pid 应为字符串');
  return { attempt: v.attempt, url, pid };
}

/** parseToolResponse 按 unknown 解析工具调用的回复，校验字段类型。 */
function parseToolResponse(v: unknown): ToolResponse {
  if (!isObject(v)) throw new Error('连接器的回复不是 JSON 对象');
  if (typeof v.code !== 'number' || !Number.isInteger(v.code)) throw new Error('连接器回复的 code 应为整数');
  if (typeof v.output !== 'string') throw new Error('连接器回复的 output 应为字符串');
  return { code: v.code, output: v.output };
}

/** replyBody 是回复的 JSON：超过上限时改为说明回复过大的错误。 */
function replyBody(out: ToolResponse): string {
  const body = JSON.stringify(out);
  const size = Buffer.byteLength(body, 'utf8');
  if (size <= maxToolReplyBytes) return body;
  return JSON.stringify({ code: 1, output: `连接器的回复有 ${size} 字节，超过 ${maxToolReplyBytes} 字节的上限，没有发送：这是 kiln-orca 的问题，请报告。` });
}

export async function serveTools(socketPath: string, handler: ToolHandler, opHandler?: OpHandler): Promise<Server> {
  checkSocketPath(socketPath);
  await mkdir(path.dirname(socketPath), { recursive: true, mode: 0o700 });
  await rm(socketPath, { force: true });
  const srv = createServer((req, res) => {
    if (req.url === '/op') {
      serveOp(req, res, opHandler);
      return;
    }
    const chunks: Buffer[] = [];
    let size = 0;
    req.on('data', (d: Buffer) => {
      size += d.length;
      // 超过上限后继续读完（不保存），再回复：直接断开时客户端只能看到连接被重置
      if (size <= maxToolRequestBytes) chunks.push(d);
    });
    req.on('end', () => {
      void (async () => {
        let out: ToolResponse;
        if (size > maxToolRequestBytes) {
          out = { code: 1, output: `请求无效：超过 ${maxToolRequestBytes} 字节` };
        } else {
          try {
            const r = parseToolRequest(JSON.parse(Buffer.concat(chunks).toString('utf8')) as unknown);
            out = await handler(r);
          } catch (err) {
            out = { code: 1, output: `请求无效：${err instanceof Error ? err.message : String(err)}` };
          }
        }
        // 一次写出完整的回复并带上长度：客户端据此确认收全（见 callTool）
        const body = Buffer.from(replyBody(out), 'utf8');
        res.writeHead(200, { 'content-type': 'application/json', 'content-length': body.length });
        res.end(body);
      })();
    });
  });
  await new Promise<void>((resolve, reject) => {
    srv.once('error', reject);
    srv.listen(socketPath, () => resolve());
  });
  return srv;
}

/** maxOpRequestBytes 是开操作的请求体上限（attempt、地址和进程号）。 */
const maxOpRequestBytes = 16 << 10;

/**
 * serveOp 处理开操作的请求：回复一行 JSON；有操作时不结束回复、保持连接，连接关闭时调用 onClose。连接在回复之前就已经
 * 关闭（包装在等待时被杀）时，拿到回复后立即调用 onClose。
 */
function serveOp(req: IncomingMessage, res: ServerResponse, handler: OpHandler | undefined): void {
  let closed = false;
  let onClose: (() => unknown) | undefined;
  // 回复没有结束时连接关闭（包括被杀的包装由内核关闭）：res 的 close 事件
  res.on('close', () => {
    closed = true;
    const f = onClose;
    onClose = undefined;
    f?.();
  });
  const chunks: Buffer[] = [];
  let size = 0;
  req.on('data', (d: Buffer) => {
    size += d.length;
    if (size <= maxOpRequestBytes) chunks.push(d);
  });
  req.on('end', () => {
    void (async () => {
      let out: OpReply & { onClose?: () => unknown };
      if (size > maxOpRequestBytes || !handler) {
        out = { code: 1, output: handler ? `请求无效：超过 ${maxOpRequestBytes} 字节` : '这个连接器不处理 git 传输', op: '' };
      } else {
        try {
          const r = parseOpRequest(JSON.parse(Buffer.concat(chunks).toString('utf8')) as unknown);
          out = await handler(r);
        } catch (err) {
          out = { code: 1, output: `请求无效：${err instanceof Error ? err.message : String(err)}`, op: '' };
        }
      }
      if (closed) {
        out.onClose?.();
        return;
      }
      const line = JSON.stringify({ code: out.code, output: out.output, op: out.op }) + '\n';
      if (out.code !== 0 || !out.op || !out.onClose) {
        out.onClose?.();
        res.writeHead(200, { 'content-type': 'application/x-ndjson' });
        res.end(line);
        return;
      }
      onClose = out.onClose;
      // 不结束回复：连接一直保持到包装关闭它
      res.writeHead(200, { 'content-type': 'application/x-ndjson' });
      res.write(line);
    })();
  });
}

/** maxOpReplyBytes 是开操作的回复第一行的上限（正常只有几十字节：code、说明和操作 id）。 */
export const maxOpReplyBytes = 64 << 10;

/** parseOpReply 解析并校验开操作的回复。 */
function parseOpReply(line: string): OpReply {
  const v = JSON.parse(line) as Partial<OpReply> | null;
  if (!v || typeof v.code !== 'number' || typeof v.output !== 'string' || typeof v.op !== 'string') throw new Error('连接器回复的操作格式不对');
  return { code: v.code, output: v.output, op: v.op };
}

/** OpConnection 是传输包装持有的操作：reply 是连接器的回复，close 关闭连接（操作随之结束）。 */
export interface OpConnection {
  reply: OpReply;
  close(): void;
}

/**
 * openOp 向连接器开一个操作并保持连接（PLAN #33.13）：读到回复的第一行即返回。连接器一侧之后断开（重启、退出）时什么都
 * 不做：操作已经由连接器结束，这次传输之后的凭据请求连不上而失败。
 */
export function openOp(socketPath: string, req: OpRequest, timeoutMs = 10 * 60_000): Promise<OpConnection> {
  return new Promise((resolve, reject) => {
    // executor 中抛出的错误成为 reject
    checkSocketPath(socketPath);
    let done = false;
    const r = request({ socketPath, path: '/op', method: 'POST', agent: false, headers: { 'content-type': 'application/json' } }, (res) => {
      let buf = Buffer.alloc(0);
      res.on('data', (d: Buffer) => {
        if (done) return;
        // 按字节限制第一帧（回复的第一行）
        buf = Buffer.concat([buf, d]);
        const i = buf.indexOf(0x0a);
        if ((i < 0 ? buf.length : i) > maxOpReplyBytes) {
          fail(new ToolReplyTooLargeError(maxOpReplyBytes));
          return;
        }
        if (i < 0) return;
        let reply: OpReply;
        try {
          reply = parseOpReply(buf.subarray(0, i).toString('utf8'));
        } catch (err) {
          fail(err instanceof Error ? err : new Error(String(err)));
          return;
        }
        done = true;
        clearTimeout(timer);
        resolve({ reply, close: () => r.destroy() });
      });
      // 第一行收齐之前连接出错、中断或关闭：立即失败（之后的断开由连接器一侧处理，这里不管）
      const cut = () => fail(new Error('连接器没有回复完就关闭了连接'));
      res.on('error', cut);
      res.on('aborted', cut);
      res.on('end', cut);
      res.on('close', cut);
    });
    const fail = (err: Error) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      r.destroy();
      reject(err);
    };
    const timer = setTimeout(() => fail(new Error('等待连接器超时')), timeoutMs);
    r.on('error', (err) => fail(err));
    r.on('close', () => fail(new Error('连接器没有回复就关闭了连接')));
    r.end(JSON.stringify(req));
  });
}

/**
 * callTool 把一个命令交给连接器并读完回复。每次调用一个连接、回复后关闭（不保持连接：kiln-orca task 输出后自然退出，
 * 连接器关闭 socket 时也没有空闲的连接要等）。回复不完整（连接中途断开）或超过 maxToolReplyBytes 时报错。
 */
export function callTool(socketPath: string, req: ToolRequest, timeoutMs = 10 * 60_000): Promise<ToolResponse> {
  return new Promise((resolve, reject) => {
    checkSocketPath(socketPath);
    const r = request(
      { socketPath, path: '/task', method: 'POST', agent: false, headers: { 'content-type': 'application/json' }, timeout: timeoutMs },
      (res) => {
        const chunks: Buffer[] = [];
        let size = 0;
        res.on('data', (d: Buffer) => {
          size += d.length;
          if (size > maxToolReplyBytes) {
            res.destroy(new ToolReplyTooLargeError(maxToolReplyBytes));
            return;
          }
          chunks.push(d);
        });
        // 连接在回复写完之前断开时（长度不够）是 aborted 错误，不会走到 end
        res.on('error', reject);
        res.on('end', () => {
          try {
            resolve(parseToolResponse(JSON.parse(Buffer.concat(chunks).toString('utf8')) as unknown));
          } catch (err) {
            reject(err instanceof Error ? err : new Error(String(err)));
          }
        });
      },
    );
    r.on('timeout', () => r.destroy(new Error('等待连接器超时')));
    r.on('error', reject);
    r.end(JSON.stringify(req));
  });
}
