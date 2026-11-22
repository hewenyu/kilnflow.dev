#!/usr/bin/env node
import { spawn } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';
import { createConnectTransport } from '@connectrpc/connect-node';
import { AgentProbe } from '../core/agents.js';
import { errMessage, RevokedError } from '../core/errors.js';
import { nodeExec } from '../core/exec.js';
import { Journal } from '../core/journal.js';
import { ConnectKiln } from '../core/kiln.js';
import { nopLogger } from '../core/log.js';
import { mailboxReport } from '../core/mailbox.js';
import { minOrcaVersion, OrcaCli } from '../core/orca.js';
import { DockerServices, homeOwner, prepareDockerConfig } from '../core/services.js';
import { bundledSkillDir, SkillManager } from '../core/skill.js';
import { applyToolchainEnv, parseEnv, Toolchain, type ToolchainEnv } from '../core/toolchain.js';
import { execEnvCmd } from '../core/attempt.js';
import { protocolVersion } from '../core/node.js';
import { credentialCmd, type ExecGit, execGitEnv, gitOpEnv, maxCredentialQueryBytes, parseCredentialQuery, readBounded, refusedCmd, remoteCmd } from '../core/credentials.js';
import { authConflicts, ensureExecMirror, machineCheckURL, realExecPath, safeURL } from '../core/gitexec.js';
import { doneCmd, doneReceiptCmd, doneReply, parseDoneSend, receiptArgs, sendWorkerDone } from '../core/done.js';
import { enroll } from './enroll.js';
import { canonicalDir, defaultHome, Home, serviceSettings, toolCommand } from './home.js';
import { lockHolder } from './lock.js';
import { accountHome, checkJournal, checkMachineAuth, offeredAgents, runConnector } from './run.js';
import { installService, restartService, serviceStatus, uninstallService } from './service.js';
import { callTool, openOp, ToolReplyTooLargeError } from './socket.js';
import { shellWord } from './escape.js';
import { kilnRunnerLocator } from './toolchain.js';

// kiln-orca：Kiln 的 AI 执行节点的连接器（PLAN #33.3）。

const usage = `用法：kiln-orca <命令> [--home <数据目录>]

  enroll --server <地址> --token <注册令牌> [--name <名称>]   注册这台机器
  run [--service]                                           运行连接器（通常由服务运行；--service 时日志只写文件）
  service install|uninstall|restart|status                  作为用户服务安装、卸载、重启、查看
  doctor                                                    检查注册、Server、Orca、Agent、Docker、journal
  drain on|off                                              停止 / 恢复领取新任务（正在执行的任务继续）
  task <info|check-report|claim|deliver|ci|doc> --attempt <id> [--sha <commit>]
  task doc list|get [<doc_id>] --attempt <id>   读取任务冻结的外部设计文档快照（依赖仓库绑定了文档集）
  task exec --attempt <id> -- <命令> [参数…]
  task done --attempt <id> --outcome succeeded|failed [--summary <一句话>]
                                                            给 Agent 用的命令（任务说明中给出完整命令）；exec 用任务的
                                                            受管理的工具链（Go、Node、pnpm 等）和 Kiln 签发的只读 git 凭据运行命令；
                                                            done 校验后向 Orca 报告这一轮结束
  version
`;

const cliPath = fileURLToPath(import.meta.url);

function packageVersion(): string {
  const p = path.join(path.dirname(cliPath), '..', '..', 'package.json');
  return (JSON.parse(readFileSync(p, 'utf8')) as { version: string }).version;
}

function transport(server: string) {
  return (interceptors: Parameters<typeof createConnectTransport>[0]['interceptors'] = []) =>
    createConnectTransport({ baseUrl: server, httpVersion: '1.1', interceptors });
}

async function cmdEnroll(home: Home, v: Record<string, string | boolean | undefined>): Promise<void> {
  const server = String(v.server ?? '').replace(/\/+$/, '');
  const token = String(v.token ?? '');
  if (!server || !token) throw new Error('需要 --server 和 --token');
  const st = await enroll(home, transport(server)(), server, token, String(v.name ?? ''), packageVersion());
  console.log(`已注册：${st.name}（id ${st.runnerId}，角色 ${st.role}），数据目录 ${home.dir}`);
  const pid = lockHolder(home.lockPath);
  if (pid !== undefined) {
    // 运行中的连接器在启动时读了注册信息，仍按之前的注册工作
    console.log(`这个数据目录的连接器正在运行（pid ${pid}），仍在使用之前的注册：kiln-orca service restart（前台运行的重新启动它）让新的注册生效。`);
  } else {
    console.log('下一步：kiln-orca doctor 检查环境，然后 kiln-orca service install 安装为服务（已经安装过的用 kiln-orca service restart）。');
  }
}

