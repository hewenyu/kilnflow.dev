import { z } from 'zod'
import { defaultLocale, type Locale } from '../../i18n/messages'
import { list, textSchema } from '../../lib/envelope'
import { enumLabel } from '../../lib/format'

// 设计依据的页面数据（internal/server/web/docs.go）：只有元数据，正文经单独的接口按需读取。
// 这些数据在每次页面刷新时都会重新取，不包含正文，也不访问外部文档系统。文档的标题、文档集名称和正文是外部文档的原文，
// 原样显示；记录不一致、完整性问题和依据关系的说明是服务端按原因码给的消息（不认识的原因码原样显示领域层的说明）。

/** docMetaSchema 是一篇冻结文档的元数据。URL 已经过服务端校验，为空时不渲染链接。 */
export const docMetaSchema = z.object({
  DocID: z.string(),
  Title: z.string(),
  Revision: z.number(),
  SHA256: z.string(),
  URL: z.string(),
  Purged: z.boolean(),
})
export type DocMeta = z.infer<typeof docMetaSchema>

/** designBasisSchema 是一份冻结依据。SourceKnown 为假表示历史快照没有记录来源，不能用当前绑定解释。 */
export const designBasisSchema = z.object({
  /** work_item / snapshot_set / none。 */
  Source: z.string(),
  Frozen: z.boolean(),
  FrozenAt: z.string().nullable(),
  Provider: z.string(),
  SiteURL: z.string(),
  CollectionID: z.string(),
  CollectionName: z.string(),
  CollectionURL: z.string(),
  SourceKnown: z.boolean(),
  BindingGeneration: z.number(),
  DocCount: z.number(),
  ManifestDigest: z.string(),
  ContentPurgedAt: z.string().nullable(),
  Docs: list(docMetaSchema),
  /** 快照的元数据不一致的原因（服务端给的消息）；没有问题时为空。 */
  IntegrityError: textSchema,
})
export type DesignBasisData = z.infer<typeof designBasisSchema>

/** requestDocsSchema 是 AI 请求固定的依据；Basis 与声明不一致时 Error 说明原因。 */
export const requestDocsSchema = z.object({
  RequestID: z.number(),
  Type: z.string(),
  DeclaredSource: z.string(),
  /** 为假表示迁移前的请求没有记录来源。 */
  Declared: z.boolean(),
  WorkItemID: z.number(),
  WorkItemURL: z.string(),
  SnapshotSetID: z.number(),
  Basis: designBasisSchema.nullable(),
  /** 记录不一致的原因（服务端给的消息）；没有问题时为空。 */
  Error: textSchema,
})
export type RequestDocsView = z.infer<typeof requestDocsSchema>

/** taskDocsSchema 是 AI 任务实际可读的依据；DeclaredSource 是请求记录，BasisSource 是解析出的来源。 */
export const taskDocsSchema = z.object({
  TaskID: z.number(),
  RequestID: z.number(),
  Type: z.string(),
  DeclaredSource: z.string(),
  BasisSource: z.string(),
  RequestURL: z.string(),
  Basis: designBasisSchema.nullable(),
  /** 记录不一致或依据无法解析的原因（服务端给的消息）；没有问题时为空。 */
  Error: textSchema,
})
export type TaskDocsView = z.infer<typeof taskDocsSchema>

/** relatedTaskSchema 是工作项的一条关联任务及它与冻结依据的关系。 */
export const relatedTaskSchema = z.object({
  TaskID: z.number(),
  RequestID: z.number(),
  /** develop / review。 */
  Type: z.string(),
  /** 开发方式（select / implement / fix）。 */
  Mode: z.string(),
  /** 名称见 DesignBasis.tsx 的 relationVariants 与词典 docsRelation.*。 */
  Relation: z.string(),
  /** 关系的说明；NoteDetail 是依据无法解析时的具体原因（都是服务端给的消息，没有时为空）。 */
  Note: textSchema,
  NoteDetail: textSchema,
  TaskURL: z.string(),
  RequestURL: z.string(),
})
export type RelatedTask = z.infer<typeof relatedTaskSchema>

/** sourceText 返回依据来源（work_item、snapshot_set、none、unplanned、legacy）的说明；未知的原样返回（locale 不传时为 zh-CN）。 */
export function sourceText(source: string, locale: Locale = defaultLocale): string {
  return enumLabel(locale, 'docsSource', source)
}
