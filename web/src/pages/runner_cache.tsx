import { ArrowLeftIcon, DatabaseIcon, SearchIcon } from '@primer/octicons-react'
import { Checkbox, CheckboxGroup, FormControl, Label, LinkButton, Select } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxHeader, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { EmptyState } from '../components/EmptyState'
import { ConfirmForm, Form, SubmitButton } from '../components/Form'
import { PageHeader } from '../components/PageHeader'
import { runnerViewSchema } from '../components/RunnerSchema'
import { Ago } from '../components/Time'
import { list } from '../lib/envelope'
import { clock, size } from '../lib/format'
import css from './runner_cache.module.css'

// Runner 的本地缓存（PLAN #8.3）：Runner 最近一次上报的缓存清单、上一次自动清理的结果和清理请求。
// 管理员可以按仓库、类别和执行身份预览清理范围（GET 参数，服务端算好），确认后发起清理（Runner 空闲时执行）。

const entrySchema = z.object({
  Kind: z.string(),
  KindLabel: z.string(),
  /** 所属仓库（owner/name）；仓库不在 Kiln 中时为 #ID，不属于单个仓库时为空。 */
  Repo: z.string(),
  Platform: z.string(),
  /** 执行身份（host、oci-root 等），只有 toolcache 有。 */
  Domain: z.string(),
  Name: z.string(),
  /** 数据目录中的相对路径。 */
  Path: z.string(),
  Size: z.number(),
  LastUsed: z.string().nullable(),
  Expires: z.string().nullable(),
})
type Entry = z.infer<typeof entrySchema>

const failureSchema = z.object({ Kind: z.string(), Path: z.string(), Error: z.string() })
type Failure = z.infer<typeof failureSchema>

const purgeSchema = z.object({
  ID: z.number(),
  Repo: z.string(),
  Kinds: list(z.string()),
  Domain: z.string(),
  /** pending：等待 Runner 空闲后执行；done：已执行。 */
  State: z.string(),
  Actor: z.string(),
  Created: z.string(),
  Done: z.string().nullable(),
  Removed: z.number(),
  RemovedBytes: z.number(),
  Failures: list(failureSchema),
})
type Purge = z.infer<typeof purgeSchema>

const optionSchema = z.object({ Value: z.string(), Label: z.string() })

const schema = z.object({
  Runner: runnerViewSchema,
  /** 清单生成的时间；Runner 还没有上报过时为 null。 */
  Generated: z.string().nullable(),
  Entries: list(entrySchema),
  TotalBytes: z.number(),
  DiskTotal: z.number(),
  DiskFree: z.number(),
  GC: z
    .object({
      At: z.string(),
      Removed: z.number(),
      RemovedBytes: z.number(),
      Failures: list(failureSchema),
    })
    .nullable(),
  Purges: list(purgeSchema),
  /** 清理表单的选项（只有管理员有 Repos）和当前取值。 */
  Kinds: list(optionSchema),
  Repos: list(z.string()),
  Domains: list(z.string()),
  Repo: z.string(),
  KindsPicked: list(z.string()),
  Domain: z.string(),
  /** 按当前取值预览的结果（管理员带 preview=1 打开时）。 */
  Preview: z
    .object({
      Matched: list(entrySchema),
      MatchedBytes: z.number(),
      Shared: list(entrySchema),
      Notes: list(z.string()),
      Error: z.string(),
    })
    .nullable(),
  Self: z.string(),
})
type Data = z.infer<typeof schema>

/** kindText 返回类别的说明（未知的类别原样显示）。 */
function kindText(data: Data, kind: string): string {
  return data.Kinds.find((k) => k.Value === kind)?.Label ?? kind
}