async function cmdRun(home: Home, service: boolean): Promise<void> {
  const ac = new AbortController();
  const stop = () => ac.abort();
  const signals = ['SIGTERM', 'SIGINT'] as const;
  // once：第二次信号按默认处理（立即结束），退出卡住时可以再按一次 Ctrl-C
  for (const sig of signals) process.once(sig, stop);
  try {
    await runConnector({
      home,
      transport: transport((await home.loadConfig()).server || (await home.loadState()).server),
      exec: nodeExec,
      version: packageVersion(),
      toolCommand: (h, id) => toolCommand(process.execPath, cliPath, h.dir, id),
      signal: ac.signal,
      service,
    });
  } catch (err) {
    // 被吊销：以 0 退出（与 kiln-runner 相同），服务管理器只在非 0 退出、崩溃时重启（service.ts），不会反复重启
    if (!(err instanceof RevokedError)) throw err;
  } finally {
    // 被吊销等不是由信号结束时，监听器还在：移除，之后的信号按默认处理
    for (const sig of signals) process.off(sig, stop);
  }
}

async function cmdDoctor(home: Home): Promise<boolean> {
  let allOk = true;
  const check = (ok: boolean, item: string, detail: string) => {
    if (!ok) allOk = false;
    console.log(`${ok ? '✓' : '✗'} ${item}：${detail}`);
  };
  const config = await home.loadConfig();
  check(true, '配置', `${home.path('config.json')}（max_tasks ${config.max_tasks}，Agent ${config.agents.join('、')}）`);
  // git 凭据由 Kiln Server 按操作签发（PLAN #33.13）：这台机器不需要、也不应有任务仓库主机的 git 凭据
  console.log(`- 仓库 clone 目录（clone_root）：${home.reposDir(config)}`);
  const auth = await checkMachineAuth({ exec: nodeExec }).catch((err: unknown) => [{ key: '检查失败', source: errMessage(err) }]);
  check(
    auth.length === 0,
    'git 认证配置',
    auth.length === 0
      ? `没有会对 ${machineCheckURL} 生效的 netrc 条目，全局 / 系统配置中也没有对它（包括其中的组织、仓库路径）声明的 http.extraHeader`
      : `${auth.map((c) => `${c.key}（来源：${c.source}）`).join('；')}。执行机器上不应有任务仓库主机的个人凭据（部署前提，PLAN #33.13）：去掉这些配置`,
  );
  try {
    const st = await home.loadState();
    check(true, '注册', `${st.name}（id ${st.runnerId}，角色 ${st.role}）`);
    const kiln = new ConnectKiln(transport(config.server || st.server), BigInt(st.runnerId), await home.key(false));
    try {
      await kiln.authenticate();
      check(true, '连接 Server', config.server || st.server);
    } catch (err) {
      check(false, '连接 Server', `${config.server || st.server}：${errMessage(err)}`);
    }
  } catch (err) {
    check(false, '注册', errMessage(err));
  }
  const orca = new OrcaCli(nodeExec, config.orca);
  const os_ = await orca.status();
  // status 同时检查最低版本：版本过低时这里失败，节点也不会领取
  check(os_.ready, 'Orca', os_.ready ? `${config.orca}（${os_.version}，要求 ${minOrcaVersion} 或更高）` : `${config.orca}：${os_.reason}`);
  if (os_.ready) {
    // 邮箱终端的状态和多余的邮箱终端（只读，PLAN #33.4）
    const handle = await home
      .identityStore()
      .load()
      .then(
        (id) => id?.handle ?? '',
        () => '',
      );
    for (const line of await mailboxReport(orca, home.dir, handle)) console.log(line);
  }
  const agents = new AgentProbe(nodeExec, offeredAgents(config.agents));
  await agents.refresh(true);
  for (const a of agents.snapshot()) check(a.available, `Agent ${a.name}`, a.available ? a.version : a.reason);
  // 随包分发的 kiln-task skill：只读检查，不创建目录；冲突时节点不领取任务，处理办法写在说明里
  const skills = new SkillManager({
    homeDir: os.homedir(),
    env: process.env,
    sourceDir: bundledSkillDir(),
    version: packageVersion(),
    protocol: protocolVersion,
    log: nopLogger,
  });
  for (const it of await skills.check(config.agents)) check(it.ok, `kiln-task skill（${it.agent}）`, it.detail);
  // kiln-runner 是可选的：没有时节点不领取声明了 requirements.tools 的任务（PLAN #33.12），其它任务不受影响
  const tc = await new Toolchain(nodeExec, kilnRunnerLocator(config.kiln_runner), home.toolchainsDir).status();
  console.log(`${tc.ok ? '✓' : '-'} kiln-runner（受管理的工具链，可选）：${tc.ok ? `${tc.bin}（${tc.version}），安装到 ${home.toolchainsDir}` : `${tc.reason}；声明了 requirements.tools 的任务不会派给这个节点`}`);
  const g = await nodeExec('git', ['--version']);
  check(g.code === 0, 'git', g.code === 0 ? g.stdout.trim() : '没有安装 git');
  // 与运行时一样用专用配置检测（生成在临时目录中，不改动运行中的连接器的配置）：配置生成失败时显示失败的步骤
  const dockerDir = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-docker-'));
  const docker = await new DockerServices(nodeExec, serviceSettings(config), () => prepareDockerConfig(dockerDir), homeOwner(home.dir))
    .available()
    .finally(() => rm(dockerDir, { recursive: true, force: true }));
  // Docker 是可选的：没有时需要测试服务的任务会在说明中告诉 Agent 测试服务不可用
  console.log(`${docker.ok ? '✓' : '-'} Docker（可选）：${docker.ok ? '可用' : docker.reason}`);
  const journal = await checkJournal(new Journal(home.journalDir));
  check(journal.ok, 'journal', journal.summary);
  for (const p of journal.problems) console.log(`    ${p}`);
  if (existsSync(home.drainFile)) console.log('- 已停止领取新任务（kiln-orca drain off 恢复）');
  const revoked = await home.loadState().then(
    (st) => home.isRevoked(st.runnerId),
    () => false,
  );
  if (revoked) check(false, '吊销', `这个节点已在 Kiln 中被吊销（${home.revokedFile}）：重新注册（kiln-orca enroll）后 kiln-orca service restart`);
  return allOk;
}

