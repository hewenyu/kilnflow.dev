import { execFile } from 'node:child_process';
import { existsSync } from 'node:fs';
import { mkdir, mkdtemp, readdir, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { promisify } from 'node:util';
import { afterEach, describe, expect, it } from 'vitest';
import type { Exec } from '../src/core/exec.js';
import { homeOwner } from '../src/core/services.js';
import { sdExecStart, shellWord, xmlComment, xmlText } from '../src/host/escape.js';
import { canonicalDir, toolCommand } from '../src/host/home.js';
import {
  definedHome,
  installService,
  launchdPlist,
  restartService,
  type ServiceHost,
  serviceId,
  serviceInfo,
  serviceStatus,
  type ServiceSpec,
  systemdUnit,
  uninstallService,
} from '../src/host/service.js';

// 服务安装：一个数据目录 = 一个节点 = 一个服务，服务的身份只由数据目录决定。launchd、
// systemd 用记录调用的 Exec 替身，服务定义写到临时目录，不碰本机的服务管理器。

const run = promisify(execFile);

let root = '';
afterEach(async () => {
  if (root) await rm(root, { recursive: true, force: true });
  root = '';
});

function recorder() {
  const calls: string[][] = [];
  const exec: Exec = (cmd, args) => {
    calls.push([cmd, ...args]);
    return Promise.resolve({ code: 0, stdout: '', stderr: '' });
  };
  return { calls, exec };
}

function spec(name: string, home: string): ServiceSpec {
  return { name, home: path.join(root, home), node: '/n/node', cli: '/n/cli.js', path: '/usr/bin', logDir: path.join(root, home, 'logs') };
}

/** unitFiles 是 unitDir 中的服务定义文件。 */
async function unitFiles(h: ServiceHost): Promise<string[]> {
  return existsSync(h.unitDir) ? (await readdir(h.unitDir)).sort() : [];
}

/** 名称归一后相同的节点对（都符合 Dashboard 的 enrollNamePattern）。 */
const pairs: Array<[string, string]> = [
  ['mini.ai', 'mini-ai'],
  ['Mini-AI', 'mini-ai'],
  ['a_b', 'a-b'],
];

describe.each(['darwin', 'linux'] as const)('服务安装（%s）', (platform) => {
  async function host(): Promise<ServiceHost> {
    root = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-svc-'));
    return { platform, unitDir: path.join(root, 'units'), uid: 501 };
  }
  /** oldFormat 是以前的格式（标识含名称）装的同一个数据目录的服务定义：文件名、label 都不同，数据目录标记相同。 */
  function oldFormat(s: ServiceSpec): { name: string; file: string; content: string } {
    const id = `old-name-${homeOwner(s.home)}`;
    const current = serviceId(s.home);
    if (platform === 'darwin') {
      const name = `dev.kiln.orca.${id}`;
      return { name, file: `${name}.plist`, content: launchdPlist(s).replace(`dev.kiln.orca.${current}`, name) };
    }
    const name = `kiln-orca-${id}.service`;
    return { name, file: name, content: systemdUnit(s) };
  }

  it.each(pairs)('%s 与 %s 在不同数据目录：服务标识、定义文件都不同，第二次安装不停止、不覆盖第一个', async (a, b) => {
    const h = await host();
    const { calls, exec } = recorder();
    const { info: first } = await installService(exec, spec(a, 'review'), h);
    const firstContent = await readFile(first.file, 'utf8');
    const before = calls.length;
    const { info: second } = await installService(exec, spec(b, 'dev'), h);

    expect(second.name).not.toBe(first.name);
    expect(second.file).not.toBe(first.file);
    // 第一个服务的定义原样保留，第二次安装的命令都不涉及它
    expect(await readFile(first.file, 'utf8')).toBe(firstContent);
    expect(definedHome(firstContent)).toBe(path.join(root, 'review'));
    expect(definedHome(await readFile(second.file, 'utf8'))).toBe(path.join(root, 'dev'));
    const secondCalls = calls.slice(before).map((c) => c.join(' '));
    expect(secondCalls.length).toBeGreaterThan(0);
    for (const c of secondCalls) expect(c).not.toContain(first.name);
    expect(secondCalls.some((c) => c.includes(second.name))).toBe(true);

    // 卸载、重启、查看只操作自己的服务
    const opBefore = calls.length;
    const dev = path.join(root, 'dev');
    await restartService(exec, dev, path.join(dev, 'logs'), h);
    await serviceStatus(exec, spec(b, 'dev'), path.join(dev, 'logs'), h);
    expect(await uninstallService(exec, dev, path.join(dev, 'logs'), h)).toEqual([second.file]);
    for (const c of calls.slice(opBefore).map((x) => x.join(' '))) expect(c).not.toContain(first.name);
    expect(existsSync(second.file)).toBe(false);
    expect(await readFile(first.file, 'utf8')).toBe(firstContent);
  });

  it('两个数据目录用同一个名称：两个服务', async () => {
    const h = await host();
    const { exec } = recorder();
    const a = await installService(exec, spec('same', 'one'), h);
    const b = await installService(exec, spec('same', 'two'), h);
    expect(a.info.name).not.toBe(b.info.name);
    expect(await unitFiles(h)).toEqual([path.basename(a.info.file), path.basename(b.info.file)].sort());
  });

  it('同一个数据目录改名（重新注册）：install、restart、status、uninstall 都作用在同一个服务上，目录里始终只有一份定义', async () => {
    const h = await host();
    const { calls, exec } = recorder();
    const old = spec('old-name', 'review');
    const renamed = { ...old, name: 'new-name' };
    const { info } = await installService(exec, old, h);
    // 服务标识只由数据目录决定
    expect(info.name).toBe(platform === 'darwin' ? `dev.kiln.orca.${homeOwner(old.home)}` : `kiln-orca-${homeOwner(old.home)}.service`);
    expect(info.name).not.toContain('old-name');
    expect(await unitFiles(h)).toEqual([path.basename(info.file)]);

    const before = calls.length;
    await restartService(exec, renamed.home, renamed.logDir, h);
    const status = await serviceStatus(exec, renamed, renamed.logDir, h);
    expect(status).toContain(`服务：${info.name}`);
    expect(status).toContain('节点：new-name');
    const again = await installService(exec, renamed, h);
    expect(again).toEqual({ info, replaced: [] });
    expect(await unitFiles(h)).toEqual([path.basename(info.file)]);
    // 新的名称只在说明中
    const content = await readFile(info.file, 'utf8');
    expect(content).toContain('new-name');
    expect(content).not.toContain('old-name');
    // 所有管理命令都只涉及这一个服务
    const named = calls.slice(before).filter((c) => c.some((x) => x.includes('kiln')));
    expect(named.length).toBeGreaterThan(0);
    for (const c of named) expect(c.join(' ')).toContain(info.name);

    expect(await uninstallService(exec, renamed.home, renamed.logDir, h)).toEqual([info.file]);
    expect(await unitFiles(h)).toEqual([]);
  });

  it('重复安装：覆盖自己的服务定义并让新的定义生效（launchd 先 bootout，systemd 重启）', async () => {
    const h = await host();
    const { calls, exec } = recorder();
    const s = spec('mini-ai', 'review');
    const a = await installService(exec, s, h);
    const b = await installService(exec, { ...s, path: '/usr/bin:/opt/new' }, h);
    expect(b).toEqual(a);
    expect(await readFile(a.info.file, 'utf8')).toContain('/opt/new');
    if (platform === 'darwin') {
      expect(calls.filter((c) => c[1] === 'bootout').map((c) => c[2])).toEqual([`gui/501/${a.info.name}`, `gui/501/${a.info.name}`]);
    } else {
      // enable --now 不重启已经在运行的服务
      expect(calls.filter((c) => c[2] === 'restart').map((c) => c[3])).toEqual([a.info.name, a.info.name]);
      expect(calls.some((c) => c.includes('--now') && c.includes('enable'))).toBe(false);
    }
  });

  it('同一个数据目录有以前格式的服务定义：install 按数据目录标记找到它，停止、删除后换成新的；别的数据目录的定义不动', async () => {
    const h = await host();
    const { calls, exec } = recorder();
    const s = spec('new-name', 'review');
    const legacy = oldFormat({ ...s, name: 'old-name' });
    const other = spec('old-name', 'other');
    const otherLegacy = oldFormat(other);
    await mkdir(h.unitDir, { recursive: true });
    await writeFile(path.join(h.unitDir, legacy.file), legacy.content);
    await writeFile(path.join(h.unitDir, `x-${otherLegacy.file}`), otherLegacy.content);
    // 不是 kiln-orca 写的文件也不动
    await writeFile(path.join(h.unitDir, `unrelated${platform === 'darwin' ? '.plist' : '.service'}`), '别人的服务\n');

    // status 指出以前的定义
    expect(await serviceStatus(exec, s, s.logDir, h)).toContain(path.join(h.unitDir, legacy.file));
    await expect(restartService(exec, s.home, s.logDir, h)).rejects.toThrow(/还没有安装服务.*service install 会替换/);

    const r = await installService(exec, s, h);
    expect(r.replaced).toEqual([path.join(h.unitDir, legacy.file)]);
    expect(await unitFiles(h)).toEqual([path.basename(r.info.file), `unrelated${platform === 'darwin' ? '.plist' : '.service'}`, `x-${otherLegacy.file}`].sort());
    const stop = platform === 'darwin' ? ['launchctl', 'bootout', `gui/501/${legacy.name}`] : ['systemctl', '--user', 'disable', '--now', legacy.name];
    expect(calls).toContainEqual(stop);
    for (const c of calls) expect(c.join(' ')).not.toContain(otherLegacy.name);

    // uninstall 同样按标记删除全部（再放一份以前的定义）
    await writeFile(path.join(h.unitDir, legacy.file), legacy.content);
    const removed = await uninstallService(exec, s.home, s.logDir, h);
    expect(removed.sort()).toEqual([path.join(h.unitDir, legacy.file), r.info.file].sort());
    expect(await unitFiles(h)).toEqual([`unrelated${platform === 'darwin' ? '.plist' : '.service'}`, `x-${otherLegacy.file}`].sort());
  });

  it('同名的服务定义属于别的数据目录（或不是 kiln-orca 写的）：安装、卸载、重启、查看都报错，不停止、不改动它', async () => {
    const h = await host();
    const { calls, exec } = recorder();
    const s = spec('mini-ai', 'review');
    const info = serviceInfo(h, s.home, s.logDir);
    // 构造碰撞：在这个节点的服务定义位置放一个别的数据目录的定义
    const other = (platform === 'darwin' ? launchdPlist : systemdUnit)({ ...s, home: path.join(root, 'other') });
    await mkdir(h.unitDir, { recursive: true });
    for (const content of [other, '手写的服务定义\n']) {
      await writeFile(info.file, content);
      await expect(installService(exec, s, h)).rejects.toThrow(/不是数据目录/);
      await expect(uninstallService(exec, s.home, s.logDir, h)).rejects.toThrow(/不是数据目录/);
      await expect(restartService(exec, s.home, s.logDir, h)).rejects.toThrow(/不是数据目录/);
      await expect(serviceStatus(exec, s, s.logDir, h)).rejects.toThrow(/不是数据目录/);
      expect(await readFile(info.file, 'utf8')).toBe(content);
    }
    expect(calls).toEqual([]);
  });

  it('没有安装：restart 报错并说明；status、uninstall 照常（没有注册也可以）', async () => {
    const h = await host();
    const { calls, exec } = recorder();
    const s = spec('', 'review');
    await expect(restartService(exec, s.home, s.logDir, h)).rejects.toThrow(/还没有安装服务.*先运行 kiln-orca service install/);
    const out = await serviceStatus(exec, s, s.logDir, h);
    expect(out).toContain('节点：（还没有注册）');
    expect(out).toContain('还没有安装服务');
    expect(await uninstallService(exec, s.home, s.logDir, h)).toEqual([]);
    expect(calls.every((c) => c[1] !== 'kickstart' && c[2] !== 'restart')).toBe(true);
  });

  it('status 打印节点名称、数据目录、实际的服务标识和定义文件', async () => {
    const h = await host();
    const { exec } = recorder();
    const s = spec('mini.ai', 'review');
    const { info } = await installService(exec, s, h);
    const out = await serviceStatus(exec, s, s.logDir, h);
    expect(out).toContain('节点：mini.ai');
    expect(out).toContain(`数据目录：${s.home}`);
    expect(out).toContain(`服务：${info.name}`);
    expect(out).toContain(`定义：${info.file}`);
    expect(serviceId(s.home)).toMatch(/^[0-9a-f]{12}$/);
  });
});

// ---- 转义----

/** 路径中的特殊字符：systemd 的 specifier、环境变量、空白、引号、反斜杠，XML 的实体。 */
const nasty = ['%n', '%%', '${VERSION}', '$HOME', 'a b', 'q"uote', "it's", 'back\\slash', 'a&b', '<x>', 'tab\there'];
const nastyHome = (base: string) => path.join(base, `ai${nasty.join('')}`);

describe('systemd 单元的转义', () => {
  const s: ServiceSpec = {
    name: '100% 节点 ${X}',
    home: nastyHome('/data'),
    node: '/opt/${VERSION}/bin/node',
    cli: '/opt/x %n/cli.js',
    path: '/usr/bin:/opt/100%/bin:$HOME/bin',
    logDir: '/data/logs',
  };
  const unit = systemdUnit(s);
  const line = (key: string) => unit.split('\n').find((l) => l.startsWith(`${key}=`)) ?? '';

  it('ExecStart：每一项引起来，引号、反斜杠 C 转义，% 写成 %%；参数中的 $ 写成 $$，可执行文件的路径中的 $ 原样（不做变量替换）', () => {
    expect(line('ExecStart')).toBe(
      'ExecStart="/opt/${VERSION}/bin/node" "/opt/x %%n/cli.js" "run" "--service" "--home" ' +
        '"/data/ai%%n%%%%$${VERSION}$$HOMEa bq\\"uoteit\'sback\\\\slasha&b<x>tab\\x09here"',
    );
  });

  it('Environment=：整条赋值引起来，% 写成 %%，$ 不转义（Environment= 中不展开变量）', () => {
    expect(line('Environment')).toBe('Environment="PATH=/usr/bin:/opt/100%%/bin:$HOME/bin"');
  });

  it('Description=：% 写成 %%，控制字符换成空格，不在行尾', () => {
    expect(line('Description')).toBe('Description=Kiln AI 执行节点 100%% 节点 ${X}（kiln-orca）');
    expect(systemdUnit({ ...s, name: 'a\nExecStartPre=/bin/evil' }).split('\n').filter((l) => l.startsWith('ExecStartPre'))).toEqual([]);
  });

  it('数据目录标记（注释）是 base64，读回来的数据目录与原来相同', () => {
    const comment = unit.split('\n')[0]!;
    expect(comment).toMatch(/^# kiln-orca-home: [A-Za-z0-9+/=]+$/);
    expect(definedHome(unit)).toBe(s.home);
  });

  it('引号内不能有换行：写成 \\x0a', () => {
    expect(sdExecStart('/bin/x', ['a\nb'])).toBe('"/bin/x" "a\\x0ab"');
  });

  it.each(['/opt/q"uote/node', "/opt/it's/node", '/opt/back\\slash/node', '/opt/tab\there/node'])('systemd 不接受的可执行文件路径 %j：install 报错，不改动任何东西', async (node) => {
    root = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-svc-'));
    const h: ServiceHost = { platform: 'linux', unitDir: path.join(root, 'units'), uid: 501 };
    const { calls, exec } = recorder();
    await expect(installService(exec, { ...spec('n', 'home'), node }, h)).rejects.toThrow(/systemd 不接受可执行文件的路径/);
    expect(calls).toEqual([]);
    expect(existsSync(h.unitDir)).toBe(false);
  });
});

describe('launchd plist 的转义', () => {
  const s: ServiceSpec = {
    name: 'a--b-',
    home: '',
    node: '/opt/${VERSION}/bin/node',
    cli: '/opt/x %n/cli.js',
    path: '/usr/bin:/opt/a&b<c>:$HOME',
    logDir: '',
  };

  it('插入的字符串做 XML 转义；注释中没有 "--"、不以 "-" 结尾', async () => {
    root = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-plist-'));
    const plist = launchdPlist({ ...s, home: nastyHome(root), logDir: path.join(root, 'l&<'), name: s.name });
    expect(plist).toContain('<string>/usr/bin:/opt/a&amp;b&lt;c&gt;:$HOME</string>');
    expect(plist).toContain(`<string>${path.join(root, 'ai')}%n%%\${VERSION}$HOMEa bq&quot;uoteit&apos;sback\\slasha&amp;b&lt;x&gt;tab&#9;here</string>`);
    const comments = [...plist.matchAll(/<!--(.*?)-->/gs)].map((m) => m[1]!);
    expect(comments).toHaveLength(2);
    for (const c of comments) {
      expect(c).not.toContain('--');
      expect(c.endsWith('-')).toBe(false);
    }
    expect(xmlComment('a--b-')).toBe('a- -b- ');
    // 制表、换行、回车写成字符引用：符合规范的 XML 解析器会把文本中原样的回车（和回车换行）规范化成换行
    expect(xmlText('a\tb\nc\rd\r\n', 'x')).toBe('a&#9;b&#10;c&#13;d&#13;&#10;');
  });

  it.skipIf(process.platform !== 'darwin')('plutil 能解析（macOS），读回来的参数、PATH、日志路径与原来逐字相同', async () => {
    root = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-plist-'));
    const home = nastyHome(root) + '\nline2\r';
    const logDir = path.join(root, 'l&<>"\'');
    const plist = launchdPlist({ ...s, home, logDir });
    const file = path.join(root, 'x.plist');
    await writeFile(file, plist);
    await run('plutil', ['-lint', file]);
    const { stdout } = await run('plutil', ['-convert', 'json', '-o', '-', file]);
    const v = JSON.parse(stdout) as { ProgramArguments: string[]; EnvironmentVariables: { PATH: string }; StandardOutPath: string };
    expect(v.ProgramArguments).toEqual([s.node, s.cli, 'run', '--service', '--home', home]);
    expect(v.EnvironmentVariables.PATH).toBe(s.path);
    expect(path.dirname(v.StandardOutPath)).toBe(logDir);
  });

  it('XML 不能表示的控制字符：报错，不写出无效的 plist', () => {
    expect(() => launchdPlist({ ...s, home: '/data/a\x01b', logDir: '/l' })).toThrow(/不能表示的控制字符/);
  });
});

describe('给 Agent 的工具命令（shell 引用）', () => {
  const words = ['', 'plain/path-1.0', 'a b', "it's", '$HOME', '${VERSION}', '`id`', 'a\nb', '~', '=ls', '*', '%n', 'back\\slash', '"q"', "'", 'a;b|c&d'];
  const shells = ['/bin/sh', '/bin/bash', '/bin/zsh'].filter((sh) => existsSync(sh));

  it('有可用的 shell', () => {
    expect(shells.length).toBeGreaterThan(0);
  });

  it.each(shells)('%s 读回来的每个参数与原来逐字相同', async (sh) => {
    // 每个参数单独一行写出（printf %s\0 再按 \0 切开，参数中的换行也保留）
    const script = `printf '%s\\0' ${words.map(shellWord).join(' ')}`;
    const { stdout } = await run(sh, ['-c', script], { env: { HOME: '/home/x', VERSION: 'v1', PATH: process.env.PATH ?? '' } });
    expect(stdout.split('\0').slice(0, -1)).toEqual(words);
  });

  it.each(shells)('%s 执行 toolCommand：node 路径含空格、引号、$ 时仍执行它，参数与原来逐字相同', async (sh) => {
    root = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-tool-'));
    // 代替 node 的脚本放在难写的路径中，写出收到的参数
    const node = path.join(root, "my 'node' $HOME", 'node');
    await mkdir(path.dirname(node), { recursive: true });
    await writeFile(node, "#!/bin/sh\nprintf '%s\\0' \"$@\"\n", { mode: 0o755 });
    const home = "/data/my 'home' $HOME `x`\n%n";
    const cmd = toolCommand(node, '/opt/cli 1.js', home, 42n);
    const { stdout } = await run(sh, ['-c', cmd]);
    expect(stdout.split('\0').slice(0, -1)).toEqual(['/opt/cli 1.js', 'task', '--home', home, '--attempt', '42']);
  });
});

describe('数据目录的规范路径', () => {
  it('经符号链接、相对路径指定同一个目录：规范路径、服务标识相同；还不存在的部分原样接上，建好前后不变', async () => {
    root = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-home-'));
    const real = path.join(root, 'real');
    await mkdir(real);
    await symlink(real, path.join(root, 'link'));
    const viaLink = canonicalDir(path.join(root, 'link', 'node'));
    const direct = canonicalDir(path.join(root, 'real', 'node'));
    expect(viaLink).toBe(direct);
    expect(serviceId(viaLink)).toBe(serviceId(direct));
    await mkdir(path.join(real, 'node'));
    expect(canonicalDir(path.join(root, 'link', 'node'))).toBe(direct);
    expect(canonicalDir(path.relative(process.cwd(), path.join(root, 'link', 'node')))).toBe(direct);
  });
});
