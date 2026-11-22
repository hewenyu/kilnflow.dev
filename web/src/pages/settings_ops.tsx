import { Label } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { useLive } from '../app/live'
import { Box } from '../components/Box'
import { Form, SubmitButton } from '../components/Form'
import { SettingsLayout } from '../components/SettingsLayout'
import { Subhead } from '../components/Subhead'
import { Ago } from '../components/Time'
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
  useLive(data.Outbox.some((o) => o.State === 'pending'))
  return (
    <SettingsLayout section={data.Section}>
      <Subhead>运维</Subhead>
      <Box>
        <dl className={css.kv}>
          <dt>版本</dt>
          <dd className="mono">{data.Version}</dd>
          <dt>GitHub App</dt>
          <dd>{data.App || <span className={css.muted}>未连接</span>}</dd>
          <dt>公网地址</dt>
          <dd className="mono">{data.PublicURL}</dd>
        </dl>
      </Box>
      <Subhead level={2} spaced id="outbox-title">
        Outbox（待发送和失败的 GitHub 请求）
      </Subhead>
      <Box>
        <table className={css.table} aria-labelledby="outbox-title">
          <thead>
            <tr>
              <th scope="col">#</th>
              <th scope="col">key</th>
              <th scope="col">状态</th>
              <th scope="col">次数</th>
              <th scope="col">错误</th>
              <th scope="col">更新</th>
              <th scope="col">
                <span className="visually-hidden">操作</span>
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
                  <td data-label="状态">
                    <Label variant={labelVariant(stateTone(o.State))} title={o.State}>
                      {outboxState(o.State)}
                    </Label>
                  </td>
                  <td data-label="次数" className="num">
                    {o.Attempts}
                  </td>
                  <td data-label="错误" className={css.err}>
                    {o.LastError}
                  </td>
                  <td data-label="更新" className={css.nowrap}>
                    <Ago t={o.UpdatedAt} />
                  </td>
                  <td>
                    {o.State === 'dead' && (
                      <Form action={`/outbox/${o.ID}/retry`} aria-label={`重试 outbox #${o.ID}`}>
                        <SubmitButton size="small">重试</SubmitButton>
                      </Form>
                    )}
                  </td>
                </tr>
              ))
            ) : (
              <tr>
                <td colSpan={7} className={css.emptyCell}>
                  全部已送达
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
