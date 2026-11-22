import { AlertIcon, StackIcon } from '@primer/octicons-react'
import { Label, Link, LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Box, BoxRow } from '../components/Box'
import { EmptyState } from '../components/EmptyState'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { list } from '../lib/envelope'
import { envViewSchema, type EnvView } from './settings/schemas'
import css from './settings_envs.module.css'

// 设置 · 环境：environment 列表（GitHub Environments 的样式），标出同名覆盖关系。

const schema = z.object({
  Section: z.string(),
  Envs: list(envViewSchema),
})
type Data = z.infer<typeof schema>

function EnvRow({ e }: { e: EnvView }) {
  return (
    <BoxRow className={css.row}>
      <StackIcon className={css.icon} aria-hidden />
      <div>
        <div className={css.title}>
          <Link href={e.URL}>{e.name}</Link>
          <Label variant="secondary">{e.repo ? '仓库级' : '账号级'}</Label>
          {e.Docker && <Label variant="accent">Docker Hub</Label>}
        </div>
        <p className={css.meta}>
          {e.repo || e.account} · {e.ref_policy.length > 0 ? <code>{e.ref_policy.join(', ')}</code> : '不限制 ref'}
          {e.allowed_publish_repos.length > 0 && ` · 可发布到 ${e.allowed_publish_repos.join(', ')}`}
        </p>
        {e.OverriddenIn.length > 0 && (
          <p className={css.warn}>
            <AlertIcon size={12} aria-hidden />
            <span>在 {e.OverriddenIn.join(', ')} 中被同名仓库级 environment 整体覆盖，这些仓库不会使用这里的变量和 secret</span>
          </p>
        )}
        {e.Overrides && (
          <p className={css.warn}>
            <AlertIcon size={12} aria-hidden />
            <span>整体覆盖账号级同名 environment（不继承其变量和 secret）</span>
          </p>
        )}
      </div>
    </BoxRow>
  )
}

function SettingsEnvs({ data }: PageProps<Data>) {
  return (
    <SettingsLayout section={data.Section}>
      <Subhead
        actions={
          <LinkButton href="/settings/environments/new" variant="primary">
            新建 environment
          </LinkButton>
        }
        description="environment 限制哪些 ref 能使用其中的变量和 secret，并可限制能发布到的仓库。账号级的由账号下所有仓库共用；同名的仓库级 environment 会整体覆盖账号级的。"
      >
        环境
      </Subhead>
      {data.Envs.length > 0 ? (
        <Box>
          {data.Envs.map((e) => (
            <EnvRow key={e.URL} e={e} />
          ))}
        </Box>
      ) : (
        <Box>
          <EmptyState icon={StackIcon} title="没有 environment" action={{ label: '新建 environment', href: '/settings/environments/new' }}>
            job 写 <code>environment: 名称</code> 时使用其中的变量和 secret，并受它的 ref 策略限制。
          </EmptyState>
        </Box>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsEnvs)
