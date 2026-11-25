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
import { Msg, Trans, useT } from '../i18n/locale'
import { isEmpty, type MsgKey } from '../i18n/messages'
import { textSchema } from '../lib/envelope'
import { Crumbs, settingsCss, settingsTags, withAgo } from './settings/common'
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
  /** 失败原因：按 CheckErrorCode 给出的消息。 */
  CheckErrorText: textSchema,
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
  const t = useT()
  const s = docStatus(status)
  return (
    <>
      <Label variant={s.variant}>{t(s.text)}</Label>
      {status === 'empty' && <Label variant={emptyDocCountLabel.variant}>{t(emptyDocCountLabel.text)}</Label>}
    </>
  )
}

function Provider({ name }: { name: string }) {
  return <>{name === 'outline' ? 'Outline' : name}</>
}

function BindingBox({ repo, b }: { repo: string; b: Binding | null }) {
  const t = useT()
  if (!b) {
    return (
      <Box as="section" aria-labelledby="doc-binding">
        <BoxHeader id="doc-binding" title={t('repoDocs.bindingTitle')}>
          <Label variant="secondary">{t('repoDocs.unbound')}</Label>
        </BoxHeader>
        <BoxRow className={css.block}>
          <p>{t('repoDocs.noBinding')}</p>
          <pre className={settingsCss.pre}>
            {`kiln doc bind ${repo} \\\n  --base-url https://outline.example.com \\\n  --collection <collection-uuid> --name ${t('repoDocs.exampleName')}`}
          </pre>
          <p className={settingsCss.muted}>{t('repoDocs.bindNote')}</p>
        </BoxRow>
      </Box>
    )
  }
  const observed = b.ObservedCollectionName && b.ObservedCollectionName !== b.CollectionName ? b.ObservedCollectionName : ''
  return (
    <Box as="section" aria-labelledby="doc-binding">
      <BoxHeader id="doc-binding" title={t('repoDocs.bindingTitle')}>
        <CheckLabel status={b.CheckStatus} />
      </BoxHeader>
      <BoxRow className={css.block}>
        <dl className={css.kv}>
          <dt>{t('repoDocs.provider')}</dt>
          <dd>
            <Provider name={b.Provider} />
          </dd>
          <dt>{t('repoDocs.site')}</dt>
          <dd>
            {b.SiteURL ? (
              <a href={b.SiteURL} target="_blank" rel="noreferrer">
                {b.BaseURL}
              </a>
            ) : (
              <code>{b.BaseURL}</code>
            )}
          </dd>
          <dt>{t('repoDocs.collection')}</dt>
          <dd>
            {b.CollectionName || t('repoDocs.noName')} · <code>{b.CollectionID}</code>
            {b.CollectionURL && (
              <a className={css.ext} href={b.CollectionURL} target="_blank" rel="noreferrer">
                {t('repoDocs.openOutline')} <LinkExternalIcon aria-hidden />
              </a>
            )}
            {observed && <div className={css.note}>{t('repoDocs.observedName', { name: observed })}</div>}
          </dd>
          <dt>{t('repoDocs.config')}</dt>
          <dd>
            {b.UpdatedBy ? (
              <Trans id="repoDocs.updatedBy" params={{ login: b.UpdatedBy }} tags={withAgo(b.UpdatedAt)} />
            ) : (
              <Trans id="repoDocs.updatedNoBy" tags={withAgo(b.UpdatedAt)} />
            )}
            <div className={css.note}>
              {b.CreatedBy ? (
                <Trans id="repoDocs.created" params={{ login: b.CreatedBy, gen: b.Generation }} tags={withAgo(b.CreatedAt)} />
              ) : (
                <Trans id="repoDocs.createdNoBy" params={{ gen: b.Generation }} tags={withAgo(b.CreatedAt)} />
              )}
            </div>
          </dd>
          <dt>{t('repoDocs.check')}</dt>
          <dd>
            <CheckStatus b={b} />
          </dd>
        </dl>
      </BoxRow>
    </Box>
  )
}

function CheckedNote({ b, by, noBy }: { b: Binding; by: MsgKey; noBy: MsgKey }) {
  return (
    <div className={css.note}>
      {b.CheckedBy ? (
        <Trans id={by} params={{ login: b.CheckedBy }} tags={withAgo(b.CheckedAt)} />
      ) : (
        <Trans id={noBy} tags={withAgo(b.CheckedAt)} />
      )}
    </div>
  )
}