/** scopeText 返回清理范围的一句话说明，例如"仓库 a/b，类别 toolcache、git 镜像，执行身份 host"。 */
export function scopeText(data: Data, repo: string, kinds: readonly string[], domain: string): string {
  return [
    repo ? `仓库 ${repo}` : '全部仓库',
    kinds.length > 0 ? `类别 ${kinds.map((k) => kindText(data, k)).join('、')}` : '全部类别',
    domain ? `执行身份 ${domain}` : '全部执行身份',
  ].join('，')
}

function Failures({ list: items, label }: { list: Failure[]; label: string }) {
  if (items.length === 0) return null
  return (
    <ul className={css.failures} aria-label={label}>
      {items.map((f) => (
        <li key={`${f.Kind}:${f.Path}`}>
          <span className="mono">{f.Path || f.Kind}</span>：<span className={css.err}>{f.Error}</span>
        </li>
      ))}
    </ul>
  )
}

function EntriesTable({ entries, label }: { entries: Entry[]; label: string }) {
  return (
    <div className={css.scroll}>
      <table className={css.table} aria-label={label}>
        <thead>
          <tr>
            <th scope="col">类别</th>
            <th scope="col">仓库</th>
            <th scope="col">平台</th>
            <th scope="col">执行身份</th>
            <th scope="col">名称</th>
            <th scope="col" className={css.right}>
              大小
            </th>
            <th scope="col">最后使用</th>
            <th scope="col">到期</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <tr key={`${e.Kind}:${e.Path}:${e.Name}`}>
              <td data-label="类别">{e.KindLabel}</td>
              <td data-label="仓库">{e.Repo || <span className={css.muted}>共享</span>}</td>
              <td data-label="平台" className="mono">
                {e.Platform || '—'}
              </td>
              <td data-label="执行身份">{e.Domain || '—'}</td>
              <td data-label="名称" className={css.name} title={e.Path}>
                {e.Name || e.Path}
              </td>
              <td data-label="大小" className={`num ${css.right ?? ''}`}>
                {size(e.Size)}
              </td>
              <td data-label="最后使用" className={css.nowrap}>
                <Ago t={e.LastUsed} fallback="—" />
              </td>
              <td data-label="到期" className={css.nowrap}>
                {e.Expires ? <time dateTime={e.Expires}>{clock(e.Expires)}</time> : '—'}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function Summary({ data }: { data: Data }) {
  return (
    <dl className={css.summary}>
      <div>
        <dt>清单</dt>
        <dd>
          <Ago t={data.Generated} fallback="未上报" />
          {data.Generated && '生成'}
        </dd>
      </div>
      <div>
        <dt>缓存总大小</dt>
        <dd>
          {size(data.TotalBytes)}，共 {data.Entries.length} 项
        </dd>
      </div>
      {data.DiskTotal > 0 && (
        <div>
          <dt>数据目录所在磁盘</dt>
          <dd>
            可用 {size(data.DiskFree)} / 共 {size(data.DiskTotal)}
          </dd>
        </div>
      )}
    </dl>
  )
}

function GC({ data }: { data: Data }) {
  const gc = data.GC
  return (
    <Box as="section" aria-labelledby="gc-title">
      <BoxHeader title="上一次自动清理" id="gc-title" />
      <BoxRow className={css.block}>
        {gc ? (
          <>
            <p className={css.line}>
              <Ago t={gc.At} />：删除 {gc.Removed} 项（{size(gc.RemovedBytes)}）
              {gc.Failures.length > 0 && <span className={css.err}>，{gc.Failures.length} 项删除失败</span>}
            </p>
            <Failures list={gc.Failures} label="自动清理失败的对象" />
          </>
        ) : (
          <p className={css.muted}>还没有记录。Runner 定期删除超过 12 小时没有使用的缓存（装好的 runtime 是 30 天），并在上报清单时带上结果。</p>
        )}
      </BoxRow>
    </Box>
  )
}

function PurgeRow({ data, p }: { data: Data; p: Purge }) {
  return (
    <BoxRow as="li" className={css.block}>
      <div className={css.purgeHead}>
        <b>#{p.ID}</b>
        <span>{scopeText(data, p.Repo, p.Kinds, p.Domain)}</span>
        {p.State === 'done' ? <Label variant="success">已完成</Label> : <Label variant="attention">等待 Runner 空闲后执行</Label>}
      </div>
      <p className={css.meta}>
        {p.Actor || '—'} 于 <Ago t={p.Created} /> 发起
        {p.State === 'done' && (
          <>
            ；<Ago t={p.Done} />
            执行，删除 {p.Removed} 项（{size(p.RemovedBytes)}）
            {p.Failures.length > 0 && <span className={css.err}>，{p.Failures.length} 项删除失败</span>}
          </>
        )}
      </p>
      <Failures list={p.Failures} label={`清理请求 #${p.ID} 失败的对象`} />
    </BoxRow>
  )
}

function Purges({ data }: { data: Data }) {
  return (
    <Box as="section" aria-labelledby="purges-title">
      <BoxHeader title="清理请求" id="purges-title" />
      {data.Purges.length > 0 ? (
        <ul className={css.rows} aria-label="清理请求">
          {data.Purges.map((p) => (
            <PurgeRow key={p.ID} data={data} p={p} />
          ))}
        </ul>
      ) : (
        <BoxRow className={css.muted}>还没有清理请求。</BoxRow>
      )}
    </Box>
  )
}

/** PurgeForm 是清理范围的表单：预览（GET，带 preview=1）在本页显示将删除的内容。 */
function PurgeForm({ data }: { data: Data }) {
  // 换了取值后重建表单，使默认值与页面数据一致
  const key = [data.Repo, data.KindsPicked.join(','), data.Domain].join('|')
  return (
    <Box as="section" aria-labelledby="purge-title">
      <BoxHeader title="清理缓存" id="purge-title" />
      <BoxRow className={css.block}>
        <Form key={key} action={data.Self} method="get" className={css.form} aria-label="清理范围">
          <input type="hidden" name="preview" value="1" />
          <FormControl>
            <FormControl.Label>仓库</FormControl.Label>
            <Select name="repo" defaultValue={data.Repo}>
              <Select.Option value="">全部仓库</Select.Option>
              {data.Repos.map((r) => (
                <Select.Option key={r} value={r}>
                  {r}
                </Select.Option>
              ))}
            </Select>
            <FormControl.Caption>选择仓库时只清理这个仓库的 toolcache、git 镜像和工作区。</FormControl.Caption>
          </FormControl>
          <CheckboxGroup>
            <CheckboxGroup.Label>类别</CheckboxGroup.Label>
            <CheckboxGroup.Caption>都不选表示全部类别。</CheckboxGroup.Caption>
            <div className={css.kinds}>
              {data.Kinds.map((k) => (
                <FormControl key={k.Value}>
                  <Checkbox name="kind" value={k.Value} defaultChecked={data.KindsPicked.includes(k.Value)} />
                  <FormControl.Label>{k.Label}</FormControl.Label>
                </FormControl>
              ))}
            </div>
          </CheckboxGroup>
          <FormControl>
            <FormControl.Label>执行身份</FormControl.Label>
            <Select name="domain" defaultValue={data.Domain}>
              <Select.Option value="">全部执行身份</Select.Option>
              {data.Domains.map((d) => (
                <Select.Option key={d} value={d}>
                  {d}
                </Select.Option>
              ))}
            </Select>
            <FormControl.Caption>toolcache 按执行身份分开存放：host 是 Runner 用户，oci-root 是容器中的 root。</FormControl.Caption>
          </FormControl>
          <div>
            <SubmitButton leadingVisual={SearchIcon}>预览</SubmitButton>
          </div>
        </Form>
      </BoxRow>
    </Box>
  )
}

function Preview({ data }: { data: Data }) {
  const p = data.Preview
  if (!p) return null
  const name = data.Runner.Name
  const scope = scopeText(data, data.Repo, data.KindsPicked, data.Domain)
  if (p.Error) {
    return (
      <Callout tone="critical" title="无法预览">
        {p.Error}
      </Callout>
    )
  }
  return (
    <Box as="section" aria-labelledby="preview-title">
      <BoxHeader title="预览" id="preview-title">
        <ConfirmForm
          action={`/runners/cache/${encodeURIComponent(name)}/purge`}
          fields={{ repo: data.Repo, kind: data.KindsPicked.join(','), domain: data.Domain }}
          button="清理…"
          buttonProps={{ size: 'small' }}
          title={`清理 ${name} 的本地缓存？`}
          confirmLabel="确认清理"
        >
          <ul className={css.confirmList}>
            <li>范围：{scope}。</li>
            <li>
              按上报的清单，会删除 {p.Matched.length} 项（{size(p.MatchedBytes)}）；实际以 Runner 上的内容为准。
            </li>
            <li>{name} 在没有正在执行的 job 时执行清理，期间不领取新任务；之后用到这些缓存的 job 会重新下载或构建。</li>
          </ul>
        </ConfirmForm>
      </BoxHeader>
      <BoxRow className={css.block}>
        <p className={css.line}>
          {scope}：将删除 <b>{p.Matched.length}</b> 项，共 <b>{size(p.MatchedBytes)}</b>。
        </p>
        {p.Notes.length > 0 && (
          <ul className={css.notes} aria-label="预览说明">
            {p.Notes.map((n) => (
              <li key={n}>{n}</li>
            ))}
          </ul>
        )}
      </BoxRow>
      {p.Matched.length > 0 && <EntriesTable entries={p.Matched} label="将删除的缓存" />}
      {p.Shared.length > 0 && (
        <>
          <BoxRow className={css.subhead}>继续保留的共享缓存（{p.Shared.length} 项）</BoxRow>
          <EntriesTable entries={p.Shared} label="继续保留的共享缓存" />
        </>
      )}
    </Box>
  )
}

function RunnerCache({ data, env }: PageProps<Data>) {
  const admin = env.Session?.Admin ?? false
  const r = data.Runner
  // 清理请求还没执行（等待 Runner 空闲）时自动刷新
  useLive(data.Purges.some((p) => p.State === 'pending'))
  return (
    <div className={css.page}>
      <PageHeader
        title={`${r.Name} 的本地缓存`}
        description="Runner 最近一次上报的缓存清单：toolcache、runtime、git 镜像、CAS、镜像等。超过 12 小时没有使用的缓存由 Runner 自动删除；装好的 runtime 是工具，30 天没有使用才删除。"
        actions={
          <LinkButton href="/runners" leadingVisual={ArrowLeftIcon}>
            Runner 列表
          </LinkButton>
        }
      />

      {r.Revoked && (
        <Callout tone="warning" title="已吊销">
          {r.Name} 已吊销，不会再上报清单，也不能清理。
        </Callout>
      )}

      <Summary data={data} />

      <Box as="section" aria-labelledby="entries-title">
        <BoxHeader title="缓存清单" id="entries-title" />
        {data.Entries.length > 0 ? (
          <EntriesTable entries={data.Entries} label="缓存清单" />
        ) : (
          <EmptyState icon={DatabaseIcon} title={data.Generated ? '没有缓存' : '还没有上报缓存清单'}>
            {data.Generated ? '清单为空。' : '旧版 kiln-runner 不上报缓存清单；新版在空闲时定期上报，清理后也会上报。'}
          </EmptyState>
        )}
      </Box>

      <GC data={data} />
      <Purges data={data} />

      {admin && !r.Revoked && (
        <>
          <PurgeForm data={data} />
          <Preview data={data} />
        </>
      )}
      {!admin && <p className={css.muted}>清理缓存需要管理员。</p>}
    </div>
  )
}

export default definePage(schema, RunnerCache)