/**
 * cmdService 管理数据目录的服务。服务的身份只由数据目录决定（service.ts），只有 install 需要注册信息（服务运行的是已注册的
 * 节点）；uninstall、restart、status 在没有注册、重新注册改名之后都作用在同一个服务上。
 */
async function cmdService(home: Home, sub: 'install' | 'uninstall' | 'restart' | 'status'): Promise<number> {
  const logDir = home.path('logs');
  switch (sub) {
    case 'install': {
      const name = (await home.loadState()).name;
      const r = await installService(nodeExec, { name, home: home.dir, node: process.execPath, cli: cliPath, path: process.env.PATH ?? '', logDir });
      for (const f of r.replaced) console.log(`已停止并删除这个数据目录以前的服务定义：${f}`);
      console.log(`已安装并启动：${r.info.name}（${r.info.file}）`);
      return 0;
    }
    case 'uninstall': {
      const removed = await uninstallService(nodeExec, home.dir, logDir);
      console.log(removed.length > 0 ? `已卸载：${removed.join('、')}（数据目录 ${home.dir} 保留）` : `数据目录 ${home.dir} 没有安装服务`);
      return 0;
    }
    case 'restart': {
      const info = await restartService(nodeExec, home.dir, logDir);
      console.log(`已重启：${info.name}`);
      return 0;
    }
    case 'status': {
      const name = await home.loadState().then(
        (st) => st.name,
        () => '',
      );
      console.log(await serviceStatus(nodeExec, { name, home: home.dir }, logDir));
      return 0;
    }
  }
}

