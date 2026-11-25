import { Label } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box } from '../components/Box'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
import { useLocale, useT } from '../i18n/locale'
import { list } from '../lib/envelope'
import { labelVariant, outboxState, stateTone } from '../lib/format'
import css from './settings_people.module.css'

// 运维：版本、GitHub App、公网地址，以及待发送和失败的 GitHub 请求（outbox）。

const itemSchema = z.object({
  ID: z.number(),
  DedupeKey: z.string(),
  Kind: z.string(),
  /** pending、done、dead */
  State: z.string(),
  Attempts: z.number(),
  LastError: z.string(),
  UpdatedAt: z.string(),
})

const schema = z.object({
  Section: z.string(),
  /** GitHub App 的 slug，没有连接时为空。 */
  App: z.string(),
  PublicURL: z.string(),
  Version: z.string(),
  Outbox: list(itemSchema),
})
type Data = z.infer<typeof schema>

function Ops({ data }: PageProps<Data>) {
  const t = useT()
  const { locale } = useLocale()
  useLive(data.Outbox.some((o) => o.State === 'pending'))
  return (
    <SettingsLayout section={data.Section}>
      <Subhead>{t('settingsNav.ops')}</Subhead>
      <Box>
        <dl className={css.kv}>
          <dt>{t('ops.version')}</dt>
          <dd className="mono">{data.Version}</dd>
          <dt>GitHub App</dt>
          <dd>{data.App || <span className={css.muted}>{t('ops.notConnected')}</span>}</dd>
          <dt>{t('ops.publicURL')}</dt>
          <dd className="mono">{data.PublicURL}</dd>
        </dl>
      </Box>
      <Subhead level={2} spaced id="outbox-title">
        {t('ops.outbox')}
      </Subhead>
      <Box>
        <table className={css.table} aria-labelledby="outbox-title">
          <thead>
            <tr>
              <th scope="col">#</th>
              <th scope="col">key</th>
              <th scope="col">{t('ops.state')}</th>
              <th scope="col">{t('ops.attempts')}</th>
              <th scope="col">{t('ops.error')}</th>
              <th scope="col">{t('ops.updated')}</th>
              <th scope="col">
                <span className="visually-hidden">{t('ops.actions')}</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {data.Outbox.length > 0 ? (
              data.Outbox.map((o) => (
                <tr key={o.ID}>
                  <td data-label="#" className="num">
                    {o.ID}
                  </td>
                  <td data-label="key" className={`${css.mono ?? ''} ${css.key ?? ''}`}>
                    {o.DedupeKey}
                  </td>
                  <td data-label={t('ops.state')}>
                    <Label variant={labelVariant(stateTone(o.State))} title={o.State}>
                      {outboxState(o.State, locale)}
                    </Label>
                  </td>
                  <td data-label={t('ops.attempts')} className="num">
                    {o.Attempts}
                  </td>
                  <td data-label={t('ops.error')} className={css.err}>
                    {o.LastError}
                  </td>
                  <td data-label={t('ops.updated')} className={css.nowrap}>
                    <Ago t={o.UpdatedAt} />
                  </td>
                  <td>
                    {o.State === 'dead' && (
                      <Form action={`/outbox/${o.ID}/retry`} aria-label={t('ops.retryLabel', { id: o.ID })}>
                        <SubmitButton size="small">{t('ops.retry')}</SubmitButton>
                      </Form>
                    )}
                  </td>
                </tr>
              ))
            ) : (
              <tr>
                <td colSpan={7} className={css.emptyCell}>
                  {t('ops.allDelivered')}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </Box>
    </SettingsLayout>
  )
}

export default definePage(schema, Ops)
