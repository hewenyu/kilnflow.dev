import { LinkExternalIcon } from '@primer/octicons-react'
import { Label } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box, BoxHeader, BoxRow } from '../components/Box'
import { Callout } from '../components/Callout'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
import { Crumbs, settingsCss } from './settings/common'
import { docStatus, emptyDocCountLabel } from './settings/docStatus'
import css from './settings_repo_docs.module.css'

// 设置 · 仓库 · 外部文档：只读展示绑定、验证状态与待决策清单；验证由管理员显式触发。
// 绑定只保存配置，普通页面加载和自动刷新不访问 Outline；token 与密文从不返回浏览器。

const bindingSchema = z.object({
  Provider: z.string(),
  BaseURL: z.string(),
  /** 服务端校验过的站点地址；不能安全生成时为空。 */
  SiteURL: z.string(),
  CollectionID: z.string(),
  CollectionName: z.string(),
  /** 文档集链接：验证前按配置生成，验证后用观测到的地址。 */
  CollectionURL: z.string(),
  CreatedBy: z.string(),
  /** 为空表示历史行没有记录（显示"未记录"）。 */
  UpdatedBy: z.string(),
  CreatedAt: z.string(),
  UpdatedAt: z.string(),
  Generation: z.number(),
  /** 空（尚未验证）/ ok / empty / failed。 */
  CheckStatus: z.string(),
  CheckStartedAt: z.string().nullable(),
  CheckedAt: z.string().nullable(),
  CheckedBy: z.string(),
  CheckErrorCode: z.string(),
  CheckErrorText: z.string(),
  CheckDocCount: z.number().nullable(),
  CheckSampleDocID: z.string(),
  ObservedCollectionName: z.string(),
})
type Binding = z.infer<typeof bindingSchema>

const decisionSchema = z.object({
  Configured: z.boolean(),
  Provider: z.string(),
  BaseURL: z.string(),
  SiteURL: z.string(),
  DocID: z.string(),
  CollectionID: z.string(),
  DocURL: z.string(),
  ObservedUnits: z.number().nullable(),
  WarningCode: z.string(),
  CreatedBy: z.string(),
  UpdatedBy: z.string(),
  UpdatedAt: z.string(),
})
type Decision = z.infer<typeof decisionSchema>

const schema = z.object({
  Section: z.string(),
  Repo: z.string(),
  Active: z.boolean(),
  Binding: bindingSchema.nullable(),
  Decision: decisionSchema,
})
type Data = z.infer<typeof schema>

/** CheckLabel 区分"配置已保存"和"已验证可以读取"，不把保存当成连接正常。 */
function CheckLabel({ status }: { status: string }) {
  const s = docStatus(status)
  return (
    <>
      <Label variant={s.variant}>{s.text}</Label>
      {status === 'empty' && <Label variant={emptyDocCountLabel.variant}>{emptyDocCountLabel.text}</Label>}
    </>
  )
}

function Provider({ name }: { name: string }) {
  return <>{name === 'outline' ? 'Outline' : name}</>
}