async function cmdTask(home: Home, cmd: string, v: Record<string, string | boolean | undefined>): Promise<number> {
  const attempt = String(v.attempt ?? '');
  if (!/^\d+$/.test(attempt)) throw new Error('需要 --attempt <id>');
  const args: Record<string, string> = {};
  if (typeof v.sha === 'string') args.sha = v.sha;
  try {
    const r = await callTool(home.socketPath, { attempt, cmd, args });
    console.log(r.output);
    return r.code;
  } catch (err) {
    if (err instanceof ToolReplyTooLargeError) console.log(`${err.message}：这是 kiln-orca 的问题，结果没有读到，不要当作成功。`);
    else console.log(`无法连接 kiln-orca（${errMessage(err)}）：稍后重试；连接器重启期间任务会被恢复。`);
    return 1;
  }
}

/**
 * cmdDoc 是 kiln-orca task doc（PLAN #33）：读取任务所属工作项冻结的外部设计文档快照。没有子命令或 list 时列出，
 * get <doc_id> 时返回正文；都经连接器（Agent 没有 Server token，连接器持有）。
 */
async function cmdDoc(home: Home, rest: string[], v: Record<string, string | boolean | undefined>): Promise<number> {
  const attempt = String(v.attempt ?? '');
  if (!/^\d+$/.test(attempt)) throw new Error('需要 --attempt <id>');
  const action = rest[0] ?? 'list';
  if (action !== 'list' && action !== 'get') return usageError(new Error('task doc 只支持 list 或 get <doc_id>'));
  const args: Record<string, string> = {};
  if (action === 'get') {
    const id = rest[1];
    if (!id) return usageError(new Error('task doc get 需要 <doc_id>'));
    args.id = id;
  }
  try {
    const r = await callTool(home.socketPath, { attempt, cmd: 'doc', args });
    console.log(r.output);
    return r.code;
  } catch (err) {
    if (err instanceof ToolReplyTooLargeError) console.log(`${err.message}：这是 kiln-orca 的问题，结果没有读到，不要当作成功。`);
    else console.log(`无法连接 kiln-orca（${errMessage(err)}）：稍后重试；连接器重启期间任务会被恢复。`);
    return 1;
  }
}

/**
 * cmdDone 是 kiln-orca task done（PLAN #33.5）：连接器校验通过、回复发送参数后，在本进程中（Agent 的后代，环境原样继承）
 * 不带 --from 执行 Orca 命令行发送 worker_done，把回执转成给 Agent 的一句话，回执摘要交回连接器记日志（交不回去不影响
 * 结果）。校验不通过时原样输出连接器的说明，不发送。
 */
async function cmdDone(home: Home, attempt: string, v: Record<string, string | boolean | undefined>): Promise<number> {
  const args = { outcome: typeof v.outcome === 'string' ? v.outcome : '', summary: typeof v.summary === 'string' ? v.summary : '' };
  let r;
  try {
    r = await callTool(home.socketPath, { attempt, cmd: doneCmd, args });
  } catch (err) {
    console.log(`无法连接 kiln-orca（${errMessage(err)}）：稍后再运行同一条命令；连接器重启期间任务会被恢复。`);
    return 1;
  }
  if (r.code !== 0) {
    console.log(r.output);
    return r.code;
  }
  let send;
  try {
    send = parseDoneSend(r.output);
  } catch (err) {
    console.log(`${errMessage(err)}：这是 kiln-orca 的问题，没有报告结束；稍后再运行同一条命令。`);
    return 1;
  }
  const { inv, receipt } = await sendWorkerDone(nodeExec, send, process.env);
  await callTool(home.socketPath, { attempt, cmd: doneReceiptCmd, args: receiptArgs(inv, receipt) }, receiptTimeoutMs).catch(() => undefined);
  const out = doneReply(inv, receipt);
  console.log(out.text);
  return out.code;
}

/** receiptTimeoutMs 是把回执交回连接器的等待上限（只记日志，等不到不影响结果）。 */
const receiptTimeoutMs = 10_000;

/**
 * cmdExec 用任务的工具链运行 Agent 的命令（kiln-orca task exec -- <命令>）：向连接器要这个 attempt 的环境变量（PATH 前面
 * 是受管理的目录、GOROOT、GOTOOLCHAIN=local 等），在本进程的终端和工作目录中运行，退出码原样返回（被信号结束时 128+n）。
 * 任务没有工具链时按原环境运行；连不上连接器时不运行（不退回到本机的工具，退出码 1）。
 */
