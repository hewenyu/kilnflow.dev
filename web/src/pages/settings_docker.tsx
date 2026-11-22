import { Link, LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
import { fieldError, formStateSchema, type FormState } from '../lib/envelope'
import {
  Crumbs,
  DangerItem,
  DangerZone,
  DockerMoreRows,
  DockerOverride,
  DockerStatusLabel,
  FormAlert,
  RefPolicy,
  TextField,
  settingsCss,
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
  if (!v.Token) {
    return (
      <TextField
        form={form}
        name="token"
        type="password"
        required
        label="访问令牌"
        caption={
          <>
            Docker Hub 的 Read &amp; Write 访问令牌（PAT）。保存后不会再显示。保存为 secret <code>DOCKERHUB_TOKEN</code>。
          </>
        }
      />
    )
  }
  return (
    <div>
      <span className={css.label}>访问令牌</span>
      <p className={css.tokenSet}>
        <span className={settingsCss.ok}>已设置</span>{' '}
        <span className={settingsCss.muted}>
          · <Ago t={v.Token.UpdatedAt} />
          {v.Token.UpdatedBy && ` 由 ${v.Token.UpdatedBy} 更新`}。令牌保存后不会再显示。
        </span>
      </p>
      <details className={css.replace} open={fieldError(form, 'token') !== undefined}>
        <summary>替换访问令牌</summary>
        <div className={css.replaceBody}>
          <TextField
            form={form}
            name="token"
            type="password"
            label="新的访问令牌"
            caption={
              <>
                留空则继续使用当前令牌。保存为 secret <code>DOCKERHUB_TOKEN</code>。
              </>
            }
          />
        </div>
      </details>
    </div>
  )
}

function Advanced({ v, form, defaultRef }: { v: DockerView; form: FormState; defaultRef: string }) {
  return (
    <details className={settingsCss.details}>
      <summary>高级：发布规则与 workflow 写法</summary>
      <div className={settingsCss.detailsBody}>
        {v.Env ? (
          <>
            <p>
              凭据保存在账号级 environment <code>{v.Env.name}</code> 中。它的发布规则：<RefPolicy env={v.Env} />
              。在这里保存不会改变这些规则；要修改请到 <Link href={v.Env.URL}>environment {v.Env.name}</Link>。
            </p>
            {v.Extra > 0 && (
              <p>
                这个 environment 中还有 {v.Extra} 个其他变量或 secret，同样在 <Link href={v.Env.URL}>environment 页面</Link> 管理。
              </p>
            )}
          </>
        ) : (
          <TextField
            form={form}
            name="ref_policy"
            label={
              <>
                允许发布的 ref（只在首次保存、创建 environment <code>docker-release</code> 时使用）
              </>
            }
            caption={
              <>
                默认 <code>{defaultRef}</code>：只有推送 v 开头的 tag 才能发布。多个用逗号或换行分隔。
              </>
            }
            mono
          />
        )}
        <p>发布 job 中这样引用（账号下的任意仓库）：</p>
        <pre className={settingsCss.pre}>{workflowExample}</pre>
        <p className={settingsCss.muted}>
          如果某个仓库自己定义了同名 environment <code>docker-release</code>，它会整体替换这里的配置（规则、变量和 secret 都不继承）。只有需要单独的凭据或 ref 策略时才这样做。
        </p>
      </div>
    </details>
  )
}

function SettingsDocker({ data }: PageProps<Data>) {
  const v = data.Docker
  const form = data.Form
  return (
    <SettingsLayout section={data.Section}>
      <Subhead
        actions={<DockerStatusLabel status={v.Status} />}
        description={`发布 job 用这里的凭据把镜像推送到 Docker Hub。${v.Account} 下的所有仓库共用这一份。`}
      >
        <Crumbs items={[{ label: '连接与发布凭据', href: '/settings' }]} current={`Docker Hub · ${v.Account}`} />
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
            在 Docker Hub 的 Account settings → Personal access tokens 中创建一个权限为 <strong>Read &amp; Write</strong> 的访问令牌。
          </li>
          <li>填写下面三项并保存。保存只把配置存进 Kiln，不会登录或推送 Docker Hub。</li>
          <li>
            推送 <code>v*</code> tag 后，发布 job 用这些凭据推送镜像；用这套配置推送成功后这里显示“已验证”；以后修改用户名、命名空间或替换令牌，都会回到“未验证”，直到再次推送成功。
          </li>
        </ol>
      )}

      <Form action="/settings/docker" className={settingsCss.form} aria-label="Docker Hub 连接">
        <input type="hidden" name="account" value={v.Account} />
        <FormAlert form={form} />
        <TextField
          form={form}
          name="username"
          required
          label="Docker Hub 用户名"
          caption={
            <>
              访问令牌所属的 Docker ID，与 GitHub 账号无关。保存为变量 <code>DOCKERHUB_USERNAME</code>。
            </>
          }
        />
        <TextField
          form={form}
          name="namespace"
          required
          label="镜像命名空间"
          caption={
            <>
              镜像推送到 <code>docker.io/&lt;命名空间&gt;/…</code>，是 Docker Hub 的用户名或组织名。保存为变量 <code>DOCKERHUB_NAMESPACE</code>。
            </>
          }
        />
        <TokenField v={v} form={form} />
        <Advanced v={v} form={form} defaultRef={data.DefaultRef} />
        <div className={settingsCss.actions}>
          <SubmitButton variant="primary">保存</SubmitButton>
          <LinkButton href="/settings">取消</LinkButton>
        </div>
      </Form>

      {v.Status !== 'unconfigured' && (
        <DangerZone>
          <DangerItem
            title="删除 Docker Hub 凭据"
            action="/settings/docker/delete"
            fields={{ account: v.Account, confirm: '1' }}
            button="删除凭据"
            confirmTitle={`删除 ${v.Account} 的 Docker Hub 凭据？`}
            confirmLabel="删除凭据"
            confirmBody="删除后发布 job 无法推送镜像，直到重新配置。"
          >
            删除用户名、命名空间和访问令牌。environment <code>docker-release</code> 及其发布规则、其他变量保留。删除后发布 job 无法推送镜像，直到重新配置。
          </DangerItem>
        </DangerZone>
      )}
    </SettingsLayout>
  )
}

export default definePage(schema, SettingsDocker)