function BindingBox({ repo, b }: { repo: string; b: Binding | null }) {
  if (!b) {
    return (
      <Box as="section" aria-labelledby="doc-binding">
        <BoxHeader id="doc-binding" title="文档集绑定">
          <Label variant="secondary">未绑定</Label>
        </BoxHeader>
        <BoxRow className={css.block}>
          <p>这个仓库还没有绑定文档集。绑定用 CLI，token 从标准输入读取：</p>
          <pre className={settingsCss.pre}>
            {`kiln doc bind ${repo} \\\n  --base-url https://outline.example.com \\\n  --collection <collection-uuid> --name 设计`}
          </pre>
          <p className={settingsCss.muted}>
            绑定只把配置保存到 Kiln，不访问 Outline。保存后回到这里点“验证读取”，确认 token 能读到文档集；验证前页面不会显示连接正常。
          </p>
        </BoxRow>
      </Box>
    )
  }
  const observed = b.ObservedCollectionName && b.ObservedCollectionName !== b.CollectionName ? b.ObservedCollectionName : ''
  return (
    <Box as="section" aria-labelledby="doc-binding">
      <BoxHeader id="doc-binding" title="文档集绑定">
        <CheckLabel status={b.CheckStatus} />
      </BoxHeader>
      <BoxRow className={css.block}>
        <dl className={css.kv}>
          <dt>提供方</dt>
          <dd>
            <Provider name={b.Provider} />
          </dd>
          <dt>站点</dt>
          <dd>
            {b.SiteURL ? (
              <a href={b.SiteURL} target="_blank" rel="noreferrer">
                {b.BaseURL}
              </a>
            ) : (
              <code>{b.BaseURL}</code>
            )}
          </dd>
          <dt>文档集</dt>
          <dd>
            {b.CollectionName || '（没有记录名称）'} · <code>{b.CollectionID}</code>
            {b.CollectionURL && (
              <a className={css.ext} href={b.CollectionURL} target="_blank" rel="noreferrer">
                在 Outline 中打开 <LinkExternalIcon aria-hidden />
              </a>
            )}
            {observed && <div className={css.note}>验证时观测到的名称：{observed}</div>}
          </dd>
          <dt>配置</dt>
          <dd>
            <Ago t={b.UpdatedAt} /> 更新
            {b.UpdatedBy ? `，由 ${b.UpdatedBy}` : '（历史记录没有更新者）'}
            <div className={css.note}>
              创建于 <Ago t={b.CreatedAt} />
              {b.CreatedBy && `，由 ${b.CreatedBy}`} · 第 {b.Generation} 代
            </div>
          </dd>
          <dt>读取验证</dt>
          <dd>
            <CheckStatus b={b} />
          </dd>
        </dl>
      </BoxRow>
    </Box>
  )
}

function CheckStatus({ b }: { b: Binding }) {
  if (b.CheckStatus === 'ok' || b.CheckStatus === 'empty') {
    return (
      <>
        {b.CheckStatus === 'ok' ? (
          <>
            可以读取：文档集可访问，共 {b.CheckDocCount ?? 0} 篇文档
            {b.CheckSampleDocID && (
              <>
                ，抽查了 <code>{b.CheckSampleDocID}</code> 的正文
              </>
            )}
          </>
        ) : (
          <>可以读取：文档集可访问，但当前没有文档（冻结时会记录一个空集合，不是读取失败）</>
        )}
        <div className={css.note}>
          <Ago t={b.CheckedAt} />
          {b.CheckedBy && `，由 ${b.CheckedBy}`} · 只读检查，不测试写 token
        </div>
      </>
    )
  }
  if (b.CheckStatus === 'failed') {
    return (
      <>
        <span className={css.failed}>{b.CheckErrorText || '读取失败'}</span>
        {b.CheckErrorCode && (
          <>
            {' '}
            <code className={css.note}>{b.CheckErrorCode}</code>
          </>
        )}
        <div className={css.note}>
          <Ago t={b.CheckedAt} />
          {b.CheckedBy && `，由 ${b.CheckedBy}`} · 检查没有读到文档集；配置仍然保留，修正后可以重新验证
        </div>
      </>
    )
  }
  return (
    <>
      还没有验证。绑定只保存了配置，不会访问 Outline，也不能据此认为连接正常。
      <div className={css.note}>点右上角“验证读取”做一次只读检查（列出文档集与文档、抽查一篇正文）。</div>
    </>
  )
}

