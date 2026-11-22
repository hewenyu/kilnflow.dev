import { OrganizationIcon, RepoIcon, StackIcon, type Icon } from '@primer/octicons-react'
import { Button, Label, Link, LinkButton, TextInput } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Box, BoxRow } from '../components/Box'
import { Form } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { list } from '../lib/envelope'
import css from './settings_vars.module.css'

// 设置 · 变量与密钥：先选择作用域（账号级、仓库级、environment 级），再进入作用域配置页。

const scopeCountSchema = z.object({
  Name: z.string(),
  URL: z.string(),
  Vars: z.number(),
  Secrets: z.number(),
  /** Docker Hub 连接使用的账号级 environment。 */
  Docker: z.boolean(),
})
type ScopeCount = z.infer<typeof scopeCountSchema>

const schema = z.object({
  Section: z.string(),
  Accounts: list(scopeCountSchema),
  Repos: list(scopeCountSchema),
  Envs: list(scopeCountSchema),
})
type Data = z.infer<typeof schema>

function CountRow({ c, icon: I, what }: { c: ScopeCount; icon: Icon; what: string }) {
  return (
    <BoxRow className={css.row}>
      <span className={css.name}>
        <I />
        <Link href={c.URL}>{c.Name}</Link>
      </span>
      <span className={css.counts}>
        {c.Vars} 个变量 · {c.Secrets} 个 secret
      </span>
      <LinkButton href={c.URL} size="small" aria-label={`管理 ${c.Name} 的${what}变量与 secret`}>
        管理
      </LinkButton>
    </BoxRow>
  )
}

function SettingsVars({ data }: PageProps<Data>) {
  return (
    <SettingsLayout section={data.Section}>
      <Subhead description="先选择作用域，再在其中新建、编辑或删除变量和 secret。同名时 environment 级 > 仓库级 > 账号级。">变量与密钥</Subhead>

      <section aria-labelledby="acc-title">
        <Subhead level={2} id="acc-title" description="账号下的所有仓库都能看到。">
          账号级
        </Subhead>
        <Box>
          {data.Accounts.map((c) => (
            <CountRow key={c.URL} c={c} icon={OrganizationIcon} what="账号级" />
          ))}
          {data.Accounts.length === 0 && <BoxRow className={css.empty}>没有已安装的账号</BoxRow>}
        </Box>
      </section>

      <section aria-labelledby="repo-title">
        <Subhead level={2} id="repo-title" spaced>
          仓库级
        </Subhead>
        <Form action="/settings/repos" method="get" className={css.search} aria-label="查找仓库">
          <TextInput type="search" name="q" aria-label="查找仓库" placeholder="仓库名的一部分" spellCheck={false} autoCapitalize="off" />
          <Button type="submit">搜索</Button>
        </Form>
        <p className={css.hint}>在搜索结果中点“变量与密钥”进入仓库级配置。下面是已经有仓库级变量或 secret 的仓库：</p>
        <Box>
          {data.Repos.map((c) => (
            <CountRow key={c.URL} c={c} icon={RepoIcon} what="仓库级" />
          ))}
          {data.Repos.length === 0 && <BoxRow className={css.empty}>还没有仓库级变量或 secret</BoxRow>}
        </Box>
      </section>

      <section aria-labelledby="env-title">
        <Subhead
          level={2}
          id="env-title"
          spaced
          actions={
            <LinkButton href="/settings/environments/new" size="small">
              新建 environment
            </LinkButton>
          }
          description={
            <>
              只有写了 <code>environment: 名称</code> 的 job 能看到。保护规则在 <Link href="/settings/environments">环境</Link> 中管理。
            </>
          }
        >
          environment 级
        </Subhead>
        <Box>
          {data.Envs.map((c) => (
            <BoxRow key={c.URL} className={css.row}>
              <span className={css.name}>
                <StackIcon />
                <Link href={c.URL}>{c.Name}</Link>
                {c.Docker && <Label variant="accent">Docker Hub</Label>}
              </span>
              {c.Docker && (
                <span className={css.note}>
                  Docker Hub 连接，建议在 <Link href="/settings">连接与发布凭据</Link> 中修改
                </span>
              )}
            </BoxRow>
          ))}
          {data.Envs.length === 0 && <BoxRow className={css.empty}>没有 environment</BoxRow>}
        </Box>
      </section>
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsVars)
