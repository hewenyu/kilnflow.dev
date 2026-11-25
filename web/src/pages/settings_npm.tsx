import { LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Trans, useT } from '../i18n/locale'
import { formStateSchema } from '../lib/envelope'
import { Crumbs, DangerItem, DangerZone, FormAlert, NpmRows, NpmStatusLabel, TextField, settingsCss, settingsTags } from './settings/common'
import { npmViewSchema } from './settings/schemas'
import css from './settings_npm.module.css'

// 设置 · npm：账号级的 scope、registry 和 token（与 Docker Hub 连接平级）。token 只写不读，每次保存整体替换，要重新填写。

const schema = z.object({
  Section: z.string(),
  Npm: npmViewSchema,
  /** 校验失败时带着 scope、registry 和错误，token 从不回填。 */
  Form: formStateSchema,
  /** registry 留空时使用的 npm 官方 registry。 */
  DefaultRegistry: z.string(),
})
type Data = z.infer<typeof schema>

/** 文案中的占位符（原样显示，不翻译）：npm 的 scope 和包名。 */
const scopeHolder = '<scope>'
const pkgHolder = '<name>'

/** workflowExample 是 release.npm 的写法；comment 是示例中的注释（按当前语言）。 */
function workflowExample(comment: string): string {
  return `  publish:
    needs: [build]
    environment: release
    release:
      npm:
        - from: build.bin
          name: mytool          # ${comment}
          access: public`
}

function SettingsNpm({ data }: PageProps<Data>) {
  const t = useT()
  const v = data.Npm
  const form = data.Form
  const configured = v.Config !== null
  return (
    <SettingsLayout section={data.Section}>
      <Subhead
        actions={<NpmStatusLabel v={v} />}
        description={<Trans id="npm.description" params={{ account: v.Account }} tags={settingsTags} />}
      >
        <Crumbs items={[{ label: t('settingsNav.conn'), href: '/settings' }]} current={`npm · ${v.Account}`} />
      </Subhead>

      {configured ? (
        <dl className={`${settingsCss.kv} ${css.summary}`}>
          <NpmRows v={v} />
        </dl>
      ) : (
        <ol className={css.steps}>
          <li>{t('npm.step1')}</li>
          <li>{t('npm.step2')}</li>
          <li>
            <Trans id="npm.step3" params={{ scope: scopeHolder, pkg: pkgHolder }} tags={settingsTags} />
          </li>
        </ol>
      )}

      <Form action="/settings/npm" className={settingsCss.form} aria-label={t('npm.formLabel')}>
        <input type="hidden" name="account" value={v.Account} />
        <FormAlert form={form} />
        <TextField
          form={form}
          name="scope"
          required
          mono
          label="scope"
          caption={<Trans id="npm.scopeCaption" params={{ pkg: pkgHolder }} tags={settingsTags} />}
        />
        <TextField
          form={form}
          name="registry"
          mono
          label="registry"
          caption={<Trans id="npm.registryCaption" params={{ registry: data.DefaultRegistry }} tags={settingsTags} />}
        />
        <TextField
          form={form}
          name="token"
          type="password"
          required
          label="token"
          caption={configured ? t('npm.tokenCaptionReplace') : t('npm.tokenCaptionNew')}
        />
        <details className={settingsCss.details}>
          <summary>{t('npm.workflowSummary')}</summary>
          <div className={settingsCss.detailsBody}>
            <p>{t('npm.workflowHint')}</p>
            <pre className={settingsCss.pre}>{workflowExample(t('npm.exampleComment', { scope: scopeHolder }))}</pre>
            <p>
              <Trans id="npm.cliHint" params={{ account: v.Account, scope: scopeHolder }} tags={settingsTags} />
            </p>
          </div>
        </details>
        <div className={settingsCss.actions}>
          <SubmitButton variant="primary">{t('settingsForm.save')}</SubmitButton>
          <LinkButton href="/settings">{t('form.cancel')}</LinkButton>
        </div>
      </Form>

      {configured && (
        <DangerZone>
          <DangerItem
            title={t('npm.deleteTitle')}
            action="/settings/npm/delete"
            fields={{ account: v.Account, confirm: '1' }}
            button={t('npm.deleteTitle')}
            confirmTitle={t('npm.deleteConfirmTitle', { account: v.Account })}
            confirmLabel={t('npm.deleteTitle')}
            confirmBody={t('npm.deleteConfirmBody')}
          >
            <Trans id="npm.deleteText" tags={settingsTags} />
          </DangerItem>
        </DangerZone>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsNpm)
