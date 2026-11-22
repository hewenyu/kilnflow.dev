import type { Icon } from '@primer/octicons-react'
import { Blankslate } from '@primer/react/experimental'
import type { ReactNode } from 'react'

/**
 * EmptyState 是列表为空时的说明（GitHub 的 Blankslate）：图标、标题（不加句号）、一句说明和下一步。
 * 页头已经有同样的主要操作时不要再放 action；设置页中一组列表里的某一小类为空时用一行灰字（BoxRow），不用 EmptyState。
 */
export function EmptyState({
  icon: I,
  title,
  children,
  action,
  secondary,
  narrow = true,
  size = 'medium',
}: {
  icon?: Icon
  title: string
  children?: ReactNode
  action?: { label: string; href: string }
  /** 次要的下一步（链接样式），例如"清空筛选"。 */
  secondary?: { label: string; href: string }
  narrow?: boolean
  /** small 用于页面中并列的几个分区（总览），medium 用于整页的列表。 */
  size?: 'small' | 'medium'
}) {
  return (
    <Blankslate narrow={narrow} size={size}>
      {I && (
        <Blankslate.Visual>
          <I size={24} />
        </Blankslate.Visual>
      )}
      <Blankslate.Heading as="h3">{title}</Blankslate.Heading>
      {children && <Blankslate.Description>{children}</Blankslate.Description>}
      {action && <Blankslate.PrimaryAction href={action.href}>{action.label}</Blankslate.PrimaryAction>}
      {secondary && <Blankslate.SecondaryAction href={secondary.href}>{secondary.label}</Blankslate.SecondaryAction>}
    </Blankslate>
  )
}
