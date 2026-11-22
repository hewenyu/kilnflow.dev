import type { Icon } from '@primer/octicons-react'
import { Button, ConfirmationDialog, IconButton, type ButtonProps } from '@primer/react'
import { createContext, useContext, useEffect, useRef, useState, type SyntheticEvent, type ReactNode } from 'react'
import { useRefreshLock } from '../app/live'
import { useRouter } from '../app/router'

// 表单：仍按 application/x-www-form-urlencoded 提交给服务端（与没有前端时相同的地址和字段），由前端发请求并按结果跳转。

interface FormCtx {
  busy: boolean
}

const Ctx = createContext<FormCtx>({ busy: false })

/** formBody 把表单的字段（含点击的提交按钮的 name=value）转换为 URLSearchParams。 */
export function formBody(form: HTMLFormElement, submitter: HTMLElement | null): URLSearchParams {
  const fd = new FormData(form, submitter)
  const body = new URLSearchParams()
  for (const [k, v] of fd) {
    if (typeof v === 'string') body.append(k, v)
  }
  return body
}

/** focusInvalid 把焦点移到第一个校验失败的字段（服务端返回表单错误后）。 */
function focusInvalid() {
  window.requestAnimationFrame(() => {
    const el = document.querySelector<HTMLElement>('main [aria-invalid="true"]')
    el?.focus()
  })
}

/**
 * Form 是提交给服务端的表单。method 为 get 时（筛选等）跳转到 action?字段；为 post 时提交，
 * 提交期间禁用提交按钮（SubmitButton 显示加载中），防止重复提交。
 */
export function Form({
  action,
  method = 'post',
  children,
  className,
  'aria-label': ariaLabel,
  'aria-labelledby': ariaLabelledBy,
}: {
  action: string
  method?: 'get' | 'post'
  children: ReactNode
  className?: string
  'aria-label'?: string
  'aria-labelledby'?: string
}) {
  const { navigate, submit } = useRouter()
  const [busy, setBusy] = useState(false)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  useRefreshLock(busy)

  const send = async (body: URLSearchParams) => {
    if (method === 'get') {
      const q = body.toString()
      await navigate(action + (q ? (action.includes('?') ? '&' : '?') + q : ''))
      return
    }
    setBusy(true)
    try {
      await submit({ action, body })
      focusInvalid()
    } finally {
      if (mounted.current) setBusy(false)
    }
  }

  const onSubmit = (e: SyntheticEvent<HTMLFormElement, SubmitEvent>) => {
    e.preventDefault()
    if (busy) return
    const submitter = e.nativeEvent.submitter
    void send(formBody(e.currentTarget, submitter))
  }

  return (
    <Ctx.Provider value={{ busy }}>
      <form action={action} method={method} onSubmit={onSubmit} className={className} aria-label={ariaLabel} aria-labelledby={ariaLabelledBy} noValidate={false}>
        {children}
      </form>
    </Ctx.Provider>
  )
}

/** useFormBusy 表示所在的表单正在提交。 */
export function useFormBusy(): boolean {
  return useContext(Ctx).busy
}

/** SubmitButton 是表单的提交按钮：提交期间显示加载中并禁用。 */
export function SubmitButton({ children, ...rest }: Omit<ButtonProps, 'type'>) {
  const busy = useFormBusy()
  return (
    <Button type="submit" loading={busy} {...rest}>
      {children}
    </Button>
  )
}

/**
 * ConfirmForm 是需要确认的操作（取消 run、吊销 Runner、删除等）：点击按钮先弹出确认对话框，确认后才提交。
 * fields 是要提交的隐藏字段。icon 不为空时按钮是图标按钮（列表行尾的删除等），button 是它的说明文字（读屏和悬停提示）。
 */
export function ConfirmForm({
  action,
  fields,
  button,
  buttonProps,
  title,
  children,
  confirmLabel,
  danger = true,
  icon,
  cancelLabel = '取消',
}: {
  action: string
  fields?: Record<string, string>
  button: ReactNode
  icon?: Icon
  buttonProps?: Omit<ButtonProps, 'type' | 'onClick'>
  title: string
  children?: ReactNode
  confirmLabel: string
  danger?: boolean
  /** 对话框中不执行操作的按钮；操作本身是"取消…"时用"返回"之类，避免两个"取消"。 */
  cancelLabel?: string
}) {
  const { submit } = useRouter()
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  useRefreshLock(open || busy)

  const onClose = (gesture: 'confirm' | 'close-button' | 'cancel' | 'escape') => {
    // 提交期间不关闭、也不重复提交（服务端已经处理了这次操作，界面等它完成）
    if (busy) return
    if (gesture !== 'confirm') {
      setOpen(false)
      return
    }
    setBusy(true)
    const body = new URLSearchParams(fields)
    void submit({ action, body }).finally(() => {
      setBusy(false)
      setOpen(false)
    })
  }

  return (
    <>
      {icon ? (
        <IconButton
          icon={icon}
          aria-label={typeof button === 'string' ? button : title}
          variant="invisible"
          size={buttonProps?.size ?? 'small'}
          className={buttonProps?.className}
          disabled={busy}
          onClick={() => setOpen(true)}
        />
      ) : (
        <Button {...buttonProps} variant={buttonProps?.variant ?? (danger ? 'danger' : 'default')} loading={busy} onClick={() => setOpen(true)}>
          {button}
        </Button>
      )}
      {open && (
        <ConfirmationDialog
          title={title}
          onClose={onClose}
          confirmButtonContent={confirmLabel}
          cancelButtonContent={cancelLabel}
          confirmButtonType={danger ? 'danger' : 'primary'}
          confirmButtonLoading={busy}
        >
          {children}
        </ConfirmationDialog>
      )}
    </>
  )
}
