import { CheckIcon, CopyIcon } from '@primer/octicons-react'
import { IconButton } from '@primer/react'
import { useEffect, useState } from 'react'

/** CopyButton 把 value 复制到剪贴板；复制后短暂显示对勾，并告诉读屏软件"已复制"。 */
export function CopyButton({ value, label = '复制' }: { value: string; label?: string }) {
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
  const text = state === 'done' ? '已复制' : state === 'failed' ? '复制失败，请手动复制' : label
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
