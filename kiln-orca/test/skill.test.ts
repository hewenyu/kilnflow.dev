import { createHash } from 'node:crypto';
import { existsSync } from 'node:fs';
import { mkdir, mkdtemp, readFile, readdir, rm, stat, symlink, utimes, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { type Logger, nopLogger } from '../src/core/log.js';
import {
  acquireLock,
  bundledSkillDir,
  claudeConfigDir,
  isSafeRelPath,
  type LockSettings,
  type SkillDeps,
  SkillManager,
  skillDirFor,
  skillMetaFile,
  skillName,
} from '../src/core/skill.js';

// kiln-task skill 的投放（src/core/skill.ts）：路径、幂等、升级、协议单调、冲突保护、跨进程锁、元数据校验、原子安装和
// 只读检查。全部在临时目录中进行，不碰宿主机真实的 ~/.agents/skills 与 ~/.claude/skills。

/**
 * recordingWriter 构造注入用的 writeFile：记录写过的路径；fail 返回 true 时该次写入失败（模拟首次安装写到一半失败）。
 * 用注入而不是 chmod / 模块 mock，root 容器里也能真实成立。
 */
function recordingWriter(fail?: (p: string) => boolean): { writeFile: NonNullable<SkillDeps['writeFile']>; paths: string[] } {
  const paths: string[] = [];
  return {
    paths,
    writeFile: async (p, data, mode) => {
      if (fail?.(p)) throw Object.assign(new Error('模拟写入失败'), { code: 'EIO' });
      paths.push(p);
      await writeFile(p, data, { mode });
    },
  };
}

const dirs: string[] = [];
afterEach(async () => {
  for (const d of dirs.splice(0)) await rm(d, { recursive: true, force: true });
});

/** tempDir 建一个临时目录（测试结束时删除）。 */
async function tempDir(): Promise<string> {
  const d = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-skill-'));
  dirs.push(d);
  return d;
}

/** capLogger 收集日志，供断言（协议不降级等）。 */
function capLogger(): Logger & { lines: string[] } {
  const lines: string[] = [];
  const add = (level: string) => (msg: string) => {
    lines.push(`${level} ${msg}`);
  };
  return { lines, info: add('info'), warn: add('warn'), error: add('error') };
}

/** manager 用临时主目录构造 SkillManager（默认安装真实的内置 skill）。 */
function manager(home: string, over: Partial<SkillDeps> = {}): SkillManager {
  return new SkillManager({
    homeDir: home,
    env: {},
    sourceDir: bundledSkillDir(),
    version: '1.0.0',
    protocol: 10,
    log: nopLogger,
    ...over,
  });
}

/** sha256 返回内容的十六进制摘要（构造元数据用）。 */
const sha256 = (data: string | Buffer): string => createHash('sha256').update(data).digest('hex');

/** writeFiles 在 dir 下写出这些文件（相对路径 → 内容）。 */
async function writeFiles(dir: string, files: Record<string, string>): Promise<string> {
  for (const [rel, text] of Object.entries(files)) {
    const p = path.join(dir, rel);
    await mkdir(path.dirname(p), { recursive: true });
    await writeFile(p, text);
  }
  return dir;
}

/** makeSource 建一个内置 skill 的源目录（必须有 SKILL.md 和 references/commands.md）。 */
async function makeSource(name: string, files: Record<string, string>): Promise<string> {
  return writeFiles(path.join(await tempDir(), name), files);
}

/** listAll 递归列出目录中的文件（相对路径，排序）；目录不存在时为 undefined。 */
async function listAll(dir: string): Promise<string[] | undefined> {
  if (!existsSync(dir)) return undefined;
  const out: string[] = [];
  const walk = async (rel: string): Promise<void> => {
    const entries = await readdir(path.join(dir, rel), { withFileTypes: true });
    for (const e of entries) {
      const next = rel === '' ? e.name : `${rel}/${e.name}`;
      if (e.isDirectory()) await walk(next);
      else out.push(next);
    }
  };
  await walk('');
  return out.sort();
}

/** metaOf 读取托管目录中的元数据。 */
async function metaOf(dir: string): Promise<Record<string, unknown>> {
  return JSON.parse(await readFile(path.join(dir, skillMetaFile), 'utf8')) as Record<string, unknown>;
}

/**
 * metaText 构造一份合法的元数据文本（可覆盖字段）。files 里默认有 SKILL.md，digest / sha256 都是合法的 64 位十六进制，
 * 这样测试能精确地只让一个字段非法。
 */
function metaText(over: Record<string, unknown> = {}): string {
  return JSON.stringify({
    managedBy: 'kiln-orca',
    skill: skillName,
    version: '1.0.0',
    protocol: 10,
    digest: 'a'.repeat(64),
    files: [{ path: 'SKILL.md', sha256: 'b'.repeat(64) }],
    ...over,
  });
}

/** targetDir 返回 Codex 的目标目录。 */
const targetDir = (home: string): string => path.join(home, '.agents', 'skills', skillName);

const bundleFiles = ['SKILL.md', 'references/commands.md'];

describe('skill 的目标目录', () => {
  it('按 Kiln 的 Agent 名解析：Codex 用 ~/.agents，Claude Code 用 $CLAUDE_CONFIG_DIR 或 ~/.claude', () => {
    expect(skillDirFor('codex', '/home/u')).toBe(path.join('/home/u', '.agents', 'skills', skillName));
    expect(skillDirFor('claude-code', '/home/u')).toBe(path.join('/home/u', '.claude', 'skills', skillName));
    expect(skillDirFor('claude-code', '/home/u', { CLAUDE_CONFIG_DIR: '/cfg/claude' })).toBe(
      path.join('/cfg/claude', 'skills', skillName),
    );
    // 相对路径按当前目录解析为绝对路径；空白值当作没有设置
    expect(claudeConfigDir({ CLAUDE_CONFIG_DIR: 'rel/dir' }, '/home/u')).toBe(path.resolve('rel/dir'));
    expect(claudeConfigDir({ CLAUDE_CONFIG_DIR: '  ' }, '/home/u')).toBe(path.join('/home/u', '.claude'));
    expect(() => skillDirFor('other-agent', '/home/u')).toThrow(/未知的 Agent/);
  });

  it('随包分发的 skill 目录能从模块位置解析，并且包含入口和命令说明', async () => {
    const dir = bundledSkillDir();
    for (const rel of bundleFiles) expect(existsSync(path.join(dir, rel)), rel).toBe(true);
  });
});

describe('安装', () => {
  it('首次安装写出全部文件、元数据和可用的 SKILL.md 路径', async () => {
    const home = await tempDir();
    const mgr = manager(home);
    const skillPath = await mgr.ensureForAgent('codex');
    const target = targetDir(home);
    expect(skillPath).toBe(path.join(target, 'SKILL.md'));
    expect((await listAll(target))?.sort()).toEqual([...bundleFiles, skillMetaFile].sort());
    const meta = await metaOf(target);
    expect(meta).toMatchObject({ managedBy: 'kiln-orca', skill: skillName, version: '1.0.0', protocol: 10 });
    expect(await readFile(skillPath, 'utf8')).toBe(await readFile(path.join(bundledSkillDir(), 'SKILL.md'), 'utf8'));
    // 锁已经释放，临时文件和 staging 目录没有残留
    expect(existsSync(path.join(home, '.agents', 'skills', '.kiln-task.lock'))).toBe(false);
    expect((await listAll(target))?.some((f) => f.endsWith('.tmp'))).toBe(false);
    const leftovers = (await readdir(path.dirname(target))).filter((e) => e.includes('.tmp') || e.includes('.stage-'));
    expect(leftovers).toEqual([]);
  });

  it('已经是最新内容时不做任何写操作（changed=false、文件不动、也没有安装日志）', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    expect(await manager(home).ensureForAgent('codex')).toBe(path.join(target, 'SKILL.md'));
    const metaBefore = await stat(path.join(target, skillMetaFile));
    const skillBefore = await stat(path.join(target, 'SKILL.md'));
    const log = capLogger();
    const rec = recordingWriter();
    const res = await manager(home, { log, writeFile: rec.writeFile }).sync('codex');
    expect(res.changed).toBe(false);
    const metaAfter = await stat(path.join(target, skillMetaFile));
    expect(metaAfter.ino).toBe(metaBefore.ino);
    expect(metaAfter.mtimeMs).toBe(metaBefore.mtimeMs);
    const skillAfter = await stat(path.join(target, 'SKILL.md'));
    expect(skillAfter.ino).toBe(skillBefore.ino);
    expect(skillAfter.mtimeMs).toBe(skillBefore.mtimeMs);
    // 第二次同步没有往目标目录写任何文件
    expect(rec.paths).toEqual([]);
    expect(log.lines.filter((l) => l.includes('已安装'))).toEqual([]);
  });

  it('Claude Code 装在 CLAUDE_CONFIG_DIR 指定的目录', async () => {
    const home = await tempDir();
    const cfg = await tempDir();
    const skillPath = await manager(home, { env: { CLAUDE_CONFIG_DIR: cfg } }).ensureForAgent('claude-code');
    expect(skillPath).toBe(path.join(cfg, 'skills', skillName, 'SKILL.md'));
    expect(existsSync(path.join(home, '.claude'))).toBe(false);
  });

  it('ensureAll 汇总失败但继续处理其他 Agent', async () => {
    const home = await tempDir();
    const all = await manager(home).ensureAll(['codex', 'unknown-agent', 'claude-code']);
    expect(all).toContain('unknown-agent：');
    expect(all).not.toContain('codex：');
    expect(existsSync(path.join(home, '.agents', 'skills', skillName, 'SKILL.md'))).toBe(true);
    expect(existsSync(path.join(home, '.claude', 'skills', skillName, 'SKILL.md'))).toBe(true);
  });

  it('升级时按新清单更新文件并删除上一次清单中的旧文件', async () => {
    const home = await tempDir();
    const v1 = await makeSource('v1', {
      'SKILL.md': '第一版\n',
      'references/commands.md': '命令第一版\n',
      'references/old.md': '已经不要的说明\n',
    });
    const target = targetDir(home);
    expect(await manager(home, { sourceDir: v1, version: '1.0.0' }).ensureForAgent('codex')).toBe(path.join(target, 'SKILL.md'));
    expect(existsSync(path.join(target, 'references', 'old.md'))).toBe(true);

    const v2 = await makeSource('v2', {
      'SKILL.md': '第二版\n',
      'references/commands.md': '命令第二版\n',
      'notes/new.md': '新增的说明\n',
    });
    await manager(home, { sourceDir: v2, version: '2.0.0' }).ensureForAgent('codex');
    expect(await readFile(path.join(target, 'SKILL.md'), 'utf8')).toBe('第二版\n');
    expect(existsSync(path.join(target, 'references', 'old.md'))).toBe(false);
    expect(existsSync(path.join(target, 'notes', 'new.md'))).toBe(true);
    const meta = await metaOf(target);
    expect(meta.version).toBe('2.0.0');
    expect(typeof meta.digest).toBe('string');
  });

  it('内容相同时升级连接器版本或协议也会更新元数据', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    await manager(home).ensureForAgent('codex');
    const skillBefore = await readFile(path.join(target, 'SKILL.md'));

    const versionOnly = await manager(home, { version: '2.0.0' }).sync('codex');
    expect(versionOnly.changed).toBe(true);
    expect(await metaOf(target)).toMatchObject({ version: '2.0.0', protocol: 10 });

    const protocolOnly = await manager(home, { version: '2.0.0', protocol: 11 }).sync('codex');
    expect(protocolOnly.changed).toBe(true);
    expect((await metaOf(target)).protocol).toBe(11);
    // 元数据的协议变了，文件内容不变
    expect(await readFile(path.join(target, 'SKILL.md'))).toEqual(skillBefore);
  });

  it('元数据多出旧文件条目时即使 digest 相同也不走快路径，并清掉旧文件', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    await manager(home).ensureForAgent('codex');
    // 造一个旧文件，并把它加进元数据：digest 不变，只有清单的路径集合多了它
    await writeFiles(target, { 'references/old.md': '旧说明\n' });
    const meta = await metaOf(target);
    (meta.files as { path: string; sha256: string }[]).push({ path: 'references/old.md', sha256: sha256('旧说明\n') });
    await writeFile(path.join(target, skillMetaFile), JSON.stringify(meta));

    const res = await manager(home).sync('codex');
    expect(res.changed).toBe(true);
    expect(existsSync(path.join(target, 'references', 'old.md'))).toBe(false);
    const after = await metaOf(target);
    expect((after.files as { path: string }[]).map((f) => f.path).sort()).toEqual([...bundleFiles].sort());
  });

  it('同机更高协议的连接器安装的版本不降级覆盖（保留前确认它完整）', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    const newer = await makeSource('newer', { 'SKILL.md': '协议 11\n', 'references/commands.md': 'c\n' });
    await manager(home, { sourceDir: newer, protocol: 11, version: '2.0.0' }).ensureForAgent('codex');

    const log = capLogger();
    const older = await makeSource('older', { 'SKILL.md': '协议 10\n', 'references/commands.md': 'c\n' });
    const skillPath = await manager(home, { sourceDir: older, protocol: 10, version: '1.0.0', log }).ensureForAgent('codex');
    expect(skillPath).toBe(path.join(target, 'SKILL.md'));
    expect(await readFile(skillPath, 'utf8')).toBe('协议 11\n');
    expect(await metaOf(target)).toMatchObject({ version: '2.0.0', protocol: 11 });
    expect(log.lines.some((l) => l.startsWith('warn ') && l.includes('不降级覆盖'))).toBe(true);

    // 更高的协议照常升级
    const newer2 = await makeSource('newer2', { 'SKILL.md': '协议 12\n', 'references/commands.md': 'c\n' });
    await manager(home, { sourceDir: newer2, protocol: 12, version: '3.0.0' }).ensureForAgent('codex');
    expect(await readFile(skillPath, 'utf8')).toBe('协议 12\n');
  });
});

