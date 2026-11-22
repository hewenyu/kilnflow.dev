import { z } from 'zod'
import { list } from '../../lib/envelope'

// 设置页共用的数据（internal/server/web 的 settings.go、settings_scope.go、settings_docker.go、settings_npm.go）。

/**
 * envView：environment（apiv1.Environment，字段带 json tag 且 omitempty，空值时没有这个键）
 * 加上同名覆盖关系和配置页地址。
 */
export const envViewSchema = z.object({
  name: z.string().default(''),
  account: z.string().default(''),
  /** 为空表示账号级 environment。 */
  repo: z.string().default(''),
  ref_policy: z.array(z.string()).default([]),
  allowed_publish_repos: z.array(z.string()).default([]),
  /** 只用于账号级：定义了同名仓库级 environment（整体覆盖这里）的仓库。 */
  OverriddenIn: list(z.string()),
  /** 这个仓库级 environment 整体取代了账号级同名的 environment。 */
  Overrides: z.boolean(),
  URL: z.string(),
  /** Docker Hub 连接使用的账号级 environment。 */
  Docker: z.boolean(),
})
export type EnvView = z.infer<typeof envViewSchema>

/** store.SecretMeta：secret 的元数据，从不包含值。 */
export const secretMetaSchema = z.object({
  Name: z.string(),
  UpdatedBy: z.string(),
  UpdatedAt: z.string(),
  Version: z.number(),
})
export type SecretMeta = z.infer<typeof secretMetaSchema>

export const variableSchema = z.object({ Name: z.string(), Value: z.string() })

/** Docker Hub 连接的状态。 */
export const dockerStatusSchema = z.enum(['unconfigured', 'incomplete', 'saved', 'verified'])
export type DockerStatus = z.infer<typeof dockerStatusSchema>

/** dockerView：一个账号的 Docker Hub 连接。 */
export const dockerViewSchema = z.object({
  Account: z.string(),
  /** 账号级 environment docker-release（还没有时为 null）。 */
  Env: envViewSchema.nullable(),
  Username: z.string(),
  Namespace: z.string(),
  /** 访问令牌的元数据（没有设置时为 null）。 */
  Token: secretMetaSchema.nullable(),
  /** docker-release 中除连接字段外的变量和 secret 数。 */
  Extra: z.number(),
  /** 最近一次用当前这套配置成功推送的镜像。 */
  Verified: z.object({ Image: z.string(), Version: z.string(), At: z.string() }).nullable(),
  Status: dockerStatusSchema,
  /** 缺少的字段（显示名）。 */
  Missing: list(z.string()),
  /** 连接编辑页。 */
  URL: z.string(),
})
export type DockerView = z.infer<typeof dockerViewSchema>

/** npmConfig：账号已保存的 npm 配置，从不包含 token。 */
export const npmConfigSchema = z.object({
  /** 带 @，例如 @acme；release.npm 发布到 @acme/<name>。 */
  Scope: z.string(),
  /** 以 / 结尾。 */
  Registry: z.string(),
  UpdatedBy: z.string(),
  UpdatedAt: z.string(),
})
export type NpmConfig = z.infer<typeof npmConfigSchema>

/** npmView：一个账号的 npm 配置（settings_npm.go）。 */
export const npmViewSchema = z.object({
  Account: z.string(),
  /** 已保存的配置（没有配置时为 null）。 */
  Config: npmConfigSchema.nullable(),
  /** 配置编辑页。 */
  URL: z.string(),
})
export type NpmView = z.infer<typeof npmViewSchema>

/** scopePage：作用域配置页（也是变量、secret 编辑页的上下文）。 */
export const scopePageSchema = z.object({
  Section: z.string(),
  /** 表单中回传的作用域（Repo 非空时 Account 为空）。 */
  Account: z.string(),
  Repo: z.string(),
  Env: z.string(),
  Label: z.string(),
  /** owner、repo、environment */
  Kind: z.string(),
  KindLabel: z.string(),
  Environment: envViewSchema.nullable(),
  Vars: list(variableSchema),
  Secrets: list(secretMetaSchema),
  Docker: z.boolean(),
  DockerURL: z.string(),
  CrumbLabel: z.string(),
  CrumbURL: z.string(),
  /** 配置页地址。 */
  URL: z.string(),
  /** 作用域的查询参数（拼接变量、secret 的新建和编辑页地址）。 */
  Query: z.string(),
})
export type ScopePage = z.infer<typeof scopePageSchema>

export type ItemKind = 'variables' | 'secrets'

/** itemURL 返回作用域中变量或 secret 的新建页（name 为空）或编辑页地址。 */
export function itemURL({ query, kind, name = '' }: { query: string; kind: ItemKind; name?: string }): string {
  if (name === '') return `/settings/${kind}/new?${query}`
  return `/settings/${kind}/edit?${query}&name=${encodeURIComponent(name)}`
}

/** dockerItems 是属于 Docker Hub 连接的变量和 secret 名称。 */
export const dockerItems: readonly string[] = ['DOCKERHUB_USERNAME', 'DOCKERHUB_NAMESPACE', 'DOCKERHUB_TOKEN']
