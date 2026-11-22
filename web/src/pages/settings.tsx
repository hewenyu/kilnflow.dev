import { ContainerIcon, OrganizationIcon, PackageIcon } from '@primer/octicons-react'
import { Link, LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Box, BoxBody, BoxHeader } from '../components/Box'
import { Callout } from '../components/Callout'
import { EmptyState } from '../components/EmptyState'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { list } from '../lib/envelope'
import { DockerOverride, DockerStatusLabel, DockerSummary, NpmStatusLabel, NpmSummary } from './settings/common'
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
          <Callout tone="warning" title="连接不完整" hideTitle>
            还缺少：{v.Missing.join('、')}。
          </Callout>
        )}
        <DockerOverride v={v} />
        <div>
          <LinkButton href={v.URL} variant={v.Status === 'unconfigured' ? 'primary' : 'default'}>
            {v.Status === 'unconfigured' ? '配置 Docker Hub' : '编辑连接'}
          </LinkButton>
        </div>
      </BoxBody>
    </Box>
  )
}

function NpmCard({ v }: { v: NpmView }) {
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
            {v.Config ? '编辑配置' : '配置 npm'}
          </LinkButton>
        </div>
      </BoxBody>
    </Box>
  )
}

function Settings({ data }: PageProps<Data>) {
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description="发布镜像和 npm 包需要的外部账号。每个 GitHub 账号配置一次，账号下的所有仓库共用。">连接与发布凭据</Subhead>
      {data.Docker.length > 0 ? (
        <>
          <section aria-labelledby="docker-title">
            <Subhead
              level={2}
              id="docker-title"
              description={
                <>
                  workflow 中 <code>release.images</code> 推送镜像到 Docker Hub 时使用。
                </>
              }
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
              description={
                <>
                  workflow 中 <code>release.npm</code> 的包用这里的 scope、registry 和 token 发布，workflow 中不写凭据。
                </>
              }
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
          <EmptyState icon={OrganizationIcon} title="还没有已安装 App 的账号">
            确认 App 已安装、账号在 KILN_ALLOWED_ACCOUNTS 中，然后到 <Link href="/settings/repos">仓库</Link> 同步。
          </EmptyState>
        </Box>
      )}
      <Subhead level={2} spaced>
        其他发布目标
      </Subhead>
      <p className={css.other}>
        GitHub Release 使用 GitHub App 的权限发布，不需要在这里配置。其他变量和 secret（例如部署密钥）在{' '}
        <Link href="/settings/variables">变量与密钥</Link> 中按账号、仓库或 environment 配置。
      </p>
    </SettingsLayout>
  )
}

export default definePage(schema, Settings)