describe('首次安装的原子发布', () => {
  it('写入中途失败时不会留下半安装目录，撤销故障后重试立即成功', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    const skills = path.dirname(target);
    // 让 staging 目录中 SKILL.md 的写入失败（写入的是同目录的临时文件，此时 references/commands.md 已经写好）
    const rec = recordingWriter((p) => p.includes(`${path.sep}.${skillName}.stage-`) && p.includes(`${path.sep}SKILL.md.`));
    await expect(manager(home, { writeFile: rec.writeFile }).ensureForAgent('codex')).rejects.toThrow(/模拟写入失败/);

    // target 绝不存在，staging 也清理干净（否则节点会因为"没有元数据的半安装目录"永久 standby）
    expect(existsSync(target)).toBe(false);
    expect((await readdir(skills)).filter((e) => e.includes('.stage-') || e.endsWith('.tmp'))).toEqual([]);

    // 撤销故障后重试立即成功
    expect(await manager(home).ensureForAgent('codex')).toBe(path.join(target, 'SKILL.md'));
    expect((await listAll(target))?.sort()).toEqual([...bundleFiles, skillMetaFile].sort());
  });
});

describe('冲突保护', () => {
  it('用户自建的同名目录不覆盖', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    await writeFiles(target, { 'SKILL.md': '用户自己的 skill\n' });
    await expect(manager(home).ensureForAgent('codex')).rejects.toThrow(/不是 kiln-orca 安装的/);
    expect(await readFile(path.join(target, 'SKILL.md'), 'utf8')).toBe('用户自己的 skill\n');
    expect(existsSync(path.join(target, skillMetaFile))).toBe(false);
  });

  it('元数据不是 kiln-orca 的（managedBy 不对）也当作冲突', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    await writeFiles(target, {
      'SKILL.md': 'x\n',
      [skillMetaFile]: metaText({ managedBy: 'someone-else' }),
    });
    await expect(manager(home).ensureForAgent('codex')).rejects.toThrow(/不是 kiln-orca 安装的/);
  });

  it('目标是符号链接或普通文件时不覆盖', async () => {
    const home = await tempDir();
    const outside = await writeFiles(await tempDir(), { 'SKILL.md': '外面的\n' });
    const skills = path.join(home, '.agents', 'skills');
    await mkdir(skills, { recursive: true });
    await symlink(outside, path.join(skills, skillName), 'dir');
    await expect(manager(home).ensureForAgent('codex')).rejects.toThrow(/符号链接/);
    expect(await readFile(path.join(outside, 'SKILL.md'), 'utf8')).toBe('外面的\n');

    const home2 = await tempDir();
    await mkdir(path.join(home2, '.agents', 'skills'), { recursive: true });
    await writeFile(path.join(home2, '.agents', 'skills', skillName), '普通文件\n');
    await expect(manager(home2).ensureForAgent('codex')).rejects.toThrow(/不是目录/);
  });

  it('托管目录中的文件被换成符号链接时拒绝写入', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    await manager(home).ensureForAgent('codex');
    const outside = await writeFiles(await tempDir(), { 'commands.md': '外面的\n' });
    await rm(path.join(target, 'references'), { recursive: true, force: true });
    await symlink(outside, path.join(target, 'references'), 'dir');

    const newer = await makeSource('x', { 'SKILL.md': '新版\n', 'references/commands.md': '新命令\n' });
    await expect(manager(home, { sourceDir: newer, version: '2.0.0' }).ensureForAgent('codex')).rejects.toThrow(/符号链接/);
    expect(await readFile(path.join(outside, 'commands.md'), 'utf8')).toBe('外面的\n');
    expect(await readFile(path.join(target, 'SKILL.md'), 'utf8')).not.toBe('新版\n');
  });

  it('元数据文件本身是符号链接或目录时拒绝（不跟随链接、不误报成用户自建）', async () => {
    // 符号链接
    const home = await tempDir();
    const target = targetDir(home);
    await writeFiles(target, { 'SKILL.md': 'x\n' });
    const outside = await writeFiles(await tempDir(), { 'meta.json': metaText() });
    await symlink(path.join(outside, 'meta.json'), path.join(target, skillMetaFile));
    await expect(manager(home).ensureForAgent('codex')).rejects.toThrow(/符号链接/);
    expect((await manager(home).check(['codex']))[0]?.ok).toBe(false);

    // 目录
    const home2 = await tempDir();
    const target2 = targetDir(home2);
    await writeFiles(target2, { 'SKILL.md': 'x\n' });
    await mkdir(path.join(target2, skillMetaFile));
    await expect(manager(home2).ensureForAgent('codex')).rejects.toThrow(/不是普通文件/);
    expect((await manager(home2).check(['codex']))[0]?.ok).toBe(false);
  });
});

