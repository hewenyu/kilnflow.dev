import type { MsgKey } from '../../i18n/messages'

// 仓库外部文档的验证状态：保存配置不等于读取正常。
// 文案键与配色只在这里定义，仓库列表和"外部文档"页共用；DESIGN.md 的规则：
// accent 表示已保存但还没有验证，success 表示已验证可以读取（空集合同样说明连接正常），danger 表示失败，
// secondary 只用于中性信息。
export type DocStatusLabel = {
  text: MsgKey
  variant: 'accent' | 'success' | 'danger' | 'secondary'
}

const labels = {
  '': { text: 'docStatus.unverified', variant: 'accent' },
  ok: { text: 'docStatus.ok', variant: 'success' },
  empty: { text: 'docStatus.empty', variant: 'success' },
  failed: { text: 'docStatus.failed', variant: 'danger' },
} as const satisfies Record<string, DocStatusLabel>

/** docStatus 返回验证状态的徽标文案键与配色；未知状态按"已保存 · 未验证"处理。 */
export function docStatus(status: string): DocStatusLabel {
  switch (status) {
    case 'ok':
      return labels.ok
    case 'empty':
      return labels.empty
    case 'failed':
      return labels.failed
    default:
      return labels['']
  }
}

/** emptyDocCountLabel 是空集合旁边的中性计数：成功色表示连接正常，"0 篇"本身不表示失败。 */
export const emptyDocCountLabel = { text: 'docStatus.emptyCount', variant: 'secondary' } as const satisfies DocStatusLabel
