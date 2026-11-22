import { SyncIcon } from '@primer/octicons-react'
import { Button, Checkbox, Dialog, FormControl, Radio, RadioGroup } from '@primer/react'
import { useState } from 'react'
import { z } from 'zod'
import { useRefreshLock } from '../../app/live'
import { Callout } from '../../components/Callout'
import { Form, SubmitButton } from '../../components/Form'
import { useLocale, useT } from '../../i18n/locale'
import { defaultLocale, msg, translate, type Locale, type MsgKey, type Params } from '../../i18n/messages'
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

/** modeHints 返回三种重跑方式各自的说明（与服务端的重跑规划规则一致）；locale 不传时为 zh-CN。 */
export function modeHints(f: RerunForm, locale: Locale = defaultLocale): { failed: string; all: string; noCache: string } {
  const t = (id: MsgKey, params?: Params) => translate(locale, msg(id, params))
  let failed: string
  if (f.Retry > 0) {
    failed = t('rerun.hintRetry', { count: f.Retry })
    failed += f.Kept > 0 ? t(f.Purged ? 'rerun.hintKeptPublish' : 'rerun.hintKept', { count: f.Kept }) : t('rerun.hintNoKept')
    if (f.Publish > 0 && !f.Purged) failed += t('rerun.hintPublishSkip')
    failed += t('rerun.hintEnd')
  } else {
    failed = t('rerun.hintAllSucceeded')
  }
  let all = t('rerun.hintAll', { count: f.Total })
  if (f.Publish > 0 || f.Effects > 0) {
    const who = [f.Publish > 0 ? t('rerun.whoPublish', { count: f.Publish }) : '', f.Effects > 0 ? t('rerun.whoEffects', { count: f.Effects }) : '']
      .filter(Boolean)
      .join(t('rerun.and'))
    all += t(f.Publish > 0 ? 'rerun.hintWhoPublish' : 'rerun.hintWho', { who })
  }
  const noCache = t('rerun.hintNoCache', { count: f.Total }) + (f.Publish > 0 ? t('rerun.hintNoCachePublish') : '')
  return { failed, all, noCache }
}

/**
 * RerunButton 是 run 详情页右上角的"重跑"按钮：打开对话框选择重跑方式（只重跑未成功的 job、全部重跑、全部重跑不用缓存）
 * 和是否失败时保留工作区，提交到 POST /runs/{id}/rerun。
 */
export function RerunButton({ runID, form, current, state }: { runID: number; form: RerunForm; current: number; state: string }) {
  const [open, setOpen] = useState(false)
  useRefreshLock(open)
  const t = useT()
  const { locale } = useLocale()
  const hints = modeHints(form, locale)
  return (
    <>
      <Button leadingVisual={SyncIcon} onClick={() => setOpen(true)}>
        {t('rerun.button')}
      </Button>
      {open && (
        <Dialog title={t('rerun.title')} onClose={() => setOpen(false)} width="large">
          <Form action={`/runs/${runID}/rerun`} className={css.form} aria-label={t('rerun.button')}>
            <p className={css.intro}>{t('rerun.intro', { rerun: rerunLabel(current, locale), state: runState(state, locale) })}</p>
            {form.Purged && (
              <Callout title={t('rerun.purgedTitle')} hideTitle>
                {t('rerun.purged')}
              </Callout>
            )}
            <RadioGroup name="mode">
              <RadioGroup.Label>{t('rerun.which')}</RadioGroup.Label>
              <FormControl disabled={form.Retry === 0}>
                <Radio value="failed" defaultChecked={form.Default === 'failed'} />
                <FormControl.Label>{t('rerun.modeFailed')}</FormControl.Label>
                <FormControl.Caption>{hints.failed}</FormControl.Caption>
              </FormControl>
              <FormControl>
                <Radio value="all" defaultChecked={form.Default === 'all'} />
                <FormControl.Label>{t('rerun.modeAll')}</FormControl.Label>
                <FormControl.Caption>{hints.all}</FormControl.Caption>
              </FormControl>
              <FormControl>
                <Radio value="all_no_cache" />
                <FormControl.Label>{t('rerun.modeNoCache')}</FormControl.Label>
                <FormControl.Caption>{hints.noCache}</FormControl.Caption>
              </FormControl>
            </RadioGroup>
            <KeepWorkspace />
            <div className={css.actions}>
              <Button onClick={() => setOpen(false)}>{t('form.cancel')}</Button>
              <SubmitButton variant="primary" leadingVisual={SyncIcon}>
                {t('rerun.button')}
              </SubmitButton>
            </div>
          </Form>
        </Dialog>
      )}
    </>
  )
}

/** KeepWorkspace 是重跑对话框中的"失败时保留工作区"（run 和 job 的重跑共用）。 */
export function KeepWorkspace() {
  const t = useT()
  return (
    <FormControl>
      <Checkbox name="keep" />
      <FormControl.Label>{t('rerun.keep')}</FormControl.Label>
      <FormControl.Caption>{t('rerun.keepHint')}</FormControl.Caption>
    </FormControl>
  )
}
