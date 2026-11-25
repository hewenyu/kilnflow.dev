import { ContainerIcon, OrganizationIcon, PackageIcon } from '@primer/octicons-react'
import { LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Box, BoxBody, BoxHeader } from '../components/Box'
import { Callout } from '../components/Callout'
import { EmptyState } from '../components/EmptyState'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Trans, useT } from '../i18n/locale'
import { list } from '../lib/envelope'
import {
  DockerOverride,
  DockerStatusLabel,
  DockerSummary,
  dockerFieldLabels,
  NpmStatusLabel,
  NpmSummary,
  settingsTags,
  withLink,
} from './settings/common'
import { dockerViewSchema, npmViewSchema, type DockerView, type NpmView } from './settings/schemas'
import css from './settings.module.css'

// 设置 · 连接与发布凭据：Docker Hub 和 npm 两类发布凭据平级，每类每个账号一张卡片。

const schema = z.object({
  Section: z.string(),
  Docker: list(dockerViewSchema),
  Npm: list(npmViewSchema),
})
type Data = z.infer<typeof schema>

function DockerCard({ v }: { v: DockerView }) {
  const t = useT()
  const titleID = `docker-${v.Account}-title`
  return (
    <Box as="section" aria-labelledby={titleID}>
      <BoxHeader
        id={titleID}
        title={
          <span className={css.cardTitle}>
            <ContainerIcon aria-hidden />
            Docker Hub <span className={css.account}>· {v.Account}</span>
          </span>
        }
      >
        <DockerStatusLabel status={v.Status} />
      </BoxHeader>
      <BoxBody className={css.body}>
        <DockerSummary v={v} />
        {v.Status === 'incomplete' && (
          <Callout tone="warning" title={t('settingsConn.incompleteTitle')} hideTitle>
            {t('settingsConn.missing', { fields: v.Missing.map((f) => t(dockerFieldLabels[f])).join(t('list.enum')) })}
          </Callout>
        )}
        <DockerOverride v={v} />
        <div>
          <LinkButton href={v.URL} variant={v.Status === 'unconfigured' ? 'primary' : 'default'}>
            {v.Status === 'unconfigured' ? t('settingsConn.configureDocker') : t('settingsConn.editConnection')}
          </LinkButton>
        </div>
      </BoxBody>
    </Box>
  )
}

function NpmCard({ v }: { v: NpmView }) {
  const t = useT()
  const titleID = `npm-${v.Account}-title`
  return (
    <Box as="section" aria-labelledby={titleID}>
      <BoxHeader
        id={titleID}
        title={
          <span className={css.cardTitle}>
            <PackageIcon aria-hidden />
            npm <span className={css.account}>· {v.Account}</span>
          </span>
        }
      >
        <NpmStatusLabel v={v} />
      </BoxHeader>
      <BoxBody className={css.body}>
        <NpmSummary v={v} />
        <div>
          <LinkButton href={v.URL} variant={v.Config ? 'default' : 'primary'}>
            {v.Config ? t('settingsConn.editNpm') : t('settingsConn.configureNpm')}
          </LinkButton>
        </div>
      </BoxBody>
    </Box>
  )
}

function Settings({ data }: PageProps<Data>) {
  const t = useT()
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description={t('settingsConn.description')}>{t('settingsNav.conn')}</Subhead>
      {data.Docker.length > 0 ? (
        <>
          <section aria-labelledby="docker-title">
            <Subhead
              level={2}
              id="docker-title"
              description={<Trans id="settingsConn.dockerDesc" tags={settingsTags} />}
            >
              Docker Hub
            </Subhead>
            <div className={css.cards}>
              {data.Docker.map((v) => (
                <DockerCard key={v.Account} v={v} />
              ))}
            </div>
          </section>
          <section aria-labelledby="npm-title">
            <Subhead
              level={2}
              id="npm-title"
              spaced
              description={<Trans id="settingsConn.npmDesc" tags={settingsTags} />}
            >
              npm
            </Subhead>
            <div className={css.cards}>
              {data.Npm.map((v) => (
                <NpmCard key={v.Account} v={v} />
              ))}
            </div>
          </section>
        </>
      ) : (
        <Box>
          <EmptyState icon={OrganizationIcon} title={t('settingsConn.emptyTitle')}>
            <Trans id="settingsConn.emptyText" tags={withLink('/settings/repos')} />
          </EmptyState>
        </Box>
      )}
      <Subhead level={2} spaced>
        {t('settingsConn.otherTitle')}
      </Subhead>
      <p className={css.other}>
        <Trans id="settingsConn.otherText" tags={withLink('/settings/variables')} />
      </p>
    </SettingsLayout>
  )
}

export default definePage(schema, Settings)
