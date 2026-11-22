import { LinkButton } from '@primer/react'
import { z } from 'zod'
import { definePage, type PageProps } from '../app/definePage'
import { KilnMark } from '../app/Shell'
import { Msg, Trans, useT, useText } from '../i18n/locale'
import { list, textSchema } from '../lib/envelope'
import css from './error.module.css'

// 统一的错误页：可读的原因和恢复入口；HTTP 状态码作为辅助信息，不显示内部错误。
// 标题、原因和入口的文字是服务端给的消息，按当前语言显示。

const schema = z.object({
  Status: z.number(),
  Heading: textSchema,
  Reason: textSchema,
  /** 错误编号（与服务器日志中的 ref 对应），只有内部错误才有。 */
  Ref: z.string(),
  Links: list(z.object({ Label: textSchema, URL: z.string(), Primary: z.boolean() })),
})
type Data = z.infer<typeof schema>

function ErrorPage({ data, env }: PageProps<Data>) {
  const t = useT()
  const text = useText()
  return (
    <section className={css.page} aria-labelledby="err-h">
      {!env.Session && (
        <a className={css.brand} href="/">
          <KilnMark size={28} />
          Kiln
        </a>
      )}
      <h1 className={css.heading} id="err-h">
        <Msg text={data.Heading} />
      </h1>
      <p className={css.reason}>
        <Msg text={data.Reason} />
      </p>
      <p className={css.ref}>
        {t('error.status', { status: data.Status })}
        {data.Ref && (
          <>
            {' · '}
            <Trans id="error.ref" params={{ ref: data.Ref }} tags={{ code: (c) => <code>{c}</code> }} />
          </>
        )}
      </p>
      {data.Links.length > 0 && (
        <nav className={css.links} aria-label={t('error.next')}>
          {data.Links.map((l) => (
            <LinkButton key={l.URL} href={l.URL} variant={l.Primary ? 'primary' : 'default'}>
              {text(l.Label)}
            </LinkButton>
          ))}
        </nav>
      )}
    </section>
  )
}

export default definePage(schema, ErrorPage)
