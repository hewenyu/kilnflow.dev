import type { HTMLAttributes, ReactNode } from 'react'
import css from './Box.module.css'

const cx = (...c: (string | false | undefined)[]) => c.filter(Boolean).join(' ')

/** Box 是带边框的容器（GitHub 的 Box），里面放 BoxHeader、BoxRow、BoxBody、BoxFooter。 */
export function Box({
  danger,
  className,
  as: Tag = 'div',
  ...rest
}: HTMLAttributes<HTMLElement> & { danger?: boolean; as?: 'div' | 'section' | 'ul' }) {
  return <Tag className={cx(css.box, danger && css.danger, className)} {...rest} />
}

/** BoxHeader 是标题行：title 是标题，children 放在右侧（按钮、筛选等）。 */
export function BoxHeader({
  title,
  level = 2,
  id,
  children,
}: {
  title: ReactNode
  level?: 2 | 3
  id?: string
  children?: ReactNode
}) {
  const H = level === 2 ? 'h2' : 'h3'
  return (
    <div className={css.header}>
      <H className={css.title} id={id}>
        {title}
      </H>
      {children}
    </div>
  )
}

/** BoxRow 是一行；hoverable 时鼠标悬停有底色（整行可点的列表）。 */
export function BoxRow({
  hoverable,
  className,
  as: Tag = 'div',
  ...rest
}: HTMLAttributes<HTMLElement> & { hoverable?: boolean; as?: 'div' | 'li' }) {
  return <Tag className={cx(css.row, hoverable && css.hoverable, className)} {...rest} />
}

export function BoxBody({ className, ...rest }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cx(css.body, className)} {...rest} />
}

export function BoxFooter({ className, ...rest }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cx(css.footer, className)} {...rest} />
}