async function cmdExec(home: Home, attempt: string, argv: string[]): Promise<number> {
  if (argv.length === 0) throw new Error('需要要运行的命令：task exec --attempt <id> -- <命令> [参数…]');
  let env: ToolchainEnv | undefined;
  let git: ExecGit | undefined;
  try {
    const r = await callTool(home.socketPath, { attempt, cmd: execEnvCmd, args: {} });
    if (r.code !== 0) {
      console.log(r.output);
      return r.code;
    }
    const v = JSON.parse(r.output) as { env?: unknown; git?: unknown };
    env = v.env === null || v.env === undefined ? undefined : parseEnv(v.env);
    git = v.git === null || v.git === undefined ? undefined : parseExecGit(v.git);
  } catch (err) {
    console.log(`无法从 kiln-orca 取得工具链和 git 设置（${errMessage(err)}）：稍后重试，不要改用本机安装的工具。`);
    return 1;
  }
  const base = env ? applyToolchainEnv(process.env, env) : process.env;
  if (!git) return runInherit(argv, base);
  // git 凭据（PLAN #33.13）：没有工具链时同样设置；HTTP(S) 传输经过 git exec 镜像中的传输包装，凭据由 helper 按这次传输的
  // 操作向连接器要，不在环境变量和参数中。镜像按这个环境中的 git 准备（Agent 的 PATH 可能与连接器的不同）
  const tool = toolCommand(process.execPath, cliPath, home.dir, BigInt(attempt));
  let execPath: string;
  try {
    const real = await realExecPath(nodeExec, base);
    execPath = await ensureExecMirror(git.execRoot, real, (helper) => remoteWrapper(tool, helper));
  } catch (err) {
    console.log(`无法准备 git 的传输包装（${errMessage(err)}）：命令没有运行。确认 PATH 中的 git 可用后重试。`);
    return 1;
  }
  return runInherit(argv, { ...base, ...execGitEnv(base, git, tool, execPath) });
}

/**
 * remoteWrapper 是 git exec 镜像中远程助手的替身（POSIX sh）：把 git 给的参数（remote 名、地址）原样交给
 * kiln-orca task git-remote，exec 之后包装就是这个进程。
 */
function remoteWrapper(tool: string, helper: string): string {
  return `#!/bin/sh\nexec ${tool} ${remoteCmd} --helper ${shellWord(helper)} -- "$@"\n`;
}

/** parseExecGit 校验 exec-env 回复中的 git 设置。 */
function parseExecGit(v: unknown): ExecGit {
  const o = v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
  if (!o || typeof o.helperURL !== 'string' || typeof o.execRoot !== 'string' || !Array.isArray(o.goprivate) || !o.goprivate.every((x) => typeof x === 'string')) {
    throw new Error('连接器回复的 git 设置格式不对');
  }
  return { helperURL: o.helperURL, goprivate: o.goprivate as string[], execRoot: o.execRoot };
}

/**
 * cmdGitRemote 是 git exec 镜像中的传输包装（git 执行 git-remote-http(s) <remote> <地址> 时运行，PLAN #33.13）：
 * 1. 检查认证冲突（地址带凭据、生效的 http.extraheader、netrc 中有这个主机，或者查询失败）：有时拒绝这一次传输（不签发），
 *    说明写到 stderr、告诉连接器记 warn 日志，只有键和来源，没有值；
 * 2. 经本机 socket 向连接器开一个操作并保持连接；
 * 3. 以操作 id（KILN_ORCA_GIT_OP）启动真实的远程助手（stdin、stdout 是 git 的远程助手协议，原样继承），等它退出；
 * 4. 关闭连接：操作随之结束，连接器吊销这次传输的凭据。包装被杀时连接由内核关闭，效果相同。
 * 本进程不写 stdout（那是给 git 的协议）。
 */
