import { SyncIcon } from '@primer/octicons-react'
import { Button, Checkbox, Dialog, FormControl, Radio, RadioGroup } from '@primer/react'
import { useState } from 'react'
import { z } from 'zod'
import { useRefreshLock } from '../../app/live'
import { Callout } from '../../components/Callout'
import { Form, SubmitButton } from '../../components/Form'
import { rerunLabel, runState } from '../../lib/format'
import css from './RerunDialog.module.css'

/** 重跑的说明（服务端 rerunForm，按最新一次运行的 job 计算）。 */
export const rerunFormSchema = z.object({
  /** 默认选中的方式：有未成功的 job 时为 failed，否则为 all。 */
  Default: z.string(),
  Total: z.number(),
  /** 按"重跑未成功的 job"会重新规划的 job 数；Kept 是沿用结果的 job 数。 */
  Retry: z.number(),
  Kept: z.number(),
  /** 发布 job 和有副作用（effects: true）的 job 数。 */
  Publish: z.number(),
  Effects: z.number(),
  /** 最新一次运行的产物已经按保留策略清理。 */
  Purged: z.boolean(),
})
export type RerunForm = z.infer<typeof rerunFormSchema>

/** modeHints 返回三种重跑方式各自的说明（与服务端的重跑规划规则一致）。 */
export function modeHints(f: RerunForm): { failed: string; all: string; noCache: string } {
  let failed: string
  if (f.Retry > 0) {
    failed = `${f.Retry} 个未成功（失败、取消、跳过）的 job 重新规划，输入没变的仍会命中缓存；`
    failed += f.Kept > 0 ? `${f.Kept} 个${f.Purged ? '已成功的发布' : '成功的'} job 沿用上一次的结果，不重新执行` : '没有沿用结果的 job'
    if (f.Publish > 0 && !f.Purged) failed += '，已成功的发布不会再次执行'
    failed += '。'
  } else {
    failed = '最新一次运行中所有 job 都成功了，没有需要重跑的 job。'
  }
  let all = `全部 ${f.Total} 个 job 重新规划；输入没变的普通 job 直接命中缓存，不真正执行。`
  if (f.Publish > 0 || f.Effects > 0) {
    const who = [f.Publish > 0 ? `${f.Publish} 个发布 job` : '', f.Effects > 0 ? `${f.Effects} 个有副作用（effects）的 job` : ''].filter(Boolean).join(' 和 ')
    all += `${who} 不使用缓存，会再次执行${f.Publish > 0 ? '（发布按 tag 幂等：已存在且内容一致的 asset 会跳过）' : ''}。`
  }
  const noCache = `全部 ${f.Total} 个 job 强制重新执行，不读取缓存，用于怀疑缓存结果有问题时；耗时最长。${f.Publish > 0 ? '发布 job 会再次执行。' : ''}`
  return { failed, all, noCache }
}

/**
 * RerunButton 是 run 详情页右上角的"重跑"按钮：打开对话框选择重跑方式（只重跑未成功的 job、全部重跑、全部重跑不用缓存）
 * 和是否失败时保留工作区，提交到 POST /runs/{id}/rerun。
 */
export function RerunButton({ runID, form, current, state }: { runID: number; form: RerunForm; current: number; state: string }) {
  const [open, setOpen] = useState(false)
  useRefreshLock(open)
  const hints = modeHints(form)
  return (
    <>
      <Button leadingVisual={SyncIcon} onClick={() => setOpen(true)}>
        重跑
      </Button>
      {open && (
        <Dialog title="重跑这个 run" onClose={() => setOpen(false)} width="large">
          <Form action={`/runs/${runID}/rerun`} className={css.form} aria-label="重跑">
            <p className={css.intro}>
              在同一个 commit 上新建一次运行，基于最新一次（{rerunLabel(current)}，{runState(state)}）的结果。workflow 文件取自这个 commit，变量、secret 和
              environment 用当前的配置。已有的运行记录保留，可以在页面右上角切换查看。
            </p>
            {form.Purged && (
              <Callout title="产物已清理" hideTitle>
                最新一次运行的产物已经按保留策略清理，成功的 job 无法沿用结果，也会重新规划（输入没变时仍可能命中缓存）；只有已成功的发布 job 沿用，不会再次发布。
              </Callout>
            )}
            <RadioGroup name="mode">
              <RadioGroup.Label>重跑哪些 job</RadioGroup.Label>
              <FormControl disabled={form.Retry === 0}>
                <Radio value="failed" defaultChecked={form.Default === 'failed'} />
                <FormControl.Label>只重跑未成功的 job</FormControl.Label>
                <FormControl.Caption>{hints.failed}</FormControl.Caption>
              </FormControl>
              <FormControl>
                <Radio value="all" defaultChecked={form.Default === 'all'} />
                <FormControl.Label>全部重跑（可命中缓存）</FormControl.Label>
                <FormControl.Caption>{hints.all}</FormControl.Caption>
              </FormControl>
              <FormControl>
                <Radio value="all_no_cache" />
                <FormControl.Label>全部重跑（不用缓存）</FormControl.Label>
                <FormControl.Caption>{hints.noCache}</FormControl.Caption>
              </FormControl>
            </RadioGroup>
            <FormControl>
              <Checkbox name="keep" />
              <FormControl.Label>失败时保留工作区</FormControl.Label>
              <FormControl.Caption>job 失败后在 Runner 上保留现场，便于登录排查。</FormControl.Caption>
            </FormControl>
            <div className={css.actions}>
              <Button onClick={() => setOpen(false)}>取消</Button>
              <SubmitButton variant="primary" leadingVisual={SyncIcon}>
                重跑
              </SubmitButton>
            </div>
          </Form>
        </Dialog>
      )}
    </>
  )
}
