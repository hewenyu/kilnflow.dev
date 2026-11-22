import { CheckIcon, CopyIcon } from '@primer/octicons-react'
import { IconButton } from '@primer/react'
import { useEffect, useState } from 'react'
import { useT } from '../i18n/locale'

/** CopyButton 把 value 复制到剪贴板；复制后短暂显示对勾，并告诉读屏软件"已复制"。label 默认是"复制"。 */
export function CopyButton({ value, label }: { value: string; label?: string }) {
  const t = useT()
  const [state, setState] = useState<'' | 'done' | 'failed'>('')
  useEffect(() => {
    if (state === '') return
    const t = window.setTimeout(() => setState(''), 2500)
    return () => window.clearTimeout(t)
  }, [state])
  const copy = () => {
    // 不是安全上下文（例如 http://）时没有 navigator.clipboard，同步抛出的错误也当作复制失败
    Promise.resolve()
      .then(() => navigator.clipboard.writeText(value))
      .then(
        () => setState('done'),
        () => setState('failed'),
      )
  }
  const text = state === 'done' ? t('copy.done') : state === 'failed' ? t('copy.failed') : (label ?? t('copy.label'))
  return (
    <>
      <IconButton
        icon={state === 'done' ? CheckIcon : CopyIcon}
        aria-label={text}
        size="small"
        variant="invisible"
        onClick={copy}
      />
      <span className="visually-hidden" role="status" aria-live="polite">
        {state === '' ? '' : text}
      </span>
    </>
  )
}
