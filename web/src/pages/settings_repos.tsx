import { LockIcon, RepoIcon } from '@primer/octicons-react'
import { Button, Label, LinkButton, TextInput } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { liveSeconds, useLive } from '../app/live'
import { Box, BoxRow } from '../components/Box'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
import { list } from '../lib/envelope'
import { Crumbs } from './settings/common'
import { docStatus, emptyDocCountLabel } from './settings/docStatus'
import css from './settings_repos.module.css'

// 设置 · 仓库：可搜索、分页的仓库列表，每 5 秒自动更新；pick=env 时是新建仓库级 environment 的仓库选择器。

const repoSchema = z.object({
  FullName: z.string(),
  DefaultBranch: z.string(),
  Private: z.boolean(),
  Active: z.boolean(),
  InstallationID: z.number(),
  /** 外部文档的简要状态；停用的仓库也保留入口。旧页面的数据没有这些字段。 */
  DocBound: z.boolean().default(false),
  /** 空（尚未验证）/ ok / empty / failed。 */
  DocCheckStatus: z.string().default(''),
  DecisionDoc: z.boolean().default(false),
  DocsURL: z.string().default(''),
})
type Repo = z.infer<typeof repoSchema>

/** store.SyncStatus：最近一次"刷新仓库"的结果（字段带 json tag）。 */
const syncSchema = z.object({
  at: z.string(),
  actor: z.string(),
  error: z.string().default(''),
  installations: z.number(),
  repos: z.number(),
})

const schema = z.object({
  Section: z.string(),
  Repos: list(repoSchema),
  Query: z.string(),
  Page: z.number(),
  Pages: z.number(),
  Total: z.number(),
  PrevURL: z.string(),
  NextURL: z.string(),
  /** 非空时是选择器（"env"：为新建仓库级 environment 选择仓库）。 */
  Pick: z.string(),
  Sync: syncSchema.nullable(),
})
type Data = z.infer<typeof schema>

function SyncNote({ sync }: { sync: Data['Sync'] }) {
  return (
    <div className={css.sync} id="r-sync">
      <p>
        <strong>刷新仓库</strong>：从 GitHub 列出 App 安装到的全部仓库（只读仓库列表，不读取分支、tag 和 PR），用于补上没有送达的安装事件。
        {sync ? (
          <>
            最近一次：<Ago t={sync.at} />，{sync.actor}，
            {sync.error ? <span className={css.warn}>失败：{sync.error}</span> : `${sync.installations} 个 installation、${sync.repos} 个启用中的仓库`}。
          </>
        ) : (
          '还没有手动刷新过。'
        )}
      </p>
      <p>
        <strong>补对账</strong>：Kiln 默认不轮询 GitHub，不会自动发现没有送达的 push、tag 或 PR 事件。推送后没有出现 run 时，对该仓库点“补对账”，只读取这一个仓库的分支、tag
        和打开的 PR 并补上漏掉的构建；第一次对账只记录基线，不为之前没有记录过的 ref 构建。
      </p>
    </div>
  )
}

function RepoOps({ r, data }: { r: Repo; data: Data }) {
  const docs = (
    <LinkButton
      href={r.DocsURL || `/settings/repos/docs?repo=${encodeURIComponent(r.FullName)}`}
      size="small"
      aria-label={`${r.FullName} 的外部文档`}
    >
      外部文档
    </LinkButton>
  )
  if (!r.Active) {
    return (
      <>
        {!data.Pick && docs}
        <span className={css.disabled}>已停用的仓库不能配置</span>
      </>
    )
  }
  if (data.Pick) {
    return (
      <LinkButton href={`/settings/environments/new?repo=${encodeURIComponent(r.FullName)}`} variant="primary" size="small" aria-label={`选择 ${r.FullName}`}>
        选择
      </LinkButton>
    )
  }
  return (
    <>
      {docs}
      <LinkButton href={`/settings/scope?repo=${encodeURIComponent(r.FullName)}`} size="small" aria-label={`${r.FullName} 的变量与密钥`}>
        变量与密钥
      </LinkButton>
      <LinkButton href={`/runs?repo=${encodeURIComponent(r.FullName)}`} size="small" aria-label={`${r.FullName} 的运行记录`}>
        运行记录
      </LinkButton>
      <Form action="/settings/repos/reconcile">
        <input type="hidden" name="repo" value={r.FullName} />
        <input type="hidden" name="q" value={data.Query} />
        <input type="hidden" name="page" value={data.Page} />
        <SubmitButton size="small" aria-label={`对 ${r.FullName} 补对账`}>
          补对账
        </SubmitButton>
      </Form>
    </>
  )
}