describe('元数据的路径与协议校验', () => {
  it('isSafeRelPath 拒绝绝对路径、..、空段、反斜杠、NUL 和元数据文件本身', () => {
    for (const ok of ['SKILL.md', 'references/commands.md', 'a/b/c.txt']) expect(isSafeRelPath(ok), ok).toBe(true);
    for (const bad of ['', '/etc/passwd', '../x', 'a/../b', 'a//b', 'a/./b', 'a\\b', 'a\u0000b', 'references/', '.kiln-orca-skill.json', '.', '..']) {
      expect(isSafeRelPath(bad), JSON.stringify(bad)).toBe(false);
    }
  });

  it('元数据里的路径穿越被拒绝，同级目录与目标目录都不受影响', async () => {
    for (const bad of ['../sibling/SKILL.md', '/etc/passwd']) {
      const home = await tempDir();
      const target = targetDir(home);
      // 同级目录 sibling/SKILL.md：绝不能被穿越写坏
      const sibling = await writeFiles(path.join(home, '.agents', 'skills', 'sibling'), { 'SKILL.md': 'sibling 原样\n' });
      await writeFiles(target, { 'SKILL.md': '目标目录原样\n' });
      await writeFile(
        path.join(target, skillMetaFile),
        metaText({
          files: [
            { path: bad, sha256: 'b'.repeat(64) },
            { path: 'SKILL.md', sha256: 'b'.repeat(64) },
          ],
        }),
      );
      await expect(manager(home).ensureForAgent('codex')).rejects.toThrow(/清单路径不安全|不是安全的相对路径/);
      const item = (await manager(home).check(['codex']))[0];
      expect(item?.ok).toBe(false);
      expect(item?.detail).toContain('清单路径不安全');
      expect(await readFile(path.join(sibling, 'SKILL.md'), 'utf8')).toBe('sibling 原样\n');
      expect(await readFile(path.join(target, 'SKILL.md'), 'utf8')).toBe('目标目录原样\n');
    }
  });

  it('元数据的 protocol 不是正整数（Infinity / -1 / 0 / 小数）时拒绝', async () => {
    for (const protocol of ['1e999', '-1', '0', '1.5']) {
      const home = await tempDir();
      const target = targetDir(home);
      await writeFiles(target, { 'SKILL.md': 'x\n' });
      // 用原始文本写入：JSON.stringify 会把 Infinity 变成 null，这里要的是 JSON.parse 后的 Infinity
      await writeFile(path.join(target, skillMetaFile), metaText().replace('"protocol":10', `"protocol":${protocol}`));
      await expect(manager(home).ensureForAgent('codex'), protocol).rejects.toThrow(/protocol 不是正整数/);
      expect((await manager(home).check(['codex']))[0]?.ok, protocol).toBe(false);
    }
  });

  it('清单里重复的路径、非十六进制的 sha256、缺少 SKILL.md 都算无效元数据', async () => {
    const cases: Record<string, unknown>[] = [
      { files: [{ path: 'SKILL.md', sha256: 'b'.repeat(64) }, { path: 'SKILL.md', sha256: 'b'.repeat(64) }] },
      { files: [{ path: 'SKILL.md', sha256: 'not-hex' }] },
      { files: [{ path: 'references/commands.md', sha256: 'b'.repeat(64) }] },
      { digest: 'xyz' },
    ];
    for (const over of cases) {
      const home = await tempDir();
      const target = targetDir(home);
      await writeFiles(target, { 'SKILL.md': 'x\n' });
      await writeFile(path.join(target, skillMetaFile), metaText(over));
      await expect(manager(home).ensureForAgent('codex')).rejects.toThrow(/不是 kiln-orca 安装的/);
      expect((await manager(home).check(['codex']))[0]?.ok).toBe(false);
    }
  });
});