async function cmdGitRemote(home: Home, attempt: string, helper: string, argv: string[]): Promise<number> {
  const [remote, given] = argv;
  if (!remote || !helper) {
    process.stderr.write('kiln-orca：git-remote 要由 git 调用：<remote> [<地址>]\n');
    return 2;
  }
  const url = given ?? remote;
  const conflicts = await authConflicts(url, { exec: nodeExec, env: process.env, platform: process.platform, accountHome: accountHome() });
  if (conflicts.length > 0) {
    const shown = safeURL(url);
    const text = conflicts.map((c) => `${c.key}（来源：${c.source}）`).join('；');
    process.stderr.write(`kiln-orca：拒绝这次 git 传输（${shown}）：执行机器上有会带上别的凭据的配置：${text}。Kiln 不使用执行机器上的个人凭据，去掉这些配置后重试。\n`);
    await callTool(home.socketPath, { attempt, cmd: refusedCmd, args: { url: shown, conflicts: text } }, refusedTimeoutMs).catch(() => undefined);
    return 1;
  }
  let conn;
  try {
    conn = await openOp(home.socketPath, { attempt, url, pid: String(process.pid) });
  } catch (err) {
    process.stderr.write(`kiln-orca：无法连接 kiln-orca 连接器（${errMessage(err)}），这次 git 传输没有执行：稍后重试。\n`);
    return 1;
  }
  if (conn.reply.code !== 0) {
    conn.close();
    process.stderr.write(`kiln-orca：${conn.reply.output}\n`);
    return conn.reply.code;
  }
  const env: NodeJS.ProcessEnv = { ...process.env };
  delete env[gitOpEnv];
  if (conn.reply.op) env[gitOpEnv] = conn.reply.op;
  try {
    return await runInherit([helper, remote, ...(given === undefined ? [] : [given])], env);
  } finally {
    conn.close();
  }
}

/** refusedTimeoutMs 是把拒绝的传输告诉连接器的等待上限（只记日志，等不到不影响结果）。 */
const refusedTimeoutMs = 10_000;

/**
 * cmdCredential 是 kiln-orca task exec 中 git 的 credential helper（git 在命令后加上 get、store、erase，从 stdin 给出请求）：
 * 只处理 get，带着这次传输的操作（传输包装设置的 KILN_ORCA_GIT_OP）向连接器要凭据（连接器只回答还没有结束、仓库一致的
 * 操作），按 helper 协议写到 stdout；store、erase 什么都不做（不落盘）。没有操作（不经传输包装）、出错时什么都不回答
 * （说明写到 stderr，git 不用凭据继续，按仓库的权限失败）。
 */
async function cmdCredential(home: Home, attempt: string, action: string | undefined): Promise<number> {
  if (action !== 'get') return 0;
  const input = await readBounded(process.stdin, maxCredentialQueryBytes);
  if (input === undefined) {
    process.stderr.write(`kiln-orca：git 的凭据请求超过 ${maxCredentialQueryBytes} 字节，不给凭据\n`);
    return 0;
  }
  const q = parseCredentialQuery(input);
  const op = process.env[gitOpEnv] ?? '';
  if (!op) {
    process.stderr.write('kiln-orca：这个 git 请求不属于经 kiln-orca task exec 的传输，不给凭据\n');
    return 0;
  }
  try {
    const r = await callTool(home.socketPath, { attempt, cmd: credentialCmd, args: { op, protocol: q.protocol, host: q.host, path: q.path } });
    if (r.code === 0) process.stdout.write(r.output);
    else process.stderr.write(`kiln-orca：${r.output}\n`);
  } catch (err) {
    process.stderr.write(`kiln-orca：无法从连接器取得 git 凭据（${errMessage(err)}）\n`);
  }
  return 0;
}

/** runInherit 运行命令（继承终端），转发 SIGINT、SIGTERM，返回它的退出码。 */
function runInherit(argv: string[], env: NodeJS.ProcessEnv): Promise<number> {
  return new Promise((resolve) => {
    const child = spawn(argv[0] as string, argv.slice(1), { stdio: 'inherit', env });
    const forward = (sig: NodeJS.Signals) => child.kill(sig);
    const signals = ['SIGINT', 'SIGTERM'] as const;
    for (const sig of signals) process.on(sig, forward);
    const done = (code: number) => {
      for (const sig of signals) process.off(sig, forward);
      resolve(code);
    };
    child.on('error', (err) => {
      process.stderr.write(`kiln-orca：无法运行 ${argv[0]}：${errMessage(err)}\n`);
      done(127);
    });
    child.on('exit', (code, signal) => done(code ?? 128 + (signal ? (os.constants.signals[signal] ?? 0) : 0)));
  });
}