function CheckStatus({ b }: { b: Binding }) {
  const t = useT()
  if (b.CheckStatus === 'ok' || b.CheckStatus === 'empty') {
    return (
      <>
        {b.CheckStatus === 'ok' ? (
          <>
            {t('repoDocs.checkOK', { count: b.CheckDocCount ?? 0 })}
            {b.CheckSampleDocID && <Trans id="repoDocs.checkSample" params={{ doc: b.CheckSampleDocID }} tags={settingsTags} />}
          </>
        ) : (
          t('repoDocs.checkEmpty')
        )}
        <CheckedNote b={b} by="repoDocs.checkedBy" noBy="repoDocs.checkedNoBy" />
      </>
    )
  }
  if (b.CheckStatus === 'failed') {
    return (
      <>
        <span className={css.failed}>{isEmpty(b.CheckErrorText) ? t('docStatus.failed') : <Msg text={b.CheckErrorText} />}</span>
        {b.CheckErrorCode && (
          <>
            {' '}
            <code className={css.note}>{b.CheckErrorCode}</code>
          </>
        )}
        <CheckedNote b={b} by="repoDocs.failedBy" noBy="repoDocs.failedNoBy" />
      </>
    )
  }
  return (
    <>
      {t('repoDocs.notVerified')}
      <div className={css.note}>{t('repoDocs.notVerifiedHint')}</div>
    </>
  )
}

function DecisionBox({ d }: { d: Decision }) {
  const t = useT()
  if (!d.Configured) {
    return (
      <Box as="section" aria-labelledby="decision-doc">
        <BoxHeader id="decision-doc" title={t('repoDocs.decisionTitle')}>
          <Label variant="secondary">{t('repoDocs.notConfigured')}</Label>
        </BoxHeader>
        <BoxRow className={css.block}>
          <p>{t('repoDocs.noDecision')}</p>
          <pre className={settingsCss.pre}>{`printf '%s' "$OUTLINE_WRITE_TOKEN" | kiln doc decision-doc set <repo> --doc-id <doc-uuid> --base-url https://outline.example.com`}</pre>
          <p className={settingsCss.muted}>{t('repoDocs.decisionTokenNote')}</p>
        </BoxRow>
      </Box>
    )
  }
  return (
    <Box as="section" aria-labelledby="decision-doc">
      <BoxHeader id="decision-doc" title={t('repoDocs.decisionTitle')}>
        <Label variant="accent">{t('repoDocs.configured')}</Label>
      </BoxHeader>
      <BoxRow className={css.block}>
        <dl className={css.kv}>
          <dt>{t('repoDocs.site')}</dt>
          <dd>
            {d.SiteURL ? (
              <a href={d.SiteURL} target="_blank" rel="noreferrer">
                {d.BaseURL}
              </a>
            ) : (
              <code>{d.BaseURL}</code>
            )}
          </dd>
          <dt>{t('repoDocs.doc')}</dt>
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
                <Trans id="repoDocs.collectionNote" params={{ id: d.CollectionID }} tags={settingsTags} />
              </div>
            )}
          </dd>
          <dt>{t('repoDocs.size')}</dt>
          <dd>
            {d.ObservedUnits != null ? (
              <>
                {t('repoDocs.units', { count: d.ObservedUnits })}
                {d.WarningCode === 'doc_near_limit' && (
                  <>
                    {' '}
                    <Label variant="attention">{t('repoDocs.nearLimit')}</Label>
                  </>
                )}
              </>
            ) : (
              <span className={settingsCss.muted}>{t('repoDocs.notMeasured')}</span>
            )}
          </dd>
          <dt>{t('repoDocs.config')}</dt>
          <dd>
            {d.UpdatedBy ? (
              <Trans id="repoDocs.updatedBy" params={{ login: d.UpdatedBy }} tags={withAgo(d.UpdatedAt)} />
            ) : (
              <Trans id="repoDocs.updatedAt" tags={withAgo(d.UpdatedAt)} />
            )}
            {d.CreatedBy && <div className={css.note}>{t('repoDocs.createdByOnly', { login: d.CreatedBy })}</div>}
          </dd>
        </dl>
        <p className={settingsCss.muted}>{t('repoDocs.decisionNote')}</p>
      </BoxRow>
    </Box>
  )
}

function SettingsRepoDocs({ data }: PageProps<Data>) {
  const t = useT()
  useLive(true)
  return (
    <SettingsLayout section={data.Section}>
      <Subhead
        actions={
          data.Binding && (
            <Form action="/settings/repos/docs/verify" aria-label={t('repoDocs.verifyLabel')}>
              <input type="hidden" name="repo" value={data.Repo} />
              <SubmitButton>{t('repoDocs.verify')}</SubmitButton>
            </Form>
          )
        }
        description={t('repoDocs.description')}
      >
        <Crumbs items={[{ label: t('settingsNav.repos'), href: '/settings/repos' }]} current={t('repoDocs.crumb', { repo: data.Repo })} />
      </Subhead>

      {!data.Active && (
        <Callout tone="warning" title={t('repoDocs.inactiveTitle')}>
          {t('repoDocs.inactiveText')}
        </Callout>
      )}

      <BindingBox repo={data.Repo} b={data.Binding} />
      <DecisionBox d={data.Decision} />

      <p className={settingsCss.muted}>{t('repoDocs.snapshotNote')}</p>
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsRepoDocs)
