import { chmod, mkdir, mkdtemp, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { create, toBinary } from '@bufbuild/protobuf';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AITaskSchema } from '../src/gen/kiln/airunner/v2/airunner_pb.js';
import { inspectJournal, recordTask } from '../src/core/attempt.js';
import { emptyBinding, Journal, journalFormat, type JournalRecord, parseRecord } from '../src/core/journal.js';
import { checkJournal } from '../src/host/run.js';

// journal 中每一种读不出来的状态都作为"读不了的记录"返回，不静默丢弃。

/** asRoot：root 下 chmod 000 挡不住读取（CI 的 Linux 镜像以 root 运行），这样的用例跳过、改用损坏的内容。 */
const asRoot = process.getuid?.() === 0;

function spec(attemptId: bigint, epoch = 1n): string {
  return Buffer.from(toBinary(AITaskSchema, create(AITaskSchema, { ref: { attemptId, epoch }, taskId: attemptId, repository: 'acme/widgets' }))).toString('base64');
}

function record(attemptId: string, extra: Partial<JournalRecord> = {}): JournalRecord {
  return { attemptId, epoch: '1', spec: spec(BigInt(attemptId)), stage: 'running', binding: { ...emptyBinding(), dispatchId: `ctx_${attemptId}` }, startedAt: 1, ...extra };
}

let dir = '';
beforeEach(async () => {
  dir = await mkdtemp(path.join(os.tmpdir(), 'kiln-orca-journal-'));
});
afterEach(async () => {
  await chmod(path.join(dir, '4.json'), 0o600).catch(() => undefined);
  await rm(dir, { recursive: true, force: true });
});

const put = (name: string, content: unknown) => writeFile(path.join(dir, name), typeof content === 'string' ? content : JSON.stringify(content));