const cliOptions = {
  home: { type: 'string' },
  server: { type: 'string' },
  token: { type: 'string' },
  name: { type: 'string' },
  attempt: { type: 'string' },
  sha: { type: 'string' },
  outcome: { type: 'string' },
  helper: { type: 'string' },
  summary: { type: 'string' },
  service: { type: 'boolean' },
  help: { type: 'boolean', short: 'h' },
} as const;

/** usageError 是用法错误（不认识的选项、缺少选项的值）：打印说明和用法，退出码 2。 */
function usageError(err: unknown): number {
  process.stderr.write(`kiln-orca：${errMessage(err)}\n\n${usage}`);
  return 2;
}

async function main(argv: string[]): Promise<number> {
  // "--" 之后是 task exec 要运行的命令（原样传递，不解析其中的选项）
  const sep = argv.indexOf('--');
  const command = sep < 0 ? undefined : argv.slice(sep + 1);
  let parsed;
  try {
    parsed = parseArgs({ args: sep < 0 ? argv : argv.slice(0, sep), allowPositionals: true, options: cliOptions });
  } catch (err) {
    // parseArgs 的错误（ERR_PARSE_ARGS_*）都是用法错误
    if (err instanceof Error && 'code' in err && String(err.code).startsWith('ERR_PARSE_ARGS')) return usageError(err);
    throw err;
  }
  const { values: v, positionals: pos } = parsed;
  const home = new Home(canonicalDir(v.home ?? defaultHome()));
  const [cmd, sub] = pos;
  switch (cmd) {
    case 'enroll':
      await cmdEnroll(home, v);
      return 0;
    case 'run':
      await cmdRun(home, v.service === true);
      return 0;
    case 'service':
      if (sub === 'install' || sub === 'uninstall' || sub === 'restart' || sub === 'status') return cmdService(home, sub);
      break;
    case 'doctor':
      return (await cmdDoctor(home)) ? 0 : 1;
    case 'drain':
      if (sub === 'on') {
        await home.setDrain(true);
        console.log('已停止领取新任务，正在执行的任务继续');
        return 0;
      }
      if (sub === 'off') {
        await home.setDrain(false);
        console.log('已恢复领取');
        return 0;
      }
      break;
    case 'task':
      if (sub === 'exec') {
        if (!/^\d+$/.test(String(v.attempt ?? ''))) throw new Error('需要 --attempt <id>');
        if (!command || pos.length > 2) return usageError(new Error('task exec 要运行的命令写在 -- 之后'));
        return cmdExec(home, String(v.attempt), command);
      }
      if (sub === doneCmd) {
        if (!/^\d+$/.test(String(v.attempt ?? ''))) throw new Error('需要 --attempt <id>');
        if (command || pos.length > 2) return usageError(new Error('task done 只接受 --outcome 和 --summary'));
        return cmdDone(home, String(v.attempt), v);
      }
      if (sub === credentialCmd) {
        if (!/^\d+$/.test(String(v.attempt ?? ''))) throw new Error('需要 --attempt <id>');
        return cmdCredential(home, String(v.attempt), pos[2]);
      }
      if (sub === remoteCmd) {
        if (!/^\d+$/.test(String(v.attempt ?? ''))) throw new Error('需要 --attempt <id>');
        return cmdGitRemote(home, String(v.attempt), String(v.helper ?? ''), command ?? []);
      }
      if (sub === 'doc') return cmdDoc(home, pos.slice(2), v);
      if (sub && !command) return cmdTask(home, sub, v);
      break;
    case 'version':
      console.log(packageVersion());
      return 0;
  }
  process.stderr.write(usage);
  return v.help ? 0 : 2;
}

// 结束时只设置退出码，让进程在输出写完、没有活动的句柄后自然退出：stdout / stderr 是管道时写入是异步的，process.exit
// 会丢掉还没写进管道的部分（Agent 经管道读 kiln-orca task 的输出）。各命令自己关闭用到的句柄（定时器、socket、监听器），
// 不靠强制退出。
main(process.argv.slice(2)).then(
  (code) => {
    process.exitCode = code;
  },
  (err: unknown) => {
    process.stderr.write(`kiln-orca：${errMessage(err)}\n`);
    process.exitCode = 1;
  },
);