function RepoRow({ r, data }: { r: Repo; data: Data }) {
  return (
    <BoxRow className={css.row}>
      {r.Private ? <LockIcon className={css.icon} aria-hidden /> : <RepoIcon className={css.icon} aria-hidden />}
      <div>
        <span className={css.name}>{r.FullName}</span>
        <div className={css.meta}>
          {r.DefaultBranch && <span>{r.DefaultBranch} ·</span>}
          <span>{r.Private ? 'private' : 'public'}</span>
          {!r.Active && <Label variant="attention">已停用</Label>}
          {r.DocBound && (
            <>
              <Label variant={docStatus(r.DocCheckStatus).variant}>{docStatus(r.DocCheckStatus).text}</Label>
              {r.DocCheckStatus === 'empty' && (
                <Label variant={emptyDocCountLabel.variant}>{emptyDocCountLabel.text}</Label>
              )}
            </>
          )}
          {r.DecisionDoc && <Label variant="secondary">待决策清单</Label>}
        </div>
      </div>
      <div className={css.ops}>
        <RepoOps r={r} data={data} />
      </div>
    </BoxRow>
  )
}

function SettingsRepos({ data }: PageProps<Data>) {
  useLive(true)
  const pick = data.Pick !== ''
  const clearURL = pick ? `/settings/repos?pick=${encodeURIComponent(data.Pick)}` : '/settings/repos'
  return (
    <SettingsLayout section={data.Section}>
      {pick ? (
        <Subhead description="为哪个仓库新建仓库级 environment？">
          <Crumbs
            items={[
              { label: '环境', href: '/settings/environments' },
              { label: '新建', href: '/settings/environments/new' },
            ]}
            current="选择仓库"
          />
        </Subhead>
      ) : (
        <>
          <Subhead
            actions={
              <Form action="/sync">
                <input type="hidden" name="q" value={data.Query} />
                <input type="hidden" name="page" value={data.Page} />
                <SubmitButton>刷新仓库</SubmitButton>
              </Form>
            }
            description={`Kiln 按 GitHub 事件更新这个列表：安装 App、在安装中增减仓库、仓库第一次推送或开 PR 时，仓库自动出现，页面每 ${liveSeconds} 秒自动更新。`}
          >
            仓库
          </Subhead>
          <SyncNote sync={data.Sync} />
        </>
      )}
      <Form action="/settings/repos" method="get" className={css.search} aria-label="搜索仓库">
        {pick && <input type="hidden" name="pick" value={data.Pick} />}
        <TextInput
          className={css.searchInput}
          type="search"
          name="q"
          defaultValue={data.Query}
          aria-label="搜索仓库"
          placeholder="仓库名的一部分，例如 api"
          spellCheck={false}
          autoCapitalize="off"
        />
        <Button type="submit">搜索</Button>
        {data.Query && <LinkButton href={clearURL}>清除</LinkButton>}
      </Form>
      <p className={css.count}>
        {data.Query ? `与“${data.Query}”匹配的 ` : '共 '}
        {data.Total} 个仓库{data.Pages > 1 && `，第 ${data.Page} / ${data.Pages} 页`}
      </p>
      <Box>
        {data.Repos.map((r) => (
          <RepoRow key={r.FullName} r={r} data={data} />
        ))}
        {data.Repos.length === 0 && (
          <BoxRow className={css.empty}>
            {data.Query
              ? '没有匹配的仓库'
              : '还没有仓库。仓库在第一次收到它的事件（安装 App、推送、开 PR）时自动出现；也可以点“刷新仓库”立即列出。确认 App 已安装、账号在 KILN_ALLOWED_ACCOUNTS 中。'}
          </BoxRow>
        )}
      </Box>
      {data.Pages > 1 && (
        <nav className={css.pager} aria-label="分页">
          {data.PrevURL && (
            <LinkButton href={data.PrevURL} rel="prev">
              上一页
            </LinkButton>
          )}
          <span>
            第 {data.Page} / {data.Pages} 页
          </span>
          {data.NextURL && (
            <LinkButton href={data.NextURL} rel="next">
              下一页
            </LinkButton>
          )}
        </nav>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsRepos)
