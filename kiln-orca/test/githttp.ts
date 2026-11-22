import { spawn } from 'node:child_process';
import { mkdir, symlink } from 'node:fs/promises';
import http from 'node:http';
import type { AddressInfo } from 'node:net';
import path from 'node:path';
import { git } from './harness.js';

// 测试用的 HTTP git 服务（git http-backend，CGI）：按 HTTP Basic 认证中的密码（installation token）决定能不能读、写，
// 记下每个请求用的凭据。连接器的网络 git 只在 HTTP(S) 地址上带凭据，测试它要用真的 HTTP。

/** GitRequest 是服务收到的一个请求：仓库路径（小写的 owner/name.git）、是不是推送、Basic 认证中的用户名和密码（没有时为空）。 */
export interface GitRequest {
  repo: string;
  /** method 是 HTTP 方法（推送时 GET info/refs 之后 POST git-receive-pack）。 */
  method: string;
  write: boolean;
  user: string;
  password: string;
}

/** Authorize 判断凭据能不能读（write 为 false）或写这个仓库。 */
export type Authorize = (req: GitRequest) => boolean;

export interface GitHTTP {
  /** url 返回仓库 owner/name 的地址（http://127.0.0.1:<端口>/owner/name.git）。 */
  url(repo: string): string;
  /** host 是 127.0.0.1:<端口>。 */
  host: string;
  readonly requests: GitRequest[];
  close(): Promise<void>;
}

/**
 * Gate 在处理一个请求之前调用（记下请求之后、校验凭据之前）：返回 Promise 时等它（测试控制请求被处理的时机）。
 * dropResponse 返回 true 时请求照常处理（推送照常生效），但不把回复交给客户端，直接断开连接（响应丢失）。
 */
export interface GitHTTPOptions {
  gate?: (req: GitRequest) => Promise<void> | undefined;
  dropResponse?: (req: GitRequest) => boolean;
}

/**
 * gitHTTP 在 root 下为 repos（owner/name → 裸仓库路径）建服务目录（符号链接），启动服务。authorize 拒绝时回复 401
 * （带 WWW-Authenticate，git 会去找凭据）。
 */
export async function gitHTTP(root: string, repos: Record<string, string>, authorize: Authorize, opts: GitHTTPOptions = {}): Promise<GitHTTP> {
  const project = path.join(root, 'git-http');
  for (const [name, bare] of Object.entries(repos)) {
    await mkdir(path.join(project, path.dirname(name)), { recursive: true });
    await symlink(bare, path.join(project, `${name}.git`));
    // 推送要求仓库允许 receive-pack（http-backend 默认只对认证过的用户开放，这里由 authorize 把关）
    await git(bare, 'config', 'http.receivepack', 'true');
  }
  const execPath = await git(root, '--exec-path');
  const requests: GitRequest[] = [];
  const server = http.createServer((req, res) => {
    const u = new URL(req.url ?? '/', 'http://localhost');
    // 与 GitHub 相同：仓库名不区分大小写，可以不带 .git
    const p = /^\/([^/]+\/[^/]+?)(?:\.git)?(\/(?:info|HEAD|objects|git-upload-pack|git-receive-pack)(?:\/.*)?)$/.exec(u.pathname);
    const repo = p?.[1] ? `${p[1].toLowerCase()}.git` : '';
    /** pathInfo 是交给 http-backend 的路径（服务目录中的仓库都是小写、带 .git），不是 git 请求时为空。 */
    const pathInfo = p ? `/${repo}${p[2]}` : '';
    const write = u.searchParams.get('service') === 'git-receive-pack' || u.pathname.endsWith('/git-receive-pack');
    const [user, password] = basic(req.headers.authorization);
    const r: GitRequest = { repo, method: req.method ?? 'GET', write, user, password };
    requests.push(r);
    const held = opts.gate?.(r);
    if (held) {
      // 等待期间请求体照常缓冲（暂停读取），放行后再交给 http-backend
      req.pause();
      void held.then(() => serve(req, res, r, pathInfo));
      return;
    }
    serve(req, res, r, pathInfo);
  });
  const serve = (req: http.IncomingMessage, res: http.ServerResponse, r: GitRequest, pathInfo: string) => {
    if (res.destroyed) return;
    const u = new URL(req.url ?? '/', 'http://localhost');
    const drop = opts.dropResponse?.(r) ?? false;
    if (!pathInfo || !authorize(r)) {
      res.writeHead(401, { 'WWW-Authenticate': 'Basic realm="kiln-test"' });
      res.end();
      return;
    }
    const child = spawn(path.join(execPath, 'git-http-backend'), [], {
      env: {
        PATH: process.env.PATH,
        GIT_PROJECT_ROOT: project,
        GIT_HTTP_EXPORT_ALL: '1',
        PATH_INFO: pathInfo,
        QUERY_STRING: u.search.slice(1),
        REQUEST_METHOD: req.method ?? 'GET',
        CONTENT_TYPE: req.headers['content-type'] ?? '',
        HTTP_CONTENT_ENCODING: req.headers['content-encoding'] ?? '',
        GIT_PROTOCOL: String(req.headers['git-protocol'] ?? ''),
        REMOTE_USER: r.user || 'anonymous',
        REMOTE_ADDR: '127.0.0.1',
      },
      stdio: ['pipe', 'pipe', 'inherit'],
    });
    req.pipe(child.stdin);
    req.resume();
    let head = Buffer.alloc(0);
    let headersDone = false;
    child.stdout.on('data', (chunk: Buffer) => {
      if (drop) return;
      if (headersDone) {
        res.write(chunk);
        return;
      }
      head = Buffer.concat([head, chunk]);
      const end = head.indexOf('\r\n\r\n');
      if (end < 0) return;
      headersDone = true;
      let status = 200;
      const headers: Record<string, string> = {};
      for (const line of head.subarray(0, end).toString('utf8').split('\r\n')) {
        const i = line.indexOf(':');
        const k = line.slice(0, i).trim();
        const v = line.slice(i + 1).trim();
        if (k.toLowerCase() === 'status') status = Number(v.split(' ')[0]);
        else headers[k] = v;
      }
      res.writeHead(status, headers);
      res.write(head.subarray(end + 4));
    });
    child.on('close', () => (drop ? res.destroy() : res.end()));
  };
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const host = `127.0.0.1:${(server.address() as AddressInfo).port}`;
  return {
    host,
    url: (repo) => `http://${host}/${repo}.git`,
    requests,
    close: () =>
      new Promise((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      }),
  };
}

/** basic 解析 HTTP Basic 认证头，返回用户名和密码（没有时为空）。 */
function basic(h: string | undefined): [string, string] {
  const m = /^Basic (.+)$/i.exec(h ?? '');
  if (!m?.[1]) return ['', ''];
  const s = Buffer.from(m[1], 'base64').toString('utf8');
  const i = s.indexOf(':');
  return i < 0 ? [s, ''] : [s.slice(0, i), s.slice(i + 1)];
}