describe('更高协议的安装完整性', () => {
  /** newerInstall 用协议 11 装一份"更新的版本"，返回目标目录。 */
  async function newerInstall(home: string): Promise<string> {
    const src = await makeSource('newer', { 'SKILL.md': '协议 11\n', 'references/commands.md': 'c\n' });
    await manager(home, { sourceDir: src, protocol: 11, version: '2.0.0' }).ensureForAgent('codex');
    return targetDir(home);
  }

  /** older 是协议 10 的连接器：它不应该降级覆盖，但必须确认更高协议的安装是完整的。 */
  const older = (home: string): SkillManager => manager(home, { protocol: 10, version: '1.0.0' });

  it('SKILL.md 被换成符号链接时 sync 与 check 都失败', async () => {
    const home = await tempDir();
    const target = await newerInstall(home);
    const outside = await writeFiles(await tempDir(), { 'SKILL.md': '外面\n' });
    await rm(path.join(target, 'SKILL.md'));
    await symlink(path.join(outside, 'SKILL.md'), path.join(target, 'SKILL.md'));
    await expect(older(home).ensureForAgent('codex')).rejects.toThrow(/符号链接/);
    const item = (await older(home).check(['codex']))[0];
    expect(item?.ok).toBe(false);
    expect(await readFile(path.join(outside, 'SKILL.md'), 'utf8')).toBe('外面\n');
  });

  it('SKILL.md 被删除时 sync 与 check 都失败', async () => {
    const home = await tempDir();
    const target = await newerInstall(home);
    await rm(path.join(target, 'SKILL.md'));
    await expect(older(home).ensureForAgent('codex')).rejects.toThrow(/不存在|安装已损坏/);
    expect((await older(home).check(['codex']))[0]?.ok).toBe(false);
  });

  it('SKILL.md 被篡改时 sync 与 check 都失败', async () => {
    const home = await tempDir();
    const target = await newerInstall(home);
    await writeFile(path.join(target, 'SKILL.md'), '被改过\n');
    await expect(older(home).ensureForAgent('codex')).rejects.toThrow(/内容与安装清单不一致/);
    expect((await older(home).check(['codex']))[0]?.ok).toBe(false);
  });
});