function DecisionBox({ d }: { d: Decision }) {
  if (!d.Configured) {
    return (
      <Box as="section" aria-labelledby="decision-doc">
        <BoxHeader id="decision-doc" title="待决策清单">
          <Label variant="secondary">未配置</Label>
        </BoxHeader>
        <BoxRow className={css.block}>
          <p>没有配置待决策清单。AI 开发在 PR 中记录的有疑问决定不会写进 Outline。</p>
          <pre className={settingsCss.pre}>{`printf '%s' "$OUTLINE_WRITE_TOKEN" | kiln doc decision-doc set <repo> --doc-id <doc-uuid> --base-url https://outline.example.com`}</pre>
          <p className={settingsCss.muted}>写 token 只从标准输入读取，保存后不会再显示；这条命令会先用它只读校验目标文档。</p>
        </BoxRow>
      </Box>
    )
  }
  return (
    <Box as="section" aria-labelledby="decision-doc">
      <BoxHeader id="decision-doc" title="待决策清单">
        <Label variant="accent">已配置</Label>
      </BoxHeader>
      <BoxRow className={css.block}>
        <dl className={css.kv}>
          <dt>站点</dt>
          <dd>
            {d.SiteURL ? (
              <a href={d.SiteURL} target="_blank" rel="noreferrer">
                {d.BaseURL}
              </a>
            ) : (
              <code>{d.BaseURL}</code>
            )}
          </dd>
          <dt>文档</dt>
          <dd>
            {d.DocURL ? (
              <a href={d.DocURL} target="_blank" rel="noreferrer">
                <code>{d.DocID}</code>
              </a>
            ) : (
              <code>{d.DocID}</code>
            )}
            {d.CollectionID && (
              <div className={css.note}>
                文档集 <code>{d.CollectionID}</code>
              </div>
            )}
          </dd>
          <dt>篇幅</dt>
          <dd>
            {d.ObservedUnits != null ? (
              <>
                {d.ObservedUnits} 个字符（UTF-16 code unit）
                {d.WarningCode === 'doc_near_limit' && (
                  <>
                    {' '}
                    <Label variant="attention">接近推荐篇幅</Label>
                  </>
                )}
              </>
            ) : (
              <span className={settingsCss.muted}>还没有量过</span>
            )}
          </dd>
          <dt>配置</dt>
          <dd>
            <Ago t={d.UpdatedAt} /> 更新{d.UpdatedBy && `，由 ${d.UpdatedBy}`}
            {d.CreatedBy && (
              <div className={css.note}>
                创建者 {d.CreatedBy}
              </div>
            )}
          </dd>
        </dl>
        <p className={settingsCss.muted}>
          这里不显示写 token，也不访问 Outline。读取验证只检查只读绑定，不测试这篇文档的写权限。
        </p>
      </BoxRow>
    </Box>
  )
}

function SettingsRepoDocs({ data }: PageProps<Data>) {
  useLive(true)
  return (
    <SettingsLayout section={data.Section}>
      <Subhead
        actions={
          data.Binding && (
            <Form action="/settings/repos/docs/verify" aria-label="验证文档读取">
              <input type="hidden" name="repo" value={data.Repo} />
              <SubmitButton>验证读取</SubmitButton>
            </Form>
          )
        }
        description={
          <>
            绑定只保存配置，不访问 Outline；普通页面加载和自动刷新都不会读取远端。只有管理员点“验证读取”时才做一次只读检查。
          </>
        }
      >
        <Crumbs items={[{ label: '仓库', href: '/settings/repos' }]} current={`${data.Repo} · 外部文档`} />
      </Subhead>

      {!data.Active && (
        <Callout tone="warning" title="已停用的仓库">
          这个仓库已停用，不能再修改绑定；仍然可以查看历史配置和做只读验证。
        </Callout>
      )}

      <BindingBox repo={data.Repo} b={data.Binding} />
      <DecisionBox d={data.Decision} />

      <p className={settingsCss.muted}>
        页面展示的是仓库当前的配置。AI 工作项和任务使用的是冻结时的快照，重绑或修改 Outline
        不会改变已有工作项的历史依据；历史依据在对应的 AI 工作项、请求和任务页面上查看。
      </p>
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsRepoDocs)