describe('Journal.list 的归类', () => {
  it('可读的记录返回；读不了、损坏、空文件、字段不对、格式版本不认识、与文件名不一致的都作为读不了的记录返回；临时文件、探测文件、其它扩展名忽略', async () => {
    const j = new Journal(dir);
    await j.save(record('1'));
    await put('2.json', '{"attemptId":');
    await put('3.json', '');
    await put('4.json', JSON.stringify(record('4')));
    if (!asRoot) await chmod(path.join(dir, '4.json'), 0o000);
    await put('5.json.0b1c.tmp', '{');
    await put('.probe', '{}');
    await put('notes.txt', 'x');
    const { binding: _b, ...noBinding } = record('6');
    await put('6.json', noBinding);
    await put('7.json', record('70'));
    await put('8.json', { ...record('8'), stage: 'paused' });
    await put('9.json', { ...record('9'), format: journalFormat + 1 });
    await put('010.json', record('010'));
    await put('11.json', { ...record('11'), complete: { outcome: 1, errorClass: '', message: '', agentVersion: '', binding: emptyBinding() } });
    // 没有格式版本字段的记录按当前版本读取
    await put('12.json', record('12'));
    await put('13.json', { ...record('13'), binding: { ...emptyBinding(), dispatchId: 42 } });
    await put('14.json', { ...record('14'), claim: { workItemId: '9', branch: 'b', phase: 'implement', issues: ['7'] } });
    // 待释放的终端：release 的字段缺失、类型不对，releasing 没有 release，dispatch 为空
    await put('17.json', { ...record('17'), stage: 'releasing', release: { since: 1 } });
    await put('18.json', { ...record('18'), stage: 'releasing', release: { dispatchId: 'ctx_18', since: '1' } });
    await put('19.json', { ...record('19'), stage: 'releasing' });
    await put('20.json', { ...record('20'), release: { dispatchId: '', since: 1 } });
    await put('21.json', { ...record('21'), release: 'ctx_21' });
    await put('22.json', { ...record('22'), stage: 'releasing', release: { dispatchId: 'ctx_22', since: 3 } });
    // 待回收的审计工作区：reclaiming 没有 reclaim、还带着 release，reclaim 的字段不对、clone 为空
    await put('23.json', { ...record('23'), stage: 'reclaiming' });
    await put('24.json', { ...record('24'), stage: 'reclaiming', reclaim: { since: 1, repo: '42', clone: '/c' }, release: { dispatchId: 'ctx_24', since: 1 } });
    await put('25.json', { ...record('25'), reclaim: { since: 1, repo: 42, clone: '/c' } });
    await put('26.json', { ...record('26'), stage: 'reclaiming', reclaim: { since: 1, repo: '42', clone: '' } });
    await put('27.json', { ...record('27'), stage: 'reclaiming', reclaim: { since: 1, repo: '42', clone: '/c' } });
    // 重新选题的轮数（审计）：必须是非负整数
    await put('28.json', { ...record('28'), selectRounds: -1 });
    await put('29.json', { ...record('29'), selectRounds: 1.5 });
    await put('30.json', { ...record('30'), selectRounds: 2 });
    await mkdir(path.join(dir, '15.json'));
    await symlink(path.join(dir, 'missing-target'), path.join(dir, '16.json'));

    const scan = await j.list();
    expect(scan.records.map((r) => r.attemptId)).toEqual(asRoot ? ['1', '12', '22', '27', '30', '4'] : ['1', '12', '22', '27', '30']);
    expect(scan.records.every((r) => !('format' in r))).toBe(true);
    const why = Object.fromEntries(scan.unreadable.map((u) => [u.attemptId, u.error]));
    expect(Object.keys(why).sort()).toEqual(['010', '11', '13', '14', '15', '16', '17', '18', '19', '2', '20', '21', '23', '24', '25', '26', '28', '29', '3', ...(asRoot ? [] : ['4']), '6', '7', '8', '9'].sort());
    for (const u of scan.unreadable) expect(u.file).toBe(path.join(dir, `${u.attemptId}.json`));
    expect(why['2']).toMatch(/不是合法的 JSON/);
    expect(why['3']).toMatch(/不是合法的 JSON/);
    if (!asRoot) expect(why['4']).toMatch(/EACCES/);
    expect(why['6']).toMatch(/字段 binding：应为对象/);
    expect(why['7']).toMatch(/与文件名不一致/);
    expect(why['8']).toMatch(/字段 stage/);
    expect(why['9']).toMatch(/格式版本 2/);
    expect(why['010']).toMatch(/不是规范的十进制数/);
    expect(why['11']).toMatch(/没有结论/);
    expect(why['13']).toMatch(/字段 binding\.dispatchId：应为字符串/);
    expect(why['14']).toMatch(/字段 claim\.issues\[0\]：应为数字/);
    expect(why['15']).toMatch(/EISDIR/);
    expect(why['16']).toMatch(/ENOENT/);
    expect(why['17']).toMatch(/字段 release\.dispatchId：应为字符串/);
    expect(why['18']).toMatch(/字段 release\.since：应为数字/);
    expect(why['19']).toMatch(/releasing 但没有要释放的终端/);
    expect(why['20']).toMatch(/release\.dispatchId：不能为空/);
    expect(why['21']).toMatch(/字段 release：应为对象/);
    expect(why['23']).toMatch(/reclaiming 但没有要回收的工作区/);
    expect(why['24']).toMatch(/reclaiming 但还有没有确认释放的终端/);
    expect(why['25']).toMatch(/字段 reclaim\.repo：应为字符串/);
    expect(why['26']).toMatch(/reclaim\.clone：不能为空/);
    expect(why['28']).toMatch(/字段 selectRounds：应为非负整数/);
    expect(why['29']).toMatch(/字段 selectRounds：应为非负整数/);
  });

  it('save 写入格式版本，读回的记录与写入的相同', async () => {
    const j = new Journal(dir);
    const rec = record('21', { stage: 'stopping', stopSince: 5, startFailed: false, services: { project: 'p', desc: ['d'] } });
    await j.save(rec);
    expect(JSON.parse(await readFile(j.fileOf('21'), 'utf8'))).toMatchObject({ format: journalFormat });
    expect((await j.list()).records).toEqual([rec]);
    const rel = record('23', { stage: 'releasing', release: { dispatchId: 'ctx_23', since: 7 } });
    await j.save(rel);
    expect((await j.list()).records).toEqual([rec, rel]);
    const rc = record('24', { stage: 'reclaiming', reclaim: { since: 8, repo: '42', clone: '/c' } });
    await j.save(rc);
    expect((await j.list()).records).toEqual([rec, rel, rc]);
    const sr = record('25', { selectRounds: 2 });
    await j.save(sr);
    expect((await j.list()).records).toEqual([rec, rel, rc, sr]);
  });

  it('parseRecord 拒绝不是对象的内容', () => {
    expect(() => parseRecord('[]', '1')).toThrow(/整条记录.*应为对象/);
    expect(() => parseRecord('null', '1')).toThrow(/应为对象/);
  });
});