describe('跨进程锁', () => {
  /** lockPath 返回 Codex 的 skills 目录中的锁文件。 */
  const lockPath = (home: string): string => path.join(home, '.agents', 'skills', '.kiln-task.lock');

  /** prepare 建好锁文件所在的目录，返回独立的临时主目录与锁路径。 */
  async function prepare(): Promise<{ home: string; lock: string }> {
    const home = await tempDir();
    const lock = lockPath(home);
    await mkdir(path.dirname(lock), { recursive: true });
    return { home, lock };
  }

  /** readOwner 读锁文件的属主（测试里断言 token 与 pid）。 */
  async function readOwner(p: string): Promise<{ pid: number; token: string }> {
    return JSON.parse(await readFile(p, 'utf8')) as { pid: number; token: string };
  }

  /** leftovers 返回目录中残留的接管锁与临时文件。 */
  async function leftovers(dir: string): Promise<string[]> {
    return (await readdir(dir)).filter((e) => e.includes('.takeover-') || e.endsWith('.tmp'));
  }

  it('多个同步真实并发竞争同一个目标目录：全部成功、文件内容完整', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    const results = await Promise.all(Array.from({ length: 6 }, () => manager(home).sync('codex')));
    for (const r of results) expect(r.skillPath).toBe(path.join(target, 'SKILL.md'));
    expect(await readFile(path.join(target, 'SKILL.md'), 'utf8')).toBe(await readFile(path.join(bundledSkillDir(), 'SKILL.md'), 'utf8'));
    expect((await listAll(target))?.sort()).toEqual([...bundleFiles, skillMetaFile].sort());
    const meta = await metaOf(target);
    expect(Array.isArray(meta.files)).toBe(true);
    expect(meta.digest).toMatch(/^[0-9a-f]{64}$/);
    expect(existsSync(lockPath(home))).toBe(false);
  });

  it('两个竞争者面对同一份死进程的锁：只有一个接管，临界区不重叠，新属主的锁不被误删', async () => {
    const { lock } = await prepare();
    const dead = JSON.stringify({ pid: 999_999, token: 'dead' }) + '\n';
    await writeFile(lock, dead);
    // 只有本进程活着：999999 那份锁确定失效，两个竞争者会同时尝试接管
    const settings: LockSettings = { now: Date.now, processAlive: (pid) => pid === process.pid, waitMs: 2_000, retryMs: 5 };
    let inside = 0;
    const overlaps: string[] = [];
    const entered: string[] = [];
    const heldTokens: string[] = [];
    const run = async (name: string): Promise<void> => {
      const release = await acquireLock(lock, settings);
      if (inside > 0) overlaps.push(name);
      inside++;
      entered.push(name);
      // 临界区里锁文件必须还在，而且已经是新属主（不是死进程那份、也不是被对方删掉的空窗）
      heldTokens.push((await readOwner(lock)).token);
      await new Promise((r) => setTimeout(r, 20));
      inside--;
      await release();
    };
    await Promise.all([run('a'), run('b')]);
    expect(overlaps).toEqual([]);
    expect(entered.sort()).toEqual(['a', 'b']);
    for (const token of heldTokens) expect(token).not.toBe('dead');
    // 锁用完即删，接管锁与临时文件都没有残留
    expect(existsSync(lock)).toBe(false);
    expect(await leftovers(path.dirname(lock))).toEqual([]);
  });

  it('接管死进程的锁之后，旧持有者释放时不删除新持有者的锁', async () => {
    const { lock } = await prepare();
    // 注入"所有进程都已退出"：第一把锁一拿到就被第二把按接管取代
    const settings: LockSettings = { now: Date.now, processAlive: () => false, waitMs: 1_000, retryMs: 5 };
    const releaseOld = await acquireLock(lock, settings);
    const old = await readOwner(lock);
    const releaseNew = await acquireLock(lock, settings);
    const fresh = await readOwner(lock);
    expect(fresh.token).not.toBe(old.token);

    await releaseOld(); // 旧持有者绝不能删掉新持有者的锁
    expect((await readOwner(lock)).token).toBe(fresh.token);

    await releaseNew();
    expect(existsSync(lock)).toBe(false);
  });

  it('活进程的锁不会因为时间过去而回收：经历多轮存活校验后超时，原活锁完好', async () => {
    const { lock } = await prepare();
    const live = JSON.stringify({ pid: 321, token: 'live' }) + '\n';
    await writeFile(lock, live);
    // 一年前的锁：任何按时间回收活进程锁的实现都会抢走它
    const old = new Date(Date.now() - 365 * 24 * 3_600_000);
    await utimes(lock, old, old);
    const day = 86_400_000;
    let clock = 1_000_000_000_000;
    let aliveChecks = 0;
    const settings: LockSettings = {
      now: () => (clock += day), // 每次看时钟都过去一天
      processAlive: (pid) => {
        if (pid === 321) aliveChecks++;
        return pid === 321;
      },
      // 必须覆盖多轮检查（否则首轮在读锁与 processAlive 之前就超时）
      waitMs: 5 * day,
      retryMs: 1,
    };
    await expect(acquireLock(lock, settings)).rejects.toThrow(/另一个连接器正在安装/);
    expect(aliveChecks).toBeGreaterThanOrEqual(3);
    expect(await readFile(lock, 'utf8')).toBe(live);
  });

  it('内容不合法或属主不完整的锁文件不被静默删除，抛出清楚的错误', async () => {
    const texts = [
      '不是 JSON',
      JSON.stringify({ pid: 0, token: 't' }),
      JSON.stringify({ pid: 1.5, token: 't' }),
      JSON.stringify({ pid: 1, token: '' }),
      JSON.stringify({ pid: 1, token: '../escape' }),
    ];
    for (const text of texts) {
      const { home, lock } = await prepare();
      await writeFile(lock, text);
      await expect(manager(home, { lockWaitMs: 100, lockRetryMs: 5 }).ensureForAgent('codex'), text).rejects.toThrow(
        /锁文件 .* 的内容无效.*删除/,
      );
      expect(await readFile(lock, 'utf8'), text).toBe(text);
      expect(existsSync(targetDir(home)), text).toBe(false);
    }
  });

  it('持有进程已经退出时接管它的锁', async () => {
    const { home, lock } = await prepare();
    await writeFile(lock, JSON.stringify({ pid: 4321, token: 'gone' }) + '\n');
    await manager(home, { processAlive: () => false, lockWaitMs: 200, lockRetryMs: 10 }).ensureForAgent('codex');
    expect(existsSync(lock)).toBe(false);
    expect(existsSync(path.join(targetDir(home), 'SKILL.md'))).toBe(true);
  });

  it('死进程遗留的接管锁可以递归接管，然后取得主锁', async () => {
    const { lock } = await prepare();
    await writeFile(lock, JSON.stringify({ pid: 9001, token: 'main' }) + '\n');
    await writeFile(`${lock}.takeover-main`, JSON.stringify({ pid: 9002, token: 'guard' }) + '\n');
    const settings: LockSettings = { now: Date.now, processAlive: (pid) => pid === process.pid, waitMs: 1_000, retryMs: 5 };
    const release = await acquireLock(lock, settings);
    const held = await readOwner(lock);
    expect(held.pid).toBe(process.pid);
    expect(held.token).not.toBe('main');
    // 主锁的接管锁与它自己的接管锁都清理干净
    expect(existsSync(`${lock}.takeover-main`)).toBe(false);
    expect(await leftovers(path.dirname(lock))).toEqual([]);
    await release();
    expect(existsSync(lock)).toBe(false);
  });

  it('接管锁被活着的进程持有时不抢占：等待超时，主锁与接管锁都保留', async () => {
    const { lock } = await prepare();
    const main = JSON.stringify({ pid: 9001, token: 'main' }) + '\n';
    const guard = JSON.stringify({ pid: 4321, token: 'guard' }) + '\n';
    await writeFile(lock, main);
    await writeFile(`${lock}.takeover-main`, guard);
    const settings: LockSettings = { now: Date.now, processAlive: (pid) => pid === 4321, waitMs: 60, retryMs: 5 };
    await expect(acquireLock(lock, settings)).rejects.toThrow(/另一个连接器正在安装/);
    expect(await readFile(lock, 'utf8')).toBe(main);
    expect(await readFile(`${lock}.takeover-main`, 'utf8')).toBe(guard);
  });

  it('另一个连接器正在安装时等待超时并报清楚的原因，不删除别人的锁', async () => {
    const { home, lock } = await prepare();
    await writeFile(lock, JSON.stringify({ pid: 4321, token: 't' }) + '\n');
    const mgr = manager(home, { processAlive: () => true, lockWaitMs: 80, lockRetryMs: 10 });
    await expect(mgr.ensureForAgent('codex')).rejects.toThrow(/另一个连接器正在安装/);
    // 不是自己的锁，不删除
    expect(existsSync(lock)).toBe(true);
    expect(existsSync(path.join(home, '.agents', 'skills', skillName, 'SKILL.md'))).toBe(false);
  });
});

