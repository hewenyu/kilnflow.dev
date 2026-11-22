import { existsSync } from 'node:fs';
import { mkdir, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { type Exec, must } from '../core/exec.js';
import { homeOwner } from '../core/services.js';
import { sdEnvironment, sdExecStart, sdText, xmlComment, xmlText } from './escape.js';
import { serviceLogs } from './logger.js';

// 作为用户服务安装（PLAN #33.3）：macOS 用 launchd 的 LaunchAgent，Linux 用 systemd --user。必须是用户级服务：
// Orca、Agent 的登录状态、git 和 Docker 的凭据都在这个用户下。安装时记录当前的 PATH，服务中能找到同样的 orca、
// codex、claude、git、docker。服务运行 run --service：正常日志只写按日轮转的文件，stderr 只留给启动失败和崩溃（logger.ts）。
//
// 重启规则（两个平台相同）：只在非 0 退出和被信号结束（崩溃、SIGKILL）时重启，以 0 退出时不重启。连接器只在收到退出
// 信号（服务管理器自己在停止它）和被吊销并清理完（cli.ts）时以 0 退出，所以被吊销的节点不会被反复拉起、反复联系 Server；
// 清理中崩溃时照常被重启，以吊销状态继续清理（不联系 Server）。launchd 用 KeepAlive 的 SuccessfulExit=false，
// systemd 用 Restart=on-failure。
//
// 一个数据目录是一个节点，也是一个服务：服务标识只由数据目录决定（homeOwner，数据目录规范路径的 sha256 前 12 位），
// 改名、重新注册都不改变它（launchd label dev.kiln.orca.<标识>、systemd 单元 kiln-orca-<标识>.service）；节点的名称只出现在
// 不参与身份的地方（systemd 的 Description=、plist 的注释、service status 的输出）。服务定义中记下数据目录（数据目录标记），
// 覆盖、卸载之前核对；install、uninstall 还按标记找出同一个数据目录的其它服务定义（例如以前的格式装的）一并停止、删除，
// 不会留下两个服务抢同一个数据目录的锁。

const labelPrefix = 'dev.kiln.orca';
const unitPrefix = 'kiln-orca-';
/** homeMarker 标记服务定义所属的数据目录（base64，不会出现 XML 注释不允许的 "--"）。 */
const homeMarker = 'kiln-orca-home:';
const homeMarkerPattern = new RegExp(`${homeMarker} ([A-Za-z0-9+/=]+)`);

/** ServiceNode 是一个节点：它的数据目录（服务的身份）和现在注册的名称（只用于显示，没有注册时为空）。 */
export interface ServiceNode {
  name: string;
  home: string;
}

/** serviceId 是数据目录的服务标识。 */
export function serviceId(home: string): string {
  return homeOwner(home);
}

/** ServiceHost 是安装服务的平台和服务定义所在的目录（测试中换成临时目录）。 */
export interface ServiceHost {
  platform: NodeJS.Platform;
  /** LaunchAgents 目录或 systemd 用户单元目录。 */
  unitDir: string;
  uid: number;
}

/** currentHost 返回本机的 ServiceHost。 */
export function currentHost(): ServiceHost {
  const unitDir =
    process.platform === 'darwin'
      ? path.join(os.homedir(), 'Library', 'LaunchAgents')
      : path.join(process.env.XDG_CONFIG_HOME || path.join(os.homedir(), '.config'), 'systemd', 'user');
  return { platform: process.platform, unitDir, uid: process.getuid?.() ?? 0 };
}

/** ServiceInfo 是数据目录的服务：launchd label 或 systemd 单元名、服务定义文件、管理它的命令。 */
export interface ServiceInfo {
  name: string;
  file: string;
  /** restart、status 的等价命令（给人看）。 */
  commands: { restart: string; status: string; logs: string };
}

/** ServiceDef 是 unitDir 中的一份服务定义：服务管理器中的名字（launchd label、systemd 单元名）和文件。 */
interface ServiceDef {
  name: string;
  file: string;
}

export function serviceInfo(h: ServiceHost, home: string, logDir: string): ServiceInfo {
  const id = serviceId(home);
  if (h.platform === 'darwin') {
    const name = `${labelPrefix}.${id}`;
    const target = `${launchdDomain(h)}/${name}`;
    return {
      name,
      file: path.join(h.unitDir, `${name}.plist`),
      commands: { restart: `launchctl kickstart -k ${target}`, status: `launchctl print ${target}`, logs: logDir },
    };
  }
  const name = `${unitPrefix}${id}.service`;
  return {
    name,
    file: path.join(h.unitDir, name),
    commands: { restart: `systemctl --user restart ${name}`, status: `systemctl --user status ${name}`, logs: `journalctl --user -u ${name}` },
  };
}

export interface ServiceSpec extends ServiceNode {
  node: string;
  cli: string;
  path: string;
  logDir: string;
}

function marker(home: string): string {
  return `${homeMarker} ${Buffer.from(path.resolve(home)).toString('base64')}`;
}

/** definedHome 返回服务定义中记下的数据目录（不是 kiln-orca 写的定义时返回 undefined）。 */
export function definedHome(content: string): string | undefined {
  const m = homeMarkerPattern.exec(content);
  return m?.[1] === undefined ? undefined : Buffer.from(m[1], 'base64').toString('utf8');
}

/** runArgs 是服务启动的命令行。 */
function runArgs(s: ServiceSpec): string[] {
  return [s.node, s.cli, 'run', '--service', '--home', s.home];
}

/** nodeLabel 是给人看的节点说明（名称可能为空）。 */
function nodeLabel(name: string): string {
  return name ? `Kiln AI 执行节点 ${name}` : 'Kiln AI 执行节点';
}

export function launchdPlist(s: ServiceSpec): string {
  const args = runArgs(s)
    .map((a) => `    <string>${xmlText(a, '服务的命令行')}</string>`)
    .join('\n');
  return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- ${marker(s.home)} -->
<!-- ${xmlComment(nodeLabel(s.name))}（kiln-orca） -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${labelPrefix}.${serviceId(s.home)}</string>
  <key>ProgramArguments</key>
  <array>
${args}
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>${xmlText(s.path, 'PATH')}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>${xmlText(path.join(s.logDir, serviceLogs.out), '日志路径')}</string>
  <key>StandardErrorPath</key>
  <string>${xmlText(path.join(s.logDir, serviceLogs.err), '日志路径')}</string>
</dict>
</plist>
`;
}

export function systemdUnit(s: ServiceSpec): string {
  return `# ${marker(s.home)}
[Unit]
Description=${sdText(nodeLabel(s.name))}（kiln-orca）
After=network-online.target

[Service]
ExecStart=${sdExecStart(s.node, runArgs(s).slice(1))}
Environment=${sdEnvironment('PATH', s.path)}
Restart=on-failure
RestartSec=10
KillSignal=SIGTERM
TimeoutStopSec=60

[Install]
WantedBy=default.target
`;
}

function supported(h: ServiceHost): void {
  if (h.platform !== 'darwin' && h.platform !== 'linux') throw new Error(`不支持在 ${h.platform} 上安装服务：请自行用 kiln-orca run 常驻运行`);
}

/**
 * checkOwner 确认这个数据目录的服务定义文件（如果存在）确实属于它。服务标识由数据目录决定，不同数据目录不会走到同一个
 * 文件；这里防的是标识碰撞和不是 kiln-orca 写的同名文件：不属于时报错，不覆盖、不停止别人的服务。
 */
async function checkOwner(info: ServiceInfo, home: string): Promise<void> {
  if (!existsSync(info.file)) return;
  const owner = definedHome(await readFile(info.file, 'utf8'));
  if (owner !== path.resolve(home)) {
    throw new Error(`${info.file} 不是数据目录 ${path.resolve(home)} 的服务（${owner ? `属于 ${owner}` : '不是 kiln-orca 安装的'}），没有改动它`);
  }
}

/** plistLabel 取 plist 中的 Label（只认 kiln-orca 写的格式）。 */
function plistLabel(content: string): string | undefined {
  const m = /<key>Label<\/key>\s*<string>([^<]*)<\/string>/.exec(content);
  return m?.[1];
}

/**
 * definitionsOf 按数据目录标记找出 unitDir 中属于 home 的服务定义（不按文件名猜：包括以前的格式、别的文件名的）。
 * 读不了的文件跳过（不是这个数据目录的）。
 */
async function definitionsOf(h: ServiceHost, home: string): Promise<ServiceDef[]> {
  const ext = h.platform === 'darwin' ? '.plist' : '.service';
  let files: string[];
  try {
    files = (await readdir(h.unitDir)).filter((f) => f.endsWith(ext)).sort();
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return [];
    throw err;
  }
  const want = path.resolve(home);
  const defs: ServiceDef[] = [];
  for (const f of files) {
    const file = path.join(h.unitDir, f);
    const content = await readFile(file, 'utf8').catch(() => '');
    if (definedHome(content) !== want) continue;
    // launchd 按 plist 中的 Label 管理（与文件名无关）；systemd 的单元名就是文件名
    const name = h.platform === 'darwin' ? plistLabel(content) : f;
    if (name) defs.push({ name, file });
  }
  return defs;
}

function launchdDomain(h: ServiceHost): string {
  return `gui/${h.uid}`;
}

/** stopAndRemove 停止并删除一份服务定义（没有加载、没有运行时停止失败，忽略）。systemd 之后要 daemon-reload。 */
async function stopAndRemove(exec: Exec, h: ServiceHost, d: ServiceDef): Promise<void> {
  if (h.platform === 'darwin') await exec('launchctl', ['bootout', `${launchdDomain(h)}/${d.name}`]);
  else await exec('systemctl', ['--user', 'disable', '--now', d.name]);
  await rm(d.file, { force: true });
}

/** InstallResult 是安装的服务和被替换掉的同一个数据目录的其它服务定义。 */
export interface InstallResult {
  info: ServiceInfo;
  replaced: string[];
}

/**
 * installService 安装并启动数据目录的服务。重复安装时覆盖自己的服务定义并重启（新的定义立即生效）；同一个数据目录的
 * 其它服务定义（按数据目录标记找到的）先停止、删除。
 */
export async function installService(exec: Exec, s: ServiceSpec, h: ServiceHost = currentHost()): Promise<InstallResult> {
  supported(h);
  const info = serviceInfo(h, s.home, s.logDir);
  await checkOwner(info, s.home);
  // 先生成（不能表示的路径在改动任何东西之前报错）
  const content = h.platform === 'darwin' ? launchdPlist(s) : systemdUnit(s);
  const others = (await definitionsOf(h, s.home)).filter((d) => d.file !== info.file);
  await mkdir(h.unitDir, { recursive: true });
  await mkdir(s.logDir, { recursive: true, mode: 0o700 });
  for (const d of others) await stopAndRemove(exec, h, d);
  if (h.platform === 'darwin') {
    // 没有加载时 bootout 失败，忽略
    await exec('launchctl', ['bootout', `${launchdDomain(h)}/${info.name}`]);
    await writeFile(info.file, content);
    await must(exec, 'launchctl', ['bootstrap', launchdDomain(h), info.file]);
  } else {
    await writeFile(info.file, content);
    await must(exec, 'systemctl', ['--user', 'daemon-reload']);
    await must(exec, 'systemctl', ['--user', 'enable', info.name]);
    // enable --now 不会重启已经在运行的服务：restart 让新的定义生效（没有运行时启动它）
    await must(exec, 'systemctl', ['--user', 'restart', info.name]);
  }
  return { info, replaced: others.map((d) => d.file) };
}

/**
 * uninstallService 停止并删除数据目录的服务（包括按数据目录标记找到的其它服务定义），返回删除的定义文件（没有时为空）。
 * 没有定义文件时也停止一次这个数据目录的服务（定义被手动删了而服务还加载着）。数据目录本身不动（注册信息、journal、日志）。
 */
export async function uninstallService(exec: Exec, home: string, logDir: string, h: ServiceHost = currentHost()): Promise<string[]> {
  supported(h);
  const info = serviceInfo(h, home, logDir);
  await checkOwner(info, home);
  const defs = await definitionsOf(h, home);
  const removed = defs.map((d) => d.file);
  if (!removed.includes(info.file)) defs.push({ name: info.name, file: info.file });
  for (const d of defs) await stopAndRemove(exec, h, d);
  if (h.platform === 'linux') await exec('systemctl', ['--user', 'daemon-reload']);
  return removed;
}

/** notInstalled 是数据目录还没有服务时的说明（有别的格式的定义时提示 install 替换）。 */
async function notInstalled(h: ServiceHost, info: ServiceInfo, home: string): Promise<string> {
  const others = await definitionsOf(h, home);
  const hint = others.length > 0 ? `；这个数据目录有以前的服务定义 ${others.map((d) => d.file).join('、')}，kiln-orca service install 会替换它` : '：先运行 kiln-orca service install';
  return `数据目录 ${path.resolve(home)} 还没有安装服务（${info.file} 不存在）${hint}`;
}

/** restartService 重启服务（升级、改了配置后用）；还没有安装时报错。 */
export async function restartService(exec: Exec, home: string, logDir: string, h: ServiceHost = currentHost()): Promise<ServiceInfo> {
  supported(h);
  const info = serviceInfo(h, home, logDir);
  await checkOwner(info, home);
  if (!existsSync(info.file)) throw new Error(await notInstalled(h, info, home));
  if (h.platform === 'darwin') await must(exec, 'launchctl', ['kickstart', '-k', `${launchdDomain(h)}/${info.name}`]);
  else await must(exec, 'systemctl', ['--user', 'restart', info.name]);
  return info;
}

/** serviceStatus 返回节点、服务标识、服务定义文件、管理命令和服务管理器给出的状态。 */
export async function serviceStatus(exec: Exec, n: ServiceNode, logDir: string, h: ServiceHost = currentHost()): Promise<string> {
  supported(h);
  const info = serviceInfo(h, n.home, logDir);
  await checkOwner(info, n.home);
  const head = [`节点：${n.name || '（还没有注册）'}`, `数据目录：${path.resolve(n.home)}`, `服务：${info.name}`];
  if (!existsSync(info.file)) return [...head, '', await notInstalled(h, info, n.home)].join('\n');
  const r =
    h.platform === 'darwin'
      ? await exec('launchctl', ['print', `${launchdDomain(h)}/${info.name}`])
      : await exec('systemctl', ['--user', 'status', '--no-pager', info.name]);
  const state = r.code !== 0 && h.platform === 'darwin' ? '没有加载或没有运行' : (r.stdout || r.stderr).trim();
  const others = (await definitionsOf(h, n.home)).filter((d) => d.file !== info.file);
  return [
    ...head,
    `定义：${info.file}`,
    ...others.map((d) => `另有：${d.file}（同一个数据目录以前的服务定义，kiln-orca service install 会替换它）`),
    `重启：${info.commands.restart}`,
    `状态：${info.commands.status}`,
    `日志：${info.commands.logs}`,
    '',
    state,
  ].join('\n');
}