describe('recordTask / inspectJournal：任务还原不出来的记录', () => {
  it('spec 不是 base64、不是任务、缺少 attempt、attempt 或 epoch 与记录不一致时抛出', () => {
    expect(recordTask(record('31')).ref?.attemptId).toBe(31n);
    expect(() => recordTask(record('31', { spec: '这不是 base64' }))).toThrow(/base64/);
    expect(() => recordTask(record('31', { spec: Buffer.from([0xff, 0xff, 0xff]).toString('base64') }))).toThrow(/不是合法的任务/);
    expect(() => recordTask(record('31', { spec: Buffer.from(toBinary(AITaskSchema, create(AITaskSchema, { taskId: 31n }))).toString('base64') }))).toThrow(/缺少 attempt/);
    expect(() => recordTask(record('31', { spec: spec(32n) }))).toThrow(/attempt 32 与记录不一致/);
    expect(() => recordTask(record('31', { spec: spec(31n, 2n) }))).toThrow(/epoch 2/);
  });

  it('要恢复执行的记录任务还原不出来时算读不了；stopping、releasing、reclaiming 不需要任务', async () => {
    const j = new Journal(dir);
    await j.save(record('41'));
    await j.save(record('42', { spec: 'AAAA' }));
    await j.save(record('43', { stage: 'stopping', spec: 'AAAA' }));
    await j.save(record('44', { stage: 'releasing', spec: 'AAAA', release: { dispatchId: 'ctx_44', since: 1 } }));
    await j.save(record('45', { stage: 'reclaiming', spec: 'AAAA', reclaim: { since: 1, repo: '42', clone: '/c' } }));
    const r = await inspectJournal(j);
    expect(r.recoveries.map((x) => [x.rec.attemptId, x.kind])).toEqual([
      ['41', 'resume'],
      ['43', 'stop'],
      ['44', 'cleanup'],
      ['45', 'cleanup'],
    ]);
    expect(r.recoveries[2]?.rec).toMatchObject({ release: { dispatchId: 'ctx_44', since: 1 } });
    expect(r.recoveries[3]?.rec).toMatchObject({ reclaim: { since: 1, repo: '42', clone: '/c' } });
    expect(r.unreadable).toEqual([{ attemptId: '42', file: j.fileOf('42'), error: expect.stringMatching(/缺少 attempt|不是合法的任务/) }]);
  });
});

describe('doctor 的 journal 检查', () => {
  it('没有问题时通过；有读不了的记录时失败，列出文件、原因和处理方法；目录读不了时失败', async () => {
    const j = new Journal(dir);
    await j.save(record('51'));
    expect(await checkJournal(j)).toMatchObject({ ok: true, problems: [], summary: `${dir}（1 条未结束的记录）` });
    await j.save(record('53', { stage: 'releasing', release: { dispatchId: 'ctx_53', since: 1 } }));
    expect(await checkJournal(j)).toMatchObject({ ok: true, summary: expect.stringContaining('2 条未结束的记录，其中 1 条待释放 Orca 中 Agent 的终端（不占容量') });
    await j.save(record('54', { stage: 'reclaiming', reclaim: { since: 1, repo: '42', clone: '/c' } }));
    await j.save(record('55', { stage: 'releasing', release: { dispatchId: 'ctx_55', since: 1 }, reclaim: { since: 1, repo: '42', clone: '/c' } }));
    expect(await checkJournal(j)).toMatchObject({ ok: true, summary: expect.stringContaining('4 条未结束的记录，其中 2 条待释放 Orca 中 Agent 的终端、2 条待回收的工作区（不占容量') });
    await j.remove('53');
    await j.remove('54');
    await j.remove('55');
    await put('52.json', '{');
    const bad = await checkJournal(j);
    expect(bad.ok).toBe(false);
    expect(bad.summary).toMatch(/1 条记录读不了.*dispatchId/);
    expect(bad.problems).toEqual([expect.stringContaining(path.join(dir, '52.json'))]);
    await writeFile(path.join(dir, 'file'), '');
    expect(await checkJournal(new Journal(path.join(dir, 'file')))).toMatchObject({ ok: false, summary: expect.stringMatching(/ENOTDIR/) });
  });
});
