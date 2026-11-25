import { LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
import { Trans, useT } from '../i18n/locale'
import { fieldError, formStateSchema, type FormState } from '../lib/envelope'
import {
  Crumbs,
  dockerFieldLabels,
  DangerItem,
  DangerZone,
  DockerMoreRows,
  DockerOverride,
  DockerStatusLabel,
  FormAlert,
  RefPolicy,
  TextField,
  settingsCss,
  settingsTags,
  withLink,
} from './settings/common'
import { dockerViewSchema, type DockerView } from './settings/schemas'
import css from './settings_docker.module.css'

// 设置 · Docker Hub 连接：用户名、命名空间和访问令牌（令牌只写不读，替换是显式动作）。

const schema = z.object({
  Section: z.string(),
  Docker: dockerViewSchema,
  Form: formStateSchema,
  /** 新建 docker-release 时默认的 ref 策略。 */
  DefaultRef: z.string(),
})
type Data = z.infer<typeof schema>

const workflowExample = `  publish:
    needs: [image]
    environment: docker-release
    release:
      images:
        - from: image.image
          name: docker.io/\${{ vars.DOCKERHUB_NAMESPACE }}/myapp
          tags: ["\${{ github.ref_name }}"]
          username: \${{ vars.DOCKERHUB_USERNAME }}
          password: \${{ secrets.DOCKERHUB_TOKEN }}`

/** TokenField 是访问令牌：已设置时只显示状态，替换要展开；没有时必填。输入的令牌从不回填。 */
function TokenField({ v, form }: { v: DockerView; form: FormState }) {
  const t = useT()
  if (!v.Token) {
    return (
      <TextField
        form={form}
        name="token"
        type="password"
        required
        label={t(dockerFieldLabels.token)}
        caption={<Trans id="docker.tokenCaptionNew" tags={settingsTags} />}
      />
    )
  }
  return (
    <div>
      <span className={css.label}>{t(dockerFieldLabels.token)}</span>
      <p className={css.tokenSet}>
        <span className={settingsCss.ok}>{t('settingsCommon.set')}</span>{' '}
        <span className={settingsCss.muted}>
          · <Ago t={v.Token.UpdatedAt} />
          {v.Token.UpdatedBy && t('settingsCommon.updatedBy', { login: v.Token.UpdatedBy })}
          {t('docker.tokenHidden')}
        </span>
      </p>
      <details className={css.replace} open={fieldError(form, 'token') !== undefined}>
        <summary>{t('docker.replaceToken')}</summary>
        <div className={css.replaceBody}>
          <TextField
            form={form}
            name="token"
            type="password"
            label={t('docker.newToken')}
            caption={<Trans id="docker.newTokenCaption" tags={settingsTags} />}
          />
        </div>
      </details>
    </div>
  )
}

function Advanced({ v, form, defaultRef }: { v: DockerView; form: FormState; defaultRef: string }) {
  const t = useT()
  return (
    <details className={settingsCss.details}>
      <summary>{t('docker.advanced')}</summary>
      <div className={settingsCss.detailsBody}>
        {v.Env ? (
          <>
            <p>
              <Trans id="docker.envNoteRule" params={{ env: v.Env.name }} tags={settingsTags} />
              <RefPolicy env={v.Env} />
              <Trans id="docker.envNoteEdit" params={{ env: v.Env.name }} tags={withLink(v.Env.URL)} />
            </p>
            {v.Extra > 0 && (
              <p>
                <Trans id="docker.extra" params={{ count: v.Extra }} tags={withLink(v.Env.URL)} />
              </p>
            )}
          </>
        ) : (
          <TextField
            form={form}
            name="ref_policy"
            label={<Trans id="docker.refLabel" tags={settingsTags} />}
            caption={<Trans id="docker.refCaption" params={{ ref: defaultRef }} tags={settingsTags} />}
            mono
          />
        )}
        <p>{t('docker.workflowHint')}</p>
        <pre className={settingsCss.pre}>{workflowExample}</pre>
        <p className={settingsCss.muted}>
          <Trans id="docker.repoOverrideNote" tags={settingsTags} />
        </p>
      </div>
    </details>
  )
}

function SettingsDocker({ data }: PageProps<Data>) {
  const t = useT()
  const v = data.Docker
  const form = data.Form
  return (
    <SettingsLayout section={data.Section}>
      <Subhead actions={<DockerStatusLabel status={v.Status} />} description={t('docker.description', { account: v.Account })}>
        <Crumbs items={[{ label: t('settingsNav.conn'), href: '/settings' }]} current={`Docker Hub · ${v.Account}`} />
      </Subhead>

      {v.Status !== 'unconfigured' ? (
        <div className={css.summary}>
          <dl className={settingsCss.kv}>
            <DockerMoreRows v={v} />
          </dl>
          <DockerOverride v={v} />
        </div>
      ) : (
        <ol className={css.steps}>
          <li>
            <Trans id="docker.step1" tags={settingsTags} />
          </li>
          <li>{t('docker.step2')}</li>
          <li>
            <Trans id="docker.step3" tags={settingsTags} />
          </li>
        </ol>
      )}

      <Form action="/settings/docker" className={settingsCss.form} aria-label={t('docker.formLabel')}>
        <input type="hidden" name="account" value={v.Account} />
        <FormAlert form={form} />
        <TextField
          form={form}
          name="username"
          required
          label={t(dockerFieldLabels.username)}
          caption={<Trans id="docker.usernameCaption" tags={settingsTags} />}
        />
        <TextField
          form={form}
          name="namespace"
          required
          label={t(dockerFieldLabels.namespace)}
          caption={<Trans id="docker.namespaceCaption" params={{ ns: `<${t('docker.nsPlaceholder')}>` }} tags={settingsTags} />}
        />
        <TokenField v={v} form={form} />
        <Advanced v={v} form={form} defaultRef={data.DefaultRef} />
        <div className={settingsCss.actions}>
          <SubmitButton variant="primary">{t('settingsForm.save')}</SubmitButton>
          <LinkButton href="/settings">{t('form.cancel')}</LinkButton>
        </div>
      </Form>

      {v.Status !== 'unconfigured' && (
        <DangerZone>
          <DangerItem
            title={t('docker.deleteTitle')}
            action="/settings/docker/delete"
            fields={{ account: v.Account, confirm: '1' }}
            button={t('docker.deleteButton')}
            confirmTitle={t('docker.deleteConfirmTitle', { account: v.Account })}
            confirmLabel={t('docker.deleteButton')}
            confirmBody={t('docker.deleteConfirmBody')}
          >
            <Trans id="docker.deleteText" tags={settingsTags} />
          </DangerItem>
        </DangerZone>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsDocker)
