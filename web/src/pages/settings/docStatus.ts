// 仓库外部文档的验证状态：保存配置不等于读取正常。
// 文案与配色只在这里定义，仓库列表和"外部文档"页共用；DESIGN.md 的规则：
// accent 表示已保存但还没有验证，success 表示已验证可以读取（空集合同样说明连接正常），danger 表示失败，
// secondary 只用于中性信息。
export type DocStatusLabel = {
  text: string
  variant: 'accent' | 'success' | 'danger' | 'secondary'
}

const labels = {
  '': { text: '已保存 · 未验证', variant: 'accent' },
  ok: { text: '已验证可读取', variant: 'success' },
  empty: { text: '已验证 · 空集合', variant: 'success' },
  failed: { text: '读取失败', variant: 'danger' },
} as const satisfies Record<string, DocStatusLabel>

/** docStatus 返回验证状态的徽标文案与配色；未知状态按"已保存 · 未验证"处理。 */
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
export const emptyDocCountLabel = { text: '0 篇', variant: 'secondary' } as const satisfies DocStatusLabel