describe('doctor 的只读检查', () => {
  it('尚未安装时说明会自动安装，并且不创建任何目录', async () => {
    const home = await tempDir();
    const items = await manager(home).check(['codex', 'claude-code']);
    expect(items.map((i) => i.agent)).toEqual(['codex', 'claude-code']);
    for (const item of items) {
      expect(item.ok).toBe(true);
      expect(item.detail).toContain('启动连接器时会自动安装到');
    }
    expect(await readdir(home)).toEqual([]);
  });

  it('已安装时为 ok；版本较旧时说明会更新；更新的版本说明会保留', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    await manager(home).ensureForAgent('codex');
    expect((await manager(home).check(['codex']))[0]?.detail).toContain('已安装（1.0.0');

    const v2 = await makeSource('v2', { 'SKILL.md': '新版\n', 'references/commands.md': '新命令\n' });
    expect((await manager(home, { sourceDir: v2, version: '2.0.0' }).check(['codex']))[0]?.detail).toContain('会更新到 2.0.0');
    expect((await manager(home, { sourceDir: v2, version: '0.9.0', protocol: 9 }).check(['codex']))[0]?.detail).toContain(
      '不降级覆盖',
    );
    // 检查本身不写文件
    expect(await readFile(path.join(target, 'SKILL.md'), 'utf8')).not.toBe('新版\n');
  });

  it('冲突时是失败项，并给出路径和怎么处理', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    await writeFiles(target, { 'SKILL.md': '用户自己的\n' });
    const item = (await manager(home).check(['codex']))[0];
    expect(item?.ok).toBe(false);
    expect(item?.path).toBe(target);
    expect(item?.detail).toContain('不会覆盖它');

    const home2 = await tempDir();
    await writeFile(path.join(home2, '.agents'), '普通文件\n');
    const notDir = (await manager(home2).check(['codex']))[0];
    expect(notDir?.ok).toBe(false);
    expect(notDir?.detail).toContain('不是目录');
  });

  it('祖先目录不可写时是失败项（用注入的可写判断，root 容器中也成立）', async () => {
    const home = await tempDir();
    const item = (await manager(home, { canWriteDir: async () => false }).check(['codex']))[0];
    expect(item?.ok).toBe(false);
    expect(item?.detail).toContain('不可写');
  });

  it('需要更新但锁目录或目标目录不可写时是失败项（doctor 不会先说 ok）', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    await manager(home).ensureForAgent('codex');
    const v2 = await makeSource('v2', { 'SKILL.md': '新版\n', 'references/commands.md': '新命令\n' });
    // 锁文件所在的 skillsRoot 不可写
    const noRoot = (await manager(home, { sourceDir: v2, version: '2.0.0', canWriteDir: async (d) => d !== path.dirname(target) }).check(['codex']))[0];
    expect(noRoot?.ok).toBe(false);
    expect(noRoot?.detail).toContain('不可写');
    // 目标目录不可写
    const noTarget = (await manager(home, { sourceDir: v2, version: '2.0.0', canWriteDir: async (d) => d !== target }).check(['codex']))[0];
    expect(noTarget?.ok).toBe(false);
    expect(noTarget?.detail).toContain('不可写');
  });

  it('需要更新但子目录不可写时是失败项（references/ 里要写文件）', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    await manager(home).ensureForAgent('codex');
    const v2 = await makeSource('v2', { 'SKILL.md': '新版\n', 'references/commands.md': '新命令\n' });
    const references = path.join(target, 'references');
    const item = (await manager(home, { sourceDir: v2, version: '2.0.0', canWriteDir: async (d) => d !== references }).check(['codex']))[0];
    expect(item?.ok).toBe(false);
    expect(item?.detail).toContain(references);
    expect(item?.detail).toContain('不可写');
  });

  it('已经是最新版本但锁目录不可写时也是失败项（sync 仍要在 skillsRoot 建锁）', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    await manager(home).ensureForAgent('codex');
    const item = (await manager(home, { canWriteDir: async (d) => d !== path.dirname(target) }).check(['codex']))[0];
    expect(item?.ok).toBe(false);
    expect(item?.detail).toContain('锁文件');
    expect(item?.detail).toContain(path.dirname(target));
  });

  it('需要更新但新增嵌套子目录的已有祖先目录不可写时是失败项', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    // v1 清单不含任何 assets 路径：安装后只有单独存在的 assets/，v2 新增 assets/new/a.md 时只能靠向上找最近已存在祖先命中它
    const v1 = await makeSource('v1', { 'SKILL.md': 'v1\n', 'references/commands.md': 'c1\n' });
    const v2 = await makeSource('v2', { 'SKILL.md': 'v2\n', 'references/commands.md': 'c2\n', 'assets/new/a.md': 'a\n' });
    await manager(home, { sourceDir: v1, version: '0.1.0' }).ensureForAgent('codex');
    const assetsDir = path.join(target, 'assets');
    await mkdir(assetsDir, { recursive: true });
    const item = (await manager(home, { sourceDir: v2, version: '0.2.0', canWriteDir: async (d) => d !== assetsDir }).check(['codex']))[0];
    expect(item?.ok).toBe(false);
    expect(item?.detail).toContain(assetsDir);
  });

  it('已安装更高协议版本但锁目录不可写时也是失败项', async () => {
    const home = await tempDir();
    const target = targetDir(home);
    const newer = await makeSource('newer', { 'SKILL.md': '协议 11\n', 'references/commands.md': 'c\n' });
    await manager(home, { sourceDir: newer, protocol: 11, version: '2.0.0' }).ensureForAgent('codex');
    const item = (await manager(home, { protocol: 10, version: '1.0.0', canWriteDir: async (d) => d !== path.dirname(target) }).check(['codex']))[0];
    expect(item?.ok).toBe(false);
    expect(item?.detail).toContain('锁文件');
  });

  it('内置 skill 不完整或 Agent 不认识时是失败项', async () => {
    const home = await tempDir();
    const empty = await tempDir();
    const broken = (await manager(home, { sourceDir: empty }).check(['codex']))[0];
    expect(broken?.ok).toBe(false);
    expect(broken?.detail).toContain('不完整');

    const unknown = (await manager(home).check(['other'])).at(0);
    expect(unknown?.ok).toBe(false);
    expect(unknown?.detail).toContain('未知的 Agent');
  });
});

