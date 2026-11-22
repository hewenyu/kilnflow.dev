import { Banner } from '@primer/react'
import type { ReactNode } from 'react'
import css from './Callout.module.css'

/**
 * Callout 是页面内的提示框（Primer Banner 的紧凑样式）。title 总是要有（读屏软件先读标题）；
 * hideTitle 时只显示 children。
 */
export function Callout({
  tone = 'info',
  title,
  hideTitle,
  children,
  action,
  onDismiss,
  className,
}: {
  tone?: 'info' | 'warning' | 'critical' | 'success'
  title: string
  hideTitle?: boolean
  children?: ReactNode
  action?: ReactNode
  onDismiss?: () => void
  className?: string
}) {
  return (
    <Banner
      variant={tone}
      title={title}
      hideTitle={hideTitle}
      description={children}
      layout="compact"
      primaryAction={action}
      onDismiss={onDismiss}
      className={[css.callout, className].filter(Boolean).join(' ')}
    />
  )
}