describe('随包分发', () => {
  it('package.json 的 files 包含 skills（npm 包里带着它）', async () => {
    const pkg = JSON.parse(await readFile(new URL('../package.json', import.meta.url), 'utf8')) as { files?: string[] };
    expect(pkg.files).toContain('skills');
  });

  it('SKILL.md 有正确的 frontmatter，命令说明覆盖全部 8 个公开子命令', async () => {
    const dir = bundledSkillDir();
    const skill = await readFile(path.join(dir, 'SKILL.md'), 'utf8');
    expect(skill.startsWith('---\n')).toBe(true);
    expect(skill).toMatch(/^name: kiln-task$/m);
    expect(skill).toMatch(/^description: .+$/m);
    for (const sub of ['info', 'check-report', 'claim', 'deliver', 'ci', 'doc list', 'doc get', 'exec', 'done']) {
      expect(skill, sub).toContain(`\`${sub}`);
    }
    const commands = await readFile(path.join(dir, 'references/commands.md'), 'utf8');
    // 每个公开子命令都有自己的完整契约段落
    for (const heading of [
      /^## \d+\. `info`/m,
      /^## \d+\. `check-report`/m,
      /^## \d+\. `claim`/m,
      /^## \d+\. `deliver`/m,
      /^## \d+\. `ci \[--sha <commit>\]`/m,
      /^## \d+\. `doc list` \/ `doc get <doc_id>`/m,
      /^## \d+\. `exec -- <命令> \[参数…\]`/m,
      /^## \d+\. `done --outcome succeeded\|failed \[--summary "…"\]`/m,
    ]) {
      expect(commands).toMatch(heading);
    }
    // 内部子命令被明确排除在公开命令之外
    for (const internal of ['exec-env', 'git-credential', 'git-remote', 'git-refused', 'done-receipt']) {
      expect(skill).toContain(internal);
    }
  });

  it('命令说明里的 ci 状态与 Server 的取值一致（none/pending/succeeded/failed/cancelled）', async () => {
    const commands = await readFile(path.join(bundledSkillDir(), 'references/commands.md'), 'utf8');
    for (const state of ['none', 'pending', 'succeeded', 'failed', 'cancelled']) {
      expect(commands, state).toContain(`\`${state}\``);
    }
    // 误写的 success / failure 不能出现在命令说明里
    expect(commands).not.toContain('`success`');
    expect(commands).not.toContain('`failure`');
  });

  it('内置 skill 从包目录读取（与宿主机上的 Agent 配置目录无关）', async () => {
    const dir = bundledSkillDir();
    expect(path.isAbsolute(dir)).toBe(true);
    expect(dir.endsWith(path.join('skills', skillName))).toBe(true);
    // 不在任何 Agent 的配置目录中：测试只读它
    expect(dir).not.toContain(path.join('.claude', 'skills'));
    expect(dir).not.toContain(path.join('.agents', 'skills'));
  });
});
